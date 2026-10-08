package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/session"
)

// TestNewServer_DistinctSessionIdentities_NoCrossTalk is the multi-
// session correctness gate: two server instances built concurrently
// with distinct session identities must resolve and persist distinct
// active projects. Before the SessionStore seam, both instances keyed
// session state by os.Getpid(), so the last guild_session_start to land
// silently switched the other session's active project.
func TestNewServer_DistinctSessionIdentities_NoCrossTalk(t *testing.T) {
	home := isolateHome(t)
	base := filepath.Join(home, ".guild")
	ctx := context.Background()

	// Two synthetic per-connection identities. The PIDs deliberately
	// differ from os.Getpid() so any accidental fallback to the process
	// default writes a third file and the per-identity assertions fail.
	storeA := session.Manager{BaseDir: base, PID: 111111}
	storeB := session.Manager{BaseDir: base, PID: 222222}

	// One shared bundle, as a multi-session host would wire it. The
	// explicit bundle also makes concurrent NewServer calls safe (the
	// default bundle path mutates the per-process tracker).
	shared := NewProviders()

	var (
		wg         sync.WaitGroup
		sA, sB     *sdkmcp.Server
		errA, errB error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		sA, errA = NewServer(Options{Sessions: storeA, Providers: shared})
	}()
	go func() {
		defer wg.Done()
		sB, errB = NewServer(Options{Sessions: storeB, Providers: shared})
	}()
	wg.Wait()
	if errA != nil || errB != nil {
		t.Fatalf("concurrent NewServer: errA=%v errB=%v", errA, errB)
	}

	_, clientA, cleanupA := connectInMemory(t, sA)
	defer cleanupA()
	_, clientB, cleanupB := connectInMemory(t, sB)
	defer cleanupB()

	// Bootstrap both sessions concurrently with different projects.
	bootstrap := func(client *sdkmcp.ClientSession, project string, errOut *error) {
		defer wg.Done()
		res, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
			Name:      "guild_session_start",
			Arguments: map[string]any{"project": project},
		})
		if err != nil {
			*errOut = err
			return
		}
		if res.IsError {
			*errOut = errStr(textOf(res.Content))
		}
	}
	wg.Add(2)
	go bootstrap(clientA, "proj-alpha", &errA)
	go bootstrap(clientB, "proj-beta", &errB)
	wg.Wait()
	if errA != nil || errB != nil {
		t.Fatalf("concurrent bootstrap: errA=%v errB=%v", errA, errB)
	}

	// Each identity resolves its own project; no last-writer-wins.
	gotA, err := storeA.ResolveForMCP(ctx, "", "")
	if err != nil {
		t.Fatalf("storeA resolve: %v", err)
	}
	gotB, err := storeB.ResolveForMCP(ctx, "", "")
	if err != nil {
		t.Fatalf("storeB resolve: %v", err)
	}
	if gotA != "proj-alpha" || gotB != "proj-beta" {
		t.Fatalf("cross-talk: storeA=%q (want proj-alpha), storeB=%q (want proj-beta)", gotA, gotB)
	}

	// On disk: one session file per identity, each carrying its own
	// project. A shared-identity regression would collapse both writes
	// into a single file.
	for _, tc := range []struct {
		pid  int
		want string
	}{
		{111111, "proj-alpha"},
		{222222, "proj-beta"},
	} {
		path := filepath.Join(base, "sessions", strconv.Itoa(tc.pid)+".json")
		data, err := os.ReadFile(path) //nolint:gosec // test path built from t.TempDir
		if err != nil {
			t.Fatalf("read session file %s: %v", path, err)
		}
		var blob struct {
			ActiveProject string `json:"active_project"`
		}
		if err := json.Unmarshal(data, &blob); err != nil {
			t.Fatalf("parse %s: %v; raw=%q", path, err, data)
		}
		if blob.ActiveProject != tc.want {
			t.Errorf("session file %s active_project = %q; want %q", path, blob.ActiveProject, tc.want)
		}
	}

	// A mid-session switch on B must not leak into A's identity.
	res, err := clientB.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "guild_set_project",
		Arguments: map[string]any{"project": "proj-gamma"},
	})
	if err != nil {
		t.Fatalf("guild_set_project on B: %v", err)
	}
	if res.IsError {
		t.Fatalf("guild_set_project on B IsError: %s", textOf(res.Content))
	}
	gotA, err = storeA.ResolveForMCP(ctx, "", "")
	if err != nil {
		t.Fatalf("storeA re-resolve: %v", err)
	}
	if gotA != "proj-alpha" {
		t.Errorf("B's guild_set_project leaked into A: A now resolves %q", gotA)
	}
}

