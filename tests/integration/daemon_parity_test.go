// daemon_parity_test.go: ADR-005 Part 1 byte-identical parity suite.
//
// The enforcement mechanism for the entire daemon phase. ADR-005's hard
// invariants for keeping the daemon optional are: no-daemon mode stays
// byte-identical to today, correctness never depends on the daemon, and
// both modes run the same smoke suite. This file is that smoke suite.
//
// Each scenario runs TWICE in two fresh, isolated HOMEs:
//
//   - arm A (daemon-down): ~/.guild/config.toml pins [daemon] autostart
//     = false, so `guild mcp serve` runs purely in-process. This is the
//     byte-identical baseline: the behavior a build without any daemon
//     support would produce.
//   - arm B (daemon-up): a real daemon is started via `guild daemon
//     start` (detached spawn), then the same scenario runs through `guild
//     mcp serve`, whose shim probes the running daemon and pipes the whole
//     stdio session to it. The CLI verbs likewise route through the daemon
//     via the JSON-exec RPC.
//
// Every tool result body (MCP) and every stdout/stderr pair (CLI) is
// captured in call order, run through a documented scrub pass that
// replaces legitimately volatile fields (timestamps, durations, pids,
// socket paths) with stable placeholders, then compared byte-for-byte
// across the two arms. Any divergence fails with a unified diff naming
// the first divergent call.
//
// Unix-only: the daemon listens on a unix domain socket; the daemon
// quests gate Windows out (no AF_UNIX discovery), so parity runs unix
// only, matching the daemon lifecycle test's build constraint.
//
//go:build unix

package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// parityProject is the fixed project basename both arms register. It must
// be identical across arms: the project name appears verbatim in
// guild_session_start output, so a per-arm random name would diverge.
const parityProject = "parityproj"

// daemonReadyTimeout bounds the post-start wait for arm B's daemon to
// publish a dialable socket + discovery file. Generous so a loaded CI
// runner does not flake; the poll exits the instant the daemon is ready.
const daemonReadyTimeout = 20 * time.Second

// ─────────────────────────── scrub list ─────────────────────────────
//
// Each rule rewrites ONE class of field that legitimately differs between
// a daemon-down and a daemon-up run of the identical scenario, or between
// two runs at different wall-clock instants. Deterministic IDs (QUEST-1,
// LORE-1, ...) assigned from fresh state match naturally across both arms
// and are intentionally NOT scrubbed, so an id-assignment regression
// would still fail the gate. Over-scrubbing weakens the gate; the list is
// kept minimal and every entry is justified.

// parityScrub is a single volatile-field normalization.
type parityScrub struct {
	re  *regexp.Regexp
	rep string
}

var parityScrubs = []parityScrub{
	// RFC3339 timestamps (e.g. last-briefing headers, started_at): wall
	// clock, never equal across two separate runs.
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?(\.\d+)?(Z|[+-]\d{2}:?\d{2})?\b`), "<TS>"},
	// Bare dates (e.g. an echo's age annotation): the two arms can in
	// principle straddle midnight on a slow runner.
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`), "<DATE>"},
	// Relative ages ("3d old", "5h ago"): clock-derived, run-relative.
	{regexp.MustCompile(`\b\d+[smhd] (old|ago)\b`), "<AGE> $1"},
	// Durations and ETAs ("extract=12ms", "ETA ~2s", "uptime=1.3s"):
	// hardware- and load-dependent timing.
	{regexp.MustCompile(`\b(\d+h)?(\d+m)?\d+(\.\d+)?(ns|µs|us|ms|s)\b`), "<DUR>"},
	// Embedder progress line: backfill coverage and ETA depend on
	// scheduling and CPU speed; "ready" vs "backfilling (...)" is timing,
	// not behavior. Normalize the whole line.
	{regexp.MustCompile(`(?m)^(\s*)embedder: .*$`), "${1}embedder: <EMBEDDER>"},
	// Probe cosine similarity is float math over whatever SIMD path the
	// host CPU exposes; pin the shape, not the digits.
	{regexp.MustCompile(`cosine=\d+(\.\d+)?`), "cosine=<COSINE>"},
	// Process ids ("pid=1234", "pid 1234"): the daemon and the shim are
	// distinct OS processes from the in-process arm; pids never match.
	{regexp.MustCompile(`\bpid[ =]\d+\b`), "pid=<PID>"},
	// Absolute temp-HOME socket / file paths: each arm has its own
	// os.MkdirTemp HOME, so any embedded "/tmp/gXXXX/.guild/..." path
	// differs. Collapse the per-run temp prefix; the suffix (kept) still
	// proves the path shape.
	{regexp.MustCompile(`/[^\s"]*/\.guild/`), "<GUILD>/"},
}

