package sleep

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/quest"
)

// renewalSubjectPrefix is the canonical prefix every renewal quest
// subject carries. The merged poster (internal/quest/renewal.go) renders
// `renew lore: ENTRY-N "<title>" (<reason>)`; this step is its
// sleep-pass caller and does not reshape the subject, so the tests
// assert the poster's real contract.
const renewalSubjectPrefix = "renew lore: "

// seedStaleEntry inserts one current entry whose valid_days window has
// already lapsed (created_at = now - (validDays + ageOver) days), so
// lore.Echoes flags it on a time basis alone. Returns the entry id.
func seedStaleEntry(t *testing.T, db *sql.DB, project, kind, title string, validDays, ageOverDays int) int64 {
	t.Helper()
	created := time.Now().UTC().AddDate(0, 0, -(validDays + ageOverDays)).Format(time.RFC3339)
	res, err := db.ExecContext(context.Background(),
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, valid_days, created_at, updated_at)
		 VALUES (?, 'test', ?, ?, 'summary', 'current', ?, ?, ?)`,
		project, kind, title, validDays, created, created,
	)
	if err != nil {
		t.Fatalf("seed stale entry: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("seed stale entry id: %v", err)
	}
	return id
}

// runRenewal executes one full pass with just the renewal step and
// returns the pass result, failing on substrate or step errors.
func runRenewal(t *testing.T, loreDB, questDB *sql.DB, caps Caps) *PassResult {
	t.Helper()
	pc := &PassContext{
		LoreDB:  loreDB,
		QuestDB: questDB,
		Trigger: TriggerAutopass,
		Caps:    caps,
	}
	res, err := Run(context.Background(), pc, []Step{RenewalStep{}}, time.Minute)
	if err != nil {
		t.Fatalf("run pass: %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(res.Steps))
	}
	if res.Steps[0].Err != nil {
		t.Fatalf("step error: %v", res.Steps[0].Err)
	}
	return res
}

// renewalOps returns the journal rows of a pass that name the renewal
// step.
func renewalOps(t *testing.T, loreDB *sql.DB, passID int64) []Op {
	t.Helper()
	var out []Op
	for _, op := range passOps(t, loreDB, passID) {
		if op.Step == StepNameRenewal {
			out = append(out, op)
		}
	}
	return out
}

// entryStatus returns the status of one entry; used to prove the step
// never mutates lore.
func entryStatus(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var status string
	if err := db.QueryRowContext(context.Background(),
		`SELECT status FROM entries WHERE id = ?`, id,
	).Scan(&status); err != nil {
		t.Fatalf("entry status %d: %v", id, err)
	}
	return status
}

// TestRenewal_PostsCappedRenewalQuests seeds stale entries and asserts
// the step posts one renewal quest per entry with the renewal subject
// prefix, RenewalPriority, the RenewalEpic campaign, and acceptance text
// that routes the executor through the three resolution verbs. The step
// must leave every entry's status untouched: renewal is the gate in
// front of lore mutation, never the mutation itself.
func TestRenewal_PostsCappedRenewalQuests(t *testing.T) {
	loreDB := openConsolidateDB(t, "lore.db", "alpha")
	questDB := openConsolidateDB(t, "quest.db", "alpha")

	a := seedStaleEntry(t, loreDB, "alpha", "research", "stale research note one", 30, 20)
	b := seedStaleEntry(t, loreDB, "alpha", "decision", "stale decision note two", 180, 10)

	res := runRenewal(t, loreDB, questDB, Caps{MaxRenewalPosts: 5})

	subjects := questSubjects(t, questDB)
	if len(subjects) != 2 {
		t.Fatalf("posted %d quests, want 2: %v", len(subjects), subjects)
	}
	for ref, subj := range subjects {
		if !strings.HasPrefix(subj, renewalSubjectPrefix) {
			t.Errorf("quest %s subject %q lacks renewal prefix", ref, subj)
		}
		q := mustLoadByRef(t, questDB, ref)
		if q.Priority != quest.RenewalPriority {
			t.Errorf("quest %s priority = %q, want %q", ref, q.Priority, quest.RenewalPriority)
		}
		if q.Epic != quest.RenewalEpic {
			t.Errorf("quest %s epic = %q, want %q", ref, q.Epic, quest.RenewalEpic)
		}
		// Resolution verbs the poster routes through. lore update +
		// lore reforge survive the [spec]-note round-trip; the poster's
		// criterion also names sealing for obsolete entries, but the
		// [spec]-note replay (internal/quest/spec.go splits the payload on
		// "; ") drops the post-semicolon "seal it" tail on read. The step
		// delegates to the merged poster and does not reshape its
		// acceptance, so the test asserts only what the round-trip
		// preserves.
		joined := strings.ToLower(strings.Join(q.Acceptance, " | "))
		for _, verb := range []string{"lore update", "lore reforge"} {
			if !strings.Contains(joined, verb) {
				t.Errorf("quest %s acceptance missing %q resolution verb: %v", ref, verb, q.Acceptance)
			}
		}
	}

	// Lore is never mutated by this step.
	if got := entryStatus(t, loreDB, a); got != "current" {
		t.Errorf("entry %d status = %q, want current (step must not mutate lore)", a, got)
	}
	if got := entryStatus(t, loreDB, b); got != "current" {
		t.Errorf("entry %d status = %q, want current (step must not mutate lore)", b, got)
	}

	// Each post is journaled as policy auto, op renewal_quest_post,
	// applied=true. Subject reason from echoReason rides in the subject;
	// the op detail records the entry, quest, and reason for narration.
	ops := renewalOps(t, loreDB, res.PassID)
	posted := 0
	for _, op := range ops {
		if !op.Applied {
			continue
		}
		posted++
		if op.Policy != PolicyAuto {
			t.Errorf("op %s policy = %q, want auto", op.Target, op.Policy)
		}
		if op.Kind != OpRenewalQuestPost {
			t.Errorf("op %s kind = %q, want %q", op.Target, op.Kind, OpRenewalQuestPost)
		}
		d := detailMap(t, op)
		if d["reason"] == "" || d["reason"] == nil {
			t.Errorf("op %s detail missing staleness reason: %v", op.Target, d)
		}
	}
	if posted != 2 {
		t.Errorf("journaled %d applied renewal ops, want 2", posted)
	}
}

// TestRenewal_DedupeRerun proves a second pass posts no duplicate quest
// for an entry whose first renewal quest is still open. Dedupe lives in
// the poster (open-marker scan); the step inherits it. A pass that posts
// nothing still completes cleanly.
func TestRenewal_DedupeRerun(t *testing.T) {
	loreDB := openConsolidateDB(t, "lore.db", "alpha")
	questDB := openConsolidateDB(t, "quest.db", "alpha")

	seedStaleEntry(t, loreDB, "alpha", "research", "stale note", 30, 15)

	first := runRenewal(t, loreDB, questDB, Caps{MaxRenewalPosts: 5})
	if got := len(questSubjects(t, questDB)); got != 1 {
		t.Fatalf("first pass posted %d quests, want 1", got)
	}
	if applied := appliedRenewalOps(t, loreDB, first.PassID); applied != 1 {
		t.Fatalf("first pass applied %d renewal ops, want 1", applied)
	}

	second := runRenewal(t, loreDB, questDB, Caps{MaxRenewalPosts: 5})
	if got := len(questSubjects(t, questDB)); got != 1 {
		t.Errorf("second pass posted a duplicate; total quests = %d, want 1", got)
	}
	if applied := appliedRenewalOps(t, loreDB, second.PassID); applied != 0 {
		t.Errorf("second pass applied %d renewal ops, want 0 (deduped)", applied)
	}
}

// TestRenewal_CapAndOverflow seeds more stale entries than the cap and
// asserts only the cap is posted, oldest entry first, with the overflow
// count journaled. Oldest-first ordering is the poster's deterministic
// selection (Echoes returns created_at ASC); proven here by which
// entries get a quest.
func TestRenewal_CapAndOverflow(t *testing.T) {
	loreDB := openConsolidateDB(t, "lore.db", "alpha")
	questDB := openConsolidateDB(t, "quest.db", "alpha")

	// Three stale entries with strictly increasing ages so created_at
	// order is unambiguous: oldest first is the highest ageOver.
	oldest := seedStaleEntry(t, loreDB, "alpha", "research", "oldest stale note", 30, 60)
	middle := seedStaleEntry(t, loreDB, "alpha", "research", "middle stale note", 30, 40)
	newest := seedStaleEntry(t, loreDB, "alpha", "research", "newest stale note", 30, 20)

	res := runRenewal(t, loreDB, questDB, Caps{MaxRenewalPosts: 2})

	// Exactly the cap is posted.
	subjects := questSubjects(t, questDB)
	if len(subjects) != 2 {
		t.Fatalf("posted %d quests, want 2 (the cap)", len(subjects))
	}

	// The two oldest entries got quests; the newest did not. The marker
	// note carries the entry id, so the open-renewal scan tells us which.
	if !hasOpenRenewal(t, questDB, "alpha", oldest) {
		t.Errorf("oldest entry %d should have a renewal quest", oldest)
	}
	if !hasOpenRenewal(t, questDB, "alpha", middle) {
		t.Errorf("middle entry %d should have a renewal quest", middle)
	}
	if hasOpenRenewal(t, questDB, "alpha", newest) {
		t.Errorf("newest entry %d is over cap and should NOT have a renewal quest", newest)
	}

	// Overflow count of 1 is journaled exactly once.
	overflow := 0
	markers := 0
	for _, op := range renewalOps(t, loreDB, res.PassID) {
		d := detailMap(t, op)
		if v, ok := d["suppressed"]; ok {
			markers++
			if n, ok := v.(float64); ok {
				overflow = int(n)
			}
		}
	}
	if markers != 1 {
		t.Errorf("overflow markers = %d, want exactly 1", markers)
	}
	if overflow != 1 {
		t.Errorf("journaled overflow = %d, want 1", overflow)
	}
}

// TestRenewal_GitAwareFileChanged covers the git-aware staleness reason
// using a real temp git repo: an entry whose file_path points at a
// committed file that was last modified AFTER the entry was inscribed is
// flagged "file modified after entry was created", even though its
// valid_days window has not lapsed. Exercises the file-changed branch
// the step inherits from lore.Echoes(gitAware=true).
func TestRenewal_GitAwareFileChanged(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := t.TempDir()
	gitInit(t, repo)

	// Commit a file with an explicit author/committer date well in the
	// past but still AFTER the entry's created_at below.
	relPath := "docs/contract.md"
	abs := filepath.Join(repo, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, []byte("contract body\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	commitDate := time.Now().UTC().AddDate(0, 0, -10)
	gitCommit(t, repo, "add contract", commitDate)

	loreDB := openConsolidateDB(t, "lore.db", "alpha")
	questDB := openConsolidateDB(t, "quest.db", "alpha")

	// Entry created 30 days ago (before the commit), valid_days NULL so
	// it never time-stales. file_path is the absolute path to the
	// committed file, mirroring how entries store source pointers.
	entryCreated := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339)
	res, err := loreDB.ExecContext(context.Background(),
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, valid_days, file_path, created_at, updated_at)
		 VALUES ('alpha', 'test', 'decision', 'git-tracked contract entry', 'summary', 'current', NULL, ?, ?, ?)`,
		abs, entryCreated, entryCreated,
	)
	if err != nil {
		t.Fatalf("seed git-tracked entry: %v", err)
	}
	entryID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("seed git-tracked entry id: %v", err)
	}

	passRes := runRenewal(t, loreDB, questDB, Caps{MaxRenewalPosts: 5})

	if !hasOpenRenewal(t, questDB, "alpha", entryID) {
		t.Fatalf("git-changed entry %d should have a renewal quest", entryID)
	}

	// The git reason rides into the journaled op detail.
	found := false
	for _, op := range renewalOps(t, loreDB, passRes.PassID) {
		if !op.Applied {
			continue
		}
		d := detailMap(t, op)
		reason, _ := d["reason"].(string)
		if strings.Contains(reason, "file modified after entry was created") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the git file-changed reason in a journaled renewal op")
	}
}

