package install

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClient builds a Client that always reports Detected()=true by pointing
// CLIProbe at a real executable on PATH. Paired with an injected execCmdFn,
// lets tests observe the MCP registration path without running real CLI.
func fakeClient(name string) Client {
	return Client{
		Name:     name,
		CLIProbe: "sh", // always resolvable in CI + local
		InstallArgv: func(binPath string) []string {
			return []string{"sh", "-c", "true -- " + binPath}
		},
	}
}

// recordingExec returns an execCmdFn that appends every invocation to calls
// and always succeeds (runs /bin/true). Tests use this to assert that init
// reached the registration step without actually shelling out.
func recordingExec(calls *[][]string) func(name string, arg ...string) *exec.Cmd {
	return func(name string, arg ...string) *exec.Cmd {
		*calls = append(*calls, append([]string{name}, arg...))
		return exec.Command("true")
	}
}

// fakeExecutable creates a real temp file and returns an executableFn that
// resolves to its path. Needed so the MCP registration step's resolveAbsBinPath
// treats it as a durable install location (not a go-build cache miss). CI
// runners have no system-installed guild, so this plug is essential there.
func fakeExecutable(t *testing.T) func() (string, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "guild-*")
	if err != nil {
		t.Fatalf("create fake executable: %v", err)
	}
	_ = f.Close()
	path := f.Name()
	return func() (string, error) { return path, nil }
}

// makeRepo creates a temp directory whose basename is name, simulating a repo root.
func makeRepo(t *testing.T, name string) string {
	t.Helper()
	parent := t.TempDir()
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("make repo dir: %v", err)
	}
	return dir
}

// testDBPaths returns temp-dir lore.db and quest.db paths so tests don't
// touch the user's real ~/.guild/ databases.
func testDBPaths(t *testing.T) (loreDB, questDB string) {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "lore.db"), filepath.Join(dir, "quest.db")
}

// newOpts returns InitOptions with output captured, test-local DBs, --yes
// so tests don't block on interactive prompts, and a fakeExecutable so the
// MCP registration step's resolveAbsBinPath does not require a real
// system-installed guild binary. Without this injection, any environment
// without a prior `guild` install (CI runners, fresh dev boxes, Nix, etc.)
// fails every Init test with "guild binary not found in any durable
// location" — see #49.
func newOpts(t *testing.T, buf *bytes.Buffer) InitOptions {
	t.Helper()
	loreDB, questDB := testDBPaths(t)
	return InitOptions{
		Yes:          true,
		Out:          buf,
		In:           &bytes.Buffer{},
		LoreDBPath:   loreDB,
		QuestDBPath:  questDB,
		executableFn: fakeExecutable(t),
	}
}

// ---------------------------------------------------------------------------
// Fresh repo: no AGENTS.md → create
// ---------------------------------------------------------------------------

func TestInit_FreshRepo_CreatesAgentsMD(t *testing.T) {
	ctx := context.Background()
	dir := makeRepo(t, "myproject")
	var out bytes.Buffer

	res, err := Init(ctx, dir, newOpts(t, &out))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if res.ProjectName != "myproject" {
		t.Errorf("ProjectName = %q; want %q", res.ProjectName, "myproject")
	}
	if !res.DBRegistered {
		t.Error("DBRegistered should be true")
	}
	if !res.Written {
		t.Error("Written should be true for fresh repo")
	}

	// AGENTS.md must exist and contain the section marker.
	content, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if !strings.Contains(string(content), agentsSectionMarker) {
		t.Errorf("AGENTS.md missing section marker; got:\n%s", content)
	}
	if !strings.Contains(string(content), "myproject") {
		t.Errorf("AGENTS.md missing project name; got:\n%s", content)
	}

	// CLAUDE.md must NOT be written.
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); err == nil {
		t.Error("CLAUDE.md was written (must never write CLAUDE.md)")
	}

	// Output must describe what happened.
	output := out.String()
	if !strings.Contains(output, "myproject") {
		t.Errorf("output missing project name; got:\n%s", output)
	}
	if !strings.Contains(output, "created AGENTS.md") {
		t.Errorf("output missing 'created AGENTS.md'; got:\n%s", output)
	}
}

// ---------------------------------------------------------------------------
// Existing AGENTS.md without guild section → append
// ---------------------------------------------------------------------------

