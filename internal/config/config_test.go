package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/mathomhaus/guild/internal/lore"
)

// writeTOML creates a config.toml inside a temporary .guild/ directory under
// dir and returns dir so the caller can os.Chdir or use it as repoConfigPath input.
func writeTOML(t *testing.T, dir, content string) string {
	t.Helper()
	guildDir := filepath.Join(dir, ".guild")
	if err := os.MkdirAll(guildDir, 0o700); err != nil {
		t.Fatalf("mkdir .guild: %v", err)
	}
	p := filepath.Join(guildDir, "config.toml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	return dir
}

// ---- unit: defaults --------------------------------------------------------

func TestDefaults(t *testing.T) {
	d := defaults()
	if d.Scoring.WFTS != 0.7 {
		t.Errorf("WFTS default: got %v want 0.7", d.Scoring.WFTS)
	}
	if d.Scoring.WRecency != 0 {
		t.Errorf("WRecency default: got %v want 0", d.Scoring.WRecency)
	}
	if d.Scoring.HalfLifeDays != 30 {
		t.Errorf("HalfLifeDays default: got %v want 30", d.Scoring.HalfLifeDays)
	}
	if d.Scoring.TitleMatchBoost != 1.0 {
		t.Errorf("TitleMatchBoost default: got %v want 1.0", d.Scoring.TitleMatchBoost)
	}
	if d.Scoring.TitleTokenBoost != 0.5 {
		t.Errorf("TitleTokenBoost default: got %v want 0.5", d.Scoring.TitleTokenBoost)
	}
	if d.Inscribe.PrincipleMaxWords != 60 {
		t.Errorf("PrincipleMaxWords default: got %v want 60", d.Inscribe.PrincipleMaxWords)
	}
	if d.Inscribe.BloatSevereThreshold != 120 {
		t.Errorf("BloatSevereThreshold default: got %v want 120", d.Inscribe.BloatSevereThreshold)
	}
	if d.Telemetry.UsageLog {
		t.Error("UsageLog default: got true, want false (telemetry is opt-in)")
	}
}

// ---- unit: fileLayer -------------------------------------------------------

func TestFileLayerMissingFileIsNotError(t *testing.T) {
	cfg := defaults()
	if err := fileLayer("/nonexistent/path/config.toml", &cfg); err != nil {
		t.Errorf("missing file should not error, got: %v", err)
	}
	// Defaults unchanged.
	if cfg.Scoring.WFTS != 0.7 {
		t.Errorf("WFTS should be default 0.7, got %v", cfg.Scoring.WFTS)
	}
}

func TestFileLayerEmptyPathIsNoop(t *testing.T) {
	cfg := defaults()
	if err := fileLayer("", &cfg); err != nil {
		t.Errorf("empty path should not error, got: %v", err)
	}
}

func TestFileLayerPartialOverride(t *testing.T) {
	// Only w_fts declared — other scoring keys must remain at defaults.
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	content := `[scoring]
w_fts = 0.5
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	if err := fileLayer(p, &cfg); err != nil {
		t.Fatalf("fileLayer: %v", err)
	}
	if cfg.Scoring.WFTS != 0.5 {
		t.Errorf("w_fts: got %v want 0.5", cfg.Scoring.WFTS)
	}
	// Untouched keys must remain at defaults.
	if cfg.Scoring.WRecency != 0 {
		t.Errorf("w_recency should be unchanged 0, got %v", cfg.Scoring.WRecency)
	}
	if cfg.Scoring.HalfLifeDays != 30 {
		t.Errorf("half_life_days should be unchanged 30, got %v", cfg.Scoring.HalfLifeDays)
	}
}

func TestFileLayerAllSections(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	content := `[scoring]
w_fts = 0.6
w_recency = 0.4
half_life_days = 14
title_match_boost = 2.0
title_token_boost = 0.8

[inscribe]
principle_max_words = 50
bloat_severe_threshold = 100

[telemetry]
usage_log = false
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	if err := fileLayer(p, &cfg); err != nil {
		t.Fatalf("fileLayer: %v", err)
	}
	if cfg.Scoring.WFTS != 0.6 {
		t.Errorf("w_fts: got %v want 0.6", cfg.Scoring.WFTS)
	}
	if cfg.Scoring.WRecency != 0.4 {
		t.Errorf("w_recency: got %v want 0.4", cfg.Scoring.WRecency)
	}
	if cfg.Scoring.HalfLifeDays != 14 {
		t.Errorf("half_life_days: got %v want 14", cfg.Scoring.HalfLifeDays)
	}
	if cfg.Scoring.TitleMatchBoost != 2.0 {
		t.Errorf("title_match_boost: got %v want 2.0", cfg.Scoring.TitleMatchBoost)
	}
	if cfg.Scoring.TitleTokenBoost != 0.8 {
		t.Errorf("title_token_boost: got %v want 0.8", cfg.Scoring.TitleTokenBoost)
	}
	if cfg.Inscribe.PrincipleMaxWords != 50 {
		t.Errorf("principle_max_words: got %v want 50", cfg.Inscribe.PrincipleMaxWords)
	}
	if cfg.Inscribe.BloatSevereThreshold != 100 {
		t.Errorf("bloat_severe_threshold: got %v want 100", cfg.Inscribe.BloatSevereThreshold)
	}
	if cfg.Telemetry.UsageLog {
		t.Error("usage_log: got true, want false")
	}
}

// ---- unit: envLayer --------------------------------------------------------

func TestEnvLayerGUILD_PROJECT(t *testing.T) {
	t.Setenv("GUILD_PROJECT", "testproj")
	cfg := defaults()
	envLayer(&cfg)
	if cfg.Project != "testproj" {
		t.Errorf("GUILD_PROJECT: got %q want %q", cfg.Project, "testproj")
	}
}

func TestEnvLayerGUILD_NO_USAGE_LOG(t *testing.T) {
	t.Setenv("GUILD_NO_USAGE_LOG", "1")
	cfg := defaults()
	envLayer(&cfg)
	if !cfg.NoUsageLog {
		t.Error("GUILD_NO_USAGE_LOG=1: NoUsageLog should be true")
	}
	if cfg.Telemetry.UsageLog {
		t.Error("GUILD_NO_USAGE_LOG=1: Telemetry.UsageLog should be false")
	}
}

func TestEnvLayerGUILD_NO_EMOJI(t *testing.T) {
	t.Setenv("GUILD_NO_EMOJI", "1")
	cfg := defaults()
	envLayer(&cfg)
	if !cfg.NoEmoji {
		t.Error("GUILD_NO_EMOJI=1: NoEmoji should be true")
	}
}

func TestEnvLayerEmpty(t *testing.T) {
	// No env vars set — nothing changes.
	t.Setenv("GUILD_PROJECT", "")
	t.Setenv("GUILD_NO_USAGE_LOG", "")
	t.Setenv("GUILD_NO_EMOJI", "")
	cfg := defaults()
	envLayer(&cfg)
	if cfg.Project != "" {
		t.Errorf("empty GUILD_PROJECT: got %q want empty", cfg.Project)
	}
	if cfg.NoUsageLog {
		t.Error("empty GUILD_NO_USAGE_LOG: NoUsageLog should be false")
	}
	if cfg.NoEmoji {
		t.Error("empty GUILD_NO_EMOJI: NoEmoji should be false")
	}
}

// ---- unit: flagLayer -------------------------------------------------------

func TestFlagLayerProjectFlag(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.String("project", "", "project name")
	if err := fs.Parse([]string{"--project", "myflag-proj"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	flagLayer(fs, &cfg)
	if cfg.Project != "myflag-proj" {
		t.Errorf("--project: got %q want %q", cfg.Project, "myflag-proj")
	}
}

func TestFlagLayerNoEmojiFlag(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.Bool("no-emoji", false, "disable emoji")
	if err := fs.Parse([]string{"--no-emoji"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	flagLayer(fs, &cfg)
	if !cfg.NoEmoji {
		t.Error("--no-emoji: NoEmoji should be true")
	}
}

func TestFlagLayerWFTSFlag(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.Float64("w-fts", 0.7, "fts weight")
	if err := fs.Parse([]string{"--w-fts", "0.9"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	flagLayer(fs, &cfg)
	if cfg.Scoring.WFTS != 0.9 {
		t.Errorf("--w-fts: got %v want 0.9", cfg.Scoring.WFTS)
	}
}

func TestFlagLayerUnchangedWhenNotSet(t *testing.T) {
	// Flags registered but not passed on command line — must not change defaults.
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.Float64("w-fts", 0.7, "fts weight")
	if err := fs.Parse([]string{}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	flagLayer(fs, &cfg)
	if cfg.Scoring.WFTS != 0.7 {
		t.Errorf("unset --w-fts: got %v, default should be 0.7", cfg.Scoring.WFTS)
	}
}

func TestFlagLayerNilFlagSetIsNoop(t *testing.T) {
	cfg := defaults()
	flagLayer(nil, &cfg)
	if cfg.Scoring.WFTS != 0.7 {
		t.Error("nil FlagSet must be a no-op")
	}
}

// ---- integration: Load (5-layer precedence table test) --------------------
//
// This is the acceptance criterion test from QUEST-2.
//
// Scenario:
//
//	Layer 1 (defaults):          WFTS=0.7  WRecency=0  HalfLifeDays=30  PrincipleMaxWords=60
//	Layer 2 (user config):       WFTS=0.6
//	Layer 3 (repo config):       WRecency=0.2
//	Layer 4 (env):               GUILD_PROJECT="envproj"  GUILD_NO_EMOJI=1
//	Layer 5 (flag):              --w-fts=0.5
//
//	Expected after merge:
//	  WFTS             = 0.5  (flag wins over user config 0.6)
//	  WRecency         = 0.2  (repo config wins over default 0)
//	  HalfLifeDays     = 30   (untouched: default)
//	  PrincipleMaxWords= 60   (untouched: default)
//	  Project          = "envproj"  (env)
//	  NoEmoji          = true (env)
//	  NoUsageLog       = true   (reconciled from default-off UsageLog)
//	  UsageLog         = false  (default — telemetry is opt-in)
func TestLoadPrecedenceTable(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()

	// Layer 2: user config overrides only WFTS.
	userGuildDir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(userGuildDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userGuildDir, "config.toml"),
		[]byte("[scoring]\nw_fts = 0.6\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Layer 3: repo config overrides only WRecency.
	writeTOML(t, repo, "[scoring]\nw_recency = 0.2\n")

	// Layer 4: env vars.
	t.Setenv("GUILD_PROJECT", "envproj")
	t.Setenv("GUILD_NO_EMOJI", "1")
	t.Setenv("GUILD_NO_USAGE_LOG", "")

	// Layer 5: CLI flag overrides w-fts (beats user config's 0.6).
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.Float64("w-fts", 0.7, "")
	if err := fs.Parse([]string{"--w-fts", "0.5"}); err != nil {
		t.Fatal(err)
	}

	// Swap HOME so userConfigDir() resolves to our temp home.
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", home)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Switch working directory to repo so repoConfigPath finds the repo config.
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWd) }()

	cfg, err := Load(fs)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cases := []struct {
		label string
		got   interface{}
		want  interface{}
	}{
		{"WFTS (flag=0.5 beats user=0.6)", cfg.Scoring.WFTS, 0.5},
		{"WRecency (repo=0.2 beats default=0)", cfg.Scoring.WRecency, 0.2},
		{"HalfLifeDays (untouched default=30)", cfg.Scoring.HalfLifeDays, float64(30)},
		{"TitleMatchBoost (untouched default=1.0)", cfg.Scoring.TitleMatchBoost, 1.0},
		{"TitleTokenBoost (untouched default=0.5)", cfg.Scoring.TitleTokenBoost, 0.5},
		{"PrincipleMaxWords (untouched default=60)", cfg.Inscribe.PrincipleMaxWords, 60},
		{"BloatSevereThreshold (untouched default=120)", cfg.Inscribe.BloatSevereThreshold, 120},
		{"UsageLog (default=false, telemetry opt-in)", cfg.Telemetry.UsageLog, false},
		{"NoUsageLog (reconciled from default-off UsageLog)", cfg.NoUsageLog, true},
		{"Project (env=envproj)", cfg.Project, "envproj"},
		{"NoEmoji (env=1)", cfg.NoEmoji, true},
	}

	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.label, tc.got, tc.want)
		}
	}
}

// TestLoadMissingConfigFilesNotError verifies that a system with no user or
// repo config files returns defaults without error.
func TestLoadMissingConfigFilesNotError(t *testing.T) {
	home := t.TempDir() // no .guild/ created inside
	repo := t.TempDir() // no .guild/ inside either

	t.Setenv("HOME", home)
	t.Setenv("GUILD_PROJECT", "")
	t.Setenv("GUILD_NO_USAGE_LOG", "")
	t.Setenv("GUILD_NO_EMOJI", "")

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWd) }()

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load with no config files: %v", err)
	}
	d := defaults()
	if cfg.Scoring.WFTS != d.Scoring.WFTS {
		t.Errorf("WFTS: got %v want default %v", cfg.Scoring.WFTS, d.Scoring.WFTS)
	}
	if cfg.Inscribe.PrincipleMaxWords != d.Inscribe.PrincipleMaxWords {
		t.Errorf("PrincipleMaxWords: got %v want default %v",
			cfg.Inscribe.PrincipleMaxWords, d.Inscribe.PrincipleMaxWords)
	}
}

// TestLoadRepoLayerDoesNotClobberUserLayer verifies that a per-project config
// that sets only one key does not reset other keys set by the user config.
func TestLoadRepoLayerDoesNotClobberUserLayer(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()

	// User config: sets WFTS=0.6 AND WRecency=0.1.
	userGuildDir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(userGuildDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userGuildDir, "config.toml"),
		[]byte("[scoring]\nw_fts = 0.6\nw_recency = 0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Repo config: sets ONLY HalfLifeDays — must NOT touch w_fts or w_recency.
	writeTOML(t, repo, "[scoring]\nhalf_life_days = 7\n")

	t.Setenv("HOME", home)
	t.Setenv("GUILD_PROJECT", "")
	t.Setenv("GUILD_NO_USAGE_LOG", "")
	t.Setenv("GUILD_NO_EMOJI", "")

	origWd, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWd) }()

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Scoring.WFTS != 0.6 {
		t.Errorf("WFTS: user layer set 0.6 but got %v", cfg.Scoring.WFTS)
	}
	if cfg.Scoring.WRecency != 0.1 {
		t.Errorf("WRecency: user layer set 0.1 but got %v", cfg.Scoring.WRecency)
	}
	if cfg.Scoring.HalfLifeDays != 7 {
		t.Errorf("HalfLifeDays: repo layer set 7 but got %v", cfg.Scoring.HalfLifeDays)
	}
}

