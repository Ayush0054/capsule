// docker.go
package docker

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/google/uuid"

	rpc "sandbox/core/api"
)

// DockerProvider implements the Provider interface from rpc.go
type DockerProvider struct {
	client     *client.Client
	sandboxes  map[string]*Sandbox // track active sandboxes
	mu         sync.RWMutex
	gcInterval time.Duration
}

type Sandbox struct {
	ID          string
	ContainerID string
	Template    string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// Template configs - which image to use for each template
var templates = map[string]string{
	"python": "python:3.11-slim",
	"node":   "node:20-slim",
	"go":     "golang:1.22-alpine",
}

func NewDockerProvider() (*DockerProvider, error) {
	// Connect to Docker daemon via unix socket
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	// Verify connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = cli.Ping(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to ping docker: %w", err)
	}

	p := &DockerProvider{
		client:     cli,
		sandboxes:  make(map[string]*Sandbox),
		gcInterval: 30 * time.Second,
	}

	// Start background cleanup of expired sandboxes
	go p.gcLoop()

	return p, nil
}

// Create implements Provider.Create
func (p *DockerProvider) Create(ctx context.Context, template string, ttl time.Duration) (string, time.Time, error) {
	// Validate template
	img, ok := templates[template]
	if !ok {
		return "", time.Time{}, fmt.Errorf("unknown template: %s", template)
	}

	// Pull image if not exists (could be slow first time)
	_, _, err := p.client.ImageInspectWithRaw(ctx, img)
	if err != nil {
		// Image not found locally, pull it
		reader, err := p.client.ImagePull(ctx, img, image.PullOptions{})
		if err != nil {
			return "", time.Time{}, fmt.Errorf("failed to pull image %s: %w", img, err)
		}
		defer reader.Close()
		// Wait for pull to complete
		_, _ = io.Copy(io.Discard, reader)
	}

	// Generate sandbox ID
	sandboxID := uuid.New().String()[:8]

	// Create container with security constraints
	config := &container.Config{
		Image:        img,
		Cmd:          []string{"sleep", "infinity"}, // Keep container running
		WorkingDir:   "/workspace",
		Tty:          false,
		OpenStdin:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	}

	hostConfig := &container.HostConfig{
		// Security: drop all capabilities
		CapDrop: []string{"ALL"},
		// Security: no network access
		NetworkMode: "none",
		// Security: prevent privilege escalation
		SecurityOpt: []string{"no-new-privileges"},
		// Resource limits
		Resources: container.Resources{
			Memory:     512 * 1024 * 1024, // 512MB
			MemorySwap: 512 * 1024 * 1024, // No swap
			CPUPeriod:  100000,
			CPUQuota:   50000, // 50% of one CPU
			PidsLimit:  func() *int64 { v := int64(100); return &v }(),
		},
		// Read-only root filesystem (optional, can be strict)
		// ReadonlyRootfs: true,
	}

	resp, err := p.client.ContainerCreate(ctx, config, hostConfig, nil, nil, "sandbox-"+sandboxID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to create container: %w", err)
	}

	// Start the container
	if err := p.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Cleanup on failure
		_ = p.client.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", time.Time{}, fmt.Errorf("failed to start container: %w", err)
	}

	now := time.Now()
	expiresAt := now.Add(ttl)

	// Track sandbox
	p.mu.Lock()
	p.sandboxes[sandboxID] = &Sandbox{
		ID:          sandboxID,
		ContainerID: resp.ID,
		Template:    template,
		CreatedAt:   now,
		ExpiresAt:   expiresAt,
	}
	p.mu.Unlock()

	return sandboxID, expiresAt, nil
}

