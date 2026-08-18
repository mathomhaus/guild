package lore

import (
	"context"
	"fmt"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/lore/embed"
)

// EmbedRebuildInput is the typed input for guild lore embed-rebuild.
type EmbedRebuildInput struct {
	Project string `json:"project,omitempty"`
}

// EmbedRebuildOutput carries the results of an embed-rebuild run.
type EmbedRebuildOutput struct {
	ProjectID string `json:"project_id"`
	Encoded   int64  `json:"encoded"`
	Skipped   int64  `json:"skipped"`
	// Disabled is true when the embedder is not enabled; rebuild is a no-op.
	Disabled bool   `json:"disabled,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// EmbedRebuildCommand is the registry spec for `guild lore embed-rebuild`.
// It deletes all lore_vectors rows for the active project, sets every active
// entry's vector_state to 'pending', then runs the rebuild routine (local
// rebuildVectors from internal/lore/embed/health.go, pending QUEST-210's
// shared Backfill helper).
var EmbedRebuildCommand = &command.Command[EmbedRebuildInput, EmbedRebuildOutput]{
	Name:       "lore_embed_rebuild",
	CLIPath:    []string{"lore", "embed-rebuild"},
	CLIAliases: []string{"embed-reset"},
	Short:      "reset and rebuild the embedding vector index for the active project",
	Long: "Delete all lore_vectors rows for the active project, flip every active entry's " +
		"vector_state to 'pending', then encode each entry and insert the resulting " +
		"int8 vectors. Safe under concurrent MCP servers: uses BEGIN IMMEDIATE for the " +
		"reset phase and INSERT OR IGNORE for each vector write (ADR-003 invariants).",
	Args: []command.ArgSpec{
		{Name: "project", Short: "p", Kind: command.ArgFlag, Type: command.ArgString, Help: "project override"},
	},
	Handler: func(ctx context.Context, d command.Deps, in EmbedRebuildInput) (EmbedRebuildOutput, error) {
		pid, err := d.ResolveProj(ctx, in.Project)
		if err != nil {
			return EmbedRebuildOutput{}, err
		}
		db, err := d.OpenDB(ctx)
		if err != nil {
			return EmbedRebuildOutput{}, err
		}
		defer func() { _ = db.Close() }()

		// Read the embedder state from meta before committing to a full rebuild.
		report, err := embed.ReadHealthReport(ctx, db, embed.LoreCorpus{})
		if err != nil {
			return EmbedRebuildOutput{}, fmt.Errorf("lore: embed-rebuild: read health: %w", err)
		}

		if report.State != embed.EmbedderStateEnabled {
			return EmbedRebuildOutput{
				ProjectID: pid,
				Disabled:  true,
				Reason:    "embedder is disabled; enable via guild init on a supported platform",
			}, nil
		}

		ed := embedFromDeps(ctx, d)
		if !ed.Enabled() {
			return EmbedRebuildOutput{
				ProjectID: pid,
				Disabled:  true,
				Reason:    "embedder runtime is not wired into this surface; embed-rebuild cannot encode and will not wipe existing vectors",
			}, nil
		}

		e := ed.Embedder
		modelID := ed.ModelID
		if modelID == "" {
			modelID = report.ModelID
		}
		if modelID == "" {
			modelID = string(MetaEmbedderModelID)
		}

		if err := embed.RebuildVectors(ctx, db, pid, e, modelID); err != nil {
			return EmbedRebuildOutput{}, fmt.Errorf("lore: embed-rebuild: %w", err)
		}

		// Re-read coverage_num after rebuild to report final counts.
		after, err := embed.ReadHealthReport(ctx, db, embed.LoreCorpus{})
		if err != nil {
			return EmbedRebuildOutput{ProjectID: pid}, nil
		}

		den := after.CoverageDen
		skipped := den - after.CoverageNum
		if skipped < 0 {
			skipped = 0
		}
		return EmbedRebuildOutput{
			ProjectID: pid,
			Encoded:   after.CoverageNum,
			Skipped:   skipped,
		}, nil
	},
	CLIFormat: func(s command.CLISink, o EmbedRebuildOutput) string { return formatEmbedRebuild(s, o) },
	MCPFormat: func(s command.MCPSink, o EmbedRebuildOutput) string { return formatEmbedRebuild(s, o) },
}

func formatEmbedRebuild(s lineSink, o EmbedRebuildOutput) string {
	if o.Disabled {
		return strings.TrimRight(
			s.Line("🔮", "[embed-rebuild]", fmt.Sprintf("skipped: %s", o.Reason)),
			"\n",
		)
	}
	msg := fmt.Sprintf("project=%s encoded=%d skipped=%d", o.ProjectID, o.Encoded, o.Skipped)
	return strings.TrimRight(s.Line("🔮", "[embed-rebuild]", msg), "\n")
}
