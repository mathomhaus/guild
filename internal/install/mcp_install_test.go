package install

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// stdClientOpts builds MCPInstallOptions for tests. It creates a real
// temporary file to satisfy the stat-exists check in resolveAbsBinPath;
// callers should use the returned fakeBin string when asserting output.
func stdClientOpts(t *testing.T, clients []Client, out *bytes.Buffer) (opts MCPInstallOptions, fakeBin string) {
	t.Helper()
	dir := t.TempDir()
	fakeBin = dir + "/guild"
	if err := os.WriteFile(fakeBin, []byte{}, 0o600); err != nil {
		t.Fatalf("stdClientOpts: create temp binary: %v", err)
	}
	bin := fakeBin
	opts = MCPInstallOptions{
		Out:          out,
		In:           &bytes.Buffer{},
		clients:      clients,
		executableFn: func() (string, error) { return bin, nil },
		// Fixtures use synthetic argv[0] names ("claude", "cursor", etc.)
		// that don't exist on CI runners. Stub PATH resolution so the
		// missing-CLI guard introduced for issue #48 doesn't skip them.
		lookPathFn: func(name string) (string, error) { return name, nil },
	}
	return opts, fakeBin
}

// alwaysDetected returns a Client that always reports detected.
// installArgv is a func that returns the structured argv for the install command.
func alwaysDetected(name string, installArgv func(string) []string) Client {
	return Client{
		Name:        name,
		CLIProbe:    "go", // "go" is always on PATH in the test env
		InstallArgv: installArgv,
	}
}

// neverDetected returns a Client that is never detected.
func neverDetected(name string, installArgv func(string) []string) Client {
	return Client{
		Name:        name,
		CLIProbe:    "nonexistent-cli-binary-xyzzy-99",
		InstallArgv: installArgv,
	}
}

// claudeArgv returns the standard Claude Code InstallArgv for tests.
func claudeArgv(b string) []string {
	return []string{"claude", "mcp", "add", "guild", "--scope", "user", "--", b, "mcp", "serve"}
}

// cursorArgv returns the standard Cursor InstallArgv for tests.
func cursorArgv(b string) []string {
	return []string{"cursor", "mcp", "add", "guild", "--", b, "mcp", "serve"}
}

// ---------------------------------------------------------------------------
// Detection + print output
// ---------------------------------------------------------------------------

func TestMCPInstall_AllClientsDetected_PrintsCommands(t *testing.T) {
	c1 := alwaysDetected("Claude Code", claudeArgv)
	c2 := alwaysDetected("Cursor", cursorArgv)

	var buf bytes.Buffer
	opts, fakeBin := stdClientOpts(t, []Client{c1, c2}, &buf)

	result, err := MCPInstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("MCPInstall: %v", err)
	}

	if len(result.Instructions) != 2 {
		t.Errorf("instructions: got %d, want 2", len(result.Instructions))
	}
	if len(result.NotDetected) != 0 {
		t.Errorf("unexpected not-detected: %v", result.NotDetected)
	}

	out := buf.String()
	// Binary path header.
	if !strings.Contains(out, fakeBin) {
		t.Errorf("output missing binary path; got:\n%s", out)
	}
	// Detected markers.
	if !strings.Contains(out, "✓ Claude Code") {
		t.Errorf("output missing '✓ Claude Code'; got:\n%s", out)
	}
	if !strings.Contains(out, "✓ Cursor") {
		t.Errorf("output missing '✓ Cursor'; got:\n%s", out)
	}
	// Install commands.
	if !strings.Contains(out, "claude mcp add guild --scope user --") {
		t.Errorf("output missing claude install command; got:\n%s", out)
	}
	if !strings.Contains(out, "cursor mcp add guild --") {
		t.Errorf("output missing cursor install command; got:\n%s", out)
	}
}

func TestMCPInstall_SomeClientsDetected(t *testing.T) {
	c1 := neverDetected("Claude Code", claudeArgv)
	c2 := alwaysDetected("Cursor", cursorArgv)

	var buf bytes.Buffer
	opts, _ := stdClientOpts(t, []Client{c1, c2}, &buf)

	result, err := MCPInstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("MCPInstall: %v", err)
	}

	if len(result.Instructions) != 1 || result.Instructions[0].Name != "Cursor" {
		t.Errorf("instructions: got %v, want [Cursor]", result.Instructions)
	}
	if len(result.NotDetected) != 1 || result.NotDetected[0] != "Claude Code" {
		t.Errorf("not-detected: got %v, want [Claude Code]", result.NotDetected)
	}

	out := buf.String()
	if !strings.Contains(out, "✗ Claude Code") {
		t.Errorf("output missing '✗ Claude Code'; got:\n%s", out)
	}
	if !strings.Contains(out, "✓ Cursor") {
		t.Errorf("output missing '✓ Cursor'; got:\n%s", out)
	}
	// Claude Code install command must NOT appear.
	if strings.Contains(out, "claude mcp add") {
		t.Errorf("output contains claude command for undetected client; got:\n%s", out)
	}
}

