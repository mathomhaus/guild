// Package codex implements the guild hooks adapter for the Codex CLI.
//
// Codex reads per-project hook configuration from <repo>/.codex/hooks.json
// (verified working on Codex v0.128.0); this adapter renders the
// abstract base config (internal/hooks.Config) into that file under the
// top-level "hooks" key, the same document shape the shared
// merge/scan helpers operate on.
//
// Codex's hook contract differs from the base config's vocabulary in
// three load-bearing ways, all handled by Render:
//
//   - SessionStart has no "compact" source: Codex has no compaction
//     lifecycle, so the default startup|resume|clear|compact matcher
//     narrows to startup|resume|clear here. Post-compact re-priming
//     does not transfer to Codex.
//   - PreCompact does not exist as a Codex event; PreCompact groups
//     are not rendered.
//   - Commands carry an optional statusMessage shown in the Codex UI
//     while the hook runs; Render populates it per event.
//
// Codex documents a [features] codex_hooks = true gate in
// ~/.codex/config.toml, but v0.128.0 fired hooks without it. Install
// therefore probes by test fire (`codex exec`) after writing the
// settings file and prints a diagnostic suggesting the flag when no
// hook signal is observed. The adapter never edits ~/.codex/config.toml
// itself; enabling the gate stays an explicit user action.
package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mathomhaus/guild/internal/guildpath"
	"github.com/mathomhaus/guild/internal/hooks"
	"github.com/mathomhaus/guild/internal/hooks/adapters"
	"github.com/mathomhaus/guild/internal/install"
	"github.com/mathomhaus/guild/internal/project"
)

const (
	// settingsDirName/settingsFileName locate the per-repo settings
	// file this adapter manages: <repo>/.codex/hooks.json.
	settingsDirName  = ".codex"
	settingsFileName = "hooks.json"

	// hooksKey is the top-level settings key the hook events live
	// under, matching the layout the shared merge/scan helpers expect.
	hooksKey = "hooks"

	// settingsPerm matches the owner-only mode the hooks package uses
	// for every settings file it writes.
	settingsPerm os.FileMode = 0o600

	// clientNamePrefix identifies the Codex row in internal/install's
	// supported-client registry, the single source of truth for
	// harness detection ("Codex (OpenAI)" today).
	clientNamePrefix = "Codex"

	// hookSignal is the debug line the Codex CLI emits when a
	// SessionStart hook fires; its presence in test-fire output proves
	// the hooks file is being read.
	hookSignal = "hook: SessionStart"

	// probeTimeout bounds the install-time test fire. `codex exec`
	// runs a full model turn; the hook debug line appears at session
	// start, so output gathered up to the deadline is still checked
	// for the signal even when the process is cut off.
	probeTimeout = 90 * time.Second
)

// errNoCodexCLI marks a probe skipped because the codex binary is not
// on PATH (Codex can be detected via ~/.codex/config.toml alone).
var errNoCodexCLI = errors.New("codex CLI not on PATH")

// eventPolicy describes how one Codex hook event constrains the
// abstract base config: whether a matcher is legal at all, which
// source values it may dispatch on, and the statusMessage rendered
// onto the event's commands.
type eventPolicy struct {
	matcherAllowed bool
	sources        []string // non-nil: legal matcher alternatives, in vocabulary order
	statusMessage  string
}

// eventPolicies is the Codex capability table. Events absent from it
// (PreCompact and anything else Codex does not document for guild's
// use) are not rendered: writing a hook the harness would never fire,
// or whose matcher semantics we have not verified, helps nobody.
var eventPolicies = map[string]eventPolicy{
	"SessionStart": {
		matcherAllowed: true,
		sources:        []string{"startup", "resume", "clear"},
		statusMessage:  "guild: loading session brief",
	},
	"UserPromptSubmit": {
		matcherAllowed: false,
		statusMessage:  "guild: searching lore relevant to your prompt",
	},
}

