package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestRootHelp ensures the root command exposes its sub-command tree via --help.
func TestRootHelp(t *testing.T) {
	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"--help"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("root --help: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"lore", "quest", "mcp"} {
		if !strings.Contains(out, want) {
			t.Errorf("root --help output missing sub-command %q\n%s", want, out)
		}
	}
	for _, want := range []string{
		"Core",
		"Inspection",
		"knowledge lifecycle (inscribe/appraise/study/reforge)",
		"dashboard: last briefing, oath, top bounty, and parallelism",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("root --help output missing grouped help text %q\n%s", want, out)
		}
	}
	for _, unwanted := range []string{
		"three modes",
		"static binary",
		"read/write/decay/supersede",
		"alias of quest bounties",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("root --help output still contains stale phrasing %q\n%s", unwanted, out)
		}
	}
	// QUEST-10: the root help should nudge the user toward the
	// natural next step after installing guild. Check for two
	// action-phrases that should appear in the Long description.
	for _, want := range []string{
		"guild mcp install",
		"guild init",
		"Next step",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("root --help missing next-step hint %q\n%s", want, out)
		}
	}
}

// TestRootFlags_VerboseAndVersion is QUEST-10's regression guard for
// the -v/-V disambiguation: -v must mean --verbose, -V must mean
// --version (and `--version` itself still prints the stamp).
func TestRootFlags_VerboseAndVersion(t *testing.T) {
	// --verbose (long form).
	if f := rootCmd.PersistentFlags().Lookup("verbose"); f == nil {
		t.Errorf("root missing --verbose persistent flag")
	} else if f.Shorthand != "v" {
		t.Errorf("--verbose shorthand = %q; want v", f.Shorthand)
	}
	// --version (long form).
	if f := rootCmd.Flags().Lookup("version"); f == nil {
		t.Errorf("root missing --version flag")
	} else if f.Shorthand != "V" {
		t.Errorf("--version shorthand = %q; want V", f.Shorthand)
	}
}

// TestSubcommandsHelpable verifies every declared placeholder sub-command
// responds to --help without error.
func TestSubcommandsHelpable(t *testing.T) {
	cases := [][]string{
		{"lore", "--help"},
		{"quest", "--help"},
		{"mcp", "--help"},
		{"mcp", "serve", "--help"},
	}

	for _, args := range cases {
		args := args
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			buf := new(bytes.Buffer)
			rootCmd.SetOut(buf)
			rootCmd.SetErr(buf)
			rootCmd.SetArgs(args)
			t.Cleanup(func() { rootCmd.SetArgs(nil) })

			if err := rootCmd.Execute(); err != nil {
				t.Fatalf("%v: %v", args, err)
			}
		})
	}
}