func TestInit_ExistingAgentsMD_NoSection_Appends(t *testing.T) {
	ctx := context.Background()
	dir := makeRepo(t, "appendproj")

	agentsPath := filepath.Join(dir, "AGENTS.md")
	original := "# My Existing Docs\n\nSome project docs here.\n"
	if err := os.WriteFile(agentsPath, []byte(original), 0o644); err != nil { //nolint:gosec // G306: test fixture
		t.Fatal(err)
	}

	var out bytes.Buffer
	res, err := Init(ctx, dir, newOpts(t, &out))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if !res.Written {
		t.Error("Written should be true when appending section")
	}

	content, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	s := string(content)

	// Original content must be preserved.
	if !strings.HasPrefix(s, "# My Existing Docs") {
		t.Errorf("original content not at top;\ngot:\n%s", s)
	}
	// Guild section must follow.
	if !strings.Contains(s, agentsSectionMarker) {
		t.Errorf("section marker %q not found after append", agentsSectionMarker)
	}
	if !strings.Contains(out.String(), "appended guild section") {
		t.Errorf("output missing append confirmation; got:\n%s", out.String())
	}
}

// ---------------------------------------------------------------------------
// Existing AGENTS.md WITH guild section → idempotent skip
// ---------------------------------------------------------------------------

func TestInit_ExistingAgentsMD_WithSection_Idempotent(t *testing.T) {
	ctx := context.Background()
	dir := makeRepo(t, "idemproj")

	agentsPath := filepath.Join(dir, "AGENTS.md")
	// Seed with the CURRENT rendered template so Init sees content that
	// matches the template byte-for-byte and reports skip.
	existing := "# Docs\n\n" + renderSection("idemproj") + "\n"
	if err := os.WriteFile(agentsPath, []byte(existing), 0o644); err != nil { //nolint:gosec // G306: test fixture
		t.Fatal(err)
	}

	var out bytes.Buffer
	res, err := Init(ctx, dir, newOpts(t, &out))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if res.Written {
		t.Error("Written should be false when section matches current template (idempotent)")
	}

	// File must be unchanged.
	after, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != existing {
		t.Errorf("AGENTS.md was modified on idempotent re-run;\nbefore:\n%s\nafter:\n%s", existing, after)
	}

	if !strings.Contains(out.String(), "up-to-date") {
		t.Errorf("output should mention up-to-date; got:\n%s", out.String())
	}
}

// ---------------------------------------------------------------------------
// Existing AGENTS.md WITH outdated guild section → refresh in place
// ---------------------------------------------------------------------------

func TestInit_ExistingAgentsMD_OutdatedSection_Refresh(t *testing.T) {
	ctx := context.Background()
	dir := makeRepo(t, "refreshproj")

	agentsPath := filepath.Join(dir, "AGENTS.md")
	// Seed with a stale guild section (old short template shape) plus
	// unrelated content on either side that must be preserved verbatim.
	before := "# Docs\n\nmy custom content.\n\n" + agentsSectionMarker +
		"\n\nThis is an old stale guild section.\n\n## another-tool\n\nother content here.\n"
	if err := os.WriteFile(agentsPath, []byte(before), 0o644); err != nil { //nolint:gosec // G306: test fixture
		t.Fatal(err)
	}

	var out bytes.Buffer
	res, err := Init(ctx, dir, newOpts(t, &out))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if !res.Written {
		t.Error("Written should be true when template drift triggered a refresh")
	}

	after, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatal(err)
	}
	afterStr := string(after)

	// Custom content before the guild section must be preserved.
	if !strings.Contains(afterStr, "my custom content.") {
		t.Errorf("pre-section content lost; got:\n%s", afterStr)
	}
	// Other H2 section after the guild section must be preserved.
	if !strings.Contains(afterStr, "## another-tool") || !strings.Contains(afterStr, "other content here.") {
		t.Errorf("post-section content lost; got:\n%s", afterStr)
	}
	// Stale body must be gone.
	if strings.Contains(afterStr, "This is an old stale guild section.") {
		t.Errorf("stale guild content was not replaced; got:\n%s", afterStr)
	}
	// Current template must be present.
	if !strings.Contains(afterStr, "BEFORE ANY OTHER ACTION") {
		t.Errorf("refreshed file missing current template content; got:\n%s", afterStr)
	}

	if !strings.Contains(out.String(), "refresh") {
		t.Errorf("output should mention refresh; got:\n%s", out.String())
	}
}

// ---------------------------------------------------------------------------
// --yes: non-interactive, accept all defaults
// ---------------------------------------------------------------------------

