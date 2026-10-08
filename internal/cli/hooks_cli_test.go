package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/hooks"
	"github.com/mathomhaus/guild/internal/storage"
)

// Tests for the hook payload CLI verbs: `guild quest brief --auto` and
// `guild lore appraise --inject`. Hermetic: quest.db is redirected via
// questDBPathOverride and lore.db via the loreDBPath seam, both into
// t.TempDir(); the live ~/.guild is never touched.

// setupHookCLI wires temp quest + lore DBs sharing one project id and
// registers cleanups that also reset the hook-mode flags (pflag values
// persist across rootCmd.Execute calls in-process).
func setupHookCLI(t *testing.T, projectName string) {
	t.Helper()
	t.Setenv("GUILD_NO_USAGE_LOG", "1")
	t.Setenv("GUILD_NO_EMOJI", "1")
	setupQuestCLI(t, projectName)
	overrideLoreDB(t, projectName)
	resetHookFlags(t)
}

// overrideLoreDB points the lore.db seam at a temp file seeded with the
// project row, so oath/appraise reads run against test data only.
func overrideLoreDB(t *testing.T, projectName string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "lore.db")
	orig := loreDBPath
	loreDBPath = func() (string, error) { return dbPath, nil }
	t.Cleanup(func() { loreDBPath = orig })

	ctx := context.Background()
	db, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open lore db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := storage.Migrate(ctx, db, "lore"); err != nil {
		t.Fatalf("migrate lore db: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO projects (id, path) VALUES (?, ?)`,
		projectName, t.TempDir(),
	); err != nil {
		t.Fatalf("seed lore project: %v", err)
	}
}