// TestRenewal_NoQuestDB journals a clean skip (no quest db wired) instead
// of erroring, mirroring the embed step's nil-corpus skip contract.
func TestRenewal_NoQuestDB(t *testing.T) {
	loreDB := openConsolidateDB(t, "lore.db", "alpha")
	seedStaleEntry(t, loreDB, "alpha", "research", "stale note", 30, 15)

	pc := &PassContext{LoreDB: loreDB, QuestDB: nil, Trigger: TriggerAutopass, Caps: Caps{MaxRenewalPosts: 5}}
	res, err := Run(context.Background(), pc, []Step{RenewalStep{}}, time.Minute)
	if err != nil {
		t.Fatalf("run pass: %v", err)
	}
	if res.Steps[0].Err != nil {
		t.Fatalf("step error: %v", res.Steps[0].Err)
	}
	ops := renewalOps(t, loreDB, res.PassID)
	if len(ops) != 1 || ops[0].Applied {
		t.Fatalf("want one unapplied skip op, got %+v", ops)
	}
	if d := detailMap(t, ops[0]); d["skipped"] != reasonRenewalQuestDBNotWired {
		t.Errorf("skip reason = %v, want %q", d["skipped"], reasonRenewalQuestDBNotWired)
	}
}

// TestRenewal_ZeroCapSkips journals a clean skip when the cap is zero,
// so a misconfigured pass is visible rather than silently inert.
func TestRenewal_ZeroCapSkips(t *testing.T) {
	loreDB := openConsolidateDB(t, "lore.db", "alpha")
	questDB := openConsolidateDB(t, "quest.db", "alpha")
	seedStaleEntry(t, loreDB, "alpha", "research", "stale note", 30, 15)

	res := runRenewal(t, loreDB, questDB, Caps{MaxRenewalPosts: 0})

	if got := len(questSubjects(t, questDB)); got != 0 {
		t.Errorf("zero cap posted %d quests, want 0", got)
	}
	ops := renewalOps(t, loreDB, res.PassID)
	if len(ops) != 1 || ops[0].Applied {
		t.Fatalf("want one unapplied skip op, got %+v", ops)
	}
	if d := detailMap(t, ops[0]); d["skipped"] != reasonRenewalCapZero {
		t.Errorf("skip reason = %v, want %q", d["skipped"], reasonRenewalCapZero)
	}
}

