package mcp

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/project"
	"github.com/mathomhaus/guild/internal/quest"
	"github.com/mathomhaus/guild/internal/session"
)

// registerCWDAsProject registers projDir in both quest and lore DBs so
// the auto-bootstrap resolver can look it up, and stubs the cwd resolver
// to return projDir as the git toplevel. Returns the project id.
//
// Shared helper for the auto-bootstrap test matrix to avoid per-test boilerplate.
func registerCWDAsProject(t *testing.T, projID, projDir string) {
	t.Helper()
	ctx := context.Background()

	qdb, err := openQuestDB(ctx)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	defer func() { _ = qdb.Close() }()
	if err := project.Register(ctx, qdb, projID, projDir, "TASKS.md"); err != nil {
		t.Fatalf("register project in quest db: %v", err)
	}

	ldb, err := openLoreDB(ctx)
	if err != nil {
		t.Fatalf("open lore db: %v", err)
	}
	defer func() { _ = ldb.Close() }()
	if err := project.Register(ctx, ldb, projID, projDir, "TASKS.md"); err != nil {
		t.Fatalf("register project in lore db: %v", err)
	}

	// Stub the cwd resolver to return projDir as the git toplevel.
	withStubbedResolver(t, projDir, nil)
}

// TestAutoBootstrap_FreshSession_RegisteredCWD_AutoBootstraps verifies the
// primary happy path: a fresh session (no prior guild_session_start) calls a
// non-bootstrap tool. The cwd is registered in the projects table. The tool
// must succeed AND the response must start with the narration line.
func TestAutoBootstrap_FreshSession_RegisteredCWD_AutoBootstraps(t *testing.T) {
	isolateHome(t)
	const (
		pid     = "auto-proj"
		projDir = "/fake/workspaces/auto-proj"
	)
	registerCWDAsProject(t, pid, projDir)
	ctx := context.Background()

	// Post a quest directly (bypassing MCP) so quest_list has something
	// to return — we want to confirm the tool ran, not just that it errored.
	qdb, err := openQuestDB(ctx)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	_, postErr := quest.Post(ctx, qdb, pid, quest.PostParams{Subject: "auto-bootstrap smoke"})
	_ = qdb.Close()
	if postErr != nil {
		t.Fatalf("quest.Post: %v", postErr)
	}

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	// Deliberately do NOT call guild_session_start — that's the point.
	res, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "quest_list",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("quest_list: %v", err)
	}
	if res.IsError {
		t.Fatalf("quest_list IsError=true: %s", textOf(res.Content))
	}
	body := textOf(res.Content)

	// The narration line must be the first line.
	firstLine := strings.SplitN(body, "\n", 2)[0]
	wantNarration := `[auto-bootstrapped to project "auto-proj" from cwd]`
	if !strings.Contains(firstLine, wantNarration) {
		t.Errorf("first line missing narration; want %q, got %q", wantNarration, firstLine)
	}
	// Tool output should follow: quest list shows something meaningful.
	if !strings.Contains(body, "auto-bootstrap smoke") {
		t.Errorf("tool output missing quest subject; body:\n%s", body)
	}
}

// TestAutoBootstrap_ReconnectMidSession_AutoBootstraps simulates the MCP
// subprocess restart scenario: a session was active (guild_session_start
// was called), the server "restarted" (session file deleted), and then a
// non-bootstrap tool is called. Auto-bootstrap must re-fire from cwd.
func TestAutoBootstrap_ReconnectMidSession_AutoBootstraps(t *testing.T) {
	isolateHome(t)
	const (
		pid     = "reconnect-proj"
		projDir = "/fake/workspaces/reconnect-proj"
	)
	registerCWDAsProject(t, pid, projDir)
	ctx := context.Background()

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	// Step 1: bootstrap normally.
	if _, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "guild_session_start",
		Arguments: map[string]any{"project": pid},
	}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// Step 2: simulate session file deletion (MCP restart). Delete the
	// per-PID session file so ResolveForMCP finds no active project.
	sPath, pathErr := currentSessionPath(t)
	if pathErr != nil {
		t.Fatalf("session path: %v", pathErr)
	}
	if err := removeIfExists(sPath); err != nil {
		t.Fatalf("remove session file: %v", err)
	}

	// Step 3: call a non-bootstrap tool without re-bootstrapping.
	res, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "quest_list",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("quest_list after reconnect: %v", err)
	}
	if res.IsError {
		t.Fatalf("quest_list IsError=true after reconnect: %s", textOf(res.Content))
	}
	body := textOf(res.Content)
	// Narration must appear because the session was wiped.
	wantNarration := `[auto-bootstrapped to project "reconnect-proj" from cwd]`
	if !strings.Contains(body, wantNarration) {
		t.Errorf("reconnect: narration missing; want %q in:\n%s", wantNarration, body)
	}
}

