package lore

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// execLookPath is a thin wrapper around exec.LookPath so tests can detect
// whether git is available without importing os/exec at the call site.
func execLookPath(name string) (string, error) { return exec.LookPath(name) }

// swapGitFileLastModifiedFn replaces the package-level seam and returns a
// restore function. Tests call t.Cleanup(restore) to ensure the original
// is always put back.
func swapGitFileLastModifiedFn(fn func(context.Context, string) time.Time) func() {
	prev := gitFileLastModifiedFn
	gitFileLastModifiedFn = fn
	return func() { gitFileLastModifiedFn = prev }
}

func TestEchoes_ProjectRequired(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	_, err := Echoes(ctx, db, "", false)
	if err == nil {
		t.Fatalf("Echoes without project should fail")
	}
}

func TestEchoes_ValidDaysElapsed(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO projects (id, path) VALUES ('p','/tmp/p')`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -40).Format(time.RFC3339)
	fresh := now.AddDate(0, 0, -5).Format(time.RFC3339)

	// Old entry with 30-day valid_days → stale
	_, err = db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, valid_days, created_at, updated_at)
		 VALUES ('p','t','research','old one','summary','current',30,?,?)`, old, old)
	if err != nil {
		t.Fatal(err)
	}
	// Fresh entry with 30-day valid_days → still good
	_, err = db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, valid_days, created_at, updated_at)
		 VALUES ('p','t','research','fresh one','summary','current',30,?,?)`, fresh, fresh)
	if err != nil {
		t.Fatal(err)
	}
	// Never-stales entry (valid_days NULL) → still good
	_, err = db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, created_at, updated_at)
		 VALUES ('p','t','principle','eternal','summary','current',?,?)`, old, old)
	if err != nil {
		t.Fatal(err)
	}

	stale, err := Echoes(ctx, db, "p", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Fatalf("got %d stale, want 1; results=%+v", len(stale), stale)
	}
	if stale[0].Entry.Title != "old one" {
		t.Fatalf("wrong entry flagged: %q", stale[0].Entry.Title)
	}
}

func TestEchoes_SkipsArchived(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO projects (id, path) VALUES ('p','/tmp/p')`)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().AddDate(0, 0, -40).Format(time.RFC3339)
	// Archived entry — even though old, should not be returned
	_, err = db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, valid_days, created_at, updated_at)
		 VALUES ('p','t','research','archived old','summary','archived',30,?,?)`, old, old)
	if err != nil {
		t.Fatal(err)
	}

	stale, err := Echoes(ctx, db, "p", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("archived entries should not appear; got %d", len(stale))
	}
}

