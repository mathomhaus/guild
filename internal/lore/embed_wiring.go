package lore

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/mathomhaus/guild/internal/lore/embed"
)

// EmbedWireOptions controls how WireEmbedDeps constructs the lore-side
// embedding pipeline port for a given process surface. The adapter
// layer (internal/mcp startup or internal/cli bootstrap) owns
// construction and hands the returned *EmbedDeps to command.Deps.Embed.
//
// This helper lives in internal/lore so the actual construction of
// BGEEmbedder, Index, and the model-id plumbing stays in one place and
// every caller observes the same nil-safety semantics: on any failure
// or when meta.embedder_state != "enabled", WireEmbedDeps returns
// (nil, reason, nil) and the caller threads that nil into
// command.Deps.Embed so the Phase-0 fallback kicks in per ADR-003.
type EmbedWireOptions struct {
	// Async picks the Tx2 concurrency shape. MCP startup passes true
	// so inscribe latency stays flat; CLI bootstrap passes false so
	// short-lived processes exit cleanly.
	Async bool
	// LoadIndex, when true, eagerly reads lore_vectors into a
	// per-process Index at wire time. MCP startup sets true (paid
	// once; every appraise reuses it). CLI bootstrap sets false to
	// avoid the 10-50 ms scan on every short-lived invocation; CLI
	// appraise falls through to BM25-only unless the caller opts in.
	LoadIndex bool
	// Logger receives one-line diagnostics: wired state + reasons.
	// Nil uses slog.Default.
	Logger *slog.Logger
}

// WireEmbedStatus is the structured diagnostic the adapter layer
// surfaces once at startup. One of the named reason strings mirrors
// the reason tags runEmbedderInit wrote into meta.embedder_state_reason
// so operators can correlate the two.
type WireEmbedStatus struct {
	// Wired is true iff *EmbedDeps is non-nil AND .Enabled() is true.
	// When false, Reason explains why the caller should expect BM25-only
	// retrieval and no vector writes.
	Wired bool
	// Reason is a short tag: "enabled", "meta_not_enabled",
	// "no_bundled_assets", "embedder_init_failed", "index_load_failed",
	// "meta_read_failed", "platform_disabled".
	Reason string
	// ModelID is the model identity the embedder was bound to (empty
	// when not wired).
	ModelID string
	// IndexLen is the number of vectors loaded into the Index when
	// LoadIndex was true and load succeeded. Zero otherwise.
	IndexLen int
}

