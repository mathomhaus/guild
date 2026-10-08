package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/daemon"
)

// TestMain scrubs the agent-mode detection env vars before the package
// runs. The package's tests assert human-readable CLI output; when the
// test process itself runs under a coding-agent harness (a developer
// asking an agent to run `make check`), the harness markers would flip
// every registry verb into agent mode and fail those assertions. Tests
// that exercise detection re-set markers via t.Setenv.
//
// It also pins the daemon-routing probe to "not running": a developer's
// LIVE daemon (discovered via the real ~/.guild/daemon.json) must never
// receive traffic from this package's verb invocations: routed verbs
// would execute against the daemon's real databases instead of the
// test-scoped overrides. Routing tests install their own probe via
// swapRouteProbe.
func TestMain(m *testing.M) {
	for _, k := range []string{
		"GUILD_AGENT",
		"CLAUDECODE",
		"CODEX_SANDBOX",
		"CODEX_SANDBOX_NETWORK_DISABLED",
		"CURSOR_AGENT",
	} {
		_ = os.Unsetenv(k)
	}
	routeProbeFn = func(string, time.Duration) (daemon.ProbeResult, daemon.Discovery, error) {
		return daemon.NotRunning, daemon.Discovery{}, nil
	}
	os.Exit(m.Run())
}

// resetAgentModeState undoes the per-process state agent-mode runs leave
// on the shared cobra tree: the persistent --agent flag (pflag keeps
// Value and Changed across Execute calls) and any SilenceErrors set by
// the agent error path. Registered via t.Cleanup so human-output tests
// running later see a pristine tree.
func resetAgentModeState(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if f := rootCmd.PersistentFlags().Lookup("agent"); f != nil {
			_ = f.Value.Set("false")
			f.Changed = false
		}
		resetSilenceErrors(rootCmd)
	})
}

func resetSilenceErrors(c *cobra.Command) {
	c.SilenceErrors = false
	for _, sub := range c.Commands() {
		resetSilenceErrors(sub)
	}
}

// decodeEnvelope parses stdout as exactly one single-line JSON envelope
// and asserts the top-level schema keys are from the locked set.
func decodeEnvelope(t *testing.T, stdout string) map[string]any {
	t.Helper()
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		t.Fatal("stdout empty, want one JSON envelope")
	}
	if strings.Contains(trimmed, "\n") {
		t.Fatalf("stdout has multiple lines, want a single JSON object: %q", stdout)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %q", err, stdout)
	}
	allowed := map[string]bool{"ok": true, "command": true, "output": true, "error": true, "hint": true}
	for k := range env {
		if !allowed[k] {
			t.Errorf("envelope has unexpected top-level key %q (schema is append-only: ok/command/output/error/hint)", k)
		}
	}
	if _, ok := env["ok"]; !ok {
		t.Error("envelope missing required key \"ok\"")
	}
	if _, ok := env["command"]; !ok {
		t.Error("envelope missing required key \"command\"")
	}
	return env
}

func TestCLI_AgentMode_QuestPostSuccess(t *testing.T) {
	setupQuestCLI(t, "agent-post")
	resetAgentModeState(t)

	stdout, stderr, err := runQuest(t, []string{"quest", "post",
		"-p", "agent-post", "--agent", "structured hello"})
	if err != nil {
		t.Fatalf("post --agent: %v (stderr=%q)", err, stderr)
	}
	env := decodeEnvelope(t, stdout)
	if env["ok"] != true {
		t.Errorf("ok = %v, want true", env["ok"])
	}
	if env["command"] != "quest_post" {
		t.Errorf("command = %v, want quest_post", env["command"])
	}
	out, ok := env["output"].(map[string]any)
	if !ok {
		t.Fatalf("output is %T, want object", env["output"])
	}
	quest, ok := out["quest"].(map[string]any)
	if !ok {
		t.Fatalf("output.quest is %T, want object", out["quest"])
	}
	if quest["id"] != "QUEST-1" {
		t.Errorf("output.quest.id = %v, want QUEST-1", quest["id"])
	}
	if quest["subject"] != "structured hello" {
		t.Errorf("output.quest.subject = %v, want the posted subject", quest["subject"])
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty in agent mode", stderr)
	}
}