func TestMCPInstall_NoneDetected(t *testing.T) {
	c1 := neverDetected("Claude Code", claudeArgv)
	c2 := neverDetected("Cursor", cursorArgv)

	var buf bytes.Buffer
	opts, _ := stdClientOpts(t, []Client{c1, c2}, &buf)

	result, err := MCPInstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("MCPInstall: %v", err)
	}

	if len(result.Instructions) != 0 {
		t.Errorf("expected no instructions, got: %v", result.Instructions)
	}
	if !strings.Contains(buf.String(), "No MCP client detected") {
		t.Errorf("expected 'No MCP client detected' message; got:\n%s", buf.String())
	}
}

// ---------------------------------------------------------------------------
// --print-config: only JSON snippet, no detection output
// ---------------------------------------------------------------------------

func TestMCPInstall_PrintConfig(t *testing.T) {
	c1 := alwaysDetected("Claude Code", claudeArgv)

	dir := t.TempDir()
	fakeBin := dir + "/guild"
	if err := os.WriteFile(fakeBin, []byte{}, 0o600); err != nil {
		t.Fatalf("create temp binary: %v", err)
	}

	var buf bytes.Buffer
	opts := MCPInstallOptions{
		PrintConfig:  true,
		Out:          &buf,
		In:           &bytes.Buffer{},
		clients:      []Client{c1},
		executableFn: func() (string, error) { return fakeBin, nil },
	}

	if _, err := MCPInstall(context.Background(), opts); err != nil {
		t.Fatalf("MCPInstall --print-config: %v", err)
	}

	out := buf.String()
	// Must contain the binary path.
	if !strings.Contains(out, fakeBin) {
		t.Errorf("output missing binary path; got:\n%s", out)
	}
	// Must contain mcpServers shape.
	if !strings.Contains(out, "mcpServers") {
		t.Errorf("output missing 'mcpServers'; got:\n%s", out)
	}
	// Must NOT contain detection output.
	if strings.Contains(out, "Detected agent clients") {
		t.Errorf("--print-config output contains detection output; got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// --skill returns not-implemented
// ---------------------------------------------------------------------------

func TestMCPInstall_SkillNotImplemented(t *testing.T) {
	var buf bytes.Buffer
	opts := MCPInstallOptions{
		Skill: true,
		Out:   &buf,
		// executableFn not needed — Skill check fires before resolve.
	}
	_, err := MCPInstall(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error for --skill, got nil")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("error should say 'not yet implemented', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// resolveAbsBinPath
// ---------------------------------------------------------------------------

func TestResolveAbsBinPath_Absolute(t *testing.T) {
	// Create a real file on disk so the stat-exists check passes and
	// the function accepts it as a durable installed binary.
	dir := t.TempDir()
	bin := dir + "/guild"
	if err := os.WriteFile(bin, []byte{}, 0o600); err != nil {
		t.Fatalf("create temp binary: %v", err)
	}
	got, err := resolveAbsBinPath(func() (string, error) { return bin, nil })
	if err != nil {
		t.Fatalf("resolveAbsBinPath: %v", err)
	}
	if got != bin {
		t.Errorf("got %q, want %q", got, bin)
	}
}

// TestResolveAbsBinPath_CachePath verifies that a go-run build-cache path is
// never returned — the function must fall through to a durable location
// (or error) rather than emit the ephemeral cache binary.
func TestResolveAbsBinPath_CachePath(t *testing.T) {
	// Simulate a go-run cache path that exists on disk.
	cacheDir := t.TempDir()
	cachePath := cacheDir + "/go-build123/b001/exe/guild"
	if err := os.MkdirAll(cacheDir+"/go-build123/b001/exe", 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(cachePath, []byte{}, 0o600); err != nil {
		t.Fatalf("create cache binary: %v", err)
	}

	// Create a durable installed binary the fallback probes can find.
	durableDir := t.TempDir()
	durableBin := durableDir + "/guild"
	if err := os.WriteFile(durableBin, []byte{}, 0o600); err != nil {
		t.Fatalf("create durable binary: %v", err)
	}

	t.Setenv("GOBIN", durableDir)
	t.Setenv("GOPATH", "")

	got, err := resolveAbsBinPath(func() (string, error) { return cachePath, nil })
	if err != nil {
		t.Fatalf("resolveAbsBinPath: %v", err)
	}
	if got == cachePath {
		t.Errorf("returned cache path %q — must never emit a go-build cache path", cachePath)
	}
	if got != durableBin {
		t.Errorf("got %q, want durable bin %q", got, durableBin)
	}
}

// TestResolveAbsBinPath_CachePath_NoFallback verifies that an error is returned
// when the path is a cache path and no durable binary can be found.
func TestResolveAbsBinPath_CachePath_NoFallback(t *testing.T) {
	cacheDir := t.TempDir()
	cachePath := cacheDir + "/go-build999/b001/exe/guild"
	if err := os.MkdirAll(cacheDir+"/go-build999/b001/exe", 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(cachePath, []byte{}, 0o600); err != nil {
		t.Fatalf("create cache binary: %v", err)
	}

	// Point env vars at non-existent locations so all probes fail.
	t.Setenv("GOBIN", "/nonexistent-gobin-xyzzy")
	t.Setenv("GOPATH", "/nonexistent-gopath-xyzzy")
	t.Setenv("HOME", t.TempDir()) // avoids ~/go/bin hitting a real install

	// PATH must not contain guild for LookPath to fail too.
	// We cannot guarantee that in all CI environments, so skip if guild is
	// found on PATH outside of GOBIN/GOPATH.
	if _, err := exec.LookPath("guild"); err == nil {
		t.Skip("guild found on PATH; cannot test no-fallback scenario")
	}

	_, err := resolveAbsBinPath(func() (string, error) { return cachePath, nil })
	if err == nil {
		t.Error("expected error when cache path is provided and no durable binary exists")
	}
}

// ---------------------------------------------------------------------------
// --run: shells out to client CLI
// ---------------------------------------------------------------------------

func TestMCPInstall_Run_WithYes(t *testing.T) {
	var executedCmds []string

	c1 := alwaysDetected("Claude Code", claudeArgv)
	c2 := alwaysDetected("Cursor", cursorArgv)

	dir := t.TempDir()
	fakeBin := dir + "/guild"
	if err := os.WriteFile(fakeBin, []byte{}, 0o600); err != nil {
		t.Fatalf("create temp binary: %v", err)
	}

	var buf bytes.Buffer
	opts := MCPInstallOptions{
		Run:          true,
		Yes:          true,
		Out:          &buf,
		In:           &bytes.Buffer{},
		clients:      []Client{c1, c2},
		executableFn: func() (string, error) { return fakeBin, nil },
		execCmdFn: func(name string, arg ...string) *exec.Cmd {
			executedCmds = append(executedCmds, name)
			// Return a no-op command that exits 0.
			return exec.Command("true")
		},
		lookPathFn: func(name string) (string, error) { return name, nil },
	}

	result, err := MCPInstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("MCPInstall --run --yes: %v", err)
	}

	if len(result.Ran) != 2 {
		t.Errorf("Ran: got %v, want 2 entries", result.Ran)
	}
	if len(executedCmds) != 2 {
		t.Errorf("executed %d commands, want 2", len(executedCmds))
	}

	// QUEST-10: --run output must NOT include the verbose preview
	// sections (the per-client "# Claude Code / claude mcp add ..."
	// pre-prompt block, or the manual JSON "mcpServers" snippet).
	// Those sections are redundant noise when the user has committed
	// to shelling out via --run.
	out := buf.String()
	if strings.Contains(out, "Run the command for each agent you use:") {
		t.Errorf("--run output still contains verbose recommended-commands header:\n%s", out)
	}
	if strings.Contains(out, "mcpServers") {
		t.Errorf("--run output still contains manual JSON snippet:\n%s", out)
	}
	// But it MUST include a compact one-liner listing the detected clients.
	if !strings.Contains(out, "Detected: ") {
		t.Errorf("--run output missing compact 'Detected:' summary:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// instruction Cmd contains binary path
// ---------------------------------------------------------------------------

func TestMCPInstall_InstructionCmdContainsBinPath(t *testing.T) {
	c1 := alwaysDetected("Claude Code", claudeArgv)

	var buf bytes.Buffer
	opts, fakeBin := stdClientOpts(t, []Client{c1}, &buf)

	result, err := MCPInstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("MCPInstall: %v", err)
	}
	if len(result.Instructions) != 1 {
		t.Fatalf("expected 1 instruction, got %d", len(result.Instructions))
	}
	if !strings.Contains(result.Instructions[0].Cmd, fakeBin) {
		t.Errorf("instruction cmd %q does not contain binary path %q",
			result.Instructions[0].Cmd, fakeBin)
	}
}

// ---------------------------------------------------------------------------
// Binary path with spaces — regression for the strings.Fields split bug
// ---------------------------------------------------------------------------

// TestMCPInstall_SpacyBinPath_Display verifies that a binary path containing
// spaces is quoted in the displayed command so users can copy/paste safely.
func TestMCPInstall_SpacyBinPath_Display(t *testing.T) {
	// Create the temp dir with a space-containing name and a real file so
	// resolveAbsBinPath stat-check accepts it.
	parent := t.TempDir()
	spacyDir := parent + "/Users Jane Doe/go/bin"
	if err := os.MkdirAll(spacyDir, 0o700); err != nil {
		t.Fatalf("mkdir spacy dir: %v", err)
	}
	spacyBin := spacyDir + "/guild"
	if err := os.WriteFile(spacyBin, []byte{}, 0o600); err != nil {
		t.Fatalf("create spacy binary: %v", err)
	}

	c1 := alwaysDetected("Claude Code", claudeArgv)

	var buf bytes.Buffer
	opts := MCPInstallOptions{
		Out:          &buf,
		In:           &bytes.Buffer{},
		clients:      []Client{c1},
		executableFn: func() (string, error) { return spacyBin, nil },
	}

	result, err := MCPInstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("MCPInstall: %v", err)
	}
	if len(result.Instructions) != 1 {
		t.Fatalf("expected 1 instruction, got %d", len(result.Instructions))
	}

	cmd := result.Instructions[0].Cmd
	// The binary path must be quoted in the display string so that a space
	// inside it doesn't look like an argument boundary.
	if strings.Contains(cmd, " "+spacyBin+" ") || strings.HasSuffix(cmd, " "+spacyBin) {
		t.Errorf("display command contains unquoted spacy path; got: %s", cmd)
	}
	// The path itself (sans quotes) must still appear in the output.
	if !strings.Contains(buf.String(), "Users Jane Doe") {
		t.Errorf("output missing spacy path; got:\n%s", buf.String())
	}
}

// TestMCPInstall_SpacyBinPath_Run verifies that --run with a space-containing
// binary path passes the path as a single argv token — not split by spaces.
func TestMCPInstall_SpacyBinPath_Run(t *testing.T) {
	parent := t.TempDir()
	spacyDir := parent + "/Users Jane Doe/go/bin"
	if err := os.MkdirAll(spacyDir, 0o700); err != nil {
		t.Fatalf("mkdir spacy dir: %v", err)
	}
	spacyBin := spacyDir + "/guild"
	if err := os.WriteFile(spacyBin, []byte{}, 0o600); err != nil {
		t.Fatalf("create spacy binary: %v", err)
	}

	var capturedArgv []string
	c1 := alwaysDetected("Claude Code", claudeArgv)

	var buf bytes.Buffer
	opts := MCPInstallOptions{
		Run:          true,
		Yes:          true,
		Out:          &buf,
		In:           &bytes.Buffer{},
		clients:      []Client{c1},
		executableFn: func() (string, error) { return spacyBin, nil },
		execCmdFn: func(name string, arg ...string) *exec.Cmd {
			capturedArgv = append([]string{name}, arg...)
			return exec.Command("true")
		},
		lookPathFn: func(name string) (string, error) { return name, nil },
	}

	if _, err := MCPInstall(context.Background(), opts); err != nil {
		t.Fatalf("MCPInstall --run --yes: %v", err)
	}

	// The spacy binary path must appear as exactly one argv token.
	found := false
	for _, tok := range capturedArgv {
		if tok == spacyBin {
			found = true
		}
		// No token should be just the first space-split fragment.
		if tok == "/Users Jane Doe/go/bin" || tok == parent+"/Users" {
			t.Errorf("argv was split on space: token %q", tok)
		}
	}
	if !found {
		t.Errorf("spacy binary path not found as a single argv token; got: %v", capturedArgv)
	}
}

// ---------------------------------------------------------------------------
// --run: skips clients whose install CLI is not on PATH (#48)
// ---------------------------------------------------------------------------

// TestMCPInstall_Run_SkipsWhenCLIMissing verifies that when a client's
// install argv[0] cannot be resolved via exec.LookPath, MCPInstall does
// not attempt to run it, records the skip in SkippedMissingCLI, and
// prints the one-line notice.
func TestMCPInstall_Run_SkipsWhenCLIMissing(t *testing.T) {
	var executed int

	badBinary := "nonexistent-install-cli-xyzzy-99"
	c := alwaysDetected("Bogus", func(b string) []string {
		return []string{badBinary, "mcp", "add", "guild", "--", b}
	})

	dir := t.TempDir()
	fakeBin := dir + "/guild"
	if err := os.WriteFile(fakeBin, []byte{}, 0o600); err != nil {
		t.Fatalf("create temp binary: %v", err)
	}

	var buf bytes.Buffer
	opts := MCPInstallOptions{
		Run:          true,
		Yes:          true,
		Out:          &buf,
		In:           &bytes.Buffer{},
		clients:      []Client{c},
		executableFn: func() (string, error) { return fakeBin, nil },
		execCmdFn: func(name string, arg ...string) *exec.Cmd {
			executed++
			return exec.Command("true")
		},
	}

	result, err := MCPInstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("MCPInstall --run --yes: %v", err)
	}
	if executed != 0 {
		t.Errorf("execCmdFn called %d times; expected 0 (CLI missing on PATH)", executed)
	}
	if got := len(result.SkippedMissingCLI); got != 1 {
		t.Errorf("SkippedMissingCLI len = %d, want 1", got)
	}
	if len(result.Ran) != 0 {
		t.Errorf("Ran = %v, want empty", result.Ran)
	}
	if !strings.Contains(buf.String(), "skipping Bogus: "+badBinary+" not on PATH") {
		t.Errorf("expected missing-CLI notice; got:\n%s", buf.String())
	}
}

// ---------------------------------------------------------------------------
// --run: pre-check skips clients that already have guild registered (#27)
// ---------------------------------------------------------------------------

// TestMCPInstall_Run_SkipsAlreadyRegistered verifies that when the
// client's ListArgv reports an existing guild entry, MCPInstall does
// NOT invoke the install command a second time and instead prints the
// skip notice — the behaviour fixed by issue #27.
func TestMCPInstall_Run_SkipsAlreadyRegistered(t *testing.T) {
	var installCalls, listCalls int

	c := alwaysDetected("Claude Code", claudeArgv)
	c.ListArgv = func() []string { return []string{"claude-mcp-list"} }

	dir := t.TempDir()
	fakeBin := dir + "/guild"
	if err := os.WriteFile(fakeBin, []byte{}, 0o600); err != nil {
		t.Fatalf("create temp binary: %v", err)
	}

	var buf bytes.Buffer
	opts := MCPInstallOptions{
		Run:          true,
		Yes:          true,
		Out:          &buf,
		In:           &bytes.Buffer{},
		clients:      []Client{c},
		executableFn: func() (string, error) { return fakeBin, nil },
		execCmdFn: func(name string, arg ...string) *exec.Cmd {
			switch name {
			case "claude-mcp-list":
				listCalls++
				//nolint:gosec // test-only: fakeBin is generated by t.TempDir.
				return exec.Command("printf", "guild: "+fakeBin+" mcp serve\n")
			case "claude":
				installCalls++
				return exec.Command("true")
			}
			return exec.Command("false")
		},
		lookPathFn: func(name string) (string, error) { return name, nil },
	}

	result, err := MCPInstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("MCPInstall --run --yes: %v", err)
	}

	if listCalls != 1 {
		t.Errorf("list probe ran %d times, want 1", listCalls)
	}
	if installCalls != 0 {
		t.Errorf("install ran %d times, want 0 (already registered)", installCalls)
	}
	if got := len(result.AlreadyRegistered); got != 1 {
		t.Errorf("AlreadyRegistered len = %d, want 1", got)
	}
	if len(result.Ran) != 0 {
		t.Errorf("Ran = %v, want empty", result.Ran)
	}
	if !strings.Contains(buf.String(), "already registered in Claude Code") {
		t.Errorf("output missing skip notice:\n%s", buf.String())
	}
}

// TestMCPInstall_Run_RegistersWhenAbsent verifies the pre-check does
// NOT suppress installation when the list output doesn't contain a
// guild entry — the first-run path must remain unchanged.
func TestMCPInstall_Run_RegistersWhenAbsent(t *testing.T) {
	var installCalls int

	c := alwaysDetected("Claude Code", claudeArgv)
	c.ListArgv = func() []string { return []string{"claude-mcp-list"} }

	dir := t.TempDir()
	fakeBin := dir + "/guild"
	if err := os.WriteFile(fakeBin, []byte{}, 0o600); err != nil {
		t.Fatalf("create temp binary: %v", err)
	}

	var buf bytes.Buffer
	opts := MCPInstallOptions{
		Run:          true,
		Yes:          true,
		Out:          &buf,
		In:           &bytes.Buffer{},
		clients:      []Client{c},
		executableFn: func() (string, error) { return fakeBin, nil },
		execCmdFn: func(name string, arg ...string) *exec.Cmd {
			switch name {
			case "claude-mcp-list":
				return exec.Command("printf", "other-server: /tmp/other\n")
			case "claude":
				installCalls++
				return exec.Command("true")
			}
			return exec.Command("false")
		},
		lookPathFn: func(name string) (string, error) { return name, nil },
	}

	result, err := MCPInstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("MCPInstall --run --yes: %v", err)
	}
	if installCalls != 1 {
		t.Errorf("install ran %d times, want 1", installCalls)
	}
	if len(result.Ran) != 1 {
		t.Errorf("Ran = %v, want one entry", result.Ran)
	}
	if len(result.AlreadyRegistered) != 0 {
		t.Errorf("AlreadyRegistered = %v, want empty", result.AlreadyRegistered)
	}
}

// TestScanForGuildEntry verifies the line-shape matcher used by the
// pre-check. It accepts the common CLI output formats and rejects
// incidental mentions of "guild" inside command-value strings.
func TestScanForGuildEntry(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		want     bool
		wantPath string
	}{
		{"claude human", "guild: /usr/local/bin/guild mcp serve\n", true, "/usr/local/bin/guild"},
		{"list marker", "- guild\n- other\n", true, ""},
		{"bare token", "guild\n", true, ""},
		{"mixed list", "  * other\n  * guild: /bin/guild\n", true, "/bin/guild"},
		{"empty", "", false, ""},
		{"only other", "other: /bin/other mcp serve\n", false, ""},
		{"mention inside value only", "other: /path/to/guild-wrapper mcp serve\n", false, ""},
		{"colon no path", "guild:\n", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, path := scanForGuildEntry([]byte(tc.in))
			if got != tc.want {
				t.Errorf("scanForGuildEntry(%q) found = %v, want %v", tc.in, got, tc.want)
			}
			if path != tc.wantPath {
				t.Errorf("scanForGuildEntry(%q) path = %q, want %q", tc.in, path, tc.wantPath)
			}
		})
	}
}

// TestMCPInstall_Run_NilListArgv_FallsThrough verifies that a client whose
// ListArgv is nil (no list-MCP CLI shape known) skips the probe entirely
// and proceeds with the install as the unconditional first-run path did.
func TestMCPInstall_Run_NilListArgv_FallsThrough(t *testing.T) {
	var installCalls int

	c := alwaysDetected("Claude Code", claudeArgv)
	c.ListArgv = nil

	dir := t.TempDir()
	fakeBin := dir + "/guild"
	if err := os.WriteFile(fakeBin, []byte{}, 0o600); err != nil {
		t.Fatalf("create temp binary: %v", err)
	}

	var buf bytes.Buffer
	opts := MCPInstallOptions{
		Run:          true,
		Yes:          true,
		Out:          &buf,
		In:           &bytes.Buffer{},
		clients:      []Client{c},
		executableFn: func() (string, error) { return fakeBin, nil },
		lookPathFn:   func(name string) (string, error) { return name, nil },
		execCmdFn: func(name string, arg ...string) *exec.Cmd {
			if name == "claude" {
				installCalls++
				return exec.Command("true")
			}
			t.Errorf("unexpected exec call: %s %v", name, arg)
			return exec.Command("false")
		},
	}

	result, err := MCPInstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("MCPInstall --run --yes: %v", err)
	}

	if installCalls != 1 {
		t.Errorf("install ran %d times, want 1", installCalls)
	}
	if len(result.AlreadyRegistered) != 0 {
		t.Errorf("AlreadyRegistered = %v, want empty (probe disabled)", result.AlreadyRegistered)
	}
}

// TestMCPInstall_Run_FailingProbe_FallsThrough verifies that a probe that
// errors (non-zero exit / unreadable output) does not block installation —
// the docstring on isGuildRegistered explicitly calls this out.
func TestMCPInstall_Run_FailingProbe_FallsThrough(t *testing.T) {
	var installCalls, listCalls int

	c := alwaysDetected("Claude Code", claudeArgv)
	c.ListArgv = func() []string { return []string{"claude-mcp-list"} }

	dir := t.TempDir()
	fakeBin := dir + "/guild"
	if err := os.WriteFile(fakeBin, []byte{}, 0o600); err != nil {
		t.Fatalf("create temp binary: %v", err)
	}

	var buf bytes.Buffer
	opts := MCPInstallOptions{
		Run:          true,
		Yes:          true,
		Out:          &buf,
		In:           &bytes.Buffer{},
		clients:      []Client{c},
		executableFn: func() (string, error) { return fakeBin, nil },
		lookPathFn:   func(name string) (string, error) { return name, nil },
		execCmdFn: func(name string, arg ...string) *exec.Cmd {
			switch name {
			case "claude-mcp-list":
				listCalls++
				return exec.Command("false") // simulate non-zero exit
			case "claude":
				installCalls++
				return exec.Command("true")
			}
			return exec.Command("false")
		},
	}

	result, err := MCPInstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("MCPInstall --run --yes: %v", err)
	}

	if listCalls != 1 {
		t.Errorf("list probe ran %d times, want 1", listCalls)
	}
	if installCalls != 1 {
		t.Errorf("install ran %d times, want 1 (probe failure must fall through)", installCalls)
	}
	if len(result.AlreadyRegistered) != 0 {
		t.Errorf("AlreadyRegistered = %v, want empty", result.AlreadyRegistered)
	}
}

