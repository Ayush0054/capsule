# Architecture Documentation

┌─────────────────────────────────────────────────────────────────────────┐
│                     DOCKER SANDBOX PLATFORM                             │
└─────────────────────────────────────────────────────────────────────────┘

                              ┌──────────────┐
                              │    Client    │
                              │   (SDK)      │
                              └──────┬───────┘
                                     │
                      ┌──────────────┴──────────────┐
                      │                             │
                  JSON-RPC                     WebSocket
                  (commands)                   (streaming)
                      │                             │
                      ▼                             ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                                                                         │
│                           API SERVER                                    │
│   ┌─────────────────┐              ┌─────────────────┐                  │
│   │   JSON-RPC      │              │   WebSocket     │                  │
│   │   Handler       │              │   Handler       │                  │
│   │   (Port 8080)   │              │   (streaming)   │                  │
│   └────────┬────────┘              └────────┬────────┘                  │
│            │                                │                           │
│            └────────────┬───────────────────┘                           │
│                         │                                               │
│                         ▼                                               │
│            ┌─────────────────────┐                                      │
│            │   Docker Provider   │                                      │
│            │   (docker.sock)     │                                      │
│            └─────────────────────┘                                      │
│                         │                                               │
│     ┌───────────────────┼───────────────────┐                           │
│     │                   │                   │                           │
│     ▼                   ▼                   ▼                           │
│ ┌────────┐        ┌──────────┐        ┌──────────┐                      │
│ │ create │        │  exec    │        │  remove  │                      │
│ │container│       │(docker   │        │container │                      │
│ └────────┘        │  exec)   │        └──────────┘                      │
│                   └──────────┘                                          │
└─────────────────────────────────────────────────────────────────────────┘
                                     │
                                     │ docker exec
                                     ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                        DOCKER CONTAINER                                 │
│                                                                         │
│   ┌─────────────────────────────────────────────────────────────────┐   │
│   │  python3 / node / bash (executed via docker exec)               │   │
│   └─────────────────────────────────────────────────────────────────┘   │
│                                                                         │
│   ┌─────────────────────────────────────────────────────────────────┐   │
│   │  /workspace  (user files)                                       │   │
│   └─────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────┘


1. Client sends JSON-RPC:  sandbox.create({ template: "python" })
   └─> Server creates Docker container, returns sandbox_id

2. Client sends JSON-RPC:  sandbox.exec({ id, code, language })
   └─> Server runs: docker exec <container> python3 -c "<code>"
   └─> Returns: { stdout, stderr, exit_code }

3. Client opens WebSocket: ws://server/sandbox/{id}/stream
   └─> Server runs: docker exec -i <container> bash
   └─> Streams stdin/stdout over WebSocket (real-time terminal)

4. Client sends JSON-RPC:  sandbox.delete({ id })
   └─> Server runs: docker rm -f <container>


   ## UPDATED Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                        SDK (Client)                          │
│  TypeScript: CapsuleClient          Python: CapsuleClient   │
└────────────────────────────┬────────────────────────────────┘
                             │ HTTP (JSON-RPC) / WebSocket
                             ▼
┌─────────────────────────────────────────────────────────────┐
│                      Capsule Server                          │
│                                                              │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────┐  │
│  │   rpc.go     │  │ websocket.go │  │     main.go      │  │
│  │  JSON-RPC    │  │  Terminal    │  │   Entry point    │  │
│  └──────┬───────┘  └──────┬───────┘  └──────────────────┘  │
│         └────────┬────────┘                                  │
│                  ▼                                           │
│         ┌────────────────┐                                  │
│         │    Provider    │  Interface                       │
│         └────────┬───────┘                                  │
└──────────────────┼──────────────────────────────────────────┘
                   ▼
┌─────────────────────────────────────────────────────────────┐
│                   DockerProvider                             │
│  - Container lifecycle (create/delete)                      │
│  - Command execution (docker exec)                          │
│  - File operations (read/write/list)                        │
│  - TTL-based garbage collection                             │
└─────────────────────────────────────────────────────────────┘
                   │
                   ▼
┌─────────────────────────────────────────────────────────────┐
│                    Docker Daemon                             │
│  Containers with: No network, dropped caps, resource limits │
└─────────────────────────────────────────────────────────────┘
```