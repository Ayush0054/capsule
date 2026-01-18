/**
 * Capsule SDK - TypeScript Client
 *
 * A client for interacting with the Capsule API server.
 * Isolated. Contained. Secure.
 */

export interface CapsuleConfig {
  baseUrl: string;
  timeout?: number;
}

export interface CreateOptions {
  template: 'python' | 'node' | 'go';
  ttlMs?: number;
}

export interface CreateResult {
  id: string;
  template: string;
  created_at: string;
  expires_at: string;
}

export interface ExecOptions {
  cmd: string[];
  cwd?: string;
  env?: Record<string, string>;
  timeoutMs?: number;
  maxStdoutBytes?: number;
  maxStderrBytes?: number;
}

export interface ExecResult {
  stdout: string;
  stderr: string;
  exit_code: number;
  timed_out: boolean;
  stdout_truncated: boolean;
  stderr_truncated: boolean;
  duration_ms: number;
}

export interface FileInfo {
  name: string;
  path: string;
  is_dir: boolean;
  size: number;
}

interface RPCRequest {
  jsonrpc: '2.0';
  id: number;
  method: string;
  params?: unknown;
}

interface RPCResponse<T> {
  jsonrpc: '2.0';
  id: number;
  result?: T;
  error?: {
    code: number;
    message: string;
    data?: unknown;
  };
}

/**
 * Capsule client for creating and managing isolated code execution environments
 */
export class CapsuleClient {
  private baseUrl: string;
  private timeout: number;
  private requestId = 0;

  constructor(config: CapsuleConfig) {
    this.baseUrl = config.baseUrl.replace(/\/$/, '');
    this.timeout = config.timeout ?? 30000;
  }

  private async rpc<T>(method: string, params?: unknown): Promise<T> {
    const request: RPCRequest = {
      jsonrpc: '2.0',
      id: ++this.requestId,
      method,
      params,
    };

    const controller = new AbortController();
    const timeoutId = setTimeout(() => controller.abort(), this.timeout);

    try {
      const response = await fetch(`${this.baseUrl}/rpc`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(request),
        signal: controller.signal,
      });

      if (!response.ok) {
        throw new Error(`HTTP ${response.status}: ${response.statusText}`);
      }

      const data: RPCResponse<T> = await response.json();

      if (data.error) {
        throw new Error(`RPC Error ${data.error.code}: ${data.error.message}`);
      }

      return data.result as T;
    } finally {
      clearTimeout(timeoutId);
    }
  }

  /**
   * Create a new capsule environment
   */
  async create(options: CreateOptions): Promise<Capsule> {
    const result = await this.rpc<CreateResult>('sandbox.v1.create', {
      template: options.template,
      ttl_ms: options.ttlMs ?? 600000, // 10 minutes default
    });

    return new Capsule(this, result.id, result);
  }

  /**
   * Execute a command in a sandbox
   */
  async exec(sandboxId: string, options: ExecOptions): Promise<ExecResult> {
    return this.rpc<ExecResult>('sandbox.v1.exec', {
      id: sandboxId,
      cmd: options.cmd,
      cwd: options.cwd,
      env: options.env,
      timeout_ms: options.timeoutMs,
      max_stdout_bytes: options.maxStdoutBytes,
      max_stderr_bytes: options.maxStderrBytes,
    });
  }

  /**
   * Delete a sandbox
   */
  async delete(sandboxId: string): Promise<void> {
    await this.rpc<{ ok: boolean }>('sandbox.v1.delete', { id: sandboxId });
  }

  /**
   * Write a file to a sandbox
   */
  async writeFile(sandboxId: string, path: string, content: string | Uint8Array): Promise<void> {
    const bytes = typeof content === 'string'
      ? new TextEncoder().encode(content)
      : content;

    const base64 = btoa(String.fromCharCode(...bytes));

    await this.rpc<{ ok: boolean }>('sandbox.v1.writeFile', {
      id: sandboxId,
      path,
      content: base64,
    });
  }

  /**
   * Read a file from a sandbox
   */
  async readFile(sandboxId: string, path: string): Promise<Uint8Array> {
    const result = await this.rpc<{ content: string }>('sandbox.v1.readFile', {
      id: sandboxId,
      path,
    });

    const binary = atob(result.content);
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) {
      bytes[i] = binary.charCodeAt(i);
    }
    return bytes;
  }

  /**
   * Read a file as text from a sandbox
   */
  async readFileText(sandboxId: string, path: string): Promise<string> {
    const bytes = await this.readFile(sandboxId, path);
    return new TextDecoder().decode(bytes);
  }

  /**
   * List files in a directory
   */
  async listDir(sandboxId: string, path: string = '/workspace'): Promise<FileInfo[]> {
    const result = await this.rpc<{ files: FileInfo[] }>('sandbox.v1.listDir', {
      id: sandboxId,
      path,
    });
    return result.files;
  }

  /**
   * Get WebSocket URL for terminal connection
   */
  getTerminalUrl(sandboxId: string): string {
    const wsUrl = this.baseUrl.replace(/^http/, 'ws');
    return `${wsUrl}/terminal/${sandboxId}`;
  }

  /**
   * Connect to terminal via WebSocket
   */
  connectTerminal(sandboxId: string): WebSocket {
    const url = this.getTerminalUrl(sandboxId);
    return new WebSocket(url);
  }
}

/**
 * Represents a capsule instance with convenience methods
 */
export class Capsule {
  private client: CapsuleClient;
  public readonly id: string;
  public readonly template: string;
  public readonly createdAt: Date;
  public readonly expiresAt: Date;

  constructor(client: CapsuleClient, id: string, info: CreateResult) {
    this.client = client;
    this.id = id;
    this.template = info.template;
    this.createdAt = new Date(info.created_at);
    this.expiresAt = new Date(info.expires_at);
  }

  /**
   * Execute a command in this sandbox
   */
  async exec(cmd: string[], options?: Omit<ExecOptions, 'cmd'>): Promise<ExecResult> {
    return this.client.exec(this.id, { cmd, ...options });
  }

  /**
   * Run code in this sandbox (convenience method)
   */
  async run(code: string, language?: string): Promise<ExecResult> {
    const lang = language ?? this.template;

    switch (lang) {
      case 'python':
        return this.exec(['python3', '-c', code]);
      case 'node':
        return this.exec(['node', '-e', code]);
      case 'go':
        // For Go, we need to write a file and run it
        await this.writeFile('/workspace/main.go', code);
        return this.exec(['go', 'run', '/workspace/main.go']);
      default:
        throw new Error(`Unknown language: ${lang}`);
    }
  }

  /**
   * Write a file to this sandbox
   */
  async writeFile(path: string, content: string | Uint8Array): Promise<void> {
    return this.client.writeFile(this.id, path, content);
  }

  /**
   * Read a file from this sandbox
   */
  async readFile(path: string): Promise<Uint8Array> {
    return this.client.readFile(this.id, path);
  }

  /**
   * Read a file as text from this sandbox
   */
  async readFileText(path: string): Promise<string> {
    return this.client.readFileText(this.id, path);
  }

  /**
   * List files in a directory
   */
  async listDir(path: string = '/workspace'): Promise<FileInfo[]> {
    return this.client.listDir(this.id, path);
  }

  /**
   * Connect to terminal
   */
  connectTerminal(): WebSocket {
    return this.client.connectTerminal(this.id);
  }

  /**
   * Delete this sandbox
   */
  async delete(): Promise<void> {
    return this.client.delete(this.id);
  }
}

export default CapsuleClient;
