package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/lore/embed"
	"github.com/mathomhaus/guild/internal/quest"
	"github.com/mathomhaus/guild/internal/storage"
)

// setupQuestCLI wires an in-tmpdir quest.db and a registered project,
// then returns a cleanup func that restores the global override.
func setupQuestCLI(t *testing.T, projectName string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "quest.db")
	questDBPathOverride = dbPath
	t.Cleanup(func() { questDBPathOverride = "" })

	// Seed the DB with schema + project row.
	ctx := context.Background()
	db, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := storage.Migrate(ctx, db, "quest"); err != nil {
		t.Fatalf("seed migrate: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (id, path, tasks_file) VALUES (?, ?, ?)`,
		projectName, dir, "TASKS.md",
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

// runQuest invokes rootCmd with the given args and returns captured
// stdout/stderr. Resets flag state first so prior tests don't bleed in.
func runQuest(t *testing.T, args []string) (stdout, stderr string, err error) {
	t.Helper()
	resetQuestFlagState()
	ob := new(bytes.Buffer)
	eb := new(bytes.Buffer)
	rootCmd.SetOut(ob)
	rootCmd.SetErr(eb)
	rootCmd.SetArgs(args)
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	err = rootCmd.Execute()
	return ob.String(), eb.String(), err
}

func TestCLI_QuestPostClearRoundTrip(t *testing.T) {
	setupQuestCLI(t, "guild-cli-test")

	// Post.
	stdout, _, err := runQuest(t, []string{"quest", "post",
		"--project", "guild-cli-test",
		"hello world"})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if !strings.Contains(stdout, "posted QUEST-1: hello world") {
		t.Errorf("post stdout = %q, want contains 'posted QUEST-1: hello world'", stdout)
	}

	// Fulfill (via the `clear` cobra alias to verify QUEST-106 backward compat).
	stdout, _, err = runQuest(t, []string{"quest", "clear",
		"--project", "guild-cli-test",
		"QUEST-1", "--report", "commit abc"})
	if err != nil {
		t.Fatalf("fulfill: %v", err)
	}
	if !strings.Contains(stdout, "fulfilled QUEST-1") {
		t.Errorf("fulfill stdout = %q, want to contain 'fulfilled QUEST-1'", stdout)
	}
}

func TestCLI_QuestPostCascade(t *testing.T) {
	setupQuestCLI(t, "guild-cli-casc")

	// Post A.
	if _, _, err := runQuest(t, []string{"quest", "post", "-p", "guild-cli-casc", "A"}); err != nil {
		t.Fatalf("post A: %v", err)
	}
	// Post B depending on QUEST-1.
	if _, _, err := runQuest(t, []string{"quest", "post", "-p", "guild-cli-casc",
		"--depends-on", "QUEST-1", "B"}); err != nil {
		t.Fatalf("post B: %v", err)
	}
	// Fulfill A — expect cascade line for QUEST-2. Using `fulfill` as
	// primary verb (QUEST-106).
	stdout, _, err := runQuest(t, []string{"quest", "fulfill", "-p", "guild-cli-casc", "QUEST-1"})
	if err != nil {
		t.Fatalf("fulfill A: %v", err)
	}
	if !strings.Contains(stdout, "fulfilled QUEST-1") {
		t.Errorf("fulfill stdout missing fulfilled: %q", stdout)
	}
	if !strings.Contains(stdout, "QUEST-2") || !strings.Contains(stdout, "unblocked") {
		t.Errorf("fulfill stdout missing cascade: %q", stdout)
	}
}

func TestCLI_QuestAcceptForfeitCycle(t *testing.T) {
	setupQuestCLI(t, "guild-cli-accept")
	ctx := context.Background()

	if _, _, err := runQuest(t, []string{"quest", "post", "-p", "guild-cli-accept", "target"}); err != nil {
		t.Fatalf("post: %v", err)
	}
	stdout, _, err := runQuest(t, []string{"quest", "accept", "-p", "guild-cli-accept", "QUEST-1", "--owner", "alice"})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	// QUEST-44 unified CLI + MCP behind one Format. CLI now emits the
	// rich card: header line, meta row, next-step hint. Assert on the
	// semantic pieces rather than the legacy one-line form.
	for _, want := range []string{
		"accepted QUEST-1: target",
		"status=in_progress",
		"owner=alice",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("accept stdout missing %q:\n%s", want, stdout)
		}
	}

	// Verify in DB that status is in_progress.
	db, err := storage.Open(ctx, questDBPathOverride)
	if err != nil {
		t.Fatalf("open for verify: %v", err)
	}
	defer func() { _ = db.Close() }()
	status, owner := readStatusOwner(t, db, "guild-cli-accept", "QUEST-1")
	if status != "in_progress" {
		t.Errorf("status = %q, want in_progress", status)
	}
	if owner != "alice" {
		t.Errorf("owner = %q, want alice", owner)
	}

	// Forfeit → back to next.
	stdout, _, err = runQuest(t, []string{"quest", "forfeit", "-p", "guild-cli-accept", "QUEST-1", "--note", "blocker"})
	if err != nil {
		t.Fatalf("forfeit: %v", err)
	}
	if !strings.Contains(stdout, "forfeited QUEST-1") {
		t.Errorf("forfeit stdout = %q", stdout)
	}
	status, owner = readStatusOwner(t, db, "guild-cli-accept", "QUEST-1")
	if status != "next" {
		t.Errorf("post-forfeit status = %q, want next", status)
	}
	if owner != "" {
		t.Errorf("post-forfeit owner = %q, want empty", owner)
	}
}

func TestCLI_QuestNoEmoji(t *testing.T) {
	setupQuestCLI(t, "guild-cli-emoji")
	stdout, _, err := runQuest(t, []string{"quest", "post",
		"--project", "guild-cli-emoji", "--no-emoji", "silent"})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if strings.Contains(stdout, "➕") {
		t.Errorf("emoji leaked with --no-emoji: %q", stdout)
	}
	if !strings.Contains(stdout, "posted QUEST-1: silent") {
		t.Errorf("stdout = %q, want 'posted QUEST-1: silent'", stdout)
	}
}

func TestCLI_QuestEpic(t *testing.T) {
	setupQuestCLI(t, "guild-cli-epic")
	for i := 0; i < 2; i++ {
		if _, _, err := runQuest(t, []string{"quest", "post", "-p", "guild-cli-epic", "t"}); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}
	stdout, _, err := runQuest(t, []string{"quest", "epic", "-p", "guild-cli-epic",
		"epic-v1", "QUEST-1", "QUEST-2"})
	if err != nil {
		t.Fatalf("epic: %v", err)
	}
	// QUEST-45 unified output — was per-line "QUEST-N → epic:epic-v1",
	// now "applied 'epic-v1' to N quest(s)" with an indented applied: list.
	for _, want := range []string{"epic-v1", "QUEST-1", "QUEST-2"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("epic stdout missing %q:\n%s", want, stdout)
		}
	}
}

