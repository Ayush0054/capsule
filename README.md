
     ██████╗ █████╗ ██████╗ ███████╗██╗   ██╗██╗     ███████╗
    ██╔════╝██╔══██╗██╔══██╗██╔════╝██║   ██║██║     ██╔════╝
    ██║     ███████║██████╔╝███████╗██║   ██║██║     █████╗
    ██║     ██╔══██║██╔═══╝ ╚════██║██║   ██║██║     ██╔══╝
    ╚██████╗██║  ██║██║     ███████║╚██████╔╝███████╗███████╗
     ╚═════╝╚═╝  ╚═╝╚═╝     ╚══════╝ ╚═════╝ ╚══════╝╚══════╝


*Isolated. Contained. Secure.*

---

Capsule is a secure, isolated code execution platform. Run untrusted code in Docker containers with resource limits, no network access, and automatic cleanup.

## Features

- **Isolated Execution** - Each sandbox runs in its own Docker container
- **Multi-language** - Python, Node.js, and Go templates
- **File Operations** - Read, write, and list files in sandboxes
- **Live Terminal** - WebSocket-based interactive shell access
- **Resource Limits** - CPU, memory, and process limits
- **Auto Cleanup** - Sandboxes expire automatically after TTL
- **SDKs** - TypeScript and Python clients included

## Quick Start

### Prerequisites

- Docker installed and running
- Go 1.22+ (for server)

### Run the Server

```bash
cd core
go run cmd/server/main.go
```

Server starts at `http://localhost:8080`

### Using the TypeScript SDK

```typescript
import { CapsuleClient } from 'capsule-sdk';

const client = new CapsuleClient({ baseUrl: 'http://localhost:8080' });

// Create a Python sandbox
const capsule = await client.create({ template: 'python' });

// Run code
const result = await capsule.run('print("Hello, Capsule!")');
console.log(result.stdout); // "Hello, Capsule!\n"

// File operations
await capsule.writeFile('/workspace/data.txt', 'Hello World');
const content = await capsule.readFileText('/workspace/data.txt');

// Interactive terminal
const ws = capsule.connectTerminal();
ws.onmessage = (e) => console.log(e.data);
ws.send('python3\n');

// Cleanup
await capsule.delete();
```

### Using the Python SDK

```python
from capsule_sdk import CapsuleClient

client = CapsuleClient("http://localhost:8080")

# Context manager auto-deletes on exit
with client.create("python") as capsule:
    result = capsule.run("print(sum(range(10)))")
    print(result.stdout)  # "45\n"

    # File operations
    capsule.write_file("/workspace/app.py", "print('hello')")
    content = capsule.read_file_text("/workspace/app.py")
    files = capsule.list_dir("/workspace")
```

## API Reference

### JSON-RPC Endpoints

All API calls use JSON-RPC 2.0 over HTTP POST to `/rpc`.

#### sandbox.v1.create

Create a new sandbox.

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "sandbox.v1.create",
  "params": {
    "template": "python",
    "ttl_ms": 600000
  }
}
```

**Response:**
```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "id": "abc12345",
    "template": "python",
    "created_at": "2024-01-19T10:00:00Z",
    "expires_at": "2024-01-19T10:10:00Z"
  }
}
```

#### sandbox.v1.exec

Execute a command.

```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "sandbox.v1.exec",
  "params": {
    "id": "abc12345",
    "cmd": ["python3", "-c", "print('hello')"],
    "timeout_ms": 30000
  }
}
```

**Response:**
```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "result": {
    "stdout": "hello\n",
    "stderr": "",
    "exit_code": 0,
    "timed_out": false,
    "duration_ms": 45
  }
}
```

#### sandbox.v1.writeFile

Write a file (content is base64 encoded).

```json
{
  "jsonrpc": "2.0",
  "id": 3,
  "method": "sandbox.v1.writeFile",
  "params": {
    "id": "abc12345",
    "path": "/workspace/app.py",
    "content": "cHJpbnQoJ2hlbGxvJyk="
  }
}
```

#### sandbox.v1.readFile

Read a file (returns base64 encoded content).

```json
{
  "jsonrpc": "2.0",
  "id": 4,
  "method": "sandbox.v1.readFile",
  "params": {
    "id": "abc12345",
    "path": "/workspace/app.py"
  }
}
```

#### sandbox.v1.listDir

List directory contents.

```json
{
  "jsonrpc": "2.0",
  "id": 5,
  "method": "sandbox.v1.listDir",
  "params": {
    "id": "abc12345",
    "path": "/workspace"
  }
}
```

#### sandbox.v1.delete

Delete a sandbox.

```json
{
  "jsonrpc": "2.0",
  "id": 6,
  "method": "sandbox.v1.delete",
  "params": {
    "id": "abc12345"
  }
}
```

### WebSocket Terminal

Connect to `ws://localhost:8080/terminal/{sandboxID}` for interactive shell access.

```javascript
const ws = new WebSocket('ws://localhost:8080/terminal/abc12345');

ws.onmessage = (e) => terminal.write(e.data);
terminal.onData((data) => ws.send(data));
```