// Exec implements Provider.Exec
func (p *DockerProvider) Exec(
	ctx context.Context,
	sandboxID string,
	cmd []string,
	cwd string,
	env map[string]string,
	maxOut, maxErr int,
) (stdout, stderr []byte, exitCode int, timedOut bool, outTrunc, errTrunc bool, duration time.Duration, err error) {

	start := time.Now()

	// Find sandbox
	p.mu.RLock()
	sandbox, ok := p.sandboxes[sandboxID]
	p.mu.RUnlock()

	if !ok {
		return nil, nil, -1, false, false, false, 0, fmt.Errorf("sandbox not found: %s", sandboxID)
	}

	// Build env slice
	envSlice := make([]string, 0, len(env))
	for k, v := range env {
		envSlice = append(envSlice, k+"="+v)
	}

	// Working directory
	workDir := "/workspace"
	if cwd != "" {
		workDir = cwd
	}

	// Create exec instance
	execConfig := container.ExecOptions{
		Cmd:          cmd,
		WorkingDir:   workDir,
		Env:          envSlice,
		AttachStdout: true,
		AttachStderr: true,
	}

	execID, err := p.client.ContainerExecCreate(ctx, sandbox.ContainerID, execConfig)
	if err != nil {
		return nil, nil, -1, false, false, false, time.Since(start), fmt.Errorf("exec create failed: %w", err)
	}

	// Attach to exec
	resp, err := p.client.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, nil, -1, false, false, false, time.Since(start), fmt.Errorf("exec attach failed: %w", err)
	}
	defer resp.Close()

	// Read output with limits
	var stdoutBuf, stderrBuf bytes.Buffer
	stdoutLimited := &limitedWriter{W: &stdoutBuf, Max: maxOut}
	stderrLimited := &limitedWriter{W: &stderrBuf, Max: maxErr}

	// Docker multiplexes stdout/stderr - demux it
	_, err = stdcopy.StdCopy(stdoutLimited, stderrLimited, resp.Reader)

	// Check if context timed out
	if ctx.Err() == context.DeadlineExceeded {
		timedOut = true
	}

	// Get exit code
	inspectResp, err := p.client.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		// If we can't inspect, return what we have
		return stdoutBuf.Bytes(), stderrBuf.Bytes(), -1, timedOut,
			stdoutLimited.Truncated, stderrLimited.Truncated, time.Since(start), nil
	}

	return stdoutBuf.Bytes(), stderrBuf.Bytes(), inspectResp.ExitCode, timedOut,
		stdoutLimited.Truncated, stderrLimited.Truncated, time.Since(start), nil
}

// Delete implements Provider.Delete
func (p *DockerProvider) Delete(ctx context.Context, sandboxID string) error {
	p.mu.Lock()
	sandbox, ok := p.sandboxes[sandboxID]
	if ok {
		delete(p.sandboxes, sandboxID)
	}
	p.mu.Unlock()

	if !ok {
		return fmt.Errorf("sandbox not found: %s", sandboxID)
	}

	// Force remove container
	err := p.client.ContainerRemove(ctx, sandbox.ContainerID, container.RemoveOptions{
		Force: true,
	})
	if err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}

	return nil
}

// Background garbage collection for expired sandboxes
func (p *DockerProvider) gcLoop() {
	ticker := time.NewTicker(p.gcInterval)
	defer ticker.Stop()

	for range ticker.C {
		p.cleanupExpired()
	}
}

func (p *DockerProvider) cleanupExpired() {
	now := time.Now()
	var toDelete []string

	p.mu.RLock()
	for id, sb := range p.sandboxes {
		if now.After(sb.ExpiresAt) {
			toDelete = append(toDelete, id)
		}
	}
	p.mu.RUnlock()

	for _, id := range toDelete {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = p.Delete(ctx, id)
		cancel()
	}
}


// WriteFile writes content to a file in the sandbox
func (p *DockerProvider) WriteFile(ctx context.Context, sandboxID, path string, content []byte) error {
	p.mu.RLock()
	sandbox, ok := p.sandboxes[sandboxID]
	p.mu.RUnlock()

	if !ok {
		return fmt.Errorf("sandbox not found: %s", sandboxID)
	}

	// Use base64 to safely transfer binary content
	encoded := base64.StdEncoding.EncodeToString(content)

	// Create parent directories and write file
	cmd := fmt.Sprintf("mkdir -p $(dirname %s) && echo %s | base64 -d > %s", path, encoded, path)

	execConfig := container.ExecOptions{
		Cmd:          []string{"sh", "-c", cmd},
		AttachStdout: true,
		AttachStderr: true,
	}

	execID, err := p.client.ContainerExecCreate(ctx, sandbox.ContainerID, execConfig)
	if err != nil {
		return fmt.Errorf("exec create failed: %w", err)
	}

	resp, err := p.client.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return fmt.Errorf("exec attach failed: %w", err)
	}
	defer resp.Close()

	// Wait for completion
	_, _ = io.Copy(io.Discard, resp.Reader)

	// Check exit code
	inspectResp, err := p.client.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return fmt.Errorf("exec inspect failed: %w", err)
	}
	if inspectResp.ExitCode != 0 {
		return fmt.Errorf("write file failed with exit code %d", inspectResp.ExitCode)
	}

	return nil
}