// TestLoadTelemetryDisabled verifies that NoUsageLog is consistent with
// Telemetry.UsageLog under the default-off policy and via explicit env override.
// Telemetry is opt-in: logging is off unless [telemetry] usage_log = true is set.
func TestLoadTelemetryDisabled(t *testing.T) {
	t.Run("default_off_confirmed_via_toml", func(t *testing.T) {
		home := t.TempDir()
		repo := t.TempDir()

		userGuildDir := filepath.Join(home, ".guild")
		if err := os.MkdirAll(userGuildDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(userGuildDir, "config.toml"),
			[]byte("[telemetry]\nusage_log = false\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		t.Setenv("HOME", home)
		t.Setenv("GUILD_NO_USAGE_LOG", "")
		t.Setenv("GUILD_PROJECT", "")
		t.Setenv("GUILD_NO_EMOJI", "")

		origWd, _ := os.Getwd()
		if err := os.Chdir(repo); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chdir(origWd) }()

		cfg, err := Load(nil)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Telemetry.UsageLog {
			t.Error("Telemetry.UsageLog should be false")
		}
		if !cfg.NoUsageLog {
			t.Error("NoUsageLog should be true when Telemetry.UsageLog=false")
		}
	})

	t.Run("env_override_also_disables", func(t *testing.T) {
		home := t.TempDir()
		repo := t.TempDir()

		t.Setenv("HOME", home)
		t.Setenv("GUILD_NO_USAGE_LOG", "1")
		t.Setenv("GUILD_PROJECT", "")
		t.Setenv("GUILD_NO_EMOJI", "")

		origWd, _ := os.Getwd()
		if err := os.Chdir(repo); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chdir(origWd) }()

		cfg, err := Load(nil)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Telemetry.UsageLog {
			t.Error("Telemetry.UsageLog should be false when GUILD_NO_USAGE_LOG=1 (telemetry is opt-in)")
		}
		if !cfg.NoUsageLog {
			t.Error("NoUsageLog should be true when GUILD_NO_USAGE_LOG=1")
		}
	})
}

