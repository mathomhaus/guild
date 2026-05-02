// Init: the full guild-init embedder bring-up path. Extract assets to
// the cache directory, probe against the pinned reference vector, seed
// meta identity, and return a ready-to-use Embedder (or the null
// fallback if anything failed). Keeps the package self-contained so the
// install/init code at internal/install/init.go can drive this with one
// call and structured error handling.
//
// Returns an InitOutcome describing what happened, which the caller
// feeds into the meta.embedder_state update and the init transcript.

package embed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strconv"
	"time"
)

// InitOutcome summarizes the embedder bring-up for logging + meta.
//
// Structured slog field names (stable; do not rename without a LORE decision):
//
//	extract_duration_ms_dylib  - libonnxruntime dylib / .so wall-clock ms
//	extract_duration_ms_model  - model.onnx wall-clock ms
//	extract_duration_ms_vocab  - vocab.txt wall-clock ms
//	extract_duration_ms_total  - total extract wall-clock ms
//	probe_duration_ms          - probe wall-clock ms
//	probe_cosine_observed      - observed cosine similarity (float64)
//	probe_cosine_floor         - acceptance floor (ProbeMinCosine)
//	extracted_dylib_sha256     - full hex SHA-256 of the extracted dylib
//	extracted_model_sha256     - full hex SHA-256 of the extracted model.onnx
//	extracted_vocab_sha256     - full hex SHA-256 of the extracted vocab.txt
//	model_id                   - canonical model identifier
//	tokenizer_hash             - digest of the vocab file
type InitOutcome struct {
	// State is the meta.embedder_state value the caller should write:
	// "enabled" when the probe passed, "disabled" otherwise.
	State string
	// Reason is a short machine-parseable tag for disabled outcomes:
	// "ok", "unsupported_platform", "no_assets", "extract_failed",
	// "embedder_init_failed", "probe_mismatch", "probe_error".
	Reason string
	// Identity is the manifest-bound identity. Populated whenever extract
	// succeeds, even when probe subsequently fails (Option A: extract-time
	// semantics). See WriteMeta for why these three fields are not gated on
	// probe success.
	Identity ManifestIdentity
	// CacheDir is the extraction target (empty on skip).
	CacheDir string
	// Extracted is true when Extract wrote bytes this call.
	Extracted bool
	// ProbeCosine is the cosine similarity against the pinned
	// reference (0 on skipped probe). Slog field: probe_cosine_observed.
	ProbeCosine float64
	// ProbeFloor is the acceptance threshold used by the probe.
	// Slog field: probe_cosine_floor.
	ProbeFloor float64
	// ExtractDuration is total wall-clock time for all three asset
	// extract calls. Slog field: extract_duration_ms_total.
	ExtractDuration time.Duration
	// ProbeDuration is the probe wall-clock time. Slog field: probe_duration_ms.
	ProbeDuration time.Duration
	// Per-asset extract durations (LORE-368). Zero when extraction was
	// skipped (no assets). Slog fields: extract_duration_ms_dylib,
	// extract_duration_ms_model, extract_duration_ms_vocab.
	DylibExtractDuration time.Duration
	ModelExtractDuration time.Duration
	VocabExtractDuration time.Duration
	// Asset SHA-256 digests from the manifest (full hex). Present whenever
	// extract succeeds. Slog fields: extracted_dylib_sha256,
	// extracted_model_sha256, extracted_vocab_sha256.
	DylibSHA256 string
	ModelSHA256 string
	VocabSHA256 string
	// Err is the outer error surfaced to the caller for logging. The
	// caller should still persist State="disabled" in meta; guild init
	// never exits non-zero just because the embedder is off.
	Err error
}

// PreparedEmbedder is a small handle the caller closes after the
// outcome has been recorded. Nil when the outcome is disabled.
type PreparedEmbedder struct {
	Embedder Embedder
	// close releases native resources. Safe to call multiple times.
	close func()
}

// Close releases any native resources (ORT session, dylib). No-op on a
// nil receiver.
func (p *PreparedEmbedder) Close() {
	if p == nil || p.close == nil {
		return
	}
	p.close()
}