// TestCLI_QuestClearDeprecationStderr verifies that invoking `quest clear`
// (cobra alias for quest fulfill) writes the deprecation notice to stderr and
// not to stdout, and that --json mode suppresses it entirely. QUEST-138.
func TestCLI_QuestClearDeprecationStderr(t *testing.T) {
	setupQuestCLI(t, "guild-cli-depr")
	const proj = "guild-cli-depr"

	// Post a quest to fulfill.
	if _, _, err := runQuest(t, []string{"quest", "post", "-p", proj, "depr-test"}); err != nil {
		t.Fatalf("post: %v", err)
	}

	t.Run("human_mode_notice_on_stderr_not_stdout", func(t *testing.T) {
		stdout, stderr, err := runQuest(t, []string{"quest", "clear",
			"-p", proj, "QUEST-1", "--report", "done"})
		if err != nil {
			t.Fatalf("clear: %v", err)
		}
		// Success line must be on stdout.
		if !strings.Contains(stdout, "fulfilled QUEST-1") {
			t.Errorf("stdout missing success line; got: %q", stdout)
		}
		// Deprecation notice must be on stderr.
		if !strings.Contains(stderr, "deprecated") {
			t.Errorf("stderr missing deprecation notice; got: %q", stderr)
		}
		if !strings.Contains(stderr, "quest_fulfill") {
			t.Errorf("stderr missing 'quest_fulfill' pointer; got: %q", stderr)
		}
		// Deprecation notice must NOT appear on stdout.
		if strings.Contains(stdout, "deprecated") {
			t.Errorf("deprecation notice leaked to stdout; got: %q", stdout)
		}
	})

	// Post another quest for the json-mode test.
	if _, _, err := runQuest(t, []string{"quest", "post", "-p", proj, "depr-json-test"}); err != nil {
		t.Fatalf("post 2: %v", err)
	}

	t.Run("json_mode_no_deprecation_notice", func(t *testing.T) {
		stdout, stderr, err := runQuest(t, []string{"quest", "clear",
			"-p", proj, "QUEST-2", "--report", "done", "--json"})
		if err != nil {
			t.Fatalf("clear --json: %v", err)
		}
		// stdout must be valid JSON without any deprecation content.
		if strings.Contains(stdout, "deprecated") {
			t.Errorf("deprecation notice leaked into JSON stdout; got: %q", stdout)
		}
		// stderr must be empty in --json mode.
		if strings.Contains(stderr, "deprecated") {
			t.Errorf("deprecation notice leaked into stderr in --json mode; got: %q", stderr)
		}
		// Sanity: stdout is parseable JSON.
		var out map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &out); err != nil {
			t.Errorf("stdout is not valid JSON: %v\nstdout: %q", err, stdout)
		}
	})

	// Verify quest fulfill (primary verb) does NOT emit the notice.
	if _, _, err := runQuest(t, []string{"quest", "post", "-p", proj, "fulfill-clean"}); err != nil {
		t.Fatalf("post 3: %v", err)
	}
	t.Run("fulfill_no_deprecation_notice", func(t *testing.T) {
		_, stderr, err := runQuest(t, []string{"quest", "fulfill",
			"-p", proj, "QUEST-3", "--report", "done"})
		if err != nil {
			t.Fatalf("fulfill: %v", err)
		}
		if strings.Contains(stderr, "deprecated") {
			t.Errorf("deprecation notice fired on quest fulfill (primary verb); stderr: %q", stderr)
		}
	})
}

