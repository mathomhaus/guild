package lore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/command"
)

// insertEchoEntry inserts one entry row shaped for staleness tests and
// returns its ID. validDays <= 0 means NULL (never time-stales).
func insertEchoEntry(t *testing.T, db *sql.DB, project, title, filePath string, validDays int, status string, createdAt time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	var vd any
	if validDays > 0 {
		vd = validDays
	}
	var fp any
	if filePath != "" {
		fp = filePath
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, valid_days, file_path, created_at, updated_at)
		 VALUES (?, 't', 'research', ?, 'summary', ?, ?, ?, ?, ?)`,
		project, title, status, vd, fp,
		createdAt.Format(time.RFC3339), createdAt.Format(time.RFC3339),
	)
	if err != nil {
		t.Fatalf("insert entry %q: %v", title, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

// countSignals returns the number of staleness_signals rows for one entry.
func countSignals(t *testing.T, db *sql.DB, entryID int64) int {
	t.Helper()
	var n int
	err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM staleness_signals WHERE entry_id = ?`, entryID).Scan(&n)
	if err != nil {
		t.Fatalf("count signals: %v", err)
	}
	return n
}

func TestFlagStaleByPath_FlagsMatchingCurrentEntries(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "p", "q")
	created := time.Now().UTC().AddDate(0, 0, -3)

	match1 := insertEchoEntry(t, db, "p", "match one", "/repo/a.go", 0, "current", created)
	match2 := insertEchoEntry(t, db, "p", "match two", "/repo/a.go", 0, "current", created)
	insertEchoEntry(t, db, "p", "archived match", "/repo/a.go", 0, "archived", created)
	insertEchoEntry(t, db, "p", "other path", "/repo/b.go", 0, "current", created)
	insertEchoEntry(t, db, "q", "other project", "/repo/a.go", 0, "current", created)

	now := time.Now().UTC().Truncate(time.Second)
	ids, err := FlagStaleByPath(ctx, db, "p", "/repo/a.go", SourceWatcherFile, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{match1, match2}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("flagged ids = %v, want %v", ids, want)
	}

	for _, id := range want {
		var project, reason, source, observedAt string
		err := db.QueryRowContext(ctx,
			`SELECT project_id, reason, source, observed_at FROM staleness_signals WHERE entry_id = ?`, id,
		).Scan(&project, &reason, &source, &observedAt)
		if err != nil {
			t.Fatalf("signal row for entry %d: %v", id, err)
		}
		if project != "p" || source != SourceWatcherFile {
			t.Errorf("entry %d: project=%q source=%q", id, project, source)
		}
		if reason == "" {
			t.Errorf("entry %d: empty reason", id)
		}
		if observedAt != now.Format(time.RFC3339) {
			t.Errorf("entry %d: observed_at=%q want %q", id, observedAt, now.Format(time.RFC3339))
		}
	}

	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM staleness_signals`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("total signal rows = %d, want 2 (archived / other path / other project must not be flagged)", total)
	}
}

func TestFlagStaleByPath_NoMatchReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "p")

	ids, err := FlagStaleByPath(ctx, db, "p", "/no/such/file.go", SourceWatcherFile, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("flagged ids = %v, want none", ids)
	}
}

func TestFlagStaleByPath_Validation(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "p")
	now := time.Now().UTC()

	if _, err := FlagStaleByPath(ctx, nil, "p", "/a", SourceWatcherFile, now); err == nil {
		t.Error("nil db should fail")
	}
	if _, err := FlagStaleByPath(ctx, db, "", "/a", SourceWatcherFile, now); err == nil {
		t.Error("empty project should fail")
	}
	if _, err := FlagStaleByPath(ctx, db, "p", "", SourceWatcherFile, now); err == nil {
		t.Error("empty path should fail")
	}
	if _, err := FlagStaleByPath(ctx, db, "p", "/a", "", now); err == nil {
		t.Error("empty source should fail")
	}
}

// TestFlagStaleByPath_IdempotentUpsert is the acceptance idempotence
// check: flagging the same entry+source twice yields one signal row and
// one echo line, with the row refreshed to the later observation.
func TestFlagStaleByPath_IdempotentUpsert(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "p")
	created := time.Now().UTC().AddDate(0, 0, -2)
	id := insertEchoEntry(t, db, "p", "flagged twice", "/repo/a.go", 0, "current", created)

	first := time.Now().UTC().Truncate(time.Second)
	second := first.Add(time.Minute)
	for _, now := range []time.Time{first, second} {
		ids, err := FlagStaleByPath(ctx, db, "p", "/repo/a.go", SourceWatcherFile, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 || ids[0] != id {
			t.Fatalf("flagged ids = %v, want [%d]", ids, id)
		}
	}

	if n := countSignals(t, db, id); n != 1 {
		t.Fatalf("signal rows = %d, want 1 (repeat flag must upsert)", n)
	}
	var observedAt string
	if err := db.QueryRowContext(ctx,
		`SELECT observed_at FROM staleness_signals WHERE entry_id = ?`, id).Scan(&observedAt); err != nil {
		t.Fatal(err)
	}
	if observedAt != second.Format(time.RFC3339) {
		t.Errorf("observed_at = %q, want refreshed to %q", observedAt, second.Format(time.RFC3339))
	}

	echoes, err := Echoes(ctx, db, "p", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(echoes) != 1 {
		t.Fatalf("echoes = %d, want exactly 1 line for the twice-flagged entry", len(echoes))
	}
	if echoes[0].Entry.ID != id || echoes[0].Reason != reasonFileChanged {
		t.Errorf("echo = (LORE-%d, %q), want (LORE-%d, %q)",
			echoes[0].Entry.ID, echoes[0].Reason, id, reasonFileChanged)
	}
}

// TestEchoes_SignalOnlySurfacesWithoutGitAware is the union acceptance
// check: an entry flagged only by a persisted signal appears in echoes
// output even when gitAware=false, with the persisted reason string.
func TestEchoes_SignalOnlySurfacesWithoutGitAware(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "p")
	created := time.Now().UTC().AddDate(0, 0, -1)
	id := insertEchoEntry(t, db, "p", "signal only", "/repo/a.go", 0, "current", created)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO staleness_signals (entry_id, project_id, reason, source, observed_at)
		 VALUES (?, 'p', 'observed by watcher', ?, ?)`,
		id, SourceWatcherFile, time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatal(err)
	}

	for _, gitAware := range []bool{false, true} {
		echoes, err := Echoes(ctx, db, "p", gitAware)
		if err != nil {
			t.Fatal(err)
		}
		if len(echoes) != 1 {
			t.Fatalf("gitAware=%v: echoes = %d, want 1", gitAware, len(echoes))
		}
		if echoes[0].Entry.ID != id || echoes[0].Reason != "observed by watcher" {
			t.Errorf("gitAware=%v: echo = (LORE-%d, %q), want persisted reason for LORE-%d",
				gitAware, echoes[0].Entry.ID, echoes[0].Reason, id)
		}
	}
}

