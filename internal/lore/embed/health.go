package embed

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// EmbedderState represents the operational state of the embedder as recorded
// in the meta table. Non-healthy states produce a line in guild_session_start.
type EmbedderState string

const (
	// EmbedderStateEnabled means the embedder is fully operational.
	EmbedderStateEnabled EmbedderState = "enabled"

	// EmbedderStateDisabled means the embedder is not operational (Windows,
	// dylib probe failed, binary outdated, etc.).
	EmbedderStateDisabled EmbedderState = "disabled"
)

// SessionHealthLine describes what to emit in guild_session_start.
// Healthy state: empty string (no noise). Non-healthy states emit a single line.
type SessionHealthLine struct {
	// Line is the compact text to append in the session-start snapshot.
	// Empty string means "healthy" (emit nothing).
	Line string
}

// HealthReport carries the full embedder health picture for guild lore health.
// This is a pure data struct; CLI and MCP adapters format it separately.
type HealthReport struct {
	// Meta row values.
	ModelID        string
	TokenizerHash  string
	RuntimeVersion string
	Dim            int
	State          EmbedderState
	DisabledReason string // non-empty only when State == disabled and reason is known
	VectorEpoch    int64

	// Coverage.
	CoverageNum int64 // count of 'indexed' entries (lore_vectors rows for current model)
	CoverageDen int64 // count of active entries eligible for embedding
	CoveragePct float64

	// Per-state counts.
	PendingCount int64 // entries with vector_state = 'pending'
	StaleCount   int64 // entries with vector_state = 'stale'

	// Error tracking.
	EmbedErrorCount int64  // rolling count of Tx2 failures (meta.embed_error_count)
	LastEncodeError string // most recent error message, "" if none
	LastEncodeErrAt *time.Time
	LastEncodeOKAt  *time.Time // last successful encode timestamp

	// Derived health classification (used by session-start line logic).
	HealthClass healthClass
}

// healthClass is the derived classification driving the session-start line.
// Each constant maps to exactly one session-start line variant or silence.
type healthClass int

const (
	healthClassHealthy     healthClass = iota // no line emitted
	healthClassDisabled                       // embedder: disabled (reason)
	healthClassBackfilling                    // embedder: backfilling (X%, ETA ~Ns)
	healthClassStale                          // embedder: stale vectors present (N rows, run ...)
	healthClassRepeated                       // embedder: repeated write failures (N in last hour, run ...)
)

// repeatedFailureThreshold is the embed_error_count value above which we
// classify the health as "repeated failures". This is a rolling count; the
// session-start line surfaces it so the user runs guild lore health.
const repeatedFailureThreshold = 5

// backfillCoverageThreshold is the RRF gate from ADR-003. Below this value
// the corpus is still "backfilling" from the session-start perspective.
const backfillCoverageThreshold = 0.90

