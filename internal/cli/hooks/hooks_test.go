package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hookcfg "github.com/mathomhaus/guild/internal/hooks"
	"github.com/mathomhaus/guild/internal/hooks/adapters"
	"github.com/mathomhaus/guild/internal/hooks/adapters/stub"
	"github.com/mathomhaus/guild/internal/install"
)

// testDeps wires the stub adapter (file-backed, identity Substitute)
// and a synthetic client registry into the command runners. HOME must
// already point at a t.TempDir() so every path is hermetic.
func testDeps(out *bytes.Buffer) deps {
	return deps{
		adapters: []adapters.Adapter{stub.Adapter{}},
		clients: []install.Client{
			// "go" is always on PATH in the test environment; the
			// normalized name "stub" maps onto the stub adapter.
			{Name: "Stub", CLIProbe: "go"},
			{Name: "Ghost Harness", CLIProbe: "nonexistent-cli-binary-xyzzy"},
		},
		out: out,
	}
}

func settingsPath(t *testing.T, home string) string {
	t.Helper()
	return filepath.Join(home, ".guild", "stub-settings.json")
}

func basePath(t *testing.T, home string) string {
	t.Helper()
	return filepath.Join(home, ".guild", "hooks-base.json")
}

// TestInstall_Clean: first run creates the base config and the harness
// settings file, and reports the detected harnesses.
func TestInstall_Clean(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer

	if err := runInstall(testDeps(&out), "", false); err != nil {
		t.Fatalf("runInstall: %v", err)
	}

	if _, err := os.Stat(basePath(t, home)); err != nil {
		t.Errorf("hooks-base.json not created: %v", err)
	}
	raw, err := os.ReadFile(settingsPath(t, home))
	if err != nil {
		t.Fatalf("settings file not written: %v", err)
	}
	for _, want := range []string{
		"guild quest brief --auto",
		"guild quest brief --auto --capture",
		"guild lore appraise --inject --from-stdin-json",
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("settings file missing command %q", want)
		}
	}

	got := out.String()
	if !strings.Contains(got, "✓ Stub") {
		t.Errorf("output missing detected harness line:\n%s", got)
	}
	if !strings.Contains(got, "✗ Ghost Harness") {
		t.Errorf("output missing undetected harness line:\n%s", got)
	}
	if !strings.Contains(got, "stub: installed hooks ->") {
		t.Errorf("output missing install confirmation:\n%s", got)
	}
}

// TestInstall_AlreadyInstalledNoOp: a second install changes nothing on
// disk and says so.
func TestInstall_AlreadyInstalledNoOp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer

	if err := runInstall(testDeps(&out), "", false); err != nil {
		t.Fatalf("first runInstall: %v", err)
	}
	first, err := os.ReadFile(settingsPath(t, home))
	if err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := runInstall(testDeps(&out), "", false); err != nil {
		t.Fatalf("second runInstall: %v", err)
	}
	second, err := os.ReadFile(settingsPath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("second install rewrote the settings file")
	}
	if !strings.Contains(out.String(), "already installed") {
		t.Errorf("output missing no-op confirmation:\n%s", out.String())
	}
}

// TestInstall_DryRunWritesNothing: --dry-run leaves the filesystem
// untouched, including the base config.
func TestInstall_DryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer

	if err := runInstall(testDeps(&out), "", true); err != nil {
		t.Fatalf("runInstall --dry-run: %v", err)
	}

	if _, err := os.Stat(basePath(t, home)); !os.IsNotExist(err) {
		t.Errorf("dry-run created hooks-base.json (stat err: %v)", err)
	}
	if _, err := os.Stat(settingsPath(t, home)); !os.IsNotExist(err) {
		t.Errorf("dry-run created the settings file (stat err: %v)", err)
	}
	if !strings.Contains(out.String(), "would write") {
		t.Errorf("dry-run output missing preview line:\n%s", out.String())
	}
}