// Adapter renders guild hooks into Codex's per-repo hooks.json. The
// zero value is the production adapter; the unexported fields are
// test seams that default to the real implementations when nil.
type Adapter struct {
	detect      func() (bool, error)
	getwd       func() (string, error)
	gitToplevel func(ctx context.Context, dir string) (string, error)
	probe       func(ctx context.Context, dir string) (string, error)
	diag        io.Writer
}

func init() { adapters.Register(Adapter{}) }

// Name implements adapters.Adapter.
func (Adapter) Name() string { return "codex" }

// Detect implements adapters.Adapter by delegating to the Codex entry
// of internal/install's supported-client registry (codex CLI on PATH,
// else ~/.codex/config.toml present) so harness detection has exactly
// one source of truth.
func (a Adapter) Detect() (bool, error) {
	if a.detect != nil {
		return a.detect()
	}
	for _, c := range install.Clients {
		if strings.HasPrefix(c.Name, clientNamePrefix) {
			return c.Detected(), nil
		}
	}
	return false, errors.New("codex: no Codex entry in the install client registry")
}

// SettingsPath implements adapters.Adapter: <repo>/.codex/hooks.json,
// where <repo> is the git toplevel of the working directory. Outside a
// git repository the working directory itself is the project root,
// mirroring how Codex scopes a non-git workspace.
func (a Adapter) SettingsPath() (string, error) {
	getwd := a.getwd
	if getwd == nil {
		getwd = os.Getwd
	}
	cwd, err := getwd()
	if err != nil {
		return "", fmt.Errorf("codex: getwd: %w", err)
	}
	toplevel := a.gitToplevel
	if toplevel == nil {
		toplevel = defaultGitToplevel
	}
	root, err := toplevel(context.Background(), cwd)
	if err != nil {
		if errors.Is(err, project.ErrNotInGitRepo) {
			root = cwd
		} else {
			return "", fmt.Errorf("codex: resolve repo root of %s: %w", cwd, err)
		}
	}
	return filepath.Join(root, settingsDirName, settingsFileName), nil
}

// defaultGitToplevel resolves the git work-tree root via the same
// helper project resolution uses (`git rev-parse --show-toplevel`).
func defaultGitToplevel(ctx context.Context, dir string) (string, error) {
	return project.DefaultResolver.GitToplevel(ctx, dir)
}

// Substitute implements adapters.Adapter: Codex commands carry no
// placeholders.
func (Adapter) Substitute(cmd string) string { return cmd }

// Render implements adapters.Renderer: project the abstract config
// onto Codex's capability table. Events Codex has no hook for are
// dropped; SessionStart matchers narrow to Codex's source vocabulary;
// statusMessage is populated per event. Configs Codex must reject
// (matcher on a matcher-less event, a SessionStart matcher with no
// Codex-supported source) return an error instead of a hook that can
// never fire correctly.
func (Adapter) Render(cfg adapters.Config) (adapters.Config, error) {
	events := make([]string, 0, len(cfg))
	for ev := range cfg {
		events = append(events, ev)
	}
	sort.Strings(events) // deterministic error selection

	out := make(adapters.Config, len(cfg))
	for _, ev := range events {
		pol, ok := eventPolicies[ev]
		if !ok {
			continue // not part of Codex's hook contract (e.g. PreCompact)
		}
		groups := cfg[ev]
		rendered := make([]hooks.Group, 0, len(groups))
		for _, g := range groups {
			matcher := g.Matcher
			if matcher != "" && !pol.matcherAllowed {
				return nil, fmt.Errorf("codex: event %s does not support a matcher (got %q)", ev, matcher)
			}
			if matcher != "" && pol.sources != nil {
				narrowed, err := narrowMatcher(ev, matcher, pol.sources)
				if err != nil {
					return nil, err
				}
				matcher = narrowed
			}
			cmds := make([]hooks.Command, len(g.Hooks))
			for i, h := range g.Hooks {
				h.StatusMessage = pol.statusMessage
				cmds[i] = h
			}
			rendered = append(rendered, hooks.Group{Matcher: matcher, Hooks: cmds})
		}
		if len(rendered) > 0 {
			out[ev] = rendered
		}
	}
	return out, nil
}