// ReadHealthReport queries the corpus's meta rows plus its entity and
// vector tables to produce a HealthReport. The caller is responsible
// for opening and closing the DB connection.
//
// corpus may be nil, which selects LoreCorpus{} for backward compat
// with callers that predate the port. Per-corpus health is
// independent: two corpora sharing a DB produce two independent
// reports because their MetaKey resolution does not alias.
func ReadHealthReport(ctx context.Context, db *sql.DB, corpus VectorCorpus) (*HealthReport, error) {
	if db == nil {
		return nil, fmt.Errorf("embed: health: nil db")
	}
	if corpus == nil {
		corpus = LoreCorpus{}
	}

	// Resolve every meta key once so the query IN list and the
	// later map lookups agree on the same strings. Order is stable
	// per MetaField enum so prefixed corpora line up with lore's
	// keyset shape.
	fields := []MetaField{
		FieldEmbedderModelID,
		FieldEmbedderTokenizerHash,
		FieldEmbedderRuntimeVersion,
		FieldEmbedderDim,
		FieldEmbedderState,
		FieldEmbedderStateReason,
		FieldVectorEpoch,
		FieldVectorCoverageNum,
		FieldVectorCoverageDen,
		FieldEmbedErrorCount,
		FieldEmbedLastError,
		FieldEmbedLastErrorAt,
		FieldEmbedLastOKAt,
	}
	keys := make([]string, 0, len(fields))
	for _, f := range fields {
		keys = append(keys, corpus.MetaKey(f))
	}
	placeholders := make([]string, len(keys))
	args := make([]any, len(keys))
	for i, k := range keys {
		placeholders[i] = "?"
		args[i] = k
	}
	metaQuery := fmt.Sprintf(`SELECT key, value FROM meta WHERE key IN (%s)`, strings.Join(placeholders, ",")) //nolint:gosec // G201: only substitution is "?,?,?..." placeholders of fixed count; keys flow as parameters.
	rows, err := db.QueryContext(ctx, metaQuery, args...)                                                      //nolint:sqlcheck // IN list expanded from compile-time corpus accessors.
	if err != nil {
		return nil, fmt.Errorf("embed: health: query meta: %w", err)
	}
	defer func() { _ = rows.Close() }()

	meta := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("embed: health: scan meta: %w", err)
		}
		meta[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("embed: health: iterate meta: %w", err)
	}

	r := &HealthReport{}

	// Look up each field by its corpus-resolved key.
	get := func(f MetaField) string { return meta[corpus.MetaKey(f)] }

	r.ModelID = get(FieldEmbedderModelID)
	r.TokenizerHash = get(FieldEmbedderTokenizerHash)
	r.RuntimeVersion = get(FieldEmbedderRuntimeVersion)
	r.State = EmbedderState(get(FieldEmbedderState))
	if r.State == "" {
		r.State = EmbedderStateDisabled
	}
	r.DisabledReason = get(FieldEmbedderStateReason)

	if v := get(FieldEmbedderDim); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			r.Dim = n
		}
	}
	if r.Dim <= 0 {
		r.Dim = Dim
	}

	r.VectorEpoch = parseInt64(get(FieldVectorEpoch))
	r.CoverageNum = parseInt64(get(FieldVectorCoverageNum))
	r.CoverageDen = parseInt64(get(FieldVectorCoverageDen))

	if r.CoverageDen > 0 {
		r.CoveragePct = float64(r.CoverageNum) / float64(r.CoverageDen) * 100.0
	}

	r.EmbedErrorCount = parseInt64(get(FieldEmbedErrorCount))
	r.LastEncodeError = get(FieldEmbedLastError)

	if ts := get(FieldEmbedLastErrorAt); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			r.LastEncodeErrAt = &t
		}
	}
	if ts := get(FieldEmbedLastOKAt); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			r.LastEncodeOKAt = &t
		}
	}

	// Count pending and stale entities. Corpora that don't track
	// state (VectorStateColumn() == "") skip these queries: their
	// health report reports zero for both counts, which is the
	// correct answer in a no-state-tracking model.
	if stateCol := corpus.VectorStateColumn(); stateCol != "" {
		activePred := corpus.ActivePredicate()
		if activePred == "" {
			activePred = "1=1"
		}
		pendingQuery := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s = 'pending' AND %s`, corpus.EntityTable(), stateCol, activePred) //nolint:gosec // G201: all substitutions are compile-time corpus accessors, not user input.
		if err := db.QueryRowContext(ctx, pendingQuery).Scan(&r.PendingCount); err != nil {                                            //nolint:sqlcheck // all parts are compile-time corpus accessors.
			slog.WarnContext(ctx, "embed: health: pending count query failed", "err", err)
		}

		staleQuery := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s = 'stale' AND %s`, corpus.EntityTable(), stateCol, activePred) //nolint:gosec // G201: all substitutions are compile-time corpus accessors, not user input.
		if err := db.QueryRowContext(ctx, staleQuery).Scan(&r.StaleCount); err != nil {                                            //nolint:sqlcheck // all parts are compile-time corpus accessors.
			slog.WarnContext(ctx, "embed: health: stale count query failed", "err", err)
		}
	}

	r.HealthClass = classifyHealth(r)
	return r, nil
}

// classifyHealth derives the healthClass from a HealthReport's fields.
// Order of precedence: disabled > repeated failures > backfilling > stale > healthy.
func classifyHealth(r *HealthReport) healthClass {
	if r.State != EmbedderStateEnabled {
		return healthClassDisabled
	}
	if r.EmbedErrorCount >= repeatedFailureThreshold {
		return healthClassRepeated
	}
	cov := 0.0
	if r.CoverageDen > 0 {
		cov = float64(r.CoverageNum) / float64(r.CoverageDen)
	}
	if cov < backfillCoverageThreshold && (r.PendingCount > 0 || r.CoverageDen == 0) {
		return healthClassBackfilling
	}
	if r.StaleCount > 0 {
		return healthClassStale
	}
	return healthClassHealthy
}

