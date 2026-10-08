package mcp

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/mathomhaus/guild/internal/daemon/watch"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/project"
	"github.com/mathomhaus/guild/internal/quest"
	"github.com/mathomhaus/guild/internal/storage"
)

// watchTestProject is the fixed project id the watch-pipeline host tests
// register. Hermetic: every DB lives under t.TempDir(), never ~/.guild.
const watchTestProject = "watchproj"

// setupWatchHost wires the openLoreDB / openQuestDB resolvers at temp DBs,
// migrates both, and returns a DaemonHost plus the two paths. The caller
// seeds entries/projects through the returned paths.
func setupWatchHost(t *testing.T) (host *DaemonHost, loreDBPath, questDBPath string) {
	t.Helper()
	tmp := t.TempDir()
	loreDBPath = filepath.Join(tmp, "lore.db")
	questDBPath = filepath.Join(tmp, "quest.db")

	origLdb := ldbPath
	origQdb := qdbPath
	ldbPath = func() (string, error) { return loreDBPath, nil }
	qdbPath = func() (string, error) { return questDBPath, nil }
	t.Cleanup(func() {
		ldbPath = origLdb
		qdbPath = origQdb
	})

	migrateWatchDB(t, loreDBPath, "lore")
	migrateWatchDB(t, questDBPath, "quest")

	host = NewDaemonHost()
	return host, loreDBPath, questDBPath
}

// migrateWatchDB opens and migrates one DB at path so seeding writes
// against the real schema production uses.
func migrateWatchDB(t *testing.T, path, description string) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open %s db: %v", description, err)
	}
	defer func() { _ = db.Close() }()
	if err := storage.Migrate(ctx, db, description); err != nil {
		t.Fatalf("migrate %s db: %v", description, err)
	}
}

// openWatchDB opens an already-migrated temp DB for seeding/assertions.
func openWatchDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open db %s: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// registerWatchProject registers the project in both DBs. lore.db backs
// inscribe/echoes; quest.db backs the renewal poster's task_status FK to
// its own projects table. Production's `guild lore init` / `guild quest
// init` register in both; the test mirrors that.
func registerWatchProject(t *testing.T, loreDBPath, questDBPath, projectRoot string) {
	t.Helper()
	ctx := context.Background()
	for _, path := range []string{loreDBPath, questDBPath} {
		db := openWatchDB(t, path)
		if err := project.Register(ctx, db, watchTestProject, projectRoot, "TASKS.md"); err != nil {
			t.Fatalf("register project in %s: %v", path, err)
		}
	}
}

// inscribeWatchEntry registers the project (idempotent) and inscribes one
// current entry citing filePath, returning its id. The entry is a
// principle (valid_days=never) so its only path to staleness is a
// persisted watcher signal: the test then proves the watcher, not the
// query-time decay, is what flags it.
func inscribeWatchEntry(t *testing.T, loreDBPath, questDBPath, projectRoot, filePath string) int64 {
	t.Helper()
	ctx := context.Background()
	registerWatchProject(t, loreDBPath, questDBPath, projectRoot)
	db := openWatchDB(t, loreDBPath)

	res, err := lore.Inscribe(ctx, db, &lore.InscribeParams{
		ProjectID: watchTestProject,
		Kind:      lore.KindPrinciple,
		Title:     "watcher pipeline fixture",
		Summary:   "Current entry citing a tracked file for the watch pipeline test.",
		Topic:     "watch-pipeline",
		FilePath:  filePath,
		NoWarn:    true,
	})
	if err != nil {
		t.Fatalf("inscribe entry: %v", err)
	}
	return res.Entry.ID
}

// countOpenRenewals returns how many open renewal quests exist for the
// project (the watcher's user-visible output).
func countOpenRenewals(t *testing.T, questDBPath string) int {
	t.Helper()
	db := openWatchDB(t, questDBPath)
	quests, err := quest.List(context.Background(), db, watchTestProject,
		quest.ListFilters{Epic: quest.RenewalEpic})
	if err != nil {
		t.Fatalf("list renewal quests: %v", err)
	}
	return len(quests)
}

