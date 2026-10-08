//go:build unix

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/daemon"
	"github.com/mathomhaus/guild/internal/daemon/testsupport"
)

// daemonSocketPath returns a unix socket path in a short-named temp dir
// outside t.TempDir(): macOS caps sun_path near 104 bytes and go-test
// temp roots under /var/folders regularly exceed it. HOME isolation
// still uses t.TempDir(); only the socket needs the short path.
func daemonSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gm")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p := filepath.Join(dir, "d.sock")
	if len(p) > 100 {
		t.Skipf("temp socket path too long for sun_path: %s", p)
	}
	return p
}

// startHostedDaemon runs a daemon.Server wired to host over sock and
// blocks until it accepts connections. Cleanup is the caller's ctx.
func startHostedDaemon(t *testing.T, ctx context.Context, host *DaemonHost, sock string) {
	t.Helper()
	srv, err := daemon.NewServer(daemon.Config{
		Version:       "v-hosttest",
		SocketPath:    sock,
		Sessions:      host.ServeSession,
		EmbedderState: host.EmbedderState,
		Logger:        slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		t.Fatalf("daemon.NewServer: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()
	t.Cleanup(func() {
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("daemon Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop within 5s of ctx cancel")
		}
	})

	testsupport.WaitReady(t, "socket "+sock+" dialable", func() bool {
		conn, err := net.DialTimeout("unix", sock, 100*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
}

// dialShimSession dials the daemon socket, sends a shim preamble, and
// connects a REAL go-sdk client over the remaining byte stream,
// exactly what the stdio shim will do. Returns the live client session.
func dialShimSession(t *testing.T, sock, cwd string, pid int) (cs *sdkmcp.ClientSession, cleanup func()) {
	t.Helper()
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}

	pre := fmt.Sprintf(`{"guild_shim":{"version":"v-hosttest","cwd":%q,"pid":%d}}`+"\n", cwd, pid)
	if _, err := conn.Write([]byte(pre)); err != nil {
		_ = conn.Close()
		t.Fatalf("write shim preamble: %v", err)
	}

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "guild-daemon-test-client"}, nil)
	cs, err = client.Connect(context.Background(), &sdkmcp.IOTransport{Reader: conn, Writer: conn}, nil)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("client connect over unix socket: %v", err)
	}
	cleanup = func() {
		_ = cs.Close()
		_ = conn.Close()
	}
	return cs, cleanup
}

// registerProjectsForDaemon registers each id→dir pair in both DBs and
// stubs the project resolver so any dir counts as its own git toplevel.
// The stub's Getwd is irrelevant on the daemon path: every connection's
// inference substitutes the shim preamble cwd for Getwd.
func registerProjectsForDaemon(t *testing.T, projects map[string]string) {
	t.Helper()
	for id, dir := range projects {
		registerCWDAsProject(t, id, dir)
	}
}

// TestDaemonHost_E2E_RealClientOverUnixSocket is the acceptance gate
// for serving MCP over the daemon socket: preamble, initialize,
// tools/list, and a real tool call, all against a hermetic HOME. The
// advertised tool set must be byte-identical to the stdio server's
// (same registerAll, no reimplementation).
func TestDaemonHost_E2E_RealClientOverUnixSocket(t *testing.T) {
	home := isolateHome(t)
	const projID = "sockproj"
	projDir := filepath.Join(home, "ws", projID)
	registerProjectsForDaemon(t, map[string]string{projID: projDir})

	sock := daemonSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startHostedDaemon(t, ctx, NewDaemonHost(), sock)

	const shimPID = 777001
	cs, cleanup := dialShimSession(t, sock, projDir, shimPID)
	defer cleanup()

	// initialize: the daemon session advertises real INSTRUCTIONS.
	init := cs.InitializeResult()
	if init == nil {
		t.Fatal("no InitializeResult on daemon session")
	}
	if !strings.Contains(init.Instructions, "MANDATORY FIRST STEP") {
		t.Fatalf("daemon session INSTRUCTIONS missing anchor phrase; first 120 chars: %q",
			init.Instructions[:min(120, len(init.Instructions))])
	}

	// tools/list over the socket == tools/list from the stdio server.
	res, err := cs.ListTools(ctx, &sdkmcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools over socket: %v", err)
	}
	socketNames := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		socketNames = append(socketNames, tool.Name)
	}
	sort.Strings(socketNames)
	stdioNames := listToolNames(t)
	if d := cmp(socketNames, stdioNames); d != "" {
		t.Fatalf("daemon tools/list diverges from stdio server:\n%s", d)
	}

	// Tool call with NO project arg: the daemon must infer the project
	// from the SHIM's preamble cwd, not its own working directory.
	callRes, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "guild_session_start",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("guild_session_start over socket: %v", err)
	}
	body := textOf(callRes.Content)
	if callRes.IsError {
		t.Fatalf("guild_session_start IsError: %s", body)
	}
	if !strings.Contains(body, "active project: "+projID) {
		t.Fatalf("session did not resolve the preamble cwd's project; body:\n%s", body)
	}

	// Presence (ADR-005 Phase 3): a daemon-served session_start carries the
	// daemon-only presence line, sourced from the registry the session
	// registered on. This single live connection reads "1 agent active".
	if !strings.Contains(body, "👥 1 agent active") {
		t.Fatalf("daemon session_start missing presence line; body:\n%s", body)
	}

	// The session file is keyed by the SHIM's pid, mirroring what the
	// shim's own in-process server would have written.
	assertSessionFileProject(t, home, shimPID, projID)
}