// ---------------------------------------------------------------------------
// --run: path-divergence detection (#61)
// ---------------------------------------------------------------------------

// TestMCPInstall_Run_PathIdentical verifies that when an existing guild
// entry's configured command path matches the running binary, the
// client is recorded in AlreadyRegistered and the install command is
// not re-invoked. This is the same-path case from issue #61.
func TestMCPInstall_Run_PathIdentical(t *testing.T) {
	var installCalls int

	c := alwaysDetected("Claude Code", claudeArgv)
	c.ListArgv = func() []string { return []string{"claude-mcp-list"} }

	dir := t.TempDir()
	fakeBin := dir + "/guild"
	if err := os.WriteFile(fakeBin, []byte{}, 0o600); err != nil {
		t.Fatalf("create temp binary: %v", err)
	}

	var buf bytes.Buffer
	opts := MCPInstallOptions{
		Run:          true,
		Yes:          true,
		Out:          &buf,
		In:           &bytes.Buffer{},
		clients:      []Client{c},
		executableFn: func() (string, error) { return fakeBin, nil },
		execCmdFn: func(name string, arg ...string) *exec.Cmd {
			switch name {
			case "claude-mcp-list":
				//nolint:gosec // test-only: fakeBin is generated by t.TempDir.
				return exec.Command("printf", "guild: "+fakeBin+" mcp serve\n")
			case "claude":
				installCalls++
				return exec.Command("true")
			}
			return exec.Command("false")
		},
		lookPathFn: func(name string) (string, error) { return name, nil },
	}

	result, err := MCPInstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("MCPInstall --run --yes: %v", err)
	}
	if installCalls != 0 {
		t.Errorf("install ran %d times, want 0 (paths identical)", installCalls)
	}
	if got := len(result.AlreadyRegistered); got != 1 {
		t.Errorf("AlreadyRegistered len = %d, want 1", got)
	}
	if got := len(result.PathDivergent); got != 0 {
		t.Errorf("PathDivergent len = %d, want 0", got)
	}
}

