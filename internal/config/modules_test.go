package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
)

// ---- [modules] toggle table (fileLayer) ------------------------------------

func TestFileLayerModulesTable(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	content := `[modules]
lore = false
quest = true
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	if err := fileLayer(p, &cfg); err != nil {
		t.Fatalf("fileLayer: %v", err)
	}
	if on, ok := cfg.Modules["lore"]; !ok || on {
		t.Errorf("modules.lore: got (%v,%v) want (false,true)", on, ok)
	}
	if on, ok := cfg.Modules["quest"]; !ok || !on {
		t.Errorf("modules.quest: got (%v,%v) want (true,true)", on, ok)
	}
	// A module absent from the table must NOT appear (predicate falls back
	// to DefaultEnabled for it).
	if _, ok := cfg.Modules["session"]; ok {
		t.Error("modules.session: should be absent (not declared in file)")
	}
}

func TestFileLayerModulesPartialMergePreservesLowerLayer(t *testing.T) {
	// Lower layer sets lore=false; this file declares only quest=false.
	// lore must stay false (per-key merge), quest becomes false.
	tmp := t.TempDir()
	p := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(p, []byte("[modules]\nquest = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	cfg.Modules = ModulesConfig{"lore": false}
	if err := fileLayer(p, &cfg); err != nil {
		t.Fatalf("fileLayer: %v", err)
	}
	if on := cfg.Modules["lore"]; on {
		t.Error("modules.lore from lower layer should be preserved as false")
	}
	if on := cfg.Modules["quest"]; on {
		t.Error("modules.quest should be false from this file")
	}
}

// ---- GUILD_MODULE_* / GUILD_NO_* env overrides -----------------------------

func TestEnvModuleOverride_GUILD_MODULE_off(t *testing.T) {
	t.Setenv("GUILD_MODULE_LORE", "0")
	cfg := defaults()
	envLayer(&cfg)
	if on, ok := cfg.Modules["lore"]; !ok || on {
		t.Errorf("GUILD_MODULE_LORE=0: got (%v,%v) want (false,true)", on, ok)
	}
}

func TestEnvModuleOverride_GUILD_MODULE_on(t *testing.T) {
	t.Setenv("GUILD_MODULE_COMPRESSION", "1")
	cfg := defaults()
	envLayer(&cfg)
	if on, ok := cfg.Modules["compression"]; !ok || !on {
		t.Errorf("GUILD_MODULE_COMPRESSION=1: got (%v,%v) want (true,true)", on, ok)
	}
}

func TestEnvModuleOverride_GUILD_NO_disables(t *testing.T) {
	t.Setenv("GUILD_NO_LORE", "1")
	cfg := defaults()
	envLayer(&cfg)
	if on, ok := cfg.Modules["lore"]; !ok || on {
		t.Errorf("GUILD_NO_LORE=1: got (%v,%v) want (false,true)", on, ok)
	}
}

func TestEnvModuleOverride_GUILD_NO_winsOverGUILD_MODULE(t *testing.T) {
	t.Setenv("GUILD_MODULE_LORE", "1")
	t.Setenv("GUILD_NO_LORE", "1")
	cfg := defaults()
	envLayer(&cfg)
	if on := cfg.Modules["lore"]; on {
		t.Error("GUILD_NO_LORE must win over GUILD_MODULE_LORE: lore should be off")
	}
}

func TestEnvModuleOverride_ReservedNotTreatedAsModule(t *testing.T) {
	// GUILD_NO_DAEMON / GUILD_NO_EMOJI etc. are NOT module toggles and must
	// not invent phantom keys in the Modules table.
	t.Setenv("GUILD_NO_DAEMON", "1")
	t.Setenv("GUILD_NO_EMOJI", "1")
	t.Setenv("GUILD_NO_USAGE_LOG", "1")
	cfg := defaults()
	envLayer(&cfg)
	for _, k := range []string{"daemon", "emoji", "usage_log"} {
		if _, ok := cfg.Modules[k]; ok {
			t.Errorf("reserved GUILD_NO_%s leaked into Modules table as %q", k, k)
		}
	}
}

// ---- --module / --disable-module flag overrides ----------------------------

func moduleFlagSet(t *testing.T) *pflag.FlagSet {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.StringArray("module", nil, "")
	fs.StringArray("disable-module", nil, "")
	return fs
}

func TestFlagModuleOverride_explicitToggle(t *testing.T) {
	fs := moduleFlagSet(t)
	if err := fs.Parse([]string{"--module", "lore=false", "--module", "compression=true"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	flagLayer(fs, &cfg)
	if on := cfg.Modules["lore"]; on {
		t.Error("--module lore=false: lore should be off")
	}
	if on, ok := cfg.Modules["compression"]; !ok || !on {
		t.Errorf("--module compression=true: got (%v,%v) want on", on, ok)
	}
}

func TestFlagModuleOverride_disableModule(t *testing.T) {
	fs := moduleFlagSet(t)
	if err := fs.Parse([]string{"--disable-module", "lore"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	flagLayer(fs, &cfg)
	if on, ok := cfg.Modules["lore"]; !ok || on {
		t.Errorf("--disable-module lore: got (%v,%v) want off", on, ok)
	}
}

func TestFlagModuleOverride_unparseableBoolSkipped(t *testing.T) {
	fs := moduleFlagSet(t)
	if err := fs.Parse([]string{"--module", "lore=maybe"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	flagLayer(fs, &cfg)
	if _, ok := cfg.Modules["lore"]; ok {
		t.Error("--module lore=maybe (bad bool) must be skipped, not applied")
	}
}

// ---- ModuleEnabled predicate ----------------------------------------------

func TestModuleEnabledPredicate(t *testing.T) {
	cfg := &Config{Modules: ModulesConfig{"lore": false, "compression": true}}
	pred := ModuleEnabled(cfg)

	// Explicit off overrides a true default.
	if pred("lore", true) {
		t.Error("lore=false in table must override DefaultEnabled=true")
	}
	// Explicit on overrides a false default.
	if !pred("compression", false) {
		t.Error("compression=true in table must override DefaultEnabled=false")
	}
	// Absent key keeps the module's own default (both directions).
	if !pred("quest", true) {
		t.Error("quest absent: must keep DefaultEnabled=true")
	}
	if pred("eval", false) {
		t.Error("eval absent: must keep DefaultEnabled=false")
	}
}

func TestModuleEnabledPredicate_NilConfig(t *testing.T) {
	pred := ModuleEnabled(nil)
	if !pred("lore", true) {
		t.Error("nil config: default true must pass")
	}
	if pred("compression", false) {
		t.Error("nil config: default false must stay off")
	}
}

// ---- presets ---------------------------------------------------------------

func TestApplyPreset_minimalBaseline(t *testing.T) {
	cfg := defaults()
	if err := applyPreset("minimal", &cfg); err != nil {
		t.Fatalf("applyPreset: %v", err)
	}
	for _, core := range []string{"quest", "lore", "session"} {
		if on, ok := cfg.Modules[core]; !ok || !on {
			t.Errorf("preset minimal: %s should be on, got (%v,%v)", core, on, ok)
		}
	}
}

func TestApplyPreset_explicitKeyWins(t *testing.T) {
	// An explicit [modules].lore=false must survive the minimal preset,
	// which would otherwise turn lore on.
	cfg := defaults()
	cfg.Modules = ModulesConfig{"lore": false}
	if err := applyPreset("minimal", &cfg); err != nil {
		t.Fatalf("applyPreset: %v", err)
	}
	if on := cfg.Modules["lore"]; on {
		t.Error("explicit lore=false must win over the preset baseline")
	}
	// A key the explicit set did not touch still gets the preset value.
	if !cfg.Modules["quest"] {
		t.Error("quest should pick up the preset baseline (on)")
	}
}

func TestApplyPreset_unknownNameErrors(t *testing.T) {
	cfg := defaults()
	if err := applyPreset("bogus", &cfg); err == nil {
		t.Error("applyPreset with unknown name must error (loud typo)")
	}
}

func TestApplyPreset_emptyNameNoop(t *testing.T) {
	cfg := defaults()
	if err := applyPreset("", &cfg); err != nil {
		t.Fatalf("applyPreset empty: %v", err)
	}
	if len(cfg.Modules) != 0 {
		t.Errorf("empty preset must not touch Modules; got %v", cfg.Modules)
	}
}

// ---- Load() end-to-end with [profile] preset -------------------------------

func TestLoad_PresetExpands(t *testing.T) {
	tmp := t.TempDir()
	guildDir := filepath.Join(tmp, ".guild")
	if err := os.MkdirAll(guildDir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := `[profile]
preset = "minimal"

[modules]
lore = false
`
	if err := os.WriteFile(filepath.Join(guildDir, "config.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// Point HOME at the fixture so the user-layer file is the one we wrote,
	// and chdir away so no stray repo .guild interferes.
	t.Setenv("HOME", tmp)
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// preset minimal turns quest/session on; explicit lore=false survives.
	if cfg.Modules["lore"] {
		t.Error("explicit lore=false must survive the preset")
	}
	if !cfg.Modules["quest"] || !cfg.Modules["session"] {
		t.Error("preset minimal should turn quest and session on")
	}
	pred := ModuleEnabled(cfg)
	if pred("lore", true) {
		t.Error("predicate must report lore disabled")
	}
	if !pred("quest", true) {
		t.Error("predicate must report quest enabled")
	}
}

func TestLoad_UnknownPresetFailsLoad(t *testing.T) {
	tmp := t.TempDir()
	guildDir := filepath.Join(tmp, ".guild")
	if err := os.MkdirAll(guildDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(guildDir, "config.toml"),
		[]byte("[profile]\npreset = \"nope\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(nil); err == nil {
		t.Error("Load with unknown preset must fail (loud typo)")
	}
}
