package quest

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/hooks"
	"github.com/mathomhaus/guild/internal/lore"
)

type BriefInput struct {
	Text    string `json:"text" jsonschema:"~200-word handoff: what was done, what's next, gotchas"`
	Project string `json:"project,omitempty"`
	// Auto and Capture are CLI-only hook-mode switches; the MCP surface
	// never sees them (CLIOnly on the ArgSpecs strips them from the
	// tool schema).
	Auto    bool `json:"auto,omitempty"`
	Capture bool `json:"capture,omitempty"`
}

type BriefOutput struct {
	// Payload is the rendered session-priming payload in --auto mode.
	// Empty for the plain text-brief path.
	Payload string `json:"payload,omitempty"`
	// Captured is true when --auto --capture stored the payload as the
	// session brief instead of printing it.
	Captured bool `json:"captured,omitempty"`
}

var BriefCommand = &command.Command[BriefInput, BriefOutput]{
	Name:    "quest_brief",
	CLIPath: []string{"quest", "brief"},
	Short:   "write session-end handoff for the next agent",
	Long:    "Session-end briefing for the next agent. Surfaced by next session's guild_session_start. Call before compact.",
	Args: []command.ArgSpec{
		{
			Name: "text",
			Kind: command.ArgPositional,
			Type: command.ArgString,
			// Not Required at the cobra layer: --auto renders the brief
			// without TEXT. The handler enforces text-or-auto.
			Required: false,
			Variadic: true,
			Help:     "~200-word handoff: what was done, what's next, gotchas",
		},
		{
			Name:  "project",
			Short: "p",
			Kind:  command.ArgFlag,
			Type:  command.ArgString,
			Help:  "project override",
		},
		{
			Name:    "auto",
			Kind:    command.ArgFlag,
			Type:    command.ArgBool,
			CLIOnly: true,
			Help:    "hook mode: print the bounded session-priming payload (oath + last brief + top bounties) to stdout",
		},
		{
			Name:    "capture",
			Kind:    command.ArgFlag,
			Type:    command.ArgBool,
			CLIOnly: true,
			Help:    "with --auto: store the rendered payload as the session brief and print a one-line confirmation",
		},
	},
	Handler: func(ctx context.Context, d command.Deps, in BriefInput) (BriefOutput, error) {
		text := strings.TrimSpace(in.Text)
		switch {
		case in.Capture && !in.Auto:
			return BriefOutput{}, errors.New("--capture requires --auto")
		case in.Auto && text != "":
			return BriefOutput{}, errors.New("cannot combine TEXT with --auto")
		case !in.Auto && text == "":
			return BriefOutput{}, errors.New("text required")
		}

		db, err := d.OpenDB(ctx)
		if err != nil {
			return BriefOutput{}, err
		}
		defer func() { _ = db.Close() }()

		pid, err := d.ResolveProj(ctx, in.Project)
		if err != nil {
			return BriefOutput{}, err
		}

		if !in.Auto {
			if err := Brief(ctx, db, pid, text, "agent"); err != nil {
				return BriefOutput{}, err
			}
			return BriefOutput{}, nil
		}

		payload, err := renderAutoBrief(ctx, d, db, pid)
		if err != nil {
			return BriefOutput{}, err
		}
		if in.Capture {
			if err := Brief(ctx, db, pid, payload, "hook"); err != nil {
				return BriefOutput{}, err
			}
			return BriefOutput{Captured: true}, nil
		}
		return BriefOutput{Payload: payload}, nil
	},
	CLIFormat: func(s command.CLISink, o BriefOutput) string { return formatBriefOutput(s, o) },
	MCPFormat: func(s command.MCPSink, o BriefOutput) string { return formatBriefOutput(s, o) },
}

// renderAutoBrief loads the session snapshot (oath + last brief + top
// bounties) and renders the bounded hook payload. The oath comes from
// the lore DB when reachable; lore failures degrade to an empty oath
// section rather than failing the hook (matching renderBounties'
// graceful degradation on the MCP surface).
func renderAutoBrief(ctx context.Context, d command.Deps, db *sql.DB, projectID string) (string, error) {
	var oathLoader OathLoader
	if d.OpenLoreDB != nil {
		if ldb, lerr := d.OpenLoreDB(ctx); lerr == nil {
			defer func() { _ = ldb.Close() }()
			oathLoader = func(ctx context.Context, proj string) ([]OathEntry, error) {
				entries, err := lore.Oath(ctx, ldb, proj)
				if err != nil {
					return nil, err
				}
				out := make([]OathEntry, len(entries))
				for i, e := range entries {
					out[i] = OathEntry{Title: e.Title, Summary: e.Summary}
				}
				return out, nil
			}
		}
	}

	res, err := Bounties(ctx, db, projectID, false, oathLoader, nil)
	if err != nil {
		return "", err
	}

	data := hooks.SessionData{
		BriefAt:    res.LastBriefAt,
		BriefAgent: res.LastBriefAgent,
		BriefText:  res.LastBriefText,
	}
	for _, o := range res.Oath {
		data.Oath = append(data.Oath, hooks.OathLine{Title: o.Title, Summary: o.Summary})
	}
	for i, q := range res.AllNext {
		if i >= hooks.MaxBountyLines {
			break
		}
		data.Bounties = append(data.Bounties, hooks.BountyLine{
			ID:       q.ID,
			Priority: string(q.Priority),
			Subject:  q.Subject,
		})
	}
	return hooks.RenderSession(data), nil
}

func formatBriefOutput(s lineListSink, o BriefOutput) string {
	switch {
	case o.Payload != "":
		// Hook mode: stdout is injected verbatim into agent context, so
		// the payload is the whole output. No narration prefix. See the
		// stdout contract in internal/hooks/payload.go.
		return strings.TrimRight(o.Payload, "\n")
	case o.Captured:
		return strings.TrimRight(s.Line("📋", "[briefing]", "auto brief captured for next session"), "\n")
	default:
		return formatBriefed(s)
	}
}

func formatBriefed(s lineListSink) string {
	return strings.TrimRight(s.Line("📋", "[briefing]", "briefed for next session"), "\n")
}
