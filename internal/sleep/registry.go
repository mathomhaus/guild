package sleep

import "sync"

// The step registry is the seam between the dream-step quests (which
// implement Step) and the schedulers that drive them (the daemon idle
// scheduler in internal/daemon, and the degraded in-process autopass).
// A step file in this package registers its Step at init; a scheduler
// asks Steps() for the ordered set to run under one pass.
//
// This PR ships the registry empty: no step file calls RegisterStep
// yet, so Steps() returns nil and a fired pass journals a pass row with
// zero steps. The step quests in this campaign add their RegisterStep
// calls without touching the scheduler.

var (
	registryMu sync.RWMutex
	registry   []Step
)

// RegisterStep adds s to the pass-step registry. It is meant to be
// called once per step from package init (or an explicit setup path in
// tests), so the registered order is the file-load order, which the
// runner then executes deterministically. A nil step is ignored.
//
// Registration is process-global on purpose: the steps are stateless
// drivers parameterized entirely by the per-pass PassContext, so one
// registered set serves every scheduler in the process.
func RegisterStep(s Step) {
	if s == nil {
		return
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = append(registry, s)
}

// Steps returns a snapshot of the registered steps in registration
// order. The returned slice is a copy, so a caller may hand it to Run
// (which does not mutate it) without racing a concurrent RegisterStep.
// Returns nil when nothing is registered.
func Steps() []Step {
	registryMu.RLock()
	defer registryMu.RUnlock()
	if len(registry) == 0 {
		return nil
	}
	out := make([]Step, len(registry))
	copy(out, registry)
	return out
}