// readStatusOwner reads the (status, claimed_by) pair for (pid, tid).
func readStatusOwner(t *testing.T, db *sql.DB, pid, tid string) (status, owner string) {
	t.Helper()
	var s, o sql.NullString
	err := db.QueryRowContext(context.Background(),
		`SELECT status, claimed_by FROM task_status
		 WHERE project_id = ? AND task_id = ?`,
		pid, tid,
	).Scan(&s, &o)
	if err != nil {
		t.Fatalf("read status/owner: %v", err)
	}
	return s.String, o.String
}

func TestCLI_QuestClear_BriefHint(t *testing.T) {
	const proj = "guild-cli-brief-hint"

	t.Run("no_brief_shows_hint", func(t *testing.T) {
		setupQuestCLI(t, proj+"-a")
		pid := proj + "-a"

		// Post + clear with no brief → hint must appear.
		if _, _, err := runQuest(t, []string{"quest", "post", "-p", pid, "task-a"}); err != nil {
			t.Fatalf("post: %v", err)
		}
		stdout, _, err := runQuest(t, []string{"quest", "clear", "-p", pid, "QUEST-1", "--report", "done"})
		if err != nil {
			t.Fatalf("clear: %v", err)
		}
		if !strings.Contains(stdout, "no quest_brief yet") {
			t.Errorf("expected hint in stdout; got:\n%s", stdout)
		}
	})

	t.Run("after_brief_no_hint", func(t *testing.T) {
		setupQuestCLI(t, proj+"-b")
		pid := proj + "-b"
		ctx := context.Background()

		// Post a quest.
		if _, _, err := runQuest(t, []string{"quest", "post", "-p", pid, "task-b"}); err != nil {
			t.Fatalf("post: %v", err)
		}

		// Write a brief directly via the quest package (same DB the CLI uses).
		db, err := storage.Open(ctx, questDBPathOverride)
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		if err := quest.Brief(ctx, db, pid, "done X, next Y", "agent"); err != nil {
			_ = db.Close()
			t.Fatalf("brief: %v", err)
		}
		_ = db.Close()

		// Clear → hint must NOT appear.
		stdout, _, err := runQuest(t, []string{"quest", "clear", "-p", pid, "QUEST-1", "--report", "done"})
		if err != nil {
			t.Fatalf("clear: %v", err)
		}
		if strings.Contains(stdout, "no quest_brief yet") {
			t.Errorf("unexpected hint after brief was written; got:\n%s", stdout)
		}
	})
}