// PrepareAndProbe runs the full extract + construct + probe sequence.
// On success, returns a non-nil PreparedEmbedder the caller should
// defer-close. On any failure path, returns (nil, InitOutcome with
// State="disabled", nil error at the function level) so guild init
// always exits 0.
//
// logger is used for structured diagnostic lines. Pass a no-op logger
// (slog.New(slog.NewTextHandler(io.Discard, nil))) to silence.
func PrepareAndProbe(ctx context.Context, logger *slog.Logger) (*PreparedEmbedder, InitOutcome) {
	if logger == nil {
		logger = slog.Default()
	}
	// Hard short-circuit on platforms where the unix-only BGE path
	// cannot compile (Windows). ADR-003 invariant: init exits 0 with
	// embedder_state=disabled, reason=unsupported_platform.
	if runtime.GOOS == "windows" {
		return nil, InitOutcome{
			State:  "disabled",
			Reason: "unsupported_platform",
		}
	}
	man := CurrentManifest()
	if !HasAssets() || !man.hasAssetBytes() {
		logger.Info("embedder disabled: no bundled assets in this build",
			slog.String("platform_tag", man.Identity.PlatformTag),
			slog.String("goos", runtime.GOOS),
			slog.String("goarch", runtime.GOARCH),
		)
		return nil, InitOutcome{
			State:  "disabled",
			Reason: "no_assets",
		}
	}
	cacheDir, err := ResolveCacheDir(man)
	if err != nil {
		logger.Warn("embedder disabled: resolve cache dir failed",
			slog.String("err", err.Error()),
		)
		return nil, InitOutcome{
			State:  "disabled",
			Reason: "extract_failed",
			Err:    err,
		}
	}

	ext, err := Extract(man, cacheDir)
	if err != nil {
		logger.Warn("embedder disabled: extract failed",
			slog.String("cache_dir", cacheDir),
			slog.String("err", err.Error()),
		)
		return nil, InitOutcome{
			State:    "disabled",
			Reason:   "extract_failed",
			CacheDir: cacheDir,
			Err:      err,
		}
	}

	// Capture per-asset and total durations from ExtractResult (LORE-368).
	dylibDur := ext.DylibDuration
	modelDur := ext.ModelDuration
	vocabDur := ext.VocabDuration
	extractDur := ext.TotalDuration
	dylibSHA := man.Assets[AssetLibrary].SHA256
	modelSHA := man.Assets[AssetModel].SHA256
	vocabSHA := man.Assets[AssetVocab].SHA256

	logger.Info("embedder assets extracted",
		slog.String("cache_dir", ext.CacheDir),
		slog.Bool("extracted", ext.Extracted),
		slog.String("library_path", ext.LibraryPath),
		slog.Int64("extract_duration_ms_dylib", dylibDur.Milliseconds()),
		slog.Int64("extract_duration_ms_model", modelDur.Milliseconds()),
		slog.Int64("extract_duration_ms_vocab", vocabDur.Milliseconds()),
		slog.Int64("extract_duration_ms_total", extractDur.Milliseconds()),
		slog.String("extracted_dylib_sha256", dylibSHA),
		slog.String("extracted_model_sha256", modelSHA),
		slog.String("extracted_vocab_sha256", vocabSHA),
	)

	emb, closeFn, err := newBGEEmbedderFromExt(ext)
	if err != nil {
		logger.Warn("embedder disabled: BGE init failed",
			slog.String("err", err.Error()),
		)
		return nil, InitOutcome{
			State:                "disabled",
			Reason:               "embedder_init_failed",
			CacheDir:             ext.CacheDir,
			Extracted:            ext.Extracted,
			ExtractDuration:      extractDur,
			DylibExtractDuration: dylibDur,
			ModelExtractDuration: modelDur,
			VocabExtractDuration: vocabDur,
			DylibSHA256:          dylibSHA,
			ModelSHA256:          modelSHA,
			VocabSHA256:          vocabSHA,
			Err:                  err,
		}
	}

	startProbe := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	probe := RunProbe(probeCtx, emb)
	cancel()
	probeDur := time.Since(startProbe)
	if probe.Err != nil {
		reason := "probe_error"
		if errors.Is(probe.Err, ErrProbeMismatch) {
			reason = "probe_mismatch"
		}
		// Structured fingerprint on failure (LORE-368): the next operator
		// reading logs can tell at a glance whether it is asset drift,
		// tokenizer drift, or pure quantization noise without another round
		// trip.
		logger.Warn("embedder disabled: probe failed",
			slog.String("reason", reason),
			slog.Float64("probe_cosine_observed", probe.Cosine),
			slog.Float64("probe_cosine_floor", probe.Floor),
			slog.Int("dim", probe.Dim),
			slog.String("err", probe.Err.Error()),
			slog.Int64("probe_duration_ms", probeDur.Milliseconds()),
			slog.Int64("extract_duration_ms_dylib", dylibDur.Milliseconds()),
			slog.Int64("extract_duration_ms_model", modelDur.Milliseconds()),
			slog.Int64("extract_duration_ms_vocab", vocabDur.Milliseconds()),
			slog.Int64("extract_duration_ms_total", extractDur.Milliseconds()),
			slog.String("platform_tag", man.Identity.PlatformTag),
			slog.String("model_id", man.Identity.ModelID),
			slog.String("tokenizer_hash", man.Identity.TokenizerHash),
			slog.String("runtime_version", man.Identity.RuntimeVersion),
			slog.String("extracted_dylib_sha256", dylibSHA),
			slog.String("extracted_model_sha256", modelSHA),
			slog.String("extracted_vocab_sha256", vocabSHA),
			slog.String("library_path", ext.LibraryPath),
			slog.String("model_path", ext.ModelPath),
			slog.String("vocab_path", ext.VocabPath),
		)
		closeFn()
		return nil, InitOutcome{
			State:  "disabled",
			Reason: reason,
			// Identity is populated even on probe failure (Option A): model_id,
			// tokenizer_hash, and runtime_version are deterministic digests of
			// the bundled bytes, not attestations of probe success. Populating
			// them on extract lets operators see the hash in `guild lore health`
			// even when probe_mismatch fires, which is exactly when the hash is
			// most useful for diagnosing asset drift (LORE-365 / LORE-369).
			Identity:             man.Identity,
			CacheDir:             ext.CacheDir,
			Extracted:            ext.Extracted,
			ExtractDuration:      extractDur,
			DylibExtractDuration: dylibDur,
			ModelExtractDuration: modelDur,
			VocabExtractDuration: vocabDur,
			ProbeCosine:          probe.Cosine,
			ProbeFloor:           probe.Floor,
			ProbeDuration:        probeDur,
			DylibSHA256:          dylibSHA,
			ModelSHA256:          modelSHA,
			VocabSHA256:          vocabSHA,
			Err:                  probe.Err,
		}
	}
	logger.Info("embedder probe passed",
		slog.Float64("probe_cosine_observed", probe.Cosine),
		slog.Float64("probe_cosine_floor", probe.Floor),
		slog.Int("dim", probe.Dim),
		slog.Int64("probe_duration_ms", probeDur.Milliseconds()),
		slog.Int64("extract_duration_ms_total", extractDur.Milliseconds()),
		slog.String("model_id", man.Identity.ModelID),
		slog.String("tokenizer_hash", man.Identity.TokenizerHash),
	)

	return &PreparedEmbedder{
			Embedder: emb,
			close:    closeFn,
		},
		InitOutcome{
			State:                "enabled",
			Reason:               "ok",
			Identity:             man.Identity,
			CacheDir:             ext.CacheDir,
			Extracted:            ext.Extracted,
			ExtractDuration:      extractDur,
			DylibExtractDuration: dylibDur,
			ModelExtractDuration: modelDur,
			VocabExtractDuration: vocabDur,
			ProbeCosine:          probe.Cosine,
			ProbeFloor:           probe.Floor,
			ProbeDuration:        probeDur,
			DylibSHA256:          dylibSHA,
			ModelSHA256:          modelSHA,
			VocabSHA256:          vocabSHA,
		}
}