func TestCLI_AgentMode_LoreInscribeSuccess(t *testing.T) {
	_, _ = cliSetup(t, "alpha")
	resetAgentModeState(t)

	stdout, stderr, err := execCmd(t,
		"lore", "inscribe", "agent mode envelope test",
		"--project", "alpha",
		"--kind", "decision",
		"--summary", "a summary",
		"--topic", "test",
		"--agent",
	)
	if err != nil {
		t.Fatalf("inscribe --agent: %v (stderr=%q)", err, stderr.String())
	}
	env := decodeEnvelope(t, stdout.String())
	if env["ok"] != true {
		t.Errorf("ok = %v, want true", env["ok"])
	}
	if env["command"] != "lore_inscribe" {
		t.Errorf("command = %v, want lore_inscribe", env["command"])
	}
	out, ok := env["output"].(map[string]any)
	if !ok {
		t.Fatalf("output is %T, want object", env["output"])
	}
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("output.result is %T, want object", out["result"])
	}
	entry, ok := result["Entry"].(map[string]any)
	if !ok {
		t.Fatalf("output.result.Entry is %T, want object", result["Entry"])
	}
	if entry["ID"] != float64(1) {
		t.Errorf("output.result.Entry.ID = %v, want 1", entry["ID"])
	}
}

// TestCLI_AgentMode_LoreInscribeWarningsBecomeHint verifies that the
// non-fatal stderr warnings of human mode (dedup hits) surface as the
// envelope's hint field instead, keeping stderr silent.
func TestCLI_AgentMode_LoreInscribeWarningsBecomeHint(t *testing.T) {
	_, _ = cliSetup(t, "alpha", "beta")
	resetAgentModeState(t)

	if _, stderr, err := execCmd(t,
		"lore", "inscribe", "duplicate title for hint test",
		"--project", "alpha", "--kind", "decision",
		"--summary", "first copy", "--topic", "test",
	); err != nil {
		t.Fatalf("first inscribe: %v (stderr=%q)", err, stderr.String())
	}

	stdout, stderr, err := execCmd(t,
		"lore", "inscribe", "duplicate title for hint test",
		"--project", "beta", "--kind", "decision",
		"--summary", "second copy", "--topic", "test",
		"--agent",
	)
	if err != nil {
		t.Fatalf("second inscribe --agent: %v (stderr=%q)", err, stderr.String())
	}
	env := decodeEnvelope(t, stdout.String())
	hint, _ := env["hint"].(string)
	if !strings.Contains(hint, "similar entries found") {
		t.Errorf("hint = %q, want the dedup warning text", hint)
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want empty in agent mode (warnings go to the hint field)", stderr.String())
	}
}

func TestCLI_AgentMode_QuestListViaEnvDetection(t *testing.T) {
	setupQuestCLI(t, "agent-list")
	resetAgentModeState(t)

	if _, _, err := runQuest(t, []string{"quest", "post", "-p", "agent-list", "listed quest"}); err != nil {
		t.Fatalf("seed post: %v", err)
	}

	t.Setenv("GUILD_AGENT", "1")
	stdout, _, err := runQuest(t, []string{"quest", "list", "-p", "agent-list"})
	if err != nil {
		t.Fatalf("list with GUILD_AGENT=1: %v", err)
	}
	env := decodeEnvelope(t, stdout)
	if env["command"] != "quest_list" {
		t.Errorf("command = %v, want quest_list", env["command"])
	}
	out, ok := env["output"].(map[string]any)
	if !ok {
		t.Fatalf("output is %T, want object", env["output"])
	}
	quests, ok := out["quests"].([]any)
	if !ok || len(quests) != 1 {
		t.Errorf("output.quests = %v, want one entry", out["quests"])
	}
}

