package cli

// lore_health.go wires the health/archive/restore subcommands of `guild lore`:
//
//	lore inquest [--all-projects]            (alias: audit)
//	lore meld [--threshold N] [--json]       (alias: dedupe)
//	lore commune [--all-projects] [--fix]    (alias: lint)
//	lore catalog DIR [--topic T --kind K]    (alias: migrate)
//	lore archive                             (alias: export)
//	lore restore                             (alias: import)
//
// All commands use the existing helpers from lore_read.go + lore_write.go
// (openLoreDB, resolveProjectID, resolveNoEmoji, withTelemetry, etc.).

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/lore"
)

// ---------------------------------------------------------------------------
// inquest
// ---------------------------------------------------------------------------

func newInquestCmd() *cobra.Command {
	var (
		projectFlag string
		allProjects bool
	)
	cmd := &cobra.Command{
		Use:     "inquest",
		Aliases: []string{"audit"},
		Short:   "audit oath wall for narrative-bloat principles (alias: audit)",
		RunE: withTelemetry("inquest", func() string { return projectFlag }, func(cmd *cobra.Command, _ []string) error {
			ctx := cmdCtx()
			cfg, err := loadCLIConfig(cmd)
			if err != nil {
				return err
			}
			db, err := openLoreDB(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			var projectID string
			if !allProjects {
				projectID, err = resolveProjectID(ctx, db, projectFlag)
				if err != nil {
					return err
				}
			}

			bloatBoundary := cfg.Inscribe.PrincipleMaxWords
			if bloatBoundary <= 0 {
				bloatBoundary = lore.InquestBloatBoundary
			}

			result, err := lore.Inquest(ctx, db, projectID, allProjects, bloatBoundary)
			if err != nil {
				return err
			}

			noEmoji := cfg.NoEmoji
			renderInquest(cmd.OutOrStdout(), result, allProjects, noEmoji)
			return nil
		}),
	}
	cmd.Flags().StringVarP(&projectFlag, "project", "p", "", "project id (defaults to git toplevel)")
	cmd.Flags().BoolVar(&allProjects, "all-projects", false, "scan every registered project")
	return cmd
}

func renderInquest(w io.Writer, result *lore.InquestResult, allProjects, noEmoji bool) {
	banner := prefix("⚖️ ", "[inquest]", noEmoji)
	fmt.Fprintf(w, "%s lore inquest\n\n", banner)

	if result.TotalOaths == 0 {
		fmt.Fprintln(w, "no principles to audit")
		return
	}

	// Header row.
	fmt.Fprintf(w, "%-22s %5s %6s %5s %4s %5s %4s\n",
		"PROJECT", "OATHS", "WORDS", "AVG", "≤30", "31-60", ">60")
	fmt.Fprintln(w, strings.Repeat("-", 60))

	for _, ps := range result.Projects {
		avg := 0.0
		if ps.TotalOaths > 0 {
			avg = float64(ps.TotalWords) / float64(ps.TotalOaths)
		}
		fmt.Fprintf(w, "%-22s %5d %6d %5.0f %4d %5d %4d\n",
			ps.ProjectID, ps.TotalOaths, ps.TotalWords, avg, ps.Short, ps.Medium, ps.Bloat)
	}

	fmt.Fprintln(w, strings.Repeat("-", 60))
	avg := 0.0
	if result.TotalOaths > 0 {
		avg = float64(result.TotalWords) / float64(result.TotalOaths)
	}
	fmt.Fprintf(w, "%-22s %5d %6d %5.0f\n", "TOTAL", result.TotalOaths, result.TotalWords, avg)
	estTokens := float64(result.TotalWords) / 0.75
	fmt.Fprintf(w, "\nestimated tokens at session-start oath load: ~%.0f\n", estTokens)

	if len(result.BloatEntries) > 0 {
		fmt.Fprintf(w, "\nbloat candidates (>%d words) — reclassify with `lore update LORE-N --kind decision`:\n",
			result.BloatBoundary)
		for _, e := range result.BloatEntries {
			projectPrefix := ""
			if allProjects {
				projectPrefix = e.ProjectID + "/"
			}
			title := e.Title
			if len(title) > 70 {
				title = title[:70]
			}
			fmt.Fprintf(w, "  [%3dw]  %sLORE-%d: %s\n", e.WordCount, projectPrefix, e.EntryID, title)
		}
		n := len(result.BloatEntries)
		pct := 0.0
		if result.TotalOaths > 0 {
			pct = float64(n) / float64(result.TotalOaths) * 100
		}
		fmt.Fprintf(w, "\n%d of %d principles (%.0f%%) are >%d words\n",
			n, result.TotalOaths, pct, result.BloatBoundary)
	} else {
		fmt.Fprintf(w, "\n%s no bloat candidates — all principles ≤%d words\n",
			prefix("✅", "OK", noEmoji), result.BloatBoundary)
	}
}

// ---------------------------------------------------------------------------
// meld
// ---------------------------------------------------------------------------

func newMeldCmd() *cobra.Command {
	var (
		projectFlag string
		threshold   float64
		asJSON      bool
	)
	cmd := &cobra.Command{
		Use:     "meld",
		Aliases: []string{"dedupe"},
		Short:   "surface duplicate entries cross-project (alias: dedupe)",
		RunE: withTelemetry("meld", func() string { return projectFlag }, func(cmd *cobra.Command, _ []string) error {
			ctx := cmdCtx()
			cfg, err := loadCLIConfig(cmd)
			if err != nil {
				return err
			}
			db, err := openLoreDB(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			// meld always cross-project by default.
			pairs, err := lore.Meld(ctx, db, threshold, true, "")
			if err != nil {
				return err
			}

			noEmoji := cfg.NoEmoji
			if asJSON {
				renderMeldJSON(cmd.OutOrStdout(), pairs, threshold)
			} else {
				renderMeld(cmd.OutOrStdout(), pairs, threshold, noEmoji)
			}
			return nil
		}),
	}
	cmd.Flags().StringVarP(&projectFlag, "project", "p", "", "project id (informational; meld is always cross-project)")
	cmd.Flags().Float64Var(&threshold, "threshold", 1.0, "Jaccard similarity threshold for near-match detection (0.0–1.0); default 1.0 = exact-only")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON output")
	return cmd
}

func renderMeld(w io.Writer, pairs []lore.MeldPair, threshold float64, noEmoji bool) {
	banner := prefix("🔮", "[meld]", noEmoji)
	fmt.Fprintf(w, "%s lore meld\n\n", banner)

	if len(pairs) == 0 {
		fmt.Fprintf(w, "%s no duplicate pairs found\n", prefix("✅", "OK", noEmoji))
		return
	}

	exact := 0
	near := 0
	drift := 0
	for _, p := range pairs {
		if p.Score == 1.0 {
			exact++
		} else {
			near++
		}
		if p.KindDrift {
			drift++
		}
	}

	header := fmt.Sprintf("%d exact-match pair(s)", exact)
	if threshold < 1.0 && near > 0 {
		header += fmt.Sprintf(", %d near-match pair(s) ≥%.2f", near, threshold)
	}
	fmt.Fprintf(w, "🔨 %s  (kind drift on %d)\n\n", header, drift)

	for _, p := range pairs {
		scoreStr := "EXACT"
		if p.Score < 1.0 {
			scoreStr = fmt.Sprintf("~%.2f", p.Score)
		}
		driftStr := ""
		if p.KindDrift {
			driftStr = "  ⚠️ KIND DRIFT"
		}
		fmt.Fprintf(w, "  [%s]%s\n", scoreStr, driftStr)
		fmt.Fprintf(w, "    L  %s/LORE-%d\n", p.LeftProject, p.LeftID)
		fmt.Fprintf(w, "    R  %s/LORE-%d\n", p.RightProject, p.RightID)
		fmt.Fprintf(w, "    → lore reforge %d --with %d -p %s\n", p.LeftID, p.RightID, p.LeftProject)
		if p.KindDrift {
			fmt.Fprintln(w, "       (pick canonical kind first via lore update before reforging)")
		}
		fmt.Fprintln(w)
	}
}

func renderMeldJSON(w io.Writer, pairs []lore.MeldPair, threshold float64) {
	exact := 0
	near := 0
	drift := 0
	for _, p := range pairs {
		if p.Score == 1.0 {
			exact++
		} else {
			near++
		}
		if p.KindDrift {
			drift++
		}
	}
	out := map[string]interface{}{
		"exact_dup_pairs":  exact,
		"near_dup_pairs":   near,
		"kind_drift_count": drift,
		"threshold":        threshold,
		"pairs":            pairs,
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	fmt.Fprintf(w, "%s\n", data)
}

// ---------------------------------------------------------------------------
// commune
// ---------------------------------------------------------------------------

func newCommuneCmd() *cobra.Command {
	var (
		projectFlag string
		allProjects bool
		fix         bool
	)
	cmd := &cobra.Command{
		Use:     "commune",
		Aliases: []string{"lint"},
		Short:   "composite health check: bloat + dups + recall (alias: lint)",
		RunE: withTelemetry("commune", func() string { return projectFlag }, func(cmd *cobra.Command, _ []string) error {
			ctx := cmdCtx()
			cfg, err := loadCLIConfig(cmd)
			if err != nil {
				return err
			}
			db, err := openLoreDB(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			var projectID string
			if !allProjects {
				projectID, err = resolveProjectID(ctx, db, projectFlag)
				if err != nil {
					return err
				}
			}

			bloatBoundary := cfg.Inscribe.PrincipleMaxWords
			if bloatBoundary <= 0 {
				bloatBoundary = lore.InquestBloatBoundary
			}
			severeBoundary := cfg.Inscribe.BloatSevereThreshold
			if severeBoundary <= 0 {
				severeBoundary = 120
			}

			report, err := lore.Commune(ctx, db, projectID, allProjects, fix, bloatBoundary, severeBoundary)
			if err != nil {
				return err
			}

			noEmoji := cfg.NoEmoji
			renderCommune(cmd.OutOrStdout(), report, allProjects, bloatBoundary, fix, noEmoji)
			return nil
		}),
	}
	cmd.Flags().StringVarP(&projectFlag, "project", "p", "", "project id (defaults to git toplevel)")
	cmd.Flags().BoolVar(&allProjects, "all-projects", false, "include all projects in health check")
	cmd.Flags().BoolVar(&fix, "fix", false, "auto-apply safe remediations (demote severe bloat, reforge exact dups)")
	return cmd
}

func renderCommune(w io.Writer, report *lore.CommuneReport, allProjects bool, bloatBoundary int, fix, noEmoji bool) {
	scopeLabel := "(current project)"
	if allProjects {
		scopeLabel = "(--all-projects)"
	}
	fmt.Fprintf(w, "%s lore commune %s\n\n", prefix("🌀", "[commune]", noEmoji), scopeLabel)

	// Bloat check.
	bloatIcon := "✅"
	if report.BloatCount > 0 {
		if report.BloatCount <= 3 {
			bloatIcon = "⚠️ "
		} else {
			bloatIcon = "❌"
		}
	}
	severeNote := ""
	if report.SevereCount > 0 {
		severeNote = fmt.Sprintf("  (%d severe >120w — --fix candidates)", report.SevereCount)
	}
	fmt.Fprintf(w, "%s  Oath bloat: %d/%d principles >%d words%s\n",
		bloatIcon, report.BloatCount, report.TotalPrinciples, bloatBoundary, severeNote)

	// Dedup check.
	dupIcon := "✅"
	if report.DupPairCount > 0 {
		if report.DupPairCount <= 5 {
			dupIcon = "⚠️ "
		} else {
			dupIcon = "❌"
		}
	}
	driftNote := ""
	if report.DriftCount > 0 {
		driftNote = fmt.Sprintf("  (%d with kind drift)", report.DriftCount)
	}
	fmt.Fprintf(w, "%s  Duplicates: %d exact-match pair(s) across projects%s\n",
		dupIcon, report.DupPairCount, driftNote)

	// Recall check.
	if report.RecallSkipped {
		fmt.Fprintln(w, "·   Recall@1 sanity: skipped (no entries with access_count > 0 yet — usage.log will populate this over time)")
	} else {
		good := report.RecallSampleSize - report.RecallMisses
		recallIcon := "✅"
		if report.RecallMisses > 0 {
			if report.RecallMisses <= 2 {
				recallIcon = "⚠️ "
			} else {
				recallIcon = "❌"
			}
		}
		fmt.Fprintf(w, "%s  Recall@1 sanity: %d/%d top-accessed entries returned at #1 for their own title\n",
			recallIcon, good, report.RecallSampleSize)
	}

	sep := strings.Repeat("═", 60)

	// Detail: bloat candidates.
	if len(report.BloatEntries) > 0 {
		fmt.Fprintf(w, "\n%s\n", sep)
		fmt.Fprintln(w, "BLOAT CANDIDATES (run lore inquest for full list):")
		limit := len(report.BloatEntries)
		if limit > 5 {
			limit = 5
		}
		for _, e := range report.BloatEntries[:limit] {
			title := e.Title
			if len(title) > 60 {
				title = title[:60]
			}
			fmt.Fprintf(w, "  [%3dw]  %s/LORE-%d: %s\n", e.WordCount, e.ProjectID, e.EntryID, title)
		}
		if len(report.BloatEntries) > 5 {
			fmt.Fprintf(w, "  ... +%d more — see `lore inquest --all-projects`\n", len(report.BloatEntries)-5)
		}
	}

	// Detail: dup pairs.
	if len(report.DupPairs) > 0 {
		fmt.Fprintf(w, "\n%s\n", sep)
		fmt.Fprintln(w, "DUPLICATE PAIRS (run lore meld for full list + reforge commands):")
		limit := len(report.DupPairs)
		if limit > 5 {
			limit = 5
		}
		for _, p := range report.DupPairs[:limit] {
			driftNote := ""
			if p.KindDrift {
				driftNote = "  ⚠️ KIND DRIFT"
			}
			fmt.Fprintf(w, "  %s/LORE-%d ↔ %s/LORE-%d%s\n",
				p.LeftProject, p.LeftID, p.RightProject, p.RightID, driftNote)
		}
		if len(report.DupPairs) > 5 {
			fmt.Fprintf(w, "  ... +%d more — see `lore meld`\n", len(report.DupPairs)-5)
		}
	}

	// Auto-fix summary.
	if fix && len(report.FixesApplied) > 0 {
		fmt.Fprintf(w, "\n%s\n", sep)
		fmt.Fprintln(w, "AUTO-FIX")
		reclassify := 0
		reforged := 0
		for _, f := range report.FixesApplied {
			switch f.Kind {
			case "reclassify":
				reclassify++
				fmt.Fprintf(w, "  reclassified LORE-%d principle → decision: %s\n", f.EntryID, f.Detail)
			case "reforge":
				reforged++
				fmt.Fprintf(w, "  %s\n", f.Detail)
			}
		}
		fmt.Fprintf(w, "\n%s applied %d reclassifications, %d reforges\n",
			prefix("✓", "OK", noEmoji), reclassify, reforged)
		if report.RecallMisses > 0 {
			fmt.Fprintln(w, "  (recall@1 misses NOT auto-fixed — scoring tuning problem, not data problem)")
		}
	}
}

// ---------------------------------------------------------------------------
// catalog
// ---------------------------------------------------------------------------

// newCatalogCmd migrated to internal/lore.CatalogCommand (QUEST-45).

// ---------------------------------------------------------------------------
// archive
// ---------------------------------------------------------------------------

func newArchiveCmd() *cobra.Command {
	var (
		projectFlag string
		outputFile  string
	)
	cmd := &cobra.Command{
		Use:     "archive",
		Aliases: []string{"export"},
		Short:   "write snapshot.json for git-trackable project checkpoint (alias: export)",
		RunE: withTelemetry("archive", func() string { return projectFlag }, func(cmd *cobra.Command, _ []string) error {
			ctx := cmdCtx()
			db, err := openLoreDB(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			pid, err := resolveProjectID(ctx, db, projectFlag)
			if err != nil {
				return err
			}

			snapshotPath := outputFile
			if snapshotPath == "" {
				// Default: <repo>/.guild/snapshot.json relative to the project's path.
				var projPath string
				if dbErr := db.QueryRowContext(ctx,
					`SELECT path FROM projects WHERE id = ?`, pid,
				).Scan(&projPath); dbErr != nil {
					return fmt.Errorf("could not resolve project path for %q: %w", pid, dbErr)
				}
				snapshotPath = filepath.Join(projPath, ".guild", "snapshot.json")
			}

			start := time.Now()
			if err := lore.Archive(ctx, db, pid, snapshotPath); err != nil {
				return err
			}

			noEmoji := resolveNoEmoji(cmd)
			var entryCount int
			_ = db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM entries WHERE project_id = ?`, pid,
			).Scan(&entryCount)

			fmt.Fprintf(cmd.OutOrStdout(), "%s archived %d entries → %s (%.0fms)\n",
				prefix("📸", "[archived]", noEmoji), entryCount, snapshotPath,
				float64(time.Since(start).Milliseconds()))
			return nil
		}),
	}
	cmd.Flags().StringVarP(&projectFlag, "project", "p", "", "project id (defaults to git toplevel)")
	cmd.Flags().StringVarP(&outputFile, "output", "o", "", "output path (default: <repo>/.guild/snapshot.json)")
	return cmd
}

// ---------------------------------------------------------------------------
// restore
// ---------------------------------------------------------------------------

func newRestoreCmd() *cobra.Command {
	var (
		projectFlag string
		inputFile   string
	)
	cmd := &cobra.Command{
		Use:     "restore",
		Aliases: []string{"import"},
		Short:   "restore lore from snapshot.json (alias: import)",
		RunE: withTelemetry("restore", func() string { return projectFlag }, func(cmd *cobra.Command, _ []string) error {
			ctx := cmdCtx()
			db, err := openLoreDB(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			pid, err := resolveProjectID(ctx, db, projectFlag)
			if err != nil {
				return err
			}

			snapshotPath := inputFile
			if snapshotPath == "" {
				// Default: <repo>/.guild/snapshot.json.
				var projPath string
				if dbErr := db.QueryRowContext(ctx,
					`SELECT path FROM projects WHERE id = ?`, pid,
				).Scan(&projPath); dbErr != nil {
					return fmt.Errorf("could not resolve project path for %q: %w", pid, dbErr)
				}
				snapshotPath = filepath.Join(projPath, ".guild", "snapshot.json")
			}

			result, err := lore.Restore(ctx, db, pid, snapshotPath)
			if err != nil {
				return err
			}

			noEmoji := resolveNoEmoji(cmd)
			fmt.Fprintf(cmd.OutOrStdout(), "%s restored %d entries, %d links (%d already existed)\n",
				prefix("📥", "[restored]", noEmoji), result.Imported, result.LinksAdded, result.Skipped)
			return nil
		}),
	}
	cmd.Flags().StringVarP(&projectFlag, "project", "p", "", "project id (defaults to git toplevel)")
	cmd.Flags().StringVarP(&inputFile, "file", "f", "", "snapshot path (default: <repo>/.guild/snapshot.json)")
	return cmd
}

// ---------------------------------------------------------------------------
// init() — register commands under the existing loreCmd
// ---------------------------------------------------------------------------

func init() {
	loreCmd.AddCommand(
		newArchiveCmd(),
		newRestoreCmd(),
	)
	// inquest/meld/commune/catalog and the embedder-health verbs are
	// registry-generated and bound by the module-registry loop
	// (bindModuleVerbs in modules.go).
}
