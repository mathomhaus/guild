package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/lore/embed"
)

// Auto-backfill closes the LORE-384 / LORE-371 upgrade UX gap: users
// install guild once, upgrade to a binary that introduces a new corpus
// (or shifts its embedding pipeline), and never re-run `guild init`. Any
// corpus with pending entries stays empty forever unless something
// triggers a backfill on their behalf. This file is that trigger.
//
// Contract:
//
//   - Fires EXACTLY ONCE per MCP server process (sync.Once).
//   - Only after embedProvider.ResolveEmbedDeps first returns a fully
//     wired *lore.EmbedDeps (meta.embedder_state=='enabled' AND the
//     probe/extract paths succeeded). This is the same observation
//     point LORE-371 documented: it is the first moment the process
//     knows the embedder is live on this machine.
//   - Iterates the registered autoBackfillTargets. For each target
//     whose corpus shows (a) pending count > 0 AND (b) coverage < 0.90,
//     spawns a background goroutine that calls embed.Backfill against
//     that corpus.
//   - Never blocks the caller. The provider's hot path finishes before
//     the backfill goroutine has even started.
//
// Cross-process safety: ADR-003 invariant 1 (INSERT OR IGNORE on every
// vector write) means two MCP servers racing the same backfill produce
// one winning row per entity. Wasted cycles on the loser, correct
// state. No leader-election primitive; the SQLite row-level conflict
// is the coordination.

// autoBackfillTarget describes one corpus the process should auto-
// backfill at startup. The openDB closure points at the DB where the
// corpus's tables live (lore.db for LoreCorpus, quest.db for
// QuestCorpus). Tests register fakes via registerAutoBackfillTargetForTest.
type autoBackfillTarget struct {
	// corpus names the VectorCorpus adapter (LoreCorpus{},
	// QuestCorpus{}, future adapters). Drives every SQL template Backfill
	// constructs.
	corpus embed.VectorCorpus
	// openDB opens the database where corpus lives. Each call gets a
	// fresh handle the caller closes; matches the openLoreDB /
	// openQuestDB contract used everywhere else in this package.
	openDB func(ctx context.Context) (*sql.DB, error)
}

// autoBackfillTargets is the package-level registry the auto-backfill
// trigger iterates. Populated via init() in this file so adding a new
// corpus is a one-line edit here plus a VectorCorpus adapter elsewhere.
// No mutex: writes happen only at package init and from test helpers
// that clear/restore under t.Cleanup in a single-goroutine test body.
var autoBackfillTargets []autoBackfillTarget

func init() {
	// LoreCorpus lives in lore.db. LORE-384's concrete failure mode was
	// 10 pending lore entries inscribed by a server predating the lazy-
	// reconstruct path; auto-backfill closes that gap.
	//
	// QuestCorpus lives in quest.db. Post-QUEST-224 upgrades leave
	// quest_vectors entirely empty on existing installs; auto-backfill
	// populates them once the embedder is live.
	autoBackfillTargets = []autoBackfillTarget{
		{corpus: embed.LoreCorpus{}, openDB: openLoreDB},
		{corpus: embed.QuestCorpus{}, openDB: openQuestDB},
	}
}

// autoBackfillOnce guards the trigger. sync.Once semantics: regardless
// of how many lore tool handlers race through ResolveEmbedDeps at
// startup, maybeTriggerAutoBackfill runs its body exactly once. A
// subsequent meta flip (mid-session disable/enable) does NOT re-fire
// auto-backfill: once is enough, and hot-path writes keep coverage
// moving afterward.
//
// Reset in Register() so each test-spawned server gets its own gate.
var autoBackfillOnce sync.Once

// autoBackfillDoneCh is closed when every spawned goroutine has
// completed (successfully or not). Nil until the trigger fires; tests
// read it via waitForAutoBackfill. Writes to this variable happen
// inside autoBackfillOnce.Do so there is no race.
var autoBackfillDoneCh chan struct{}

// resetAutoBackfillState restores the package-level gate so the next
// provider-resolve-wired cycle can fire the trigger again. Called from
// Register() so each rebuilt server (real or test-spawned) sees a
// clean sync.Once.
func resetAutoBackfillState() {
	autoBackfillOnce = sync.Once{}
	autoBackfillDoneCh = nil
	autoBackfillInsertHook = nil
}

