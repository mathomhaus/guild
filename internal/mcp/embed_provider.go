package mcp

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/mathomhaus/guild/internal/lore"
)

// embedProvider is the MCP-adapter lazy resolver for *lore.EmbedDeps.
// It exists because the MCP server is long-lived while meta.embedder_state
// can flip mid-session (the most common trigger: the user runs `guild
// init` in another process while Claude is attached). A startup-only
// read of meta leaves the handler stuck on whatever state the server
// observed at boot. LORE-371 captured the concrete E2E where the
// feature worked from the CLI but MCP appraise returned byte-identical
// BM25-only rankings until the user restarted Claude.
//
// Contract: Resolve is called at the top of every lore tool handler.
// On the common path (state unchanged since last resolve) it is a
// single indexed meta SELECT plus an RLock acquire, dominated by the
// SELECT. On a state transition it upgrades to a write lock, calls
// lore.WireEmbedDeps to reconstruct, caches the result, and emits one
// structured slog line.
//
// Hexagonal: the provider is an adapter concern. Keeps internal/lore
// free of adapter-specific lifecycle code; the lore handlers see a
// *lore.EmbedDeps exactly as they did before.
type embedProvider struct {
	// mu guards cached, lastState, and lastModelID. RWMutex is
	// sufficient for the access pattern: many concurrent readers
	// (every lore tool call), a rare writer (state flip).
	mu sync.RWMutex

	// cached is the current resolved *lore.EmbedDeps. A nil value is
	// legitimate and means the Phase-0 BM25 fallback is active.
	cached *lore.EmbedDeps

	// lastState is the meta.embedder_state value observed at the most
	// recent reconstruct. Empty string means "never resolved." The
	// hot-path compares the freshly read state string against this
	// cached value under the read lock.
	lastState string

	// lastModelID is the meta.embedder_model_id value observed at the
	// most recent reconstruct. Tracked so an embed-rebuild that
	// rotates the canonical model identity forces a reconstruct even
	// when the state string is still "enabled" on both sides.
	lastModelID string

	// openDB opens a short-lived lore.db handle for meta reads and
	// index load. Injected so tests route around ~/.guild/lore.db.
	openDB func(ctx context.Context) (*sql.DB, error)

	// logger receives the "embedder wired lazily" structured line on
	// every reconstruction and the "embedder resolve failed" warn on
	// any error.
	logger *slog.Logger
}

// newEmbedProvider builds a provider with an unset cache. The first
// Resolve call populates it. openDB and logger must be non-nil; the
// provider's hot path depends on both.
func newEmbedProvider(openDB func(ctx context.Context) (*sql.DB, error), logger *slog.Logger) *embedProvider {
	if logger == nil {
		logger = slog.Default()
	}
	return &embedProvider{
		openDB: openDB,
		logger: logger,
	}
}

// ResolveEmbedDeps returns the current *lore.EmbedDeps for this MCP
// server, reconstructing it if meta.embedder_state has changed since
// the last resolve. A nil return is the documented Phase-0 fallback
// path and every lore handler tolerates it.
//
// This method satisfies the interface internal/lore embedFromDeps type-
// switches on; see internal/lore/embed_deps.go. Naming is intentional:
// the lore package reads a provider via its interface shape, not via
// an imported type, so there is no cycle.
func (p *embedProvider) ResolveEmbedDeps(ctx context.Context) *lore.EmbedDeps {
	if p == nil {
		return nil
	}

	// Bounded context for the meta read, derived from the caller's
	// ctx. The lore Command.Handler always threads a real context; we
	// do not fabricate a background one here. A 2 s budget is
	// generous for a single indexed SELECT against ~/.guild/lore.db.
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	state, modelID, err := p.readMetaState(readCtx)
	if err != nil {
		// Meta-read failure should not crash the tool; surface one
		// structured warn and return whatever was cached. A nil cache
		// becomes the BM25 fallback per ADR-003.
		p.logger.Warn("embedder resolve: meta read failed; using cache",
			slog.String("err", err.Error()),
		)
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.cached
	}

	// Hot path: nothing changed. Read-lock, compare, return cache.
	p.mu.RLock()
	if p.lastState == state && p.lastModelID == modelID && p.lastState != "" {
		cached := p.cached
		p.mu.RUnlock()
		return cached
	}
	priorState := p.lastState
	p.mu.RUnlock()

	// Slow path: state changed (or first resolve). Upgrade to write
	// lock. A second goroutine may race us here; we re-check under
	// the write lock and skip the reconstruct if the peer already
	// stored the same state. Reconstruct is idempotent, so two
	// concurrent reconstructs that both succeed and store the same
	// value is still correct; the re-check just saves the wasted
	// probe + LoadFromDB.
	p.mu.Lock()
	if p.lastState == state && p.lastModelID == modelID && p.lastState != "" {
		cached := p.cached
		p.mu.Unlock()
		return cached
	}
	p.mu.Unlock()

	// Construct outside the lock: WireEmbedDeps opens a DB handle,
	// probes the extractor, and loads the index. All of those are
	// long enough to hurt latency for concurrent readers if held
	// under the write lock. The cached-store step below is a quick
	// Lock/Unlock.
	reason := "initial_boot_enabled"
	if priorState != "" {
		reason = "state_flip_mid_session"
	}
	newDeps := p.reconstruct(ctx, reason)

	p.mu.Lock()
	p.cached = newDeps
	p.lastState = state
	p.lastModelID = modelID
	p.mu.Unlock()

	return newDeps
}

