package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// AgentEnvelope is the stable JSON wire shape every registry-generated
// CLI verb emits (one object per invocation, on stdout) when agent mode
// is active. The schema is intentionally minimal and append-only:
//
//	success: {"ok":true,"command":"<wire name>","output":{...},"hint":"..."}
//	failure: {"ok":false,"command":"<wire name>","error":"...","hint":"..."}
//
// Output carries the verb's typed output struct exactly as the --json
// flag would marshal it; Command is the MCP wire name (e.g. "quest_post")
// so agents can correlate CLI calls with the equivalent MCP tools. Hint
// is optional advisory text: recovery guidance on errors, non-fatal
// warnings (dedup hits, bloat notices) on success.
type AgentEnvelope struct {
	OK      bool   `json:"ok"`
	Command string `json:"command"`
	Output  any    `json:"output,omitempty"`
	Error   string `json:"error,omitempty"`
	Hint    string `json:"hint,omitempty"`
}

// agentHarnessMarkers lists environment variables that identify a
// coding-agent harness shelling out to the guild CLI. Detection is
// mechanism-based: any harness that exports a marker into the shells it
// spawns is recognized; everything else can opt in via GUILD_AGENT=1 or
// the --agent flag. Current set (verified 2026-06):
//
//   - CLAUDECODE: Claude Code sets CLAUDECODE=1 in its shell tool.
//   - CODEX_SANDBOX: Codex CLI sets this in sandboxed shells (value is
//     the sandbox backend, e.g. "seatbelt" on macOS).
//   - CODEX_SANDBOX_NETWORK_DISABLED: Codex CLI sets =1 when the
//     sandbox has network access disabled; covers configurations where
//     CODEX_SANDBOX itself is absent.
//   - CURSOR_AGENT: the Cursor agent CLI sets CURSOR_AGENT=1 in the
//     shells it spawns.
var agentHarnessMarkers = []string{
	"CLAUDECODE",
	"CODEX_SANDBOX",
	"CODEX_SANDBOX_NETWORK_DISABLED",
	"CURSOR_AGENT",
}

// DetectAgentEnv reports whether the environment indicates the CLI is
// being driven by a coding agent. getenv is injectable for tests; pass
// nil for os.Getenv.
//
// GUILD_AGENT is checked first and only for the literal toggles "1",
// "true", "0", "false" (case-insensitive): the same variable doubles as
// the agent IDENTITY override for quest journal/orders (see
// internal/quest.resolveAgent), so identity values like "alice" must
// neither enable nor disable agent mode. "0"/"false" force agent mode
// off even when a harness marker is present.
func DetectAgentEnv(getenv func(string) string) bool {
	if getenv == nil {
		getenv = os.Getenv
	}
	switch v := strings.TrimSpace(getenv("GUILD_AGENT")); {
	case v == "1" || strings.EqualFold(v, "true"):
		return true
	case v == "0" || strings.EqualFold(v, "false"):
		return false
	}
	for _, marker := range agentHarnessMarkers {
		v := strings.TrimSpace(getenv(marker))
		if v != "" && v != "0" && !strings.EqualFold(v, "false") {
			return true
		}
	}
	return false
}

// agentModeActive resolves agent mode for one cobra invocation. An
// explicitly-set --agent bool flag wins in both directions (--agent
// forces on, --agent=false forces off); otherwise the environment
// decides via DetectAgentEnv.
//
// The type check matters: a few verbs (quest journal/orders/campfire/
// summon) predate agent mode with a local STRING --agent flag carrying
// the agent identity. pflag's merge keeps the local flag, shadowing the
// root persistent bool, so for those verbs --agent retains its
// historical meaning and agent mode is reachable via env only.
func agentModeActive(cmd *cobra.Command) bool {
	if cmd != nil {
		if f := cmd.Flags().Lookup("agent"); f != nil && f.Value.Type() == "bool" && f.Changed {
			on, _ := cmd.Flags().GetBool("agent")
			return on
		}
	}
	return DetectAgentEnv(os.Getenv)
}

// hintedError attaches an agent-facing recovery hint to an error.
// Error() passes through unchanged so human-mode output is byte
// identical; the hint only surfaces in the agent-mode JSON envelope.
type hintedError struct {
	err  error
	hint string
}

func (e *hintedError) Error() string     { return e.err.Error() }
func (e *hintedError) Unwrap() error     { return e.err }
func (e *hintedError) AgentHint() string { return e.hint }

// WithHint wraps err with an agent-facing recovery hint. Returns err
// unchanged when err is nil or hint is blank. Callers anywhere in the
// stack can attach hints; AgentHintFromError recovers the outermost one.
func WithHint(err error, hint string) error {
	if err == nil || strings.TrimSpace(hint) == "" {
		return err
	}
	return &hintedError{err: err, hint: hint}
}

// AgentHintFromError walks err's wrap chain for an attached agent hint.
// Returns "" when none exists.
func AgentHintFromError(err error) string {
	var h interface{ AgentHint() string }
	if errors.As(err, &h) {
		return h.AgentHint()
	}
	return ""
}

// AgentReportedError marks a handler error that has already been
// emitted as a structured {ok:false,...} envelope on stdout. The
// process entry point (cmd/guild/main.go) detects it via IsAgentReported
// to skip the duplicate human-readable stderr line while preserving the
// non-zero exit code.
type AgentReportedError struct{ Err error }

func (e *AgentReportedError) Error() string { return e.Err.Error() }
func (e *AgentReportedError) Unwrap() error { return e.Err }

// IsAgentReported reports whether err (or anything it wraps) is an
// AgentReportedError.
func IsAgentReported(err error) bool {
	var target *AgentReportedError
	return errors.As(err, &target)
}

// emitAgentSuccess writes the success envelope for out to stdout.
// CLIWarnings (the non-fatal stderr notices of human mode) become the
// envelope's hint so agents see dedup/bloat advisories without a
// second channel; the ASCII sink keeps the JSON emoji-free.
func (c *Command[I, O]) emitAgentSuccess(cc *cobra.Command, out O) error {
	env := AgentEnvelope{OK: true, Command: c.Name, Output: out}
	if c.CLIWarnings != nil {
		if w := c.CLIWarnings(CLISink{NoEmoji: true}, out); w != "" {
			env.Hint = strings.TrimSpace(w)
		}
	}
	buf, err := json.Marshal(env)
	if err != nil {
		return c.emitAgentError(cc, fmt.Errorf("marshal agent envelope: %w", err))
	}
	_, _ = fmt.Fprintln(cc.OutOrStdout(), string(buf))
	return nil
}

// emitAgentError writes the failure envelope for herr to stdout and
// returns an AgentReportedError so the exit code stays non-zero.
// cobra's own "Error: ..." stderr print is silenced: the envelope
// already carries the failure and agent mode promises exactly one JSON
// object per invocation.
func (c *Command[I, O]) emitAgentError(cc *cobra.Command, herr error) error {
	env := AgentEnvelope{
		OK:      false,
		Command: c.Name,
		Error:   herr.Error(),
		Hint:    AgentHintFromError(herr),
	}
	buf, merr := json.Marshal(env)
	if merr != nil {
		// All envelope fields are plain strings here, so this is close
		// to unreachable; fall back to the human error path.
		return herr
	}
	_, _ = fmt.Fprintln(cc.OutOrStdout(), string(buf))
	cc.SilenceErrors = true
	return &AgentReportedError{Err: herr}
}