// SessionLine returns the compact text to emit in guild_session_start.
// Returns an empty string when the embedder is healthy (no noise on normal sessions).
func (r *HealthReport) SessionLine() string {
	switch r.HealthClass {
	case healthClassDisabled:
		reason := disabledReason(r)
		if reason != "" {
			return fmt.Sprintf("embedder: disabled (%s)", reason)
		}
		return "embedder: disabled"

	case healthClassBackfilling:
		pct := 0.0
		if r.CoverageDen > 0 {
			pct = float64(r.CoverageNum) / float64(r.CoverageDen) * 100.0
		}
		eta := backfillETA(r.PendingCount)
		return fmt.Sprintf("embedder: backfilling (coverage %.0f%%, ETA ~%s)", pct, eta)

	case healthClassStale:
		return fmt.Sprintf("embedder: stale vectors present (%d rows, run `guild lore embed-rebuild`)", r.StaleCount)

	case healthClassRepeated:
		return fmt.Sprintf("embedder: repeated write failures (%d in the last hour, run `guild lore health`)", r.EmbedErrorCount)

	default:
		// healthClassHealthy: emit nothing.
		return ""
	}
}

// disabledReason returns the parenthesised tag for the session-start line.
// It reads the stored meta.embedder_state_reason first; if absent it falls
// back to inspecting embed_last_error for legacy DBs written before
// embedder_state_reason was added.
//
// Known reason values from PrepareAndProbe / WriteMeta:
//
//	unsupported_platform, probe_mismatch, extract_failed, no_assets,
//	embedder_init_failed, probe_error, binary_outdated
//
// Unknown future values are rendered verbatim for forward compatibility.
// Returns "" only when no reason info is available at all.
func disabledReason(r *HealthReport) string {
	if r.DisabledReason != "" {
		// Map the stored machine tag to the display form prescribed by ADR-003.
		switch r.DisabledReason {
		case "unsupported_platform":
			return "unsupported_platform"
		case "probe_mismatch":
			return "probe_mismatch"
		case "extract_failed":
			return "extract_failed"
		case "no_assets":
			return "no_assets"
		case "embedder_init_failed":
			return "init_failed"
		case "probe_error":
			return "probe_error"
		case "binary_outdated":
			return "binary_outdated"
		case "ok":
			// Should not occur when state=disabled, but guard cleanly.
			return ""
		default:
			// Forward-compat: unknown future reason rendered verbatim.
			return r.DisabledReason
		}
	}
	// Legacy fallback for DBs that predate embedder_state_reason: inspect
	// embed_last_error as a heuristic only.
	if r.LastEncodeError != "" {
		return fmt.Sprintf("dylib probe failed: %s", r.LastEncodeError)
	}
	return ""
}

// backfillETA returns a rough ETA string for the remaining pending entries.
// Uses the measured per-entry encode cost from the spike (18.5 ms p50 doc-embed).
func backfillETA(pending int64) string {
	if pending <= 0 {
		return "0s"
	}
	// 18.5 ms per entry from spike measurements (ADR-003).
	const msPerEntry = 18.5
	totalMs := float64(pending) * msPerEntry
	dur := time.Duration(totalMs) * time.Millisecond
	switch {
	case dur < time.Second:
		return "<1s"
	case dur < time.Minute:
		return fmt.Sprintf("%ds", int(dur.Seconds()))
	default:
		return fmt.Sprintf("%dm", int(dur.Minutes()))
	}
}