// TestRepoConfigPathWalksUp verifies that repoConfigPath can find .guild/
// in a parent directory when started from a subdirectory.
func TestRepoConfigPathWalksUp(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}

	// Place config at root/.guild/config.toml.
	writeTOML(t, root, "[scoring]\nw_fts = 0.55\n")

	found, err := repoConfigPath(sub)
	if err != nil {
		t.Fatalf("repoConfigPath: %v", err)
	}
	want := filepath.Join(root, ".guild", "config.toml")
	if found != want {
		t.Errorf("walk-up: got %q want %q", found, want)
	}
}

// TestRepoConfigPathMissingReturnsEmpty verifies that no .guild/ directory
// returns ("", nil) — not an error.
func TestRepoConfigPathMissingReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	path, err := repoConfigPath(root)
	if err != nil {
		t.Errorf("missing .guild: unexpected error: %v", err)
	}
	if path != "" {
		t.Errorf("missing .guild: expected empty path, got %q", path)
	}
}

// ---- unit + integration: [inscribe.valid_days] -----------------------------

// TestDefaultsValidDays pins the built-in per-kind decay windows:
// research=30, decision=180, idea/observation/principle never stale (0).
// These must match the kindValidDays fallback in internal/lore so
// zero-config behavior is byte-identical.
func TestDefaultsValidDays(t *testing.T) {
	d := defaults()
	want := map[string]int{
		"idea":        0,
		"research":    30,
		"decision":    180,
		"observation": 0,
		"principle":   0,
	}
	if len(d.Inscribe.ValidDays) != len(want) {
		t.Fatalf("ValidDays defaults: got %d kinds, want %d (%v)",
			len(d.Inscribe.ValidDays), len(want), d.Inscribe.ValidDays)
	}
	for kind, days := range want {
		if got := d.Inscribe.ValidDays[kind]; got != days {
			t.Errorf("ValidDays[%q]: got %d want %d", kind, got, days)
		}
	}
}

