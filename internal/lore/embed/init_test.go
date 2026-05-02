// init_test.go: regression tests for WriteMeta tokenizer_hash lifecycle.
//
// Decision (Option A, extract-time identity): embedder_tokenizer_hash,
// embedder_model_id, and embedder_runtime_version are written whenever
// extract succeeds, regardless of probe outcome. These three fields are
// deterministic digests of the bundled bytes; probe success is orthogonal
// to their correctness. Blank hash during probe_mismatch (LORE-365) made
// it impossible to tell at a glance whether extraction had succeeded.
//
// Covered cases:
//   - Extract succeeds, probe fails: hash is populated (not blank).
//   - Extract fails: hash stays blank (write site never reached).
//   - Extract and probe both succeed: hash populated (both options agree).
//   - Re-run with same Identity: same hash (idempotence).

package embed

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// noopLogger returns a slog.Logger that discards all output.
func noopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestWriteMeta_ExtractSucceedsProbeFailsHashPopulated verifies Option A:
// when extract succeeds but probe fails, tokenizer_hash is still written.
func TestWriteMeta_ExtractSucceedsProbeFailsHashPopulated(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	log := noopLogger()

	// Simulate: extract succeeded (Identity is populated), probe failed.
	outcome := InitOutcome{
		State:  "disabled",
		Reason: "probe_mismatch",
		Identity: ManifestIdentity{
			ModelID:        "bge-small-en-v1.5-int8-cls",
			TokenizerHash:  "deadbeef1234",
			RuntimeVersion: "onnxruntime-1.23.0",
			Dim:            Dim,
		},
	}

	if err := WriteMeta(ctx, log, db, outcome); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}

	// tokenizer_hash must be populated (Option A: extract-time semantics).
	var hash string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'embedder_tokenizer_hash'`,
	).Scan(&hash); err != nil {
		t.Fatalf("read embedder_tokenizer_hash: %v", err)
	}
	if hash != "deadbeef1234" {
		t.Errorf("embedder_tokenizer_hash = %q, want %q (should be populated on extract even when probe failed)", hash, "deadbeef1234")
	}

	// model_id and runtime_version also populated.
	var modelID string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'embedder_model_id'`,
	).Scan(&modelID); err != nil {
		t.Fatalf("read embedder_model_id: %v", err)
	}
	if modelID != "bge-small-en-v1.5-int8-cls" {
		t.Errorf("embedder_model_id = %q, want bge-small-en-v1.5-int8-cls", modelID)
	}

	var rtVersion string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'embedder_runtime_version'`,
	).Scan(&rtVersion); err != nil {
		t.Fatalf("read embedder_runtime_version: %v", err)
	}
	if rtVersion != "onnxruntime-1.23.0" {
		t.Errorf("embedder_runtime_version = %q, want onnxruntime-1.23.0", rtVersion)
	}

	// state and reason are also written.
	var state string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'embedder_state'`,
	).Scan(&state); err != nil {
		t.Fatalf("read embedder_state: %v", err)
	}
	if state != "disabled" {
		t.Errorf("embedder_state = %q, want disabled", state)
	}

	// embedder_dim must NOT be written (probe did not pass).
	var dim string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'embedder_dim'`,
	).Scan(&dim)
	if err == nil {
		t.Errorf("embedder_dim should not be written on probe failure, got %q", dim)
	}
}

// TestWriteMeta_ExtractFailsHashStaysBlank verifies that when extract fails
// (Identity is zero-value / ModelID empty), tokenizer_hash is not touched.
func TestWriteMeta_ExtractFailsHashStaysBlank(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	log := noopLogger()

	// Seed a blank hash row to simulate what the migration writes.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES ('embedder_tokenizer_hash', '')`,
	); err != nil {
		t.Fatalf("seed blank hash: %v", err)
	}

	// Simulate: extract failed before Identity was populated.
	outcome := InitOutcome{
		State:  "disabled",
		Reason: "extract_failed",
		// Identity is zero-value: ModelID == "" signals extract never ran.
	}

	if err := WriteMeta(ctx, log, db, outcome); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}

	var hash string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'embedder_tokenizer_hash'`,
	).Scan(&hash); err != nil {
		t.Fatalf("read embedder_tokenizer_hash: %v", err)
	}
	if hash != "" {
		t.Errorf("embedder_tokenizer_hash = %q, want empty (extract never reached write site)", hash)
	}
}

// TestWriteMeta_ExtractAndProbeSucceedHashPopulated verifies the happy path:
// both extract and probe succeed, hash is populated and embedder_dim is written.
func TestWriteMeta_ExtractAndProbeSucceedHashPopulated(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	log := noopLogger()

	outcome := InitOutcome{
		State:  "enabled",
		Reason: "ok",
		Identity: ManifestIdentity{
			ModelID:        "bge-small-en-v1.5-int8-cls",
			TokenizerHash:  "cafecafe5678",
			RuntimeVersion: "onnxruntime-1.23.0",
			Dim:            Dim,
		},
	}

	if err := WriteMeta(ctx, log, db, outcome); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}

	var hash string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'embedder_tokenizer_hash'`,
	).Scan(&hash); err != nil {
		t.Fatalf("read embedder_tokenizer_hash: %v", err)
	}
	if hash != "cafecafe5678" {
		t.Errorf("embedder_tokenizer_hash = %q, want cafecafe5678", hash)
	}

	// embedder_dim IS written when probe passes.
	var dim string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'embedder_dim'`,
	).Scan(&dim); err != nil {
		t.Fatalf("read embedder_dim: %v", err)
	}
	if dim != "384" {
		t.Errorf("embedder_dim = %q, want 384", dim)
	}
}

// TestWriteMeta_Idempotent verifies that running WriteMeta twice with the same
// Identity leaves tokenizer_hash unchanged (same hash, not a duplicate-key error).
func TestWriteMeta_Idempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	log := noopLogger()

	outcome := InitOutcome{
		State:  "enabled",
		Reason: "ok",
		Identity: ManifestIdentity{
			ModelID:        "bge-small-en-v1.5-int8-cls",
			TokenizerHash:  "aabbccdd",
			RuntimeVersion: "onnxruntime-1.23.0",
			Dim:            Dim,
		},
	}

	if err := WriteMeta(ctx, log, db, outcome); err != nil {
		t.Fatalf("WriteMeta first call: %v", err)
	}
	if err := WriteMeta(ctx, log, db, outcome); err != nil {
		t.Fatalf("WriteMeta second call (idempotent): %v", err)
	}

	var hash string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'embedder_tokenizer_hash'`,
	).Scan(&hash); err != nil {
		t.Fatalf("read embedder_tokenizer_hash: %v", err)
	}
	if hash != "aabbccdd" {
		t.Errorf("embedder_tokenizer_hash after idempotent run = %q, want aabbccdd", hash)
	}
}
