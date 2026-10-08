package sleep

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/mathomhaus/guild/internal/lore"
)

// autopass.go is the degraded-mode entrypoint for the sleep cycle: the
// once-per-process dream pass an in-process MCP server fires when no
// daemon is resident to own the idle scheduler (ADR-005 Part 2,
// degraded fallback row). It is the sibling of the daemon idle
// scheduler's pass path: both build a PassContext and call Run against
// the same registered Steps under the same HYBRID policy and journaling.
// The two differences are intentional and live here:
//
//   - Trigger is TriggerAutopass (recorded on the sleep_passes row), so
//     narration and audit can tell a degraded pass from a daemon pass.
//   - A throttle gate: an autopass steals cycles from an active session's
//     machine, so it must not fire if a recent pass (from either trigger)
//     already did the work. A daemon that was briefly stopped and is now
//     gone should not cause the next session-start to re-dream.
//
// The package stays transport-neutral (no internal/daemon or
// internal/mcp import): the caller in internal/mcp owns the daemon
// liveness gate and the sync.Once that makes this fire at most once per
// process. This function owns only the journal-throttle decision and the
// pass execution, because the journal is this package's table.

// AutopassConfig parameterizes one degraded-mode pass. The caller (the
// in-process MCP server) supplies the db handles, embed deps, budget,
// caps, and the throttle interval; everything time-sensitive is
// injectable so tests drive the throttle math without sleeping.
type AutopassConfig struct {
	// LoreDB is the lore.db handle. Required: it carries the sleep
	// journal the throttle reads and the runner writes.
	LoreDB *sql.DB

	// QuestDB is the quest.db handle for steps that post quests
	// (renewal, approval). Optional; steps that need it check for nil.
	QuestDB *sql.DB

	// Embed carries the embedding-pipeline dependencies for the embed
	// step. nil or disabled means BM25-only behavior, the same contract
	// as everywhere else in internal/lore.
	Embed *lore.EmbedDeps

	// Budget is the wall budget for the pass. The caller sets this
	// tighter than the daemon's idle-pass budget because an autopass runs
	// while a session is active. A non-positive budget is a usage error.
	Budget time.Duration

	// Caps are the per-pass mutation guardrails threaded into every step.
	// The caller sets these tighter than the daemon's idle caps for the
	// same reason as Budget.
	Caps Caps

	// MinInterval is the throttle window: if the most recent pass ended
	// within this duration, Autopass skips entirely (ran=false). Zero
	// disables the throttle (every call runs a pass), which is only
	// appropriate for tests.
	MinInterval time.Duration

	// Logger receives structured diagnostics. nil defaults to
	// slog.Default().
	Logger *slog.Logger

	// Now supplies the current time for the throttle comparison,
	// swappable for deterministic tests. nil defaults to time.Now().UTC.
	Now func() time.Time
}

// Autopass runs at most one bounded degraded-mode dream pass and reports
// whether a pass actually ran.
//
// Gate order:
//
//  1. Validate: a nil lore db or non-positive budget is a usage error.
//  2. Throttle: if MinInterval > 0 and the journal's most recent pass
//     ended less than MinInterval ago, skip and return (nil, false, nil).
//     A journal read error (e.g. a pre-migration DB with no sleep_passes
//     table) is treated as "no prior pass, nothing to throttle against"
//     so a fresh install still gets its first drip of consolidation.
//  3. Run the registered Steps under Budget with TriggerAutopass.
//
// On a pass that ran, the returned bool is true and the *PassResult is
// the runner's record (nil only on a substrate failure, in which case
// err is non-nil). The caller treats a (nil, false, nil) return as a
// throttled no-op, not a failure.
func Autopass(ctx context.Context, cfg AutopassConfig) (*PassResult, bool, error) {
	if cfg.LoreDB == nil {
		return nil, false, fmt.Errorf("sleep: autopass: nil lore db")
	}
	if cfg.Budget <= 0 {
		return nil, false, fmt.Errorf("sleep: autopass: non-positive budget %v", cfg.Budget)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	if cfg.MinInterval > 0 {
		ended, ok, err := LastPassEndedAt(ctx, cfg.LoreDB)
		switch {
		case err != nil:
			// A read failure (locked DB, missing sleep_passes table on a
			// pre-migration install) is not fatal: with no observable prior
			// pass there is nothing to throttle against, so fall through and
			// let the pass run. The runner's own BeginPass will surface a
			// genuine substrate failure as an error.
			logger.Debug("sleep autopass: last-pass probe failed; not throttling",
				slog.String("err", err.Error()),
			)
		case ok:
			if elapsed := now().Sub(ended); elapsed < cfg.MinInterval {
				logger.Debug("sleep autopass: throttled",
					slog.Duration("since_last_pass", elapsed),
					slog.Duration("min_interval", cfg.MinInterval),
				)
				return nil, false, nil
			}
		}
	}

	pc := &PassContext{
		LoreDB:  cfg.LoreDB,
		QuestDB: cfg.QuestDB,
		Embed:   cfg.Embed,
		Logger:  logger,
		Caps:    cfg.Caps,
		Now:     now,
		Trigger: TriggerAutopass,
	}

	res, err := Run(ctx, pc, Steps(), cfg.Budget)
	if err != nil {
		return res, true, fmt.Errorf("sleep: autopass: %w", err)
	}
	return res, true, nil
}
