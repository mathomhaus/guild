package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mathomhaus/guild/internal/hooks"
	"github.com/mathomhaus/guild/internal/hooks/adapters"
	"github.com/mathomhaus/guild/internal/project"
)

// testAdapter returns an Adapter whose ambient seams (cwd, git, codex
// CLI, detection) are pinned to a hermetic repo dir. The probe reports
// the hook signal so Install never prints the manual-add diagnostic
// unless a test overrides it.
func testAdapter(repo string) Adapter {
	return Adapter{
		detect: func() (bool, error) { return true, nil },
		getwd:  func() (string, error) { return repo, nil },
		gitToplevel: func(_ context.Context, _ string) (string, error) {
			return repo, nil
		},
		probe: func(_ context.Context, _ string) (string, error) {
			return "hook: SessionStart\nhook: SessionStart Completed\n", nil
		},
		diag: io.Discard,
	}
}

func settingsPath(repo string) string {
	return filepath.Join(repo, ".codex", "hooks.json")
}

// TestCodex_SelfRegisters: importing this package is enough to make
// the adapter visible through the registry (the init() side effect).
func TestCodex_SelfRegisters(t *testing.T) {
	a, ok := adapters.Lookup("codex")
	if !ok {
		t.Fatal("codex adapter not found in registry after import")
	}
	if a.Name() != "codex" {
		t.Errorf("Name() = %q; want %q", a.Name(), "codex")
	}
	if _, ok := a.(adapters.Renderer); !ok {
		t.Error("registered codex adapter does not implement adapters.Renderer")
	}
}

// TestCodex_SettingsPath_GitToplevel: inside a repo, the settings file
// lives under the git toplevel, not the working directory.
func TestCodex_SettingsPath_GitToplevel(t *testing.T) {
	repo := t.TempDir()
	a := Adapter{
		getwd: func() (string, error) { return filepath.Join(repo, "sub", "dir"), nil },
		gitToplevel: func(_ context.Context, dir string) (string, error) {
			if want := filepath.Join(repo, "sub", "dir"); dir != want {
				t.Errorf("gitToplevel called with %q; want %q", dir, want)
			}
			return repo, nil
		},
	}
	got, err := a.SettingsPath()
	if err != nil {
		t.Fatalf("SettingsPath: %v", err)
	}
	if want := settingsPath(repo); got != want {
		t.Errorf("SettingsPath = %q; want %q", got, want)
	}
}

// TestCodex_SettingsPath_FallbackToCwd: outside a git repo the working
// directory is the project root.
func TestCodex_SettingsPath_FallbackToCwd(t *testing.T) {
	dir := t.TempDir()
	a := Adapter{
		getwd: func() (string, error) { return dir, nil },
		gitToplevel: func(_ context.Context, _ string) (string, error) {
			return "", project.ErrNotInGitRepo
		},
	}
	got, err := a.SettingsPath()
	if err != nil {
		t.Fatalf("SettingsPath: %v", err)
	}
	if want := settingsPath(dir); got != want {
		t.Errorf("SettingsPath = %q; want %q", got, want)
	}
}

// TestCodex_Render_DefaultBase: the built-in base config narrows to
// Codex's contract: PreCompact dropped, SessionStart matcher loses
// "compact", UserPromptSubmit kept matcher-less, statusMessage set.
func TestCodex_Render_DefaultBase(t *testing.T) {
	got, err := Adapter{}.Render(hooks.DefaultBase())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if _, ok := got["PreCompact"]; ok {
		t.Error("PreCompact rendered; Codex has no such event")
	}
	if len(got) != 2 {
		t.Errorf("rendered %d events; want 2 (SessionStart, UserPromptSubmit)", len(got))
	}

	ss := got["SessionStart"]
	if len(ss) != 1 {
		t.Fatalf("SessionStart groups = %d; want 1", len(ss))
	}
	if ss[0].Matcher != "startup|resume|clear" {
		t.Errorf("SessionStart matcher = %q; want %q", ss[0].Matcher, "startup|resume|clear")
	}
	if cmd := ss[0].Hooks[0]; cmd.Command != "guild quest brief --auto" || cmd.StatusMessage == "" {
		t.Errorf("SessionStart command = %+v; want guild quest brief --auto with a statusMessage", cmd)
	}

	ups := got["UserPromptSubmit"]
	if len(ups) != 1 {
		t.Fatalf("UserPromptSubmit groups = %d; want 1", len(ups))
	}
	if ups[0].Matcher != "" {
		t.Errorf("UserPromptSubmit matcher = %q; want empty", ups[0].Matcher)
	}
	if cmd := ups[0].Hooks[0]; cmd.Command != "guild lore appraise --inject --from-stdin-json" || cmd.StatusMessage == "" {
		t.Errorf("UserPromptSubmit command = %+v; want the appraise inject verb with a statusMessage", cmd)
	}
}