// TestValidDaysKindsMatchLore asserts the local loreKinds list stays in
// sync with the canonical lore.AllKinds(). The list is duplicated here
// (rather than importing internal/lore from production code) to keep
// internal/config a leaf package; this test is the sync enforcement.
func TestValidDaysKindsMatchLore(t *testing.T) {
	canonical := lore.AllKinds()
	if len(canonical) != len(loreKinds) {
		t.Fatalf("loreKinds has %d kinds, lore.AllKinds has %d", len(loreKinds), len(canonical))
	}
	d := defaults()
	for _, k := range canonical {
		if !loreKinds[string(k)] {
			t.Errorf("loreKinds missing canonical kind %q", k)
		}
		if _, ok := d.Inscribe.ValidDays[string(k)]; !ok {
			t.Errorf("defaults().Inscribe.ValidDays missing canonical kind %q", k)
		}
	}
}

// TestFileLayerValidDaysPartialOverride verifies per-key merge: a file
// that sets only research must not clobber the other kinds' defaults or
// the sibling [inscribe] keys.
func TestFileLayerValidDaysPartialOverride(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	content := "[inscribe.valid_days]\nresearch = 7\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	if err := fileLayer(p, &cfg); err != nil {
		t.Fatalf("fileLayer: %v", err)
	}
	if got := cfg.Inscribe.ValidDays["research"]; got != 7 {
		t.Errorf("research: got %d want 7", got)
	}
	if got := cfg.Inscribe.ValidDays["decision"]; got != 180 {
		t.Errorf("decision should keep default 180, got %d", got)
	}
	if got := cfg.Inscribe.ValidDays["idea"]; got != 0 {
		t.Errorf("idea should keep default 0, got %d", got)
	}
	if cfg.Inscribe.PrincipleMaxWords != 60 {
		t.Errorf("principle_max_words should keep default 60, got %d", cfg.Inscribe.PrincipleMaxWords)
	}
}

