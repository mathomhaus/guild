package quest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
)

// AcceptInput is the shared input struct for `guild quest accept` and
// the MCP `quest_accept` tool. Fields carry `json:` + `jsonschema:`
// tags so the MCP SDK's reflection-based schema generation emits the
// right descriptions without a second source of truth.
type AcceptInput struct {
	QuestID string `json:"quest_id" jsonschema:"QUEST-NNN"`
	Owner   string `json:"owner,omitempty" jsonschema:"claim owner (defaults to 'agent')"`
	Project string `json:"project,omitempty"`
}

// AcceptOutput is the registry's return type for quest_accept. Scroll
// is best-effort: nil when the follow-up Scroll read fails. Handlers
// treat the scroll tail as observability, not an atomicity invariant.
type AcceptOutput struct {
	Quest  *Quest        `json:"quest"`
	Scroll *ScrollResult `json:"scroll,omitempty"`
}

// AcceptCommand is the registry spec for `quest_accept`. Both the
// cobra CLI surface and the MCP tool surface are generated from this
// single value by internal/command.
var AcceptCommand = &command.Command[AcceptInput, AcceptOutput]{
	Name:    "quest_accept",
	CLIPath: []string{"quest", "accept"},
	Short:   "atomically claim a quest",
	Long:    "Atomically claim a quest so two agents do not take the same work, then return the current spec and recent state.",
	Args: []command.ArgSpec{
		{
			Name:     "quest_id",
			Kind:     command.ArgPositional,
			Type:     command.ArgString,
			Required: true,
			Help:     "QUEST-NNN",
		},
		{
			Name:    "owner",
			Kind:    command.ArgFlag,
			Type:    command.ArgString,
			Help:    "claim owner (defaults to 'agent')",
			CLIOnly: true, // MCP agents always claim as 'agent' — keeps schema lean
		},
		{
			Name:  "project",
			Short: "p",
			Kind:  command.ArgFlag,
			Type:  command.ArgString,
			Help:  "project override",
		},
	},
	Handler: func(ctx context.Context, d command.Deps, in AcceptInput) (AcceptOutput, error) {
		db, err := d.OpenDB(ctx)
		if err != nil {
			return AcceptOutput{}, err
		}
		defer func() { _ = db.Close() }()

		pid, err := d.ResolveProj(ctx, in.Project)
		if err != nil {
			return AcceptOutput{}, err
		}
		if strings.TrimSpace(in.QuestID) == "" {
			return AcceptOutput{}, errors.New("quest_id required")
		}

		q, err := Accept(ctx, db, pid, in.QuestID, in.Owner)
		if err != nil {
			return AcceptOutput{}, err
		}
		// Daemon-mediated accepts acquire a lease so a crashed agent's
		// claim can later be reaped (ADR-005 Part 1, daemon Phase 3).
		// The seam is nil without a daemon: no lease row, byte-identical
		// to today. Best-effort, same invariant as the trail writer
		// (accept.go): a lease failure never converts a committed claim
		// into an API error, and a claim made with no lease is never
		// falsely forfeitable later because no expiry row exists for it.
		if lease := leaseFromDeps(d); lease != nil {
			if lerr := lease.AcquireLease(ctx, pid, q.ID, q.Owner); lerr != nil {
				slog.Warn("quest: accept: lease acquire failed; claim is durable",
					"quest_id", q.ID,
					"error", lerr,
				)
			}
		}
		scroll, _ := Scroll(ctx, db, pid, q.ID) // best-effort — not fatal
		return AcceptOutput{Quest: q, Scroll: scroll}, nil
	},
	CLIFormat: func(s command.CLISink, o AcceptOutput) string { return formatAccepted(s, o) },
	MCPFormat: func(s command.MCPSink, o AcceptOutput) string { return formatAccepted(s, o) },
	CLIErrorFormat: func(s command.CLISink, err error) (string, bool) {
		return formatAcceptError(s, err)
	},
	MCPErrorFormat: func(s command.MCPSink, err error) (string, bool) {
		return formatAcceptError(s, err)
	},
}

