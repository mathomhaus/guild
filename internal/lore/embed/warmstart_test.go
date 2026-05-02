// warmstart_test.go: regression tests for the CLI warm-start fast path.
//
// MaybeSkipProbe returns Skip=true only when all three conditions hold:
//   - meta.embedder_state = 'enabled'
//   - meta.embedder_model_id non-empty and equal to the manifest's ModelID
//   - meta.embedder_tokenizer_hash either empty (grace period) or equal to
//     the manifest's TokenizerHash
//
// On any miss the caller must fall through to PrepareAndProbe unchanged.
//
// WarmStartEmbedder is only tested for the no-assets path (bundled bytes
// are only present under -tags=withembed). The full extract+construct path
// is covered by the bundled integration tests (parity_bundled_test.go).

package embed

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
)

// countingProbe is a fake Embedder that records how many times Embed is called.
// Used to assert that the warm-start path does NOT call the probe.
type countingProbe struct {
	calls atomic.Int64
}

func (c *countingProbe) Embed(_ context.Context, _ string) ([]float32, error) {
	c.calls.Add(1)
	// Return a valid vector so RunProbe would pass if it were called.
	v := make([]float32, Dim)
	for i := range v {
		v[i] = 0.01
	}
	return v, nil
}

func (c *countingProbe) Dimension() int { return Dim }

