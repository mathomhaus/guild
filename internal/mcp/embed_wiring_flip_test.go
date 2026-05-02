package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/lore/embed"
	"github.com/mathomhaus/guild/internal/storage"
)

// TestEmbedProvider_StateFlip is the QUEST-219 regression gate: an MCP
// server observed meta.embedder_state='disabled' at construction time,
// a peer process then flipped it to 'enabled' (the guild-init trap per
// LORE-371), and a subsequent resolve MUST pick up the flip without a
// server restart.
//
// The test stops short of probing the real BGE extractor (that requires
// -tags=withembed bundled bytes). It asserts the provider's meta-
// resolver + slog reason tag reaches the post-flip reconstruct
// invocation. The "fully wired" outcome is trust-WireEmbedDeps because
// that wiring has its own truth-table test. The boundary this test
// pins is: does the provider re-enter the wire path AT ALL after a
// mid-session flip? That is the exact gap LORE-371 captured.
func TestEmbedProvider_StateFlip(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "lore.db")
	seedLoreDB(t, dbPath)

	// Pin logger to a buffer so we can assert the slog reason tags
	// that the spec requires.
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	openDB := func(ctx context.Context) (*sql.DB, error) {
		return storage.Open(ctx, dbPath)
	}
	prov := newEmbedProvider(openDB, logger)

	// Phase 1: state='disabled' on boot. Resolve returns nil.
	ctx := context.Background()
	got := prov.ResolveEmbedDeps(ctx)
	if got != nil {
		t.Fatalf("resolve with state=disabled: got %+v, want nil", got)
	}
	if !slogHas(logBuf.String(), `"reason":"initial_boot_enabled"`) {
		t.Errorf("expected 'initial_boot_enabled' reason on first resolve; logs:\n%s", logBuf.String())
	}
	// The wire helper emits a non-wired status; the embed provider's
	// own log line carries status != "enabled".
	if !slogHas(logBuf.String(), `"status":"meta_not_enabled"`) {
		t.Errorf("expected status=meta_not_enabled on disabled boot; logs:\n%s", logBuf.String())
	}

	// Phase 2: an external process flips meta.embedder_state to
	// 'enabled'. Simulate the `guild init` completion from a peer.
	logBuf.Reset()
	flipMeta(t, dbPath, "embedder_state", "enabled")

	// Resolve again. The provider must detect the flip and re-enter
	// the wire path. We expect either a non-nil *EmbedDeps (when
	// bundled assets are present) or nil (default build without
	// -tags=withembed). The critical assertion is the slog reason
	// tag switches to "state_flip_mid_session".
	_ = prov.ResolveEmbedDeps(ctx)
	if !slogHas(logBuf.String(), `"reason":"state_flip_mid_session"`) {
		t.Errorf("expected 'state_flip_mid_session' after meta flip; logs:\n%s", logBuf.String())
	}
	// Either "enabled" (bundled build) or "no_bundled_assets" /
	// "platform_disabled" / "embedder_init_failed" on a default
	// build. Any of these is evidence the wire path ran, which is
	// the contract.
	for _, wantAny := range []string{
		`"status":"enabled"`,
		`"status":"no_bundled_assets"`,
		`"status":"platform_disabled"`,
		`"status":"embedder_init_failed"`,
		`"status":"index_load_failed"`,
	} {
		if strings.Contains(logBuf.String(), wantAny) {
			t.Logf("post-flip slog sample:\n%s", logBuf.String())
			return
		}
	}
	t.Errorf("expected a post-flip wire status slog; logs:\n%s", logBuf.String())
}

// TestEmbedProvider_HotPathReadLock exercises the common case: the
// state has not changed since the previous resolve, so the provider
// must NOT emit a new "embedder wired lazily" log line and must NOT
// call the reconstruct path. Proven indirectly: after the first
// resolve we reset the log buffer and assert that ten subsequent
// resolves produce zero log lines. QUEST-219 targets sub-500us hot
// path; this is the trip-wire for a regression that would log-spam
// on every tool call.
func TestEmbedProvider_HotPathReadLock(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "lore.db")
	seedLoreDB(t, dbPath)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	prov := newEmbedProvider(
		func(ctx context.Context) (*sql.DB, error) { return storage.Open(ctx, dbPath) },
		logger,
	)

	ctx := context.Background()
	_ = prov.ResolveEmbedDeps(ctx) // seeds cache.
	logBuf.Reset()

	for i := 0; i < 10; i++ {
		_ = prov.ResolveEmbedDeps(ctx)
	}

	if logBuf.Len() != 0 {
		t.Errorf("hot-path resolve should be silent; got logs:\n%s", logBuf.String())
	}
}