// lineListSink is the minimum interface both CLISink and MCPSink
// satisfy. Local to this package because the helpers below are the
// only users — keeps the command package free of shared abstractions
// that would tempt other verbs into surface-blind output.
type lineListSink interface {
	Line(glyph, ascii, text string) string
	List(label string, items []string) string
}

func formatAccepted(s lineListSink, o AcceptOutput) string {
	var b strings.Builder
	q := o.Quest
	b.WriteString(s.Line("⚔️", "[accepted]", fmt.Sprintf("accepted %s: %s", q.ID, q.Subject)))

	var meta []string
	meta = append(meta, fmt.Sprintf("status=%s", q.Status))
	if q.Priority != "" {
		meta = append(meta, fmt.Sprintf("priority=%s", q.Priority))
	}
	if q.Epic != "" {
		meta = append(meta, fmt.Sprintf("campaign=%s", q.Epic))
	}
	if q.Owner != "" {
		meta = append(meta, fmt.Sprintf("owner=%s", q.Owner))
	}
	b.WriteString("  ")
	b.WriteString(strings.Join(meta, " · "))
	b.WriteString("\n")

	b.WriteString(s.List("files", q.Files))
	b.WriteString(s.List("acceptance", q.Acceptance))
	b.WriteString(s.List("depends_on", q.DependsOn))
	b.WriteString(s.List("blocks", q.Blocks))

	journal, campfire := latestScrollContext(o.Scroll)
	if journal != "" {
		b.WriteString("  latest journal: ")
		b.WriteString(journal)
		b.WriteString("\n")
	}
	if campfire != "" {
		b.WriteString("  latest campfire: ")
		b.WriteString(campfire)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "  next useful lore call: lore_appraise(query=%q, all_projects=True)", q.Subject)
	return strings.TrimRight(b.String(), "\n")
}

func formatAcceptError(s lineListSink, err error) (string, bool) {
	var claimed *AlreadyClaimedError
	if errors.As(err, &claimed) {
		msg := fmt.Sprintf("already accepted: %s is held by %s (status=%s)",
			claimed.QuestID, claimed.Owner, claimed.Status)
		return strings.TrimRight(s.Line("❌", "[err]", msg), "\n"), true
	}
	if errors.Is(err, ErrNotFound) {
		msg := fmt.Sprintf("quest_accept: %v", err)
		return strings.TrimRight(s.Line("❌", "[err]", msg), "\n"), true
	}
	return "", false
}

// latestScrollContext picks the most recent journal + campfire lines
// from a ScrollResult. Mirrors the older latestQuestContext helper in
// internal/mcp/tools_quest.go; co-locating it here lets both adapters
// share the same selection rules.
func latestScrollContext(scroll *ScrollResult) (journal, campfire string) {
	if scroll == nil {
		return "", ""
	}
	for i := len(scroll.Notes) - 1; i >= 0; i-- {
		note := oneLineNote(scroll.Notes[i].Note)
		if note == "" {
			continue
		}
		if campfire == "" && isScrollCampfireNote(note) {
			campfire = strings.TrimPrefix(note, "[checkpoint] ")
			continue
		}
		if journal == "" && !isScrollSystemNote(note) {
			journal = note
		}
		if journal != "" && campfire != "" {
			break
		}
	}
	return journal, campfire
}

func isScrollCampfireNote(note string) bool {
	return strings.HasPrefix(note, "[checkpoint] ") &&
		!strings.HasPrefix(note, "[checkpoint] accepted by ")
}

func isScrollSystemNote(note string) bool {
	return strings.HasPrefix(note, "[spec] ") ||
		strings.HasPrefix(note, "[spec-replace] ") ||
		strings.HasPrefix(note, "[rework] of:") ||
		strings.HasPrefix(note, "[checkpoint] ") ||
		strings.HasPrefix(note, "[completed] ")
}

func oneLineNote(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}
