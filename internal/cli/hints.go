package cli

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/guildpath"
	"github.com/mathomhaus/guild/internal/hints"
	"github.com/mathomhaus/guild/internal/storage"
)

// hintsCmd is the parent for `guild hints ...` operations. The engine
// fires hints automatically during MCP/CLI tool calls; this subcommand
// lets operators inspect the rule table, read per-rule stats, and
// manually enable/disable/prune rules.
var hintsCmd = &cobra.Command{
	Use:   "hints",
	Short: "hint engine inspection + administration",
	Long: `Inspect and administer the SQL-backed hint engine (QUEST-58).

The engine fires advisory lines on top of tool responses. The launch-9
rules were calibrated in ENTRY-29; later additions (e.g. the thin-citation
rule from QUEST-167) ship seeded via migrations alongside the rest. Use
these subcommands to see rule state, compute per-rule hit rates, prune
under-performing rules, or manually override enable/disable.`,
	RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
}

var hintsListCmd = &cobra.Command{
	Use:   "list",
	Short: "list all registered hint rules with severity and enabled state",
	RunE:  runHintsList,
}

var hintsStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "per-rule fire count, scored count, hit rate, and prune-floor comparison",
	RunE:  runHintsStats,
}

var hintsPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "run the auto-prune pass (demote/disable rules below 14.46% hit rate)",
	RunE:  runHintsPrune,
}

var hintsEnableCmd = &cobra.Command{
	Use:   "enable RULE_ID",
	Short: "re-enable a previously disabled rule",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runHintsSetEnabled(cmd, args[0], true)
	},
}

var hintsDisableCmd = &cobra.Command{
	Use:   "disable RULE_ID",
	Short: "disable a rule",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runHintsSetEnabled(cmd, args[0], false)
	},
}

func init() {
	hintsCmd.AddCommand(hintsListCmd, hintsStatsCmd, hintsPruneCmd, hintsEnableCmd, hintsDisableCmd)
	rootCmd.AddCommand(hintsCmd)
}

// openHintsDB opens ~/.guild/quest.db with migrations applied. Shared by
// every hints subcommand.
func openHintsDB(ctx context.Context) (*sql.DB, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home: %w", err)
	}
	path := filepath.Join(home, ".guild", "quest.db")
	if dir := filepath.Dir(path); dir != "" {
		_ = guildpath.EnsureDir(dir)
	}
	db, err := storage.Open(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := guildpath.TightenDBPerms(path); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cli: %w", err)
	}
	if err := storage.Migrate(ctx, db, "quest"); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func runHintsList(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	db, err := openHintsDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	store := hints.NewStore(db)
	rules, err := store.LoadRules(ctx)
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "📋 %d rule(s):\n", len(rules))
	ids := sortedRuleIDs(rules)
	for _, id := range ids {
		r := rules[id]
		enabled := "enabled"
		if !r.Enabled {
			enabled = "disabled"
		}
		fmt.Fprintf(w, "  %-32s  %-8s  %-8s  %s\n",
			id, r.Severity.Emoji()+" "+r.Severity.String(), enabled, r.Template)
	}
	return nil
}

func runHintsStats(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	db, err := openHintsDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	stats, err := hints.NewStore(db).StatsAll(ctx)
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "📊 hint-engine stats (auto-prune floor = %.4f, min scored = %d)\n",
		hints.AutoPruneFloor, hints.MinScoredBeforePrune)
	fmt.Fprintf(w, "  %-32s  %-6s  %-6s  %-6s  %-8s  %s\n",
		"rule_id", "fires", "scored", "hits", "hit_rate", "state")
	for _, s := range stats {
		state := fmt.Sprintf("%s/%s", s.Severity.String(), stateLabel(s.Enabled))
		flag := ""
		if s.Enabled && s.Scored >= hints.MinScoredBeforePrune && s.HitRate() < hints.AutoPruneFloor {
			flag = " ← below floor"
		}
		fmt.Fprintf(w, "  %-32s  %-6d  %-6d  %-6d  %-8.4f  %s%s\n",
			s.RuleID, s.Fires, s.Scored, s.Hits, s.HitRate(), state, flag)
	}
	return nil
}

func runHintsPrune(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	db, err := openHintsDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	sessionID := strconv.Itoa(os.Getpid())
	eng := hints.NewEngine(hints.NewStore(db), sessionID, hints.EraBash)
	if err := eng.LoadRules(ctx); err != nil {
		return err
	}
	actions, err := eng.Prune(ctx)
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if len(actions) == 0 {
		fmt.Fprintln(w, "✂️  no prune actions — every rule is at or above the hit-rate floor")
		return nil
	}
	fmt.Fprintf(w, "✂️  %d prune action(s):\n", len(actions))
	for _, a := range actions {
		fmt.Fprintf(w, "  - %s\n", a.String())
	}
	return nil
}

func runHintsSetEnabled(cmd *cobra.Command, ruleID string, enabled bool) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	db, err := openHintsDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	store := hints.NewStore(db)
	if err := store.SetEnabled(ctx, ruleID, enabled); err != nil {
		return err
	}
	verb := "enabled"
	if !enabled {
		verb = "disabled"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✓ %s %s\n", ruleID, verb)
	return nil
}

// sortedRuleIDs returns the rule ids in lexical order so CLI output is
// deterministic.
func sortedRuleIDs(rules map[string]hints.RuleRow) []string {
	ids := make([]string, 0, len(rules))
	for id := range rules {
		ids = append(ids, id)
	}
	// Tiny n — bubble sort by lex order keeps this allocation-free.
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			if ids[j] < ids[i] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
	return ids
}

func stateLabel(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}
