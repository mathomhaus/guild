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

// EmbedderHealthCmdOutput wraps the HealthReport for the command registry.
type EmbedderHealthCmdOutput struct {
	Report *embed.HealthReport `json:"report"`
}

// EmbedderHealthCommand is the registry spec for `guild lore health`.
// It reads meta rows and lore_vectors/entries counts and renders the embedder
// health section. Does not touch the existing commune/inquest/meld output.
var EmbedderHealthCommand = &command.Command[EmbedderHealthInput, EmbedderHealthCmdOutput]{
	Name:       "lore_health",
	CLIPath:    []string{"lore", "health"},
	CLIAliases: []string{"embedder-health"},
	Short:      "embedder health report (coverage, pending, stale, errors)",
	Long: "Print the embedder health section: model_id, tokenizer_hash, runtime_version, dim, " +
		"coverage (num/den and percent), pending count, stale count, last encode error " +
		"(if any), last successful encode timestamp, and rolling embed_error_count.",
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

		report, err := embed.ReadHealthReport(ctx, db, embed.LoreCorpus{})
		if err != nil {
			return EmbedderHealthCmdOutput{}, fmt.Errorf("lore: health: %w", err)
		}
		return EmbedderHealthCmdOutput{Report: report}, nil
	},
	CLIFormat: func(s command.CLISink, o EmbedderHealthCmdOutput) string {
		return formatEmbedderHealth(s, o)
	},
	MCPFormat: func(s command.MCPSink, o EmbedderHealthCmdOutput) string {
		return formatEmbedderHealth(s, o)
	},
}

// formatEmbedderHealth renders the embedder health section.
// Works for both CLI and MCP sinks (both satisfy the lineSink interface).
func formatEmbedderHealth(s lineSink, o EmbedderHealthCmdOutput) string {
	r := o.Report
	if r == nil {
		return strings.TrimRight(s.Line("🔮", "[health]", "embedder: no data available"), "\n")
	}

	var b strings.Builder
	b.WriteString(s.Line("🔮", "[health]", "embedder section"))

	// State line.
	stateStr := string(r.State)
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
