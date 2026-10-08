package e2e

// Minimal JSON-RPC 2.0 driver for MCP over stdio.
//
// Deliberately hand-rolled instead of reusing the go-sdk client: the
// harness exists to pin guild's wire behavior (newline-delimited JSON-RPC
// over stdio, per the MCP stdio transport), and the daemon work will
// assert byte-identical transcripts across server process models. A raw
// driver keeps the full request/response surface visible with no
// client-side abstraction that could mask framing or envelope changes,
// and it swaps transports (stdio today, daemon socket later) by changing
// only how the two pipes are obtained.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// callTimeout bounds a single JSON-RPC round-trip. The slowest call is
// the first embedding-backed one in a fresh container (ONNX runtime
// extraction + model load).
const callTimeout = 90 * time.Second

// lockedBuffer is a mutex-guarded bytes.Buffer. exec.Cmd copies the
// server's stderr into it from its own goroutine while rpc error paths
// read it from the session's goroutine; without the lock that pair is a
// data race.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// mcpSession is one `guild mcp serve` process inside a scenario
// container, driven over docker exec stdin/stdout.
type mcpSession struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	msgs   chan json.RawMessage
	stderr *lockedBuffer
	nextID int
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// initializeResult is the subset of the MCP initialize response the
// scenarios assert on.
type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
	Instructions string `json:"instructions"`
}

// toolResult is the MCP tools/call result envelope.
type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// openSession starts `guild mcp serve` inside the container (workdir =
// the scenario project dir) and wires the NDJSON reader. The caller must
// call initialize before tools/call.
func (c *container) openSession(ctx context.Context, t *testing.T) *mcpSession {
	t.Helper()

	args := []string{
		"exec", "-i",
		"-w", projectDir,
		"-e", "GUILD_NO_UPDATE_CHECK=1",
	}
	// Direct mode is the no-daemon, in-process path: pin GUILD_NO_DAEMON=1
	// so `guild mcp serve` never autostarts a daemon (autostart is on by
	// default). This keeps direct mode the byte-identical no-daemon
	// baseline the golden transcripts encode. Daemon mode (when it starts
	// a daemon inside the container) deliberately omits the opt-out so the
	// shim pipes through the daemon and the same goldens still hold.
	if suite.mode != modeDaemon {
		args = append(args, "-e", "GUILD_NO_DAEMON=1")
	}
	// Container-scoped extra env (e.g. GUILD_MODULE_LORE=0 for the
	// module-toggle proof). Empty for the golden scenarios.
	args = append(args, c.extraEnvArgs()...)
	args = append(args, c.name, "guild", "mcp", "serve")

	//nolint:gosec // argv is harness-controlled
	cmd := exec.CommandContext(ctx, "docker", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("mcp session stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("mcp session stdout: %v", err)
	}
	s := &mcpSession{t: t, cmd: cmd, stdin: stdin, stderr: &lockedBuffer{}}
	cmd.Stderr = s.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mcp session: %v", err)
	}

	// Reader goroutine: one JSON message per line (MCP stdio framing is
	// newline-delimited JSON). Channel closes on EOF / transport error.
	s.msgs = make(chan json.RawMessage, 16)
	go func() {
		defer close(s.msgs)
		dec := json.NewDecoder(stdout)
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return
			}
			s.msgs <- raw
		}
	}()

	t.Cleanup(func() { s.close() })
	return s
}

// close shuts the session down: stdin EOF tells the server to exit, then
// we wait briefly before killing.
func (s *mcpSession) close() {
	if s.stdin != nil {
		_ = s.stdin.Close()
		s.stdin = nil
	}
	if s.cmd == nil {
		return
	}
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
	s.cmd = nil
}

// send writes one JSON-RPC message (newline-delimited). Goroutine-safe
// in the error-returning sense: it never touches testing.T.
func (s *mcpSession) send(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal jsonrpc message: %w", err)
	}
	data = append(data, '\n')
	if _, err := s.stdin.Write(data); err != nil {
		return fmt.Errorf("write jsonrpc message: %w (server stderr:\n%s)", err, s.stderr.String())
	}
	return nil
}

