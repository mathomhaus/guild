package lore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/lore/embed"
)

// EmbedderHealthInput is the typed input for `guild lore health` embedder section.
type EmbedderHealthInput struct {
	Project string `json:"project,omitempty"`
}

// EmbedderHealthCmdOutput wraps per-corpus HealthReports for the command registry.
type EmbedderHealthCmdOutput struct {
	LoreReport  *embed.HealthReport `json:"lore_report"`
	QuestReport *embed.HealthReport `json:"quest_report"`
}

// EmbedderHealthCommand is the registry spec for `guild lore health`.
// It reads meta rows and vector/entity counts for both lore and quest
// corpora and renders the embedder health sections. Does not touch the
// existing commune/inquest/meld output.
var EmbedderHealthCommand = &command.Command[EmbedderHealthInput, EmbedderHealthCmdOutput]{
	Name:       "lore_health",
	CLIPath:    []string{"lore", "health"},
	CLIAliases: []string{"embedder-health"},
	Short:      "embedder health report (coverage, pending, stale, errors)",
	Long: "Print the embedder health sections for lore and quest corpora: model_id, " +
		"tokenizer_hash, runtime_version, dim, coverage (num/den and percent), pending " +
		"count, stale count, last encode error (if any), last successful encode " +
		"timestamp, and rolling embed_error_count.",
	Args: []command.ArgSpec{
		{Name: "project", Short: "p", Kind: command.ArgFlag, Type: command.ArgString, Help: "project override"},
	},
	Handler: func(ctx context.Context, d command.Deps, in EmbedderHealthInput) (EmbedderHealthCmdOutput, error) {
		db, err := d.OpenDB(ctx)
		if err != nil {
			return EmbedderHealthCmdOutput{}, err
		}
		defer func() { _ = db.Close() }()
		// ResolveProj enforces the bootstrap contract (active project must
		// be set via guild_session_start). The project ID is not used by
		// ReadHealthReport (embedder meta is global) but skipping this
		// call would let the tool operate without a bootstrapped project,
		// violating the TestTools_BootstrapRequired contract.
		if _, err := d.ResolveProj(ctx, in.Project); err != nil {
			return EmbedderHealthCmdOutput{}, err
		}

		loreReport, err := embed.ReadHealthReport(ctx, db, embed.LoreCorpus{})
		if err != nil {
			return EmbedderHealthCmdOutput{}, fmt.Errorf("lore: health: %w", err)
		}

		questReport := emptyQuestHealthReport()
		if d.OpenQuestDB != nil {
			questDB, qerr := d.OpenQuestDB(ctx)
			if qerr == nil {
				defer func() { _ = questDB.Close() }()
				if report, rerr := embed.ReadHealthReport(ctx, questDB, embed.QuestCorpus{}); rerr == nil {
					questReport = report
				}
			}
		}

		return EmbedderHealthCmdOutput{LoreReport: loreReport, QuestReport: questReport}, nil
	},
	CLIFormat: func(s command.CLISink, o EmbedderHealthCmdOutput) string {
		return formatEmbedderHealth(s, o)
	},
	MCPFormat: func(s command.MCPSink, o EmbedderHealthCmdOutput) string {
		return formatEmbedderHealth(s, o)
	},
}

// emptyQuestHealthReport returns a zeroed report so the quest section
// renders 0/0 coverage when quest.db is unavailable or unreadable.
func emptyQuestHealthReport() *embed.HealthReport {
	return &embed.HealthReport{State: embed.EmbedderStateDisabled}
}

// formatEmbedderHealth renders the lore and quest embedder health sections.
// Works for both CLI and MCP sinks (both satisfy the lineSink interface).
func formatEmbedderHealth(s lineSink, o EmbedderHealthCmdOutput) string {
	var b strings.Builder
	b.WriteString(formatCorpusEmbedderHealth(s, "lore", o.LoreReport))
	b.WriteString("\n")
	b.WriteString(formatCorpusEmbedderHealth(s, "quest", o.QuestReport))
	return strings.TrimRight(b.String(), "\n")
}

// formatCorpusEmbedderHealth renders one corpus embedder health section.
func formatCorpusEmbedderHealth(s lineSink, corpus string, r *embed.HealthReport) string {
	if r == nil {
		r = emptyQuestHealthReport()
	}

	var b strings.Builder
	b.WriteString(s.Line("🔮", "[health]", fmt.Sprintf("%s corpus embedder section", corpus)))

	// State line.
	stateStr := string(r.State)
	if stateStr == "" {
		stateStr = string(embed.EmbedderStateDisabled)
	}
	sessionLine := r.SessionLine()
	if sessionLine != "" {
		stateStr += fmt.Sprintf(": %s", sessionLine)
	}
	b.WriteString(fmt.Sprintf("  state:           %s\n", stateStr))

	// Identity fields.
	b.WriteString(fmt.Sprintf("  model_id:        %s\n", orNA(r.ModelID)))
	b.WriteString(fmt.Sprintf("  tokenizer_hash:  %s\n", orNA(r.TokenizerHash)))
	b.WriteString(fmt.Sprintf("  runtime_version: %s\n", orNA(r.RuntimeVersion)))
	b.WriteString(fmt.Sprintf("  dim:             %d\n", r.Dim))

	// Coverage.
	b.WriteString(fmt.Sprintf("  coverage:        %d/%d (%.1f%%)\n",
		r.CoverageNum, r.CoverageDen, r.CoveragePct))
	b.WriteString(fmt.Sprintf("  pending:         %d\n", r.PendingCount))
	b.WriteString(fmt.Sprintf("  stale:           %d\n", r.StaleCount))
	b.WriteString(fmt.Sprintf("  vector_epoch:    %d\n", r.VectorEpoch))

	// Error tracking.
	b.WriteString(fmt.Sprintf("  embed_errors:    %d (rolling)\n", r.EmbedErrorCount))

	if r.LastEncodeError != "" {
		errLine := r.LastEncodeError
		if r.LastEncodeErrAt != nil {
			errLine += fmt.Sprintf(" (at %s)", r.LastEncodeErrAt.Format(time.RFC3339))
		}
		b.WriteString(fmt.Sprintf("  last_error:      %s\n", errLine))
	}

	if r.LastEncodeOKAt != nil {
		b.WriteString(fmt.Sprintf("  last_ok_at:      %s\n", r.LastEncodeOKAt.Format(time.RFC3339)))
	}

	// Session-start line preview (only when non-healthy).
	if sessionLine != "" {
		b.WriteString(fmt.Sprintf("  session_line:    %s\n", sessionLine))
	}

	return strings.TrimRight(b.String(), "\n")
}

// orNA returns s if non-empty, otherwise "(n/a)".
func orNA(s string) string {
	if s == "" {
		return "(n/a)"
	}
	return s
}
