"""
Capsule SDK - Python Client

A client for interacting with the Capsule API server.
Isolated. Contained. Secure.
"""

import base64
import json
from dataclasses import dataclass
from datetime import datetime
from typing import Optional, List, Dict, Any, Union
import urllib.request
import urllib.error


@dataclass
class ExecResult:
    """Result of executing a command in a sandbox."""
    stdout: str
    stderr: str
    exit_code: int
    timed_out: bool
    stdout_truncated: bool
    stderr_truncated: bool
    duration_ms: int


@dataclass
class FileInfo:
    """Information about a file in the sandbox."""
    name: str
    path: str
    is_dir: bool
    size: int


class CapsuleError(Exception):
    """Base exception for capsule errors."""
    def __init__(self, code: int, message: str, data: Any = None):
        self.code = code
        self.message = message
        self.data = data
        super().__init__(f"RPC Error {code}: {message}")


class CapsuleClient:
    """
    Client for creating and managing isolated code execution environments.

    Usage:
        client = CapsuleClient("http://localhost:8080")
        capsule = client.create("python")
        result = capsule.run("print('Hello, World!')")
        print(result.stdout)
        capsule.delete()
    """

    def __init__(self, base_url: str, timeout: int = 30):
        """
        Initialize the sandbox client.

        Args:
            base_url: Base URL of the sandbox server (e.g., "http://localhost:8080")
            timeout: Request timeout in seconds
        """
        self.base_url = base_url.rstrip('/')
        self.timeout = timeout
        self._request_id = 0

    def _rpc(self, method: str, params: Optional[Dict] = None) -> Any:
        """Make a JSON-RPC request."""
        self._request_id += 1

        request_data = {
            "jsonrpc": "2.0",
            "id": self._request_id,
            "method": method,
        }
        if params:
            request_data["params"] = params

        req = urllib.request.Request(
            f"{self.base_url}/rpc",
            data=json.dumps(request_data).encode('utf-8'),
            headers={"Content-Type": "application/json"},
            method="POST"
        )

        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as response:
                data = json.loads(response.read().decode('utf-8'))
        except urllib.error.HTTPError as e:
            raise CapsuleError(-1, f"HTTP Error: {e.code} {e.reason}")
        except urllib.error.URLError as e:
            raise CapsuleError(-1, f"Connection Error: {e.reason}")

        if "error" in data and data["error"]:
            error = data["error"]
            raise CapsuleError(error.get("code", -1), error.get("message", "Unknown error"), error.get("data"))

        return data.get("result")

    def create(self, template: str, ttl_ms: int = 600000) -> "Capsule":
        """
        Create a new sandbox environment.

        Args:
            template: Template to use ("python", "node", or "go")
            ttl_ms: Time-to-live in milliseconds (default: 10 minutes)

        Returns:
            A Capsule instance
        """
        result = self._rpc("sandbox.v1.create", {
            "template": template,
            "ttl_ms": ttl_ms,
        })

        return Capsule(
            client=self,
            id=result["id"],
            template=result["template"],
            created_at=datetime.fromisoformat(result["created_at"].replace("Z", "+00:00")),
            expires_at=datetime.fromisoformat(result["expires_at"].replace("Z", "+00:00")),
        )

    def exec(
        self,
        sandbox_id: str,
        cmd: List[str],
        cwd: Optional[str] = None,
        env: Optional[Dict[str, str]] = None,
        timeout_ms: Optional[int] = None,
        max_stdout_bytes: Optional[int] = None,
        max_stderr_bytes: Optional[int] = None,
    ) -> ExecResult:
        """
        Execute a command in a sandbox.

        Args:
            sandbox_id: The sandbox ID
            cmd: Command to execute as a list of strings
            cwd: Working directory (optional)
            env: Environment variables (optional)
            timeout_ms: Execution timeout in milliseconds (optional)
            max_stdout_bytes: Maximum stdout bytes to capture (optional)
            max_stderr_bytes: Maximum stderr bytes to capture (optional)

        Returns:
            ExecResult with stdout, stderr, exit code, etc.
        """
        params = {"id": sandbox_id, "cmd": cmd}
        if cwd:
            params["cwd"] = cwd
        if env:
            params["env"] = env
        if timeout_ms:
            params["timeout_ms"] = timeout_ms
        if max_stdout_bytes:
            params["max_stdout_bytes"] = max_stdout_bytes
        if max_stderr_bytes:
            params["max_stderr_bytes"] = max_stderr_bytes

        result = self._rpc("sandbox.v1.exec", params)

        return ExecResult(
            stdout=result["stdout"],
            stderr=result["stderr"],
            exit_code=result["exit_code"],
            timed_out=result["timed_out"],
            stdout_truncated=result["stdout_truncated"],
            stderr_truncated=result["stderr_truncated"],
            duration_ms=result["duration_ms"],
        )

    def delete(self, sandbox_id: str) -> None:
        """Delete a sandbox."""
        self._rpc("sandbox.v1.delete", {"id": sandbox_id})

    def write_file(self, sandbox_id: str, path: str, content: Union[str, bytes]) -> None:
        """
        Write a file to a sandbox.

        Args:
            sandbox_id: The sandbox ID
            path: Path in the sandbox (e.g., "/workspace/app.py")
            content: File content (string or bytes)
        """
        if isinstance(content, str):
            content = content.encode('utf-8')

        encoded = base64.b64encode(content).decode('ascii')

        self._rpc("sandbox.v1.writeFile", {
            "id": sandbox_id,
            "path": path,
            "content": encoded,
        })

    def read_file(self, sandbox_id: str, path: str) -> bytes:
        """
        Read a file from a sandbox.

        Args:
            sandbox_id: The sandbox ID
            path: Path in the sandbox

        Returns:
            File content as bytes
        """
        result = self._rpc("sandbox.v1.readFile", {
            "id": sandbox_id,
            "path": path,
        })

        return base64.b64decode(result["content"])

    def read_file_text(self, sandbox_id: str, path: str, encoding: str = "utf-8") -> str:
        """
        Read a file as text from a sandbox.

        Args:
            sandbox_id: The sandbox ID
            path: Path in the sandbox
            encoding: Text encoding (default: utf-8)

        Returns:
            File content as string
        """
        return self.read_file(sandbox_id, path).decode(encoding)

    def list_dir(self, sandbox_id: str, path: str = "/workspace") -> List[FileInfo]:
        """
        List files in a directory.

        Args:
            sandbox_id: The sandbox ID
            path: Directory path (default: /workspace)

        Returns:
            List of FileInfo objects
        """
        result = self._rpc("sandbox.v1.listDir", {
            "id": sandbox_id,
            "path": path,
        })

        return [
            FileInfo(
                name=f["name"],
                path=f["path"],
                is_dir=f["is_dir"],
                size=f["size"],
            )
            for f in result.get("files", [])
        ]

    def get_terminal_url(self, sandbox_id: str) -> str:
        """Get WebSocket URL for terminal connection."""
        ws_url = self.base_url.replace("http://", "ws://").replace("https://", "wss://")
        return f"{ws_url}/terminal/{sandbox_id}"


