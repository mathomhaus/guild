package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/lore/embed"
	"github.com/mathomhaus/guild/internal/storage"
)

// TestAutoBackfill_EndToEnd is the QUEST-229 regression gate: a post-
// upgrade user boots an MCP server against a DB where lore has pending
// entries and quest_vectors is entirely empty. The provider's first
// wired resolve fires maybeTriggerAutoBackfill; within seconds the
// goroutines drive both corpora to full coverage.
//
// The test uses a DeterministicEmbedder so it works on default builds
// (no -tags=withembed required) and seeds both DBs through the same
// migration path production uses.
func TestAutoBackfill_EndToEnd(t *testing.T) {
	tmp := t.TempDir()
	loreDBPath := filepath.Join(tmp, "lore.db")
	questDBPath := filepath.Join(tmp, "quest.db")

	// Reset package-level state so this test sees a clean once-guard.
	resetAutoBackfillState()
	t.Cleanup(resetAutoBackfillState)

	// Redirect the openLoreDB / openQuestDB path resolvers so the
	// init()-registered targets find our temp DBs.
	origLdb := ldbPath
	origQdb := qdbPath
	ldbPath = func() (string, error) { return loreDBPath, nil }
	qdbPath = func() (string, error) { return questDBPath, nil }
	t.Cleanup(func() {
		ldbPath = origLdb
		qdbPath = origQdb
	})

	// Seed lore.db: 5 pending entries, embedder_state='enabled'.
	seedLoreForAutoBackfill(t, loreDBPath, 5)

	// Seed quest.db: 3 task_status rows with [spec] notes. quest_vectors
	// starts empty (the whole point of QUEST-229).
	seedQuestForAutoBackfill(t, questDBPath, 3)

	// Pin a JSON-handler slog to a buffer so we can assert the
	// "auto-backfill started" / "auto-backfill complete" lines.
	var logBuf safeBuffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// Construct a *lore.EmbedDeps by hand. In production the provider
	// reconstruct path returns one of these via lore.WireEmbedDeps +
	// PrepareAndProbe; for the test we synthesize an equivalent deps
	// carrying a DeterministicEmbedder so the default-build test runner
	// does not need bundled ORT assets.
	deps := &lore.EmbedDeps{
		Embedder: embed.NewDeterministicEmbedder(),
		ModelID:  "bge-small-en-v1.5-int8-cls",
		Async:    true,
		Logger:   logger,
	}

	// Fire the trigger. Non-blocking: returns immediately.
	start := time.Now()
	maybeTriggerAutoBackfill(deps, logger)

	// Wait for completion (bounded). The sync.WaitGroup inside
	// runAutoBackfill closes autoBackfillDoneCh when every per-corpus
	// goroutine finishes.
	select {
	case <-autoBackfillDoneCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("auto-backfill did not finish within 30s; logs:\n%s", logBuf.String())
	}
	totalElapsed := time.Since(start)
	t.Logf("auto-backfill total wall time: %s", totalElapsed)

	// Assert: lore coverage == 1.0 (5/5).
	assertCorpusCoverage(t, loreDBPath, embed.LoreCorpus{}, 5, 5)
	// Assert: quest coverage == 1.0 (3/3). QuestCorpus coverage_den is
	// reconciled by Backfill's ReconcileDen step; we seeded 3 rows and
	// expect 3 indexed.
	assertCorpusCoverage(t, questDBPath, embed.QuestCorpus{}, 3, 3)

	// Assert: slog samples contain the started+complete lines for both
	// corpora. Parse each JSON line and accumulate the (msg, corpus)
	// pairs.
	got := collectAutoBackfillEvents(t, logBuf.String())
	mustHaveEvent(t, got, "auto-backfill started", "lore")
	mustHaveEvent(t, got, "auto-backfill started", "quest")
	mustHaveEvent(t, got, "auto-backfill complete", "lore")
	mustHaveEvent(t, got, "auto-backfill complete", "quest")

	// Print a sample transcript so the quest_fulfill report can cite
	// the slog text verbatim.
	t.Logf("slog sample:\n%s", logBuf.String())
}

