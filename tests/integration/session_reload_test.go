// session_reload_test.go — Behavior #5: active-project persists across MCP restarts
//
// This test exercises the per-PID session file design: each server process
// writes its active project to ~/.guild/sessions/<pid>.json, and stale files
// from dead processes are cleaned up on startup via a kill(pid, 0) probe.
// Per-PID isolation prevents concurrent sessions from clobbering each other's
// active-project state.
//
// These fixtures force direct in-process routing. A shim reconnecting to a
// resident daemon does not restart the server or rerun startup cleanup; daemon
// routing is covered separately by daemon_parity_test.go.
//
// This test exercises the full subprocess lifecycle:
//
//  1. Spawn `guild mcp serve` as a subprocess (FIRST server, PID_1).
//  2. Send newline-delimited JSON-RPC over stdin:
//     initialize → notifications/initialized → tools/call guild_session_start.
//  3. Assert ~/.guild/sessions/<PID_1>.json has active_project="testproj".
//  4. SIGTERM the subprocess (simulate host-initiated restart).
//  5. Wait for PID_1 to die.
//  6. Spawn a SECOND `guild mcp serve` subprocess (PID_2 ≠ PID_1).
//  7. Assert PID_2 ≠ PID_1.
//  8. Assert PID_2's startup cleanup removed PID_1's stale session file.
//
// Protocol note: the Go SDK's StdioTransport uses NEWLINE-DELIMITED JSON
// (not Content-Length framing). Each JSON-RPC message is one JSON object
// on a single line terminated by "\n". See mcp/transport.go in the SDK.
//
// Windows: session cleanup uses a different process-probe mechanism.
// Skip on Windows with the build tag below.
//
//go:build !windows

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSessionReload_PIDLifecycle is the full MCP subprocess lifecycle test.
//
// Invariants proven:
//  1. guild_session_start writes active_project to ~/.guild/sessions/<pid>.json
//  2. Two consecutive server processes have distinct PIDs
//  3. The SECOND server's startup cleanup removes the FIRST server's stale file
func TestSessionReload_PIDLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess MCP test in -short mode")
	}

	ctx := context.Background()
	homeDir := t.TempDir()
	t.Logf("test HOME: %s", homeDir)

	// ── Phase 1: first MCP server ─────────────────────────────────────────
	srv1, sessionFile1 := runMCPBootstrap(ctx, t, homeDir, "testproj")
	pid1 := srv1.PID
	t.Logf("server 1 PID: %d", pid1)
	t.Logf("server 1 session file: %s", sessionFile1)
	fmt.Printf("=== session-reload: server 1 PID=%d ===\n", pid1)

	// Assert session file written with right content.
	assertSessionFileHasProject(t, sessionFile1, "testproj")

	// ── Phase 2: kill server 1 and fully reap it ────────────────────────
	// waitReaped closes stdin (unblocking the JSON-RPC reader), sends
	// SIGKILL, and calls cmd.Wait() to reap the zombie. On macOS, until
	// Wait() is called, kill(pid, 0) returns nil for zombies — the stale
	// cleanup logic would incorrectly treat the dead PID as alive.
	srv1.waitReaped(t, 5*time.Second)
	t.Logf("server 1 (PID %d) reaped", pid1)

	// File must still exist — stale cleanup runs on NEXT startup, not this one.
	if _, err := os.Stat(sessionFile1); os.IsNotExist(err) {
		t.Fatalf("session file for dead PID %d should still exist before server 2 starts", pid1)
	}
	t.Logf("stale file for PID %d still present (expected — cleanup runs on next startup)", pid1)

	// ── Phase 3: second MCP server ──────────────────────────────────────
	// PID_2's startup CleanupStale must remove sessionFile1.
	srv2, sessionFile2 := runMCPBootstrap(ctx, t, homeDir, "testproj2")
	pid2 := srv2.PID
	t.Logf("server 2 PID: %d", pid2)
	t.Logf("server 2 session file: %s", sessionFile2)
	fmt.Printf("=== session-reload: server 2 PID=%d ===\n", pid2)

	// ── Assert distinct PIDs ─────────────────────────────────────────────
	if pid1 == pid2 {
		t.Errorf("PIDs must be distinct; both got %d", pid1)
	} else {
		t.Logf("PID transition confirmed: %d → %d", pid1, pid2)
		fmt.Printf("=== session-reload PID transition: %d → %d ===\n", pid1, pid2)
	}

	// ── Assert stale cleanup ─────────────────────────────────────────────
	// The server's startup cleanup is synchronous before accepting connections.
	// The session_start response returned means the server has already run cleanup.
	// Poll briefly in case of filesystem lag.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sessionFile1); os.IsNotExist(err) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if _, err := os.Stat(sessionFile1); !os.IsNotExist(err) {
		t.Errorf("stale session file for dead PID %d was NOT cleaned up by server 2 startup\nfile: %s",
			pid1, sessionFile1)
	} else {
		t.Logf("stale cleanup confirmed: PID %d file removed by PID %d startup", pid1, pid2)
		fmt.Printf("=== stale cleanup: PID %d file removed by PID %d startup ===\n", pid1, pid2)
	}

	// ── Assert server 2 session has right project ────────────────────────
	assertSessionFileHasProject(t, sessionFile2, "testproj2")

	// Kill server 2.
	srv2.stop()
}