// TestCodex_Render_DoesNotMutateInput: the base config the CLI holds
// must come back untouched.
func TestCodex_Render_DoesNotMutateInput(t *testing.T) {
	base := hooks.DefaultBase()
	if _, err := (Adapter{}).Render(base); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if m := base["SessionStart"][0].Matcher; m != "startup|resume|clear|compact" {
		t.Errorf("Render mutated input matcher to %q", m)
	}
	if sm := base["SessionStart"][0].Hooks[0].StatusMessage; sm != "" {
		t.Errorf("Render mutated input statusMessage to %q", sm)
	}
}

// TestCodex_Render_Validation: configs Codex cannot accept error
// instead of silently writing hooks that never fire.
func TestCodex_Render_Validation(t *testing.T) {
	cases := []struct {
		name    string
		cfg     hooks.Config
		wantErr string
	}{
		{
			name: "matcher on UserPromptSubmit",
			cfg: hooks.Config{
				"UserPromptSubmit": {{
					Matcher: "startup",
					Hooks:   []hooks.Command{{Type: "command", Command: "guild lore appraise --inject"}},
				}},
			},
			wantErr: "does not support a matcher",
		},
		{
			name: "SessionStart matcher with no supported source",
			cfg: hooks.Config{
				"SessionStart": {{
					Matcher: "compact",
					Hooks:   []hooks.Command{{Type: "command", Command: "guild quest brief --auto"}},
				}},
			},
			wantErr: "no Codex-supported source",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Adapter{}.Render(tc.cfg)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestCodex_Render_EdgeMatchers: an empty SessionStart matcher stays
// empty (fires on every source) and unknown events vanish quietly.
func TestCodex_Render_EdgeMatchers(t *testing.T) {
	cfg := hooks.Config{
		"SessionStart": {{
			Hooks: []hooks.Command{{Type: "command", Command: "guild quest brief --auto"}},
		}},
		"SomeFutureEvent": {{
			Hooks: []hooks.Command{{Type: "command", Command: "guild quest pulse"}},
		}},
	}
	got, err := Adapter{}.Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("rendered %d events; want 1", len(got))
	}
	if m := got["SessionStart"][0].Matcher; m != "" {
		t.Errorf("empty matcher rewritten to %q; want empty", m)
	}
}

// TestCodex_InstallSyncScanRoundTrip exercises the full adapter
// contract against a hermetic repo dir: install writes the rendered
// config, scan reads it back, a second sync is a byte-level no-op.
func TestCodex_InstallSyncScanRoundTrip(t *testing.T) {
	repo := t.TempDir()
	a := testAdapter(repo)
	base := hooks.DefaultBase()

	if err := a.Install(base); err != nil {
		t.Fatalf("Install: %v", err)
	}
	first, err := os.ReadFile(settingsPath(repo))
	if err != nil {
		t.Fatalf("settings file not written: %v", err)
	}

	// The on-disk document carries the Codex-rendered shape.
	var doc struct {
		Hooks map[string][]hooks.Group `json:"hooks"`
	}
	if err := json.Unmarshal(first, &doc); err != nil {
		t.Fatalf("settings file is not valid JSON: %v", err)
	}
	if _, ok := doc.Hooks["PreCompact"]; ok {
		t.Error("PreCompact written to Codex settings")
	}
	if m := doc.Hooks["SessionStart"][0].Matcher; m != "startup|resume|clear" {
		t.Errorf("on-disk SessionStart matcher = %q; want startup|resume|clear", m)
	}
	if !strings.Contains(string(first), `"statusMessage"`) {
		t.Error("on-disk settings missing statusMessage")
	}

	scanned, err := a.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(scanned) != 2 {
		t.Fatalf("Scan returned %d hooks; want 2", len(scanned))
	}
	for _, h := range scanned {
		if !h.GuildOwned {
			t.Errorf("hook %q not guild-owned after clean install", h.Command)
		}
	}

	// Second sync: idempotent, no rewrite.
	if err := a.Sync(base); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	second, err := os.ReadFile(settingsPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("second Sync rewrote the file:\nfirst:  %s\nsecond: %s", first, second)
	}
}

// TestCodex_PreservesForeignContent: installing over a hooks.json that
// already carries foreign hooks and unrelated fields keeps both, and
// scan classifies ownership per group.
func TestCodex_PreservesForeignContent(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	pre := []byte(`{
  "model": "gpt-5.1",
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [{"type": "command", "command": "other-tool prime"}]}
    ]
  }
}`)
	if err := os.WriteFile(settingsPath(repo), pre, 0o600); err != nil {
		t.Fatal(err)
	}

	a := testAdapter(repo)
	if err := a.Install(hooks.DefaultBase()); err != nil {
		t.Fatalf("Install: %v", err)
	}

	raw, err := os.ReadFile(settingsPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, `"model"`) {
		t.Error("foreign top-level field dropped by install")
	}
	if !strings.Contains(got, "other-tool prime") {
		t.Error("foreign hook dropped by install")
	}

	scanned, err := a.Scan()
	if err != nil {
		t.Fatal(err)
	}
	var foreign, guild int
	for _, h := range scanned {
		if h.GuildOwned {
			guild++
		} else {
			foreign++
		}
	}
	if foreign != 1 || guild != 2 {
		t.Errorf("scan after install: %d foreign / %d guild; want 1 / 2", foreign, guild)
	}
}

// TestCodex_SyncRemovesStaleGuildEvents: a guild-owned group under an
// event the render no longer produces (e.g. a PreCompact group from an
// older guild) is cleaned up by sync.
func TestCodex_SyncRemovesStaleGuildEvents(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	pre := []byte(`{
  "hooks": {
    "PreCompact": [
      {"matcher": "auto|manual", "hooks": [{"type": "command", "command": "guild quest brief --auto --capture"}]}
    ]
  }
}`)
	if err := os.WriteFile(settingsPath(repo), pre, 0o600); err != nil {
		t.Fatal(err)
	}

	a := testAdapter(repo)
	if err := a.Sync(hooks.DefaultBase()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	scanned, err := a.Scan()
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range scanned {
		if h.Event == "PreCompact" {
			t.Errorf("stale guild-owned PreCompact hook survived sync: %+v", h)
		}
	}
}

// TestCodex_ScanMissingFileIsEmpty: a never-installed repo scans to an
// empty list, not an error.
func TestCodex_ScanMissingFileIsEmpty(t *testing.T) {
	a := testAdapter(t.TempDir())
	hs, err := a.Scan()
	if err != nil {
		t.Fatalf("Scan on missing file: %v", err)
	}
	if len(hs) != 0 {
		t.Errorf("Scan on missing file returned %d hooks; want 0", len(hs))
	}
}

// TestCodex_SyncRejectsInvalidBase: Sync surfaces Render validation
// errors instead of writing a broken settings file.
func TestCodex_SyncRejectsInvalidBase(t *testing.T) {
	repo := t.TempDir()
	a := testAdapter(repo)
	bad := hooks.Config{
		"UserPromptSubmit": {{
			Matcher: "oops",
			Hooks:   []hooks.Command{{Type: "command", Command: "guild lore appraise --inject"}},
		}},
	}
	if err := a.Sync(bad); err == nil {
		t.Fatal("Sync accepted a matcher on UserPromptSubmit")
	}
	if _, err := os.Stat(settingsPath(repo)); !os.IsNotExist(err) {
		t.Error("Sync wrote a settings file despite the validation error")
	}
}

// Probe behavior: the test fire diagnoses but never fails Install.
func TestCodex_InstallProbe(t *testing.T) {
	cases := []struct {
		name       string
		probe      func(context.Context, string) (string, error)
		wantDiag   string
		rejectDiag string
	}{
		{
			name: "signal observed",
			probe: func(_ context.Context, _ string) (string, error) {
				return "boot...\nhook: SessionStart\nhook: SessionStart Completed\n", nil
			},
			wantDiag:   "hooks are live",
			rejectDiag: "codex_hooks",
		},
		{
			name: "no signal",
			probe: func(_ context.Context, _ string) (string, error) {
				return "boot...\nno hooks here\n", nil
			},
			wantDiag: "[features] codex_hooks = true",
		},
		{
			name: "cli missing",
			probe: func(_ context.Context, _ string) (string, error) {
				return "", errNoCodexCLI
			},
			wantDiag: "codex CLI not on PATH",
		},
		{
			name: "probe error with partial output carrying the signal",
			probe: func(_ context.Context, _ string) (string, error) {
				return "hook: SessionStart\n", context.DeadlineExceeded
			},
			wantDiag:   "hooks are live",
			rejectDiag: "codex_hooks",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			var diag bytes.Buffer
			a := testAdapter(repo)
			a.probe = tc.probe
			a.diag = &diag

			if err := a.Install(hooks.DefaultBase()); err != nil {
				t.Fatalf("Install must not fail on probe outcomes: %v", err)
			}
			if _, err := os.Stat(settingsPath(repo)); err != nil {
				t.Fatalf("settings file missing after install: %v", err)
			}
			if !strings.Contains(diag.String(), tc.wantDiag) {
				t.Errorf("diagnostic missing %q:\n%s", tc.wantDiag, diag.String())
			}
			if tc.rejectDiag != "" && strings.Contains(diag.String(), tc.rejectDiag) {
				t.Errorf("diagnostic unexpectedly mentions %q:\n%s", tc.rejectDiag, diag.String())
			}
		})
	}
}

// TestCodex_DefaultDetect_Hermetic: with PATH and HOME pointed at
// empty temp dirs the registry probe finds nothing; dropping a
// ~/.codex/config.toml in flips detection on without the CLI.
func TestCodex_DefaultDetect_Hermetic(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())

	detected, err := Adapter{}.Detect()
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if detected {
		t.Error("Detect = true with no codex CLI and no config")
	}

	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("# codex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	detected, err = Adapter{}.Detect()
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !detected {
		t.Error("Detect = false with ~/.codex/config.toml present")
	}
}