// seedLoreEntry inserts one lore entry through the loreDBPath seam.
func seedLoreEntry(t *testing.T, projectName, kind, title, summary string) {
	t.Helper()
	ctx := context.Background()
	path, err := loreDBPath()
	if err != nil {
		t.Fatalf("loreDBPath: %v", err)
	}
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open lore db: %v", err)
	}
	defer func() { _ = db.Close() }()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, created_at, updated_at)
		 VALUES (?, 't', ?, ?, ?, 'current', ?, ?)`,
		projectName, kind, title, summary, now, now,
	); err != nil {
		t.Fatalf("seed lore entry: %v", err)
	}
}

// resetHookFlags restores the hook-mode flag defaults after the test.
// pflag values survive across Execute calls, so a test that sets
// --auto or --inject would otherwise bleed into later tests.
func resetHookFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if cmd, _, err := questCmd.Find([]string{"brief"}); err == nil {
			_ = cmd.Flags().Set("auto", "false")
			_ = cmd.Flags().Set("capture", "false")
		}
		if cmd, _, err := loreCmd.Find([]string{"appraise"}); err == nil {
			_ = cmd.Flags().Set("inject", "false")
			_ = cmd.Flags().Set("query", "")
			_ = cmd.Flags().Set("from-stdin-json", "false")
		}
	})
}

// ---------------------------------------------------------------------------
// quest brief --auto
// ---------------------------------------------------------------------------

func TestQuestBriefAuto_PrintsBoundedPayload(t *testing.T) {
	const proj = "hook-brief-auto"
	setupHookCLI(t, proj)
	seedLoreEntry(t, proj, "principle", "Verify in source before asserting", "cite the file:line")

	if _, _, err := runQuest(t, []string{"quest", "post", "-p", proj, "--priority", "P1", "ship the payload verbs"}); err != nil {
		t.Fatalf("post: %v", err)
	}
	if _, _, err := runQuest(t, []string{"quest", "brief", "-p", proj, "prior session handoff text"}); err != nil {
		t.Fatalf("brief: %v", err)
	}

	stdout, _, err := runQuest(t, []string{"quest", "brief", "-p", proj, "--auto"})
	if err != nil {
		t.Fatalf("brief --auto: %v", err)
	}
	for _, want := range []string{
		"## guild session priming [" + hooks.PayloadVersion + "]",
		"- Verify in source before asserting: cite the file:line",
		"prior session handoff text",
		"- QUEST-1 [P1] ship the payload verbs",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("payload missing %q:\n%s", want, stdout)
		}
	}
	if len(stdout) > hooks.MaxSessionBytes {
		t.Errorf("stdout %d bytes exceeds budget %d", len(stdout), hooks.MaxSessionBytes)
	}
}

func TestQuestBriefAutoCapture_StoresBriefAndConfirms(t *testing.T) {
	const proj = "hook-brief-capture"
	setupHookCLI(t, proj)

	stdout, _, err := runQuest(t, []string{"quest", "brief", "-p", proj, "--auto", "--capture"})
	if err != nil {
		t.Fatalf("brief --auto --capture: %v", err)
	}
	trimmed := strings.TrimRight(stdout, "\n")
	if !strings.Contains(trimmed, "auto brief captured") {
		t.Errorf("missing confirmation line, got %q", stdout)
	}
	if strings.Contains(trimmed, "\n") {
		t.Errorf("confirmation must be a single line, got %q", stdout)
	}

	// The stored note must be the rendered payload on the quest_brief
	// storage path (task_notes, "[brief] " prefix).
	ctx := context.Background()
	db, err := storage.Open(ctx, questDBPathOverride)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	defer func() { _ = db.Close() }()
	var note string
	if err := db.QueryRowContext(ctx,
		`SELECT note FROM task_notes WHERE project_id = ? AND task_id = '__PROJECT__'
		 ORDER BY id DESC LIMIT 1`, proj,
	).Scan(&note); err != nil {
		t.Fatalf("read stored brief: %v", err)
	}
	if !strings.HasPrefix(note, "[brief] ## guild session priming ["+hooks.PayloadVersion+"]") {
		t.Errorf("stored note is not the rendered payload: %q", note[:min(len(note), 80)])
	}
}

func TestQuestBriefAuto_ErrorLeavesStdoutEmpty(t *testing.T) {
	setupHookCLI(t, "hook-brief-err")

	stdout, _, err := runQuest(t, []string{"quest", "brief", "-p", "no-such-project", "--auto"})
	if err == nil {
		t.Fatal("want error for unknown project, got nil")
	}
	if stdout != "" {
		t.Errorf("stdout must be empty on error (hook contract), got %q", stdout)
	}
}

func TestQuestBriefAuto_FlagValidation(t *testing.T) {
	const proj = "hook-brief-flags"
	setupHookCLI(t, proj)

	cases := []struct {
		name string
		args []string
	}{
		{"capture without auto", []string{"quest", "brief", "-p", proj, "--capture", "some text"}},
		{"auto with text", []string{"quest", "brief", "-p", proj, "--auto", "some text"}},
		{"no text no auto", []string{"quest", "brief", "-p", proj}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetHookFlags(t)
			stdout, _, err := runQuest(t, tc.args)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if stdout != "" {
				t.Errorf("stdout must be empty on error, got %q", stdout)
			}
		})
	}
}

func TestQuestBriefPlainText_StillWorks(t *testing.T) {
	const proj = "hook-brief-plain"
	setupHookCLI(t, proj)

	stdout, _, err := runQuest(t, []string{"quest", "brief", "-p", proj, "classic handoff"})
	if err != nil {
		t.Fatalf("brief: %v", err)
	}
	if !strings.Contains(stdout, "briefed for next session") {
		t.Errorf("missing classic confirmation, got %q", stdout)
	}
}

// ---------------------------------------------------------------------------
// lore appraise --inject
// ---------------------------------------------------------------------------

func TestLoreAppraiseInject_QueryFlag(t *testing.T) {
	const proj = "hook-appraise-query"
	setupHookCLI(t, proj)
	seedLoreEntry(t, proj, "decision", "splice refusal design", "out-of-order splices land via per-slot epochs")

	stdout, _, err := runQuest(t, []string{"lore", "appraise", "--inject", "--query", "splice refusal"})
	if err != nil {
		t.Fatalf("appraise --inject --query: %v", err)
	}
	for _, want := range []string{
		"## relevant lore [" + hooks.PayloadVersion + "]",
		"LORE-",
		"(decision, 0d) splice refusal design: out-of-order splices land via per-slot epochs",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("payload missing %q:\n%s", want, stdout)
		}
	}
	if len(stdout) > hooks.MaxAppraiseBytes {
		t.Errorf("stdout %d bytes exceeds budget %d", len(stdout), hooks.MaxAppraiseBytes)
	}
}

func TestLoreAppraiseInject_FromStdinJSON(t *testing.T) {
	const proj = "hook-appraise-stdin"
	setupHookCLI(t, proj)
	seedLoreEntry(t, proj, "research", "splice refusal design", "epoch summary")

	// Claude Code and Codex pipe this identical UserPromptSubmit
	// envelope shape; the query must come from its .prompt field.
	envelope := `{"session_id":"abc","cwd":"/work","hook_event_name":"UserPromptSubmit","prompt":"splice refusal"}`
	rootCmd.SetIn(strings.NewReader(envelope))
	t.Cleanup(func() { rootCmd.SetIn(nil) })

	stdout, _, err := runQuest(t, []string{"lore", "appraise", "--inject", "--from-stdin-json"})
	if err != nil {
		t.Fatalf("appraise --inject --from-stdin-json: %v", err)
	}
	if !strings.Contains(stdout, "splice refusal design") {
		t.Errorf("expected results for the envelope's .prompt query, got:\n%s", stdout)
	}
	if len(stdout) > hooks.MaxAppraiseBytes {
		t.Errorf("stdout %d bytes exceeds budget %d", len(stdout), hooks.MaxAppraiseBytes)
	}
}

func TestLoreAppraiseInject_ZeroMatchesEmptyStdoutExitZero(t *testing.T) {
	const proj = "hook-appraise-zero"
	setupHookCLI(t, proj)

	stdout, _, err := runQuest(t, []string{"lore", "appraise", "--inject", "--query", "zzqqxyzzy"})
	if err != nil {
		t.Fatalf("zero matches must exit 0, got %v", err)
	}
	if stdout != "" {
		t.Errorf("zero matches must print empty stdout, got %q", stdout)
	}
}

func TestLoreAppraiseInject_ErrorsLeaveStdoutEmpty(t *testing.T) {
	const proj = "hook-appraise-err"
	setupHookCLI(t, proj)

	cases := []struct {
		name  string
		args  []string
		stdin string
	}{
		{"bad stdin envelope", []string{"lore", "appraise", "--inject", "--from-stdin-json"}, `{not json`},
		{"empty prompt", []string{"lore", "appraise", "--inject", "--from-stdin-json"}, `{"prompt":""}`},
		{"no query source", []string{"lore", "appraise", "--inject"}, ""},
		{"query and stdin together", []string{"lore", "appraise", "--inject", "--query", "x", "--from-stdin-json"}, `{"prompt":"y"}`},
		{"hook flags without inject", []string{"lore", "appraise", "--query", "x", "positional"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetHookFlags(t)
			rootCmd.SetIn(strings.NewReader(tc.stdin))
			t.Cleanup(func() { rootCmd.SetIn(nil) })
			stdout, _, err := runQuest(t, tc.args)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if stdout != "" {
				t.Errorf("stdout must be empty on error (hook contract), got %q", stdout)
			}
		})
	}
}

func TestLoreAppraise_NoArgsStillRequiresQuery(t *testing.T) {
	const proj = "hook-appraise-legacy"
	setupHookCLI(t, proj)

	_, _, err := runQuest(t, []string{"lore", "appraise"})
	if err == nil {
		t.Fatal("plain appraise without QUERY must keep erroring")
	}
}
