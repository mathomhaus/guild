package embed

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	_ "modernc.org/sqlite"
)

// openTestDB creates an in-memory SQLite database with the minimal schema
// needed for health tests: meta and entries tables with vector_state, plus
// lore_vectors.
//
// Uses file::memory:?cache=shared so db.Conn() calls see the same database
// (pure ":memory:" gives each new connection its own isolated database, which
// breaks the multiple-connection patterns in RebuildVectors).
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	// Unique name per test so parallel tests don't collide on the shared cache.
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}

	schema := `
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS entries (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id   TEXT NOT NULL DEFAULT 'test',
  summary      TEXT NOT NULL DEFAULT '',
  vector_state TEXT NOT NULL DEFAULT 'pending',
  status       TEXT NOT NULL DEFAULT 'current'
);
CREATE TABLE IF NOT EXISTS lore_vectors (
  entry_id     INTEGER PRIMARY KEY REFERENCES entries(id) ON DELETE CASCADE,
  model_id     TEXT    NOT NULL,
  dim          INTEGER NOT NULL,
  vec          BLOB    NOT NULL,
  encoded_at   INTEGER NOT NULL,
  content_hash TEXT    NOT NULL
);
`
	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		_ = db.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedMeta inserts the standard meta rows.
func seedMeta(t *testing.T, db *sql.DB, state EmbedderState, coverageNum, coverageDen, errCount int64) {
	t.Helper()
	ctx := context.Background()
	rows := []struct{ k, v string }{
		{"embedder_model_id", "bge-small-en-v1.5-int8-cls"},
		{"embedder_tokenizer_hash", "abc123"},
		{"embedder_runtime_version", "onnxruntime-1.23.x"},
		{"embedder_dim", "384"},
		{"embedder_state", string(state)},
		{"vector_epoch", "1"},
		{"vector_coverage_num", fmt.Sprintf("%d", coverageNum)},
		{"vector_coverage_den", fmt.Sprintf("%d", coverageDen)},
		{"embed_error_count", fmt.Sprintf("%d", errCount)},
	}
	for _, r := range rows {
		if _, err := db.ExecContext(ctx,
			`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, r.k, r.v,
		); err != nil {
			t.Fatalf("seed meta %s: %v", r.k, err)
		}
	}
}

// seedEntriesWithState inserts n entries with the given vector_state.
// Distinct from backfill_test.go's seedEntries helper (same package),
// which pre-existed and returns IDs without a state parameter.
func seedEntriesWithState(t *testing.T, db *sql.DB, n int, state string) {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO entries (project_id, summary, vector_state, status)
			 VALUES ('test', ?, ?, 'current')`,
			fmt.Sprintf("entry %d", i), state,
		); err != nil {
			t.Fatalf("seed entry: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Health report reads
// ---------------------------------------------------------------------------

func TestReadHealthReport_Healthy(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedMeta(t, db, EmbedderStateEnabled, 10, 10, 0)

	r, err := ReadHealthReport(ctx, db, LoreCorpus{})
	if err != nil {
		t.Fatalf("ReadHealthReport: %v", err)
	}

	if r.ModelID != "bge-small-en-v1.5-int8-cls" {
		t.Errorf("ModelID = %q, want bge-small-en-v1.5-int8-cls", r.ModelID)
	}
	if r.State != EmbedderStateEnabled {
		t.Errorf("State = %q, want enabled", r.State)
	}
	if r.CoverageNum != 10 {
		t.Errorf("CoverageNum = %d, want 10", r.CoverageNum)
	}
	if r.CoverageDen != 10 {
		t.Errorf("CoverageDen = %d, want 10", r.CoverageDen)
	}
	if r.HealthClass != healthClassHealthy {
		t.Errorf("HealthClass = %d, want healthClassHealthy", r.HealthClass)
	}
}

func TestReadHealthReport_ReadsMetaFaithfully(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedMeta(t, db, EmbedderStateDisabled, 3, 10, 2)

	// Store a last-error in meta.
	if _, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO meta (key, value) VALUES ('embed_last_error', 'dylib probe failed: libonnxruntime.dylib not found')`,
	); err != nil {
		t.Fatalf("seed embed_last_error: %v", err)
	}

	r, err := ReadHealthReport(ctx, db, LoreCorpus{})
	if err != nil {
		t.Fatalf("ReadHealthReport: %v", err)
	}

	if r.TokenizerHash != "abc123" {
		t.Errorf("TokenizerHash = %q, want abc123", r.TokenizerHash)
	}
	if r.RuntimeVersion != "onnxruntime-1.23.x" {
		t.Errorf("RuntimeVersion = %q, want onnxruntime-1.23.x", r.RuntimeVersion)
	}
	if r.Dim != 384 {
		t.Errorf("Dim = %d, want 384", r.Dim)
	}
	if r.EmbedErrorCount != 2 {
		t.Errorf("EmbedErrorCount = %d, want 2", r.EmbedErrorCount)
	}
	if r.LastEncodeError != "dylib probe failed: libonnxruntime.dylib not found" {
		t.Errorf("LastEncodeError = %q, unexpected", r.LastEncodeError)
	}
}

func TestReadHealthReport_PendingAndStaleCount(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedMeta(t, db, EmbedderStateEnabled, 2, 7, 0)
	seedEntriesWithState(t, db, 3, "pending")
	seedEntriesWithState(t, db, 2, "stale")

	r, err := ReadHealthReport(ctx, db, LoreCorpus{})
	if err != nil {
		t.Fatalf("ReadHealthReport: %v", err)
	}
	if r.PendingCount != 3 {
		t.Errorf("PendingCount = %d, want 3", r.PendingCount)
	}
	if r.StaleCount != 2 {
		t.Errorf("StaleCount = %d, want 2", r.StaleCount)
	}
}

// ---------------------------------------------------------------------------
// Session-start line variants (table-driven)
// ---------------------------------------------------------------------------

func TestSessionLine_HealthyEmitsNothing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// Fully covered corpus, no errors.
	seedMeta(t, db, EmbedderStateEnabled, 100, 100, 0)

	r, err := ReadHealthReport(ctx, db, LoreCorpus{})
	if err != nil {
		t.Fatalf("ReadHealthReport: %v", err)
	}
	if line := r.SessionLine(); line != "" {
		t.Errorf("healthy state should emit no session-start line; got %q", line)
	}
}

// TestSessionLine_Variants tests each of the non-healthy session-start line
// shapes using a table-driven approach. Each row sets up meta/DB state and
// checks the exact format prescribed by ADR-003 "Failure visibility".
func TestSessionLine_Variants(t *testing.T) {
	type tc struct {
		name        string
		state       EmbedderState
		stateReason string // written to meta.embedder_state_reason when non-empty
		covNum      int64
		covDen      int64
		errCount    int64
		pending     int
		stale       int
		lastErr     string
		wantExact   string // exact line, when non-empty (takes priority over wantStart/wantSub)
		wantStart   string // prefix the line must start with
		wantSub     string // substring the line must contain
	}

	cases := []tc{
		// Seven known reasons from PrepareAndProbe / WriteMeta.
		{
			name:        "disabled_unsupported_platform",
			state:       EmbedderStateDisabled,
			stateReason: "unsupported_platform",
			covDen:      10,
			wantExact:   "embedder: disabled (unsupported_platform)",
		},
		{
			name:        "disabled_probe_mismatch",
			state:       EmbedderStateDisabled,
			stateReason: "probe_mismatch",
			covDen:      10,
			wantExact:   "embedder: disabled (probe_mismatch)",
		},
		{
			name:        "disabled_extract_failed",
			state:       EmbedderStateDisabled,
			stateReason: "extract_failed",
			covDen:      10,
			wantExact:   "embedder: disabled (extract_failed)",
		},
		{
			name:        "disabled_no_assets",
			state:       EmbedderStateDisabled,
			stateReason: "no_assets",
			covDen:      10,
			wantExact:   "embedder: disabled (no_assets)",
		},
		{
			name:        "disabled_embedder_init_failed",
			state:       EmbedderStateDisabled,
			stateReason: "embedder_init_failed",
			covDen:      10,
			wantExact:   "embedder: disabled (init_failed)",
		},
		{
			name:        "disabled_probe_error",
			state:       EmbedderStateDisabled,
			stateReason: "probe_error",
			covDen:      10,
			wantExact:   "embedder: disabled (probe_error)",
		},
		{
			name:        "disabled_binary_outdated",
			state:       EmbedderStateDisabled,
			stateReason: "binary_outdated",
			covDen:      10,
			wantExact:   "embedder: disabled (binary_outdated)",
		},
		// Forward-compat: unknown future reason is rendered verbatim.
		{
			name:        "disabled_unknown_reason_verbatim",
			state:       EmbedderStateDisabled,
			stateReason: "some_future_reason",
			covDen:      10,
			wantExact:   "embedder: disabled (some_future_reason)",
		},
		// Legacy fallback: no state_reason stored, but embed_last_error present.
		{
			name:      "disabled_legacy_last_error",
			state:     EmbedderStateDisabled,
			covDen:    10,
			lastErr:   "libonnxruntime.dylib failed to load",
			wantStart: "embedder: disabled",
			wantSub:   "dylib probe failed",
		},
		// Disabled with no reason and no last error: bare "embedder: disabled".
		{
			name:      "disabled_no_reason_at_all",
			state:     EmbedderStateDisabled,
			covDen:    10,
			wantExact: "embedder: disabled",
		},
		// Non-disabled states.
		{
			name:      "backfilling",
			state:     EmbedderStateEnabled,
			covNum:    47,
			covDen:    100,
			pending:   53,
			wantStart: "embedder: backfilling",
			wantSub:   "47%",
		},
		{
			name:      "stale_vectors",
			state:     EmbedderStateEnabled,
			covNum:    90,
			covDen:    100,
			stale:     43,
			wantStart: "embedder: stale vectors present",
			wantSub:   "43 rows",
		},
		{
			name:      "repeated_failures",
			state:     EmbedderStateEnabled,
			covNum:    95,
			covDen:    100,
			errCount:  12,
			wantStart: "embedder: repeated write failures",
			wantSub:   "12",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			seedMeta(t, db, tc.state, tc.covNum, tc.covDen, tc.errCount)

			if tc.stateReason != "" {
				_, _ = db.ExecContext(ctx,
					`INSERT OR REPLACE INTO meta (key, value) VALUES ('embedder_state_reason', ?)`,
					tc.stateReason,
				)
			}
			if tc.lastErr != "" {
				_, _ = db.ExecContext(ctx,
					`INSERT OR REPLACE INTO meta (key, value) VALUES ('embed_last_error', ?)`,
					tc.lastErr,
				)
			}
			if tc.pending > 0 {
				seedEntriesWithState(t, db, tc.pending, "pending")
			}
			if tc.stale > 0 {
				seedEntriesWithState(t, db, tc.stale, "stale")
			}

			r, err := ReadHealthReport(ctx, db, LoreCorpus{})
			if err != nil {
				t.Fatalf("ReadHealthReport: %v", err)
			}

			line := r.SessionLine()
			if line == "" {
				t.Fatalf("expected non-empty session-start line for %s; got empty", tc.name)
			}

			if tc.wantExact != "" {
				if line != tc.wantExact {
					t.Errorf("line = %q, want exact %q", line, tc.wantExact)
				}
				return
			}
			if tc.wantStart != "" {
				if len(line) < len(tc.wantStart) || line[:len(tc.wantStart)] != tc.wantStart {
					t.Errorf("line %q does not start with %q", line, tc.wantStart)
				}
			}
			if tc.wantSub != "" && !containsStr(line, tc.wantSub) {
				t.Errorf("line %q missing expected substring %q", line, tc.wantSub)
			}
		})
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || sub == "" || containsStrSlow(s, sub))
}

func containsStrSlow(s, sub string) bool {
	for i := range s {
		if i+len(sub) > len(s) {
			break
		}
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// embed-rebuild zeros coverage_num then restores to den
// ---------------------------------------------------------------------------

func TestRebuildVectors_ZerosThenRestores(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Seed 5 active entries.
	seedMeta(t, db, EmbedderStateEnabled, 5, 5, 0)
	seedEntriesWithState(t, db, 5, "indexed")

	// Rebuild with a DeterministicEmbedder (not NullEmbedder so encodes succeed).
	e := NewDeterministicEmbedder()
	if err := RebuildVectors(ctx, db, "test", e, "bge-small-en-v1.5-int8-cls"); err != nil {
		t.Fatalf("RebuildVectors: %v", err)
	}

	// After rebuild: coverage_num should equal number of successfully encoded entries.
	var numStr string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'vector_coverage_num'`,
	).Scan(&numStr); err != nil {
		t.Fatalf("read coverage_num: %v", err)
	}
	if numStr != "5" {
		t.Errorf("coverage_num after rebuild = %q, want 5", numStr)
	}

	// Check that lore_vectors has 5 rows.
	var cnt int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM lore_vectors`).Scan(&cnt); err != nil {
		t.Fatalf("count lore_vectors: %v", err)
	}
	if cnt != 5 {
		t.Errorf("lore_vectors count = %d, want 5", cnt)
	}
}

func TestRebuildVectors_NullEmbedderZerosCoverage(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const numEntries = 3

	// Start with fully covered corpus.
	seedMeta(t, db, EmbedderStateEnabled, int64(numEntries), int64(numEntries), 0)
	seedEntriesWithState(t, db, numEntries, "indexed")

	// RebuildVectors with NullEmbedder: encodes will all fail so coverage_num ends at 0.
	e := NewNullEmbedder()
	if err := RebuildVectors(ctx, db, "test", e, "bge-small-en-v1.5-int8-cls"); err != nil {
		t.Fatalf("RebuildVectors with NullEmbedder: %v", err)
	}

	// coverage_num should be 0 because NullEmbedder fails every encode.
	var numStr string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'vector_coverage_num'`,
	).Scan(&numStr); err != nil {
		t.Fatalf("read coverage_num: %v", err)
	}
	if numStr != "0" {
		t.Errorf("coverage_num after NullEmbedder rebuild = %q, want 0", numStr)
	}

	// All entries should be 'pending' (reset ran, no encode succeeded).
	var pendingCnt int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entries WHERE vector_state = 'pending'`,
	).Scan(&pendingCnt); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pendingCnt != numEntries {
		t.Errorf("pending count after NullEmbedder = %d, want %d", pendingCnt, numEntries)
	}
}

// TestQuantizeInt8_RoundTrip checks that quantize produces a blob of the
// right length with no panics on all-zero, max, and negative inputs.
func TestQuantizeInt8_RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   []float32
	}{
		{"zero", make([]float32, Dim)},
		{"ones", filledFloat32(Dim, 1.0)},
		{"neg", filledFloat32(Dim, -1.0)},
		{"short", []float32{0.1, -0.2, 0.3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			blob := quantizeInt8(tc.in)
			if len(blob) != len(tc.in) {
				t.Errorf("len(blob) = %d, want %d", len(blob), len(tc.in))
			}
		})
	}
}

func filledFloat32(n int, v float32) []float32 {
	s := make([]float32, n)
	for i := range s {
		s[i] = v
	}
	return s
}