// narrowMatcher filters a pipe-separated matcher down to the
// alternatives the harness dispatches on, preserving the base config's
// order. A matcher left with no supported alternative is an error: the
// group would silently never fire.
func narrowMatcher(event, matcher string, supported []string) (string, error) {
	ok := make(map[string]bool, len(supported))
	for _, s := range supported {
		ok[s] = true
	}
	var kept []string
	for _, tok := range strings.Split(matcher, "|") {
		if ok[tok] {
			kept = append(kept, tok)
		}
	}
	if len(kept) == 0 {
		return "", fmt.Errorf("codex: event %s matcher %q has no Codex-supported source (supported: %s)",
			event, matcher, strings.Join(supported, ", "))
	}
	return strings.Join(kept, "|"), nil
}

// Install implements adapters.Adapter: render and write the settings
// file, then probe by test fire. Codex's documented feature gate
// ([features] codex_hooks in ~/.codex/config.toml) was not required on
// v0.128.0, so the probe only diagnoses; it never blocks the install
// and the adapter never edits config.toml.
func (a Adapter) Install(base adapters.Config) error {
	if err := a.Sync(base); err != nil {
		return err
	}
	a.probeTestFire()
	return nil
}

// Sync implements adapters.Adapter: merge the rendered guild-owned
// groups into <repo>/.codex/hooks.json, preserving foreign content,
// writing atomically and only when something changed.
func (a Adapter) Sync(base adapters.Config) error {
	path, err := a.SettingsPath()
	if err != nil {
		return err
	}
	// G304: path derives from the git toplevel (or cwd) plus
	// compile-time constant components; no user-controlled input.
	raw, err := os.ReadFile(path) //nolint:gosec // trusted path; see note above
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("codex: read %s: %w", path, err)
	}
	desired, err := a.Render(hooks.ApplySubstitution(base, a.Substitute))
	if err != nil {
		return err
	}
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
// file, guild-owned and foreign alike. Missing file means empty list.
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
		return nil, fmt.Errorf("codex: read %s: %w", path, err)
	}
	return hooks.ScanSettingsDoc(raw, hooksKey)
}

// probeTestFire runs `codex exec` against the repo the settings file
// was written into and looks for the SessionStart hook debug signal in
// its output. Diagnostics go to the diag writer (stderr by default);
// nothing here ever fails the install: a missing CLI, an
// unauthenticated codex, or a silent hook all degrade to advice.
func (a Adapter) probeTestFire() {
	diag := a.diag
	if diag == nil {
		diag = os.Stderr
	}
	path, err := a.SettingsPath()
	if err != nil {
		fmt.Fprintf(diag, "codex: skipped hook test fire (%v)\n", err)
		return
	}
	repo := filepath.Dir(filepath.Dir(path))

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	probe := a.probe
	if probe == nil {
		probe = defaultProbe
	}
	out, err := probe(ctx, repo)
	switch {
	case strings.Contains(out, hookSignal):
		fmt.Fprintln(diag, "codex: test fire observed the SessionStart hook signal; hooks are live")
	case errors.Is(err, errNoCodexCLI):
		fmt.Fprintf(diag, "codex: wrote %s but skipped the hook test fire (codex CLI not on PATH); "+
			"if hooks do not fire, add [features] codex_hooks = true to ~/.codex/config.toml "+
			"(guild never edits that file)\n", path)
	default:
		fmt.Fprintf(diag, "codex: wrote %s but a test fire observed no %q signal; "+
			"your Codex version may require the feature gate: add [features] codex_hooks = true "+
			"to ~/.codex/config.toml and retry (guild never edits that file)\n", path, hookSignal)
	}
}

// defaultProbe is the production test fire: a one-shot non-interactive
// `codex exec` run from the repo root so Codex discovers the
// just-written .codex/hooks.json. Combined output is returned even on
// error or timeout; the caller only greps it for the hook signal.
func defaultProbe(ctx context.Context, dir string) (string, error) {
	if _, err := exec.LookPath("codex"); err != nil {
		return "", errNoCodexCLI
	}
	cmd := exec.CommandContext(ctx, "codex", "exec", "test")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
