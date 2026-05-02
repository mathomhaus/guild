package lore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
)

type InscribeInput struct {
	Title         string   `json:"title" jsonschema:"short distinctive title (search-friendly)"`
	Kind          string   `json:"kind" jsonschema:"one of: idea|research|decision|observation|principle"`
	Summary       string   `json:"summary" jsonschema:"2-3 sentence distillation"`
	Topic         string   `json:"topic" jsonschema:"topic slug (e.g. 'auth', 'caching')"`
	Tags          []string `json:"tags,omitempty" jsonschema:"semantic tags"`
	Informs       []string `json:"informs,omitempty" jsonschema:"source entry IDs (LORE-N, ENTRY-N, or bare N) that inform this entry — creates informs provenance edges at write-time"`
	Source        string   `json:"source,omitempty" jsonschema:"URL or path the knowledge came from"`
	PromptedBy    string   `json:"prompted_by,omitempty" jsonschema:"QUEST-N that prompted this inscribe"`
	NeedsReview   bool     `json:"needs_review,omitempty" jsonschema:"flag for human review"`
	NoWarn        bool     `json:"no_warn,omitempty" jsonschema:"suppress the ≤60-word principle warning"`
	StrictProject bool     `json:"strict_project,omitempty" jsonschema:"scope dedup to the current project only (default: cross-project)"`
	Project       string   `json:"project,omitempty"`
}

type InscribeCmdOutput struct {
	Result *InscribeResult `json:"result"`
}

var InscribeCommand = &command.Command[InscribeInput, InscribeCmdOutput]{
	Name:       "lore_inscribe",
	CLIPath:    []string{"lore", "inscribe"},
	CLIAliases: []string{"add"},
	Short:      "inscribe a new knowledge entry into the lore",
	Long:       "Store knowledge that transcends the current task — patterns, decisions, research that outlive the quest. Call lore_appraise first; pass informs=[IDs] for entries that informed this one to create provenance edges at write-time. Cross-project dedup and principle-hygiene warnings are built in.",
	Args: []command.ArgSpec{
		{Name: "title", Kind: command.ArgPositional, Type: command.ArgString, Required: true, Variadic: true, Help: "short distinctive title"},
		{Name: "kind", Short: "k", Kind: command.ArgFlag, Type: command.ArgString, Required: true, Help: "entry kind (required): idea|research|decision|observation|principle"},
		{Name: "summary", Short: "s", Kind: command.ArgFlag, Type: command.ArgString, Required: true, Help: "2-3 sentence summary (required)"},
		{Name: "topic", Short: "t", Kind: command.ArgFlag, Type: command.ArgString, Required: true, Help: "topic slug (required)"},
		{Name: "tags", Kind: command.ArgFlag, Type: command.ArgStringSlice, Repeatable: true, Help: "semantic tag (repeatable)"},
		{Name: "informs", Kind: command.ArgFlag, Type: command.ArgStringSlice, Repeatable: true, Help: "source entry id (LORE-N, ENTRY-N, or bare N) that informs this entry — repeatable, creates provenance edges"},
		{Name: "source", Kind: command.ArgFlag, Type: command.ArgString, Help: "source URL or reference"},
		{Name: "prompted_by", Kind: command.ArgFlag, Type: command.ArgString, Help: "QUEST-N that prompted this inscribe"},
		{Name: "needs_review", Kind: command.ArgFlag, Type: command.ArgBool, Help: "flag for human review"},
		{Name: "no_warn", Kind: command.ArgFlag, Type: command.ArgBool, Help: "suppress principle-bloat warning"},
		{Name: "strict_project", Kind: command.ArgFlag, Type: command.ArgBool, CLIFlagName: "strict-project", Help: "scope dedup to the current project only (default: cross-project)"},
		{Name: "project", Short: "p", Kind: command.ArgFlag, Type: command.ArgString, Help: "project override"},
	},
	Handler: func(ctx context.Context, d command.Deps, in InscribeInput) (InscribeCmdOutput, error) {
		for name, v := range map[string]string{"title": in.Title, "kind": in.Kind, "summary": in.Summary, "topic": in.Topic} {
			if strings.TrimSpace(v) == "" {
				return InscribeCmdOutput{}, fmt.Errorf("%s required", name)
			}
		}
		// Parse --informs entry IDs (accepts LORE-N, ENTRY-N, or bare N).
		informs, err := parseEntryIDs(in.Informs)
		if err != nil {
			return InscribeCmdOutput{}, err
		}
		db, err := d.OpenDB(ctx)
		if err != nil {
			return InscribeCmdOutput{}, err
		}
		defer func() { _ = db.Close() }()
		pid, err := d.ResolveProj(ctx, in.Project)
		if err != nil {
			return InscribeCmdOutput{}, err
		}
		res, err := Inscribe(ctx, db, &InscribeParams{
			ProjectID:     pid,
			Kind:          Kind(in.Kind),
			Title:         in.Title,
			Summary:       in.Summary,
			Topic:         in.Topic,
			Tags:          in.Tags,
			Informs:       informs,
			Source:        in.Source,
			PromptedBy:    in.PromptedBy,
			NeedsReview:   in.NeedsReview,
			NoWarn:        in.NoWarn,
			StrictProject: in.StrictProject,
			Embed:         embedFromDeps(ctx, d),
		})
		if err != nil {
			return InscribeCmdOutput{}, err
		}
		return InscribeCmdOutput{Result: res}, nil
	},
	CLIFormat:   func(s command.CLISink, o InscribeCmdOutput) string { return formatInscribedBody(s, o) },
	CLIWarnings: formatInscribedWarnings,
	MCPFormat:   formatInscribedMCP,
	CLIErrorFormat: func(s command.CLISink, err error) (string, bool) {
		if errors.Is(err, ErrInvalidKind) {
			return strings.TrimRight(s.Line("❌", "[err]", fmt.Sprintf("lore_inscribe: %v", err)), "\n"), true
		}
		return "", false
	},
}