func TestEchoes_GitAwareSkippedWhenFilePathEmpty(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO projects (id, path) VALUES ('p','/tmp/p')`)
	if err != nil {
		t.Fatal(err)
	}
	// Current, valid_days=NULL, no file_path → git-aware should NOT flag it.
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, created_at, updated_at)
		 VALUES ('p','t','research','no file','summary','current',?,?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}

	stale, err := Echoes(ctx, db, "p", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("expected 0 stale; got %d", len(stale))
	}
}

// TestEchoes_GitAwareDetectsModifiedFile verifies that when the injected
// gitFileLastModifiedFn reports a modification time after the entry's
// created_at, the entry is flagged — regardless of the process CWD.
func TestEchoes_GitAwareDetectsModifiedFile(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO projects (id, path) VALUES ('p','/tmp/p')`)
	if err != nil {
		t.Fatal(err)
	}

	entryCreated := time.Now().UTC().AddDate(0, 0, -10)
	fileModified := entryCreated.Add(24 * time.Hour) // file changed 1 day after the entry

	_, err = db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, file_path, created_at, updated_at)
		 VALUES ('p','t','research','tracked file entry','summary','current','/some/repo/file.go',?,?)`,
		entryCreated.Format(time.RFC3339), entryCreated.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}

	// Inject a seam that reports the file was modified after the entry was
	// created. The seam is called with the path from the DB; it must not
	// care about or depend on the process CWD.
	t.Cleanup(swapGitFileLastModifiedFn(func(_ context.Context, path string) time.Time {
		if path == "/some/repo/file.go" {
			return fileModified
		}
		return time.Time{}
	}))

	stale, err := Echoes(ctx, db, "p", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Fatalf("want 1 stale entry; got %d: %+v", len(stale), stale)
	}
	if stale[0].Entry.Title != "tracked file entry" {
		t.Fatalf("wrong entry flagged: %q", stale[0].Entry.Title)
	}
	if stale[0].Reason != "file modified after entry was created" {
		t.Fatalf("unexpected reason: %q", stale[0].Reason)
	}
}

// TestEchoes_GitAwareNotFlaggedWhenFileUnmodified verifies that an entry
// is not flagged when the injected fn reports the file's last-modified
// time is before (or equal to) the entry's created_at.
func TestEchoes_GitAwareNotFlaggedWhenFileUnmodified(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO projects (id, path) VALUES ('p','/tmp/p')`)
	if err != nil {
		t.Fatal(err)
	}

	entryCreated := time.Now().UTC().AddDate(0, 0, -10)

	_, err = db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, file_path, created_at, updated_at)
		 VALUES ('p','t','research','stable file entry','summary','current','/some/repo/stable.go',?,?)`,
		entryCreated.Format(time.RFC3339), entryCreated.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}

	// File was last modified before the entry was written.
	fileModified := entryCreated.Add(-24 * time.Hour)
	t.Cleanup(swapGitFileLastModifiedFn(func(_ context.Context, _ string) time.Time {
		return fileModified
	}))

	stale, err := Echoes(ctx, db, "p", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("expected 0 stale entries; got %d: %+v", len(stale), stale)
	}
}

// TestEchoes_GitAwareDegradesMissingGit verifies that when the injected fn
// returns the zero time (simulating missing git, non-repo path, or
// untracked file), the entry is NOT flagged and no error is returned.
func TestEchoes_GitAwareDegradesMissingGit(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO projects (id, path) VALUES ('p','/tmp/p')`)
	if err != nil {
		t.Fatal(err)
	}

	entryCreated := time.Now().UTC().AddDate(0, 0, -10)

	_, err = db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, file_path, created_at, updated_at)
		 VALUES ('p','t','research','non-repo file','summary','current','/not/a/repo/file.go',?,?)`,
		entryCreated.Format(time.RFC3339), entryCreated.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}

	// Simulate git missing / non-repo / untracked — fn returns zero time.
	t.Cleanup(swapGitFileLastModifiedFn(func(_ context.Context, _ string) time.Time {
		return time.Time{}
	}))

	stale, err := Echoes(ctx, db, "p", true)
	if err != nil {
		t.Fatal(err)
	}
	// Zero time is treated as "no git info" — entry must NOT be flagged.
	if len(stale) != 0 {
		t.Fatalf("expected 0 stale entries when git unavailable; got %d: %+v", len(stale), stale)
	}
}

// TestDefaultGitFileLastModified_OutsideRepo exercises the real
// defaultGitFileLastModified against a path that genuinely lives in a
// temporary directory that is NOT inside any git repository. It must
// return the zero time rather than panic or error.
func TestDefaultGitFileLastModified_OutsideRepo(t *testing.T) {
	if _, err := execLookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}
	ctx := context.Background()
	// t.TempDir() is outside any repo — git rev-parse will fail.
	path := t.TempDir() + "/nonexistent.go"
	got := defaultGitFileLastModified(ctx, path)
	if !got.IsZero() {
		t.Fatalf("expected zero time for non-repo path; got %v", got)
	}
}

// TestRepoRootResolver_CachesPerDir verifies the per-batch cache
// behaviour the issue called out: N entries in the same dir cost one
// lookup, not N. Different dirs still resolve independently and their
// cached values do not leak across each other.
func TestRepoRootResolver_CachesPerDir(t *testing.T) {
	r := newRepoRootResolver()
	calls := map[string]int{}
	r.lookup = func(_ context.Context, dir string) string {
		calls[dir]++
		switch dir {
		case "/repo-a/sub":
			return "/repo-a"
		case "/repo-b/sub":
			return "/repo-b"
		case "/not-a-repo":
			return "" // empty result is itself a cache entry
		}
		return ""
	}

	ctx := context.Background()

	// Same dir resolved 5 times -> 1 lookup, 4 cache hits.
	for i := 0; i < 5; i++ {
		if got := r.Resolve(ctx, "/repo-a/sub"); got != "/repo-a" {
			t.Fatalf("call %d: got %q, want /repo-a", i, got)
		}
	}

	// Different dir -> separate lookup, separate cache slot.
	if got := r.Resolve(ctx, "/repo-b/sub"); got != "/repo-b" {
		t.Fatalf("repo-b: got %q, want /repo-b", got)
	}

	// Empty result also cached; second call must not re-shell.
	for i := 0; i < 3; i++ {
		if got := r.Resolve(ctx, "/not-a-repo"); got != "" {
			t.Fatalf("not-a-repo call %d: got %q, want empty", i, got)
		}
	}

	if calls["/repo-a/sub"] != 1 {
		t.Errorf("repo-a/sub: %d lookups, want 1", calls["/repo-a/sub"])
	}
	if calls["/repo-b/sub"] != 1 {
		t.Errorf("repo-b/sub: %d lookups, want 1", calls["/repo-b/sub"])
	}
	if calls["/not-a-repo"] != 1 {
		t.Errorf("not-a-repo: %d lookups, want 1 (empty result must cache)", calls["/not-a-repo"])
	}
}

// TestRepoRootResolver_ContextThreading verifies that resolveRepoRoot
// finds the resolver attached to a parent context even when called
// with a derived (timeout-scoped) context — the realistic shape
// defaultGitFileLastModified produces.
func TestRepoRootResolver_ContextThreading(t *testing.T) {
	r := newRepoRootResolver()
	var calls int
	r.lookup = func(_ context.Context, _ string) string {
		calls++
		return "/repo"
	}

	parent := withRepoRootResolver(context.Background(), r)
	derived, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		if got := resolveRepoRoot(derived, "/d"); got != "/repo" {
			t.Fatalf("call %d: got %q, want /repo", i, got)
		}
	}

	if calls != 1 {
		t.Errorf("got %d lookups across the batch, want 1", calls)
	}
}
