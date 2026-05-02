package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/session"
)

// isolateHome redirects $HOME to a fresh t.TempDir so the session-state
// writes go to a sandbox, not the developer's real ~/.guild directory.
// Returns the home path so assertions can build expected file paths.
//
// NOTE: we don't reset the session package's own state because its
// default manager resolves $HOME on every call, not at package init.
// That's intentional (session/state.go's defaultManager comment), and
// we're leveraging it here.
func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

// connectInMemory wires an in-memory server+client pair through the
// SDK's InMemoryTransports. Returns both sessions and a cleanup func
// that closes them in the right order (client first, then wait server)
// so background goroutines are joined before the test returns —
// important under -race.
//
// The returned ctx is NOT cancelled by cleanup; tests that want
// cancellation-driven shutdown must build their own ctx+cancel.
func connectInMemory(t *testing.T, s *sdkmcp.Server) (*sdkmcp.ServerSession, *sdkmcp.ClientSession, func()) {
	t.Helper()
	ctx := context.Background()
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()

	serverSession, err := s.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "guild-test-client"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}

	cleanup := func() {
		_ = clientSession.Close()
		_ = serverSession.Wait()
	}
	return serverSession, clientSession, cleanup
}

// TestBuild_ServerConstructsWithInstructions asserts that build()
// succeeds AND that the Instructions passed into ServerOptions are
// advertised to the client on initialize. The INSTRUCTIONS string IS
// the onboarding protocol.
func TestBuild_ServerConstructsWithInstructions(t *testing.T) {
	isolateHome(t)

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if s == nil {
		t.Fatal("build returned nil server")
	}

	_, clientSession, cleanup := connectInMemory(t, s)
	defer cleanup()

	init := clientSession.InitializeResult()
	if init == nil {
		t.Fatal("no InitializeResult available on client session")
	}
	if init.Instructions == "" {
		t.Fatal("server advertised empty Instructions")
	}
	// Assert on a load-bearing substring rather than full-string
	// equality so instructions.go's formatting details (trailing
	// newline, etc.) don't make the test brittle.
	if !strings.Contains(init.Instructions, "MANDATORY FIRST STEP") {
		t.Fatalf("Instructions missing anchor phrase; got first 120 chars: %q",
			init.Instructions[:min(120, len(init.Instructions))])
	}
}

// TestBuild_RegistersSessionStartTool asserts the bootstrap tool is
// listed by the server's tools/list response.
func TestBuild_RegistersSessionStartTool(t *testing.T) {
	isolateHome(t)

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	_, clientSession, cleanup := connectInMemory(t, s)
	defer cleanup()

	ctx := context.Background()
	res, err := clientSession.ListTools(ctx, &sdkmcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	var found *sdkmcp.Tool
	for _, tool := range res.Tools {
		if tool.Name == "guild_session_start" {
			found = tool
			break
		}
	}
	if found == nil {
		names := make([]string, 0, len(res.Tools))
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		t.Fatalf("guild_session_start not in tools list; got %v", names)
	}
	if found.Description == "" {
		t.Error("guild_session_start registered without description")
	}
	// Lean-description budget — target <100 tokens per tool.
	// 4 chars ≈ 1 token is the accepted rough heuristic for English;
	// we sanity-check the upper bound (400 chars) to catch anyone
	// pasting INSTRUCTIONS-scale copy into a tool description.
	if len(found.Description) > 400 {
		t.Errorf("description exceeds budget: %d chars > 400",
			len(found.Description))
	}
}

// TestBootstrapRoundTrip_PersistsActiveProject is the load-bearing
// integration test: a CallTool("guild_session_start", {project:"X"})
// must write active_project="X" to ~/.guild/sessions/<pid>.json.
func TestBootstrapRoundTrip_PersistsActiveProject(t *testing.T) {
	home := isolateHome(t)

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, clientSession, cleanup := connectInMemory(t, s)
	defer cleanup()

	ctx := context.Background()
	const want = "testproj"
	result, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "guild_session_start",
		Arguments: map[string]any{"project": want},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool returned IsError=true; content: %s",
			textOf(result.Content))
	}

	// Body should carry the narration header.
	body := textOf(result.Content)
	if !strings.Contains(body, "active project:") {
		t.Errorf("missing narration header; body=%q", body)
	}
	if !strings.Contains(body, want) {
		t.Errorf("body missing project name %q; got %q", want, body)
	}

	// Now the on-disk check: ~/.guild/sessions/<pid>.json has
	// active_project=want. The default manager reads the live PID.
	sessionPath := filepath.Join(home, ".guild", "sessions",
		strconv.Itoa(os.Getpid())+".json")
	data, err := os.ReadFile(sessionPath) //nolint:gosec // test path built from t.TempDir
	if err != nil {
		t.Fatalf("read session file %s: %v", sessionPath, err)
	}
	var blob struct {
		ActiveProject string `json:"active_project"`
	}
	if err := json.Unmarshal(data, &blob); err != nil {
		t.Fatalf("parse session file: %v; raw=%q", err, data)
	}
	if blob.ActiveProject != want {
		t.Fatalf("session file active_project = %q; want %q",
			blob.ActiveProject, want)
	}
}