// TestSessionReload_SessionsDirCreatedOnFirstStart verifies that
// ~/.guild/sessions/ is created by the first `guild mcp serve` startup.
func TestSessionReload_SessionsDirCreatedOnFirstStart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess MCP test in -short mode")
	}

	ctx := context.Background()
	homeDir := t.TempDir()

	sessionsDir := filepath.Join(homeDir, ".guild", "sessions")
	if _, err := os.Stat(sessionsDir); !os.IsNotExist(err) {
		t.Fatalf("sessions dir should not exist before server start")
	}

	srv1, sessionFile1 := runMCPBootstrap(ctx, t, homeDir, "fresh-proj")
	defer srv1.stop()

	if _, err := os.Stat(sessionsDir); err != nil {
		t.Errorf("sessions dir %s not created: %v", sessionsDir, err)
	}
	if _, err := os.Stat(sessionFile1); err != nil {
		t.Errorf("session file %s not found: %v", sessionFile1, err)
	}
	t.Logf("sessions dir created: %s", sessionsDir)
}

// ─────────────────────── subprocess lifecycle helpers ────────────────────────

// mcpServer holds the subprocess state returned by startMCPServer.
type mcpServer struct {
	PID       int
	stdinW    *os.File // write-end of server's stdin (caller writes JSON-RPC here)
	stdoutR   *os.File // read-end of server's stdout (caller reads responses here)
	stderrBuf *bytes.Buffer
	cmd       *exec.Cmd
}

// stop kills the process and reaps it (calls Wait). This is critical on
// macOS: without Wait(), the process becomes a zombie and kill(pid, 0)
// still returns nil (zombie is "alive" to the OS until reaped), which
// prevents the stale-cleanup logic from removing the session file.
func (s *mcpServer) stop() {
	_ = s.stdinW.Close()                      // close stdin so server's JSON-RPC reader unblocks
	_ = s.cmd.Process.Signal(syscall.SIGKILL) // force-kill
	_ = s.cmd.Wait()                          // reap the zombie so kill(pid, 0) returns ESRCH
	_ = s.stdoutR.Close()
}

// waitReaped kills the process, reaps it, and waits until kill(pid,0)
// returns ESRCH. Use this instead of stop() when the test needs to
// confirm the process is truly gone before starting the next server.
func (s *mcpServer) waitReaped(t *testing.T, timeout time.Duration) {
	t.Helper()
	_ = s.stdinW.Close()
	_ = s.cmd.Process.Signal(syscall.SIGKILL)
	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		t.Logf("PID %d reaped", s.PID)
	case <-time.After(timeout):
		t.Logf("PID %d Wait timed out after %s", s.PID, timeout)
	}
	_ = s.stdoutR.Close()
}

// startMCPServer starts `guild mcp serve` and returns an mcpServer handle.
// The subprocess is NOT bootstrapped yet — caller must drive the handshake.
func startMCPServer(t *testing.T, homeDir string) *mcpServer {
	t.Helper()
	bin := requireBinary(t)

	//nolint:gosec // bin and homeDir are trusted
	cmd := exec.Command(bin, "mcp", "serve")
	cmd.Env = []string{
		"HOME=" + homeDir,
		"PATH=" + os.Getenv("PATH"),
		"GUILD_NO_USAGE_LOG=1",
		// Each child must own its MCP server lifecycle. Socket-path limits
		// must not decide whether this fixture autostarts a resident daemon.
		"GUILD_NO_DAEMON=1",
	}
	cmd.Dir = homeDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderrBuf bytes.Buffer

	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		t.Fatalf("cmd.Start mcp serve: %v", err)
	}
	pid := cmd.Process.Pid
	t.Logf("started MCP server PID=%d", pid)

	// Close the server-side ends in this (parent) process.
	_ = stdinR.Close()
	_ = stdoutW.Close()

	srv := &mcpServer{
		PID:       pid,
		stdinW:    stdinW,
		stdoutR:   stdoutR,
		stderrBuf: &stderrBuf,
		cmd:       cmd,
	}

	// Register test-end cleanup (best-effort kill).
	t.Cleanup(func() { srv.stop() })

	return srv
}

