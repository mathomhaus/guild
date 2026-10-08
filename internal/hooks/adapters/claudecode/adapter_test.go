package claudecode

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mathomhaus/guild/internal/hooks"
	"github.com/mathomhaus/guild/internal/hooks/adapters"
)

// chtemp moves the test into a fresh temp dir (outside any git repo,
// so the project root falls back to the directory itself) and returns
// it. Settings written by the adapter land under this dir, never in
// the developer's real checkout.
func chtemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

// resolvePath resolves symlinks in the longest existing prefix of
// path and keeps the (possibly not-yet-created) remainder verbatim.
// macOS temp dirs live behind /var -> /private/var, and the paths
// under comparison may not exist yet.
func resolvePath(path string) string {
	rest := ""
	for p := path; ; p = filepath.Dir(p) {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		if filepath.Dir(p) == p {
			return path
		}
	}
}

// mustSamePath compares two paths after symlink resolution.
func mustSamePath(t *testing.T, got, want string) {
	t.Helper()
	if g, w := resolvePath(got), resolvePath(want); g != w {
		t.Errorf("path = %q (resolved %q); want %q (resolved %q)", got, g, want, w)
	}
}

// settingsOn returns the settings path the adapter resolves right now,
// asserting it stays under root.
func settingsOn(t *testing.T, a adapters.Adapter, root string) string {
	t.Helper()
	path, err := a.SettingsPath()
	if err != nil {
		t.Fatalf("SettingsPath: %v", err)
	}
	mustSamePath(t, filepath.Dir(filepath.Dir(path)), root)
	return path
}

// TestSelfRegisters: importing this package registers the adapter
// under the key the `guild hooks` CLI derives from the install
// registry's "Claude Code" display name.
func TestSelfRegisters(t *testing.T) {
	a, ok := adapters.Lookup("claude-code")
	if !ok {
		t.Fatal("claude-code adapter not found in registry after import")
	}
	if a.Name() != "claude-code" {
		t.Errorf("Name() = %q; want %q", a.Name(), "claude-code")
	}
}

// TestCleanInstall: installing into a project with no .claude/
// settings.json writes the spec'd three events with the exact matcher
// and command strings, wrapped under the top-level "hooks" key.
func TestCleanInstall(t *testing.T) {
	root := chtemp(t)
	a := Adapter{}

	if err := a.Install(hooks.DefaultBase()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	path := settingsOn(t, a, root)
	mustSamePath(t, path, filepath.Join(root, ".claude", "settings.json"))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("settings file not written: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("settings file is not valid JSON: %v", err)
	}
	rawHooks, ok := doc["hooks"]
	if !ok {
		t.Fatalf("settings file missing top-level \"hooks\" key: %s", raw)
	}
	var cfg hooks.Config
	if err := json.Unmarshal(rawHooks, &cfg); err != nil {
		t.Fatalf("hooks section does not parse: %v", err)
	}

	checks := []struct {
		event   string
		matcher string
		command string
	}{
		{"SessionStart", "startup|resume|clear|compact", "guild quest brief --auto"},
		{"PreCompact", "auto|manual", "guild quest brief --auto --capture"},
		{"UserPromptSubmit", "", "guild lore appraise --inject --from-stdin-json"},
	}
	for _, c := range checks {
		groups := cfg[c.event]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Errorf("event %s: got %d groups; want 1 group with 1 hook", c.event, len(groups))
			continue
		}
		if groups[0].Matcher != c.matcher {
			t.Errorf("event %s: matcher = %q; want %q", c.event, groups[0].Matcher, c.matcher)
		}
		if got := groups[0].Hooks[0].Command; got != c.command {
			t.Errorf("event %s: command = %q; want %q", c.event, got, c.command)
		}
		if groups[0].Hooks[0].Type != "command" {
			t.Errorf("event %s: hook type = %q; want %q", c.event, groups[0].Hooks[0].Type, "command")
		}
	}

	// UserPromptSubmit must not carry a matcher key at all: Claude Code
	// does not support one on that event, and omitempty keeps it out.
	var byEvent map[string][]json.RawMessage
	if err := json.Unmarshal(rawHooks, &byEvent); err != nil {
		t.Fatal(err)
	}
	for _, g := range byEvent["UserPromptSubmit"] {
		if strings.Contains(string(g), `"matcher"`) {
			t.Errorf("UserPromptSubmit group carries a matcher key: %s", g)
		}
	}

	scanned, err := a.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(scanned) != 3 {
		t.Fatalf("Scan returned %d hooks; want 3", len(scanned))
	}
	for _, h := range scanned {
		if !h.GuildOwned {
			t.Errorf("hook %q not guild-owned after clean install", h.Command)
		}
	}
}