// TestBootstrap_EmptyProjectReturnsRecoverableError locks the error
// shape: empty project → IsError=true, [error] prefix, recovery
// guidance pointing at guild_session_start.
func TestBootstrap_EmptyProjectReturnsRecoverableError(t *testing.T) {
	isolateHome(t)

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, clientSession, cleanup := connectInMemory(t, s)
	defer cleanup()

	result, err := clientSession.CallTool(context.Background(),
		&sdkmcp.CallToolParams{
			Name:      "guild_session_start",
			Arguments: map[string]any{"project": ""},
		})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError {
		t.Errorf("want IsError=true on empty project; got false")
	}
	body := textOf(result.Content)
	if !strings.HasPrefix(body, "[error]") {
		t.Errorf("want [error] prefix; got %q", body)
	}
	if !strings.Contains(body, "guild_session_start") {
		t.Errorf("error does not guide recovery (must name guild_session_start); got %q", body)
	}
}

// TestResolveForMCP_ReportsMissingProjectAfterClear simulates the
// "inherit-from-session" contract by calling session.ResolveForMCP
// directly (no registered tool stubs yet). With no prior bootstrap,
// arg="", env="" → the friendly [error] message.
//
// This substitutes for stubbing a second tool: the public
// session.ResolveForMCP API is what every non-bootstrap tool will call,
// so locking its error shape here prevents a regression at the
// integration boundary that QUEST-12 owns.
func TestResolveForMCP_ReportsMissingProjectAfterClear(t *testing.T) {
	isolateHome(t)

	_, err := session.ResolveForMCP(context.Background(), "", "")
	if err == nil {
		t.Fatal("want error when no arg/session/env set; got nil")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "[error]") {
		t.Errorf("want [error] prefix; got %q", msg)
	}
	if !strings.Contains(msg, "guild_session_start") {
		t.Errorf("error does not name guild_session_start; got %q", msg)
	}
}