// TestAutoBackfill_ExactlyOnce proves the sync.Once gate: ten concurrent
// triggers produce exactly one goroutine per corpus, not ten.
func TestAutoBackfill_ExactlyOnce(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	loreDBPath := filepath.Join(tmp, "lore.db")
	questDBPath := filepath.Join(tmp, "quest.db")

	resetAutoBackfillState()
	t.Cleanup(resetAutoBackfillState)

	origLdb := ldbPath
	origQdb := qdbPath
	ldbPath = func() (string, error) { return loreDBPath, nil }
	qdbPath = func() (string, error) { return questDBPath, nil }
	t.Cleanup(func() {
		ldbPath = origLdb
		qdbPath = origQdb
	})

	seedLoreForAutoBackfill(t, loreDBPath, 2)
	seedQuestForAutoBackfill(t, questDBPath, 2)

	// Counting embedder: wraps DeterministicEmbedder and atomically
	// increments on every Embed call. Exactly-once trigger means the
	// counter hits (pending_lore + pending_quest) once, not twice.
	counted := &countingEmbedder{inner: embed.NewDeterministicEmbedder()}

	logger := slog.New(slog.NewJSONHandler(newSafeBuffer(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	deps := &lore.EmbedDeps{
		Embedder: counted,
		ModelID:  "bge-small-en-v1.5-int8-cls",
		Async:    true,
		Logger:   logger,
	}

	// Race ten goroutines through maybeTriggerAutoBackfill. sync.Once
	// must serialize them.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			maybeTriggerAutoBackfill(deps, logger)
		}()
	}
	wg.Wait()

	select {
	case <-autoBackfillDoneCh:
	case <-time.After(10 * time.Second):
		t.Fatalf("auto-backfill did not finish within 10s")
	}

	// Expected encodes: 2 lore + 2 quest = 4. Anything higher is a
	// sync.Once regression.
	got := counted.count.Load()
	if got != 4 {
		t.Errorf("embed count: got %d, want 4 (2 lore + 2 quest). sync.Once may be leaking", got)
	}
	_ = ctx
}

// TestAutoBackfill_NoOpWhenCovered verifies the gate: a corpus already
// at >= 0.90 coverage with zero pending entries does not spawn a
// goroutine and does not emit the "auto-backfill started" line.
func TestAutoBackfill_NoOpWhenCovered(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	loreDBPath := filepath.Join(tmp, "lore.db")
	questDBPath := filepath.Join(tmp, "quest.db")

	resetAutoBackfillState()
	t.Cleanup(resetAutoBackfillState)

	origLdb := ldbPath
	origQdb := qdbPath
	ldbPath = func() (string, error) { return loreDBPath, nil }
	qdbPath = func() (string, error) { return questDBPath, nil }
	t.Cleanup(func() {
		ldbPath = origLdb
		qdbPath = origQdb
	})

	// Seed lore with zero pending entries and coverage=1/1.
	seedEmptyLoreAlreadyCovered(t, loreDBPath)
	// Seed quest with zero pending entities.
	seedEmptyQuestAlreadyCovered(t, questDBPath)

	var logBuf safeBuffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	deps := &lore.EmbedDeps{
		Embedder: embed.NewDeterministicEmbedder(),
		ModelID:  "bge-small-en-v1.5-int8-cls",
		Logger:   logger,
	}

	maybeTriggerAutoBackfill(deps, logger)
	select {
	case <-autoBackfillDoneCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("auto-backfill did not finish within 5s")
	}
	if strings.Contains(logBuf.String(), `"auto-backfill started"`) {
		t.Errorf("expected silence for already-covered corpora; got started line:\n%s", logBuf.String())
	}
	_ = ctx
}

// ----- helpers --------------------------------------------------------

// safeBuffer is a concurrency-safe bytes.Buffer wrapper. Multiple
// goroutines log into the same buffer during auto-backfill, so the test
// logger needs a synchronized writer. bytes.Buffer.Write alone is not
// goroutine-safe.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newSafeBuffer() *safeBuffer { return &safeBuffer{} }

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// countingEmbedder wraps an Embedder and counts calls atomically. Used
// to prove sync.Once collapses N concurrent triggers to one pass.
type countingEmbedder struct {
	inner embed.Embedder
	count atomic.Int64
}

func (c *countingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	c.count.Add(1)
	return c.inner.Embed(ctx, text)
}

func (c *countingEmbedder) Dimension() int { return c.inner.Dimension() }

func seedLoreForAutoBackfill(t *testing.T, path string, pendingCount int) {
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
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO projects (id, path) VALUES ('p', '/tmp/p')`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	// Insert pending entries.
	for i := 1; i <= pendingCount; i++ {
		_, err := db.ExecContext(ctx,
			`INSERT INTO entries
			   (project_id, topic, kind, title, summary, status, vector_state, created_at, updated_at)
			 VALUES ('p', 'topic', 'observation', 'title', ?, 'current', 'pending', datetime('now'), datetime('now'))`,
			"summary text "+strings.Repeat("x", i),
		)
		if err != nil {
			t.Fatalf("seed entry %d: %v", i, err)
		}
	}
	// Mark lore embedder as enabled so the per-corpus promotion path
	// recognizes the state is already good and skips the identity
	// rewrite.
	upsertMeta(t, db, "embedder_state", "enabled")
	upsertMeta(t, db, "embedder_model_id", "bge-small-en-v1.5-int8-cls")
	upsertMeta(t, db, "vector_coverage_num", "0")
	upsertMeta(t, db, "vector_coverage_den", "0")
}

func seedQuestForAutoBackfill(t *testing.T, path string, count int) {
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
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO projects (id, path) VALUES ('p', '/tmp/p')`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	for i := 1; i <= count; i++ {
		taskID := "QUEST-AB" + strings.Repeat("0", 3-len(intToStr(i))) + intToStr(i)
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO task_status (project_id, task_id, status) VALUES ('p', ?, 'next')`,
			taskID,
		); err != nil {
			t.Fatalf("seed task_status %s: %v", taskID, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO task_notes (project_id, task_id, agent_id, note)
			 VALUES ('p', ?, 'test', ?)`,
			taskID, "[spec] subject: auto-backfill test quest "+intToStr(i),
		); err != nil {
			t.Fatalf("seed task_notes %s: %v", taskID, err)
		}
		// Trigger tasks_fts_status_ai populates tasks_fts_rows; guard
		// with INSERT OR IGNORE so an already-populated row is fine.
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO tasks_fts_rows (task_id) VALUES (?)`,
			taskID,
		); err != nil {
			t.Fatalf("seed tasks_fts_rows %s: %v", taskID, err)
		}
	}
	// Leave quest.embedder_state at its migration-005 default
	// ('disabled'); the finalizeCorpusState helper in the auto-backfill
	// path must promote it after the bounded inner loop. This mirrors
	// the upgrade scenario QUEST-229 targets.
}

func seedEmptyLoreAlreadyCovered(t *testing.T, path string) {
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
	upsertMeta(t, db, "embedder_state", "enabled")
	upsertMeta(t, db, "embedder_model_id", "bge-small-en-v1.5-int8-cls")
	upsertMeta(t, db, "vector_coverage_num", "0")
	upsertMeta(t, db, "vector_coverage_den", "0")
}

func seedEmptyQuestAlreadyCovered(t *testing.T, path string) {
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
	// No tasks_fts_rows seeded; coverage_den stays zero via migration
	// seeds. assessCorpus treats den==0 as fully covered.
}

func upsertMeta(t *testing.T, db *sql.DB, key, value string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		t.Fatalf("upsertMeta(%s=%s): %v", key, value, err)
	}
}

func assertCorpusCoverage(t *testing.T, path string, corpus embed.VectorCorpus, wantNum, wantDen int64) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open db for coverage assert: %v", err)
	}
	defer func() { _ = db.Close() }()

	var num, den int64
	for _, row := range []struct {
		field embed.MetaField
		dst   *int64
	}{
		{embed.FieldVectorCoverageNum, &num},
		{embed.FieldVectorCoverageDen, &den},
	} {
		var v string
		err := db.QueryRowContext(ctx,
			`SELECT value FROM meta WHERE key = ?`, corpus.MetaKey(row.field),
		).Scan(&v)
		if err != nil && err != sql.ErrNoRows {
			t.Fatalf("read coverage %v: %v", corpus.MetaKey(row.field), err)
		}
		if v == "" {
			continue
		}
		n, perr := parseInt64(v)
		if perr != nil {
			t.Fatalf("parse coverage %s=%q: %v", corpus.MetaKey(row.field), v, perr)
		}
		*row.dst = n
	}
	if num != wantNum || den != wantDen {
		t.Errorf("%s coverage: got %d/%d, want %d/%d", corpus.Name(), num, den, wantNum, wantDen)
	}
}

type autoBackfillEvent struct {
	Msg        string `json:"msg"`
	CorpusName string `json:"corpus_name"`
}

func collectAutoBackfillEvents(t *testing.T, logs string) []autoBackfillEvent {
	t.Helper()
	var out []autoBackfillEvent
	for _, line := range strings.Split(strings.TrimRight(logs, "\n"), "\n") {
		if line == "" {
			continue
		}
		var ev autoBackfillEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if strings.HasPrefix(ev.Msg, "auto-backfill ") {
			out = append(out, ev)
		}
	}
	return out
}

func mustHaveEvent(t *testing.T, events []autoBackfillEvent, msg, corpus string) {
	t.Helper()
	for _, ev := range events {
		if ev.Msg == msg && ev.CorpusName == corpus {
			return
		}
	}
	t.Errorf("missing event: msg=%q corpus=%q; got events=%+v", msg, corpus, events)
}

// TestAutoBackfill_QUEST248_BoundedInnerLoopConverges is the QUEST-248
// regression gate. Reproduces the LORE-416 production-shape failure
// (transient writer-lock contention, ~70% of insertVectorRow calls fail
// per cycle) and proves the bounded inner loop converges where
// single-pass orchestration would not.
//
// Design choice: Option 1 from the QUEST-248 spec. Inject a fail-
// intermittently shim into BackfillOptions.InsertHook via the
// autoBackfillInsertHook test seam in embed_autobackfill.go. Option 1
// beats Option 2 (concurrent goroutines + real lock contention) on CI
// determinism: the failure budget is exact, the embedder is the
// deterministic stub, and convergence is a function of the iteration
// cap rather than wall-clock timing.
//
// Three sub-cases share the seed shape:
//
//  1. PartialFirstCycle: cycle 1 fails ~70% of inserts; subsequent
//     cycles succeed. Assert state_reason='auto_backfill_promoted' and
//     coverage clears the floor.
//
//  2. AlwaysFail: every cycle fails 100% of inserts. Assert
//     state_reason='auto_backfill_no_progress' (zero rows ever landed).
//
//  3. PartialOnlyEverySucceeds: every cycle succeeds ~30% of inserts
//     but never enough to clear the floor (the cap exhausts before
//     coverage >= 0.90). Assert state_reason='auto_backfill_partial'.
func TestAutoBackfill_QUEST248_BoundedInnerLoopConverges(t *testing.T) {
	t.Run("PartialFirstCycle", func(t *testing.T) {
		runQUEST248InsertHookCase(t, quest248Case{
			pendingCount: 200,
			// Hook policy: fail the first 140 invocations (cycle 1's
			// failure budget); succeed every invocation after that. The
			// LEFT JOIN scan in cycle 2 picks up the 140 that failed in
			// cycle 1, and they all succeed on the retry.
			policy: func(invocation int64) bool {
				return invocation > 140
			},
			wantStateReason:        stateReasonAutoBackfillPromoted,
			wantCoverageAtLeast:    0.90,
			wantVectorCountAtLeast: 180,
		})
	})

	t.Run("AlwaysFail", func(t *testing.T) {
		runQUEST248InsertHookCase(t, quest248Case{
			pendingCount: 50,
			// Always fail: hook never lets a row through. The bounded
			// loop sees res.Embedded == 0 in cycle 1 and breaks (no-
			// progress exit), so the cap is not actually hit; the
			// state_reason still reflects "no progress" because zero
			// vectors landed across the whole run.
			policy:                 func(invocation int64) bool { return false },
			wantStateReason:        stateReasonAutoBackfillNoProgress,
			wantCoverageAtMost:     0.0,
			wantVectorCountAtMost:  0,
			wantVectorCountAtLeast: 0,
		})
	})

	t.Run("PartialNeverConverges", func(t *testing.T) {
		// Succeed exactly 1 in every 10 invocations across all cycles.
		// pendingCount=200 means cycle 1 lands ~20 vectors, cycle 2
		// scans the remaining 180 and lands ~18, etc. The cap of 5
		// iterations limits how far this can go; coverage stays below
		// 0.90 but is strictly positive, so state_reason should be
		// auto_backfill_partial.
		runQUEST248InsertHookCase(t, quest248Case{
			pendingCount: 200,
			policy: func(invocation int64) bool {
				// Succeed only on every 10th invocation.
				return invocation%10 == 0
			},
			wantStateReason:    stateReasonAutoBackfillPartial,
			wantCoverageAtMost: 0.89,
			// At least the first cap-bounded handful of cycles each
			// land a few vectors, so positive but well below the floor.
			wantVectorCountAtLeast: 1,
		})
	})
}

// quest248Case configures one sub-case of the QUEST-248 regression
// test. Zero-value fields are ignored by the assertions.
type quest248Case struct {
	pendingCount int
	// policy returns true when the invocation should succeed (call the
	// real insertVectorRow), false when it should fail with a synthetic
	// error that mimics BEGIN IMMEDIATE retry exhaustion.
	policy                 func(invocation int64) bool
	wantStateReason        string
	wantCoverageAtLeast    float64
	wantCoverageAtMost     float64
	wantVectorCountAtLeast int64
	wantVectorCountAtMost  int64
}

func runQUEST248InsertHookCase(t *testing.T, tc quest248Case) {
	t.Helper()
	tmp := t.TempDir()
	loreDBPath := filepath.Join(tmp, "lore.db")
	questDBPath := filepath.Join(tmp, "quest.db")

	resetAutoBackfillState()
	t.Cleanup(resetAutoBackfillState)

	origLdb := ldbPath
	origQdb := qdbPath
	ldbPath = func() (string, error) { return loreDBPath, nil }
	qdbPath = func() (string, error) { return questDBPath, nil }
	t.Cleanup(func() {
		ldbPath = origLdb
		qdbPath = origQdb
	})

	seedEmptyLoreAlreadyCovered(t, loreDBPath)
	seedQuestHistoricalRows(t, questDBPath, tc.pendingCount)

	// Install the fail-intermittently shim. Call counter is a captured
	// atomic so the policy callback is goroutine-safe (Backfill itself
	// is single-goroutine per corpus today, but the test seam should
	// not bake that assumption into the hook).
	var invocations atomic.Int64
	autoBackfillInsertHook = func(ctx context.Context, db *sql.DB, corpus embed.VectorCorpus, entry embed.PendingEntry, vec []float32, modelID string) error {
		n := invocations.Add(1)
		if tc.policy(n) {
			return embed.InsertVectorRow(ctx, db, corpus, entry, vec, modelID)
		}
		// Synthetic error mimics the BEGIN IMMEDIATE timeout shape that
		// LORE-416 named as the dominant production failure mode. The
		// actual string does not matter for the regression assertion;
		// what matters is that the hook returns non-nil so Backfill
		// counts the row as Failed and continues.
		return contextStub("simulated SQLITE_BUSY: writer-lock contention")
	}
	t.Cleanup(func() { autoBackfillInsertHook = nil })

	logger := slog.New(slog.NewJSONHandler(newSafeBuffer(), &slog.HandlerOptions{Level: slog.LevelDebug}))
	deps := &lore.EmbedDeps{
		Embedder: embed.NewDeterministicEmbedder(),
		ModelID:  "bge-small-en-v1.5-int8-cls",
		Async:    true,
		Logger:   logger,
	}

	maybeTriggerAutoBackfill(deps, logger)
	select {
	case <-autoBackfillDoneCh:
	case <-time.After(60 * time.Second):
		t.Fatalf("auto-backfill did not finish within 60s")
	}

	// Read final state.
	ctx := context.Background()
	qdb, err := storage.Open(ctx, questDBPath)
	if err != nil {
		t.Fatalf("reopen quest db: %v", err)
	}
	defer func() { _ = qdb.Close() }()

	var vecCount int64
	if err := qdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM quest_vectors`).Scan(&vecCount); err != nil {
		t.Fatalf("count quest_vectors: %v", err)
	}

	var stateReason string
	if err := qdb.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'quest.embedder_state_reason'`,
	).Scan(&stateReason); err != nil && err != sql.ErrNoRows {
		t.Fatalf("read state_reason: %v", err)
	}

	var numStr, denStr string
	_ = qdb.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'quest.vector_coverage_num'`,
	).Scan(&numStr)
	_ = qdb.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'quest.vector_coverage_den'`,
	).Scan(&denStr)
	num, _ := parseInt64(numStr)
	den, _ := parseInt64(denStr)
	var coverage float64
	if den > 0 {
		coverage = float64(num) / float64(den)
	}

	if tc.wantStateReason != "" && stateReason != tc.wantStateReason {
		t.Errorf("state_reason: got %q, want %q (vec_count=%d coverage=%.3f num=%d den=%d)",
			stateReason, tc.wantStateReason, vecCount, coverage, num, den)
	}
	if tc.wantCoverageAtLeast > 0 && coverage < tc.wantCoverageAtLeast {
		t.Errorf("coverage: got %.3f, want >= %.3f (vec_count=%d num=%d den=%d)",
			coverage, tc.wantCoverageAtLeast, vecCount, num, den)
	}
	if tc.wantCoverageAtMost > 0 && coverage > tc.wantCoverageAtMost {
		t.Errorf("coverage: got %.3f, want <= %.3f (vec_count=%d num=%d den=%d)",
			coverage, tc.wantCoverageAtMost, vecCount, num, den)
	}
	if tc.wantVectorCountAtLeast > 0 && vecCount < tc.wantVectorCountAtLeast {
		t.Errorf("quest_vectors count: got %d, want >= %d", vecCount, tc.wantVectorCountAtLeast)
	}
	if tc.wantVectorCountAtMost > 0 && vecCount > tc.wantVectorCountAtMost {
		t.Errorf("quest_vectors count: got %d, want <= %d", vecCount, tc.wantVectorCountAtMost)
	}
	// AlwaysFail explicit zero check (wantVectorCountAtMost==0 above is
	// treated as "skip" because zero is the zero value; assert it
	// directly when the case demands zero).
	if tc.wantStateReason == stateReasonAutoBackfillNoProgress && vecCount != 0 {
		t.Errorf("AlwaysFail: quest_vectors count: got %d, want 0", vecCount)
	}
}

// contextStub is a lightweight error type for the QUEST-248 hook to
// return. Avoids importing fmt or errors for one constant-shaped string.
type contextStub string

func (c contextStub) Error() string { return string(c) }

// TestAutoBackfill_QUEST246_HistoricalQuestsBackfilled is the QUEST-246
// regression gate at the auto-backfill boundary. Reproduces the
// LORE-404 reproducer at fixture scale: a quest.db whose 200 task_status
// rows were never bridged to tasks_fts_rows by the per-INSERT trigger
// (the historical-data condition migrations 005+006 cohabit on every
// upgrading install). After the auto-backfill goroutine completes,
// quest_vectors must hold >= 200 rows and quest.vector_coverage_den
// must reflect the same.
func TestAutoBackfill_QUEST246_HistoricalQuestsBackfilled(t *testing.T) {
	tmp := t.TempDir()
	loreDBPath := filepath.Join(tmp, "lore.db")
	questDBPath := filepath.Join(tmp, "quest.db")

	resetAutoBackfillState()
	t.Cleanup(resetAutoBackfillState)

	origLdb := ldbPath
	origQdb := qdbPath
	ldbPath = func() (string, error) { return loreDBPath, nil }
	qdbPath = func() (string, error) { return questDBPath, nil }
	t.Cleanup(func() {
		ldbPath = origLdb
		qdbPath = origQdb
	})

	// Seed lore with zero pending: keeps the lore corpus quiet so the
	// test's signal stays focused on the quest path.
	seedEmptyLoreAlreadyCovered(t, loreDBPath)

	// Seed quest.db with 200 historical-style rows. The helper drops the
	// auto-bridge trigger before inserting so the seed mirrors the
	// pre-006 historical-data state.
	seedQuestHistoricalRows(t, questDBPath, 200)

	logger := slog.New(slog.NewJSONHandler(newSafeBuffer(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	deps := &lore.EmbedDeps{
		Embedder: embed.NewDeterministicEmbedder(),
		ModelID:  "bge-small-en-v1.5-int8-cls",
		Async:    true,
		Logger:   logger,
	}

	maybeTriggerAutoBackfill(deps, logger)

	select {
	case <-autoBackfillDoneCh:
	case <-time.After(60 * time.Second):
		t.Fatalf("auto-backfill did not finish within 60s")
	}

	// Assertion 1: quest_vectors row count >= 200.
	ctx := context.Background()
	qdb, err := storage.Open(ctx, questDBPath)
	if err != nil {
		t.Fatalf("reopen quest db: %v", err)
	}
	defer func() { _ = qdb.Close() }()

	var vecCount int64
	if err := qdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM quest_vectors`).Scan(&vecCount); err != nil {
		t.Fatalf("count quest_vectors: %v", err)
	}
	if vecCount < 200 {
		t.Errorf("quest_vectors count after auto-backfill: got %d, want >= 200", vecCount)
	}

	// Assertion 2: quest.vector_coverage_den >= 200.
	var denStr string
	if err := qdb.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'quest.vector_coverage_den'`,
	).Scan(&denStr); err != nil {
		t.Fatalf("read coverage_den: %v", err)
	}
	den, perr := parseInt64(denStr)
	if perr != nil {
		t.Fatalf("parse coverage_den %q: %v", denStr, perr)
	}
	if den < 200 {
		t.Errorf("quest.vector_coverage_den after auto-backfill: got %d, want >= 200", den)
	}
}

// TestAutoBackfill_SilentSuccessGuard verifies the QUEST-246 Part B
// behavior. A quest.db where tasks_fts_rows is implausibly small
// relative to task_status (the LORE-404 reproducer shape) must produce
// a slog WARN line naming the observed numbers. The guard is the
// upgrade-path safety net that prevented LORE-404 from being noticed
// silently for many cycles.
func TestAutoBackfill_SilentSuccessGuard(t *testing.T) {
	tmp := t.TempDir()
	loreDBPath := filepath.Join(tmp, "lore.db")
	questDBPath := filepath.Join(tmp, "quest.db")

	resetAutoBackfillState()
	t.Cleanup(resetAutoBackfillState)

	origLdb := ldbPath
	origQdb := qdbPath
	ldbPath = func() (string, error) { return loreDBPath, nil }
	qdbPath = func() (string, error) { return questDBPath, nil }
	t.Cleanup(func() {
		ldbPath = origLdb
		qdbPath = origQdb
	})

	seedEmptyLoreAlreadyCovered(t, loreDBPath)

	// Build the LORE-404 shape: many task_status rows, only a handful
	// of tasks_fts_rows. The migration-006 backfill is undone via DELETE
	// so the entity count remains tiny against the live activity signal.
	seedQuestSilentSuccessShape(t, questDBPath, 100, 5)

	var logBuf safeBuffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	deps := &lore.EmbedDeps{
		Embedder: embed.NewDeterministicEmbedder(),
		ModelID:  "bge-small-en-v1.5-int8-cls",
		Async:    true,
		Logger:   logger,
	}

	maybeTriggerAutoBackfill(deps, logger)
	select {
	case <-autoBackfillDoneCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("auto-backfill did not finish within 30s")
	}

	// Assertion: a WARN line about implausible entity count was emitted.
	logs := logBuf.String()
	if !strings.Contains(logs, "entity count implausibly small vs live activity") {
		t.Errorf("expected silent-success WARN line; got logs:\n%s", logs)
	}
	if !strings.Contains(logs, `"corpus":"quest"`) {
		t.Errorf("expected WARN to name corpus=quest; got logs:\n%s", logs)
	}
	if !strings.Contains(logs, `"reference_table":"task_status"`) {
		t.Errorf("expected WARN to name reference_table=task_status; got logs:\n%s", logs)
	}
}

// TestAutoBackfill_ArmGateFlipsToRRF verifies that after auto-backfill
// completes against a fixture with >= 200 historical task_status rows
// (the QUEST-246 acceptance), the coverage gate trips and a downstream
// quest_search call would observe arm=rrf rather than arm=bm25.
//
// This test does not call quest_search end-to-end (which would require
// QuestEmbedDeps wiring): instead it asserts the underlying coverage
// computation that drives the arm choice. The coverage gate constant
// is questCoverageGate = 0.90; coverage = num/den must clear it.
func TestAutoBackfill_ArmGateFlipsToRRF(t *testing.T) {
	tmp := t.TempDir()
	loreDBPath := filepath.Join(tmp, "lore.db")
	questDBPath := filepath.Join(tmp, "quest.db")

	resetAutoBackfillState()
	t.Cleanup(resetAutoBackfillState)

	origLdb := ldbPath
	origQdb := qdbPath
	ldbPath = func() (string, error) { return loreDBPath, nil }
	qdbPath = func() (string, error) { return questDBPath, nil }
	t.Cleanup(func() {
		ldbPath = origLdb
		qdbPath = origQdb
	})

	seedEmptyLoreAlreadyCovered(t, loreDBPath)
	seedQuestHistoricalRows(t, questDBPath, 200)

	logger := slog.New(slog.NewJSONHandler(newSafeBuffer(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	deps := &lore.EmbedDeps{
		Embedder: embed.NewDeterministicEmbedder(),
		ModelID:  "bge-small-en-v1.5-int8-cls",
		Async:    true,
		Logger:   logger,
	}
	maybeTriggerAutoBackfill(deps, logger)
	select {
	case <-autoBackfillDoneCh:
	case <-time.After(60 * time.Second):
		t.Fatalf("auto-backfill did not finish within 60s")
	}

	// Read coverage_num + coverage_den and confirm coverage >= 0.90.
	ctx := context.Background()
	qdb, err := storage.Open(ctx, questDBPath)
	if err != nil {
		t.Fatalf("reopen quest db: %v", err)
	}
	defer func() { _ = qdb.Close() }()

	var numStr, denStr string
	if err := qdb.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'quest.vector_coverage_num'`,
	).Scan(&numStr); err != nil {
		t.Fatalf("read coverage_num: %v", err)
	}
	if err := qdb.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'quest.vector_coverage_den'`,
	).Scan(&denStr); err != nil {
		t.Fatalf("read coverage_den: %v", err)
	}
	num, _ := parseInt64(numStr)
	den, _ := parseInt64(denStr)
	if den == 0 {
		t.Fatalf("coverage_den is zero; arm gate cannot evaluate")
	}
	coverage := float64(num) / float64(den)
	if coverage < 0.90 {
		t.Errorf("post-backfill coverage: got %.3f (num=%d den=%d), want >= 0.90 to flip arm to rrf",
			coverage, num, den)
	}
}

// seedQuestHistoricalRows writes a quest.db that mimics the LORE-404
// historical-data shape: count task_status rows seeded after the
// auto-bridge trigger has been temporarily dropped, so each row arrives
// without a tasks_fts_rows companion. Migration 006 is then re-executed
// against the seeded DB to backfill the bridge (the production migration
// has already run via storage.Migrate, so the explicit re-run is the
// idempotent safety path that proves the SQL recovers the install).
func seedQuestHistoricalRows(t *testing.T, path string, count int) {
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
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO projects (id, path) VALUES ('p', '/tmp/p')`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// Drop the auto-bridge trigger so the seeded rows mirror the
	// pre-006 historical condition (rows without bridge entries).
	if _, err := db.ExecContext(ctx, `DROP TRIGGER IF EXISTS tasks_fts_status_ai`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	for i := 1; i <= count; i++ {
		taskID := "QUEST-H" + intToStr(i)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO task_status (project_id, task_id, status) VALUES ('p', ?, 'next')`,
			taskID,
		); err != nil {
			t.Fatalf("seed task_status %s: %v", taskID, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO task_notes (project_id, task_id, agent_id, note)
			 VALUES ('p', ?, 'test', ?)`,
			taskID, "[spec] subject: historical quest "+intToStr(i),
		); err != nil {
			t.Fatalf("seed task_notes %s: %v", taskID, err)
		}
	}

	// Re-execute migration 006 SQL: idempotent backfill of bridge rows
	// + body. Production already applied this once via storage.Migrate;
	// re-running it after the historical seed is what closes the gap.
	if _, err := db.ExecContext(ctx, `
		INSERT OR IGNORE INTO tasks_fts_rows (task_id)
		SELECT DISTINCT task_id FROM task_status;
	`); err != nil {
		t.Fatalf("re-run migration 006 INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE tasks_fts_rows
		SET body = COALESCE((
		  SELECT group_concat(tn.note, ' ')
		  FROM task_notes tn
		  WHERE tn.task_id = tasks_fts_rows.task_id
		    AND tn.note LIKE '[spec]%'
		), '')
		WHERE body = '';
	`); err != nil {
		t.Fatalf("re-run migration 006 UPDATE: %v", err)
	}
	// Reset coverage_den so the auto-backfill assess step recomputes it
	// from the live bridge count.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES ('quest.vector_coverage_den', (SELECT CAST(COUNT(*) AS TEXT) FROM tasks_fts_rows))
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
	); err != nil {
		t.Fatalf("reset coverage_den: %v", err)
	}
}

// seedQuestSilentSuccessShape writes a quest.db with the LORE-404
// shape: bigStatusCount task_status rows but only fewBridgeCount entries
// in tasks_fts_rows. It mimics an install that ran migration 005 but
// somehow ended up without the migration-006 backfill (e.g. the assess
// path is the only line of defense for a corpus whose entity table got
// out of sync after operator intervention).
func seedQuestSilentSuccessShape(t *testing.T, path string, bigStatusCount, fewBridgeCount int) {
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
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO projects (id, path) VALUES ('p', '/tmp/p')`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// Drop trigger so task_status seeds do not auto-bridge.
	if _, err := db.ExecContext(ctx, `DROP TRIGGER IF EXISTS tasks_fts_status_ai`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	for i := 1; i <= bigStatusCount; i++ {
		taskID := "QUEST-S" + intToStr(i)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO task_status (project_id, task_id, status) VALUES ('p', ?, 'next')`,
			taskID,
		); err != nil {
			t.Fatalf("seed task_status %s: %v", taskID, err)
		}
	}

	// Wipe whatever migration 006 already populated, then bridge only the
	// first fewBridgeCount rows. This produces den << ref activity, the
	// silent-success shape.
	if _, err := db.ExecContext(ctx, `DELETE FROM tasks_fts_rows`); err != nil {
		t.Fatalf("clear bridge: %v", err)
	}
	for i := 1; i <= fewBridgeCount; i++ {
		taskID := "QUEST-S" + intToStr(i)
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO tasks_fts_rows (task_id, body) VALUES (?, ?)`,
			taskID, "[spec] subject: bridged quest "+intToStr(i),
		); err != nil {
			t.Fatalf("seed bridge %s: %v", taskID, err)
		}
	}
	// Reset coverage_den so the assess path recomputes from the bridge.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES ('quest.vector_coverage_den', (SELECT CAST(COUNT(*) AS TEXT) FROM tasks_fts_rows))
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
	); err != nil {
		t.Fatalf("reset coverage_den: %v", err)
	}
}

