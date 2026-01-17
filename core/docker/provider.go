// docker.go
package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/google/uuid"
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