// rpc sends a request and blocks until the matching response arrives.
// Server-originated notifications/requests received in between are
// skipped. Never touches testing.T, so it is safe to drive a session
// from a non-test goroutine (each session from exactly one goroutine).
func (s *mcpSession) rpc(method string, params any) (json.RawMessage, error) {
	s.nextID++
	id := s.nextID
	if err := s.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}); err != nil {
		return nil, err
	}

	deadline := time.After(callTimeout)
	for {
		select {
		case raw, ok := <-s.msgs:
			if !ok {
				return nil, fmt.Errorf("mcp session closed while waiting for %s response (server stderr:\n%s)",
					method, s.stderr.String())
			}
			var env rpcEnvelope
			if err := json.Unmarshal(raw, &env); err != nil {
				return nil, fmt.Errorf("decode jsonrpc message %q: %w", raw, err)
			}
			if env.ID == nil || *env.ID != id || env.Method != "" {
				// Notification or server-initiated request: not ours.
				continue
			}
			if env.Error != nil {
				return nil, fmt.Errorf("%s: jsonrpc error %d: %s", method, env.Error.Code, env.Error.Message)
			}
			return env.Result, nil
		case <-deadline:
			return nil, fmt.Errorf("timeout (%s) waiting for %s response (server stderr:\n%s)",
				callTimeout, method, s.stderr.String())
		}
	}
}

// call is the test-failing wrapper around rpc for sequential scenario
// steps on the test goroutine.
func (s *mcpSession) call(method string, params any) json.RawMessage {
	s.t.Helper()
	raw, err := s.rpc(method, params)
	if err != nil {
		s.t.Fatalf("%v", err)
	}
	return raw
}

// notify sends a JSON-RPC notification (no id, no response).
func (s *mcpSession) notify(method string, params any) {
	s.t.Helper()
	if err := s.send(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}); err != nil {
		s.t.Fatalf("%v", err)
	}
}

// initialize performs the MCP handshake and returns the server's
// initialize result.
func (s *mcpSession) initialize() initializeResult {
	s.t.Helper()
	raw := s.call("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "guild-e2e-harness",
			"version": "0.0.0",
		},
	})
	var res initializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		s.t.Fatalf("decode initialize result: %v", err)
	}
	s.notify("notifications/initialized", map[string]any{})
	return res
}

// listToolNames returns the names from tools/list in server order.
func (s *mcpSession) listToolNames() []string {
	s.t.Helper()
	raw := s.call("tools/list", map[string]any{})
	var res struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		s.t.Fatalf("decode tools/list result: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// callToolErr invokes tools/call and returns the concatenated text
// content. Goroutine-safe (never touches testing.T); isError results
// and empty content come back as errors.
func (s *mcpSession) callToolErr(name string, args map[string]any) (string, error) {
	raw, err := s.rpc("tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}
	var res toolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("decode tools/call %s result: %w", name, err)
	}
	var text bytes.Buffer
	for _, content := range res.Content {
		if content.Type == "text" {
			text.WriteString(content.Text)
		}
	}
	if res.IsError {
		return "", fmt.Errorf("tools/call %s returned isError:\n%s", name, text.String())
	}
	if text.Len() == 0 {
		return "", fmt.Errorf("tools/call %s returned no text content", name)
	}
	return text.String(), nil
}

// callTool is the test-failing wrapper around callToolErr for
// sequential scenario steps on the test goroutine.
func (s *mcpSession) callTool(name string, args map[string]any) string {
	s.t.Helper()
	out, err := s.callToolErr(name, args)
	if err != nil {
		s.t.Fatalf("%v", err)
	}
	return out
}

// sessionStart is the canonical bootstrap: guild_session_start with an
// explicit project (the container has no git, so cwd auto-inference is
// not available; explicit project is the supported path).
func (s *mcpSession) sessionStart(project string) string {
	s.t.Helper()
	return s.callTool("guild_session_start", map[string]any{"project": project})
}
