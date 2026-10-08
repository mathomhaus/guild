package mcp

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/config"
	"github.com/mathomhaus/guild/internal/sleep"
	"github.com/mathomhaus/guild/internal/storage"
)

// autopassTestEnv wires the package-level seams the sleep-autopass gate
// reads so a test runs hermetically against temp DBs with sleep enabled
// and no daemon resident. It returns the lore.db path so the test can
// inspect the journal. Every seam is restored in t.Cleanup.
func autopassTestEnv(t *testing.T) (loreDBPath string) {
	t.Helper()

	resetSleepAutopassState()
	t.Cleanup(resetSleepAutopassState)

	tmp := t.TempDir()
	loreDBPath = filepath.Join(tmp, "lore.db")
	questDBPath := filepath.Join(tmp, "quest.db")
	seedAutopassLoreDB(t, loreDBPath)
	seedAutopassQuestDB(t, questDBPath)

	origLdb, origQdb := ldbPath, qdbPath
	ldbPath = func() (string, error) { return loreDBPath, nil }
	qdbPath = func() (string, error) { return questDBPath, nil }
	t.Cleanup(func() { ldbPath, qdbPath = origLdb, origQdb })

	origCfg := sleepAutopassConfigLoad
	sleepAutopassConfigLoad = func() config.SleepConfig { return config.SleepConfig{Enabled: true} }
	t.Cleanup(func() { sleepAutopassConfigLoad = origCfg })

	origProbe := sleepAutopassDaemonRunning
	sleepAutopassDaemonRunning = func() bool { return false }
	t.Cleanup(func() { sleepAutopassDaemonRunning = origProbe })

	return loreDBPath
}

// seedAutopassLoreDB creates a migrated lore.db so the sleep journal
// tables exist. quietTestLogger keeps the open quiet.
func seedAutopassLoreDB(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open lore db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := storage.Migrate(ctx, db, "lore"); err != nil {
		t.Fatalf("migrate lore: %v", err)
	}
}

func seedAutopassQuestDB(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := storage.Migrate(ctx, db, "quest"); err != nil {
		t.Fatalf("migrate quest: %v", err)
	}
}

func quietTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(newSafeBuffer(), nil))
}