// parseInt64 parses a decimal string to int64; returns 0 on failure.
func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// RebuildVectors resets all lore_vectors rows for a project and re-encodes
// every active entry using the provided Embedder. This is a local helper
// implemented here for QUEST-211; once QUEST-210 lands backfill.go this
// function will be replaced by (or aliased to) the shared Backfill helper.
//
// Transaction semantics follow ADR-003 invariants:
//   - The reset (DELETE + UPDATE) uses BEGIN IMMEDIATE to prevent concurrent
//     writers from racing against the state flip.
//   - Each vector INSERT uses INSERT OR IGNORE (entry_id is PK) so concurrent
//     backfill attempts are idempotent.
//
// The function quantizes each float32 vector to int8 before storage.
// It updates meta.vector_coverage_num and meta.vector_epoch after the full
// encode pass.
func RebuildVectors(ctx context.Context, db *sql.DB, projectID string, embedder Embedder, modelID string) error {
	if db == nil {
		return fmt.Errorf("embed: rebuildVectors: nil db")
	}

	// Phase 1: reset transaction using BEGIN IMMEDIATE (ADR-003).
	// Serializes concurrent writers so no reader sees a half-reset state.
	conn, rollback, err := beginImmediateConn(ctx, db, "rebuildVectors-reset")
	if err != nil {
		return err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	// Delete all vectors for this project's entries.
	if _, err := conn.ExecContext(ctx,
		`DELETE FROM lore_vectors WHERE entry_id IN (
		   SELECT id FROM entries WHERE project_id = ?
		 )`, projectID,
	); err != nil {
		return fmt.Errorf("embed: rebuildVectors: delete vectors: %w", err)
	}

	// Flip all active entries to pending.
	if _, err := conn.ExecContext(ctx,
		`UPDATE entries SET vector_state = 'pending'
		  WHERE project_id = ?
		    AND status NOT IN ('archived', 'parked')`,
		projectID,
	); err != nil {
		return fmt.Errorf("embed: rebuildVectors: reset vector_state: %w", err)
	}

	// Reset coverage_num to 0 (den stays; we don't change which entries are active).
	if _, err := conn.ExecContext(ctx,
		`UPDATE meta SET value = '0' WHERE key = 'vector_coverage_num'`,
	); err != nil {
		return fmt.Errorf("embed: rebuildVectors: reset coverage_num: %w", err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("embed: rebuildVectors: commit reset tx: %w", err)
	}
	committed = true

	// Phase 2: encode and insert each pending entry.
	// Fetch pending entries for the project.
	rows, err := db.QueryContext(ctx,
		`SELECT id, summary FROM entries
		  WHERE project_id = ?
		    AND vector_state = 'pending'
		    AND status NOT IN ('archived', 'parked')
		  ORDER BY id`,
		projectID,
	)
	if err != nil {
		return fmt.Errorf("embed: rebuildVectors: query pending: %w", err)
	}
	type pendingEntry struct {
		id      int64
		summary string
	}
	var pending []pendingEntry
	for rows.Next() {
		var e pendingEntry
		if err := rows.Scan(&e.id, &e.summary); err != nil {
			_ = rows.Close()
			return fmt.Errorf("embed: rebuildVectors: scan pending: %w", err)
		}
		pending = append(pending, e)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("embed: rebuildVectors: iterate pending: %w", err)
	}

	dim := embedder.Dimension()
	encoded := int64(0)
	now := time.Now().UTC().Unix()

	for _, e := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		vec, err := embedder.Embed(ctx, e.summary)
		if err != nil {
			slog.WarnContext(ctx, "embed: rebuildVectors: encode failed",
				"entry_id", e.id, "err", err)
			continue
		}

		// Quantize float32 -> int8.
		blob := quantizeInt8(vec)

		// Compute content hash (SHA-256 of the summary text).
		contentHash := contentHashOf(e.summary)

		// Each vector insert is its own immediate transaction so concurrent
		// MCP servers can make progress without one long lock. INSERT OR IGNORE
		// per ADR-003 invariant 1: if another process already encoded this entry
		// the insert is silently skipped.
		insConn, insRollback, err := beginImmediateConn(ctx, db, "rebuildVectors-insert")
		if err != nil {
			slog.WarnContext(ctx, "embed: rebuildVectors: begin insert tx",
				"entry_id", e.id, "err", err)
			continue
		}
		insCommitted := false

		_, err = insConn.ExecContext(ctx,
			`INSERT OR IGNORE INTO lore_vectors
			   (entry_id, model_id, dim, vec, encoded_at, content_hash)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			e.id, modelID, dim, blob, now, contentHash,
		)
		if err != nil {
			insRollback(&insCommitted)
			_ = insConn.Close()
			slog.WarnContext(ctx, "embed: rebuildVectors: insert vector",
				"entry_id", e.id, "err", err)
			continue
		}

		_, err = insConn.ExecContext(ctx,
			`UPDATE entries SET vector_state = 'indexed' WHERE id = ?`,
			e.id,
		)
		if err != nil {
			insRollback(&insCommitted)
			_ = insConn.Close()
			slog.WarnContext(ctx, "embed: rebuildVectors: update vector_state",
				"entry_id", e.id, "err", err)
			continue
		}

		if _, err := insConn.ExecContext(ctx, "COMMIT"); err != nil {
			insRollback(&insCommitted)
			_ = insConn.Close()
			slog.WarnContext(ctx, "embed: rebuildVectors: commit insert tx",
				"entry_id", e.id, "err", err)
			continue
		}
		insCommitted = true
		_ = insConn.Close()

		encoded++
	}

	// Phase 3: update meta counters in one final immediate transaction.
	metaConn, metaRollback, err := beginImmediateConn(ctx, db, "rebuildVectors-meta")
	if err != nil {
		return fmt.Errorf("embed: rebuildVectors: begin meta tx: %w", err)
	}
	defer metaConn.Close()
	metaCommitted := false
	defer metaRollback(&metaCommitted)

	if _, err := metaConn.ExecContext(ctx,
		`UPDATE meta SET value = ? WHERE key = 'vector_coverage_num'`,
		strconv.FormatInt(encoded, 10),
	); err != nil {
		return fmt.Errorf("embed: rebuildVectors: update coverage_num: %w", err)
	}

	if _, err := metaConn.ExecContext(ctx,
		`UPDATE meta SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)
		  WHERE key = 'vector_epoch'`,
	); err != nil {
		return fmt.Errorf("embed: rebuildVectors: bump epoch: %w", err)
	}

	if _, err := metaConn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("embed: rebuildVectors: commit meta tx: %w", err)
	}
	metaCommitted = true

	slog.InfoContext(ctx, "embed: rebuildVectors complete",
		"project", projectID, "encoded", encoded, "total_pending", len(pending))
	return nil
}

// beginImmediateConn acquires a *sql.Conn from db and issues BEGIN IMMEDIATE
// with a capped backoff retry. Returns the pinned conn (caller must Close()
// when done) and a rollback closure (call with &committed before conn.Close()).
func beginImmediateConn(ctx context.Context, db *sql.DB, opName string) (*sql.Conn, func(*bool), error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("embed: %s: acquire conn: %w", opName, err)
	}

	const maxAttempts = 20
	var beginErr error
	for attempt := range maxAttempts {
		_, beginErr = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
		if beginErr == nil {
			break
		}
		if !isEmbedBusyErr(beginErr.Error()) {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("embed: %s: begin immediate: %w", opName, beginErr)
		}
		base := time.Duration(attempt+1) * 10 * time.Millisecond
		if base > 200*time.Millisecond {
			base = 200 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			_ = conn.Close()
			return nil, nil, fmt.Errorf("embed: %s: begin immediate: %w", opName, ctx.Err())
		case <-time.After(base):
		}
	}
	if beginErr != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("embed: %s: begin immediate (contended after %d attempts): %w",
			opName, maxAttempts, beginErr)
	}

	rollback := func(committed *bool) { //nolint:contextcheck // rollback must survive caller cancellation
		if !*committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}
	return conn, rollback, nil
}

// isEmbedBusyErr reports whether the SQLite error string indicates a busy/
// locked state that should be retried.
func isEmbedBusyErr(msg string) bool {
	return strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "database is locked")
}

// quantizeInt8 converts a float32 vector to a raw int8 blob using symmetric
// per-vector quantization (scale = max(abs) / 127). Matches the int8
// storage convention referenced in ADR-003.
func quantizeInt8(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	maxAbs := float32(0)
	for _, x := range v {
		a := x
		if a < 0 {
			a = -a
		}
		if a > maxAbs {
			maxAbs = a
		}
	}
	out := make([]byte, len(v))
	if maxAbs == 0 {
		return out
	}
	scale := 127.0 / maxAbs
	for i, x := range v {
		q := int8(x * scale)
		out[i] = byte(q)
	}
	return out
}

// contentHashOf returns a hex-encoded SHA-256 of the text, used as the
// content_hash field in lore_vectors for staleness detection.
func contentHashOf(text string) string {
	h := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%x", h[:])
}