// runMCPBootstrap starts `guild mcp serve` as a subprocess, drives the
// JSON-RPC initialize + guild_session_start handshake over newline-delimited
// JSON stdin/stdout, and returns (*mcpServer, sessionFilePath).
//
// The subprocess is registered with t.Cleanup for termination.
// Callers that need to kill it early should call srv.waitReaped() or srv.stop().
func runMCPBootstrap(ctx context.Context, t *testing.T, homeDir, project string) (srv *mcpServer, sessionFile string) {
	t.Helper()
	_ = ctx // reserved for future use in sub-helpers

	srv = startMCPServer(t, homeDir)

	// Give the server a moment to start its JSON-RPC read loop.
	time.Sleep(150 * time.Millisecond)

	// Send newline-delimited JSON messages to the server's stdin.
	// The SDK's StdioTransport reads one JSON object per line.
	sendLine := func(msg string) {
		t.Helper()
		if _, err := fmt.Fprintln(srv.stdinW, msg); err != nil {
			t.Logf("sendLine: %v (server may have exited)", err)
		}
	}

	// Step 1: initialize
	sendLine(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"integration-test","version":"v0"}}}`)
	time.Sleep(100 * time.Millisecond)

	// Step 2: notifications/initialized (no response expected)
	sendLine(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	time.Sleep(50 * time.Millisecond)

	// Step 3: guild_session_start
	sendLine(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"guild_session_start","arguments":{"project":%q}}}`,
		project,
	))

	// Read responses until we get the tools/call response (id=2).
	gotBootstrap := make(chan struct{}, 1)
	go func() {
		scanner := bufio.NewScanner(srv.stdoutR)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}
			var msg map[string]any
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}
			t.Logf("MCP response id=%v", msg["id"])
			if id, ok := msg["id"].(float64); ok && id == 2 {
				select {
				case gotBootstrap <- struct{}{}:
				default:
				}
			}
		}
	}()

	select {
	case <-gotBootstrap:
		t.Logf("guild_session_start response received for PID %d", srv.PID)
	case <-time.After(10 * time.Second):
		t.Fatalf("timeout waiting for guild_session_start response from PID %d\nstderr:\n%s",
			srv.PID, srv.stderrBuf.String())
	}

	// Pin the lifecycle assumption independently of platform socket limits.
	if _, err := os.Stat(filepath.Join(homeDir, ".guild", "daemon.json")); !os.IsNotExist(err) {
		t.Fatalf("direct MCP lifecycle fixture created daemon discovery: %v", err)
	}

	// Derive session file path: ~/.guild/sessions/<pid>.json
	sessionFile = filepath.Join(homeDir, ".guild", "sessions",
		strconv.Itoa(srv.PID)+".json")

	// Wait for the session file to appear on disk.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sessionFile); err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if _, err := os.Stat(sessionFile); err != nil {
		t.Fatalf("session file %s never appeared: %v\nstderr:\n%s",
			sessionFile, err, srv.stderrBuf.String())
	}
	t.Logf("session file confirmed at: %s", sessionFile)

	return srv, sessionFile
}

// assertSessionFileHasProject reads path and verifies active_project == want.
func assertSessionFileHasProject(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // path is from t.TempDir()
	if err != nil {
		t.Fatalf("read session file %s: %v", path, err)
	}
	var state struct {
		ActiveProject string `json:"active_project"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse session file %s: %v; raw=%q", path, err, data)
	}
	if state.ActiveProject != want {
		t.Errorf("session file active_project=%q; want %q (file: %s)", state.ActiveProject, want, path)
	} else {
		t.Logf("session file: active_project=%q OK", state.ActiveProject)
	}
}

// killPID sends SIGTERM then (after 1s) SIGKILL to ensure the process dies.
// Non-fatal on error (process may have already exited).
func killPID(t *testing.T, pid int) {
	t.Helper()
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Logf("killPID: FindProcess(%d): %v", pid, err)
		return
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		t.Logf("killPID: SIGTERM(%d): %v", pid, err)
	}
	// Give it 1s to exit gracefully, then force-kill.
	time.Sleep(500 * time.Millisecond)
	// Check if still alive.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return // already dead
	}
	t.Logf("killPID: PID %d still alive after SIGTERM, sending SIGKILL", pid)
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		t.Logf("killPID: SIGKILL(%d): %v", pid, err)
	}
}

// waitPIDDead polls until pid is no longer running or timeout elapses.
func waitPIDDead(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return // can't find it → dead
		}
		// On Unix, signal 0 probes existence without sending a signal.
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return // ESRCH → dead
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("waitPIDDead: PID %d still alive after %s (proceeding)", pid, timeout)
}