// countAutopasses returns how many sleep_passes rows carry trigger
// 'autopass' in the lore.db at path.
func countAutopasses(t *testing.T, path string) int {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open lore db: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sleep_passes WHERE "trigger" = ?`, string(sleep.TriggerAutopass),
	).Scan(&n); err != nil {
		t.Fatalf("count autopasses: %v", err)
	}
	return n
}

// lastAutopassBudgetMS returns the budget_ms of the most recent autopass
// row, or -1 when none exists.
func lastAutopassBudgetMS(t *testing.T, path string) int64 {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open lore db: %v", err)
	}
	defer func() { _ = db.Close() }()
	var ms int64
	err = db.QueryRowContext(ctx,
		`SELECT budget_ms FROM sleep_passes WHERE "trigger" = ? ORDER BY id DESC LIMIT 1`,
		string(sleep.TriggerAutopass),
	).Scan(&ms)
	if err == sql.ErrNoRows {
		return -1
	}
	if err != nil {
		t.Fatalf("last autopass budget: %v", err)
	}
	return ms
}

// waitDone blocks until the gate's pass goroutine finishes or the test
// deadline elapses.
func waitDone(t *testing.T, g *sleepAutopassGate) {
	t.Helper()
	if g.doneCh == nil {
		t.Fatalf("gate doneCh is nil: trigger did not fire")
	}
	select {
	case <-g.doneCh:
	case <-time.After(15 * time.Second):
		t.Fatalf("autopass did not finish within 15s")
	}
}

// TestSleepAutopass_ExactlyOncePerProcess is the core sync.Once gate:
// ten racing triggers (mimicking ten near-simultaneous session_starts)
// fire exactly one background pass, recorded once with trigger
// 'autopass'.
func TestSleepAutopass_ExactlyOncePerProcess(t *testing.T) {
	loreDBPath := autopassTestEnv(t)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			maybeTriggerSleepAutopass(nil, quietTestLogger())
		}()
	}
	wg.Wait()

	waitDone(t, processSleepAutopassGate)

	if n := countAutopasses(t, loreDBPath); n != 1 {
		t.Errorf("autopass count = %d, want exactly 1 (sync.Once leak)", n)
	}
}

// TestSleepAutopass_NeverBlocksResponse proves the handler-path contract:
// the trigger returns immediately even when the background pass is still
// running slow work. The goroutine hook stands in for the slow step the
// acceptance describes; the pass cannot get past the barrier until the
// test releases it, yet maybeTriggerSleepAutopass returns promptly.
func TestSleepAutopass_NeverBlocksResponse(t *testing.T) {
	loreDBPath := autopassTestEnv(t)

	release := make(chan struct{})
	sleepAutopassGoroutineHook = func() { <-release }
	t.Cleanup(func() { sleepAutopassGoroutineHook = nil })

	returned := make(chan struct{})
	go func() {
		maybeTriggerSleepAutopass(nil, quietTestLogger())
		close(returned)
	}()

	select {
	case <-returned:
		// Good: the trigger returned while the pass is still blocked.
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatalf("maybeTriggerSleepAutopass blocked on the background pass")
	}

	// The pass must not have journaled anything yet: it is still parked
	// at the barrier.
	if n := countAutopasses(t, loreDBPath); n != 0 {
		t.Errorf("autopass journaled %d passes before release, want 0", n)
	}

	// Now let the pass proceed and confirm it completes and journals.
	close(release)
	waitDone(t, processSleepAutopassGate)
	if n := countAutopasses(t, loreDBPath); n != 1 {
		t.Errorf("autopass count = %d, want 1 after release", n)
	}
}

// TestSleepAutopass_ThrottledByRecentPass proves the throttle: a pass
// that ended minutes ago suppresses the autopass entirely, so a
// briefly-stopped daemon does not cause the next session to re-dream.
func TestSleepAutopass_ThrottledByRecentPass(t *testing.T) {
	loreDBPath := autopassTestEnv(t)

	// Seed a daemon-idle pass that ended one minute ago, well inside the
	// six-hour throttle window.
	ctx := context.Background()
	db, err := storage.Open(ctx, loreDBPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().UTC()
	id, err := sleep.BeginPass(ctx, db, sleep.TriggerDaemonIdle, time.Minute, now.Add(-2*time.Minute))
	if err != nil {
		t.Fatalf("BeginPass: %v", err)
	}
	if err := sleep.EndPass(ctx, db, id, now.Add(-time.Minute)); err != nil {
		t.Fatalf("EndPass: %v", err)
	}
	_ = db.Close()

	maybeTriggerSleepAutopass(nil, quietTestLogger())
	waitDone(t, processSleepAutopassGate)

	if n := countAutopasses(t, loreDBPath); n != 0 {
		t.Errorf("autopass count = %d, want 0 (recent pass must throttle)", n)
	}
}

// TestSleepAutopass_TighterBudgetAndTrigger proves the pass runs under
// the tight autopass budget (10s, far below the daemon default 60s) and
// is journaled with trigger 'autopass'.
func TestSleepAutopass_TighterBudgetAndTrigger(t *testing.T) {
	loreDBPath := autopassTestEnv(t)

	maybeTriggerSleepAutopass(nil, quietTestLogger())
	waitDone(t, processSleepAutopassGate)

	if got := lastAutopassBudgetMS(t, loreDBPath); got != sleepAutopassBudget.Milliseconds() {
		t.Errorf("budget_ms = %d, want %d (tight autopass budget)", got, sleepAutopassBudget.Milliseconds())
	}
	// Sanity: the autopass budget really is tighter than the daemon's
	// configured idle-pass budget default (config.SleepConfig default
	// pass_budget_seconds is 60s).
	const daemonDefaultBudget = 60 * time.Second
	if sleepAutopassBudget >= daemonDefaultBudget {
		t.Errorf("autopass budget %v is not tighter than daemon budget %v", sleepAutopassBudget, daemonDefaultBudget)
	}
}

// TestSleepAutopass_DisabledByConfig proves [sleep] enabled=false keeps
// the in-process path byte-identical to today: no goroutine fires, no
// pass row is written.
func TestSleepAutopass_DisabledByConfig(t *testing.T) {
	loreDBPath := autopassTestEnv(t)
	sleepAutopassConfigLoad = func() config.SleepConfig { return config.SleepConfig{Enabled: false} }

	maybeTriggerSleepAutopass(nil, quietTestLogger())

	if processSleepAutopassGate.doneCh != nil {
		t.Errorf("doneCh non-nil: disabled config still fired the gate")
	}
	if n := countAutopasses(t, loreDBPath); n != 0 {
		t.Errorf("autopass count = %d, want 0 when disabled", n)
	}
}

// TestSleepAutopass_DisabledByEnv proves GUILD_NO_SLEEP disables the
// autopass through the real config loader (no seam override): the merged
// config folds the env var into Sleep.Enabled=false.
func TestSleepAutopass_DisabledByEnv(t *testing.T) {
	loreDBPath := autopassTestEnv(t)
	// Drop the config seam so the real loader runs, then set the env.
	sleepAutopassConfigLoad = origSleepAutopassConfigLoad
	t.Setenv("GUILD_NO_SLEEP", "1")

	maybeTriggerSleepAutopass(nil, quietTestLogger())

	if processSleepAutopassGate.doneCh != nil {
		t.Errorf("doneCh non-nil: GUILD_NO_SLEEP still fired the gate")
	}
	if n := countAutopasses(t, loreDBPath); n != 0 {
		t.Errorf("autopass count = %d, want 0 with GUILD_NO_SLEEP=1", n)
	}
}

// TestSleepAutopass_SuppressedWhenDaemonRunning proves a resident daemon
// (which owns the idle scheduler) suppresses the in-process autopass so
// the two never double-dream.
func TestSleepAutopass_SuppressedWhenDaemonRunning(t *testing.T) {
	loreDBPath := autopassTestEnv(t)
	sleepAutopassDaemonRunning = func() bool { return true }

	maybeTriggerSleepAutopass(nil, quietTestLogger())

	if processSleepAutopassGate.doneCh != nil {
		t.Errorf("doneCh non-nil: a running daemon still fired the autopass")
	}
	if n := countAutopasses(t, loreDBPath); n != 0 {
		t.Errorf("autopass count = %d, want 0 when a daemon is running", n)
	}
}

// TestSleepAutopass_ResetGivesFreshGate proves resetSleepAutopassState
// re-arms the once-guard so a subsequent test-spawned server can fire
// again (mirrors the auto-backfill reset contract). The gate's sync.Once
// is what reset clears; the throttle is independent and lives inside the
// fired goroutine, so this test asserts the goroutine fired both times
// (a fresh doneCh) rather than counting journal rows.
func TestSleepAutopass_ResetGivesFreshGate(t *testing.T) {
	autopassTestEnv(t)

	maybeTriggerSleepAutopass(nil, quietTestLogger())
	first := processSleepAutopassGate.doneCh
	waitDone(t, processSleepAutopassGate)

	// Firing again without a reset is a spent Once: no new goroutine, so
	// the gate keeps the same (already-closed) doneCh.
	maybeTriggerSleepAutopass(nil, quietTestLogger())
	if processSleepAutopassGate.doneCh != first {
		t.Errorf("spent gate fired again without a reset")
	}

	// After a reset the next trigger fires a fresh goroutine on a new
	// doneCh.
	resetSleepAutopassState()
	maybeTriggerSleepAutopass(nil, quietTestLogger())
	second := processSleepAutopassGate.doneCh
	if second == nil || second == first {
		t.Errorf("reset did not arm a fresh gate: doneCh=%v first=%v", second, first)
	}
	waitDone(t, processSleepAutopassGate)
}

// origSleepAutopassConfigLoad preserves the production config loader so a
// test can restore it after overriding the seam in autopassTestEnv.
var origSleepAutopassConfigLoad = sleepAutopassConfigLoad