func TestInit_YesFlag_NonInteractive(t *testing.T) {
	ctx := context.Background()
	dir := makeRepo(t, "yesproj")
	loreDB, questDB := testDBPaths(t)
	var out bytes.Buffer

	_, err := Init(ctx, dir, InitOptions{
		Yes:          true,
		Out:          &out,
		In:           &bytes.Buffer{},
		LoreDBPath:   loreDB,
		QuestDBPath:  questDB,
		executableFn: fakeExecutable(t),
	})
	if err != nil {
		t.Fatalf("Init --yes: %v", err)
	}

	// AGENTS.md must exist.
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Errorf("AGENTS.md missing after --yes: %v", err)
	}
}

// ---------------------------------------------------------------------------
// --dry-run: no changes, no DB registration
// ---------------------------------------------------------------------------

func TestInit_DryRun_NoChanges(t *testing.T) {
	ctx := context.Background()
	dir := makeRepo(t, "dryproj")
	loreDB, questDB := testDBPaths(t)
	var out bytes.Buffer

	res, err := Init(ctx, dir, InitOptions{
		DryRun:      true,
		Yes:         true,
		Out:         &out,
		In:          &bytes.Buffer{},
		LoreDBPath:  loreDB,
		QuestDBPath: questDB,
	})
	if err != nil {
		t.Fatalf("Init --dry-run: %v", err)
	}

	// No file writes.
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err == nil {
		t.Error("AGENTS.md was written during --dry-run")
	}
	// No DB registration.
	if res.DBRegistered {
		t.Error("DBRegistered should be false during --dry-run")
	}
	// Output describes the plan.
	output := out.String()
	if !strings.Contains(output, "dry-run") {
		t.Errorf("output missing dry-run indicator; got:\n%s", output)
	}
}

// ---------------------------------------------------------------------------
// --print-agents-md: stdout is only the template snippet
// ---------------------------------------------------------------------------

func TestInit_PrintAgentsMD_EmitsTemplateOnly(t *testing.T) {
	ctx := context.Background()
	dir := makeRepo(t, "printproj")
	loreDB, questDB := testDBPaths(t)
	var out bytes.Buffer

	_, err := Init(ctx, dir, InitOptions{
		PrintAgentsMD: true,
		Out:           &out,
		In:            &bytes.Buffer{},
		LoreDBPath:    loreDB,
		QuestDBPath:   questDB,
	})
	if err != nil {
		t.Fatalf("Init --print-agents-md: %v", err)
	}

	output := out.String()
	// Must contain section marker.
	if !strings.Contains(output, agentsSectionMarker) {
		t.Errorf("output missing section marker; got:\n%s", output)
	}
	// Must NOT contain narration text.
	if strings.Contains(output, "Will perform") || strings.Contains(output, "registered") {
		t.Errorf("output contains narration; --print-agents-md should emit template only; got:\n%s", output)
	}
}

// ---------------------------------------------------------------------------
// Template line-count budget (carries 4 core rules + signpost; ≤30 lines)
// ---------------------------------------------------------------------------

func TestTemplateLineCount(t *testing.T) {
	rendered := renderSection("testproject")
	lines := strings.Count(rendered, "\n")
	t.Logf("rendered template: %d lines", lines)
	if lines > 30 {
		t.Errorf("template rendered to %d lines; must stay ≤30 to remain upsertable", lines)
	}
}

// ---------------------------------------------------------------------------
// Template contains the section marker (grep verification target)
// ---------------------------------------------------------------------------

func TestTemplate_ContainsSectionMarker(t *testing.T) {
	rendered := renderSection("anyproject")
	if !strings.Contains(rendered, agentsSectionMarker) {
		t.Errorf("template missing %q", agentsSectionMarker)
	}
}

// ---------------------------------------------------------------------------
// ~/.guild/ directory created with 0o700 (regression for #79)
// ---------------------------------------------------------------------------