// TestFileLayerValidDaysZeroMeansNeverStale verifies an explicit 0 is a
// valid override (never stale), distinct from "key absent".
func TestFileLayerValidDaysZeroMeansNeverStale(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	content := "[inscribe.valid_days]\ndecision = 0\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	if err := fileLayer(p, &cfg); err != nil {
		t.Fatalf("fileLayer: %v", err)
	}
	if got := cfg.Inscribe.ValidDays["decision"]; got != 0 {
		t.Errorf("decision: got %d want 0 (never stale)", got)
	}
}

// TestFileLayerValidDaysUnknownKind verifies an unrecognized kind key
// fails the load with an error naming the bad key.
func TestFileLayerValidDaysUnknownKind(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	content := "[inscribe.valid_days]\nreasearch = 7\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	err := fileLayer(p, &cfg)
	if err == nil {
		t.Fatal("unknown kind should fail config load")
	}
	if !strings.Contains(err.Error(), `unknown kind "reasearch"`) {
		t.Errorf("error should name the bad key, got: %v", err)
	}
}

// TestFileLayerValidDaysNegative verifies a negative window fails the
// load with an error naming the bad key and value.
func TestFileLayerValidDaysNegative(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	content := "[inscribe.valid_days]\nresearch = -1\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	err := fileLayer(p, &cfg)
	if err == nil {
		t.Fatal("negative valid_days should fail config load")
	}
	if !strings.Contains(err.Error(), "research = -1") {
		t.Errorf("error should name the bad key/value, got: %v", err)
	}
}

// TestLoadValidDaysLayerPrecedence is the layer-precedence spec for the
// valid_days knob: per-project config beats user-wide config per key,
// while keys set only in the user-wide file survive, and untouched
// kinds stay at built-in defaults.
func TestLoadValidDaysLayerPrecedence(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()

	// Layer 2: user-wide config sets research=10 and decision=200.
	userGuildDir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(userGuildDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userGuildDir, "config.toml"),
		[]byte("[inscribe.valid_days]\nresearch = 10\ndecision = 200\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Layer 3: per-project config overrides only research.
	writeTOML(t, repo, "[inscribe.valid_days]\nresearch = 3\n")

	t.Setenv("HOME", home)
	t.Setenv("GUILD_PROJECT", "")
	t.Setenv("GUILD_NO_USAGE_LOG", "")
	t.Setenv("GUILD_NO_EMOJI", "")

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWd) }()

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Inscribe.ValidDays["research"]; got != 3 {
		t.Errorf("research (repo=3 beats user=10): got %d", got)
	}
	if got := cfg.Inscribe.ValidDays["decision"]; got != 200 {
		t.Errorf("decision (user=200 survives repo layer): got %d", got)
	}
	if got := cfg.Inscribe.ValidDays["idea"]; got != 0 {
		t.Errorf("idea (untouched default=0): got %d", got)
	}
	if got := cfg.Inscribe.ValidDays["principle"]; got != 0 {
		t.Errorf("principle (untouched default=0): got %d", got)
	}
}

// ---- unit: daemon.autostart ------------------------------------------------

func TestDefaultsDaemonAutostart(t *testing.T) {
	if !defaults().Daemon.Autostart {
		t.Error("daemon.autostart default: got false, want true (autostart is on from day one)")
	}
}