// TestCLI_QuestSearch_RRFArmAboveCoverageFloor is the QUEST-259 regression
// gate: when a *quest.QuestEmbedDeps is wired into command.Deps.Embed on
// the CLI surface and quest corpus coverage is at or above the 0.90 floor,
// the quest search handler must return arm=rrf.
//
// This mirrors TestQuestSearch_RRFArmAboveCoverageFloor in
// internal/quest/search_cmd_test.go but exercises it via the CLI
// command.Deps path (same handler code that `guild quest search` uses).
// The *quest.QuestEmbedDeps is injected directly rather than via
// wireQuestEmbedDeps because CLI tests run without -tags=withembed
// (no bundled BGE assets); wireQuestEmbedDeps is tested separately
// for the nil-safety contract (it must return nil when the embedder
// is not available rather than panic).
func TestCLI_QuestSearch_RRFArmAboveCoverageFloor(t *testing.T) {
	const proj = "guild-cli-rrf"
	setupQuestCLI(t, proj)
	ctx := context.Background()

	db, err := storage.Open(ctx, questDBPathOverride)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Post enough quests to have a non-trivial corpus.
	const totalQuests = 10
	for i := 1; i <= totalQuests; i++ {
		if _, postErr := quest.Post(ctx, db, proj, quest.PostParams{
			Subject: fmt.Sprintf("implement feature variant %d for search pipeline", i),
		}); postErr != nil {
			t.Fatalf("post quest %d: %v", i, postErr)
		}
	}

	// tasks_fts_rows is populated by trigger on task_notes insert.
	rows, err := db.QueryContext(ctx, `SELECT id FROM tasks_fts_rows`)
	if err != nil {
		t.Fatalf("query tasks_fts_rows: %v", err)
	}
	var bridgeIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan bridge id: %v", err)
		}
		bridgeIDs = append(bridgeIDs, id)
	}
	_ = rows.Close()
	if len(bridgeIDs) == 0 {
		t.Skip("no tasks_fts_rows after insert; trigger may not have fired in this schema version")
	}

	// Seed vectors for all bridge rows using DeterministicEmbedder.
	embedder := embed.NewDeterministicEmbedder()
	const modelID = "bge-small-en-v1.5-int8-cls"
	for _, id := range bridgeIDs {
		vec, embedErr := embedder.Embed(ctx, "subject: feature variant search pipeline")
		if embedErr != nil {
			t.Fatalf("embed row %d: %v", id, embedErr)
		}
		entry := embed.PendingEntry{ID: id, Summary: "feature variant"}
		if insertErr := embed.InsertVectorRow(ctx, db, embed.QuestCorpus{}, entry, vec, modelID); insertErr != nil {
			t.Fatalf("insert vector row %d: %v", id, insertErr)
		}
	}

	// Set coverage meta so questVectorTopK reads a passing ratio.
	n := int64(len(bridgeIDs))
	upsertCLIQuestMeta(t, db, "quest.embedder_state", "enabled")
	upsertCLIQuestMeta(t, db, "quest.embedder_model_id", modelID)
	upsertCLIQuestMeta(t, db, "quest.vector_coverage_num", fmt.Sprintf("%d", n))
	upsertCLIQuestMeta(t, db, "quest.vector_coverage_den", fmt.Sprintf("%d", n))

	// Build QuestEmbedDeps with an Index loaded from the DB.
	idx := embed.NewIndex(embed.QuestCorpus{}, modelID)
	if _, loadErr := idx.LoadFromDB(ctx, db); loadErr != nil {
		t.Fatalf("LoadFromDB: %v", loadErr)
	}
	embedDeps := &quest.QuestEmbedDeps{
		Embedder: embedder,
		Index:    idx,
		ModelID:  modelID,
	}

	// Construct CLI-surface Deps with Embed wired, mirroring the shape
	// buildCLICommandDeps produces when wireQuestEmbedDeps returns non-nil.
	// openQuestDB reads questDBPathOverride at call time, so the handler
	// sees the same seeded DB the fixture just populated.
	deps := command.Deps{
		OpenDB: openQuestDB,
		ResolveProj: func(_ context.Context, _ string) (string, error) {
			return proj, nil
		},
		Embed: embedDeps,
	}

	out, err := quest.SearchCommand.Handler(ctx, deps, quest.SearchInput{
		Query:   "feature search pipeline",
		Limit:   10,
		Project: proj,
	})
	if err != nil {
		t.Fatalf("search handler: %v", err)
	}
	if out.Arm != "rrf" {
		t.Errorf("arm: got %q want %q (coverage=%.2f)", out.Arm, "rrf", out.Coverage)
	}
	if len(out.Results) == 0 {
		t.Error("expected results with rrf arm, got none")
	}
}

// upsertCLIQuestMeta inserts or updates a row in the meta table for CLI tests.
func upsertCLIQuestMeta(t *testing.T, db *sql.DB, key, value string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		t.Fatalf("upsertCLIQuestMeta %s=%s: %v", key, value, err)
	}
}