// reconstruct opens lore.db and calls lore.WireEmbedDeps. Returns nil
// on any failure; WireEmbedDeps is already nil-return-on-failure so
// this function just threads the DB handle through and closes it.
//
// Emits one structured slog line per invocation regardless of outcome
// so operators can correlate a session's retrieval mode with the
// resolve event. The line carries the reason tag
// ("initial_boot_enabled" | "state_flip_mid_session") so log readers
// can distinguish a cold start from an mid-session flip.
func (p *embedProvider) reconstruct(ctx context.Context, reason string) *lore.EmbedDeps {
	bootCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	db, err := p.openDB(bootCtx)
	if err != nil {
		p.logger.Info("embedder wired lazily",
			slog.String("reason", reason),
			slog.String("status", "lore_db_open_failed"),
			slog.String("err", err.Error()),
		)
		return nil
	}
	defer func() { _ = db.Close() }()

	deps, status, _ := lore.WireEmbedDeps(bootCtx, db, lore.EmbedWireOptions{
		Async:     true, // MCP surface: fire-and-forget Tx2.
		LoadIndex: true, // warm once; subsequent appraises reuse it.
		Logger:    p.logger,
	})

	coverageNum, coverageDen, epoch := readCoverageDiag(bootCtx, db)
	if !status.Wired {
		p.logger.Info("embedder wired lazily",
			slog.String("reason", reason),
			slog.String("status", status.Reason),
			slog.String("model_id", status.ModelID),
			slog.Int64("coverage_num", coverageNum),
			slog.Int64("coverage_den", coverageDen),
			slog.Int64("vector_epoch", epoch),
		)
		return nil
	}

	p.logger.Info("embedder wired lazily",
		slog.String("reason", reason),
		slog.String("status", "enabled"),
		slog.String("model_id", status.ModelID),
		slog.Int("index_len", status.IndexLen),
		slog.Int64("coverage_num", coverageNum),
		slog.Int64("coverage_den", coverageDen),
		slog.Int64("vector_epoch", epoch),
	)
	// Trigger auto-backfill the first time we observe a wired embedder.
	// The sync.Once inside maybeTriggerAutoBackfill guarantees exactly-
	// once semantics per process, so concurrent provider resolves racing
	// the initial enable all collapse to one invocation. Non-blocking:
	// each per-corpus backfill runs in its own goroutine. QUEST-229 /
	// LORE-384.
	//
	//nolint:contextcheck // the auto-backfill goroutine is server-lifetime work and intentionally uses context.Background() internally, per QUEST-229 design bar.
	maybeTriggerAutoBackfill(deps, p.logger)
	return deps
}

// readMetaState reads meta.embedder_state and meta.embedder_model_id
// in one short-lived handle. Missing rows are treated as empty
// strings: the caller compares to the empty-string cached defaults so
// a fresh DB on first resolve counts as a state change exactly once.
func (p *embedProvider) readMetaState(ctx context.Context) (state, modelID string, err error) {
	db, oerr := p.openDB(ctx)
	if oerr != nil {
		return "", "", oerr
	}
	defer func() { _ = db.Close() }()

	state, err = readMetaValue(ctx, db, "embedder_state")
	if err != nil {
		return "", "", err
	}
	modelID, err = readMetaValue(ctx, db, "embedder_model_id")
	if err != nil {
		return "", "", err
	}
	return state, modelID, nil
}

// readMetaValue returns value for the given meta key, empty string on
// missing row. Any other error propagates.
func readMetaValue(ctx context.Context, db *sql.DB, key string) (string, error) {
	var v string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, key,
	).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// readCoverageDiag reads the three coverage diagnostic counters for
// the "embedder wired lazily" slog line. Any failure is downgraded to
// zeros so the log line still fires.
func readCoverageDiag(ctx context.Context, db *sql.DB) (num, den, epoch int64) {
	num, _ = readMetaInt64(ctx, db, "vector_coverage_num")
	den, _ = readMetaInt64(ctx, db, "vector_coverage_den")
	epoch, _ = readMetaInt64(ctx, db, "vector_epoch")
	return num, den, epoch
}

// readMetaInt64 returns the parsed meta value for key, zero on any
// failure (missing row, parse error, DB error). Diagnostic path only.
func readMetaInt64(ctx context.Context, db *sql.DB, key string) (int64, error) {
	s, err := readMetaValue(ctx, db, key)
	if err != nil || s == "" {
		return 0, err
	}
	n, perr := parseInt64(s)
	if perr != nil {
		return 0, perr
	}
	return n, nil
}

// parseInt64 is a tiny helper to avoid importing strconv in a hot
// path file. Values originate from guild-owned meta rows and are
// always decimal-encoded integers.
func parseInt64(s string) (int64, error) {
	var n int64
	var sign int64 = 1
	i := 0
	if i < len(s) && s[i] == '-' {
		sign = -1
		i++
	}
	if i == len(s) {
		return 0, errors.New("empty numeric string")
	}
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, errors.New("non-digit in numeric string")
		}
		n = n*10 + int64(c-'0')
	}
	return sign * n, nil
}

// Compile-time check: *embedProvider satisfies the lore-side resolver
// interface. The lore package declares the interface locally and
// type-switches d.Embed against it; keeping this assertion here keeps
// the contract visible at the call site without introducing a
// reverse import.
var _ interface {
	ResolveEmbedDeps(ctx context.Context) *lore.EmbedDeps
} = (*embedProvider)(nil)