// TestMCPInstall_Run_PathDivergent_NoUpdate verifies that when the
// configured path differs from the running binary and neither --update
// nor --force is set, the entry is reported in PathDivergent, the
// install is NOT re-invoked, and the user-facing message names both
// paths. This is the "stale entry, no flag" case from issue #61.
func TestMCPInstall_Run_PathDivergent_NoUpdate(t *testing.T) {
	var installCalls int

	c := alwaysDetected("Claude Code", claudeArgv)
	c.ListArgv = func() []string { return []string{"claude-mcp-list"} }

	dir := t.TempDir()
	fakeBin := dir + "/guild"
	if err := os.WriteFile(fakeBin, []byte{}, 0o600); err != nil {
		t.Fatalf("create temp binary: %v", err)
	}
	stalePath := "/opt/old/bin/guild"

	var buf bytes.Buffer
	opts := MCPInstallOptions{
		Run:          true,
		Yes:          true,
		Out:          &buf,
		In:           &bytes.Buffer{},
		clients:      []Client{c},
		executableFn: func() (string, error) { return fakeBin, nil },
		execCmdFn: func(name string, arg ...string) *exec.Cmd {
			switch name {
			case "claude-mcp-list":
				return exec.Command("printf", "guild: "+stalePath+" mcp serve\n")
			case "claude":
				installCalls++
				return exec.Command("true")
			}
			return exec.Command("false")
		},
		lookPathFn: func(name string) (string, error) { return name, nil },
	}

	result, err := MCPInstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("MCPInstall --run --yes: %v", err)
	}
	if installCalls != 0 {
		t.Errorf("install ran %d times, want 0 (no --update flag)", installCalls)
	}
	if got := len(result.PathDivergent); got != 1 {
		t.Errorf("PathDivergent len = %d, want 1", got)
	}
	if got := len(result.AlreadyRegistered); got != 0 {
		t.Errorf("AlreadyRegistered len = %d, want 0", got)
	}
	out := buf.String()
	if !strings.Contains(out, stalePath) {
		t.Errorf("output missing stale path %q; got:\n%s", stalePath, out)
	}
	if !strings.Contains(out, "--update") {
		t.Errorf("output should hint at --update; got:\n%s", out)
	}
}

