// Package testsupport holds shared, test-only helpers for daemon
// readiness waits. It is imported from the daemon, cli, and mcp test
// packages so every "wait for the daemon to come ready" site uses one
// polling loop with a generous, deadline-aware ceiling instead of a
// fixed timeout that flakes on loaded CI runners.
//
// This package carries no production code and is only referenced from
// _test.go files. It lives outside those files so a single helper can
// be shared across the three packages whose tests spin up a daemon.
package testsupport

import (
	"testing"
	"time"
)

// defaultReadyCeiling bounds a readiness wait when the test sets no
// deadline or when the package deadline is farther away. It is generous:
// the assertion is "the daemon eventually comes ready", and a slow,
// heavily loaded runner is not a product failure. The poll exits the
// instant the predicate passes, so this ceiling only governs the
// pathological case where readiness never arrives.
const defaultReadyCeiling = 30 * time.Second

// readyTick is how often the readiness predicate is polled. Small
// enough that a ready daemon is observed promptly, large enough to
// avoid busy-spinning the dialer.
const readyTick = 25 * time.Millisecond

// deadlineMargin is reserved before t.Deadline() so the readiness wait
// fails with its own descriptive message rather than being killed by
// the test runner's panic-on-timeout, which would obscure the cause.
const deadlineMargin = 2 * time.Second

// WaitReady polls ready until it returns true, then returns. If the
// ceiling elapses first it fails the test via t.Fatalf with what,
// describing the readiness signal that never arrived.
//
// The ceiling is capped at 30s and shortened by t.Deadline() when needed,
// reserving a margin so this helper, not the runner, reports the failure.
// Polling is on a fixed tick; the wait returns as soon as ready
// reports true, so a healthy daemon incurs at most one tick of delay.
//
// ready is the daemon's own readiness signal, e.g. "socket dialable
// AND daemon.json present", so the wait is meaningful rather than a
// blind sleep. It must be safe to call repeatedly.
func WaitReady(t *testing.T, what string, ready func() bool) {
	t.Helper()

	deadline := time.Now().Add(ceiling(t))
	tick := time.NewTicker(readyTick)
	defer tick.Stop()

	for {
		if ready() {
			return
		}
		if time.Now().After(deadline) {
			// One last check closes the race where readiness landed
			// between the predicate poll and the deadline crossing.
			if ready() {
				return
			}
			t.Fatalf("daemon did not become ready before the readiness ceiling: %s", what)
			return
		}
		<-tick.C
	}
}

// ceiling caps every wait independently of the package timeout while keeping
// a margin for reporting the failure before the test runner's deadline.
func ceiling(t *testing.T) time.Duration {
	deadline, ok := t.Deadline()
	return readinessBudget(time.Now(), deadline, ok)
}

func readinessBudget(now, deadline time.Time, hasDeadline bool) time.Duration {
	if !hasDeadline {
		return defaultReadyCeiling
	}
	budget := deadline.Sub(now) - deadlineMargin
	if budget < readyTick {
		budget = readyTick
	}
	return min(budget, defaultReadyCeiling)
}