// TestInstall_UnknownHarnessErrors: the --harness filter rejects names
// that no adapter registered.
func TestInstall_UnknownHarnessErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out bytes.Buffer

	err := runInstall(testDeps(&out), "no-such-harness", false)
	if err == nil || !strings.Contains(err.Error(), "unknown harness") {
		t.Errorf("err = %v; want unknown harness error", err)
	}
}

// TestInstall_UnknownHarnessErrorsWithoutAdapters: the --harness value
// is validated before the zero-adapters early return, so a misspelled
// name errors in every build instead of silently exiting 0 in an
// adapter-less one.
func TestInstall_UnknownHarnessErrorsWithoutAdapters(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out bytes.Buffer

	d := testDeps(&out)
	d.adapters = nil
	err := runInstall(d, "no-such-harness", false)
	if err == nil || !strings.Contains(err.Error(), "unknown harness") {
		t.Errorf("err = %v; want unknown harness error", err)
	}
}

// TestSync_DriftRepairAndIdempotent: editing the base config makes the
// target drift; sync repairs it; a second sync is a no-op; dry-run
// never writes.
func TestSync_DriftRepairAndIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer

	if err := runInstall(testDeps(&out), "", false); err != nil {
		t.Fatalf("runInstall: %v", err)
	}

	// User edits the base: still guild-owned, different flagging.
	custom := hookcfg.DefaultBase()
	custom["SessionStart"][0].Hooks[0].Command = "guild quest brief --auto --depth 2"
	if err := hookcfg.SaveBase(custom); err != nil {
		t.Fatalf("SaveBase: %v", err)
	}

	// Dry-run reports the pending change without writing.
	before, err := os.ReadFile(settingsPath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runSync(testDeps(&out), true); err != nil {
		t.Fatalf("runSync --dry-run: %v", err)
	}
	after, err := os.ReadFile(settingsPath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("sync --dry-run modified the settings file")
	}
	if !strings.Contains(out.String(), "would update") {
		t.Errorf("dry-run output missing preview:\n%s", out.String())
	}

	// Real sync repairs the drift.
	out.Reset()
	if err := runSync(testDeps(&out), false); err != nil {
		t.Fatalf("runSync: %v", err)
	}
	raw, err := os.ReadFile(settingsPath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "guild quest brief --auto --depth 2") {
		t.Error("sync did not propagate the edited base command")
	}

	// Second sync: no-op.
	out.Reset()
	if err := runSync(testDeps(&out), false); err != nil {
		t.Fatalf("runSync (second): %v", err)
	}
	if !strings.Contains(out.String(), "in sync (no-op)") {
		t.Errorf("second sync output missing no-op line:\n%s", out.String())
	}
}

// TestSync_ThreeRunsByteIdentical: regression test for the duplication
// bug where repeated syncs appended the desired groups over and over.
// With a valid (guild-only) base, three consecutive syncs leave the
// settings file byte-identical and the third run reports in-sync.
func TestSync_ThreeRunsByteIdentical(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer

	if err := runInstall(testDeps(&out), "", false); err != nil {
		t.Fatalf("runInstall: %v", err)
	}

	// Edit the base to a different (still guild-owned) command so the
	// first sync has real work to do.
	custom := hookcfg.DefaultBase()
	custom["SessionStart"][0].Hooks[0].Command = "guild quest brief --auto --depth 2"
	if err := hookcfg.SaveBase(custom); err != nil {
		t.Fatalf("SaveBase: %v", err)
	}

	var snapshots [3][]byte
	for i := range snapshots {
		out.Reset()
		if err := runSync(testDeps(&out), false); err != nil {
			t.Fatalf("runSync #%d: %v", i+1, err)
		}
		raw, err := os.ReadFile(settingsPath(t, home))
		if err != nil {
			t.Fatal(err)
		}
		snapshots[i] = raw
	}

	if !bytes.Equal(snapshots[0], snapshots[1]) || !bytes.Equal(snapshots[1], snapshots[2]) {
		t.Error("repeated syncs changed the settings file; sync is not idempotent")
	}
	if !strings.Contains(out.String(), "in sync (no-op)") {
		t.Errorf("third sync output missing in-sync no-op line:\n%s", out.String())
	}
}