// TestIdempotentResync: a second sync over an in-sync file is a no-op,
// byte-for-byte and write-for-write.
func TestIdempotentResync(t *testing.T) {
	root := chtemp(t)
	a := Adapter{}
	base := hooks.DefaultBase()

	if err := a.Install(base); err != nil {
		t.Fatalf("Install: %v", err)
	}
	path := settingsOn(t, a, root)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	firstStat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Sync(base); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("second Sync changed the file:\nfirst:  %s\nsecond: %s", first, second)
	}
	secondStat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !firstStat.ModTime().Equal(secondStat.ModTime()) {
		t.Error("second Sync rewrote an in-sync file (mtime changed)")
	}
}

// TestCoexistsWithForeignHooks: installing over a settings file that
// already carries another tool's hooks and unrelated top-level fields
// keeps all of it intact alongside the guild groups.
func TestCoexistsWithForeignHooks(t *testing.T) {
	root := chtemp(t)
	a := Adapter{}

	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := []byte(`{
  "model": "opus",
  "permissions": {"allow": ["Bash(npm run test)"]},
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [{"type": "command", "command": "gt prime"}]}
    ],
    "PostToolUse": [
      {"matcher": "Edit|Write", "hooks": [{"type": "command", "command": "npm run lint"}]}
    ]
  }
}`)
	if err := os.WriteFile(filepath.Join(root, ".claude", "settings.json"), pre, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := a.Install(hooks.DefaultBase()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, keep := range []string{`"model"`, `"permissions"`, `"Bash(npm run test)"`, `"gt prime"`, `"npm run lint"`, `"Edit|Write"`} {
		if !strings.Contains(got, keep) {
			t.Errorf("foreign content %s dropped by install:\n%s", keep, got)
		}
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
	if foreign != 2 || guild != 3 {
		t.Errorf("scan after install: %d foreign / %d guild; want 2 / 3", foreign, guild)
	}
}

// TestOwnershipRuleAgainstExistingGroups: ownership is decided by the
// command string, never by matcher inspection. A stale all-guild group
// is replaced in place; a mixed group (guild plus foreign command) and
// a "guildctl" near-miss are foreign and preserved verbatim.
func TestOwnershipRuleAgainstExistingGroups(t *testing.T) {
	root := chtemp(t)
	a := Adapter{}

	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := []byte(`{
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [{"type": "command", "command": "guild brief --auto"}]},
      {"matcher": "resume", "hooks": [
        {"type": "command", "command": "guild quest brief --auto"},
        {"type": "command", "command": "npm test"}
      ]},
      {"matcher": "clear", "hooks": [{"type": "command", "command": "guildctl prime"}]}
    ]
  }
}`)
	path := filepath.Join(root, ".claude", "settings.json")
	if err := os.WriteFile(path, pre, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := a.Sync(hooks.DefaultBase()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	// Stale guild-owned group (old command shape) replaced by the
	// current one; the old command string is gone.
	if strings.Contains(got, `"guild brief --auto"`) {
		t.Errorf("stale guild-owned group not replaced:\n%s", got)
	}
	if !strings.Contains(got, `"startup|resume|clear|compact"`) {
		t.Errorf("desired SessionStart group missing after sync:\n%s", got)
	}
	// Mixed group and guildctl near-miss preserved.
	for _, keep := range []string{`"npm test"`, `"guildctl prime"`} {
		if !strings.Contains(got, keep) {
			t.Errorf("foreign group content %s dropped by sync:\n%s", keep, got)
		}
	}

	scanned, err := a.Scan()
	if err != nil {
		t.Fatal(err)
	}
	var owned []string
	var foreign []string
	for _, h := range scanned {
		if h.GuildOwned {
			owned = append(owned, h.Command)
		} else {
			foreign = append(foreign, h.Command)
		}
	}
	// Guild-owned: the three desired commands exactly once each.
	if len(owned) != 3 {
		t.Errorf("guild-owned commands after sync = %v; want 3", owned)
	}
	// Foreign: both commands of the mixed group plus the guildctl one.
	// The guild command inside the mixed group stays foreign: mixed
	// groups are never partially rewritten.
	if len(foreign) != 3 {
		t.Errorf("foreign commands after sync = %v; want 3", foreign)
	}
}

// TestValidate: per-event matcher validation against Claude Code's
// contract. Matchers on no-matcher events and unparseable regexes are
// hard errors; dead matchers on closed-vocabulary events warn.
func TestValidate(t *testing.T) {
	group := func(ev, matcher string) adapters.Config {
		return adapters.Config{ev: {{
			Matcher: matcher,
			Hooks:   []hooks.Command{{Type: "command", Command: "guild quest brief --auto"}},
		}}}
	}
	cases := []struct {
		name     string
		cfg      adapters.Config
		wantErr  string
		wantWarn int
	}{
		{name: "defaults are clean", cfg: hooks.DefaultBase()},
		{name: "matcher on UserPromptSubmit rejected", cfg: group("UserPromptSubmit", "startup"),
			wantErr: "does not support a matcher"},
		{name: "matcher on Stop rejected", cfg: group("Stop", ".*"),
			wantErr: "does not support a matcher"},
		{name: "matcher on TaskCreated rejected", cfg: group("TaskCreated", "auto"),
			wantErr: "does not support a matcher"},
		{name: "invalid regex rejected", cfg: group("SessionStart", "(startup"),
			wantErr: "not a valid regular expression"},
		{name: "dead SessionStart matcher warns", cfg: group("SessionStart", "quux"),
			wantWarn: 1},
		{name: "dead PreCompact matcher warns", cfg: group("PreCompact", "sometimes"),
			wantWarn: 1},
		{name: "narrowed SessionStart matcher is fine", cfg: group("SessionStart", "startup|resume")},
		{name: "narrowed PreCompact matcher is fine", cfg: group("PreCompact", "manual")},
		{name: "tool-name matcher passes open vocabulary", cfg: group("PreToolUse", "Bash|Edit")},
		{name: "unknown event passes through", cfg: group("SomeFutureEvent", "whatever")},
		{name: "empty matcher is always fine", cfg: group("UserPromptSubmit", "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warnings, err := Validate(tc.cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Validate err = %v; want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: unexpected error: %v", err)
			}
			if len(warnings) != tc.wantWarn {
				t.Errorf("Validate warnings = %v; want %d", warnings, tc.wantWarn)
			}
		})
	}
}

// TestSyncRejectsInvalidConfig: validation runs before the write; an
// invalid config leaves no settings file behind.
func TestSyncRejectsInvalidConfig(t *testing.T) {
	root := chtemp(t)
	a := Adapter{}

	bad := adapters.Config{
		"UserPromptSubmit": {{
			Matcher: "startup",
			Hooks:   []hooks.Command{{Type: "command", Command: "guild lore appraise --inject --from-stdin-json"}},
		}},
	}
	err := a.Sync(bad)
	if err == nil || !strings.Contains(err.Error(), "does not support a matcher") {
		t.Fatalf("Sync(bad) err = %v; want matcher rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".claude", "settings.json")); !os.IsNotExist(statErr) {
		t.Errorf("settings file written despite invalid config (stat err = %v)", statErr)
	}
}

// TestScanMissingFileIsEmpty: a project without .claude/settings.json
// scans to an empty list, not an error.
func TestScanMissingFileIsEmpty(t *testing.T) {
	chtemp(t)
	hs, err := Adapter{}.Scan()
	if err != nil {
		t.Fatalf("Scan on missing file: %v", err)
	}
	if len(hs) != 0 {
		t.Errorf("Scan on missing file returned %d hooks; want 0", len(hs))
	}
}

// TestSettingsPathGitToplevel: inside a repository the settings file
// resolves against the work-tree root even from a subdirectory.
func TestSettingsPathGitToplevel(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	gitInit := exec.Command("git", "init", "-q")
	gitInit.Dir = root
	if out, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	sub := filepath.Join(root, "cmd", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	path, err := Adapter{}.SettingsPath()
	if err != nil {
		t.Fatalf("SettingsPath: %v", err)
	}
	mustSamePath(t, path, filepath.Join(root, ".claude", "settings.json"))
}

// TestSettingsPathNonGitFallback: outside any repository the project
// root is the working directory itself.
func TestSettingsPathNonGitFallback(t *testing.T) {
	dir := chtemp(t)
	path, err := Adapter{}.SettingsPath()
	if err != nil {
		t.Fatalf("SettingsPath: %v", err)
	}
	mustSamePath(t, path, filepath.Join(dir, ".claude", "settings.json"))
}

// TestDetect: detection reuses the install registry's Claude Code
// entry: CLI probe first, then the ~/.claude.json config probe.
func TestDetect(t *testing.T) {
	a := Adapter{}

	t.Run("nothing present", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		got, err := a.Detect()
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if got {
			t.Error("Detect = true with no claude CLI and no ~/.claude.json")
		}
	})

	t.Run("cli on PATH", func(t *testing.T) {
		bin := t.TempDir()
		fake := filepath.Join(bin, "claude")
		if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // fake test executable
			t.Fatal(err)
		}
		t.Setenv("PATH", bin)
		t.Setenv("HOME", t.TempDir())
		got, err := a.Detect()
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if !got {
			t.Error("Detect = false with claude on PATH")
		}
	})

	t.Run("config probe", func(t *testing.T) {
		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", t.TempDir())
		t.Setenv("HOME", home)
		got, err := a.Detect()
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if !got {
			t.Error("Detect = false with ~/.claude.json present")
		}
	})
}

// TestSubstituteIsIdentity: Claude Code has no command placeholders;
// the prompt travels in the stdin JSON envelope.
func TestSubstituteIsIdentity(t *testing.T) {
	const cmd = "guild lore appraise --inject --from-stdin-json"
	if got := (Adapter{}).Substitute(cmd); got != cmd {
		t.Errorf("Substitute(%q) = %q; want unchanged", cmd, got)
	}
}
