package hints

import (
	"context"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/mathomhaus/guild/internal/quest"
)

// FollowThroughWindow is how many subsequent events the engine tracks
// a pending fire for before scoring it as a miss. Matches the
// "within N subsequent tool calls" language in QUEST-58 acceptance.
const FollowThroughWindow = 10

// FYICapPerSession is the max number of ℹ️ fyi hints fired in one
// session. Per QUEST-58 acceptance.
const FYICapPerSession = 3

// Engine is the top-level hint orchestrator. One Engine per process is
// typical: MCP boots one in server.build(); the CLI gets its own
// (scoped to the command process's lifetime). Safe for concurrent use —
// every mutable field is guarded.
type Engine struct {
	// Logger receives structured fire events + prune actions. Defaulted
	// to slog.Default in NewEngine when nil.
	Logger *slog.Logger

	// Now returns the engine's "now" for fire timestamps. Overrideable
	// in tests.
	Now func() time.Time

	// Store is the SQL backend. Can be nil in tests that only exercise
	// evaluation without persistence.
	Store *Store

	// context is the session-scoped Context. One per Engine.
	context *Context

	// rules is the composed rule set: DB rows + Definitions() merged by
	// Rule.ID. Loaded once per process via LoadRules.
	rules map[string]composedRule

	// pending tracks in-flight fires awaiting follow-through evaluation.
	// Slice, not map, so the scorer iterates in order and the oldest
	// fires fall off once their window expires.
	pending []pendingEntry

	mu sync.Mutex
}

// pendingEntry is one in-flight fire tracked in memory. Mirrors the
// hint_fires row shape but skips fields the scorer doesn't need.
type pendingEntry struct {
	fireID    int64
	ruleID    string
	firedAt   time.Time
	firedAtCC int // the engine callCount at fire time
}

// composedRule is the DB metadata + Rule detectors, merged by rule_id.
type composedRule struct {
	row  RuleRow
	rule Rule
}

// NewEngine constructs a fresh Engine with a session Context scoped to
// the given sessionID and era. Store may be nil for tests.
func NewEngine(store *Store, sessionID string, era Era) *Engine {
	return &Engine{
		Logger:  slog.Default(),
		Now:     time.Now,
		Store:   store,
		context: NewContext(sessionID, era),
	}
}