// TestSync_ForeignBaseFailsLoud: end-to-end check of the reviewer
// repro. A hand-edited base config containing a custom (non-guild)
// command must make sync fail with the actionable validation error and
// write nothing, instead of duplicating the command into the settings
// file on every run.
func TestSync_ForeignBaseFailsLoud(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer

	if err := runInstall(testDeps(&out), "", false); err != nil {
		t.Fatalf("runInstall: %v", err)
	}
	before, err := os.ReadFile(settingsPath(t, home))
	if err != nil {
		t.Fatal(err)
	}

	// Written by hand: SaveBase would already reject this.
	raw := []byte(`{
  "SessionStart": [
    {"matcher": "startup", "hooks": [{"type": "command", "command": "echo my-custom-thing"}]}
  ]
}`)
	if err := os.WriteFile(basePath(t, home), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	err = runSync(testDeps(&out), false)
	if err == nil || !strings.Contains(err.Error(), "not a guild command") {
		t.Fatalf("runSync err = %v; want non-guild command validation error", err)
	}

	after, err := os.ReadFile(settingsPath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("sync modified the settings file despite the invalid base config")
	}
}

// TestScan_DetectsExistingHooks: scan surfaces hooks already present in
// a settings file, foreign ones included, without writing anything.
func TestScan_DetectsExistingHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pre := []byte(`{
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [{"type": "command", "command": "other-tool prime"}]},
      {"matcher": "resume", "hooks": [{"type": "command", "command": "guild quest brief --auto"}]}
    ]
  }
}`)
	if err := os.WriteFile(settingsPath(t, home), pre, 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runScan(testDeps(&out), false, false); err != nil {
		t.Fatalf("runScan: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "[foreign]") || !strings.Contains(got, "other-tool prime") {
		t.Errorf("scan missing foreign hook:\n%s", got)
	}
	if !strings.Contains(got, "[guild]") || !strings.Contains(got, "guild quest brief --auto") {
		t.Errorf("scan missing guild hook:\n%s", got)
	}

	// No writes: file is byte-identical.
	raw, err := os.ReadFile(settingsPath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, pre) {
		t.Error("scan modified the settings file")
	}
}

// TestScan_JSON: machine-readable output round-trips.
func TestScan_JSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer

	if err := runInstall(testDeps(&out), "", false); err != nil {
		t.Fatalf("runInstall: %v", err)
	}
	out.Reset()
	if err := runScan(testDeps(&out), false, true); err != nil {
		t.Fatalf("runScan --json: %v", err)
	}
	var reports []scanReport
	if err := json.Unmarshal(out.Bytes(), &reports); err != nil {
		t.Fatalf("scan --json output not valid JSON: %v\n%s", err, out.String())
	}
	if len(reports) != 1 || reports[0].Harness != "stub" || len(reports[0].Hooks) != 3 {
		t.Errorf("unexpected scan report: %+v", reports)
	}
}

// TestList_StatusLifecycle: missing before install, in-sync after,
// drift after a base edit.
func TestList_StatusLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer

	listStatus := func() string {
		t.Helper()
		out.Reset()
		if err := runList(testDeps(&out), true); err != nil {
			t.Fatalf("runList: %v", err)
		}
		var rows []listRow
		if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
			t.Fatalf("list --json output not valid JSON: %v\n%s", err, out.String())
		}
		if len(rows) != 1 {
			t.Fatalf("list returned %d rows; want 1", len(rows))
		}
		if rows[0].Harness != "stub" || !rows[0].Detected {
			t.Fatalf("unexpected row: %+v", rows[0])
		}
		return rows[0].Status
	}

	if st := listStatus(); st != "missing" {
		t.Errorf("status before install = %q; want missing", st)
	}

	if err := runInstall(testDeps(&out), "", false); err != nil {
		t.Fatalf("runInstall: %v", err)
	}
	if st := listStatus(); st != "in-sync" {
		t.Errorf("status after install = %q; want in-sync", st)
	}

	custom := hookcfg.DefaultBase()
	custom["UserPromptSubmit"][0].Hooks[0].Command = "guild lore appraise --inject --from-stdin-json --top-k 3"
	if err := hookcfg.SaveBase(custom); err != nil {
		t.Fatalf("SaveBase: %v", err)
	}
	if st := listStatus(); st != "drift" {
		t.Errorf("status after base edit = %q; want drift", st)
	}
}

