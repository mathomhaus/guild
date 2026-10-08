//go:build unix

package mcp

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/daemon"
)

// TestShim_RealClientFullSessionAndKillRecovery is the end-to-end
// acceptance gate for the stdio shim: a REAL go-sdk client speaks to
// `guild mcp serve`'s pipe mode (daemon.RunShim) exactly as a harness
// would over stdio, with a real daemon behind the socket and
// mcp.ServeIO wired as the crash fallback, the same assembly
// cmd/guild/mcp.go ships.
//
// Phase 1 (daemon up): initialize, tools/list, and a tool call all
// flow through the socket; the advertised tool set is identical to the
// stdio server's.
//
// Phase 2 (daemon killed mid-session): the daemon goes away with the
// client still attached. The next tool call must still return a valid
// result: the shim re-dials once, finds nothing, and splices the
// in-process server under the live session by replaying the retained
// handshake.
func TestShim_RealClientFullSessionAndKillRecovery(t *testing.T) {
	home := isolateHome(t)
	const projID = "shimproj"
	projDir := filepath.Join(home, "ws", projID)
	registerProjectsForDaemon(t, map[string]string{projID: projDir})

	sock := daemonSocketPath(t)
	dctx, dcancel := context.WithCancel(context.Background())
	defer dcancel()

	host := NewDaemonHost()
	srv, err := daemon.NewServer(daemon.Config{
		Version:       "v-shimtest",
		SocketPath:    sock,
		Sessions:      host.ServeSession,
		EmbedderState: host.EmbedderState,
		Logger:        slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		t.Fatalf("daemon.NewServer: %v", err)
	}
	daemonDone := make(chan error, 1)
	go func() { daemonDone <- srv.Run(dctx) }()
	waitDialable(t, sock)

	// The harness-facing stdio pair, with RunShim in the middle.
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	shimCtx, shimCancel := context.WithCancel(context.Background())
	defer shimCancel()
	shimDone := make(chan error, 1)
	go func() {
		shimDone <- daemon.RunShim(shimCtx, daemon.ShimConfig{
			SocketPath: sock,
			// PID is this test process: the same identity the real shim
			// sends, and what keys the per-session state file for both
			// the daemon session and the in-process fallback.
			Preamble: daemon.ShimPreamble{Version: "v-shimtest", CWD: projDir, PID: os.Getpid()},
			Stdin:    stdinR,
			Stdout:   stdoutW,
			Fallback: ServeIO, // the exact wiring cmd/guild uses
			Logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		})
	}()

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "guild-shim-test-client"}, nil)
	cs, err := client.Connect(context.Background(), &sdkmcp.IOTransport{Reader: stdoutR, Writer: stdinW}, nil)
	if err != nil {
		t.Fatalf("client connect through shim: %v", err)
	}

	ctx := context.Background()

	// Phase 1: full session through the socket.
	init := cs.InitializeResult()
	if init == nil {
		t.Fatal("no InitializeResult through the shim")
	}
	if !strings.Contains(init.Instructions, "MANDATORY FIRST STEP") {
		t.Fatalf("INSTRUCTIONS through the shim missing anchor phrase; first 120 chars: %q",
			init.Instructions[:min(120, len(init.Instructions))])
	}

	res, err := cs.ListTools(ctx, &sdkmcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools through shim: %v", err)
	}
	shimNames := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		shimNames = append(shimNames, tool.Name)
	}
	sort.Strings(shimNames)
	if d := cmp(shimNames, listToolNames(t)); d != "" {
		t.Fatalf("tools/list through shim diverges from the stdio server:\n%s", d)
	}

	callRes, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "guild_session_start",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("guild_session_start through shim: %v", err)
	}
	if callRes.IsError {
		t.Fatalf("guild_session_start through shim IsError: %s", textOf(callRes.Content))
	}
	if body := textOf(callRes.Content); !strings.Contains(body, "active project: "+projID) {
		t.Fatalf("daemon session did not resolve the shim cwd's project; body:\n%s", body)
	}

	// Phase 2: kill the daemon mid-session. Waiting for Run to return
	// guarantees the socket and discovery file are gone, so the shim's
	// single re-dial deterministically fails and it must fall back
	// in-process.
	dcancel()
	select {
	case derr := <-daemonDone:
		if derr != nil {
			t.Fatalf("daemon Run returned %v on cancel; want nil", derr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop within 5s of cancel")
	}

	callRes, err = cs.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "guild_session_start",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("tool call after daemon kill: %v (the session must survive via in-process fallback)", err)
	}
	if callRes.IsError {
		t.Fatalf("tool call after daemon kill IsError: %s", textOf(callRes.Content))
	}
	if body := textOf(callRes.Content); !strings.Contains(body, "active project: "+projID) {
		t.Fatalf("fallback session lost the active project; body:\n%s", body)
	}

	// Clean shutdown: closing stdin ends the fallback server (EOF),
	// and RunShim must report a clean session.
	_ = stdinW.Close()
	select {
	case serr := <-shimDone:
		if serr != nil {
			t.Fatalf("RunShim returned %v; want nil after clean stdin close", serr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunShim did not return after stdin close")
	}
	_ = cs.Close()
	_ = stdoutR.Close()
}

// waitDialable blocks until the daemon socket accepts connections.
func waitDialable(t *testing.T, sock string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", sock, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never became dialable: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