// WireEmbedDeps constructs the *EmbedDeps for a single process surface.
//
// Contract:
//
//   - On platforms where the BGE path cannot link (Windows today),
//     returns (nil, {Wired:false, Reason:"platform_disabled"}, nil).
//   - If meta.embedder_state != "enabled", returns
//     (nil, {Wired:false, Reason:"meta_not_enabled"}, nil). The caller
//     must run `guild init` to flip the meta row.
//   - If the embedder fails to construct (missing bundled assets,
//     dylib probe fails, etc.), returns (nil, {Wired:false,
//     Reason:"embedder_init_failed"}, nil). The caller MUST continue
//     serving on the BM25 path.
//   - On success, returns (*EmbedDeps, {Wired:true, Reason:"enabled",
//     ModelID:..., IndexLen:...}, nil) and the caller threads the
//     *EmbedDeps into every command.Deps it builds.
//
// db must be a live connection to the lore database. WireEmbedDeps does
// not close db; the caller owns its lifecycle. The returned EmbedDeps
// does not retain db (it is only read during wire-up); the hot-path
// callers (Inscribe/Appraise) open and close their own connections.
//
// WireEmbedDeps is never the right place to surface an error to the
// user. Everything that could fail is logged as structured fields on
// opts.Logger with the caller-facing "embedder inactive" line; the
// function returns a nil error because adapter startup should never
// fail just because the embedder is off.
func WireEmbedDeps(ctx context.Context, db *sql.DB, opts EmbedWireOptions) (*EmbedDeps, WireEmbedStatus, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if db == nil {
		return nil, WireEmbedStatus{Reason: "nil_db"}, nil
	}

	// meta.embedder_state gates every other check. If guild init has
	// not flipped it to "enabled" the whole pipeline stays dormant;
	// this is the explicit ADR-003 "Partial coverage and deterministic
	// fallback" handshake.
	modelID, _, state, err := embed.ReadMeta(ctx, db)
	if err != nil {
		return nil, WireEmbedStatus{Reason: "meta_read_failed"}, nil
	}
	if state != "enabled" {
		return nil, WireEmbedStatus{Reason: "meta_not_enabled", ModelID: modelID}, nil
	}

	man := embed.CurrentManifest()
	if !embed.HasAssets() {
		// The binary was built without -tags=withembed; nothing to
		// probe. Tell the operator so they know which build they are
		// running.
		return nil, WireEmbedStatus{Reason: "no_bundled_assets", ModelID: modelID}, nil
	}

	boundModelID := man.Identity.ModelID
	if boundModelID == "" {
		boundModelID = modelID
	}

	// CLI warm-start fast path (LORE-375): skip the probe when this
	// binary's manifest identity matches the stored meta identity and
	// state is already 'enabled'. Asset SHA verification still runs
	// inside WarmStartEmbedder via Extract, which verifies each file's
	// SHA-256 against the manifest. Probe is only skipped on CLI
	// invocations (Async=false, LoadIndex=false); the MCP lazy-
	// reconstruct path is unaffected (it never calls WireEmbedDeps
	// after startup).
	if !opts.Async && !opts.LoadIndex {
		ws := embed.MaybeSkipProbe(ctx, db, man, logger)
		if ws.Skip {
			wsr := embed.WarmStartEmbedder(ctx, man, logger)
			if wsr.Err == nil && wsr.Embedder != nil {
				deps := &EmbedDeps{
					Embedder: wsr.Embedder,
					Index:    nil,
					ModelID:  boundModelID,
					Async:    opts.Async,
					Logger:   logger,
				}
				return deps, WireEmbedStatus{
					Wired:   true,
					Reason:  "warm_start",
					ModelID: boundModelID,
				}, nil
			}
			// Warm-start failed (unlikely: asset corruption or platform
			// gap). Fall through to the full PrepareAndProbe path.
			logger.Warn("embedder warm-start failed; falling through to full probe",
				slog.String("err", wsr.Err.Error()),
			)
		}
	}

	// Extract + probe. PrepareAndProbe writes its own structured log
	// line for the failure mode; we surface the outcome reason.
	prep, outcome := embed.PrepareAndProbe(ctx, logger)
	if outcome.State != "enabled" || prep == nil {
		reason := "embedder_init_failed"
		if outcome.Reason == "unsupported_platform" {
			reason = "platform_disabled"
		}
		if prep != nil {
			prep.Close()
		}
		return nil, WireEmbedStatus{Reason: reason, ModelID: modelID}, nil
	}

	// Index construction. LoadFromDB is cheap per the ADR-003 10 ms/1k
	// rows bench budget, but paying it on every CLI invocation is
	// pure overhead for the short-lived process. Callers set
	// LoadIndex=true only when they expect the index to be reused
	// across many calls (the MCP server).
	var idx *embed.Index
	indexLen := 0
	if opts.LoadIndex {
		idx = embed.NewIndex(embed.LoreCorpus{}, boundModelID, embed.WithLogger(logger))
		n, lerr := idx.LoadFromDB(ctx, db)
		if lerr != nil {
			// Fail safe: log and continue without the index. The
			// Appraise path still runs the RRF arm via live
			// embedding + per-call vector lookup; it just loses the
			// in-memory TopK acceleration. This is better than
			// failing startup.
			logger.Warn("embedder inactive: index load failed",
				slog.String("err", lerr.Error()),
				slog.String("model_id", boundModelID),
			)
			prep.Close()
			return nil, WireEmbedStatus{Reason: "index_load_failed", ModelID: boundModelID}, nil
		}
		indexLen = n
	}

	deps := &EmbedDeps{
		Embedder: prep.Embedder,
		Index:    idx,
		ModelID:  boundModelID,
		Async:    opts.Async,
		Logger:   logger,
	}
	return deps, WireEmbedStatus{
		Wired:    true,
		Reason:   "enabled",
		ModelID:  boundModelID,
		IndexLen: indexLen,
	}, nil
}