// TestDaemonHost_TwoConnections_DistinctCWDs_NoCrossTalk drives two
// concurrent socket sessions whose preambles carry different cwds; each
// must resolve its own project, into its own per-pid session file.
func TestDaemonHost_TwoConnections_DistinctCWDs_NoCrossTalk(t *testing.T) {
	home := isolateHome(t)
	dirA := filepath.Join(home, "ws", "proj-a")
	dirB := filepath.Join(home, "ws", "proj-b")
	registerProjectsForDaemon(t, map[string]string{"proj-a": dirA, "proj-b": dirB})

	sock := daemonSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startHostedDaemon(t, ctx, NewDaemonHost(), sock)

	const pidA, pidB = 888001, 888002
	csA, cleanupA := dialShimSession(t, sock, dirA, pidA)
	defer cleanupA()
	csB, cleanupB := dialShimSession(t, sock, dirB, pidB)
	defer cleanupB()

	// Bootstrap both sessions concurrently with NO project arg: cwd
	// inference is the only signal, so any shared ambient state between
	// the two connections shows up as cross-talk here.
	var wg sync.WaitGroup
	bodies := make([]string, 2)
	errs := make([]error, 2)
	bootstrap := func(i int, cs *sdkmcp.ClientSession) {
		defer wg.Done()
		res, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{
			Name:      "guild_session_start",
			Arguments: map[string]any{},
		})
		if err != nil {
			errs[i] = err
			return
		}
		bodies[i] = textOf(res.Content)
		if res.IsError {
			errs[i] = fmt.Errorf("IsError: %s", bodies[i])
		}
	}
	wg.Add(2)
	go bootstrap(0, csA)
	go bootstrap(1, csB)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("concurrent bootstrap: A=%v B=%v", errs[0], errs[1])
	}

	if !strings.Contains(bodies[0], "active project: proj-a") {
		t.Errorf("session A resolved wrong project; body:\n%s", bodies[0])
	}
	if !strings.Contains(bodies[1], "active project: proj-b") {
		t.Errorf("session B resolved wrong project; body:\n%s", bodies[1])
	}

	// On disk: one session file per shim pid, each with its own project.
	assertSessionFileProject(t, home, pidA, "proj-a")
	assertSessionFileProject(t, home, pidB, "proj-b")

	// A mid-session switch on B must not leak into A.
	res, err := csB.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "guild_set_project",
		Arguments: map[string]any{"project": "proj-a"},
	})
	if err != nil {
		t.Fatalf("guild_set_project on B: %v", err)
	}
	if res.IsError {
		t.Fatalf("guild_set_project on B IsError: %s", textOf(res.Content))
	}
	assertSessionFileProject(t, home, pidA, "proj-a")
	assertSessionFileProject(t, home, pidB, "proj-a")
}

// assertSessionFileProject reads ~/.guild/sessions/<pid>.json and
// asserts its active_project.
func assertSessionFileProject(t *testing.T, home string, pid int, want string) {
	t.Helper()
	path := filepath.Join(home, ".guild", "sessions", strconv.Itoa(pid)+".json")
	data, err := os.ReadFile(path) //nolint:gosec // test path built from t.TempDir
	if err != nil {
		t.Fatalf("read session file %s: %v", path, err)
	}
	var blob struct {
		ActiveProject string `json:"active_project"`
	}
	if err := json.Unmarshal(data, &blob); err != nil {
		t.Fatalf("parse %s: %v; raw=%q", path, err, data)
	}
	if blob.ActiveProject != want {
		t.Errorf("session file %s active_project = %q; want %q", path, blob.ActiveProject, want)
	}
}

// TestDaemonHost_OneProviderBundleAcrossSessions asserts the daemon's
// core economic win: any number of sessions share exactly one provider
// bundle, so the embed provider reconstructs once per process, not once
// per connection. Counted via the "embedder wired lazily" log line,
// same instrument as the multi-session NewServer test.
func TestDaemonHost_OneProviderBundleAcrossSessions(t *testing.T) {
	pid := isolateProject(t) // registers "testproj" under a temp HOME
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	host := NewDaemonHost()
	t.Cleanup(host.providers.closeHintsEngine)
	var logBuf safeBuffer
	host.providers.embed.logger = slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Two back-to-back sessions over in-memory pipes. ServeSession is
	// the exact handler the daemon listener invokes after the preamble;
	// the transport (unix socket vs pipe) is irrelevant to bundle
	// sharing and the socket path is covered by the e2e tests above.
	for i, shimPID := range []int{999001, 999002} {
		serverEnd, clientEnd := net.Pipe()
		done := make(chan error, 1)
		go func() {
			done <- host.ServeSession(ctx,
				daemon.ShimPreamble{Version: "t", CWD: "/nowhere", PID: shimPID},
				serverEnd)
		}()

		client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "guild-pipe-client"}, nil)
		cs, err := client.Connect(ctx, &sdkmcp.IOTransport{Reader: clientEnd, Writer: clientEnd}, nil)
		if err != nil {
			t.Fatalf("session %d connect: %v", i, err)
		}
		res, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{
			Name:      "lore_appraise",
			Arguments: map[string]any{"query": "daemon-bundle-smoke", "project": pid},
		})
		if err != nil {
			t.Fatalf("session %d lore_appraise: %v", i, err)
		}
		if res.IsError {
			t.Fatalf("session %d lore_appraise IsError: %s", i, textOf(res.Content))
		}
		_ = cs.Close()
		_ = clientEnd.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("session %d ServeSession returned %v; want nil on peer close", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("session %d ServeSession did not return after client close", i)
		}
	}

	// Exactly one reconstruct across both sessions: the second resolve
	// hit the shared provider's cache.
	wired := strings.Count(logBuf.String(), "embedder wired lazily")
	if wired != 1 {
		t.Errorf("expected exactly 1 embed reconstruct across 2 daemon sessions; got %d; logs:\n%s",
			wired, logBuf.String())
	}
}