// TestDiff_ShowsPendingChangeWithoutWriting: diff prints +/- lines for
// a drifted target and leaves the file untouched.
func TestDiff_ShowsPendingChangeWithoutWriting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer

	if err := runInstall(testDeps(&out), "", false); err != nil {
		t.Fatalf("runInstall: %v", err)
	}

	// In-sync: no +/- lines.
	out.Reset()
	if err := runDiff(testDeps(&out), true); err != nil {
		t.Fatalf("runDiff: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "in-sync") || strings.Contains(got, "\n  + ") {
		t.Errorf("in-sync diff unexpected output:\n%s", got)
	}

	custom := hookcfg.DefaultBase()
	custom["SessionStart"][0].Matcher = "startup|resume"
	if err := hookcfg.SaveBase(custom); err != nil {
		t.Fatalf("SaveBase: %v", err)
	}

	before, err := os.ReadFile(settingsPath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runDiff(testDeps(&out), true); err != nil {
		t.Fatalf("runDiff (drift): %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "- SessionStart [startup|resume|clear|compact]") {
		t.Errorf("diff missing removal line:\n%s", got)
	}
	if !strings.Contains(got, "+ SessionStart [startup|resume]") {
		t.Errorf("diff missing addition line:\n%s", got)
	}
	if strings.Contains(got, ansiRed) {
		t.Errorf("--no-color output contains ANSI codes:\n%s", got)
	}
	after, err := os.ReadFile(settingsPath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("diff modified the settings file")
	}
}

// TestOwnershipDetection_MixedGroupSurvivesSync: end-to-end check that
// a mixed group (guild + foreign command) is treated as foreign and
// survives install/sync untouched.
func TestOwnershipDetection_MixedGroupSurvivesSync(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pre := []byte(`{
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [
        {"type": "command", "command": "guild quest brief --auto"},
        {"type": "command", "command": "other-tool prime"}
      ]}
    ]
  }
}`)
	if err := os.WriteFile(settingsPath(t, home), pre, 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runInstall(testDeps(&out), "", false); err != nil {
		t.Fatalf("runInstall: %v", err)
	}

	raw, err := os.ReadFile(settingsPath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]hookcfg.Group `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	ss := doc.Hooks["SessionStart"]
	if len(ss) != 2 {
		t.Fatalf("SessionStart has %d groups; want 2 (mixed preserved + guild added)", len(ss))
	}
	if len(ss[0].Hooks) != 2 || ss[0].Hooks[1].Command != "other-tool prime" {
		t.Errorf("mixed group rewritten: %+v", ss[0])
	}
}

func TestAdapterNameForClient(t *testing.T) {
	cases := map[string]string{
		"Claude Code":    "claude-code",
		"Cursor":         "cursor",
		"Codex (OpenAI)": "codex",
		"Stub":           "stub",
	}
	for in, want := range cases {
		if got := adapterNameForClient(in); got != want {
			t.Errorf("adapterNameForClient(%q) = %q; want %q", in, got, want)
		}
	}
}