func TestCLI_AgentMode_HarnessMarkerDetection(t *testing.T) {
	setupQuestCLI(t, "agent-marker")
	resetAgentModeState(t)

	if _, _, err := runQuest(t, []string{"quest", "post", "-p", "agent-marker", "marker quest"}); err != nil {
		t.Fatalf("seed post: %v", err)
	}

	t.Setenv("CLAUDECODE", "1")
	stdout, _, err := runQuest(t, []string{"quest", "list", "-p", "agent-marker"})
	if err != nil {
		t.Fatalf("list under harness marker: %v", err)
	}
	decodeEnvelope(t, stdout)

	// Explicit --agent=false overrides detection: human output returns.
	stdout, _, err = runQuest(t, []string{"quest", "list", "-p", "agent-marker", "--agent=false"})
	if err != nil {
		t.Fatalf("list --agent=false: %v", err)
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Errorf("--agent=false still emitted JSON: %q", stdout)
	}
	if !strings.Contains(stdout, "marker quest") {
		t.Errorf("human output missing quest subject: %q", stdout)
	}
}

func TestCLI_AgentMode_ErrorEnvelope(t *testing.T) {
	setupQuestCLI(t, "agent-err")
	resetAgentModeState(t)

	stdout, stderr, err := runQuest(t, []string{"quest", "accept",
		"-p", "agent-err", "QUEST-999", "--agent"})
	if err == nil {
		t.Fatal("accept of missing quest: want error, got nil")
	}
	if !command.IsAgentReported(err) {
		t.Errorf("returned error is not AgentReportedError: %v", err)
	}
	env := decodeEnvelope(t, stdout)
	if env["ok"] != false {
		t.Errorf("ok = %v, want false", env["ok"])
	}
	if env["command"] != "quest_accept" {
		t.Errorf("command = %v, want quest_accept", env["command"])
	}
	errMsg, _ := env["error"].(string)
	if !strings.Contains(errMsg, "QUEST-999") {
		t.Errorf("error = %q, want it to name the missing quest", errMsg)
	}
	if strings.Contains(stderr, "Error:") {
		t.Errorf("cobra error narration leaked to stderr in agent mode: %q", stderr)
	}
}

func TestCLI_AgentMode_ErrorHint(t *testing.T) {
	setupQuestCLI(t, "agent-hint")
	resetAgentModeState(t)

	stdout, _, err := runQuest(t, []string{"quest", "post",
		"-p", "never-registered", "--agent", "doomed"})
	if err == nil {
		t.Fatal("post to unregistered project: want error, got nil")
	}
	env := decodeEnvelope(t, stdout)
	if env["ok"] != false {
		t.Errorf("ok = %v, want false", env["ok"])
	}
	hint, _ := env["hint"].(string)
	if !strings.Contains(hint, "guild init") {
		t.Errorf("hint = %q, want recovery guidance mentioning 'guild init'", hint)
	}
}

// TestCLI_AgentMode_IdentityFlagShadowing locks the back-compat contract
// for verbs that predate agent mode with a local string --agent flag
// (quest journal/orders/campfire/summon): --agent keeps carrying the
// agent identity there, and without env markers the output stays human.
func TestCLI_AgentMode_IdentityFlagShadowing(t *testing.T) {
	setupQuestCLI(t, "agent-shadow")
	resetAgentModeState(t)

	if _, _, err := runQuest(t, []string{"quest", "post", "-p", "agent-shadow", "shadow quest"}); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	stdout, _, err := runQuest(t, []string{"quest", "journal",
		"-p", "agent-shadow", "--agent", "alice", "QUEST-1", "a note"})
	if err != nil {
		t.Fatalf("journal --agent alice: %v", err)
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Errorf("journal --agent <identity> must stay human output, got JSON: %q", stdout)
	}
	if !strings.Contains(stdout, "QUEST-1") {
		t.Errorf("journal output missing quest id: %q", stdout)
	}
}

// TestCLI_AgentMode_HumanPathUntouched pins that without the flag and
// without env markers the human rendering is produced, not JSON.
func TestCLI_AgentMode_HumanPathUntouched(t *testing.T) {
	setupQuestCLI(t, "agent-human")
	resetAgentModeState(t)

	stdout, _, err := runQuest(t, []string{"quest", "post", "-p", "agent-human", "plain quest"})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Errorf("human mode emitted JSON: %q", stdout)
	}
	if !strings.Contains(stdout, "posted QUEST-1: plain quest") {
		t.Errorf("human output changed: %q", stdout)
	}
}
