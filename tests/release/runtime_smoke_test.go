package release_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestReleaseBinaryRuntime executes an opt-in native candidate. The semantic
// assertions prove model/runtime activation, not retrieval quality or recall.
func TestReleaseBinaryRuntime(t *testing.T) {
	binary := os.Getenv("GUILD_RELEASE_BINARY")
	if binary == "" {
		t.Skip("set GUILD_RELEASE_BINARY to exercise a native candidate executable")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	semantic := os.Getenv("GUILD_RELEASE_EXPECT_SEMANTIC") == "1"
	modes := []bool{true}
	if runtime.GOOS == "windows" {
		// Unsupported Unix-daemon routing must gracefully fall back by
		// default; users should not need an undocumented environment flag.
		modes = []bool{false, true}
	}
	for _, direct := range modes {
		t.Run(fmt.Sprintf("direct=%v", direct), func(t *testing.T) {
			runtimeSmoke(t, binary, semantic, direct)
		})
	}
}

func runtimeSmoke(t *testing.T, binary string, semantic, direct bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	home := filepath.Join(t.TempDir(), "home")
	project := filepath.Join(home, "runtime-smoke")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"HOME=" + home, "USERPROFILE=" + home,
		"APPDATA=" + filepath.Join(home, "AppData", "Roaming"),
		"LOCALAPPDATA=" + filepath.Join(home, "AppData", "Local"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"GUILD_EMBEDDED_CACHE=" + filepath.Join(home, "runtime-cache"),
		"GUILD_NO_UPDATE_CHECK=1", "GUILD_NO_USAGE_LOG=1",
	}
	if direct {
		env = append(env, "GUILD_NO_DAEMON=1")
	}
	// Only executable lookup and Windows system paths survive from the
	// host. No credentials, Guild overrides, or host configuration paths.
	for _, key := range []string{"PATH", "PATHEXT", "SystemRoot", "WINDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		env = append(env, key+"="+home)
	}
	safe := func(text string) string {
		return strings.ReplaceAll(strings.ReplaceAll(text, home, "<HOME>"), binary, "<BINARY>")
	}
	cli := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // explicit opt-in candidate with fixed synthetic fixture arguments
		cmd.Dir, cmd.Env = project, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("candidate command %v failed: %s\n%s", args, safe(err.Error()), safe(string(out)))
		}
		return string(out)
	}
	cli("init", "--yes")
	const title = "Password rotation after compromise"
	cli("lore", "inscribe", title, "--kind", "observation", "--topic", "runtime-smoke",
		"--summary", "Replace compromised database passwords and invalidate stolen login tokens to restore account security.", "--project", "runtime-smoke")
	cli("lore", "inscribe", "Garden watering schedule", "--kind", "observation", "--topic", "runtime-smoke",
		"--summary", "Water tomato seedlings, prune fruit trees, and check soil moisture.", "--project", "runtime-smoke")
	requireRuntimeRow(t, cli("lore", "appraise", title, "--limit", "1", "--project", "runtime-smoke"), "LORE-1", title)
	const subject = "Replace service passwords"
	cli("quest", "post", subject, "--priority", "P1", "--acceptance", "Encrypted vault updated", "--project", "runtime-smoke")
	requireRuntimeRow(t, cli("quest", "search", subject, "--limit", "1", "--project", "runtime-smoke"), "QUEST-1", subject)
	for _, name := range []string{"lore.db", "quest.db"} {
		if _, err := os.Stat(filepath.Join(home, ".guild", name)); err != nil {
			t.Fatalf("persistent SQLite store %s was not created: %v", name, err)
		}
	}

	stderr := &runtimeSmokeOutput{}
	cmd := exec.CommandContext(ctx, binary, "mcp", "serve") //nolint:gosec // same explicit opt-in candidate, literal command
	cmd.Dir, cmd.Env, cmd.Stderr = project, env, stderr
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "release-runtime-fixture", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdkmcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("native MCP startup failed: %s\n%s", safe(err.Error()), safe(stderr.String()))
	}
	defer func() { _ = session.Close() }()
	call := func(name string, args map[string]any) string {
		t.Helper()
		result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("MCP %s failed: %s\n%s", name, safe(err.Error()), safe(stderr.String()))
		}
		var text strings.Builder
		for _, part := range result.Content {
			if content, ok := part.(*sdkmcp.TextContent); ok {
				text.WriteString(content.Text)
			}
		}
		if result.IsError {
			t.Fatalf("MCP %s returned an error:\n%s\n%s", name, safe(text.String()), safe(stderr.String()))
		}
		return text.String()
	}
	call("guild_session_start", map[string]any{"project": "runtime-smoke"})
	requireRuntimeRow(t, call("lore_appraise", map[string]any{"query": title, "limit": 1}), "LORE-1", title)
	health := call("lore_health", map[string]any{})
	if !semantic {
		if !regexp.MustCompile(`(?m)^\s*state:\s+disabled`).MatchString(health) {
			t.Fatalf("expected lexical fallback health:\n%s", safe(health))
		}
		out := call("quest_search", map[string]any{"query": subject, "limit": 1})
		requireRuntimeRow(t, out, "QUEST-1", subject)
		if !strings.Contains(out, "arm=bm25") {
			t.Fatalf("disabled semantic platform used unexpected search arm:\n%s", out)
		}
		return
	}
	if regexp.MustCompile(`(?m)^\s*state:\s+disabled`).MatchString(strings.Split(health, "quest embedder section")[0]) {
		t.Fatalf("native semantic build initialized with a disabled lore runtime:\n%s\n%s", safe(health), safe(stderr.String()))
	}
	// The initial appraise starts repair of the quest's missing vector.
	// Wait for actual fresh vectors in both corpora, not cached counters.
	deadline := time.Now().Add(30 * time.Second)
	fresh := regexp.MustCompile(`(?m)^\s*fresh:\s+(\d+)`)
	enabled := regexp.MustCompile(`(?m)^\s*state:\s+enabled`)
	for {
		parts := strings.Split(health, "quest embedder section")
		if len(parts) == 2 {
			loreFresh, questFresh := fresh.FindStringSubmatch(parts[0]), fresh.FindStringSubmatch(parts[1])
			if enabled.MatchString(parts[0]) && enabled.MatchString(parts[1]) &&
				len(loreFresh) == 2 && loreFresh[1] == "2" && len(questFresh) == 2 && questFresh[1] == "1" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("native embeddings did not become fresh:\n%s\n%s", safe(health), safe(stderr.String()))
		}
		time.Sleep(20 * time.Millisecond)
		health = call("lore_health", map[string]any{})
	}
	// These tokens occur in neither title, summary nor quest spec. A
	// result proves the native semantic path rather than an echoed query
	// or a BM25 match. This is an activation smoke, not a quality metric.
	const query = "renew authentication credentials"
	requireRuntimeRow(t, call("lore_appraise", map[string]any{"query": query, "limit": 1}), "LORE-1", title)
	out := call("quest_search", map[string]any{"query": query, "limit": 1})
	requireRuntimeRow(t, out, "QUEST-1", subject)
	if !strings.Contains(out, "arm=rrf") {
		t.Fatalf("fresh native quest vectors were not used:\n%s", out)
	}
}

// requireRuntimeRow only accepts an actual result row, never a query echo.
func requireRuntimeRow(t *testing.T, text, id, title string) {
	t.Helper()
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != id || !strings.HasPrefix(fields[1], "[") {
			continue
		}
		if end := strings.Index(line, "]"); end >= 0 {
			if strings.TrimSpace(line[end+1:]) == title {
				return // compact MCP/quest result row
			}
			if strings.TrimSpace(line[end+1:]) == "" && i+1 < len(lines) && strings.TrimSpace(lines[i+1]) == title {
				return // CLI lore result: ID row followed by its title
			}
		}
	}
	t.Fatalf("actual result row %s %q missing:\n%s", id, title, text)
}

type runtimeSmokeOutput struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (o *runtimeSmokeOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.Write(p)
}

func (o *runtimeSmokeOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.String()
}
