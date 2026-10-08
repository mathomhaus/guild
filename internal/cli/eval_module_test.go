package cli

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/eval"
)

// eval_module_test.go is the CLI-surface half of the ADR-006 Phase 6
// enabled/disabled acceptance. The cli package binds module verbs once at
// init() against the process config (eval off by default), so the
// disabled-surface proof asserts the default state and the enabled proof
// drives the binding helpers directly with a fresh cobra parent (the same
// path bindModuleVerbs takes when the module is on).

// findSub returns the named direct subcommand of parent, or nil.
func findSub(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

// TestEvalCLI_AbsentByDefault asserts that with the default config (eval off),
// the `eval` top-level command is NOT attached to the root tree, so `guild
// --help` is byte-identical to a build without the module. The core groups
// stay present.
func TestEvalCLI_AbsentByDefault(t *testing.T) {
	if sub := findSub(rootCmd, "eval"); sub != nil {
		t.Fatal("eval command attached to root by default; it must appear only when enabled")
	}
	// The evalCmd parent itself must be detached from any tree.
	if evalCmd.Parent() != nil {
		t.Errorf("evalCmd has a parent (%q) by default; it must be unattached",
			evalCmd.Parent().Name())
	}
	// Core groups are present regardless.
	if findSub(rootCmd, "lore") == nil || findSub(rootCmd, "quest") == nil {
		t.Error("core lore/quest groups missing from root")
	}
}

// TestEvalCLI_BindsRunVerb proves the eval module's verb spec generates the
// `eval run` cobra subcommand (the enabled path). It binds onto a fresh parent
// via the same Deps the module loop uses, then asserts the run verb and its
// flags exist. This exercises cliBindTargetForModule + the command adapter
// without mutating the shared rootCmd.
func TestEvalCLI_BindsRunVerb(t *testing.T) {
	deps, parent, ok := cliBindTargetForModule("eval")
	if !ok {
		t.Fatal("cliBindTargetForModule(\"eval\") returned ok=false; eval has no CLI bind target")
	}
	if parent != evalCmd {
		t.Errorf("eval bind target = %v, want evalCmd", parent)
	}

	// Bind onto a throwaway parent so the assertion does not depend on shared
	// init state and cannot collide with a real registration.
	fresh := &cobra.Command{Use: "eval"}
	eval.RunCommand.BindCobra(fresh, deps)

	run := findSub(fresh, "run")
	if run == nil {
		t.Fatal("eval module did not produce a `run` subcommand")
	}
	// cobra dashes the JSON arg names: skip_parity -> --skip-parity.
	for _, flag := range []string{"strict", "skip-parity"} {
		if run.Flags().Lookup(flag) == nil {
			t.Errorf("eval run is missing the --%s flag", flag)
		}
	}
}

// TestEvalCLI_WireName guards the verb's MCP/CLI identity so a rename is a
// deliberate, reviewed change.
func TestEvalCLI_WireName(t *testing.T) {
	if got := eval.RunCommand.WireName(); got != "eval_run" {
		t.Errorf("eval verb wire name = %q, want eval_run", got)
	}
	path := eval.RunCommand.CobraPath()
	if len(path) != 2 || path[0] != "eval" || path[1] != "run" {
		t.Errorf("eval verb cobra path = %v, want [eval run]", path)
	}
}
