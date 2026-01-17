package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

const (
	DefaultTimeoutMS   = 5000
	DefaultStdoutBytes = 1 << 20  // 1MB
	DefaultStderrBytes = 1 << 20  // 1MB
	MaxTimeoutMS       = 120_000  // 2 minutes (set your cap)
	MaxOutputBytesCap  = 10 << 20 // 10MB cap even if client asks more
)

type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type RPCResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type ErrorData struct {
	Type      string `json:"type"`
	Retryable bool   `json:"retryable"`
	Details   any    `json:"details,omitempty"`
}

// ---- Params/Results ----

type CreateParams struct {
	Template string `json:"template"`
	TTLMS    int    `json:"ttl_ms,omitempty"`
}

type CreateResult struct {
	ID        string `json:"id"`
	Template  string `json:"template"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

type ExecParams struct {
	ID             string            `json:"id"`
	Cmd            []string          `json:"cmd"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutMS      int               `json:"timeout_ms,omitempty"`
	MaxStdoutBytes int               `json:"max_stdout_bytes,omitempty"`
	MaxStderrBytes int               `json:"max_stderr_bytes,omitempty"`
}

type ExecResult struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	TimedOut        bool   `json:"timed_out"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
	DurationMS      int64  `json:"duration_ms"`
}

type DeleteParams struct {
	ID string `json:"id"`
}

type DeleteResult struct {
	OK bool `json:"ok"`
}

// ---- Provider interface you’ll implement with Docker ----

type Provider interface {
	Create(ctx context.Context, template string, ttl time.Duration) (sandboxID string, expiresAt time.Time, err error)
	Exec(ctx context.Context, sandboxID string, cmd []string, cwd string, env map[string]string, maxOut, maxErr int) (stdout, stderr []byte, exitCode int, timedOut bool, outTrunc, errTrunc bool, duration time.Duration, err error)
	Delete(ctx context.Context, sandboxID string) error
}

// ---- HTTP handler ----

type Server struct {
	P Provider
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20)) // 2MB request cap
	if err != nil {
		writeRPC(w, RPCResponse{JSONRPC: "2.0", Error: rpcErr(-32001, "invalid request body", "INVALID_PARAMS", false, nil)})
		return
	}

	var req RPCRequest
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, RPCResponse{JSONRPC: "2.0", Error: rpcErr(-32001, "invalid json-rpc request", "INVALID_PARAMS", false, nil)})
		return
	}

	// Dispatch
	switch req.Method {
	case "sandbox.v1.create":
		var p CreateParams
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Template == "" {
			writeRPC(w, RPCResponse{JSONRPC: "2.0", ID: req.ID, Error: rpcErr(-32001, "invalid params", "INVALID_PARAMS", false, nil)})
			return
		}

		ttl := time.Duration(p.TTLMS) * time.Millisecond
		if p.TTLMS <= 0 {
			ttl = 10 * time.Minute
		}

		id, exp, err := s.P.Create(r.Context(), p.Template, ttl)
		if err != nil {
			writeRPC(w, RPCResponse{JSONRPC: "2.0", ID: req.ID, Error: rpcErr(-32003, "container create failed", "CONTAINER_CREATE_FAILED", true, map[string]any{"err": err.Error()})})
			return
		}

		now := time.Now().UTC()
		res := CreateResult{
			ID:        id,
			Template:  p.Template,
			CreatedAt: now.Format(time.RFC3339),
			ExpiresAt: exp.UTC().Format(time.RFC3339),
		}
		writeRPC(w, RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: res})

	case "sandbox.v1.exec":
		var p ExecParams
		if err := json.Unmarshal(req.Params, &p); err != nil || p.ID == "" || len(p.Cmd) == 0 {
			writeRPC(w, RPCResponse{JSONRPC: "2.0", ID: req.ID, Error: rpcErr(-32001, "invalid params", "INVALID_PARAMS", false, nil)})
			return
		}

		timeoutMS := p.TimeoutMS
		if timeoutMS <= 0 {
			timeoutMS = DefaultTimeoutMS
		}
		if timeoutMS > MaxTimeoutMS {
			timeoutMS = MaxTimeoutMS
		}

		maxOut := p.MaxStdoutBytes
		if maxOut <= 0 {
			maxOut = DefaultStdoutBytes
		}
		if maxOut > MaxOutputBytesCap {
			maxOut = MaxOutputBytesCap
		}

		maxErr := p.MaxStderrBytes
		if maxErr <= 0 {
			maxErr = DefaultStderrBytes
		}
		if maxErr > MaxOutputBytesCap {
			maxErr = MaxOutputBytesCap
		}

		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeoutMS)*time.Millisecond)
		defer cancel()

		stdout, stderr, exitCode, timedOut, outTrunc, errTrunc, dur, err := s.P.Exec(ctx, p.ID, p.Cmd, p.Cwd, p.Env, maxOut, maxErr)
		if err != nil {
			// distinguish not found / timeout / generic
			if errors.Is(err, context.DeadlineExceeded) || timedOut {
				writeRPC(w, RPCResponse{JSONRPC: "2.0", ID: req.ID, Error: rpcErr(-32004, "exec timed out", "EXEC_TIMEOUT", true, map[string]any{"timeout_ms": timeoutMS})})
				return
			}
			writeRPC(w, RPCResponse{JSONRPC: "2.0", ID: req.ID, Error: rpcErr(-32005, "exec failed", "EXEC_FAILED", true, map[string]any{"err": err.Error()})})
			return
		}

		res := ExecResult{
			Stdout:          string(stdout),
			Stderr:          string(stderr),
			ExitCode:        exitCode,
			TimedOut:        timedOut,
			StdoutTruncated: outTrunc,
			StderrTruncated: errTrunc,
			DurationMS:      dur.Milliseconds(),
		}
		writeRPC(w, RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: res})

	case "sandbox.v1.delete":
		var p DeleteParams
		if err := json.Unmarshal(req.Params, &p); err != nil || p.ID == "" {
			writeRPC(w, RPCResponse{JSONRPC: "2.0", ID: req.ID, Error: rpcErr(-32001, "invalid params", "INVALID_PARAMS", false, nil)})
			return
		}

		if err := s.P.Delete(r.Context(), p.ID); err != nil {
			writeRPC(w, RPCResponse{JSONRPC: "2.0", ID: req.ID, Error: rpcErr(-32007, "delete failed", "DELETE_FAILED", true, map[string]any{"err": err.Error()})})
			return
		}
		writeRPC(w, RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: DeleteResult{OK: true}})

	default:
		// JSON-RPC standard “method not found”
		writeRPC(w, RPCResponse{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{Code: -32601, Message: "method not found"}})
	}
}

func rpcErr(code int, msg, typ string, retryable bool, details any) *RPCError {
	return &RPCError{
		Code:    code,
		Message: msg,
		Data: ErrorData{
			Type:      typ,
			Retryable: retryable,
			Details:   details,
		},
	}
}

func writeRPC(w http.ResponseWriter, resp RPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