func TestFileLayerDaemonAutostartFalse(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(p, []byte("[daemon]\nautostart = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	if err := fileLayer(p, &cfg); err != nil {
		t.Fatalf("fileLayer: %v", err)
	}
	if cfg.Daemon.Autostart {
		t.Error("[daemon] autostart = false should turn autostart off")
	}
}

func TestFileLayerDaemonAutostartAbsentKeepsLowerLayer(t *testing.T) {
	// A file with no [daemon] table must not clobber the default-true
	// value (per-key merge, same as every other knob).
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(p, []byte("[scoring]\nw_fts = 0.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	if err := fileLayer(p, &cfg); err != nil {
		t.Fatalf("fileLayer: %v", err)
	}
	if !cfg.Daemon.Autostart {
		t.Error("autostart should remain default-true when the file omits [daemon]")
	}
}

func TestEnvLayerGUILD_NO_DAEMON(t *testing.T) {
	t.Setenv("GUILD_NO_DAEMON", "1")
	cfg := defaults()
	envLayer(&cfg)
	if cfg.Daemon.Autostart {
		t.Error("GUILD_NO_DAEMON=1: autostart should be false")
	}
}

func TestEnvLayerGUILD_NO_DAEMON_Empty(t *testing.T) {
	t.Setenv("GUILD_NO_DAEMON", "")
	cfg := defaults()
	envLayer(&cfg)
	if !cfg.Daemon.Autostart {
		t.Error("empty GUILD_NO_DAEMON: autostart should remain default-true")
	}
}

// ---- unit: daemon watch knobs ---------------------------------------------

func TestDefaultsDaemonWatch(t *testing.T) {
	d := defaults().Daemon
	if !d.Watch {
		t.Error("daemon.watch default: got false, want true (watch is on from day one)")
	}
	if d.RenewalCapPerPass != 3 {
		t.Errorf("daemon.renewal_cap_per_pass default: got %d want 3", d.RenewalCapPerPass)
	}
	if d.WatchDebounceMS != 0 {
		t.Errorf("daemon.watch_debounce_ms default: got %d want 0 (watcher built-in default)", d.WatchDebounceMS)
	}
}

func TestFileLayerDaemonWatchKnobs(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	body := "[daemon]\nwatch = false\nrenewal_cap_per_pass = 7\nwatch_debounce_ms = 500\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	if err := fileLayer(p, &cfg); err != nil {
		t.Fatalf("fileLayer: %v", err)
	}
	if cfg.Daemon.Watch {
		t.Error("[daemon] watch = false should turn the watcher off")
	}
	if cfg.Daemon.RenewalCapPerPass != 7 {
		t.Errorf("renewal_cap_per_pass: got %d want 7", cfg.Daemon.RenewalCapPerPass)
	}
	if cfg.Daemon.WatchDebounceMS != 500 {
		t.Errorf("watch_debounce_ms: got %d want 500", cfg.Daemon.WatchDebounceMS)
	}
}

func TestFileLayerDaemonWatchPartialKeepsLowerLayer(t *testing.T) {
	// Only watch declared: the cap and debounce must keep their defaults
	// (per-key merge, same as every other knob).
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(p, []byte("[daemon]\nwatch = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	if err := fileLayer(p, &cfg); err != nil {
		t.Fatalf("fileLayer: %v", err)
	}
	if cfg.Daemon.Watch {
		t.Error("[daemon] watch = false should turn the watcher off")
	}
	if cfg.Daemon.RenewalCapPerPass != 3 {
		t.Errorf("renewal_cap_per_pass should remain default 3, got %d", cfg.Daemon.RenewalCapPerPass)
	}
	if cfg.Daemon.Autostart != true {
		t.Error("autostart should remain default-true when the file omits it")
	}
}

func TestEnvLayerGUILD_NO_WATCH(t *testing.T) {
	t.Setenv("GUILD_NO_WATCH", "1")
	cfg := defaults()
	envLayer(&cfg)
	if cfg.Daemon.Watch {
		t.Error("GUILD_NO_WATCH=1: daemon.watch should be false")
	}
}

func TestEnvLayerGUILD_NO_WATCH_Empty(t *testing.T) {
	t.Setenv("GUILD_NO_WATCH", "")
	cfg := defaults()
	envLayer(&cfg)
	if !cfg.Daemon.Watch {
		t.Error("empty GUILD_NO_WATCH: daemon.watch should remain default-true")
	}
}

func TestFlagLayerNoDaemonFlag(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.Bool("no-daemon", false, "disable daemon autostart")
	if err := fs.Parse([]string{"--no-daemon"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	flagLayer(fs, &cfg)
	if cfg.Daemon.Autostart {
		t.Error("--no-daemon: autostart should be false")
	}
}

func TestFlagLayerNoDaemonFlagUnsetKeepsLowerLayer(t *testing.T) {
	// The flag is registered but not passed: it must not turn off an
	// autostart the lower layers left on.
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.Bool("no-daemon", false, "disable daemon autostart")
	if err := fs.Parse([]string{}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	flagLayer(fs, &cfg)
	if !cfg.Daemon.Autostart {
		t.Error("unset --no-daemon must leave autostart at the lower-layer value (true)")
	}
}

// TestLoadDaemonAutostartLayerPrecedence walks autostart through every
// layer: default-true, a user file flips it off, a repo file flips it
// back on, and finally GUILD_NO_DAEMON wins as the highest opt-out.
func TestLoadDaemonAutostartLayerPrecedence(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()

	userGuildDir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(userGuildDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Layer 2: user file turns autostart OFF.
	if err := os.WriteFile(filepath.Join(userGuildDir, "config.toml"),
		[]byte("[daemon]\nautostart = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Layer 3: repo file turns it back ON (beats the user layer).
	writeTOML(t, repo, "[daemon]\nautostart = true\n")

	t.Setenv("HOME", home)
	t.Setenv("GUILD_PROJECT", "")
	t.Setenv("GUILD_NO_USAGE_LOG", "")
	t.Setenv("GUILD_NO_EMOJI", "")
	t.Setenv("GUILD_NO_DAEMON", "")

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWd) }()

	// Without the env opt-out, the repo layer (true) wins over user (false).
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Daemon.Autostart {
		t.Error("repo autostart=true should beat user autostart=false")
	}

	// Env opt-out is the highest layer reachable with nil flags: it
	// forces autostart off regardless of either file.
	t.Setenv("GUILD_NO_DAEMON", "1")
	cfg, err = Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Daemon.Autostart {
		t.Error("GUILD_NO_DAEMON=1 must force autostart off over any file layer")
	}
}

// ---- unit: sleep ----------------------------------------------------------

func TestDefaultsSleep(t *testing.T) {
	d := defaults()
	if !d.Sleep.Enabled {
		t.Error("sleep.enabled default: got false, want true (on with the daemon)")
	}
	if d.Sleep.IdleMinutes != 10 {
		t.Errorf("sleep.idle_minutes default: got %d want 10", d.Sleep.IdleMinutes)
	}
	if d.Sleep.PassBudgetSeconds != 60 {
		t.Errorf("sleep.pass_budget_seconds default: got %d want 60", d.Sleep.PassBudgetSeconds)
	}
}

func TestFileLayerSleepAllKeys(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	content := "[sleep]\nenabled = false\nidle_minutes = 30\npass_budget_seconds = 120\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	if err := fileLayer(p, &cfg); err != nil {
		t.Fatalf("fileLayer: %v", err)
	}
	if cfg.Sleep.Enabled {
		t.Error("[sleep] enabled = false should turn sleep off")
	}
	if cfg.Sleep.IdleMinutes != 30 {
		t.Errorf("idle_minutes: got %d want 30", cfg.Sleep.IdleMinutes)
	}
	if cfg.Sleep.PassBudgetSeconds != 120 {
		t.Errorf("pass_budget_seconds: got %d want 120", cfg.Sleep.PassBudgetSeconds)
	}
}

func TestFileLayerSleepPartialKeepsLowerLayer(t *testing.T) {
	// Only idle_minutes declared: enabled and pass_budget_seconds must
	// keep their default values (per-key merge).
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(p, []byte("[sleep]\nidle_minutes = 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	if err := fileLayer(p, &cfg); err != nil {
		t.Fatalf("fileLayer: %v", err)
	}
	if cfg.Sleep.IdleMinutes != 5 {
		t.Errorf("idle_minutes: got %d want 5", cfg.Sleep.IdleMinutes)
	}
	if !cfg.Sleep.Enabled {
		t.Error("enabled should remain default-true when the file omits it")
	}
	if cfg.Sleep.PassBudgetSeconds != 60 {
		t.Errorf("pass_budget_seconds should remain default 60, got %d", cfg.Sleep.PassBudgetSeconds)
	}
}

func TestFileLayerSleepAbsentKeepsDefaults(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(p, []byte("[scoring]\nw_fts = 0.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	if err := fileLayer(p, &cfg); err != nil {
		t.Fatalf("fileLayer: %v", err)
	}
	if !cfg.Sleep.Enabled || cfg.Sleep.IdleMinutes != 10 || cfg.Sleep.PassBudgetSeconds != 60 {
		t.Errorf("a file without [sleep] must keep sleep defaults, got %+v", cfg.Sleep)
	}
}

func TestEnvLayerGUILD_NO_SLEEP(t *testing.T) {
	t.Setenv("GUILD_NO_SLEEP", "1")
	cfg := defaults()
	envLayer(&cfg)
	if cfg.Sleep.Enabled {
		t.Error("GUILD_NO_SLEEP=1: sleep.enabled should be false")
	}
}

func TestEnvLayerGUILD_NO_SLEEP_Empty(t *testing.T) {
	t.Setenv("GUILD_NO_SLEEP", "")
	cfg := defaults()
	envLayer(&cfg)
	if !cfg.Sleep.Enabled {
		t.Error("empty GUILD_NO_SLEEP: sleep.enabled should remain default-true")
	}
}

// TestLoadSleepEnabledLayerPrecedence walks sleep.enabled through the
// layers: default-true, a user file flips it off, a repo file flips it
// back on, and finally GUILD_NO_SLEEP wins as the highest opt-out.
func TestLoadSleepEnabledLayerPrecedence(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()

	userGuildDir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(userGuildDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userGuildDir, "config.toml"),
		[]byte("[sleep]\nenabled = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTOML(t, repo, "[sleep]\nenabled = true\n")

	t.Setenv("HOME", home)
	t.Setenv("GUILD_PROJECT", "")
	t.Setenv("GUILD_NO_USAGE_LOG", "")
	t.Setenv("GUILD_NO_EMOJI", "")
	t.Setenv("GUILD_NO_SLEEP", "")

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWd) }()

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Sleep.Enabled {
		t.Error("repo enabled=true should beat user enabled=false")
	}

	t.Setenv("GUILD_NO_SLEEP", "1")
	cfg, err = Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Sleep.Enabled {
		t.Error("GUILD_NO_SLEEP=1 must force sleep off over any file layer")
	}
}

// ---- unit: daemon lease heartbeat knobs ------------------------------------

func TestDefaultsDaemonLeaseHeartbeat(t *testing.T) {
	d := defaults().Daemon
	if d.LeaseTTLSeconds != 600 {
		t.Errorf("daemon.lease_ttl default: got %d want 600 (ten minutes)", d.LeaseTTLSeconds)
	}
	if d.HeartbeatIntervalSeconds != 30 {
		t.Errorf("daemon.heartbeat_interval default: got %d want 30", d.HeartbeatIntervalSeconds)
	}
	if d.ReapIntervalSeconds != 60 {
		t.Errorf("daemon.reap_interval default: got %d want 60", d.ReapIntervalSeconds)
	}
}

func TestFileLayerDaemonLeaseHeartbeatKnobs(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	body := "[daemon]\nlease_ttl = 1200\nheartbeat_interval = 45\nreap_interval = 90\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	if err := fileLayer(p, &cfg); err != nil {
		t.Fatalf("fileLayer: %v", err)
	}
	if cfg.Daemon.LeaseTTLSeconds != 1200 {
		t.Errorf("lease_ttl: got %d want 1200", cfg.Daemon.LeaseTTLSeconds)
	}
	if cfg.Daemon.HeartbeatIntervalSeconds != 45 {
		t.Errorf("heartbeat_interval: got %d want 45", cfg.Daemon.HeartbeatIntervalSeconds)
	}
	if cfg.Daemon.ReapIntervalSeconds != 90 {
		t.Errorf("reap_interval: got %d want 90", cfg.Daemon.ReapIntervalSeconds)
	}
}

func TestFileLayerDaemonLeasePartialKeepsLowerLayer(t *testing.T) {
	// Only lease_ttl declared: heartbeat_interval keeps its default
	// (per-key merge, same as every other knob).
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(p, []byte("[daemon]\nlease_ttl = 900\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	if err := fileLayer(p, &cfg); err != nil {
		t.Fatalf("fileLayer: %v", err)
	}
	if cfg.Daemon.LeaseTTLSeconds != 900 {
		t.Errorf("lease_ttl: got %d want 900", cfg.Daemon.LeaseTTLSeconds)
	}
	if cfg.Daemon.HeartbeatIntervalSeconds != 30 {
		t.Errorf("heartbeat_interval should remain default 30, got %d", cfg.Daemon.HeartbeatIntervalSeconds)
	}
}

// TestLoadDaemonLeaseInvalidFallsBackToDefault verifies that a
// non-positive lease TTL or heartbeat interval in config silently falls
// back to the built-in default instead of disarming the liveness layer.
func TestLoadDaemonLeaseInvalidFallsBackToDefault(t *testing.T) {
	home := t.TempDir()
	userGuildDir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(userGuildDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userGuildDir, "config.toml"),
		[]byte("[daemon]\nlease_ttl = 0\nheartbeat_interval = -5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GUILD_PROJECT", "")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	base := defaults()
	if cfg.Daemon.LeaseTTLSeconds != base.Daemon.LeaseTTLSeconds {
		t.Errorf("lease_ttl = 0 should fall back to default %d, got %d",
			base.Daemon.LeaseTTLSeconds, cfg.Daemon.LeaseTTLSeconds)
	}
	if cfg.Daemon.HeartbeatIntervalSeconds != base.Daemon.HeartbeatIntervalSeconds {
		t.Errorf("heartbeat_interval = -5 should fall back to default %d, got %d",
			base.Daemon.HeartbeatIntervalSeconds, cfg.Daemon.HeartbeatIntervalSeconds)
	}
	if cfg.Daemon.ReapIntervalSeconds != base.Daemon.ReapIntervalSeconds {
		t.Errorf("reap_interval unset should keep default %d, got %d",
			base.Daemon.ReapIntervalSeconds, cfg.Daemon.ReapIntervalSeconds)
	}
}

// TestLoadDaemonLeaseValidPreserved verifies a valid positive override
// survives the reconciliation pass in Load (the fallback only triggers on
// non-positive values).
func TestLoadDaemonLeaseValidPreserved(t *testing.T) {
	home := t.TempDir()
	userGuildDir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(userGuildDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userGuildDir, "config.toml"),
		[]byte("[daemon]\nlease_ttl = 300\nheartbeat_interval = 15\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GUILD_PROJECT", "")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Daemon.LeaseTTLSeconds != 300 {
		t.Errorf("lease_ttl = 300 should survive, got %d", cfg.Daemon.LeaseTTLSeconds)
	}
	if cfg.Daemon.HeartbeatIntervalSeconds != 15 {
		t.Errorf("heartbeat_interval = 15 should survive, got %d", cfg.Daemon.HeartbeatIntervalSeconds)
	}
}