// ──────────────── daemon-only additive-line allowlist ───────────────
//
// Phase 3 introduces the first intentional daemon-ONLY additive output: the
// presence line in guild_session_start ("N agents active"), which exists
// only when a session registry is live, i.e. only in arm B. A naive
// byte-for-byte comparison would flag it as a divergence, so the suite
// consciously normalizes it away before comparing: every line whose trimmed
// text begins with an allowlisted prefix is dropped from BOTH arms' output.
//
// This is the deliberate amendment ADR-005 calls for, not a weakening: the
// allowlist is a closed, documented set. In arm A (daemon-down) there is no
// registry, so no presence line is ever emitted and the strip is a no-op,
// which keeps the no-daemon byte-identity check strict. In arm B the one
// daemon-only line is removed and EVERYTHING ELSE is still byte-compared, so
// any other daemon-induced divergence (a stray extra line, a changed body)
// still fails the gate. A future additive daemon-only line (for example a
// "while you slept" narration) must register its prefix here CONSCIOUSLY
// rather than slip past parity silently. TestParityAllowlistIsLoadBearing
// guards that the presence line genuinely needs this carve-out: drop the
// allowlist and the daemon-up transcript diverges.
var daemonOnlyLinePrefixes = []string{
	// guild_session_start presence line (ADR-005 Phase 3): emitted only
	// when served inside a running daemon, sourced from the in-memory
	// session registry. The 👥 prefix is the stable marker.
	"👥 ",
}