// TestMaybeSkipProbe_IdentityMatch verifies the core fast-path contract:
// when state='enabled' and identity matches, Skip=true and reason="identity_match".
func TestMaybeSkipProbe_IdentityMatch(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	ctx := context.Background()
	log := noopLogger()

	// Write meta rows that match the manifest we'll pass in.
	for _, kv := range []struct{ k, v string }{
		{"embedder_state", "enabled"},
		{"embedder_model_id", "bge-small-en-v1.5-int8-cls"},
		{"embedder_tokenizer_hash", "abc123tokenhash"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO meta (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			kv.k, kv.v,
		); err != nil {
			t.Fatalf("seed meta %s: %v", kv.k, err)
		}
	}

	man := Manifest{
		Identity: ManifestIdentity{
			ModelID:       "bge-small-en-v1.5-int8-cls",
			TokenizerHash: "abc123tokenhash",
			PlatformTag:   "darwin-arm64",
		},
	}

	res := MaybeSkipProbe(ctx, db, man, log)

	if !res.Skip {
		t.Errorf("MaybeSkipProbe.Skip = false, want true when identity matches")
	}
	if res.Reason != "identity_match" {
		t.Errorf("MaybeSkipProbe.Reason = %q, want %q", res.Reason, "identity_match")
	}
	if res.StoredModelID != "bge-small-en-v1.5-int8-cls" {
		t.Errorf("StoredModelID = %q, want bge-small-en-v1.5-int8-cls", res.StoredModelID)
	}
}

// TestMaybeSkipProbe_ProbeNotCalledOnWarmHit verifies via a counting fake
// that when MaybeSkipProbe returns Skip=true the caller is not obliged to
// call the probe. This is the behavioral contract: the counting embedder's
// call count must stay at zero through the fast-path decision.
func TestMaybeSkipProbe_ProbeNotCalledOnWarmHit(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	ctx := context.Background()
	log := noopLogger()

	for _, kv := range []struct{ k, v string }{
		{"embedder_state", "enabled"},
		{"embedder_model_id", "bge-small-en-v1.5-int8-cls"},
		{"embedder_tokenizer_hash", "stablehash"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO meta (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			kv.k, kv.v,
		); err != nil {
			t.Fatalf("seed meta %s: %v", kv.k, err)
		}
	}

	man := Manifest{
		Identity: ManifestIdentity{
			ModelID:       "bge-small-en-v1.5-int8-cls",
			TokenizerHash: "stablehash",
			PlatformTag:   "linux-amd64",
		},
	}

	fake := &countingProbe{}

	// Simulate the caller logic: check MaybeSkipProbe, only call RunProbe
	// if Skip=false.
	res := MaybeSkipProbe(ctx, db, man, log)
	if !res.Skip {
		// This would be the cold path - probe would fire.
		_ = RunProbe(ctx, fake)
	}

	// On a warm hit, probe must NOT have been called.
	if got := fake.calls.Load(); got != 0 {
		t.Errorf("probe call count = %d, want 0 on warm-start hit", got)
	}
}

// TestMaybeSkipProbe_StateNotEnabled verifies that a non-enabled state
// always returns Skip=false regardless of identity.
func TestMaybeSkipProbe_StateNotEnabled(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	ctx := context.Background()
	log := noopLogger()

	for _, kv := range []struct{ k, v string }{
		{"embedder_state", "disabled"},
		{"embedder_model_id", "bge-small-en-v1.5-int8-cls"},
		{"embedder_tokenizer_hash", "abc123"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO meta (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			kv.k, kv.v,
		); err != nil {
			t.Fatalf("seed meta %s: %v", kv.k, err)
		}
	}

	man := Manifest{
		Identity: ManifestIdentity{
			ModelID:       "bge-small-en-v1.5-int8-cls",
			TokenizerHash: "abc123",
			PlatformTag:   "linux-arm64",
		},
	}

	res := MaybeSkipProbe(ctx, db, man, log)

	if res.Skip {
		t.Errorf("MaybeSkipProbe.Skip = true, want false when state=disabled")
	}
	if res.Reason != "state_not_enabled" {
		t.Errorf("MaybeSkipProbe.Reason = %q, want %q", res.Reason, "state_not_enabled")
	}
}

// TestMaybeSkipProbe_FreshDB verifies that an empty model_id (fresh DB)
// forces the cold path so identity is seeded correctly on first init.
func TestMaybeSkipProbe_FreshDB(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	ctx := context.Background()
	log := noopLogger()

	// Only set state=enabled; leave model_id empty (as a fresh migration would).
	if _, err := db.ExecContext(ctx,
		`INSERT INTO meta (key,value) VALUES ('embedder_state','enabled')`,
	); err != nil {
		t.Fatalf("seed embedder_state: %v", err)
	}

	man := Manifest{
		Identity: ManifestIdentity{
			ModelID:       "bge-small-en-v1.5-int8-cls",
			TokenizerHash: "abc123",
			PlatformTag:   "darwin-amd64",
		},
	}

	res := MaybeSkipProbe(ctx, db, man, log)

	if res.Skip {
		t.Errorf("MaybeSkipProbe.Skip = true, want false on fresh DB with no stored model_id")
	}
	if res.Reason != "no_stored_identity" {
		t.Errorf("MaybeSkipProbe.Reason = %q, want %q", res.Reason, "no_stored_identity")
	}
}

// TestMaybeSkipProbe_ModelIDMismatch verifies that a differing model_id
// forces the cold path (e.g. binary upgraded to a different model).
func TestMaybeSkipProbe_ModelIDMismatch(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	ctx := context.Background()
	log := noopLogger()

	for _, kv := range []struct{ k, v string }{
		{"embedder_state", "enabled"},
		{"embedder_model_id", "bge-small-en-v1.5-int8-cls"},
		{"embedder_tokenizer_hash", "oldhash"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO meta (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			kv.k, kv.v,
		); err != nil {
			t.Fatalf("seed meta %s: %v", kv.k, err)
		}
	}

	// Binary upgraded to a new model.
	man := Manifest{
		Identity: ManifestIdentity{
			ModelID:       "bge-small-en-v1.5-int8-q8",
			TokenizerHash: "newhash",
			PlatformTag:   "darwin-arm64",
		},
	}

	res := MaybeSkipProbe(ctx, db, man, log)

	if res.Skip {
		t.Errorf("MaybeSkipProbe.Skip = true, want false when model_id differs")
	}
	if res.Reason != "identity_mismatch" {
		t.Errorf("MaybeSkipProbe.Reason = %q, want %q", res.Reason, "identity_mismatch")
	}
}

// TestMaybeSkipProbe_TokenizerHashMismatch verifies that a differing
// tokenizer hash forces the cold path even when ModelID is the same.
func TestMaybeSkipProbe_TokenizerHashMismatch(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	ctx := context.Background()
	log := noopLogger()

	for _, kv := range []struct{ k, v string }{
		{"embedder_state", "enabled"},
		{"embedder_model_id", "bge-small-en-v1.5-int8-cls"},
		{"embedder_tokenizer_hash", "hash-v1"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO meta (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			kv.k, kv.v,
		); err != nil {
			t.Fatalf("seed meta %s: %v", kv.k, err)
		}
	}

	// Same model, tokenizer upgraded.
	man := Manifest{
		Identity: ManifestIdentity{
			ModelID:       "bge-small-en-v1.5-int8-cls",
			TokenizerHash: "hash-v2",
			PlatformTag:   "linux-amd64",
		},
	}

	res := MaybeSkipProbe(ctx, db, man, log)

	if res.Skip {
		t.Errorf("MaybeSkipProbe.Skip = true, want false when tokenizer_hash differs")
	}
	if res.Reason != "identity_mismatch" {
		t.Errorf("MaybeSkipProbe.Reason = %q, want %q", res.Reason, "identity_mismatch")
	}
}

// TestMaybeSkipProbe_EmptyStoredHashGracePeriod verifies that an empty
// stored tokenizer_hash does not block the fast path (first-enable grace
// period matching IdentityChanged semantics).
func TestMaybeSkipProbe_EmptyStoredHashGracePeriod(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	ctx := context.Background()
	log := noopLogger()

	for _, kv := range []struct{ k, v string }{
		{"embedder_state", "enabled"},
		{"embedder_model_id", "bge-small-en-v1.5-int8-cls"},
		{"embedder_tokenizer_hash", ""}, // empty: schema seed value
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO meta (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			kv.k, kv.v,
		); err != nil {
			t.Fatalf("seed meta %s: %v", kv.k, err)
		}
	}

	man := Manifest{
		Identity: ManifestIdentity{
			ModelID:       "bge-small-en-v1.5-int8-cls",
			TokenizerHash: "freshnewhash",
			PlatformTag:   "darwin-arm64",
		},
	}

	res := MaybeSkipProbe(ctx, db, man, log)

	// Empty stored hash = grace period: fast path should still fire.
	if !res.Skip {
		t.Errorf("MaybeSkipProbe.Skip = false, want true for empty stored tokenizer_hash (grace period)")
	}
	if res.Reason != "identity_match" {
		t.Errorf("MaybeSkipProbe.Reason = %q, want %q", res.Reason, "identity_match")
	}
}

// TestMaybeSkipProbe_NilLogger verifies that a nil logger is safely treated
// as slog.Default() without panicking.
func TestMaybeSkipProbe_NilLogger(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	ctx := context.Background()

	for _, kv := range []struct{ k, v string }{
		{"embedder_state", "enabled"},
		{"embedder_model_id", "bge-small-en-v1.5-int8-cls"},
		{"embedder_tokenizer_hash", "xyz"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO meta (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			kv.k, kv.v,
		); err != nil {
			t.Fatalf("seed meta %s: %v", kv.k, err)
		}
	}

	man := Manifest{
		Identity: ManifestIdentity{ModelID: "bge-small-en-v1.5-int8-cls", TokenizerHash: "xyz"},
	}

	// Must not panic with nil logger.
	res := MaybeSkipProbe(ctx, db, man, nil)
	if !res.Skip {
		t.Errorf("MaybeSkipProbe.Skip = false, want true with nil logger and matching identity")
	}
}

// TestWarmStartEmbedder_NoAssets verifies that WarmStartEmbedder returns a
// non-nil Err when no bundled assets are present (the default non-embed build).
// This covers the platform/build gate without requiring -tags=withembed.
func TestWarmStartEmbedder_NoAssets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	man := CurrentManifest()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if HasAssets() {
		t.Skip("skipping no-assets test in -tags=withembed build")
	}

	res := WarmStartEmbedder(ctx, man, log)

	if res.Err == nil {
		t.Errorf("WarmStartEmbedder.Err = nil, want non-nil when HasAssets()=false")
	}
	if res.Embedder != nil {
		t.Errorf("WarmStartEmbedder.Embedder non-nil on no-assets path")
	}
}

// TestMaybeSkipProbe_SecondCallProbeSkipped is the two-invocation test:
// run MaybeSkipProbe twice on the same warm DB and confirm probe is never
// called. Simulates two sequential CLI invocations on a warmed cache.
func TestMaybeSkipProbe_SecondCallProbeSkipped(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	ctx := context.Background()
	log := noopLogger()

	for _, kv := range []struct{ k, v string }{
		{"embedder_state", "enabled"},
		{"embedder_model_id", "bge-small-en-v1.5-int8-cls"},
		{"embedder_tokenizer_hash", "consistenthash"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO meta (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			kv.k, kv.v,
		); err != nil {
			t.Fatalf("seed meta %s: %v", kv.k, err)
		}
	}

	man := Manifest{
		Identity: ManifestIdentity{
			ModelID:       "bge-small-en-v1.5-int8-cls",
			TokenizerHash: "consistenthash",
			PlatformTag:   "linux-arm64",
		},
	}
	fake := &countingProbe{}

	// First call.
	res1 := MaybeSkipProbe(ctx, db, man, log)
	if !res1.Skip {
		_ = RunProbe(ctx, fake)
	}

	// Second call (simulates next CLI invocation).
	res2 := MaybeSkipProbe(ctx, db, man, log)
	if !res2.Skip {
		_ = RunProbe(ctx, fake)
	}

	if got := fake.calls.Load(); got != 0 {
		t.Errorf("probe called %d times across two warm-start invocations, want 0", got)
	}
	if res1.Reason != "identity_match" || res2.Reason != "identity_match" {
		t.Errorf("reasons = %q, %q; want identity_match, identity_match", res1.Reason, res2.Reason)
	}
}
