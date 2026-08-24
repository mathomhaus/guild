package lore

import (
	"context"
	"fmt"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/lore/embed"
)

// CoverageReconcileInput is the typed input for `guild lore coverage-reconcile`.
type CoverageReconcileInput struct {
	Project string `json:"project,omitempty"`
}

// CoverageReconcileOutput reports the before/after state of vector coverage counters.
type CoverageReconcileOutput struct {
	ProjectID string `json:"project_id"`
	// DenBefore is the meta.vector_coverage_den value before the reconcile.
	DenBefore int64 `json:"den_before"`
	// DenAfter is the meta.vector_coverage_den value after the reconcile
	// (equals the live COUNT(*) of active entries).
	DenAfter int64 `json:"den_after"`
	// NumBefore is the meta.vector_coverage_num value before the reconcile.
	NumBefore int64 `json:"num_before"`
	// NumAfter is the meta.vector_coverage_num value after the reconcile
	// (equals the live COUNT(*) of lore_vectors rows).
	NumAfter int64 `json:"num_after"`
	// Drift is DenAfter - DenBefore (positive = den was too low, negative = too high).
	Drift int64 `json:"drift"`
}

// CoverageReconcileCommand is the registry spec for `guild lore coverage-reconcile`.
// It resets meta.vector_coverage_den and meta.vector_coverage_num to their
// live COUNT(*) values and reports the before/after state so operators can
// verify the fix.
//
// This is the manual escape hatch for QUEST-220 / LORE-373. Backfill also
// calls ReconcileDen automatically, so normal usage should never require this
// command. Surface it as a diagnostic tool.
var CoverageReconcileCommand = &command.Command[CoverageReconcileInput, CoverageReconcileOutput]{
	Name:       "lore_coverage_reconcile",
	CLIPath:    []string{"lore", "coverage-reconcile"},
	CLIAliases: []string{"fix-coverage"},
	Short:      "reset vector coverage counters to live counts",
	Long: "Reset meta.vector_coverage_den and meta.vector_coverage_num to live " +
		"COUNT(*) values (active entries and lore_vectors rows) and report " +
		"before/after values. Corrects num/den drift that can disable the " +
		"appraise semantic arm or produce coverage > 100%. Backfill also runs " +
		"den reconcile automatically, so this command is a manual escape hatch.",
	Args: []command.ArgSpec{
		{Name: "project", Short: "p", Kind: command.ArgFlag, Type: command.ArgString, Help: "project override"},
	},
	Handler: func(ctx context.Context, d command.Deps, in CoverageReconcileInput) (CoverageReconcileOutput, error) {
		pid, err := d.ResolveProj(ctx, in.Project)
		if err != nil {
			return CoverageReconcileOutput{}, err
		}
		db, err := d.OpenDB(ctx)
		if err != nil {
			return CoverageReconcileOutput{}, err
		}
		defer func() { _ = db.Close() }()

		// Read before state.
		before, err := embed.ReadHealthReport(ctx, db, embed.LoreCorpus{})
		if err != nil {
			return CoverageReconcileOutput{}, fmt.Errorf("lore: coverage-reconcile: read before state: %w", err)
		}
		denBefore := before.CoverageDen
		numBefore := before.CoverageNum

		// Run the reconcile for both counters.
		if err := embed.ReconcileNum(ctx, db, embed.LoreCorpus{}); err != nil {
			return CoverageReconcileOutput{}, fmt.Errorf("lore: coverage-reconcile: reconcile num: %w", err)
		}
		if err := embed.ReconcileDen(ctx, db, embed.LoreCorpus{}); err != nil {
			return CoverageReconcileOutput{}, fmt.Errorf("lore: coverage-reconcile: reconcile den: %w", err)
		}

		// Read after state.
		after, err := embed.ReadHealthReport(ctx, db, embed.LoreCorpus{})
		if err != nil {
			return CoverageReconcileOutput{}, fmt.Errorf("lore: coverage-reconcile: read after state: %w", err)
		}

		return CoverageReconcileOutput{
			ProjectID: pid,
			DenBefore: denBefore,
			DenAfter:  after.CoverageDen,
			NumBefore: numBefore,
			NumAfter:  after.CoverageNum,
			Drift:     after.CoverageDen - denBefore,
		}, nil
	},
	CLIFormat: func(s command.CLISink, o CoverageReconcileOutput) string { return formatCoverageReconcile(s, o) },
	MCPFormat: func(s command.MCPSink, o CoverageReconcileOutput) string { return formatCoverageReconcile(s, o) },
}

func formatCoverageReconcile(s lineSink, o CoverageReconcileOutput) string {
	var b strings.Builder
	b.WriteString(s.Line("🔮", "[coverage-reconcile]", fmt.Sprintf("project=%s", o.ProjectID)))
	b.WriteString(fmt.Sprintf("  den_before: %d\n", o.DenBefore))
	b.WriteString(fmt.Sprintf("  den_after:  %d\n", o.DenAfter))
	b.WriteString(fmt.Sprintf("  num_before: %d\n", o.NumBefore))
	b.WriteString(fmt.Sprintf("  num_after:  %d\n", o.NumAfter))
	drift := o.Drift
	sign := "+"
	if drift < 0 {
		sign = ""
	}
	b.WriteString(fmt.Sprintf("  drift (den): %s%d\n", sign, drift))
	numDrift := o.NumAfter - o.NumBefore
	numSign := "+"
	if numDrift < 0 {
		numSign = ""
	}
	b.WriteString(fmt.Sprintf("  drift (num): %s%d\n", numSign, numDrift))
	if drift == 0 && numDrift == 0 {
		b.WriteString("  status:     counters were already correct\n")
	} else {
		b.WriteString("  status:     counters reconciled to live counts\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