// TestNewServer_SharedProviders_NoClobber asserts that building and
// using a second server against a shared provider bundle neither resets
// nor closes the bundle's state. Before the bundle existed, every
// Register call replaced the package-level embed providers, closed the
// previous hints engine, and re-armed the auto-backfill once-guard,
// which would have defeated the share-one-embedder goal.
func TestNewServer_SharedProviders_NoClobber(t *testing.T) {
	pid := isolateProject(t) // registers "testproj" in both DBs under a temp $HOME
	ctx := context.Background()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	base := filepath.Join(home, ".guild")

	shared := NewProviders()
	t.Cleanup(shared.closeHintsEngine)
	// Pin the embed provider's logger to a buffer so reconstruct events
	// are countable: each "embedder wired lazily" line marks one
	// reconstruction. Shared state means exactly one across both servers.
	var logBuf safeBuffer
	shared.embed.logger = slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	embedBefore := shared.embed
	questEmbedBefore := shared.questEmbed
	gateBefore := shared.backfill

	sA, err := NewServer(Options{
		Sessions:  session.Manager{BaseDir: base, PID: 333333},
		Providers: shared,
	})
	if err != nil {
		t.Fatalf("NewServer A: %v", err)
	}
	engineA := shared.hintsEngine
	if engineA == nil || engineA.Store == nil || engineA.Store.DB == nil {
		t.Fatal("first registration did not build the shared hints engine")
	}

	sB, err := NewServer(Options{
		Sessions:  session.Manager{BaseDir: base, PID: 444444},
		Providers: shared,
	})
	if err != nil {
		t.Fatalf("NewServer B: %v", err)
	}

	// The second build must not have replaced or re-armed anything.
	if shared.embed != embedBefore {
		t.Error("second registration replaced the shared embed provider")
	}
	if shared.questEmbed != questEmbedBefore {
		t.Error("second registration replaced the shared quest embed provider")
	}
	if shared.backfill != gateBefore {
		t.Error("second registration replaced the shared backfill gate")
	}
	if shared.hintsEngine != engineA {
		t.Error("second registration replaced the shared hints engine")
	}
	if err := engineA.Store.DB.PingContext(ctx); err != nil {
		t.Errorf("second registration closed the shared hints engine DB: %v", err)
	}

	// Drive one lore tool through each server. The first resolve
	// reconstructs once (fresh DB, embedder_state seeded 'disabled');
	// the second server's resolve must hit the SAME provider's hot-path
	// cache. Two reconstruct lines would mean per-connection
	// registration rebuilt provider state.
	_, clientA, cleanupA := connectInMemory(t, sA)
	defer cleanupA()
	_, clientB, cleanupB := connectInMemory(t, sB)
	defer cleanupB()

	appraise := func(client *sdkmcp.ClientSession, label string) {
		t.Helper()
		res, err := client.CallTool(ctx, &sdkmcp.CallToolParams{
			Name:      "lore_appraise",
			Arguments: map[string]any{"query": "shared-bundle-smoke", "project": pid},
		})
		if err != nil {
			t.Fatalf("lore_appraise via %s: %v", label, err)
		}
		if res.IsError {
			t.Fatalf("lore_appraise via %s IsError: %s", label, textOf(res.Content))
		}
	}
	appraise(clientA, "server A")
	appraise(clientB, "server B")

	// Embed provider state survived both servers: exactly one
	// reconstruction, then a cache hit.
	wired := strings.Count(logBuf.String(), "embedder wired lazily")
	if wired != 1 {
		t.Errorf("expected exactly 1 embed reconstruct across both servers; got %d; logs:\n%s",
			wired, logBuf.String())
	}
}

// errStr is a minimal error wrapper for tool-result error bodies so the
// concurrent bootstrap helper can report them through an error slot.
type errStr string

func (e errStr) Error() string { return string(e) }