// TestEmbedProvider_ConcurrentResolveNoRace drives N goroutines through
// ResolveEmbedDeps while a peer goroutine flips meta. Under -race this
// gates the sync.RWMutex double-checked lock invariant: two
// reconstructs may legitimately race and both succeed; neither may
// observe a torn write on (cached, lastState, lastModelID).
func TestEmbedProvider_ConcurrentResolveNoRace(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "lore.db")
	seedLoreDB(t, dbPath)

	prov := newEmbedProvider(
		func(ctx context.Context) (*sql.DB, error) { return storage.Open(ctx, dbPath) },
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)

	ctx := context.Background()
	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers + 1)

	// Flipper: toggles meta.embedder_state every iteration so the
	// readers see contended reconstructs.
	go func() {
		defer wg.Done()
		states := []string{"enabled", "disabled"}
		for i := 0; i < 32; i++ {
			flipMeta(t, dbPath, "embedder_state", states[i%2])
		}
	}()

	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < 32; i++ {
				_ = prov.ResolveEmbedDeps(ctx)
			}
		}()
	}
	wg.Wait()
}

// TestEmbedProvider_DisabledAppraiseBM25Fallback is the regression
// guard for the Phase-0 path: with state='disabled' the MCP lore
// handler sees a nil *EmbedDeps from the provider and the classic
// BM25+stopwords code-path runs. Uses the lore.AppraiseCommand
// handler directly via command.Deps so the assertion matches the
// production call-stack byte for byte.
func TestEmbedProvider_DisabledAppraiseBM25Fallback(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "lore.db")
	seedLoreDBWithEntries(t, dbPath, []seedEntry{
		{ProjectID: "p1", Title: "SQLite busy contention", Summary: "BEGIN IMMEDIATE eliminates SQLITE_BUSY under parallel writers."},
		{ProjectID: "p1", Title: "Handoff before rot", Summary: "Agents hand off context at 40 percent budget to avoid attention degradation."},
	})

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	prov := newEmbedProvider(
		func(ctx context.Context) (*sql.DB, error) { return storage.Open(ctx, dbPath) },
		logger,
	)

	ctx := context.Background()

	// Confirm the resolver reports nil for this meta state.
	if got := prov.ResolveEmbedDeps(ctx); got != nil {
		t.Fatalf("state=disabled should resolve to nil *EmbedDeps; got %+v", got)
	}

	// Build command.Deps the same way buildMCPLoreDeps does, stash the
	// provider behind the opaque Embed field, and run an appraise.
	db, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	d := command.Deps{
		OpenDB:      func(ctx context.Context) (*sql.DB, error) { return storage.Open(ctx, dbPath) },
		ResolveProj: func(ctx context.Context, arg string) (string, error) { return "p1", nil },
		Embed:       prov,
	}

	out, err := lore.AppraiseCommand.Handler(ctx, d, lore.AppraiseInput{
		Query: "SQLite busy",
	})
	if err != nil {
		t.Fatalf("appraise returned err: %v", err)
	}
	if out.Output == nil || len(out.Output.Results) == 0 {
		t.Fatalf("expected BM25 hits under disabled embedder; got %+v", out.Output)
	}
}