// WriteMeta upserts the embedder-identity meta rows from outcome into
// db in one BEGIN IMMEDIATE transaction.
//
// Write semantics (Option A, extract-time identity):
//
//   - embedder_state and embedder_state_reason are always written.
//   - embedder_model_id, embedder_tokenizer_hash, and embedder_runtime_version
//     are written whenever Identity.ModelID is non-empty, i.e. as soon as
//     extract succeeds, regardless of whether probe passed or failed.
//     These three fields are deterministic digests of the bundled bytes; their
//     value is independent of probe outcome. Writing them on extract lets
//     operators see the hash in `guild lore health` even during probe_mismatch,
//     which is the most useful time to inspect it (LORE-365 / LORE-369).
//   - embedder_dim is written only when state="enabled" because dim is an
//     assertion about the running embedder, not merely a property of the files.
//
// The upsert is idempotent: re-running init with the same bundled vocab writes
// the same hash (ON CONFLICT DO UPDATE SET value=excluded.value semantics).
func WriteMeta(ctx context.Context, logger *slog.Logger, db *sql.DB, outcome InitOutcome) error {
	if db == nil {
		return fmt.Errorf("embed: WriteMeta: nil db")
	}
	if logger == nil {
		logger = slog.Default()
	}
	conn, rollback, err := beginImmediateLocal(ctx, db, "write-meta")
	if err != nil {
		return err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	rows := []struct{ k, v string }{
		{"embedder_state", outcome.State},
		{"embedder_state_reason", outcome.Reason},
	}
	// Identity fields are extract-time values: populate whenever extract
	// succeeded (signalled by a non-empty ModelID), probe success is not
	// required. This makes tokenizer_hash visible in health output even when
	// probe_mismatch fires, aiding drift diagnosis.
	if outcome.Identity.ModelID != "" {
		rows = append(rows,
			struct{ k, v string }{"embedder_model_id", outcome.Identity.ModelID},
			struct{ k, v string }{"embedder_tokenizer_hash", outcome.Identity.TokenizerHash},
			struct{ k, v string }{"embedder_runtime_version", outcome.Identity.RuntimeVersion},
		)
		logger.Info("embedder meta: writing extract-time identity",
			slog.String("model_id", outcome.Identity.ModelID),
			slog.String("tokenizer_hash", outcome.Identity.TokenizerHash),
			slog.String("runtime_version", outcome.Identity.RuntimeVersion),
		)
	}
	// embedder_dim is an assertion about the running embedder; only write it
	// when the embedder is fully validated (probe passed).
	if outcome.State == "enabled" {
		rows = append(rows,
			struct{ k, v string }{"embedder_dim", strconv.Itoa(outcome.Identity.Dim)},
		)
	}
	for _, kv := range rows {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO meta (key,value) VALUES (?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			kv.k, kv.v,
		); err != nil {
			return fmt.Errorf("embed: WriteMeta: upsert %s: %w", kv.k, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("embed: WriteMeta: commit: %w", err)
	}
	committed = true
	return nil
}

// ReadMeta reads the embedder identity + state rows from meta. Missing
// rows return ("", "", ...) without error so callers can treat an empty
// model_id as "never been embedded". Use HasIdentity to check.
func ReadMeta(ctx context.Context, db *sql.DB) (storedModelID, storedTokenizerHash, state string, err error) {
	if db == nil {
		return "", "", "", fmt.Errorf("embed: ReadMeta: nil db")
	}
	for _, key := range []string{"embedder_model_id", "embedder_tokenizer_hash", "embedder_state"} {
		var v sql.NullString
		scanErr := db.QueryRowContext(ctx,
			`SELECT value FROM meta WHERE key = ?`, key,
		).Scan(&v)
		if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
			return "", "", "", fmt.Errorf("embed: ReadMeta: read %s: %w", key, scanErr)
		}
		switch key {
		case "embedder_model_id":
			storedModelID = v.String
		case "embedder_tokenizer_hash":
			storedTokenizerHash = v.String
		case "embedder_state":
			state = v.String
		}
	}
	return storedModelID, storedTokenizerHash, state, nil
}

// IdentityChanged reports whether storedID/storedHash differ from the
// binary's bound identity. Treats an empty stored model_id as "same"
// (fresh DB with only the seed value; no invalidation needed on the
// first-ever run).
func IdentityChanged(stored, bound ManifestIdentity) bool {
	if stored.ModelID == "" {
		return false
	}
	if stored.ModelID != bound.ModelID {
		return true
	}
	// A non-empty stored tokenizer hash that differs from the binary's
	// is an upgrade. An empty stored hash paired with a real bound
	// hash is NOT treated as change: that is the first-enable case
	// (the schema seed has tokenizer_hash='').
	if stored.TokenizerHash != "" && stored.TokenizerHash != bound.TokenizerHash {
		return true
	}
	return false
}