// TestRenewal_ContextCancellation proves the step honors a pre-cancelled
// context: it posts nothing and returns the cancellation error without
// touching the quest board.
func TestRenewal_ContextCancellation(t *testing.T) {
	loreDB := openConsolidateDB(t, "lore.db", "alpha")
	questDB := openConsolidateDB(t, "quest.db", "alpha")
	seedStaleEntry(t, loreDB, "alpha", "research", "stale note", 30, 15)

	// Begin a real pass so PassID is valid, then run the step directly
	// under an already-cancelled context.
	passID, err := BeginPass(context.Background(), loreDB, TriggerAutopass, time.Minute, time.Now().UTC())
	if err != nil {
		t.Fatalf("begin pass: %v", err)
	}
	pc := &PassContext{LoreDB: loreDB, QuestDB: questDB, Trigger: TriggerAutopass, Caps: Caps{MaxRenewalPosts: 5}, PassID: passID}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, runErr := RenewalStep{}.Run(ctx, pc)
	if runErr == nil {
		t.Fatal("want a cancellation error, got nil")
	}
	if got := len(questSubjects(t, questDB)); got != 0 {
		t.Errorf("cancelled pass posted %d quests, want 0", got)
	}
}

// TestRenewal_HealthyCorpusNoPosts asserts a corpus with no fading
// echoes posts nothing and writes no renewal journal rows: the healthy
// case stays quiet so the journal does not fill with no-op rows.
func TestRenewal_HealthyCorpusNoPosts(t *testing.T) {
	loreDB := openConsolidateDB(t, "lore.db", "alpha")
	questDB := openConsolidateDB(t, "quest.db", "alpha")
	// Fresh entry inside its validity window: not an echo.
	seedEntry(t, loreDB, "alpha", "research", "fresh note", "summary")

	res := runRenewal(t, loreDB, questDB, Caps{MaxRenewalPosts: 5})

	if got := len(questSubjects(t, questDB)); got != 0 {
		t.Errorf("healthy corpus posted %d quests, want 0", got)
	}
	if ops := renewalOps(t, loreDB, res.PassID); len(ops) != 0 {
		t.Errorf("healthy corpus journaled %d renewal ops, want 0: %+v", len(ops), ops)
	}
}