// TestEmbedProvider_EpochReloadPath confirms that when the cached
// *EmbedDeps is non-nil and an external writer bumps meta.vector_epoch
// and inserts a vector row, the Index CheckAndReload path the lore
// appraise hot-path depends on can observe it. The provider itself is
// not the one calling CheckAndReload (that is appraise_rrf.go at the
// top of each appraise); this test stands in for that wiring by
// constructing an Index directly, holding it under an EmbedDeps the
// provider returns, and proving Index.Len grows after an out-of-band
// Splice-equivalent write.
//
// Kept here rather than under internal/lore because the scenario is
// specifically about the MCP-long-lived-Index lifecycle.
func TestEmbedProvider_EpochReloadPath(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "lore.db")
	seedLoreDB(t, dbPath)

	ctx := context.Background()

	// Seed entry+vector row at epoch 1 and index it.
	insertEntry(t, dbPath, 1, "p1", "first summary")
	flipMeta(t, dbPath, "embedder_state", "enabled")
	flipMeta(t, dbPath, "vector_epoch", "1")
	insertVectorRow(t, dbPath, 1, "bge-small-en-v1.5-int8-cls")

	idx := embed.NewIndex(embed.LoreCorpus{}, "bge-small-en-v1.5-int8-cls")
	db, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	n, err := idx.LoadFromDB(ctx, db)
	if err != nil {
		t.Fatalf("index load: %v", err)
	}
	if n != 1 {
		t.Fatalf("initial Index.Len = %d, want 1", n)
	}

	// External writer: add another row, bump epoch to 2.
	insertEntry(t, dbPath, 2, "p1", "other summary")
	insertVectorRow(t, dbPath, 2, "bge-small-en-v1.5-int8-cls")
	flipMeta(t, dbPath, "vector_epoch", "2")

	reloaded, err := idx.CheckAndReload(ctx, db)
	if err != nil {
		t.Fatalf("CheckAndReload: %v", err)
	}
	if !reloaded {
		t.Errorf("CheckAndReload should have reloaded after epoch bump; returned false")
	}
	if got := idx.Len(); got != 2 {
		t.Errorf("post-reload Index.Len = %d, want 2", got)
	}
}

// ----- helpers --------------------------------------------------------

func seedLoreDB(t *testing.T, path string, projectIDs ...string) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	if err := storage.Migrate(ctx, db, "lore"); err != nil {
		t.Fatalf("storage.Migrate: %v", err)
	}
	if len(projectIDs) == 0 {
		projectIDs = []string{"p1"}
	}
	for _, pid := range projectIDs {
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO projects (id, path) VALUES (?, ?)`,
			pid, "/fake/"+pid,
		); err != nil {
			t.Fatalf("insert project %q: %v", pid, err)
		}
	}
	_ = db.Close()
}

type seedEntry struct {
	ProjectID string
	Title     string
	Summary   string
}

func seedLoreDBWithEntries(t *testing.T, path string, rows []seedEntry) {
	t.Helper()
	seedLoreDB(t, path)
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, r := range rows {
		_, err := db.ExecContext(ctx,
			`INSERT INTO entries (project_id, topic, kind, title, summary, status, created_at, updated_at)
			 VALUES (?, 'topic', 'observation', ?, ?, 'current', datetime('now'), datetime('now'))`,
			r.ProjectID, r.Title, r.Summary,
		)
		if err != nil {
			t.Fatalf("insert entry: %v", err)
		}
	}
}

func flipMeta(t *testing.T, path, key, value string) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		t.Fatalf("flipMeta(%s=%s): %v", key, value, err)
	}
}

func insertEntry(t *testing.T, path string, id int64, projectID, summary string) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(ctx,
		`INSERT INTO entries (id, project_id, topic, kind, title, summary, status, created_at, updated_at)
		 VALUES (?, ?, 'topic', 'observation', 'title', ?, 'current', datetime('now'), datetime('now'))`,
		id, projectID, summary,
	)
	if err != nil {
		t.Fatalf("insert entry %d: %v", id, err)
	}
}

func insertVectorRow(t *testing.T, path string, entryID int64, modelID string) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	blob := make([]byte, embed.VecDim)
	// Arbitrary non-zero pattern; cosine math does not matter for the
	// test, we only count rows loaded into the index.
	for i := range blob {
		blob[i] = byte(i % 7)
	}
	_, err = db.ExecContext(ctx,
		`INSERT OR REPLACE INTO lore_vectors
		   (entry_id, model_id, dim, vec, encoded_at, content_hash)
		 VALUES (?, ?, ?, ?, strftime('%s','now'), 'test-hash')`,
		entryID, modelID, embed.VecDim, blob,
	)
	if err != nil {
		t.Fatalf("insert vector %d: %v", entryID, err)
	}
}

func slogHas(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
