package mcp

import (
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/project"
	"github.com/mathomhaus/guild/internal/session"
)

func TestQuestSearch_MCPProjectIsolation(t *testing.T) {
	home := isolateHome(t)
	ctx := context.Background()
	const alpha, beta = "search-alpha", "search-beta"
	// Real migrated SQLite stores; only registration bypasses tools because
	// project registration is an install operation rather than an MCP tool.
	for _, open := range []func(context.Context) (*sql.DB, error){openQuestDB, openLoreDB} {
		db, err := open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, pid := range []string{alpha, beta} {
			if err := project.Register(ctx, db, pid, filepath.Join(home, pid), "TASKS.md"); err != nil {
				_ = db.Close()
				t.Fatal(err)
			}
		}
		_ = db.Close()
	}
	store := session.Manager{BaseDir: filepath.Join(home, ".guild"), PID: 333333}
	server, err := NewServer(Options{Sessions: store})
	if err != nil {
		t.Fatal(err)
	}
	_, client, cleanup := connectInMemory(t, server)
	defer cleanup()
	call := func(name string, args map[string]any) string {
		t.Helper()
		result, err := client.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s protocol error: %v", name, err)
		}
		body := textOf(result.Content)
		if result.IsError {
			t.Fatalf("%s domain error: %s", name, body)
		}
		return body
	}
	call("guild_session_start", map[string]any{"project": alpha})
	postRE := regexp.MustCompile(`posted (QUEST-\d+):`)
	post := func(pid, subject string) string {
		t.Helper()
		body := call("quest_post", map[string]any{"project": pid, "subject": subject})
		match := postRE.FindStringSubmatch(body)
		if len(match) != 2 {
			t.Fatalf("post response lacks quest identifier: %s", body)
		}
		return match[1]
	}
	alphaID := post(alpha, "ambercircuit task")
	betaID := post(beta, "violetpipeline task")
	if alphaID != betaID {
		t.Fatalf("fixture requires project-local ID collision: %s != %s", alphaID, betaID)
	}
	// Parse only actual result rows. The response header echoes the query,
	// which must never satisfy a positive subject or relevance assertion.
	resultRE := regexp.MustCompile(`(?m)^ {2}(QUEST-\d+) \[[^\]]+\] (.+)$`)
	search := func(pid, query, wantedSubject string) {
		t.Helper()
		args := map[string]any{"query": query, "limit": 10}
		if pid != "" {
			args["project"] = pid
		}
		body := call("quest_search", args)
		rows := resultRE.FindAllStringSubmatch(body, -1)
		if wantedSubject == "" {
			if len(rows) != 0 || !strings.Contains(body, "results=0") {
				t.Fatalf("foreign spec contributed to results: %s", body)
			}
			return
		}
		if len(rows) != 1 || rows[0][1] != alphaID || rows[0][2] != wantedSubject {
			t.Fatalf("search results do not match scoped quest: %s", body)
		}
	}
	search(alpha, "ambercircuit", "ambercircuit task")
	search(alpha, "violetpipeline", "")
	search(beta, "violetpipeline", "violetpipeline task")
	search(beta, "ambercircuit", "")
	search("", "ambercircuit", "ambercircuit task")
	search("", "violetpipeline", "")

	call("quest_update", map[string]any{
		"project": beta, "quest_id": betaID,
		"subject": "indigorelay revised task", "replace_acceptance": []string{"silverspec"},
	})
	search(beta, "indigorelay", "indigorelay revised task")
	search(beta, "silverspec", "indigorelay revised task")
	search(alpha, "indigorelay", "")
	search(alpha, "silverspec", "")
	search("", "ambercircuit", "ambercircuit task")
	search("", "indigorelay", "")
	active, err := store.ResolveForMCP(ctx, "", "")
	if err != nil || active != alpha {
		t.Fatalf("explicit post/update/search override changed active project: active=%q err=%v", active, err)
	}
}