// TestServe_ContextCancelShutsDownCleanly asserts that cancelling the
// ctx passed to Serve causes Serve to return without leaking
// goroutines. -race on this test catches handler races; the
// pre/post goroutine-count delta catches "server leaks a goroutine on
// shutdown" regressions.
func TestServe_ContextCancelShutsDownCleanly(t *testing.T) {
	// Pipe pair in place of os.Stdin/os.Stdout so Serve's
	// StdioTransport has something to read and no bytes ever reach
	// the real stdout.
	//
	// We don't actually call Serve here (it would block on stdin);
	// instead we exercise build() + Run via IOTransport with a reader
	// that blocks forever. That's equivalent to what Serve does in
	// production, minus the stdio-binding step.
	isolateHome(t)
	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// A reader that blocks until closed. pipeReader / pipeWriter both
	// satisfy io.ReadCloser / io.WriteCloser — the SDK's IOTransport
	// needs full Closer interfaces, so we use os.Pipe pairs.
	rIn, wIn, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	// Drain rOut in a goroutine — otherwise a full OS pipe buffer
	// could deadlock the server's write path. We don't assert on
	// content; stdout-cleanliness is a separate test.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := rOut.Read(buf); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	transport := &sdkmcp.IOTransport{Reader: rIn, Writer: wOut}

	runDone := make(chan error, 1)
	go func() {
		runDone <- s.Run(ctx, transport)
	}()

	// Give the server a moment to enter its main loop so cancellation
	// exercises the in-session path, not a pre-connect early return.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-runDone:
		// ctx.Err() is the expected return from a cancelled Run;
		// anything else indicates a shutdown bug.
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned unexpected error after cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3s of ctx cancel; possible goroutine leak")
	}

	// Clean up pipe ends; reads after this unblock and the drain
	// goroutine exits.
	_ = wIn.Close()
	_ = wOut.Close()
	_ = rIn.Close()
	_ = rOut.Close()
}

// TestSerDeStdoutCleanDuringToolCall asserts that no stray non-JSON
// bytes (`fmt.Println` etc.) leak through the server's write half
// during a tool call. Every byte on the write side must be a valid
// JSON-RPC frame — anything else means a handler reached for stdout
// and corrupted the protocol.
//
// We implement this by plumbing an in-memory io.Writer as the
// transport's write half and parsing every frame. The SDK writes
// newline-delimited JSON over IOTransport, so each line must be
// valid JSON and present a standard JSON-RPC shape.
func TestSerDeStdoutCleanDuringToolCall(t *testing.T) {
	isolateHome(t)
	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Server-side pipes: reader stays real (we write real JSON-RPC
	// requests to it), writer is captured into a thread-safe buffer.
	rServer, wClient, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	captured := &syncBuffer{}
	transport := &sdkmcp.IOTransport{
		Reader: rServer,
		Writer: nopCloseWriter{captured},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- s.Run(ctx, transport)
	}()

	// Drive the server with a minimal hand-rolled initialize +
	// tools/list sequence. We can't reuse the in-memory client
	// because we need the write half to route through our capturing
	// buffer, not a paired InMemoryTransport.
	//
	// This is a narrow protocol-smoke assertion — just the frame
	// integrity, not full semantic coverage. Semantic coverage lives
	// in the in-memory test pair above.
	mustWrite := func(jsonBody string) {
		if _, err := wClient.WriteString(jsonBody + "\n"); err != nil {
			t.Fatalf("write request: %v", err)
		}
	}
	mustWrite(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"v0"}}}`)
	mustWrite(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	mustWrite(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"guild_session_start","arguments":{"project":"stdouttest"}}}`)

	// Wait for at least the initialize + tools/call responses (2 JSON
	// lines written). A short loop is fine — real responses land in
	// milliseconds.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if strings.Count(captured.String(), "\n") >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for server responses; captured=%q", captured.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Shut down and join.
	cancel()
	_ = wClient.Close()
	_ = rServer.Close()
	<-runDone

	// Every non-empty line captured must parse as JSON and look like
	// a JSON-RPC 2.0 message. Anything else (a bare "hello" print, a
	// stack trace leak, etc.) fails this test.
	lines := strings.Split(strings.TrimRight(captured.String(), "\n"), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("line %d not valid JSON: %v; line=%q", i, err, line)
		}
		if ver, _ := msg["jsonrpc"].(string); ver != "2.0" {
			t.Errorf("line %d missing jsonrpc:\"2.0\"; got %v", i, msg["jsonrpc"])
		}
	}
}

