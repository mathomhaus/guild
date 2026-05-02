package lore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mathomhaus/guild/internal/command"
)

type AppraiseInput struct {
	Query       string `json:"query" jsonschema:"search query; BM25+recency+title-boost ranked"`
	AllProjects bool   `json:"all_projects,omitempty" jsonschema:"search every project (recommended for research)"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max results (default 10)"`
	Since       string `json:"since,omitempty" jsonschema:"entries within window: 7d|2w|1m"`
	Project     string `json:"project,omitempty" jsonschema:"project override; ignored when all_projects=true"`
}

type AppraiseCmdOutput struct {
	Query  string          `json:"query"`
	Output *AppraiseOutput `json:"output"`
	Now    time.Time       `json:"now"`
}

var AppraiseCommand = &command.Command[AppraiseInput, AppraiseCmdOutput]{
	Name:       "lore_appraise",
	CLIPath:    []string{"lore", "appraise"},
	CLIAliases: []string{"check"},
	Short:      "search lore before researching or inscribing",
	Long:       "Search lore before storing new knowledge or spawning research subagents. Returns ranked entries with project, kind, age, and summary — if current results exist, use them instead of re-deriving.",
	Args: []command.ArgSpec{
		{Name: "query", Kind: command.ArgPositional, Type: command.ArgString, Required: true, Variadic: true, Help: "search query (remaining positional args joined on CLI)"},
		{Name: "all_projects", Kind: command.ArgFlag, Type: command.ArgBool, Help: "search every project"},
		{Name: "limit", Kind: command.ArgFlag, Type: command.ArgInt, Help: "max results (default 10)"},
		{Name: "since", Kind: command.ArgFlag, Type: command.ArgString, Help: "entries within window (7d|2w|1m)"},
		{Name: "project", Short: "p", Kind: command.ArgFlag, Type: command.ArgString, Help: "project override"},
	},
	Handler: func(ctx context.Context, d command.Deps, in AppraiseInput) (AppraiseCmdOutput, error) {
		query := strings.TrimSpace(in.Query)
		if query == "" {
			return AppraiseCmdOutput{}, errors.New("query required")
		}
		db, err := d.OpenDB(ctx)
		if err != nil {
			return AppraiseCmdOutput{}, err
		}
		defer func() { _ = db.Close() }()

		var pid string
		if !in.AllProjects {
			pid, err = d.ResolveProj(ctx, in.Project)
			if err != nil {
				return AppraiseCmdOutput{}, err
			}
		}
		var since time.Duration
		if in.Since != "" {
			since, err = ParseSince(in.Since)
			if err != nil {
				return AppraiseCmdOutput{}, fmt.Errorf("--since %q: %w", in.Since, err)
			}
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 10
		}
		now := time.Now().UTC()
		out, err := Appraise(ctx, db, AppraiseParams{
			Query:       query,
			Limit:       limit,
			AllProjects: in.AllProjects,
			Project:     pid,
			Since:       since,
			Scoring:     DefaultScoring(),
			Now:         now,
			Embed:       embedFromDeps(ctx, d),
		})
		if err != nil {
			return AppraiseCmdOutput{}, err
		}
		if len(out.Results) == 0 && d.RecordMiss != nil {
			recordProject := pid
			if recordProject == "" {
				recordProject = "all"
			}
			d.RecordMiss(ctx, recordProject, query)
		}
		// QUEST-58: the hint engine owns the slug-query rule now. When the
		// engine is wired via Deps.EvaluateHints (MCP surface today; any
		// migrated CLI surface tomorrow), zero the legacy MissHint so we
		// don't render the same advisory twice. The legacy CLI path in
		// internal/cli/lore_read.go bypasses this Command handler and
		// still sees MissHint as before.
		//
		// QUEST-73: the slug-query detector needs to know when the search
		// returned zero rows so it only fires on misses (the original
		// lore.slugHint was gated on len(rows)==0). Stuff the signal into
		// the hint Extras bag — the same mechanism no-brief-24h uses.
		if d.EvaluateHints != nil && out != nil {
			out.MissHint = ""
			if extras := command.HintExtras(ctx); extras != nil {
				extras["__hints_zero_result"] = len(out.Results) == 0
			}
		}
		return AppraiseCmdOutput{Query: query, Output: out, Now: now}, nil
	},
	CLIFormat: func(s command.CLISink, o AppraiseCmdOutput) string { return formatAppraiseResult(s, o) },
	MCPFormat: func(s command.MCPSink, o AppraiseCmdOutput) string { return formatAppraiseResult(s, o) },
}

func formatAppraiseResult(s lineSink, o AppraiseCmdOutput) string {
	out := o.Output
	if len(out.Results) == 0 {
		hint := out.MissHint
		if hint == "" {
			hint = "research needed"
		}
		msg := fmt.Sprintf("nothing found for %q — %s", o.Query, hint)
		return strings.TrimRight(s.Line("🔮", "[appraise]", msg), "\n")
	}
	var b strings.Builder
	b.WriteString(s.Line("🔮", "[appraise]", fmt.Sprintf("%d result(s) for %q:", len(out.Results), o.Query)))
	for _, r := range out.Results {
		e := r.Entry
		fmt.Fprintf(&b, "  %s [%s/%s · %s · %s]  %s\n",
			formatEntryID(e.ID), e.ProjectID, e.Kind, e.Status, formatAppraiseAge(e.CreatedAt, o.Now), e.Title)
		summary := e.Summary
		if len(summary) > 160 {
			summary = summary[:160] + "…"
		}
		fmt.Fprintf(&b, "  %s\n", summary)
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatAppraiseAge(createdAt, now time.Time) string {
	if createdAt.IsZero() || now.IsZero() {
		return "age unknown"
	}
	days := int(now.Sub(createdAt).Hours() / 24)
	if days < 0 {
		days = 0
	}
	return fmt.Sprintf("%dd ago", days)
}