// TestFinalizeCorpusState_RefreshesStaleAutoBackfillPromoted is the QUEST-253
// regression gate for the stuck-state shape: a corpus already at
// state=enabled with state_reason=auto_backfill_promoted but coverage well
// below the floor (the maintainer's real install: 9/247 = 3.6%). The refined
// gate must fall through and flip state_reason to auto_backfill_partial so
// the operator can triage instead of seeing a falsely-promoted corpus.
func TestFinalizeCorpusState_RefreshesStaleAutoBackfillPromoted(t *testing.T) {
	tmp := t.TempDir()
	questDBPath := filepath.Join(tmp, "quest.db")
	ctx := context.Background()

	db, err := storage.Open(ctx, questDBPath)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := storage.Migrate(ctx, db, "quest"); err != nil {
		t.Fatalf("migrate quest: %v", err)
	}

	corpus := embed.QuestCorpus{}

	// Seed the maintainer's stuck-state shape: state=enabled,
	// state_reason=auto_backfill_promoted, num=9, den=247.
	upsertMeta(t, db, corpus.MetaKey(embed.FieldEmbedderState), "enabled")
	upsertMeta(t, db, corpus.MetaKey(embed.FieldEmbedderStateReason), stateReasonAutoBackfillPromoted)
	upsertMeta(t, db, corpus.MetaKey(embed.FieldVectorCoverageNum), "9")
	upsertMeta(t, db, corpus.MetaKey(embed.FieldVectorCoverageDen), "247")

	// finalCoverage = 9/247 = 0.036, well below the 0.90 floor.
	finalCoverage := 9.0 / 247.0

	logger := slog.New(slog.NewJSONHandler(newSafeBuffer(), &slog.HandlerOptions{Level: slog.LevelDebug}))
	if err := finalizeCorpusState(ctx, db, corpus, "bge-small-en-v1.5-int8-cls", finalCoverage, logger); err != nil {
		t.Fatalf("finalizeCorpusState: %v", err)
	}

	// Assert: state_reason flipped to auto_backfill_partial.
	var gotReason string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, corpus.MetaKey(embed.FieldEmbedderStateReason),
	).Scan(&gotReason); err != nil {
		t.Fatalf("read state_reason: %v", err)
	}
	if gotReason != stateReasonAutoBackfillPartial {
		t.Errorf("state_reason: got %q, want %q (stale promoted with coverage=%.3f should be refreshed)",
			gotReason, stateReasonAutoBackfillPartial, finalCoverage)
	}

	// Assert: state stays 'enabled'.
	var gotState string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, corpus.MetaKey(embed.FieldEmbedderState),
	).Scan(&gotState); err != nil {
		t.Fatalf("read embedder_state: %v", err)
	}
	if gotState != "enabled" {
		t.Errorf("embedder_state: got %q, want %q", gotState, "enabled")
	}
}