// countSignals returns how many staleness_signals rows exist for the
// project across all sources.
func countSignals(t *testing.T, loreDBPath string) int {
	t.Helper()
	db := openWatchDB(t, loreDBPath)
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM staleness_signals WHERE project_id = ?`,
		watchTestProject).Scan(&n); err != nil {
		t.Fatalf("count signals: %v", err)
	}
	return n
}

// TestWatchProcessor_FileEventFlagsAndPostsOnce is the hermetic E2E
// acceptance: a current entry cites a tracked file; one file event flags
// exactly one staleness signal and posts exactly one open renewal quest;
// a second event for the same file does not produce a second open renewal
// quest (the poster's dedupe).
func TestWatchProcessor_FileEventFlagsAndPostsOnce(t *testing.T) {
	host, loreDBPath, questDBPath := setupWatchHost(t)
	projectRoot := t.TempDir()
	tracked := filepath.Join(projectRoot, "doc.md")
	entryID := inscribeWatchEntry(t, loreDBPath, questDBPath, projectRoot, tracked)

	proc := host.WatchProcessor(3)
	ev := watch.Event{Project: watchTestProject, Path: tracked, Kind: watch.KindFile}

	res, err := proc(context.Background(), ev)
	if err != nil {
		t.Fatalf("first event: %v", err)
	}
	if res.Signals != 1 {
		t.Fatalf("first event signals=%d, want 1 (the one entry citing %s)", res.Signals, tracked)
	}
	if res.QuestsPosted != 1 {
		t.Fatalf("first event quests=%d, want 1", res.QuestsPosted)
	}
	if got := countSignals(t, loreDBPath); got != 1 {
		t.Fatalf("after first event: %d signal rows, want 1", got)
	}
	if got := countOpenRenewals(t, questDBPath); got != 1 {
		t.Fatalf("after first event: %d open renewal quests, want 1", got)
	}

	// Second event for the same file: the signal upserts (still one row),
	// and the open renewal quest dedupes (no second quest).
	res2, err := proc(context.Background(), ev)
	if err != nil {
		t.Fatalf("second event: %v", err)
	}
	if res2.Signals != 1 {
		t.Fatalf("second event signals=%d, want 1 (upsert, same entry)", res2.Signals)
	}
	if res2.QuestsPosted != 0 {
		t.Fatalf("second event quests=%d, want 0 (dedupe against the open renewal)", res2.QuestsPosted)
	}
	if got := countSignals(t, loreDBPath); got != 1 {
		t.Fatalf("after second event: %d signal rows, want 1 (upsert)", got)
	}
	if got := countOpenRenewals(t, questDBPath); got != 1 {
		t.Fatalf("after second event: %d open renewal quests, want 1 (no duplicate)", got)
	}

	_ = entryID
}

// TestWatchProcessor_CapHonored proves the per-event renewal cap bounds a
// burst: with several entries citing the same file flagged at once and a
// cap of one, only one renewal quest is posted (the rest wait for a later
// event or the idle dream pass).
func TestWatchProcessor_CapHonored(t *testing.T) {
	host, loreDBPath, questDBPath := setupWatchHost(t)
	projectRoot := t.TempDir()
	tracked := filepath.Join(projectRoot, "doc.md")

	// Three current entries citing the same tracked file: one file event
	// flags all three.
	ctx := context.Background()
	registerWatchProject(t, loreDBPath, questDBPath, projectRoot)
	db := openWatchDB(t, loreDBPath)
	for i := 0; i < 3; i++ {
		if _, err := lore.Inscribe(ctx, db, &lore.InscribeParams{
			ProjectID: watchTestProject,
			Kind:      lore.KindPrinciple,
			Title:     "cap fixture " + string(rune('A'+i)),
			Summary:   "Current entry citing the shared tracked file.",
			Topic:     "watch-pipeline-cap",
			FilePath:  tracked,
			NoWarn:    true,
		}); err != nil {
			t.Fatalf("inscribe entry %d: %v", i, err)
		}
	}

	proc := host.WatchProcessor(1) // cap = 1
	ev := watch.Event{Project: watchTestProject, Path: tracked, Kind: watch.KindFile}
	res, err := proc(ctx, ev)
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	if res.Signals != 3 {
		t.Fatalf("signals=%d, want 3 (all three entries flagged)", res.Signals)
	}
	if res.QuestsPosted != 1 {
		t.Fatalf("quests=%d, want 1 (cap honored)", res.QuestsPosted)
	}
	if got := countOpenRenewals(t, questDBPath); got != 1 {
		t.Fatalf("open renewal quests=%d, want 1 (cap honored)", got)
	}
}

// TestWatchProcessor_ZeroCapFlagsButPostsNothing proves a non-positive cap
// records signals but mints no quests: the operator can keep event-driven
// flagging while opting out of automatic posting.
func TestWatchProcessor_ZeroCapFlagsButPostsNothing(t *testing.T) {
	host, loreDBPath, questDBPath := setupWatchHost(t)
	projectRoot := t.TempDir()
	tracked := filepath.Join(projectRoot, "doc.md")
	inscribeWatchEntry(t, loreDBPath, questDBPath, projectRoot, tracked)

	proc := host.WatchProcessor(0)
	res, err := proc(context.Background(), watch.Event{
		Project: watchTestProject, Path: tracked, Kind: watch.KindFile,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	if res.Signals != 1 {
		t.Fatalf("signals=%d, want 1", res.Signals)
	}
	if res.QuestsPosted != 0 {
		t.Fatalf("quests=%d, want 0 (zero cap)", res.QuestsPosted)
	}
	if got := countSignals(t, loreDBPath); got != 1 {
		t.Fatalf("signal rows=%d, want 1 (flagging still happens)", got)
	}
	if got := countOpenRenewals(t, questDBPath); got != 0 {
		t.Fatalf("open renewal quests=%d, want 0 (zero cap posts nothing)", got)
	}
}

// TestWatchProcessor_NoMatchingEntryNoOp proves a file event for a path no
// entry cites records no signals and posts nothing: the watcher is quiet
// for untracked files.
func TestWatchProcessor_NoMatchingEntryNoOp(t *testing.T) {
	host, loreDBPath, questDBPath := setupWatchHost(t)
	projectRoot := t.TempDir()
	tracked := filepath.Join(projectRoot, "doc.md")
	inscribeWatchEntry(t, loreDBPath, questDBPath, projectRoot, tracked)

	proc := host.WatchProcessor(3)
	other := filepath.Join(projectRoot, "untracked.md")
	res, err := proc(context.Background(), watch.Event{
		Project: watchTestProject, Path: other, Kind: watch.KindFile,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	if res.Signals != 0 || res.QuestsPosted != 0 {
		t.Fatalf("untracked file event flagged something: signals=%d quests=%d", res.Signals, res.QuestsPosted)
	}
	if got := countSignals(t, loreDBPath); got != 0 {
		t.Fatalf("signal rows=%d, want 0 for an untracked path", got)
	}
	if got := countOpenRenewals(t, questDBPath); got != 0 {
		t.Fatalf("open renewal quests=%d, want 0 for an untracked path", got)
	}
}

// TestWatchRoots_EnumeratesRegisteredProjects proves the RootsFunc maps
// project.List rows to watch roots with absolute paths.
func TestWatchRoots_EnumeratesRegisteredProjects(t *testing.T) {
	host, loreDBPath, _ := setupWatchHost(t)
	projectRoot := t.TempDir()

	db := openWatchDB(t, loreDBPath)
	if err := project.Register(context.Background(), db, watchTestProject, projectRoot, "TASKS.md"); err != nil {
		t.Fatalf("register project: %v", err)
	}

	roots, err := host.WatchRoots()(context.Background())
	if err != nil {
		t.Fatalf("watch roots: %v", err)
	}
	if len(roots) != 1 {
		t.Fatalf("got %d roots, want 1", len(roots))
	}
	if roots[0].Project != watchTestProject {
		t.Fatalf("root project=%q, want %q", roots[0].Project, watchTestProject)
	}
	if roots[0].Path != projectRoot {
		t.Fatalf("root path=%q, want %q", roots[0].Path, projectRoot)
	}
}