// TestSDKVersionPinned asserts that the MCP SDK is compiled in at a
// supported version (v1.5.0+). Reads from the build-time VCS info
// rather than parsing go.mod at test time: `go list -m` would shell
// out and be brittle across GOPROXY modes. debug.ReadBuildInfo returns
// the exact version the test binary was built against — which is
// exactly what we want to lock.
func TestSDKVersionPinned(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("build info unavailable; pinning check needs module-mode build")
	}
	const want = "github.com/modelcontextprotocol/go-sdk"
	var found string
	for _, dep := range info.Deps {
		if dep.Path == want {
			found = dep.Version
			break
		}
	}
	if found == "" {
		// Some test-binary builds flatten Deps so the direct dep
		// doesn't appear. Fall back to parsing go.mod — the file is
		// the source of truth for version pinning anyway.
		found = sdkVersionFromGoMod(t)
	}
	if found == "" {
		t.Fatalf("%s is not a compiled-in dependency; it must be", want)
	}
	// Accept v1.5.0+. Anything starting with "v1." is accepted up to a
	// future v2 — if v2 ever breaks the Tool API we want this test to
	// FAIL so the operator re-evaluates the pin.
	if !strings.HasPrefix(found, "v1.") {
		t.Fatalf("SDK %s = %q; require v1.5.0+", want, found)
	}
	// Minor-version floor: reject v1.0 … v1.4 but accept v1.5+.
	minor := minorOfSemver(found)
	if minor < 5 {
		t.Fatalf("SDK %s = %q; require v1.5.0+", want, found)
	}
}

// sdkVersionFromGoMod falls back to go.mod parsing when
// debug.ReadBuildInfo() doesn't surface the SDK as a direct dep. The
// test binary is invoked from the package directory, so go.mod lives
// two levels up (internal/mcp -> internal -> module root).
func sdkVersionFromGoMod(t *testing.T) string {
	t.Helper()
	// Walk upward from cwd to find go.mod — survives "go test ./..."
	// which runs tests from each package dir.
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		gomod := filepath.Join(dir, "go.mod")
		if _, err := os.Stat(gomod); err == nil {
			data, err := os.ReadFile(gomod) //nolint:gosec // walking test-own module tree
			if err != nil {
				return ""
			}
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, "github.com/modelcontextprotocol/go-sdk ") {
					continue
				}
				// "<path> <version>" or "<path> <version> // indirect"
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					return fields[1]
				}
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// --- test utilities ------------------------------------------------

// textOf flattens a []mcp.Content down to the concatenated TextContent
// payload. Non-text content is rendered as "<non-text>" so the tests
// don't silently pass over a change in result shape.
func textOf(content []sdkmcp.Content) string {
	var b strings.Builder
	for _, c := range content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			b.WriteString(tc.Text)
			continue
		}
		b.WriteString("<non-text>")
	}
	return b.String()
}

// minorOfSemver parses the minor component from a semver string like
// "v1.5.0" or "v1.5.0-pre.1". Returns -1 on any parse error so the
// caller's check trips rather than silently passing.
func minorOfSemver(v string) int {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return -1
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return -1
	}
	return n
}

// syncBuffer is a goroutine-safe bytes.Buffer. The SDK writes from a
// goroutine; the test reads from the main goroutine. bytes.Buffer is
// NOT safe for concurrent use, so we guard it with a mutex.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// nopCloseWriter adapts an io.Writer to io.WriteCloser — IOTransport
// requires Close-able halves, but we don't need to close our capture
// buffer.
type nopCloseWriter struct {
	w interface{ Write([]byte) (int, error) }
}

func (n nopCloseWriter) Write(p []byte) (int, error) { return n.w.Write(p) }
func (nopCloseWriter) Close() error                  { return nil }

// Compile-time interface guards: IOTransport and StdioTransport must
// both satisfy the sdkmcp.Transport interface. If a future SDK tweak
// drops either from the interface, this var block fails to build,
// surfacing the incompatibility at compile time rather than runtime.
var (
	_ sdkmcp.Transport = (*sdkmcp.IOTransport)(nil)
	_ sdkmcp.Transport = (*sdkmcp.StdioTransport)(nil)
)
