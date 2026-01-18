// main.go
package main

import (
	"log"
	"net/http"

	"sandbox/core/docker"
	rpc "sandbox/core/api"
)

func main() {
	// Create Docker provider
	provider, err := docker.NewDockerProvider()
	if err != nil {
		log.Fatalf("Failed to create provider: %v", err)
	}

	// Create RPC server with provider
	server := &rpc.Server{P: provider}

	// Create terminal handler for WebSocket
	terminal := &rpc.TerminalHandler{P: provider}

	// Mount handlers
	http.Handle("/rpc", server)
	http.Handle("/terminal/", terminal)

	// Health check
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	addr := ":8080"
	log.Printf("Sandbox server starting on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