// autoBackfillInsertHook is a test-only seam that lets the regression
// test for QUEST-248 inject a fail-intermittently shim around the real
// insertVectorRow without invasive surgery. Production code leaves this
// nil; embed.Backfill then calls insertVectorRow directly. Tests assign
// it under t.Cleanup so concurrent runs do not see each other's hook.
var autoBackfillInsertHook func(ctx context.Context, db *sql.DB, corpus embed.VectorCorpus, entry embed.PendingEntry, vec []float32, modelID string) error

// maybeTriggerAutoBackfill is the single entry point invoked from
// embedProvider.reconstruct after a successful wire. Called with the
// live *lore.EmbedDeps so the goroutines can reuse the binary's
// Embedder instance without re-probing.
//
// Parameter intent:
//
//   - deps: the just-wired *lore.EmbedDeps. Must be non-nil and
//     Enabled(); the caller (reconstruct) ensures this.
//   - logger: the server-scoped structured logger.
//
// This function returns immediately; all work happens in background
// goroutines guarded by autoBackfillOnce.
func maybeTriggerAutoBackfill(deps *lore.EmbedDeps, logger *slog.Logger) {
	if deps == nil || !deps.Enabled() {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	autoBackfillOnce.Do(func() {
		targets := autoBackfillTargets
		done := make(chan struct{})
		autoBackfillDoneCh = done

		// context.Background() because auto-backfill is server-lifetime
		// work: the handler call that triggered us may finish in
		// milliseconds, but the backfill runs for as long as it takes.
		// ctx.Err() checks inside Backfill make this safe to abandon.
		go runAutoBackfill(context.Background(), targets, deps, logger, done)
	})
}

// runAutoBackfill is the body of the once-guarded trigger. For each
// registered corpus target: decide whether to act (pending > 0 and
// coverage < 0.90) and if so spawn a per-corpus goroutine. Waits for
// every goroutine to finish before closing done so tests can
// deterministically assert completion.
func runAutoBackfill(ctx context.Context, targets []autoBackfillTarget, deps *lore.EmbedDeps, logger *slog.Logger, done chan struct{}) {
	defer close(done)

	var wg sync.WaitGroup
	for _, tgt := range targets {
		pending, coverage, ok := assessCorpus(ctx, tgt, logger)
		if !ok {
			continue
		}
		if pending <= 0 || coverage >= backfillCoverageFloor {
			// Healthy or nothing to do. No slog noise; the healthy
			// case is the common path and should stay quiet.
			continue
		}
		wg.Add(1)
		go func(tgt autoBackfillTarget, pending int64, coverage float64) {
			defer wg.Done()
			runOneCorpusBackfill(ctx, tgt, deps, pending, coverage, logger)
		}(tgt, pending, coverage)
	}
	wg.Wait()
}

// backfillCoverageFloor is the ADR-003 gate: coverage >= 0.90 means the
// corpus is considered "live" and appraise can safely use the vector
// arm. Below the floor we assume the corpus needs help. Mirrors
// internal/lore/embed/health.go's backfillCoverageThreshold.
const backfillCoverageFloor = 0.90

// assessCorpus decides whether a corpus wants a backfill. Counts live
// active entities (the true "den") and live vector rows (the true
// "num") via direct SQL so the answer is accurate even when the
// meta.vector_coverage_den row was never reconciled (a post-upgrade
// DB that never ran `guild init`). Returns pending count, coverage
// ratio, and an ok flag (false on any unrecoverable read error so the
// outer loop skips the target without panicking).
func assessCorpus(ctx context.Context, tgt autoBackfillTarget, logger *slog.Logger) (pending int64, coverage float64, ok bool) {
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	db, err := tgt.openDB(readCtx)
	if err != nil {
		logger.Warn("auto-backfill assess: open db failed",
			slog.String("corpus", tgt.corpus.Name()),
			slog.String("err", err.Error()),
		)
		return 0, 0, false
	}
	defer func() { _ = db.Close() }()

	activePred := tgt.corpus.ActivePredicate()
	if activePred == "" {
		activePred = "1=1"
	}
	// den = number of active entities eligible for embedding.
	denQuery := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s`, //nolint:gosec // G201: table + predicate are compile-time corpus accessors, not user input.
		tgt.corpus.EntityTable(), activePred)
	var den int64
	if err := db.QueryRowContext(readCtx, denQuery).Scan(&den); err != nil { //nolint:sqlcheck // table + predicate are compile-time corpus accessors.
		logger.Warn("auto-backfill assess: count den failed",
			slog.String("corpus", tgt.corpus.Name()),
			slog.String("err", err.Error()),
		)
		return 0, 0, false
	}
	// num = number of vector rows the corpus already has. A missing
	// vector table surfaces as a SQL error; surface as skip rather than
	// blow up the whole trigger.
	numQuery := fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tgt.corpus.VectorTable()) //nolint:gosec // G201: table name is a compile-time corpus accessor, not user input.
	var num int64
	if err := db.QueryRowContext(readCtx, numQuery).Scan(&num); err != nil { //nolint:sqlcheck // table name is a compile-time corpus accessor.
		logger.Warn("auto-backfill assess: count num failed",
			slog.String("corpus", tgt.corpus.Name()),
			slog.String("err", err.Error()),
		)
		return 0, 0, false
	}

	// Silent-success guard (QUEST-246, LORE-404): if den is implausibly
	// small relative to obvious live activity in the same DB, the corpus
	// is reporting a healthy num/den that masks a broken entity-source
	// wiring. Without this guard, a 9/9 = 100% coverage with 240+ real
	// quests in task_status looks healthy and auto-backfill exits silent.
	// emitSanityWarn reads a corpus-specific reference count and logs one
	// WARN line when the den/reference ratio falls below 0.10; the line
	// names the observed numbers and points operators at the diagnostic
	// SQL. The check is best-effort: any read error (e.g. the reference
	// table missing) is silently skipped so the outer assess flow does
	// not regress on corpora that opt out.
	emitSanityWarn(readCtx, db, tgt.corpus, den, logger)

	// Empty corpus (zero active entities): nothing to backfill.
	if den <= 0 {
		return 0, 1.0, true
	}
	pending = den - num
	if pending < 0 {
		pending = 0
	}
	coverage = float64(num) / float64(den)
	return pending, coverage, true
}

// sanityRefThreshold is the den/reference ratio below which the
// silent-success guard fires. 0.10 means "den is less than 10% of the
// reference signal in the same DB", strong evidence the entity source
// is wired wrong. The threshold matches the heuristic recommended in
// LORE-404 (the QUEST-246 spec).
const sanityRefThreshold = 0.10

// emitSanityWarn checks whether the den computed from
// corpus.EntityTable() is implausibly small relative to a corpus-
// specific reference table that signals real activity. When the ratio
// breaches sanityRefThreshold a single WARN line is emitted naming the
// observed numbers and the diagnostic action.
//
// Per-corpus dispatch is a small switch on corpus.Name() rather than a
// new VectorCorpus method; the heuristic is a startup-time observability
// concern, not a property of the corpus's storage shape, and should not
// pollute the algorithmic port. Adding a new corpus reuses the helper
// only when its activity-vs-entities mismatch is a plausible failure
// mode worth surfacing.
func emitSanityWarn(ctx context.Context, db *sql.DB, corpus embed.VectorCorpus, den int64, logger *slog.Logger) {
	if logger == nil || db == nil {
		return
	}
	refTable, refQuery, ok := sanityReference(corpus)
	if !ok {
		return
	}
	var ref int64
	if err := db.QueryRowContext(ctx, refQuery).Scan(&ref); err != nil { //nolint:sqlcheck // refQuery is a compile-time literal selected from sanityReference.
		// Reference table missing or unreadable. Silent skip: the guard
		// is advisory, not authoritative.
		return
	}
	if ref <= 0 {
		// No reference activity to compare against; den at any value is
		// not surprising on a fresh DB.
		return
	}
	if float64(den) >= sanityRefThreshold*float64(ref) {
		return
	}
	ratio := 0.0
	if den > 0 {
		ratio = float64(ref) / float64(den)
	}
	logger.Warn("auto-backfill assess: entity count implausibly small vs live activity",
		slog.String("corpus", corpus.Name()),
		slog.Int64("entity_den", den),
		slog.String("reference_table", refTable),
		slog.Int64("reference_count", ref),
		slog.Float64("mismatch_ratio", ratio),
		slog.String("hint", "embeddings will be skipped for most rows; check that the entity-source wiring populates from the canonical activity table"),
	)
}

// sanityReference returns the reference-table name, the COUNT(*) query
// against it, and an ok flag for the given corpus. ok=false opts out of
// the silent-success guard. The query string is a compile-time literal
// so callers can pass it directly to QueryRowContext without exposing a
// SQL-injection seam.
//
//nolint:gocritic // unnamedResult: the three return positions are documented in the comment above
func sanityReference(corpus embed.VectorCorpus) (refTable, refQuery string, ok bool) {
	switch corpus.Name() {
	case "quest":
		// task_status carries one row per state transition (multiple rows
		// per task_id over its lifecycle: posted, accepted, fulfilled,
		// etc.). It is the canonical activity signal even though it is
		// not 1-to-1 with quests. A QuestCorpus den < 0.10 *
		// COUNT(task_status) means tasks_fts_rows was never backfilled
		// from task_status (LORE-404 reproducer); the 10% threshold is
		// permissive enough to absorb the row-multiplicity noise.
		return "task_status", `SELECT COUNT(*) FROM task_status`, true
	default:
		return "", "", false
	}
}

// backfillIterCap caps the bounded inner loop in runOneCorpusBackfill.
// 5 gives ~5x the throughput of single-pass against transient writer-lock
// contention (LORE-416's diagnosed dominant failure mode) without risking
// pathological loops on persistent bugs (e.g. a model-side encode error
// every cycle). The exit conditions inside runOneCorpusBackfill are:
//
//   - coverage >= backfillCoverageFloor (success)
//   - res.Embedded == 0 in the latest cycle (no progress)
//   - iteration count reaches the cap
//
// Any of the three terminates the loop; the cap is the last-resort guard.
const backfillIterCap = 5

// State-reason values written by the auto-backfill finalize helper. Three
// values reflect the actual outcome (LORE-416): "promoted" means coverage
// cleared the floor, "partial" means coverage exists but is below the
// floor, "no_progress" means zero rows landed even though the embedder
// is wired. Operators read state_reason to triage; the values must remain
// honest about what actually happened.
const (
	stateReasonAutoBackfillPromoted   = "auto_backfill_promoted"
	stateReasonAutoBackfillPartial    = "auto_backfill_partial"
	stateReasonAutoBackfillNoProgress = "auto_backfill_no_progress"
)

// isAutoBackfillReason returns true when the given state_reason was written
// by the auto-backfill path (as opposed to an external init path). The
// finalizeCorpusState gate uses this to distinguish "safe to refresh"
// (auto-backfill managed, QUEST-253) from "preserve unchanged" (init-path
// or operator-set provenance).
func isAutoBackfillReason(reason string) bool {
	switch reason {
	case stateReasonAutoBackfillPromoted,
		stateReasonAutoBackfillPartial,
		stateReasonAutoBackfillNoProgress:
		return true
	default:
		return false
	}
}

// runOneCorpusBackfill encodes one corpus end-to-end with a bounded
// inner loop (LORE-416 / QUEST-248): re-invoke embed.Backfill until
// coverage clears the floor, no progress is observed in the latest
// cycle, or the iteration cap is reached. Each Backfill call uses a
// LEFT JOIN to find pending rows so re-invoking naturally picks up the
// rows that failed last time.
//
// Final state-promote runs AFTER the loop with a tri-state reason that
// reflects the actual outcome ("promoted" / "partial" / "no_progress").
// LORE-411 named the original anti-pattern: state was promoted before
// Backfill ran, so 9/247 coverage looked like success.
//
// Emits one INFO line at start, one INFO per inner iteration, one INFO
// at completion (carrying the dominant failure class for the last
// cycle), or one ERROR on a non-recoverable open/finalize failure.
// Never panics; the caller's goroutine recovers silently via the
// normal defer chain.
func runOneCorpusBackfill(ctx context.Context, tgt autoBackfillTarget, deps *lore.EmbedDeps, pending int64, coverageBefore float64, logger *slog.Logger) {
	started := time.Now()
	corpusName := tgt.corpus.Name()

	logger.Info("auto-backfill started",
		slog.String("corpus_name", corpusName),
		slog.Int64("pending_count", pending),
		slog.Float64("coverage_before", coverageBefore),
	)

	db, err := tgt.openDB(ctx)
	if err != nil {
		logger.Error("auto-backfill failed: open db",
			slog.String("corpus_name", corpusName),
			slog.String("err", err.Error()),
		)
		return
	}
	defer func() { _ = db.Close() }()

	// Bounded inner loop: re-invoke Backfill while coverage is below
	// the floor and the latest cycle made progress. The LEFT JOIN scan
	// in scanPending naturally picks up rows that failed in earlier
	// cycles (each successful insertVectorRow adds a quest_vectors row
	// that drops out of the candidate set on the next pass).
	var (
		lastRes              *embed.BackfillResult
		totalEmbedded        int
		totalFailed          int
		totalSkipped         int
		finalCoverage        float64 = coverageBefore
		dominantFailureClass string
		capHit               bool
	)
	for iter := 1; iter <= backfillIterCap; iter++ {
		res, err := embed.Backfill(ctx, embed.BackfillOptions{
			DB:         db,
			Corpus:     tgt.corpus,
			Embedder:   deps.Embedder,
			ModelID:    deps.ModelID,
			Logger:     logger,
			InsertHook: autoBackfillInsertHook,
		})
		if err != nil {
			fields := []any{
				slog.String("corpus_name", corpusName),
				slog.Int("iteration", iter),
				slog.String("err", err.Error()),
			}
			if res != nil {
				fields = append(fields,
					slog.Int("embedded", res.Embedded),
					slog.Int("failed", res.Failed),
					slog.Int("skipped", res.Skipped),
				)
			}
			logger.Error("auto-backfill failed", fields...)
			// Even on error, finalize the state with whatever coverage
			// we have so the corpus is not left in 'disabled' purgatory.
			// Any finalize error is logged inside the helper; we cannot
			// recover further from this path.
			_ = finalizeCorpusState(ctx, db, tgt.corpus, deps.ModelID, finalCoverage, logger)
			return
		}
		if res == nil {
			logger.Error("auto-backfill failed: nil result",
				slog.String("corpus_name", corpusName),
				slog.Int("iteration", iter),
			)
			_ = finalizeCorpusState(ctx, db, tgt.corpus, deps.ModelID, finalCoverage, logger)
			return
		}
		lastRes = res
		totalEmbedded += res.Embedded
		totalFailed += res.Failed
		totalSkipped += res.Skipped
		dominantFailureClass = res.DominantFailureClass

		// Recompute coverage from the live tables: the meta counters
		// drift in the same tx as insertVectorRow so a fresh count is
		// authoritative.
		_, coverage, ok := assessCorpus(ctx, tgt, logger)
		if ok {
			finalCoverage = coverage
		}

		logger.Info("auto-backfill iteration",
			slog.String("corpus_name", corpusName),
			slog.Int("iteration", iter),
			slog.Int("embedded_this_cycle", res.Embedded),
			slog.Int("failed_this_cycle", res.Failed),
			slog.Int("skipped_this_cycle", res.Skipped),
			slog.Float64("coverage_after", finalCoverage),
		)

		if finalCoverage >= backfillCoverageFloor {
			break
		}
		if res.Embedded == 0 {
			// Genuine no-progress: nothing landed this cycle. Surface as
			// WARN so the operator sees the cause rather than mistaking
			// the silence for success (LORE-416).
			logger.Warn("auto-backfill no progress in cycle",
				slog.String("corpus_name", corpusName),
				slog.Int("iteration", iter),
				slog.Int("failed", res.Failed),
				slog.Int("skipped", res.Skipped),
				slog.String("dominant_failure_class", res.DominantFailureClass),
			)
			break
		}
		if iter == backfillIterCap {
			capHit = true
			logger.Warn("auto-backfill iteration cap reached",
				slog.String("corpus_name", corpusName),
				slog.Int("cap", backfillIterCap),
				slog.Float64("coverage_final", finalCoverage),
			)
		}
	}

	// Finalize state AFTER the loop with a tri-state reason that
	// reflects the actual outcome (LORE-411 anti-pattern fix).
	if err := finalizeCorpusState(ctx, db, tgt.corpus, deps.ModelID, finalCoverage, logger); err != nil {
		logger.Error("auto-backfill failed: finalize corpus state",
			slog.String("corpus_name", corpusName),
			slog.String("err", err.Error()),
		)
		return
	}

	// Aggregate completion line: names the dominant failure class from
	// the last Backfill cycle so a partial-coverage finish points at the
	// likely cause without scanning the per-iteration WARN lines.
	completeFields := []any{
		slog.String("corpus_name", corpusName),
		slog.Int("encoded_total", totalEmbedded),
		slog.Int("skipped_total", totalSkipped),
		slog.Int("failed_total", totalFailed),
		slog.Float64("coverage_final", finalCoverage),
		slog.Bool("cap_hit", capHit),
		slog.Duration("duration", time.Since(started)),
	}
	if lastRes != nil {
		completeFields = append(completeFields, slog.Int64("epoch", lastRes.Epoch))
	}
	if dominantFailureClass != "" {
		completeFields = append(completeFields,
			slog.String("dominant_failure_class", dominantFailureClass),
		)
	}
	logger.Info("auto-backfill complete", completeFields...)
}

// finalizeCorpusState writes the corpus meta identity rows and the
// state='enabled' / state_reason rows that reflect the actual outcome
// of the bounded inner loop. The tri-state rule (LORE-416):
//
//   - coverage >= backfillCoverageFloor: state_reason='auto_backfill_promoted'
//   - 0 < coverage < backfillCoverageFloor: state_reason='auto_backfill_partial'
//   - coverage == 0: state_reason='auto_backfill_no_progress'
//
// The identity rows (model_id, tokenizer_hash, runtime_version, dim) are
// written regardless of coverage; they record the embedder identity that
// would have been used. State='enabled' regardless of coverage too: the
// embedder IS wired, even if the backfill did not converge. The reason
// column is the honest signal.
//
// Gate semantics (QUEST-253): when the corpus's current state is already
// 'enabled', the behavior depends on the existing state_reason:
//   - Non-auto-backfill reason (e.g. 'init_path_custom'): no-op. Preserves
//     init-path or operator-set provenance.
//   - Auto-backfill reason AND coverage >= backfillCoverageFloor AND reason
//     is already 'auto_backfill_promoted': no-op. Already converged.
//   - Auto-backfill reason with stale coverage (e.g. state was promoted on a
//     prior partial run but coverage is still below the floor): fall through
//     and write the new state_reason so meta honestly reflects the outcome.
func finalizeCorpusState(ctx context.Context, db *sql.DB, corpus embed.VectorCorpus, modelID string, finalCoverage float64, logger *slog.Logger) error {
	stateKey := corpus.MetaKey(embed.FieldEmbedderState)
	current, err := readMetaValue(ctx, db, stateKey)
	if err != nil {
		return fmt.Errorf("read %s: %w", stateKey, err)
	}
	if current == "enabled" {
		reason, _ := readMetaValue(ctx, db, corpus.MetaKey(embed.FieldEmbedderStateReason))
		// Preserve init-path or external provenance: do not trample if
		// reason is not auto-backfill-managed (QUEST-253 gate semantics).
		if !isAutoBackfillReason(reason) {
			return nil
		}
		// Healthy: previously promoted AND coverage cleared the floor. No refresh needed.
		if reason == stateReasonAutoBackfillPromoted && finalCoverage >= backfillCoverageFloor {
			return nil
		}
		// Else: auto-backfill-managed AND (stale promoted with low coverage
		// OR previous partial/no_progress reason). Fall through to write the
		// new state_reason so meta honestly reflects the latest outcome.
	}

	reason := stateReasonAutoBackfillPromoted
	switch {
	case finalCoverage >= backfillCoverageFloor:
		reason = stateReasonAutoBackfillPromoted
	case finalCoverage > 0:
		reason = stateReasonAutoBackfillPartial
	default:
		reason = stateReasonAutoBackfillNoProgress
	}

	man := embed.CurrentManifest()
	identity := man.Identity
	if identity.ModelID == "" {
		identity.ModelID = modelID
	}
	if identity.Dim == 0 {
		identity.Dim = embed.Dim
	}

	rows := []struct{ k, v string }{
		{corpus.MetaKey(embed.FieldEmbedderState), "enabled"},
		{corpus.MetaKey(embed.FieldEmbedderStateReason), reason},
		{corpus.MetaKey(embed.FieldEmbedderModelID), identity.ModelID},
		{corpus.MetaKey(embed.FieldEmbedderTokenizerHash), identity.TokenizerHash},
		{corpus.MetaKey(embed.FieldEmbedderRuntimeVersion), identity.RuntimeVersion},
		{corpus.MetaKey(embed.FieldEmbedderDim), strconv.Itoa(identity.Dim)},
	}
	for _, kv := range rows {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO meta (key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			kv.k, kv.v,
		); err != nil {
			if logger != nil {
				logger.Warn("auto-backfill finalize: meta upsert failed",
					slog.String("corpus", corpus.Name()),
					slog.String("key", kv.k),
					slog.String("err", err.Error()),
				)
			}
			return fmt.Errorf("finalize upsert %s: %w", kv.k, err)
		}
	}
	return nil
}
