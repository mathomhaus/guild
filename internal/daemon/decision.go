package daemon

import (
	"sync/atomic"
	"time"
)

// This file is the ADR-006 Phase 5 decision-gate instrumentation seam for
// the daemon's three autonomous yes/no decisions: the idle-pass autopass
// (sleep_scheduler.go), the lease-reaper sweep (reaper.go), and the
// staleness-renewal of one watch event (pipeline.go). Each decision is
// captured as a frozen value object recording EVERY boolean input plus a
// human-readable reason, then handed to a DecisionRecorder. The pattern is
// ported from Headroom's CompressionDecision value type (every constituent
// boolean exposed so a dashboard can answer "what did the decision see?"
// without re-deriving it).
//
// CRITICAL PARITY INVARIANT: instrumentation NEVER changes a decision's
// outcome. The recorder is a settable package-global sink defaulting to a
// no-op; the daemon's loops record AFTER computing the outcome and the
// recorded struct's Allow field is the already-computed result, never an
// input to it. When no recorder is installed (the default, and the only
// state when the observability module is disabled), recordDecision is a
// cheap atomic load returning nil and the loops take exactly the same
// branches they took before this file existed: byte-identical behavior.
//
// The sink is a package-global rather than a Config field so the
// observability module can install itself from its own Service.Start
// without any edit to the daemon wiring in cmd/guild or internal/mcp (the
// only cross-agent file touched is internal/modules). The daemon never
// imports internal/observability; the module imports the daemon and
// installs a recorder through SetDecisionRecorder. No import cycle.

// DecisionKind identifies which daemon decision a record describes.
type DecisionKind string

const (
	// DecisionAutopass is the idle scheduler's "fire a dream pass now?"
	// decision (sleep_scheduler.go canFire). The ADR table calls this the
	// autopass decision.
	DecisionAutopass DecisionKind = "autopass"
	// DecisionLeaseReap is the lease reaper's "run a sweep this tick?"
	// decision (reaper.go sweep). The ADR table calls this lease-grant /
	// forfeit; at the daemon-loop level the observable gate is whether a
	// sweep ran and what it forfeited.
	DecisionLeaseReap DecisionKind = "lease_reap"
	// DecisionStaleRenew is the watch pipeline's "process this event into
	// staleness signals + renewal quests?" decision (pipeline.go handle).
	// The ADR table calls this staleness-renewal.
	DecisionStaleRenew DecisionKind = "stale_renew"
)

// Decision is an immutable, value-equal snapshot of one daemon yes/no
// decision: the kind, the final Allow result, a human-readable Reason, the
// moment it was taken, and the full set of boolean inputs the gate saw.
// Frozen by convention (every field is a value type and callers receive it
// by value); the recorder must treat it as read-only.
//
// Inputs carries the named booleans of THIS decision so a recorder can
// answer "why did it act?" without re-running the gate. The map is built
// once at the decision site and never mutated afterwards. Metrics carries
// any non-boolean scalar the decision produced (counts a sweep forfeited,
// steps a pass drove) for the same observability purpose.
type Decision struct {
	Kind    DecisionKind
	Allow   bool
	Reason  string
	At      time.Time
	Inputs  map[string]bool
	Metrics map[string]int
}

// DecisionRecorder records a daemon decision for observability. The daemon
// calls Record exactly once per decision, AFTER the outcome is computed, so
// an implementation can never influence the outcome. Implementations must
// be safe for concurrent use (the scheduler, reaper, and pipeline loops run
// on separate goroutines) and must return promptly: the daemon records on
// its hot loops and a slow recorder would delay serving.
type DecisionRecorder interface {
	Record(d Decision)
}

// decisionSink holds the installed recorder. atomic.Value so the loops read
// it lock-free on every decision and the observability Service can swap it
// in/out from another goroutine. A nil-typed value means "no recorder", the
// default and the disabled-module state.
var decisionSink atomic.Value // stores DecisionRecorder (possibly nil)

// SetDecisionRecorder installs (or, with nil, removes) the process-wide
// daemon decision recorder. The observability module's Service calls it
// with its recorder on Start and with nil on Stop, so recording is wired
// only while that module is enabled and its loop is live. Passing nil
// restores the byte-identical no-op path. Safe for concurrent use.
func SetDecisionRecorder(r DecisionRecorder) {
	// atomic.Value rejects storing a nil interface, so wrap in a holder.
	decisionSink.Store(recorderHolder{r})
}

// recorderHolder boxes the interface so atomic.Value can store a nil
// recorder (a nil interface cannot be Stored directly).
type recorderHolder struct{ r DecisionRecorder }

// recordDecision hands d to the installed recorder, if any. It is the only
// call the daemon loops make; with no recorder installed (the default) it
// is a single atomic load and a nil check, adding nothing observable to the
// decision path. It NEVER returns a value the caller branches on: the
// outcome is already in d.Allow before this is called.
func recordDecision(d Decision) {
	v := decisionSink.Load()
	if v == nil {
		return
	}
	h, ok := v.(recorderHolder)
	if !ok || h.r == nil {
		return
	}
	h.r.Record(d)
}