// TestMCPInstall_Run_PathDivergent_WithUpdate verifies that when the
// configured path differs and --update is set, the install command IS
// re-invoked to refresh the entry, and the client is recorded in
// PathDivergent so callers can surface that the entry was rewritten.
func TestMCPInstall_Run_PathDivergent_WithUpdate(t *testing.T) {
	var installCalls int

	c := alwaysDetected("Claude Code", claudeArgv)
	c.ListArgv = func() []string { return []string{"claude-mcp-list"} }

	dir := t.TempDir()
	fakeBin := dir + "/guild"
	if err := os.WriteFile(fakeBin, []byte{}, 0o600); err != nil {
		t.Fatalf("create temp binary: %v", err)
	}
	stalePath := "/opt/old/bin/guild"

	var buf bytes.Buffer
	opts := MCPInstallOptions{
		Run:          true,
		Yes:          true,
		Update:       true,
		Out:          &buf,
		In:           &bytes.Buffer{},
		clients:      []Client{c},
		executableFn: func() (string, error) { return fakeBin, nil },
		execCmdFn: func(name string, arg ...string) *exec.Cmd {
			switch name {
			case "claude-mcp-list":
				return exec.Command("printf", "guild: "+stalePath+" mcp serve\n")
			case "claude":
				installCalls++
				return exec.Command("true")
			}
			return exec.Command("false")
		},
		lookPathFn: func(name string) (string, error) { return name, nil },
	}

	result, err := MCPInstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("MCPInstall --run --yes --update: %v", err)
	}
	if installCalls != 1 {
		t.Errorf("install ran %d times, want 1 (--update should refresh)", installCalls)
	}
	if got := len(result.PathDivergent); got != 1 {
		t.Errorf("PathDivergent len = %d, want 1", got)
	}
	if got := len(result.Ran); got != 1 {
		t.Errorf("Ran len = %d, want 1", got)
	}
	out := buf.String()
	if !strings.Contains(out, "refreshing") {
		t.Errorf("output should announce refresh; got:\n%s", out)
	}
}