// TestEchoes_SignalReasonSuffixedToQueryReason pins the chosen dedupe
// rule: when an entry is flagged by both a query-time check and a
// persisted signal, one echo line surfaces with the signal reason
// suffixed after the query-time reason.
func TestEchoes_SignalReasonSuffixedToQueryReason(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "p")
	created := time.Now().UTC().AddDate(0, 0, -40)
	id := insertEchoEntry(t, db, "p", "doubly stale", "/repo/a.go", 30, "current", created)

	if _, err := FlagStaleByPath(ctx, db, "p", "/repo/a.go", SourceWatcherFile, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	echoes, err := Echoes(ctx, db, "p", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(echoes) != 1 {
		t.Fatalf("echoes = %d, want exactly 1 (no duplicate line per entry)", len(echoes))
	}
	want := fmt.Sprintf("40d old (valid: 30d); %s", reasonFileChanged)
	if echoes[0].Entry.ID != id || echoes[0].Reason != want {
		t.Errorf("echo = (LORE-%d, %q), want (LORE-%d, %q)",
			echoes[0].Entry.ID, echoes[0].Reason, id, want)
	}
}

// TestEchoes_SignalStopsWhenEntryLeavesCurrent pins the invalidation
// rule: signals are read-side-invalidated, so the moment an entry
// leaves status='current' (reforge -> superseded, seal -> archived)
// its signals stop surfacing. The rows themselves remain inert.
func TestEchoes_SignalStopsWhenEntryLeavesCurrent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		retire func(t *testing.T, db *sql.DB, id int64)
	}{
		{
			name: "seal to archived",
			retire: func(t *testing.T, db *sql.DB, id int64) {
				t.Helper()
				if _, err := Seal(context.Background(), db, id, "p", time.Now().UTC()); err != nil {
					t.Fatalf("seal: %v", err)
				}
			},
		},
		{
			name: "reforge to superseded",
			retire: func(t *testing.T, db *sql.DB, id int64) {
				t.Helper()
				_, err := db.ExecContext(context.Background(),
					`UPDATE entries SET status = 'superseded' WHERE id = ?`, id)
				if err != nil {
					t.Fatalf("supersede: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := openTestDB(t, "p")
			created := time.Now().UTC().AddDate(0, 0, -1)
			id := insertEchoEntry(t, db, "p", "retired entry", "/repo/a.go", 0, "current", created)

			if _, err := FlagStaleByPath(ctx, db, "p", "/repo/a.go", SourceWatcherFile, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			echoes, err := Echoes(ctx, db, "p", false)
			if err != nil {
				t.Fatal(err)
			}
			if len(echoes) != 1 {
				t.Fatalf("pre-retire echoes = %d, want 1", len(echoes))
			}

			tc.retire(t, db, id)

			echoes, err = Echoes(ctx, db, "p", false)
			if err != nil {
				t.Fatal(err)
			}
			if len(echoes) != 0 {
				t.Fatalf("post-retire echoes = %d, want 0 (signal must stop surfacing)", len(echoes))
			}
			if n := countSignals(t, db, id); n != 1 {
				t.Errorf("signal rows = %d, want 1 (invalidation is read-side, not a delete)", n)
			}
		})
	}
}

// TestGitSweep_PersistsHitsAndInstallsResolver verifies the sweep
// replays the git-aware check through the shared seam (with the
// per-batch repo-root resolver installed) and persists a git-sweep
// signal per hit, idempotently.
func TestGitSweep_PersistsHitsAndInstallsResolver(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "p")
	created := time.Now().UTC().AddDate(0, 0, -10)

	hit := insertEchoEntry(t, db, "p", "modified in git", "/repo/changed.go", 0, "current", created)
	insertEchoEntry(t, db, "p", "untouched in git", "/repo/stable.go", 0, "current", created)
	insertEchoEntry(t, db, "p", "no file", "", 0, "current", created)

	resolverInstalled := false
	t.Cleanup(swapGitFileLastModifiedFn(func(ctx context.Context, path string) time.Time {
		if _, ok := ctx.Value(repoRootResolverKey{}).(*repoRootResolver); ok {
			resolverInstalled = true
		}
		if path == "/repo/changed.go" {
			return created.Add(24 * time.Hour)
		}
		return time.Time{}
	}))

	now := time.Now().UTC().Truncate(time.Second)
	for run := 0; run < 2; run++ {
		ids, err := GitSweep(ctx, db, "p", now)
		if err != nil {
			t.Fatalf("sweep %d: %v", run, err)
		}
		if len(ids) != 1 || ids[0] != hit {
			t.Fatalf("sweep %d: flagged ids = %v, want [%d]", run, ids, hit)
		}
	}
	if !resolverInstalled {
		t.Error("git seam ran without the per-batch repo-root resolver installed")
	}

	if n := countSignals(t, db, hit); n != 1 {
		t.Fatalf("signal rows = %d, want 1 (repeat sweep must upsert)", n)
	}
	var source string
	if err := db.QueryRowContext(ctx,
		`SELECT source FROM staleness_signals WHERE entry_id = ?`, hit).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != SourceGitSweep {
		t.Errorf("source = %q, want %q", source, SourceGitSweep)
	}

	// The persisted hit now surfaces on a plain (gitAware=false) read,
	// which is the entire point: later reads are subprocess-free.
	echoes, err := Echoes(ctx, db, "p", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(echoes) != 1 || echoes[0].Entry.ID != hit || echoes[0].Reason != reasonFileChanged {
		t.Fatalf("echoes after sweep = %+v, want one line for LORE-%d with %q", echoes, hit, reasonFileChanged)
	}
}

func TestGitSweep_Validation(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "p")

	if _, err := GitSweep(ctx, nil, "p", time.Now().UTC()); err == nil {
		t.Error("nil db should fail")
	}
	if _, err := GitSweep(ctx, db, "", time.Now().UTC()); err == nil {
		t.Error("empty project should fail")
	}
}

// TestEchoes_ParityWithZeroSignals is the regression net for the
// ADR-005 hard invariant: in an install where nothing ever writes
// signal rows, Echoes output (entries, reasons, order, and formatted
// bytes) is identical to the pre-signals behavior, for both gitAware
// modes.
func TestEchoes_ParityWithZeroSignals(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "p")
	now := time.Now().UTC()

	expired := insertEchoEntry(t, db, "p", "expired entry", "", 30, "current", now.AddDate(0, 0, -40))
	gitHit := insertEchoEntry(t, db, "p", "git modified entry", "/repo/changed.go", 0, "current", now.AddDate(0, 0, -10))
	insertEchoEntry(t, db, "p", "fresh entry", "/repo/stable.go", 30, "current", now.AddDate(0, 0, -5))

	t.Cleanup(swapGitFileLastModifiedFn(func(_ context.Context, path string) time.Time {
		if path == "/repo/changed.go" {
			return now.AddDate(0, 0, -9) // one day after gitHit's created_at
		}
		return time.Time{}
	}))

	type line struct {
		id     int64
		reason string
	}
	for _, tc := range []struct {
		gitAware bool
		want     []line
	}{
		{gitAware: false, want: []line{
			{expired, "40d old (valid: 30d)"},
		}},
		{gitAware: true, want: []line{
			{expired, "40d old (valid: 30d)"},
			{gitHit, "file modified after entry was created"},
		}},
	} {
		echoes, err := Echoes(ctx, db, "p", tc.gitAware)
		if err != nil {
			t.Fatalf("gitAware=%v: %v", tc.gitAware, err)
		}
		if len(echoes) != len(tc.want) {
			t.Fatalf("gitAware=%v: %d echoes, want %d: %+v", tc.gitAware, len(echoes), len(tc.want), echoes)
		}
		var wantFormatted strings.Builder
		fmt.Fprintf(&wantFormatted, "[stale] %d fading echoes:\n", len(tc.want))
		for i, w := range tc.want {
			if echoes[i].Entry.ID != w.id || echoes[i].Reason != w.reason {
				t.Errorf("gitAware=%v echo[%d] = (LORE-%d, %q), want (LORE-%d, %q)",
					tc.gitAware, i, echoes[i].Entry.ID, echoes[i].Reason, w.id, w.reason)
			}
			fmt.Fprintf(&wantFormatted, "  %s  [%s]  %s\n", formatEntryID(w.id), echoes[i].Entry.Kind, w.reason)
			fmt.Fprintf(&wantFormatted, "  %s\n", echoes[i].Entry.Title)
			if echoes[i].Entry.FilePath != "" {
				fmt.Fprintf(&wantFormatted, "  file: %s\n", echoes[i].Entry.FilePath)
			}
		}
		got := formatEchoes(command.CLISink{NoEmoji: true}, EchoesOutput{Echoes: echoes})
		want := strings.TrimRight(wantFormatted.String(), "\n")
		if got != want {
			t.Errorf("gitAware=%v formatted output diverged:\n got: %q\nwant: %q", tc.gitAware, got, want)
		}
	}
}