class Capsule:
    """
    Represents a sandbox instance with convenience methods.

    Usage:
        sandbox = client.create("python")
        result = sandbox.run("print(1 + 1)")
        print(result.stdout)  # "2\n"
        sandbox.delete()
    """

    def __init__(
        self,
        client: CapsuleClient,
        id: str,
        template: str,
        created_at: datetime,
        expires_at: datetime,
    ):
        self._client = client
        self.id = id
        self.template = template
        self.created_at = created_at
        self.expires_at = expires_at

    def exec(
        self,
        cmd: List[str],
        cwd: Optional[str] = None,
        env: Optional[Dict[str, str]] = None,
        timeout_ms: Optional[int] = None,
    ) -> ExecResult:
        """Execute a command in this sandbox."""
        return self._client.exec(self.id, cmd, cwd, env, timeout_ms)

    def run(self, code: str, language: Optional[str] = None) -> ExecResult:
        """
        Run code in this sandbox.

        Args:
            code: Code to execute
            language: Language override (default: use sandbox template)

        Returns:
            ExecResult with stdout, stderr, exit code, etc.
        """
        lang = language or self.template

        if lang == "python":
            return self.exec(["python3", "-c", code])
        elif lang == "node":
            return self.exec(["node", "-e", code])
        elif lang == "go":
            # For Go, write a file and run it
            self.write_file("/workspace/main.go", code)
            return self.exec(["go", "run", "/workspace/main.go"])
        else:
            raise ValueError(f"Unknown language: {lang}")

    def write_file(self, path: str, content: Union[str, bytes]) -> None:
        """Write a file to this sandbox."""
        self._client.write_file(self.id, path, content)

    def read_file(self, path: str) -> bytes:
        """Read a file from this sandbox."""
        return self._client.read_file(self.id, path)

    def read_file_text(self, path: str, encoding: str = "utf-8") -> str:
        """Read a file as text from this sandbox."""
        return self._client.read_file_text(self.id, path, encoding)

    def list_dir(self, path: str = "/workspace") -> List[FileInfo]:
        """List files in a directory."""
        return self._client.list_dir(self.id, path)

    def get_terminal_url(self) -> str:
        """Get WebSocket URL for terminal connection."""
        return self._client.get_terminal_url(self.id)

    def delete(self) -> None:
        """Delete this sandbox."""
        self._client.delete(self.id)

    def __enter__(self) -> "Capsule":
        """Support context manager usage."""
        return self

    def __exit__(self, exc_type, exc_val, exc_tb) -> None:
        """Auto-delete on context exit."""
        self.delete()