// stripDaemonOnlyLines removes every line whose trimmed text begins with an
// allowlisted daemon-only prefix. Applied as the final normalization step in
// scrubParity so it runs on both arms; in the daemon-down arm no such line
// exists, so it is a no-op.
func stripDaemonOnlyLines(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		drop := false
		for _, prefix := range daemonOnlyLinePrefixes {
			if strings.HasPrefix(trimmed, prefix) {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// scrubParity applies every volatile-field rule in order, then strips the
// allowlisted daemon-only additive lines.
func scrubParity(s string) string {
	for _, r := range parityScrubs {
		s = r.re.ReplaceAllString(s, r.rep)
	}
	return stripDaemonOnlyLines(s)
}

// ───────────────────────── recorded transcript ──────────────────────
//
// A transcript is an ordered list of labeled steps. The label names the
// call (e.g. `tools/call quest_post`); the body is its captured output.
// Comparison is per-step so a divergence can name the exact call index.

type parityStep struct {
	label string
	body  string
}

type parityTranscript struct {
	steps []parityStep
}

func (tr *parityTranscript) add(label, body string) {
	tr.steps = append(tr.steps, parityStep{label: label, body: scrubParity(body)})
}

// compareTranscripts asserts two transcripts are byte-identical step for
// step, failing with a unified diff that names the first divergent call.
func compareTranscripts(t *testing.T, what string, down, up *parityTranscript) {
	t.Helper()

	n := len(down.steps)
	if len(up.steps) < n {
		n = len(up.steps)
	}
	for i := 0; i < n; i++ {
		a, b := down.steps[i], up.steps[i]
		if a.label != b.label {
			t.Fatalf("%s: step %d label diverges:\n  daemon-down: %q\n  daemon-up:   %q",
				what, i, a.label, b.label)
		}
		if a.body != b.body {
			t.Fatalf("%s: output diverges at call %d (%s):\n%s",
				what, i, a.label, unifiedDiff(a.body, b.body))
		}
	}
	if len(down.steps) != len(up.steps) {
		t.Fatalf("%s: step count diverges: daemon-down=%d daemon-up=%d (first extra: %s)",
			what, len(down.steps), len(up.steps), extraStepLabel(down, up, n))
	}
}

func extraStepLabel(down, up *parityTranscript, n int) string {
	if len(down.steps) > n {
		return "daemon-down has " + down.steps[n].label
	}
	if len(up.steps) > n {
		return "daemon-up has " + up.steps[n].label
	}
	return ""
}

// unifiedDiff renders a minimal line-oriented diff between two bodies:
// enough to read the first divergent line straight out of a CI log
// without downloading artifacts.
func unifiedDiff(down, up string) string {
	dl := strings.Split(down, "\n")
	ul := strings.Split(up, "\n")
	n := len(dl)
	if len(ul) < n {
		n = len(ul)
	}
	for i := 0; i < n; i++ {
		if dl[i] != ul[i] {
			return fmt.Sprintf("first divergent line %d:\n  - daemon-down: %q\n  + daemon-up:   %q", i+1, dl[i], ul[i])
		}
	}
	if len(dl) != len(ul) {
		return fmt.Sprintf("bodies share %d lines, then lengths diverge: daemon-down=%d daemon-up=%d",
			n, len(dl), len(ul))
	}
	return "(scrubbed bodies are equal; divergence was pre-scrub, check the scrub list)"
}

// ───────────────────────── in-test MCP driver ───────────────────────
//
// A minimal newline-delimited JSON-RPC driver over a `guild mcp serve`
// subprocess. Deliberately raw (not the go-sdk client) so the full wire
// surface stays visible and the same driver works whether stdio is served
// in-process (arm A) or piped to the daemon socket (arm B): only the
// subprocess env differs between arms, never the framing.

type parityMCP struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdinW *os.File
	lines  chan string
	stderr *parityBuf
	nextID int
}

// parityBuf is a tiny mutex-free stderr sink read only after the process
// has been waited on (close()), so no concurrent access occurs.
type parityBuf struct{ b strings.Builder }

func (p *parityBuf) Write(b []byte) (int, error) { return p.b.Write(b) }
func (p *parityBuf) String() string              { return p.b.String() }

// openMCP starts `guild mcp serve` against homeDir with workDir as the
// process cwd (a git repo so project resolution and the daemon shim
// preamble both have a real path). extraEnv is appended after the minimal
// HOME+PATH base; arm A passes GUILD_NO_DAEMON=1, arm B passes nothing.
func openMCP(t *testing.T, homeDir, workDir string, extraEnv []string) *parityMCP {
	t.Helper()
	bin := requireBinary(t)

	//nolint:gosec // bin is from buildOnce (trusted), args are literals
	cmd := exec.Command(bin, "mcp", "serve")
	cmd.Env = append([]string{
		"HOME=" + homeDir,
		"PATH=" + os.Getenv("PATH"),
		"GUILD_NO_USAGE_LOG=1",
		"GUILD_NO_UPDATE_CHECK=1",
	}, extraEnv...)
	cmd.Dir = workDir
	// Own process group so a stuck server can be killed without taking
	// the test runner down with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("mcp stdin pipe: %v", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("mcp stdout pipe: %v", err)
	}
	stderr := &parityBuf{}
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start mcp serve: %v", err)
	}
	_ = stdinR.Close()
	_ = stdoutW.Close()

	m := &parityMCP{t: t, cmd: cmd, stdinW: stdinW, stderr: stderr, lines: make(chan string, 32)}
	go func() {
		defer close(m.lines)
		sc := bufio.NewScanner(stdoutR)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			m.lines <- sc.Text()
		}
		_ = stdoutR.Close()
	}()
	t.Cleanup(m.close)
	return m
}

func (m *parityMCP) close() {
	if m.cmd == nil {
		return
	}
	_ = m.stdinW.Close()
	done := make(chan struct{})
	go func() { _ = m.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = m.cmd.Process.Kill()
		<-done
	}
	m.cmd = nil
}