// parseEntryIDs parses a slice of raw entry-id strings (LORE-N, ENTRY-N, or bare N)
// into int64 IDs. Returns an error if any token is malformed.
func parseEntryIDs(raw []string) ([]int64, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]int64, 0, len(raw))
	for _, s := range raw {
		id, err := ParseEntryID(s)
		if err != nil {
			return nil, fmt.Errorf("informs: %w", err)
		}
		out = append(out, id)
	}
	return out, nil
}

// formatInscribedBody renders the success confirmation line for both CLI
// and MCP surfaces — no warnings, so both surfaces share the same body.
func formatInscribedBody(s lineSink, o InscribeCmdOutput) string {
	res := o.Result
	return strings.TrimRight(s.Line("📜", "[inscribed]",
		fmt.Sprintf("inscribed %s: %s [%s]", formatEntryID(res.Entry.ID), res.Entry.Title, res.Entry.Kind)), "\n")
}

// formatInscribedWarnings renders dedup hits, bloat notices, and the
// near-duplicate hint for the CLI surface only. The result is written to
// stderr by the CLI adapter so warnings don't contaminate stdout and don't
// cause exit non-zero. Returns "" when there is nothing to warn about.
func formatInscribedWarnings(s command.CLISink, o InscribeCmdOutput) string {
	res := o.Result
	if len(res.DedupHits) == 0 && !res.BloatWarned && res.NearDupHint == "" {
		return ""
	}
	var b strings.Builder
	if len(res.DedupHits) > 0 {
		b.WriteString(s.Line("⚠️", "[warn]", "similar entries found:"))
		for _, h := range res.DedupHits {
			fmt.Fprintf(&b, "   %s  [%s · %s]  (%s)  %s\n",
				formatEntryID(h.EntryID), h.Kind, h.Status, h.ProjectID, h.Title)
		}
		fmt.Fprintf(&b, "   -> If duplicate, run: lore reforge %s --with %s\n",
			formatEntryID(res.DedupHits[0].EntryID), formatEntryID(res.Entry.ID))
	}
	if res.BloatWarned {
		fmt.Fprintf(&b, "⚠️  principle exceeds %d-word oath hygiene (%d words) — consider kind=decision\n",
			PrincipleMaxWordsDefault, res.BloatWords)
		fmt.Fprintf(&b, "  remedy: lore_update(entry_id=%d, kind=\"decision\")\n", res.Entry.ID)
	}
	if res.NearDupHint != "" {
		b.WriteString(strings.TrimRight(s.Line("💡", "[hint]", res.NearDupHint), "\n") + "\n")
	}
	return b.String()
}

// formatInscribedMCP renders the full inscribe result for MCP clients:
// the success line plus any warnings and hints inlined into the structured
// body. MCP has no separate stderr channel so warnings stay in the body.
func formatInscribedMCP(s command.MCPSink, o InscribeCmdOutput) string {
	res := o.Result
	var b strings.Builder
	b.WriteString(s.Line("📜", "[inscribed]",
		fmt.Sprintf("inscribed %s: %s [%s]", formatEntryID(res.Entry.ID), res.Entry.Title, res.Entry.Kind)))
	if len(res.DedupHits) > 0 {
		b.WriteString(s.Line("⚠️", "[warn]", "similar entries found:"))
		for _, h := range res.DedupHits {
			fmt.Fprintf(&b, "   %s  [%s · %s]  (%s)  %s\n",
				formatEntryID(h.EntryID), h.Kind, h.Status, h.ProjectID, h.Title)
		}
		fmt.Fprintf(&b, "   -> If duplicate, run: lore reforge %s --with %s\n",
			formatEntryID(res.DedupHits[0].EntryID), formatEntryID(res.Entry.ID))
	}
	if res.BloatWarned {
		fmt.Fprintf(&b, "⚠️  principle exceeds %d-word oath hygiene (%d words) — consider kind=decision\n",
			PrincipleMaxWordsDefault, res.BloatWords)
		fmt.Fprintf(&b, "  remedy: lore_update(entry_id=%d, kind=\"decision\")\n", res.Entry.ID)
	}
	if res.NearDupHint != "" {
		b.WriteString(s.Line("💡", "[hint]", res.NearDupHint))
	}
	return strings.TrimRight(b.String(), "\n")
}