// LoadRules merges DB metadata with the pure-function Rule detectors,
// building the engine's rule table. Call once after NewEngine, before
// the first Evaluate. Reloading is idempotent.
func (e *Engine) LoadRules(ctx context.Context) error {
	if e == nil {
		return nil
	}
	defs := Definitions()
	defMap := make(map[string]Rule, len(defs))
	for _, d := range defs {
		defMap[d.ID] = d
	}
	rows := map[string]RuleRow{}
	if e.Store != nil {
		r, err := e.Store.LoadRules(ctx)
		if err != nil {
			return err
		}
		rows = r
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules = make(map[string]composedRule, len(defs))
	for id, d := range defMap {
		row, ok := rows[id]
		if !ok {
			// No DB row — synthesize a disabled placeholder. The seed
			// migration always lands these rows in production; this
			// branch only fires in tests that bypass migrations.
			row = RuleRow{ID: id, TriggerTool: d.TriggerTool, Severity: SeverityHint,
				Template: id, CooldownCalls: 5, Enabled: false}
		}
		e.rules[id] = composedRule{row: row, rule: d}
	}
	return nil
}

// Context returns the engine's session Context for external event
// tracking (e.g. the MCP handler wrapper records bootstrap calls into
// it as part of its telemetry hook).
func (e *Engine) Context() *Context {
	if e == nil {
		return nil
	}
	return e.context
}

// Fire is the result of an Evaluate call. Empty Fire (zero-value) means
// no hint should be rendered.
type Fire struct {
	// RuleID is the rule_id that fired.
	RuleID string
	// Severity is the resolved (era-aware) severity for display.
	Severity Severity
	// Message is the rendered hint text.
	Message string
	// Top reports whether the renderer should place this at the top of
	// the response (bolded). Mirrors Severity.IsTop() for convenience.
	Top bool
}

// Empty reports whether no fire was produced.
func (f Fire) Empty() bool { return f.RuleID == "" }

// Render returns the formatted hint line, ready to prepend/append to
// the tool output body.
//
//	💡 [hint] no quest_brief yet this session — consider quest_brief(...)
//
// Top-severity fires bold the label portion ("**[blocker]**") so the
// agent's visual scan catches them first; hint/fyi stay muted.
func (f Fire) Render() string {
	if f.Empty() {
		return ""
	}
	emoji := f.Severity.Emoji()
	label := f.Severity.Label()
	if f.Top {
		label = "**" + label + "**"
	}
	if emoji == "" {
		return label + " " + f.Message
	}
	return emoji + " " + label + " " + f.Message
}

// Evaluate processes one CallEvent: records it on the session Context,
// runs every matching rule's Trigger, applies budget+cooldown+suppression
// gates, resolves era-aware severity, and persists the winning fire to
// hint_fires (if any). Returns the Fire the caller should render, or an
// empty Fire if nothing fired.
//
// ctx is threaded through to the Store writes so cancellation propagates.
// Never returns an error: fire-or-not is a single bit; persistence errors
// are logged best-effort because a broken DB must not break the tool.
func (e *Engine) Evaluate(ctx context.Context, ev CallEvent) Fire {
	if e == nil {
		return Fire{}
	}

	// Record the event BEFORE evaluating so rules see their own call
	// count (needed for session-end-without-brief's >=30 check).
	e.context.RecordEvent(ev)

	// Score any pending fires from prior events whose follow-through
	// window now closes against this event. Best-effort — persistence
	// errors are logged, never propagated.
	e.scorePending(ctx, ev)

	e.mu.Lock()
	// Snapshot the rule table into a slice and sort by rule_id so the
	// budget selection below is deterministic. Without sorting, map
	// iteration order is random: two rules with equal severity rank would
	// race for the single hint slot, causing non-deterministic output
	// under the race detector's scheduling perturbation (QUEST-71).
	rules := make([]composedRule, 0, len(e.rules))
	for k := range e.rules {
		rules = append(rules, e.rules[k])
	}
	e.mu.Unlock()
	sort.Slice(rules, func(i, j int) bool { return rules[i].row.ID < rules[j].row.ID })

	var best *Fire
	for i := range rules {
		cr := rules[i]
		if !cr.row.Enabled {
			continue
		}
		// Normalize both sides to canonical tool names before comparing so
		// backward-compat aliases (e.g. quest_clear → quest_fulfill) trigger
		// rules authored against the canonical name. See quest.CanonicalToolName.
		if cr.row.TriggerTool != "*" &&
			quest.CanonicalToolName(cr.row.TriggerTool) != quest.CanonicalToolName(ev.Tool) {
			continue
		}
		if cr.rule.Trigger == nil || !cr.rule.Trigger(e.context, ev) {
			continue
		}
		// Cooldown check — identical rule cannot refire within N calls.
		cd := cr.row.CooldownCalls
		if cd <= 0 {
			cd = 5
		}
		if e.context.RuleFiredWithin(cr.row.ID, cd) {
			continue
		}
		// Era-aware severity resolution.
		sev := ResolveEraSeverity(cr.row.Severity, e.context.Era(), cr.row.PerEraSeverity)

		// Per-session fyi cap.
		if sev == SeverityFYI && e.context.FYIFiresThisSession() >= FYICapPerSession {
			continue
		}

		// Contextual suppression — if the agent already did the suggested
		// follow-through action in the last 5 events, skip.
		if cr.rule.FollowThrough != nil && e.recentlySatisfied(cr.rule.FollowThrough, 5) {
			continue
		}

		fire := Fire{
			RuleID:   cr.row.ID,
			Severity: sev,
			Message:  cr.row.Template,
			Top:      sev.IsTop(),
		}

		// Budget: highest-severity wins the single hint slot. Ties are
		// broken by the rule_id sort above so the winner is stable across
		// runs; without a tiebreak the race detector's scheduling
		// perturbation changes which rule wins non-deterministically.
		if best == nil || sev.Rank() > best.Severity.Rank() {
			fireCopy := fire
			best = &fireCopy
		}
	}

	if best == nil {
		return Fire{}
	}

	// Persist the fire.
	var fireID int64
	if e.Store != nil {
		id, err := e.Store.RecordFire(ctx, best.RuleID, "", e.context.SessionID(), e.Now())
		if err != nil {
			if e.Logger != nil {
				e.Logger.Debug("hints: record fire failed", "rule", best.RuleID, "err", err)
			}
		} else {
			fireID = id
		}
	}

	e.context.MarkFired(best.RuleID, best.Severity)

	// Track pending follow-through so a later scorePending call can
	// score the fire.
	e.mu.Lock()
	e.pending = append(e.pending, pendingEntry{
		fireID:    fireID,
		ruleID:    best.RuleID,
		firedAt:   e.Now(),
		firedAtCC: e.context.CallCount(),
	})
	e.mu.Unlock()

	if e.Logger != nil {
		e.Logger.Debug("hints: fired",
			"rule", best.RuleID,
			"severity", best.Severity.String(),
			"tool", ev.Tool,
			"session", e.context.SessionID())
	}
	return *best
}

// recentlySatisfied returns true when any event in the last n entries of
// the context history (excluding the current triggering call) satisfies
// followThrough. Used for contextual suppression.
func (e *Engine) recentlySatisfied(followThrough func(*Context, CallEvent) bool, n int) bool {
	events := e.context.Events(n + 1) // +1 to reach past the triggering call
	if len(events) == 0 {
		return false
	}
	// Drop the last (triggering) event.
	events = events[:len(events)-1]
	for _, ev := range events {
		if followThrough(e.context, ev) {
			return true
		}
	}
	return false
}

// scorePending walks the pending-fire list and closes out any entries
// whose follow-through window has elapsed or whose follow-through
// detector matches the current event.
func (e *Engine) scorePending(ctx context.Context, ev CallEvent) {
	e.mu.Lock()
	rules := e.rules
	pending := e.pending
	cc := e.context.CallCount()
	// Fresh backing array so concurrent Evaluate calls that append to
	// e.pending don't write into the same array we're iterating below.
	// pending[:0] would alias the same underlying array and race with
	// concurrent appenders under the -race detector even though the test
	// is logically sequential (Engine is documented as concurrent-safe).
	remaining := make([]pendingEntry, 0, len(pending))
	e.mu.Unlock()

	for _, p := range pending {
		cr, ok := rules[p.ruleID]
		if !ok || cr.rule.FollowThrough == nil {
			continue // rule vanished mid-session; drop silently
		}
		age := cc - p.firedAtCC
		hit := cr.rule.FollowThrough(e.context, ev)
		switch {
		case hit:
			if e.Store != nil && p.fireID > 0 {
				if err := e.Store.RecordFollowThrough(ctx, p.fireID, true, age); err != nil && e.Logger != nil {
					e.Logger.Debug("hints: score hit failed", "rule", p.ruleID, "err", err)
				}
			}
			// Don't retain — hit.
		case age >= FollowThroughWindow:
			if e.Store != nil && p.fireID > 0 {
				if err := e.Store.RecordFollowThrough(ctx, p.fireID, false, age); err != nil && e.Logger != nil {
					e.Logger.Debug("hints: score miss failed", "rule", p.ruleID, "err", err)
				}
			}
			// Window elapsed — don't retain.
		default:
			remaining = append(remaining, p)
		}
	}

	e.mu.Lock()
	e.pending = remaining
	e.mu.Unlock()
}

// CurrentPID returns the sessionID the Engine uses by default (the
// stringified OS pid). Exposed so tests can assert session correlation.
func CurrentPID() string {
	return strconv.Itoa(os.Getpid())
}