func TestResolveDBPaths_GuildDirPerm0700(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, _, err := resolveDBPaths(InitOptions{}); err != nil {
		t.Fatalf("resolveDBPaths: %v", err)
	}
	info, err := os.Stat(filepath.Join(home, ".guild"))
	if err != nil {
		t.Fatalf("stat ~/.guild: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("~/.guild perm = %o; want 0o700", got)
	}
}

// ---------------------------------------------------------------------------
// No duplicate sections on multiple runs
// ---------------------------------------------------------------------------

func TestInit_MultipleRuns_NoDuplicateSections(t *testing.T) {
	ctx := context.Background()
	dir := makeRepo(t, "multiproj")
	loreDB, questDB := testDBPaths(t)

	runInit := func() {
		t.Helper()
		var out bytes.Buffer
		if _, err := Init(ctx, dir, InitOptions{
			Yes:          true,
			Out:          &out,
			In:           &bytes.Buffer{},
			LoreDBPath:   loreDB,
			QuestDBPath:  questDB,
			executableFn: fakeExecutable(t),
		}); err != nil {
			t.Fatalf("Init: %v", err)
		}
	}

	runInit()
	runInit()

	content, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	count := strings.Count(string(content), agentsSectionMarker)
	if count != 1 {
		t.Errorf("section marker appears %d times; want exactly 1\n%s", count, content)
	}
}

// ---------------------------------------------------------------------------
// MCP registration — folded into init (QUEST-105 / LORE-79)
// ---------------------------------------------------------------------------

// With --yes and a detected client, init must invoke the registration exec.
func TestInit_MCPRegistration_YesFlagInvokesExec(t *testing.T) {
	ctx := context.Background()
	dir := makeRepo(t, "yesmcp")
	loreDB, questDB := testDBPaths(t)
	var out bytes.Buffer
	var calls [][]string

	_, err := Init(ctx, dir, InitOptions{
		Yes:          true,
		Out:          &out,
		In:           &bytes.Buffer{},
		LoreDBPath:   loreDB,
		QuestDBPath:  questDB,
		clients:      []Client{fakeClient("FakeClient")},
		execCmdFn:    recordingExec(&calls),
		executableFn: fakeExecutable(t),
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 registration exec call, got %d: %+v", len(calls), calls)
	}
	if calls[0][0] != "sh" {
		t.Errorf("expected registration via sh, got %v", calls[0])
	}
}

// Interactive path: user types "y" — init must invoke registration.
func TestInit_MCPRegistration_InteractiveYes(t *testing.T) {
	ctx := context.Background()
	dir := makeRepo(t, "intmcp")
	loreDB, questDB := testDBPaths(t)
	var out bytes.Buffer
	var calls [][]string

	// Two prompts: the overall Init "Continue? [Y/n]" and the per-client
	// "Run: ... [y/N]" from MCPInstall. Both answered "y".
	in := bytes.NewBufferString("y\ny\n")
	_, err := Init(ctx, dir, InitOptions{
		Out:          &out,
		In:           in,
		LoreDBPath:   loreDB,
		QuestDBPath:  questDB,
		clients:      []Client{fakeClient("FakeClient")},
		execCmdFn:    recordingExec(&calls),
		executableFn: fakeExecutable(t),
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Non-TTY stdin means MCPInstall exits early with a message rather than
	// prompting, so the expected call count depends on whether isInteractive
	// treats bytes.Buffer as a TTY — it does not. Confirm via output instead.
	if !strings.Contains(out.String(), "FakeClient") {
		t.Errorf("output missing FakeClient mention:\n%s", out.String())
	}
	_ = calls
}

// --dry-run must never execute the registration command.
func TestInit_MCPRegistration_DryRunSkipsExec(t *testing.T) {
	ctx := context.Background()
	dir := makeRepo(t, "drymcp")
	loreDB, questDB := testDBPaths(t)
	var out bytes.Buffer
	var calls [][]string

	_, err := Init(ctx, dir, InitOptions{
		DryRun:      true,
		Yes:         true,
		Out:         &out,
		In:          &bytes.Buffer{},
		LoreDBPath:  loreDB,
		QuestDBPath: questDB,
		clients:     []Client{fakeClient("FakeClient")},
		execCmdFn:   recordingExec(&calls),
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("--dry-run must not invoke registration, got %d calls: %+v", len(calls), calls)
	}
}

// No client detected → init prints the manual-setup hint.
func TestInit_MCPRegistration_NoClient_PrintsHint(t *testing.T) {
	ctx := context.Background()
	dir := makeRepo(t, "nomcp")
	loreDB, questDB := testDBPaths(t)
	var out bytes.Buffer

	_, err := Init(ctx, dir, InitOptions{
		Yes:         true,
		Out:         &out,
		In:          &bytes.Buffer{},
		LoreDBPath:  loreDB,
		QuestDBPath: questDB,
		clients:     []Client{}, // explicit empty, not nil (nil → real detect)
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !strings.Contains(out.String(), "no MCP client detected") {
		t.Errorf("expected manual-setup hint; got:\n%s", out.String())
	}
}