// TestAutoBootstrap_CWDNotInGit_CleanError verifies that when the cwd is
// not inside a git repository, auto-bootstrap does NOT fire and the tool
// returns the original "no active project set" error unchanged.
func TestAutoBootstrap_CWDNotInGit_CleanError(t *testing.T) {
	isolateHome(t)
	// Stub resolver to report "not in a git repo".
	withStubbedResolver(t, "/tmp/not-a-git-repo", project.ErrNotInGitRepo)

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	res, err := client.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "quest_list",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("quest_list: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true without bootstrap in non-git cwd; got body:\n%s",
			textOf(res.Content))
	}
	body := textOf(res.Content)
	// Must carry the standard error sentinel.
	if !strings.Contains(body, "[error]") {
		t.Errorf("error body missing [error] prefix: %q", body)
	}
	// Must NOT have fired auto-bootstrap narration.
	if strings.Contains(body, "[auto-bootstrapped") {
		t.Errorf("auto-bootstrap narration appeared in non-git cwd error: %q", body)
	}
}

// TestAutoBootstrap_CWDInUnregisteredRepo_CleanError verifies that when the
// cwd is inside a git repo but not registered in the projects table, the
// original "no active project set" error is returned (not a confusing
// inference-failure message).
func TestAutoBootstrap_CWDInUnregisteredRepo_CleanError(t *testing.T) {
	isolateHome(t)
	// Stub resolver: git succeeds but the path won't be in the projects table.
	withStubbedResolver(t, "/unregistered/git/repo", nil)

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	res, err := client.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      "quest_list",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("quest_list: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true for unregistered cwd; got body:\n%s",
			textOf(res.Content))
	}
	body := textOf(res.Content)
	if !strings.Contains(body, "[error]") {
		t.Errorf("error body missing [error] prefix: %q", body)
	}
	if strings.Contains(body, "[auto-bootstrapped") {
		t.Errorf("auto-bootstrap narration appeared for unregistered repo: %q", body)
	}
}

// TestAutoBootstrap_ExplicitGuildSessionStartStillReturnsFullSnapshot is a
// regression guard: guild_session_start with an explicit project must still
// return the full briefing/oath/echoes/top-bounty snapshot, not just the
// narration line from implicit auto-bootstrap.
func TestAutoBootstrap_ExplicitGuildSessionStartStillReturnsFullSnapshot(t *testing.T) {
	// Use isolateProject so the project is registered and guild_session_start
	// can load bounties/oath.
	pid := isolateProject(t)
	ctx := context.Background()

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	res, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "guild_session_start",
		Arguments: map[string]any{"project": pid},
	})
	if err != nil {
		t.Fatalf("guild_session_start: %v", err)
	}
	if res.IsError {
		t.Fatalf("guild_session_start IsError=true: %s", textOf(res.Content))
	}
	body := textOf(res.Content)

	// The snapshot must carry all five structural markers.
	for _, want := range []string{
		"active project:",
		"📋 last briefing",
		"⚔️", // oath section (any oath marker)
		"👻",  // fading echoes
		"🎯",  // bounties
		"⚡",  // parallelism
	} {
		if !strings.Contains(body, want) {
			t.Errorf("guild_session_start snapshot missing %q; body:\n%s", want, body)
		}
	}
	// Must NOT look like the terse implicit narration from auto-bootstrap.
	if strings.Contains(body, "[auto-bootstrapped") {
		t.Errorf("guild_session_start emitted auto-bootstrap narration — should return full snapshot: %q", body)
	}
}

// TestAutoBootstrap_NoNarrationOnRepeatCall verifies that when the project is
// already bootstrapped (session file has active_project set), subsequent tool
// calls do NOT emit the auto-bootstrap narration line. Narration only fires on
// the transition from empty → resolved.
func TestAutoBootstrap_NoNarrationOnRepeatCall(t *testing.T) {
	isolateHome(t)
	const (
		pid     = "repeat-proj"
		projDir = "/fake/workspaces/repeat-proj"
	)
	registerCWDAsProject(t, pid, projDir)
	ctx := context.Background()

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	// First call: should auto-bootstrap (no session).
	res1, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "quest_list",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if res1.IsError {
		t.Fatalf("first call IsError=true: %s", textOf(res1.Content))
	}
	body1 := textOf(res1.Content)
	if !strings.Contains(body1, "[auto-bootstrapped") {
		t.Fatalf("first call: expected narration in body; got:\n%s", body1)
	}

	// Second call: project is now active in session file — no narration.
	res2, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "quest_list",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if res2.IsError {
		t.Fatalf("second call IsError=true: %s", textOf(res2.Content))
	}
	body2 := textOf(res2.Content)
	if strings.Contains(body2, "[auto-bootstrapped") {
		t.Errorf("second call: narration should NOT appear after session is established; got:\n%s", body2)
	}
}

// --- helpers for auto_bootstrap_test.go ---

// currentSessionPath returns the canonical session file path for the current
// process (via session.Path) so tests can delete it to simulate MCP restarts.
func currentSessionPath(t *testing.T) (string, error) {
	t.Helper()
	return session.Path()
}

// removeIfExists deletes path, ignoring os.ErrNotExist.
func removeIfExists(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
