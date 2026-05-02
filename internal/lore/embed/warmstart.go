// warmstart.go: CLI warm-start fast path.
//
// MaybeSkipProbe checks meta identity rows against the binary's manifest.
// If state='enabled' AND model_id and tokenizer_hash both match, the probe
// is redundant for the current binary version and the caller may skip it.
//
// Asset SHA verification (Extract) is NOT skipped; it protects against
// cache corruption, which is a different failure mode from identity drift.
// The probe validates embedder numeric output quality; SHA validates bytes
// on disk. Both serve independent invariants.
//
// Design reference: LORE-375.

package embed

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// WarmStartResult is the structured outcome returned by MaybeSkipProbe.
type WarmStartResult struct {
	// Skip is true when the probe may be safely omitted because meta
	// identity matches the binary's manifest and state is 'enabled'.
	Skip bool
	// Reason is a short tag:
	//   "identity_match"    - fast path applies
	//   "state_not_enabled" - meta.embedder_state != 'enabled'
	//   "no_stored_identity" - meta model_id is empty (fresh DB)
	//   "identity_mismatch" - model_id or tokenizer_hash differs
	//   "meta_read_error"   - ReadMeta returned an error
	Reason string
	// StoredModelID is the model_id read from meta (empty on error/missing).
	StoredModelID string
	// StoredTokenizerHash is the tokenizer_hash read from meta.
	StoredTokenizerHash string
}

// MaybeSkipProbe reports whether the CLI path may skip RunProbe for this
// invocation. It reads the three embedder-identity meta rows (a single
// lightweight SELECT), compares them to man, and returns a WarmStartResult
// describing the decision.
//
// Returns (result.Skip=true, reason="identity_match") when:
//   - meta.embedder_state = 'enabled'
//   - meta.embedder_model_id == man.Identity.ModelID (non-empty)
//   - meta.embedder_tokenizer_hash == man.Identity.TokenizerHash (non-empty)
//
// Returns (false, reason) in all other cases so the caller falls through to
// PrepareAndProbe unchanged.
//
// logger receives one structured line per call so hit rate is measurable
// across a real user session without additional instrumentation. Pass
// slog.Default() or a no-op logger.
func MaybeSkipProbe(ctx context.Context, db *sql.DB, man Manifest, logger *slog.Logger) WarmStartResult {
	if logger == nil {
		logger = slog.Default()
	}

	storedModelID, storedTokenizerHash, state, err := ReadMeta(ctx, db)
	if err != nil {
		logger.Warn("embedder warm-start: meta read error; falling through to full probe",
			slog.String("err", err.Error()),
		)
		return WarmStartResult{Skip: false, Reason: "meta_read_error"}
	}

	if state != "enabled" {
		return WarmStartResult{
			Skip:                false,
			Reason:              "state_not_enabled",
			StoredModelID:       storedModelID,
			StoredTokenizerHash: storedTokenizerHash,
		}
	}

	if storedModelID == "" {
		// Fresh DB: meta was never written by a successful init run.
		// Fall through so the cold path fires and seeds identity.
		return WarmStartResult{
			Skip:   false,
			Reason: "no_stored_identity",
		}
	}

	bound := man.Identity
	if storedModelID != bound.ModelID {
		return WarmStartResult{
			Skip:                false,
			Reason:              "identity_mismatch",
			StoredModelID:       storedModelID,
			StoredTokenizerHash: storedTokenizerHash,
		}
	}
	// Non-empty stored tokenizer hash that differs from the binary's
	// signals a tokenizer upgrade even when ModelID is unchanged.
	// An empty stored hash paired with a real bound hash is treated as
	// "match" (first-enable grace period mirrors IdentityChanged logic).
	if storedTokenizerHash != "" && storedTokenizerHash != bound.TokenizerHash {
		return WarmStartResult{
			Skip:                false,
			Reason:              "identity_mismatch",
			StoredModelID:       storedModelID,
			StoredTokenizerHash: storedTokenizerHash,
		}
	}

	logger.Debug("embedder warm-start, probe skipped",
		slog.String("reason", "identity_match"),
		slog.String("model_id", storedModelID),
		slog.String("tokenizer_hash", storedTokenizerHash),
	)
	return WarmStartResult{
		Skip:                true,
		Reason:              "identity_match",
		StoredModelID:       storedModelID,
		StoredTokenizerHash: storedTokenizerHash,
	}
}

// WarmStartEmbedderResult is the outcome of WarmStartEmbedder: either a
// ready-to-use Embedder (and close function) or a reason the warm path
// could not complete.
type WarmStartEmbedderResult struct {
	// Embedder is non-nil on success.
	Embedder Embedder
	// Close releases native resources. Safe to call on a nil result (no-op).
	Close func()
	// ExtractDuration is the wall-clock time spent in Extract (SHA verify).
	ExtractDuration time.Duration
	// TotalDuration is the total warm-start construction time.
	TotalDuration time.Duration
	// Err is non-nil when the warm path could not produce an Embedder.
	// The caller should fall through to PrepareAndProbe.
	Err error
}

// WarmStartEmbedder constructs an Embedder directly from cached assets
// without running the probe. It still calls Extract (which verifies each
// asset's SHA-256 against the manifest), so on-disk corruption triggers
// a rewrite and the resulting SHA-mismatch surfaces as an error.
//
// Callers MUST only invoke this after MaybeSkipProbe returned Skip=true.
// On non-unix platforms (Windows) newBGEEmbedderFromExt returns
// ErrEmbedderDisabled; callers should treat any non-nil Err as a signal
// to fall back to PrepareAndProbe or BM25-only.
//
// logger receives one structured line on success carrying extract_duration_ms
// and total_duration_ms for latency tracking.
func WarmStartEmbedder(ctx context.Context, man Manifest, logger *slog.Logger) WarmStartEmbedderResult {
	if logger == nil {
		logger = slog.Default()
	}
	start := time.Now()

	if !HasAssets() || !man.hasAssetBytes() {
		return WarmStartEmbedderResult{
			Close: func() {},
			Err:   fmt.Errorf("embed: WarmStartEmbedder: no bundled assets"),
		}
	}

	cacheDir, err := ResolveCacheDir(man)
	if err != nil {
		return WarmStartEmbedderResult{
			Close: func() {},
			Err:   fmt.Errorf("embed: WarmStartEmbedder: resolve cache dir: %w", err),
		}
	}

	extStart := time.Now()
	ext, err := Extract(man, cacheDir)
	extractDur := time.Since(extStart)
	if err != nil {
		return WarmStartEmbedderResult{
			Close:           func() {},
			ExtractDuration: extractDur,
			Err:             fmt.Errorf("embed: WarmStartEmbedder: extract: %w", err),
		}
	}

	emb, closeFn, err := newBGEEmbedderFromExt(ext)
	if err != nil {
		return WarmStartEmbedderResult{
			Close:           func() {},
			ExtractDuration: extractDur,
			Err:             fmt.Errorf("embed: WarmStartEmbedder: embedder init: %w", err),
		}
	}

	total := time.Since(start)
	logger.Debug("embedder warm-start complete",
		slog.String("reason", "identity_match"),
		slog.Int64("extract_duration_ms", extractDur.Milliseconds()),
		slog.Int64("total_duration_ms", total.Milliseconds()),
		slog.String("model_id", man.Identity.ModelID),
		slog.String("tokenizer_hash", man.Identity.TokenizerHash),
	)

	return WarmStartEmbedderResult{
		Embedder:        emb,
		Close:           closeFn,
		ExtractDuration: extractDur,
		TotalDuration:   total,
	}
}
