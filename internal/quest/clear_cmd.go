package quest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mathomhaus/guild/internal/command"
)

// FulfillInput carries the args for quest fulfillment.
// ClearInput is a backward-compat type alias.
type FulfillInput struct {
	QuestID string `json:"quest_id" jsonschema:"QUEST-NNN to fulfill"`
	Report  string `json:"report" jsonschema:"specific completion report: commit hash, files, issues — REQUIRED"`
	Project string `json:"project,omitempty"`
}

type FulfillOutput struct {
	Result    *FulfillResult `json:"result"`
	BriefHint string         `json:"brief_hint,omitempty"`
}

// Backward-compat type aliases so existing callers keep compiling.
type ClearInput = FulfillInput
type ClearOutput = FulfillOutput

// clearDeprecationNotice is the one-line migration gradient emitted
// whenever an agent or user invokes the legacy quest_clear / quest clear
// alias. Positioned after the success line so it doesn't crowd the main
// output. JSON mode is never affected (CLI path exits before this renders;
// MCP format appends it outside the JSON schema). QUEST-138 / LORE-122.
const clearDeprecationNotice = "⚠️ [deprecated] quest_clear will be removed — use quest_fulfill"

// FulfillCommand is the primary quest-completion verb. `quest clear` stays
// as a cobra alias for muscle-memory; `quest_fulfill` is the canonical MCP
// tool name. QUEST-106 / LORE-80.
var FulfillCommand = &command.Command[FulfillInput, FulfillOutput]{
	Name:       "quest_fulfill",
	CLIPath:    []string{"quest", "fulfill"},
	CLIAliases: []string{"clear"},
	Short:      "fulfill a quest (cascades unblock)",
	Long:       "Fulfill a quest. Report is REQUIRED — commit hash, files, remaining issues. Cascades unblock dependent quests.",
	Args: []command.ArgSpec{
		{
			Name:     "quest_id",
			Kind:     command.ArgPositional,
			Type:     command.ArgString,
			Required: true,
			Help:     "QUEST-NNN to mark fulfilled",
		},
		{
			Name: "report",
			Kind: command.ArgFlag,
			Type: command.ArgString,
			// Not Required at the domain layer — Fulfill() accepts empty
			// report and just writes the `done` event without a
			// [completed] note. Encourage callers to pass one via Help
			// phrasing, don't reject the call.
			Help: "completion report: commit hash, files, remaining issues",
		},
		{
			Name:  "project",
			Short: "p",
			Kind:  command.ArgFlag,
			Type:  command.ArgString,
			Help:  "project override",
		},
	},
	Handler: func(ctx context.Context, d command.Deps, in FulfillInput) (FulfillOutput, error) {
		if strings.TrimSpace(in.QuestID) == "" {
			return FulfillOutput{}, errors.New("quest_id required")
		}
		db, err := d.OpenDB(ctx)
		if err != nil {
			return FulfillOutput{}, err
		}
		defer func() { _ = db.Close() }()

		pid, err := d.ResolveProj(ctx, in.Project)
		if err != nil {
			return FulfillOutput{}, err
		}
		res, err := Fulfill(ctx, db, pid, in.QuestID, in.Report)
		if err != nil {
			return FulfillOutput{}, err
		}
		// Activity renewal: a daemon-mediated fulfill on a quest this session
		// leased refreshes that lease. No-op without a daemon. The lease's
		// own lifecycle (release on clean shutdown, expiry-driven cleanup) is
		// independent of the claim being fulfilled; renewing here only keeps
		// the row from expiring under the holding session before then.
		renewLeaseActivity(ctx, d, pid, res.Cleared.ID)

		out := FulfillOutput{Result: res}

		// Advisory hint: if no brief has been written recently, remind the
		// caller to write a handoff before compacting. Two paths coexist
		// during the QUEST-58 migration window:
		//
		//   1. HintExtras signal — the hint engine's no-brief-24h rule reads
		//      __hints_brief_stale and fires the canonical 💡 line. Preferred.
		//   2. out.BriefHint — legacy field; still populated so CLI paths
		//      that don't yet route through the engine keep their hint. The
		//      MCP format function drops it when the engine is wired so we
		//      don't render twice.
		lastAt, briefErr := LastBriefAt(ctx, db, pid)
		stale := briefErr == nil && (lastAt.IsZero() || time.Since(lastAt) > briefStaleThreshold)
		if stale {
			if extras := command.HintExtras(ctx); extras != nil {
				extras["__hints_brief_stale"] = true
			} else {
				out.BriefHint = `no quest_brief yet this session — consider quest_brief("what was done, what's next") before compact`
			}
		}

		return out, nil
	},
	CLIFormat:      func(s command.CLISink, o FulfillOutput) string { return formatFulfilled(s, o) },
	MCPFormat:      func(s command.MCPSink, o FulfillOutput) string { return formatFulfilled(s, o) },
	CLIErrorFormat: func(s command.CLISink, err error) (string, bool) { return formatFulfillError(s, err) },
	MCPErrorFormat: func(s command.MCPSink, err error) (string, bool) { return formatFulfillError(s, err) },
	// Emit the deprecation notice to stderr when invoked as the `clear`
	// alias so agents and users get a migration gradient. JSON mode is
	// unaffected — the cobra adapter exits before reaching this path.
	CLIAliasDeprecations: map[string]string{
		"clear": clearDeprecationNotice,
	},
}

// ClearCommand is the backward-compat MCP alias for FulfillCommand. It
// points at the same handler + formatters; only the tool name differs.
// Agents trained on `quest_clear` still work; new agents see `quest_fulfill`
// in tool discovery. CLIOnly=false, MCPOnly=true — CLI alias is handled
// via FulfillCommand.CLIAliases, so this struct stays off the CLI surface
// to avoid duplicate cobra verbs.
var ClearCommand = func() *command.Command[FulfillInput, FulfillOutput] {
	c := *FulfillCommand
	c.Name = "quest_clear"
	c.MCPOnly = true
	c.CLIAliases = nil // avoid cobra registering an orphan alias
	c.Short = "fulfill a quest (alias for quest_fulfill; cascades unblock)"
	// Override MCPFormat to append the deprecation notice after the success
	// line. This gives agents a clear pointer to the canonical verb without
	// polluting the primary quest_fulfill output. QUEST-138 / LORE-122.
	c.MCPFormat = func(s command.MCPSink, o FulfillOutput) string {
		return formatFulfilled(s, o) + "\n" + clearDeprecationNotice
	}
	return &c
}()

func formatFulfilled(s lineListSink, o FulfillOutput) string {
	res := o.Result
	var b strings.Builder
	b.WriteString(s.Line("🏆", "[fulfilled]", fmt.Sprintf("fulfilled %s", res.Cleared.ID)))
	if len(res.Unblocked) > 0 {
		b.WriteString("  unblocked:\n")
		for _, q := range res.Unblocked {
			if q.Subject != "" {
				fmt.Fprintf(&b, "    - %s: %s\n", q.ID, q.Subject)
				continue
			}
			fmt.Fprintf(&b, "    - %s\n", q.ID)
		}
	}
	if o.BriefHint != "" {
		b.WriteString(s.Line("💡", "[hint]", o.BriefHint))
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatFulfillError(s lineListSink, err error) (string, bool) {
	if errors.Is(err, ErrNotFound) {
		msg := fmt.Sprintf("quest_fulfill: %v", err)
		return strings.TrimRight(s.Line("❌", "[err]", msg), "\n"), true
	}
	return "", false
}
