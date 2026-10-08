// Package claudecode implements the Claude Code harness adapter for
// guild lifecycle hooks. It renders the abstract base config
// (internal/hooks.Config) into the per-project Claude Code settings
// file, .claude/settings.json under the project root, wrapping the
// events in Claude Code's top-level "hooks" key:
//
//	{
//	  "hooks": {
//	    "SessionStart": [
//	      {"matcher": "startup|resume|clear|compact",
//	       "hooks": [{"type": "command", "command": "guild quest brief --auto"}]}
//	    ],
//	    "PreCompact": [
//	      {"matcher": "auto|manual",
//	       "hooks": [{"type": "command", "command": "guild quest brief --auto --capture"}]}
//	    ],
//	    "UserPromptSubmit": [
//	      {"hooks": [{"type": "command", "command": "guild lore appraise --inject --from-stdin-json"}]}
//	    ]
//	  }
//	}
//
// Design notes:
//
//   - The target is .claude/settings.json, the project-shared settings
//     file. NOT .claude/settings.local.json: that file is reserved for
//     the user's personal overrides and is never managed by tooling.
//   - The SessionStart matcher includes "compact" on purpose. Claude
//     Code treats PreCompact stdout as observability output (it never
//     reaches model context), so the post-compaction re-prime rides on
//     SessionStart, which the harness re-fires with source "compact"
//     after compaction completes. One hook entry covers all four
//     cold-start sources.
//   - PreCompact is retained for the write-side capture only: it
//     persists a structured brief before the window collapses; the
//     SessionStart re-fire reads it back.
//   - UserPromptSubmit carries no matcher: Claude Code does not support
//     one on that event. The user's prompt arrives as the "prompt"
//     field of the JSON envelope the harness pipes to the hook's
//     stdin (there is no prompt environment variable), which the base
//     config's appraise command reads via --from-stdin-json. That is
//     also why Substitute is the identity: the settings file needs no
//     placeholder expansion.
//   - Hook ownership is decided by command string
//     (hooks.GroupIsGuildOwned), never by matcher inspection: matcher
//     values are harness dispatch, evaluated as regular expressions
//     against an event-specific field, and synthetic label values
//     would simply never fire. Foreign and mixed groups, plus every
//     unrelated top-level settings field, are preserved verbatim.
package claudecode

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mathomhaus/guild/internal/guildpath"
	"github.com/mathomhaus/guild/internal/hooks"
	"github.com/mathomhaus/guild/internal/hooks/adapters"
	"github.com/mathomhaus/guild/internal/install"
)

// adapterName is the registry key. It must equal what the `guild
// hooks` CLI derives from the install registry's display name
// ("Claude Code"), see adapterNameForClient in internal/cli/hooks.
const adapterName = "claude-code"

// clientName is the display name of the Claude Code entry in
// internal/install.Clients, the single source of truth for harness
// detection (CLI probe "claude" on PATH, config probe ~/.claude.json).
const clientName = "Claude Code"

// hooksKey is the top-level settings key Claude Code reads hook events
// from.
const hooksKey = "hooks"

// settingsDir and settingsFile locate the managed settings file
// relative to the project root.
const (
	settingsDir  = ".claude"
	settingsFile = "settings.json"
)

// settingsPerm is the mode for a settings file this adapter creates.
// Consistent with the rest of guild's hook machinery; an existing file
// keeps whatever mode the atomic rename grants the replacement.
const settingsPerm os.FileMode = 0o600

// noMatcherEvents are Claude Code hook events with no matcher support.
// Pairing a matcher with one of these is a configuration error: the
// harness would reject or ignore it.
var noMatcherEvents = map[string]bool{
	"UserPromptSubmit": true,
	"Stop":             true,
	"TaskCreated":      true,
}

// closedMatcherValues maps events whose matcher is regex-evaluated
// against a documented closed vocabulary: SessionStart matches the
// session source, PreCompact and PostCompact match the compaction
// trigger. A matcher that matches none of the values never fires.
// Events absent from both tables (tool-use events match the open set
// of tool names; future events are unknown) pass through unvalidated.
var closedMatcherValues = map[string][]string{
	"SessionStart": {"startup", "resume", "clear", "compact"},
	"PreCompact":   {"auto", "manual"},
	"PostCompact":  {"auto", "manual"},
}

// Adapter is the Claude Code hooks adapter. It self-registers under
// "claude-code" on import.
type Adapter struct{}

func init() { adapters.Register(Adapter{}) }

// Compile-time guard: Adapter must expose Validate as a method so the
// hooks-scan validator interface picks it up. Without this, the
// dead-matcher check stays a package func and scan silently shows no
// warnings.
var _ interface {
	Validate(adapters.Config) ([]string, error)
} = Adapter{}

// Name implements adapters.Adapter.
func (Adapter) Name() string { return adapterName }

// Detect implements adapters.Adapter by reusing the Claude Code entry
// of the install client registry, the same source of truth `guild
// init` and `guild mcp install` report from: the harness is present
// when the "claude" CLI is on PATH or ~/.claude.json exists.
func (Adapter) Detect() (bool, error) {
	for _, c := range install.Clients {
		if c.Name == clientName {
			return c.Detected(), nil
		}
	}
	return false, fmt.Errorf("claude-code: client %q missing from the install registry", clientName)
}