// TestFinalizeCorpusState_PreservesNonAutoBackfillReason is the QUEST-253
// regression gate for init-path provenance preservation. A corpus at
// state=enabled with a non-auto-backfill state_reason (e.g. set by guild
// init or an operator) must not have its state_reason overwritten, even
// when coverage is below the floor.
func TestFinalizeCorpusState_PreservesNonAutoBackfillReason(t *testing.T) {
	tmp := t.TempDir()
	questDBPath := filepath.Join(tmp, "quest.db")
	ctx := context.Background()

	db, err := storage.Open(ctx, questDBPath)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := storage.Migrate(ctx, db, "quest"); err != nil {
		t.Fatalf("migrate quest: %v", err)
	}

	corpus := embed.QuestCorpus{}
	const initPathReason = "init_path_custom"

	// Seed with an init-path reason (not managed by auto-backfill).
	upsertMeta(t, db, corpus.MetaKey(embed.FieldEmbedderState), "enabled")
	upsertMeta(t, db, corpus.MetaKey(embed.FieldEmbedderStateReason), initPathReason)
	upsertMeta(t, db, corpus.MetaKey(embed.FieldVectorCoverageNum), "9")
	upsertMeta(t, db, corpus.MetaKey(embed.FieldVectorCoverageDen), "247")

	finalCoverage := 9.0 / 247.0

	logger := slog.New(slog.NewJSONHandler(newSafeBuffer(), &slog.HandlerOptions{Level: slog.LevelDebug}))
	if err := finalizeCorpusState(ctx, db, corpus, "bge-small-en-v1.5-int8-cls", finalCoverage, logger); err != nil {
		t.Fatalf("finalizeCorpusState: %v", err)
	}

	// Assert: state_reason unchanged (init-path provenance preserved).
	var gotReason string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, corpus.MetaKey(embed.FieldEmbedderStateReason),
	).Scan(&gotReason); err != nil {
		t.Fatalf("read state_reason: %v", err)
	}
	if gotReason != initPathReason {
		t.Errorf("state_reason: got %q, want %q (init-path provenance must not be overwritten by auto-backfill)",
			gotReason, initPathReason)
	}
}

// intToStr is a local itoa to avoid importing strconv for one call.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