// ReadFile reads content from a file in the sandbox
func (p *DockerProvider) ReadFile(ctx context.Context, sandboxID, path string) ([]byte, error) {
	p.mu.RLock()
	sandbox, ok := p.sandboxes[sandboxID]
	p.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("sandbox not found: %s", sandboxID)
	}

	execConfig := container.ExecOptions{
		Cmd:          []string{"cat", path},
		AttachStdout: true,
		AttachStderr: true,
	}

	execID, err := p.client.ContainerExecCreate(ctx, sandbox.ContainerID, execConfig)
	if err != nil {
		return nil, fmt.Errorf("exec create failed: %w", err)
	}

	resp, err := p.client.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("exec attach failed: %w", err)
	}
	defer resp.Close()

	var stdoutBuf, stderrBuf bytes.Buffer
	_, _ = stdcopy.StdCopy(&stdoutBuf, &stderrBuf, resp.Reader)

	// Check exit code
	inspectResp, err := p.client.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return nil, fmt.Errorf("exec inspect failed: %w", err)
	}
	if inspectResp.ExitCode != 0 {
		return nil, fmt.Errorf("file not found or read error: %s", stderrBuf.String())
	}

	return stdoutBuf.Bytes(), nil
}

// ListDir lists files in a directory in the sandbox
func (p *DockerProvider) ListDir(ctx context.Context, sandboxID, path string) ([]rpc.FileInfo, error) {
	p.mu.RLock()
	sandbox, ok := p.sandboxes[sandboxID]
	p.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("sandbox not found: %s", sandboxID)
	}

	// Use stat to get detailed file info in JSON-like format
	cmd := fmt.Sprintf(`find %s -maxdepth 1 -printf '%%f\t%%s\t%%Y\t%%T@\n' 2>/dev/null | tail -n +2`, path)

	execConfig := container.ExecOptions{
		Cmd:          []string{"sh", "-c", cmd},
		AttachStdout: true,
		AttachStderr: true,
	}

	execID, err := p.client.ContainerExecCreate(ctx, sandbox.ContainerID, execConfig)
	if err != nil {
		return nil, fmt.Errorf("exec create failed: %w", err)
	}

	resp, err := p.client.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("exec attach failed: %w", err)
	}
	defer resp.Close()

	var stdoutBuf, stderrBuf bytes.Buffer
	_, _ = stdcopy.StdCopy(&stdoutBuf, &stderrBuf, resp.Reader)

	// Parse output
	var files []rpc.FileInfo
	lines := bytes.Split(bytes.TrimSpace(stdoutBuf.Bytes()), []byte("\n"))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		parts := bytes.Split(line, []byte("\t"))
		if len(parts) < 4 {
			continue
		}

		name := string(parts[0])
		size := int64(0)
		fmt.Sscanf(string(parts[1]), "%d", &size)
		isDir := string(parts[2]) == "d"

		files = append(files, rpc.FileInfo{
			Name:  name,
			Path:  path + "/" + name,
			IsDir: isDir,
			Size:  size,
		})
	}

	return files, nil
}

// GetContainerID returns the Docker container ID for a sandbox (used by WebSocket handler)
func (p *DockerProvider) GetContainerID(sandboxID string) (string, error) {
	p.mu.RLock()
	sandbox, ok := p.sandboxes[sandboxID]
	p.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("sandbox not found: %s", sandboxID)
	}
	return sandbox.ContainerID, nil
}

// GetClient returns the Docker client (used by WebSocket handler)
func (p *DockerProvider) GetClient() *client.Client {
	return p.client
}

// limitedWriter wraps a writer with a max byte limit
type limitedWriter struct {
	W         io.Writer
	Max       int
	Written   int
	Truncated bool
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.Written >= lw.Max {
		lw.Truncated = true
		return len(p), nil // Discard but report success
	}

	remaining := lw.Max - lw.Written
	if len(p) > remaining {
		lw.Truncated = true
		p = p[:remaining]
	}

	n, err := lw.W.Write(p)
	lw.Written += n
	return n, err
}