// --- test helpers ---

// appliedRenewalOps counts journaled renewal ops in a pass with
// applied=true (the actual posts, excluding skip/overflow markers).
func appliedRenewalOps(t *testing.T, loreDB *sql.DB, passID int64) int {
	t.Helper()
	n := 0
	for _, op := range renewalOps(t, loreDB, passID) {
		if op.Applied {
			n++
		}
	}
	return n
}

// hasOpenRenewal reports whether an open (next/in_progress/blocked)
// renewal quest exists for entryID in projectID, matched on the poster's
// marker note.
func hasOpenRenewal(t *testing.T, questDB *sql.DB, projectID string, entryID int64) bool {
	t.Helper()
	marker := fmt.Sprintf("%sENTRY-%d", quest.NotePrefixRenewal, entryID)
	var one int
	err := questDB.QueryRowContext(context.Background(),
		`SELECT 1 FROM task_notes n
		 JOIN task_status s ON s.project_id = n.project_id AND s.task_id = n.task_id
		 WHERE n.project_id = ? AND n.note = ?
		   AND s.status IN ('next', 'in_progress', 'blocked') LIMIT 1`,
		projectID, marker,
	).Scan(&one)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatalf("hasOpenRenewal: %v", err)
	}
	return true
}

// mustLoadByRef loads a quest given a "project/QUEST-N" ref.
func mustLoadByRef(t *testing.T, db *sql.DB, ref string) *quest.Quest {
	t.Helper()
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 {
		t.Fatalf("bad quest ref %q", ref)
	}
	q, err := quest.Load(context.Background(), db, parts[0], parts[1])
	if err != nil {
		t.Fatalf("load quest %s: %v", ref, err)
	}
	return q
}

// gitInit initializes a git repo in dir with a deterministic identity so
// commits are reproducible across machines.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, nil, "init", "-q")
	runGit(t, dir, nil, "config", "user.email", "test@example.com")
	runGit(t, dir, nil, "config", "user.name", "Test")
	runGit(t, dir, nil, "config", "commit.gpgsign", "false")
}

// gitCommit stages everything and commits with an explicit author /
// committer date so the file's git mtime is deterministic.
func gitCommit(t *testing.T, dir, msg string, when time.Time) {
	t.Helper()
	runGit(t, dir, nil, "add", "-A")
	stamp := when.Format(time.RFC3339)
	env := append(os.Environ(),
		"GIT_AUTHOR_DATE="+stamp,
		"GIT_COMMITTER_DATE="+stamp,
	)
	runGit(t, dir, env, "commit", "-q", "-m", msg)
}

// runGit runs a git command in dir, failing the test on error.
func runGit(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