// TestMCPInstall_Run_Force_BypassesProbe verifies that --force skips the
// list probe entirely and always re-invokes install — useful when the
// configured command is unparseable or the user wants to overwrite
// regardless of inspection. Issue #61.
func TestMCPInstall_Run_Force_BypassesProbe(t *testing.T) {
	var installCalls, listCalls int

	c := alwaysDetected("Claude Code", claudeArgv)
	c.ListArgv = func() []string { return []string{"claude-mcp-list"} }

	dir := t.TempDir()
	fakeBin := dir + "/guild"
	if err := os.WriteFile(fakeBin, []byte{}, 0o600); err != nil {
		t.Fatalf("create temp binary: %v", err)
	}

	var buf bytes.Buffer
	opts := MCPInstallOptions{
		Run:          true,
		Yes:          true,
		Force:        true,
		Out:          &buf,
		In:           &bytes.Buffer{},
		clients:      []Client{c},
		executableFn: func() (string, error) { return fakeBin, nil },
		execCmdFn: func(name string, arg ...string) *exec.Cmd {
			switch name {
			case "claude-mcp-list":
				listCalls++
				//nolint:gosec // test-only: fakeBin is generated by t.TempDir.
				return exec.Command("printf", "guild: "+fakeBin+" mcp serve\n")
			case "claude":
				installCalls++
				return exec.Command("true")
			}
			return exec.Command("false")
		},
		lookPathFn: func(name string) (string, error) { return name, nil },
	}

	result, err := MCPInstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("MCPInstall --run --yes --force: %v", err)
	}
	if listCalls != 0 {
		t.Errorf("list probe ran %d times, want 0 (--force skips probe)", listCalls)
	}
	if installCalls != 1 {
		t.Errorf("install ran %d times, want 1 (--force always installs)", installCalls)
	}
	if len(result.Ran) != 1 {
		t.Errorf("Ran = %v, want one entry", result.Ran)
	}
}