// SettingsPath implements adapters.Adapter: .claude/settings.json
// under the project root of the current working directory.
func (Adapter) SettingsPath() (string, error) {
	root, err := projectRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, settingsDir, settingsFile), nil
}

// Substitute implements adapters.Adapter. Claude Code needs no
// placeholder expansion: the prompt reaches the appraise hook through
// the stdin JSON envelope, not through the command string.
func (Adapter) Substitute(cmd string) string { return cmd }

// Install implements adapters.Adapter. First-time setup and resync are
// the same operation: an ownership-aware merge into the settings file.
func (a Adapter) Install(base adapters.Config) error { return a.Sync(base) }

// Sync implements adapters.Adapter: validate the config against Claude
// Code's per-event matcher contract, then merge the guild-owned groups
// into .claude/settings.json, preserving foreign content, writing
// atomically and only when something changed.
func (a Adapter) Sync(base adapters.Config) error {
	if _, err := Validate(base); err != nil {
		return fmt.Errorf("claude-code: refusing to write hooks: %w", err)
	}
	path, err := a.SettingsPath()
	if err != nil {
		return err
	}
	// G304: path derives from the resolved project root plus
	// compile-time constant components; no user-controlled input.
	raw, err := os.ReadFile(path) //nolint:gosec // trusted path; see note above
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("claude-code: read %s: %w", path, err)
	}
	desired := hooks.ApplySubstitution(base, a.Substitute)
	merged, changed, err := hooks.MergeSettingsDoc(raw, desired, hooksKey)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := guildpath.EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	return hooks.WriteFileAtomic(path, merged, settingsPerm)
}

// Scan implements adapters.Adapter: flatten every hook in the settings
// file, guild-owned and foreign alike, with ownership decided by the
// command-string rule. A missing file yields an empty list.
func (a Adapter) Scan() ([]adapters.Hook, error) {
	path, err := a.SettingsPath()
	if err != nil {
		return nil, err
	}
	// G304: see Sync.
	raw, err := os.ReadFile(path) //nolint:gosec // trusted path; see note above
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("claude-code: read %s: %w", path, err)
	}
	return hooks.ScanSettingsDoc(raw, hooksKey)
}

// Validate checks cfg against Claude Code's per-event matcher
// contract before anything is written.
//
// Hard violations return an error and block the write:
//   - a matcher on an event that does not support one (UserPromptSubmit,
//     Stop, TaskCreated);
//   - a matcher that is not a valid regular expression.
//
// Dead matchers, valid regular expressions that match none of the
// documented values for a closed-vocabulary event (SessionStart
// sources, PreCompact/PostCompact triggers), come back as warnings:
// they are written through, since the vocabulary may grow, but the
// hook group would never fire on a current harness. Events unknown to
// the tables pass through unvalidated; Claude Code grows events faster
// than this adapter does.
func Validate(cfg adapters.Config) (warnings []string, err error) {
	events := make([]string, 0, len(cfg))
	for ev := range cfg {
		events = append(events, ev)
	}
	sort.Strings(events)
	for _, ev := range events {
		for _, g := range cfg[ev] {
			if g.Matcher == "" {
				continue
			}
			if noMatcherEvents[ev] {
				return warnings, fmt.Errorf("event %s does not support a matcher (got %q); remove it", ev, g.Matcher)
			}
			re, rerr := regexp.Compile(g.Matcher)
			if rerr != nil {
				return warnings, fmt.Errorf("event %s: matcher %q is not a valid regular expression: %w", ev, g.Matcher, rerr)
			}
			vocab, closed := closedMatcherValues[ev]
			if !closed {
				continue
			}
			fires := false
			for _, v := range vocab {
				if re.MatchString(v) {
					fires = true
					break
				}
			}
			if !fires {
				warnings = append(warnings, fmt.Sprintf(
					"event %s: matcher %q matches none of the documented values (%s); this hook group will never fire",
					ev, g.Matcher, strings.Join(vocab, ", ")))
			}
		}
	}
	return warnings, nil
}

// Validate as a method lets the adapter satisfy the read-only validator
// interface that `guild hooks scan` type-asserts, so dead-matcher
// warnings surface in the inventory without scan learning this harness
// by name. It delegates to the package-level check.
func (Adapter) Validate(cfg adapters.Config) (warnings []string, err error) {
	return Validate(cfg)
}

// projectRoot resolves the directory whose .claude/settings.json
// Claude Code reads for sessions started here: the git work-tree
// toplevel of the current directory (correct from any subdirectory,
// and per-worktree for linked worktrees), or the current directory
// itself when outside a repository, since guild projects are not
// required to be git repositories.
func projectRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("claude-code: resolve working directory: %w", err)
	}
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = cwd
	// Stderr is intentionally not captured: git's noise confuses more
	// than it helps, and the fallback below is well-defined either way.
	out, err := cmd.Output()
	if err != nil {
		// Not inside a git repository, or git is not installed. Either
		// way the per-project root is the working directory itself.
		return cwd, nil
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return cwd, nil
	}
	return top, nil
}