// rpc sends one request and blocks for the matching response, skipping
// any server-originated notifications. Returns the result envelope's
// raw `result` field.
func (m *parityMCP) rpc(method string, params any) json.RawMessage {
	m.t.Helper()
	m.nextID++
	id := m.nextID
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	data, err := json.Marshal(req)
	if err != nil {
		m.t.Fatalf("marshal %s: %v", method, err)
	}
	if _, err := fmt.Fprintf(m.stdinW, "%s\n", data); err != nil {
		m.t.Fatalf("write %s: %v (stderr:\n%s)", method, err, m.stderr.String())
	}

	deadline := time.After(90 * time.Second)
	for {
		select {
		case line, ok := <-m.lines:
			if !ok {
				m.t.Fatalf("mcp closed awaiting %s (stderr:\n%s)", method, m.stderr.String())
			}
			if strings.TrimSpace(line) == "" {
				continue
			}
			var env struct {
				ID     *int            `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(line), &env); err != nil {
				m.t.Fatalf("decode %q: %v", line, err)
			}
			if env.ID == nil || *env.ID != id || env.Method != "" {
				continue // notification or another id
			}
			if env.Error != nil {
				m.t.Fatalf("%s: jsonrpc error %d: %s", method, env.Error.Code, env.Error.Message)
			}
			return env.Result
		case <-deadline:
			m.t.Fatalf("timeout awaiting %s (stderr:\n%s)", method, m.stderr.String())
		}
	}
}

func (m *parityMCP) notify(method string, params any) {
	m.t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	data, _ := json.Marshal(req)
	if _, err := fmt.Fprintf(m.stdinW, "%s\n", data); err != nil {
		m.t.Fatalf("write notify %s: %v", method, err)
	}
}

// initialize runs the MCP handshake and returns the server's instructions
// fingerprint (length only: the full INSTRUCTIONS string is a large
// deterministic blob; its length proves it crossed the wire intact
// without inlining kilobytes into the transcript).
func (m *parityMCP) initialize() string {
	m.t.Helper()
	raw := m.rpc("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "guild-parity-harness", "version": "0.0.0"},
	})
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		m.t.Fatalf("decode initialize: %v", err)
	}
	m.notify("notifications/initialized", map[string]any{})
	// serverInfo.version is scrubbed out as a <VERSION> stamp elsewhere;
	// here both arms run the same binary so name + protocol + instruction
	// length are the stable, comparable surface.
	return fmt.Sprintf("serverInfo.name=%s protocolVersion=%s instructions=%d bytes",
		res.ServerInfo.Name, res.ProtocolVersion, len(res.Instructions))
}

// callTool invokes tools/call and returns the concatenated text content.
func (m *parityMCP) callTool(name string, args map[string]any) string {
	m.t.Helper()
	raw := m.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		m.t.Fatalf("decode tools/call %s: %v", name, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	if res.IsError {
		m.t.Fatalf("tools/call %s returned isError:\n%s", name, text.String())
	}
	return text.String()
}

// ───────────────────────── MCP scenario ─────────────────────────────

// runMCPScenario drives the canonical session over one `guild mcp serve`
// process and records every step. The sequence covers the spec's minimum
// surface: initialize, guild_session_start, quest_post, quest_list,
// quest_accept, lore_inscribe, lore_appraise, quest_brief.
func runMCPScenario(t *testing.T, homeDir, workDir string, extraEnv []string) *parityTranscript {
	t.Helper()
	tr := &parityTranscript{}
	m := openMCP(t, homeDir, workDir, extraEnv)

	tr.add("initialize", m.initialize())

	tr.add(`tools/call guild_session_start`,
		m.callTool("guild_session_start", map[string]any{"project": parityProject}))

	tr.add(`tools/call quest_post`,
		m.callTool("quest_post", map[string]any{
			"subject":    "wire the drydock beacon relay",
			"priority":   "P1",
			"acceptance": []string{"beacon relay answers a ping from the harness"},
		}))

	tr.add(`tools/call quest_list`,
		m.callTool("quest_list", map[string]any{}))

	tr.add(`tools/call quest_accept`,
		m.callTool("quest_accept", map[string]any{"quest_id": "QUEST-1"}))

	tr.add(`tools/call lore_inscribe`,
		m.callTool("lore_inscribe", map[string]any{
			"title":   "cobalt heron drydock survey",
			"kind":    "research",
			"summary": "Parity entry proving the MCP write path is mode-agnostic.",
			"topic":   "daemon-parity",
		}))

	tr.add(`tools/call lore_appraise`,
		m.callTool("lore_appraise", map[string]any{
			"query": "cobalt heron drydock",
			"limit": 1,
		}))

	tr.add(`tools/call quest_brief`,
		m.callTool("quest_brief", map[string]any{
			"text": "Beacon relay wired and verified by the parity harness; next is the capstone.",
		}))

	m.close()
	return tr
}

// ───────────────────────── CLI scenario ─────────────────────────────

// runCLIScenario runs a representative CLI verb sequence against homeDir,
// recording stdout + stderr + exit for each verb. extraEnv lets the
// daemon-down arm pin GUILD_NO_DAEMON=1 (mirrored into runArgsEnv below).
func runCLIScenario(t *testing.T, homeDir, workDir string, extraEnv []string) *parityTranscript {
	t.Helper()
	tr := &parityTranscript{}
	run := func(label string, argv ...string) {
		inv := runArgsEnv(context.Background(), t, homeDir, workDir, extraEnv, argv)
		tr.add(label+" [stdout]", inv.Stdout)
		tr.add(label+" [stderr]", inv.Stderr)
		tr.add(label+" [exit]", fmt.Sprintf("%d", inv.ExitCode))
	}

	run("quest post", "quest", "post", "wire the drydock beacon relay",
		"--priority", "P1", "--acceptance", "beacon relay answers a ping from the harness")
	run("quest list", "quest", "list")
	run("quest accept", "quest", "accept", "QUEST-1")
	run("lore inscribe", "lore", "inscribe", "cobalt heron drydock survey",
		"--kind", "research",
		"--summary", "Parity entry proving the CLI write path is mode-agnostic.",
		"--topic", "daemon-parity")
	run("lore appraise", "lore", "appraise", "cobalt heron drydock", "--limit", "1")
	return tr
}

// runArgsEnv is runArgs with caller-supplied extra env appended after the
// minimal HOME+PATH base, so the daemon-down CLI arm can pin
// GUILD_NO_DAEMON=1 (forcing in-process routing) while the daemon-up arm
// routes its CLI verbs through the running daemon's JSON-exec RPC.
func runArgsEnv(ctx context.Context, t *testing.T, homeDir, dir string, extraEnv, argv []string) Invocation {
	t.Helper()
	bin := requireBinary(t)

	//nolint:gosec // bin is trusted; argv is test-controlled
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Env = append([]string{
		"HOME=" + homeDir,
		"PATH=" + os.Getenv("PATH"),
		"GUILD_NO_USAGE_LOG=1",
		"GUILD_NO_UPDATE_CHECK=1",
	}, extraEnv...)
	cmd.Dir = dir

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	exitCode := 0
	if err != nil {
		var ee *exec.ExitError
		if asExitErr(err, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return Invocation{
		Stdout:   strings.TrimRight(stdout.String(), "\n"),
		Stderr:   strings.TrimRight(stderr.String(), "\n"),
		ExitCode: exitCode,
	}
}

// ───────────────────────── arm setup ────────────────────────────────

// pinAutostartOff writes ~/.guild/config.toml with [daemon] autostart =
// false, the documented opt-out that keeps `guild mcp serve` and the CLI
// verbs on the byte-identical in-process path with no daemon side effects.
func pinAutostartOff(t *testing.T, homeDir string) {
	t.Helper()
	dir := filepath.Join(homeDir, ".guild")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir .guild: %v", err)
	}
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte("[daemon]\nautostart = false\n"), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
}

// setupProject creates a git repo under homeDir and registers the fixed
// parity project in it (lore init + quest init), returning the project
// directory. Both arms register the same basename so project-name output
// matches across them.
func setupProject(t *testing.T, homeDir string) string {
	t.Helper()
	projDir := filepath.Join(homeDir, parityProject)
	_ = initProject(context.Background(), t, homeDir, projDir)
	return projDir
}

// startParityDaemon starts a detached daemon for arm B via `guild daemon
// start`, waits for it to publish a dialable socket + discovery file, and
// registers teardown that always SIGTERMs (escalating to SIGKILL) so a
// red test never leaks a daemon into the runner.
func startParityDaemon(t *testing.T, homeDir, workDir string) {
	t.Helper()

	// Teardown first so even a failed start is cleaned up.
	t.Cleanup(func() {
		_ = runArgsEnv(context.Background(), t, homeDir, workDir, nil,
			[]string{"daemon", "stop"})
	})

	inv := runArgsEnv(context.Background(), t, homeDir, workDir, nil,
		[]string{"daemon", "start"})
	if inv.ExitCode != 0 {
		t.Fatalf("daemon start: exit %d\nstdout: %s\nstderr: %s", inv.ExitCode, inv.Stdout, inv.Stderr)
	}

	// Readiness: status returns running (exit 0) once the socket is
	// dialable and discovery is published. Poll with a generous ceiling.
	deadline := time.Now().Add(daemonReadyTimeout)
	for {
		st := runArgsEnv(context.Background(), t, homeDir, workDir, nil,
			[]string{"daemon", "status"})
		if st.ExitCode == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not become ready within %s\nlast status stdout: %s\nstderr: %s",
				daemonReadyTimeout, st.Stdout, st.Stderr)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// assertNoDaemonSideEffects is arm A's negative backstop: after the
// daemon-down scenario, no daemon artifact may exist under the temp HOME,
// and no orphan daemon process may be running against it. The only
// permitted stat is the config-disabled opt-out file we wrote ourselves.
func assertNoDaemonSideEffects(t *testing.T, homeDir, workDir string) {
	t.Helper()
	guildDir := filepath.Join(homeDir, ".guild")

	for _, name := range []string{"daemon.json", "daemon.sock", "daemon.lock", "daemon.log"} {
		p := filepath.Join(guildDir, name)
		if _, err := os.Stat(p); err == nil {
			t.Errorf("arm A leaked daemon artifact %s (autostart was pinned off)", p)
		} else if !os.IsNotExist(err) {
			t.Errorf("arm A stat %s: %v", p, err)
		}
	}

	// No daemon process: `daemon status` against this HOME must report
	// not-running (exit 3). A spawned daemon would flip this to exit 0.
	st := runArgsEnv(context.Background(), t, homeDir, workDir, nil,
		[]string{"daemon", "status"})
	if st.ExitCode != 3 {
		t.Errorf("arm A: daemon status exit %d, want 3 (not running); a daemon was spawned despite autostart off\nstdout: %s",
			st.ExitCode, st.Stdout)
	}
}

// ───────────────────────── parity tests ─────────────────────────────

// TestDaemonParity_MCP runs the canonical MCP scenario daemon-down and
// daemon-up and asserts the two scrubbed transcripts are byte-identical,
// then asserts arm A produced no daemon side effects.
func TestDaemonParity_MCP(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping daemon parity (subprocess + daemon) in -short mode")
	}

	// ── arm A: daemon-down ──────────────────────────────────────────
	homeA := shortHome(t)
	pinAutostartOff(t, homeA)
	projA := setupProject(t, homeA)
	// GUILD_NO_DAEMON=1 belt-and-suspenders on top of the config opt-out:
	// either alone forces the in-process path; both together make the
	// no-daemon intent unmistakable to a future reader.
	down := runMCPScenario(t, homeA, projA, []string{"GUILD_NO_DAEMON=1"})
	assertNoDaemonSideEffects(t, homeA, projA)

	// ── arm B: daemon-up ────────────────────────────────────────────
	homeB := shortHome(t)
	projB := setupProject(t, homeB)
	startParityDaemon(t, homeB, projB)
	// No GUILD_NO_DAEMON: the shim probes the running daemon and pipes
	// the whole stdio session to it (RunningMatch, same binary).
	up := runMCPScenario(t, homeB, projB, nil)

	compareTranscripts(t, "MCP parity", down, up)
}

// TestParityAllowlistIsLoadBearing proves the daemon-only allowlist carve-out
// is doing real work: the presence line genuinely diverges the two arms, and
// only the strip reconciles them. It guards two ways at once:
//
//   - WITHOUT the strip (volatile scrubs only) the daemon-up body (with the
//     presence line) and the daemon-down body (without it) DIVERGE, so the
//     carve-out is necessary, not cosmetic.
//   - WITH the full scrubParity (which includes the strip) they MATCH, so the
//     carve-out is sufficient and removing the presence prefix from the
//     allowlist while the line still renders would reintroduce the failure.
//
// If a future change stops emitting the presence line, the "without strip"
// arm stops diverging and this test fails, forcing the allowlist entry to be
// retired alongside the line rather than lingering as dead normalization.
func TestParityAllowlistIsLoadBearing(t *testing.T) {
	const presencePrefix = "👥 "

	// The presence line must be covered by the allowlist, or the carve-out
	// it is supposed to prove does not exist.
	covered := false
	for _, p := range daemonOnlyLinePrefixes {
		if p == presencePrefix {
			covered = true
			break
		}
	}
	if !covered {
		t.Fatalf("presence prefix %q is not in daemonOnlyLinePrefixes; the carve-out is missing", presencePrefix)
	}

	// Two session_start bodies that differ ONLY by the daemon-only presence
	// line, matching the real shape: header, board summary, then (daemon-up
	// only) the presence line, then the briefing body.
	const board = "📊 board: 1 oaths, 2 bounties, 0 echoes\n"
	daemonDown := "📍 active project: parityproj\n" + board + "\n📋 last briefing: (none yet)\n"
	daemonUp := "📍 active project: parityproj\n" + board + presencePrefix + "1 agent active\n" + "\n📋 last briefing: (none yet)\n"

	// Volatile scrubs only (no strip): the bodies must diverge, proving the
	// presence line is a real, unmasked difference between the arms.
	downScrubbed := applyVolatileScrubs(daemonDown)
	upScrubbed := applyVolatileScrubs(daemonUp)
	if downScrubbed == upScrubbed {
		t.Fatal("without the allowlist strip the two arms already match; the carve-out proves nothing (the presence line is not actually divergent)")
	}

	// Full scrub (volatile + strip): now they must match, proving the strip
	// is exactly what reconciles the daemon-only line.
	if got, want := scrubParity(daemonUp), scrubParity(daemonDown); got != want {
		t.Fatalf("with the allowlist strip the arms still diverge:\n%s", unifiedDiff(want, got))
	}
}

// applyVolatileScrubs runs only the volatile-field rules, deliberately
// skipping the daemon-only-line strip, so a test can observe the pre-strip
// divergence the allowlist exists to reconcile.
func applyVolatileScrubs(s string) string {
	for _, r := range parityScrubs {
		s = r.re.ReplaceAllString(s, r.rep)
	}
	return s
}

// TestDaemonParity_CLI runs the representative CLI verb sequence in both
// modes and asserts stdout + stderr + exit are byte-identical per verb.
// This is the acceptance backstop for the CLI-routing byte-identical
// claim: routing a verb through the daemon's JSON-exec RPC must produce
// exactly the in-process bytes.
func TestDaemonParity_CLI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping daemon parity (subprocess + daemon) in -short mode")
	}

	// ── arm A: daemon-down ──────────────────────────────────────────
	homeA := shortHome(t)
	pinAutostartOff(t, homeA)
	projA := setupProject(t, homeA)
	down := runCLIScenario(t, homeA, projA, []string{"GUILD_NO_DAEMON=1"})
	assertNoDaemonSideEffects(t, homeA, projA)

	// ── arm B: daemon-up ────────────────────────────────────────────
	homeB := shortHome(t)
	projB := setupProject(t, homeB)
	startParityDaemon(t, homeB, projB)
	up := runCLIScenario(t, homeB, projB, nil)

	compareTranscripts(t, "CLI parity", down, up)
}
