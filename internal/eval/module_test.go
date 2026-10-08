package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/config"
	"github.com/mathomhaus/guild/internal/module"
)

// TestModule_Identity pins the module's identity contract: it registers under
// "eval" and ships OFF by default. The default-off bit is load-bearing for
// the byte-identical-default-surface guarantee.
func TestModule_Identity(t *testing.T) {
	m, ok := module.Lookup("eval")
	if !ok {
		t.Fatal("eval module is not registered")
	}
	if m.Name() != "eval" {
		t.Errorf("Name() = %q, want eval", m.Name())
	}
	if m.DefaultEnabled() {
		t.Error("DefaultEnabled() = true, want false (eval is opt-in)")
	}
	// No storage, no daemon loops, no instruction fragment.
	if fsys, db := m.Migrations(); fsys != nil || db != "" {
		t.Errorf("Migrations() = (%v,%q), want (nil,\"\")", fsys, db)
	}
	if svcs := m.Services(); svcs != nil {
		t.Errorf("Services() = %v, want nil", svcs)
	}
	if frag := m.Instructions(); frag != "" {
		t.Errorf("Instructions() = %q, want empty", frag)
	}
	// Exactly one verb, the eval_run spec, bound to both surfaces.
	cmds := m.Commands()
	if len(cmds) != 1 {
		t.Fatalf("Commands() len = %d, want 1", len(cmds))
	}
	if got := cmds[0].WireName(); got != "eval_run" {
		t.Errorf("verb wire name = %q, want eval_run", got)
	}
}

// TestModule_AbsentByDefault is the disabled-surface proof: with a silent
// config (eval's own DefaultEnabled), module.Enabled must NOT include eval, so
// the kernel never binds its verb on any surface. Conversely, an explicit
// [modules].eval=true flips it on.
func TestModule_AbsentByDefault(t *testing.T) {
	// Silent config: nil predicate => every module on its own default. eval
	// defaults false, so it must be absent.
	for _, m := range module.Enabled(nil) {
		if m.Name() == "eval" {
			t.Fatal("eval present in Enabled(nil); it must be absent by default")
		}
	}

	// Explicit enable via the config predicate flips it on.
	cfg := &config.Config{Modules: config.ModulesConfig{"eval": true}}
	pred := config.ModuleEnabled(cfg)
	found := false
	for _, m := range module.Enabled(pred) {
		if m.Name() == "eval" {
			found = true
		}
	}
	if !found {
		t.Fatal("eval absent from Enabled even with [modules].eval=true")
	}
}

// TestRunHandler_DefaultPath runs the verb handler directly (the same entry
// the CLI and MCP adapters call). The default (non-strict) path always exits
// clean and reports the verdicts as data, even though the grid contains a
// deliberate red cell (the caught recency vulnerability). Parity must hold
// against the committed golden.
func TestRunHandler_DefaultPath(t *testing.T) {
	resetConfigForTest()
	ctx := context.Background()
	rep, err := runHandler(ctx, command.Deps{}, RunInput{})
	if err != nil {
		t.Fatalf("default (non-strict) run must not error: %v", err)
	}
	if rep.Grid.Total == 0 {
		t.Error("grid produced no verdicts")
	}
	if !rep.ParityChecked {
		t.Error("parity should be checked on the default path")
	}
	if rep.ParityDrift != "" {
		t.Errorf("unexpected parity drift: %s", rep.ParityDrift)
	}
	if rep.Failed {
		t.Error("default (non-strict) path must never be Failed")
	}
}

// TestRunHandler_StrictFailsOnRed proves strict mode bites: with the default
// MinGreen=0 (all cells must be green) and the corpus's deliberate red cell,
// a strict run returns a non-nil error so a CI gate exits non-zero. This is
// the "exits non-zero on drift" contract for the grid arm.
func TestRunHandler_StrictFailsOnRed(t *testing.T) {
	resetConfigForTest()
	ctx := context.Background()
	rep, err := runHandler(ctx, command.Deps{}, RunInput{Strict: true})
	if err == nil {
		t.Fatal("strict run with a red grid cell must return an error")
	}
	if !rep.Failed {
		t.Error("report.Failed must be true on a strict failure")
	}
}

// TestRunHandler_StrictMinGreenTolerates confirms a MinGreen floor lets strict
// mode pass with a known-red cell as long as enough cells are green. The
// corpus has two green cells; MinGreen=2 accepts the run.
func TestRunHandler_StrictMinGreenTolerates(t *testing.T) {
	resetConfigForTest()
	t.Cleanup(resetConfigForTest)
	ctx := context.Background()
	// Drive the gate through the config store (the same path config.Load
	// feeds), tolerating the one known-red cell.
	setConfigForTest(EvalConfig{Strict: true, MinGreen: 2})
	if _, err := runHandler(ctx, command.Deps{}, RunInput{}); err != nil {
		t.Fatalf("strict run with MinGreen=2 must pass on a 2-green grid: %v", err)
	}
}

// TestRunHandler_SkipParity omits the parity check.
func TestRunHandler_SkipParity(t *testing.T) {
	resetConfigForTest()
	ctx := context.Background()
	rep, err := runHandler(ctx, command.Deps{}, RunInput{SkipParity: true})
	if err != nil {
		t.Fatalf("runHandler: %v", err)
	}
	if rep.ParityChecked {
		t.Error("ParityChecked should be false when SkipParity is set")
	}
	if rep.ParityDrift != "" {
		t.Error("no parity drift should be reported when parity is skipped")
	}
}

// TestConfigMerge_EvalSection drives the registered [eval] merger end to end
// through config.Load, proving the RegisterModuleConfig seam wires the section
// without a field on the core Config struct. It writes a user config under a
// temp HOME and asserts the eval store picked up strict + min_green.
func TestConfigMerge_EvalSection(t *testing.T) {
	resetConfigForTest()
	t.Cleanup(resetConfigForTest)

	tmp := t.TempDir()
	guildDir := filepath.Join(tmp, ".guild")
	if err := os.MkdirAll(guildDir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "[modules]\neval = true\n\n[eval]\nstrict = true\nmin_green = 2\n"
	if err := os.WriteFile(filepath.Join(guildDir, "config.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(nil)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// The [modules].eval toggle landed in the core table.
	if !config.ModuleEnabled(cfg)("eval", false) {
		t.Error("predicate must report eval enabled from [modules].eval=true")
	}
	// The [eval] section landed in the module's own store via the merger,
	// resolvable for the loaded Config pointer.
	got := ConfigFor(cfg)
	if !got.Strict {
		t.Error("[eval].strict=true did not reach the eval config store")
	}
	if got.MinGreen != 2 {
		t.Errorf("[eval].min_green = %d, want 2", got.MinGreen)
	}
}

// TestConfigMerge_BadMinGreen asserts a negative min_green fails the load
// loudly rather than silently defaulting.
func TestConfigMerge_BadMinGreen(t *testing.T) {
	resetConfigForTest()
	t.Cleanup(resetConfigForTest)

	tmp := t.TempDir()
	guildDir := filepath.Join(tmp, ".guild")
	if err := os.MkdirAll(guildDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(guildDir, "config.toml"),
		[]byte("[eval]\nmin_green = -1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(nil); err == nil {
		t.Error("config.Load must fail on a negative [eval].min_green")
	}
}
