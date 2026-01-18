package rpc

import (
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for development
	},
}

// TerminalProvider interface for WebSocket terminal
type TerminalProvider interface {
	GetContainerID(sandboxID string) (string, error)
	GetClient() *client.Client
}

// TerminalHandler handles WebSocket terminal connections
type TerminalHandler struct {
	P TerminalProvider
}

// ServeHTTP handles WebSocket upgrade and terminal streaming
// URL format: /terminal/{sandboxID}
func (h *TerminalHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Extract sandbox ID from URL path
	path := strings.TrimPrefix(r.URL.Path, "/terminal/")
	sandboxID := strings.Split(path, "/")[0]

	if sandboxID == "" {
		http.Error(w, "sandbox ID required", http.StatusBadRequest)
		return
	}

	// Get container ID
	containerID, err := h.P.GetContainerID(sandboxID)
	if err != nil {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	// Upgrade to WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	// Create exec with TTY
	cli := h.P.GetClient()
	ctx := context.Background()

	execConfig := container.ExecOptions{
		Cmd:          []string{"/bin/sh"},
		Tty:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	}

	execID, err := cli.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		log.Printf("Exec create failed: %v", err)
		conn.WriteMessage(websocket.TextMessage, []byte("Error: "+err.Error()))
		return
	}

	// Attach to exec with TTY
	resp, err := cli.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{
		Tty: true,
	})
	if err != nil {
		log.Printf("Exec attach failed: %v", err)
		conn.WriteMessage(websocket.TextMessage, []byte("Error: "+err.Error()))
		return
	}
	defer resp.Close()

	// Start the exec
	err = cli.ContainerExecStart(ctx, execID.ID, container.ExecStartOptions{Tty: true})
	if err != nil {
		log.Printf("Exec start failed: %v", err)
		conn.WriteMessage(websocket.TextMessage, []byte("Error: "+err.Error()))
		return
	}

	// Create done channel for cleanup
	done := make(chan struct{})

	// Container stdout → WebSocket
	go func() {
		defer close(done)
		buf := make([]byte, 1024)
		for {
			n, err := resp.Reader.Read(buf)
			if err != nil {
				if err != io.EOF {
					log.Printf("Read from container failed: %v", err)
				}
				return
			}
			if n > 0 {
				err = conn.WriteMessage(websocket.BinaryMessage, buf[:n])
				if err != nil {
					log.Printf("Write to WebSocket failed: %v", err)
					return
				}
			}
		}
	}()

	// WebSocket → Container stdin
	go func() {
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Printf("WebSocket read error: %v", err)
				}
				return
			}
			_, err = resp.Conn.Write(message)
			if err != nil {
				log.Printf("Write to container failed: %v", err)
				return
			}
		}
	}()

	// Handle resize messages (optional, for terminal resize)
	conn.SetPingHandler(func(appData string) error {
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
	})

	// Wait for done
	<-done
}
