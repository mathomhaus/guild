// Package hooks manages guild's harness lifecycle-hook configuration:
// the abstract base config stored at ~/.guild/hooks-base.json, the
// ownership-aware merge into harness settings files, and the atomic
// write primitive shared by every adapter.
//
// # ~/.guild/hooks-base.json schema
//
// The file is a single JSON object mapping a harness event name to an
// ordered list of hook groups:
//
//	{
//	  "<EventName>": [
//	    {
//	      "matcher": "<harness-defined dispatch pattern; omit when the event has no matcher>",
//	      "hooks": [
//	        {"type": "command", "command": "<shell command>"}
//	      ]
//	    }
//	  ]
//	}
//
// The matcher is harness dispatch, not a label: its values are defined
// by the harness (for SessionStart it matches the event's source field).
// Never invent synthetic matcher values; they would never fire.
//
// Built-in defaults (returned by DefaultBase, written on first
// `guild hooks install`):
//
//	SessionStart      matcher "startup|resume|clear|compact"
//	                  -> guild quest brief --auto
//	PreCompact        matcher "auto|manual"
//	                  -> guild quest brief --auto --capture
//	                  PreCompact stdout does not reach context
//	                  (observability-only); the capture path writes a
//	                  structured brief that the post-compact
//	                  SessionStart re-prime (matcher value "compact")
//	                  reads back.
//	UserPromptSubmit  no matcher (the event does not support one)
//	                  -> guild lore appraise --inject --from-stdin-json
//
// When ~/.guild/hooks-base.json exists it replaces the built-in
// defaults wholesale: the file is the user's override surface. Editing
// it and running `guild hooks sync` propagates the change to every
// harness. Per-project or per-event overrides are out of scope for now
// (global config only).
//
// Every command in the base config must be a guild command (matching
// ^guild( |$), see merge.go); LoadBase and SaveBase reject anything
// else. The ownership model only manages guild commands: a non-guild
// command written by sync would be classified foreign on the next scan
// and duplicated on every subsequent run. Custom hooks belong directly
// in the harness settings file, where sync preserves them untouched.
//
// The base config is harness-agnostic. Adapters (see the adapters
// subpackage) render it into each harness's native settings format and
// apply Substitute for adapter-specific placeholders.
package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/mathomhaus/guild/internal/guildpath"
)

// baseFileName is the basename of the base config under ~/.guild/.
const baseFileName = "hooks-base.json"

// basePerm is the mode for hooks-base.json and adapter settings files
// written by this package. ~/.guild is 0700; the file itself stays
// owner-only for consistency with the rest of guild's local state.
const basePerm os.FileMode = 0o600

// Command is one executable hook inside a group. StatusMessage is an
// optional harness-specific progress label (Codex renders it in its UI
// while the hook runs); adapters whose harness supports it set the
// field while rendering, base configs normally omit it. It carries no
// dispatch semantics and is ignored by ownership and drift comparisons.
type Command struct {
	Type          string `json:"type"`
	Command       string `json:"command"`
	StatusMessage string `json:"statusMessage,omitempty"`
}

// Group is one matcher block: an optional harness dispatch pattern plus
// the ordered commands that fire when it matches. Events that do not
// support a matcher (e.g. UserPromptSubmit) leave Matcher empty and the
// field is omitted from JSON.
type Group struct {
	Matcher string    `json:"matcher,omitempty"`
	Hooks   []Command `json:"hooks"`
}

// Config maps an event name (SessionStart, PreCompact, ...) to its
// ordered hook groups. This is the abstract, harness-agnostic shape;
// adapters wrap it in each harness's native format.
type Config map[string][]Group

// Hook is the flattened scan view of one hook command found in a
// harness settings file: the event and matcher context it fires under,
// the command string, and group-level ownership. Adapters return it
// from Scan; the CLI renders it for `guild hooks scan` and computes
// drift from it.
type Hook struct {
	Event      string `json:"event"`
	Matcher    string `json:"matcher,omitempty"`
	Command    string `json:"command"`
	GuildOwned bool   `json:"guild_owned"`
}

// DefaultBase returns the built-in base config (see the package doc for
// the rationale behind each event). The result is freshly allocated;
// callers may mutate it.
func DefaultBase() Config {
	return Config{
		"SessionStart": {
			{
				Matcher: "startup|resume|clear|compact",
				Hooks:   []Command{{Type: "command", Command: "guild quest brief --auto"}},
			},
		},
		"PreCompact": {
			{
				Matcher: "auto|manual",
				Hooks:   []Command{{Type: "command", Command: "guild quest brief --auto --capture"}},
			},
		},
		"UserPromptSubmit": {
			{
				Hooks: []Command{{Type: "command", Command: "guild lore appraise --inject --from-stdin-json"}},
			},
		},
	}
}

// BasePath returns the location of the base config file without
// creating anything: ~/.guild/hooks-base.json.
func BasePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("hooks: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".guild", baseFileName), nil
}

// ValidateBase rejects any base config containing a non-guild command.
// The ownership model (see merge.go) manages only commands matching
// ^guild( |$): a non-guild command rendered into a harness settings
// file would be classified foreign on the next scan, so sync could
// never reconcile it and would duplicate it on every run. Failing loud
// here keeps that corruption impossible.
func ValidateBase(cfg Config) error {
	events := make([]string, 0, len(cfg))
	for ev := range cfg {
		events = append(events, ev)
	}
	sort.Strings(events)
	for _, ev := range events {
		for _, g := range cfg[ev] {
			for _, h := range g.Hooks {
				if !CommandIsGuild(h.Command) {
					return fmt.Errorf("event %s: command %q is not a guild command: "+
						"the base config manages only commands matching ^guild( |$); "+
						"put custom hooks directly in your harness settings file instead "+
						"(guild hooks sync preserves them untouched)", ev, h.Command)
				}
			}
		}
	}
	return nil
}

// LoadBase reads ~/.guild/hooks-base.json. A missing file is not an
// error: the built-in defaults are returned instead, so read-only verbs
// (sync/diff/list/scan) work before `guild hooks install` ever ran. A
// file that exists but does not parse is an error; silently falling
// back to defaults would make sync clobber a config the user was
// mid-edit on. A file containing a non-guild command is also an error
// (see ValidateBase).
func LoadBase() (Config, error) {
	path, err := BasePath()
	if err != nil {
		return nil, err
	}
	// G304: path is the trusted ~/.guild dir plus a compile-time
	// constant basename; no user-controlled input.
	data, err := os.ReadFile(path) //nolint:gosec // trusted path; see note above
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultBase(), nil
		}
		return nil, fmt.Errorf("hooks: read %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("hooks: parse %s: %w", path, err)
	}
	if err := ValidateBase(cfg); err != nil {
		return nil, fmt.Errorf("hooks: invalid base config %s: %w", path, err)
	}
	return cfg, nil
}

// SaveBase writes cfg to ~/.guild/hooks-base.json atomically, creating
// ~/.guild (0700) if needed. Configs containing non-guild commands are
// rejected (see ValidateBase).
func SaveBase(cfg Config) error {
	if err := ValidateBase(cfg); err != nil {
		return fmt.Errorf("hooks: refusing to write base config: %w", err)
	}
	if _, err := guildpath.EnsureGuildDir(); err != nil {
		return err
	}
	path, err := BasePath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("hooks: encode base config: %w", err)
	}
	return WriteFileAtomic(path, append(data, '\n'), basePerm)
}

// EnsureBase loads the base config, writing the built-in defaults to
// ~/.guild/hooks-base.json first when the file does not exist. The
// second return reports whether the file was created by this call.
func EnsureBase() (Config, bool, error) {
	path, err := BasePath()
	if err != nil {
		return nil, false, err
	}
	if _, err := os.Stat(path); err == nil {
		cfg, err := LoadBase()
		return cfg, false, err
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("hooks: stat %s: %w", path, err)
	}
	cfg := DefaultBase()
	if err := SaveBase(cfg); err != nil {
		return nil, false, err
	}
	return cfg, true, nil
}

// WriteFileAtomic writes data to path via tmp file + fsync + rename, so
// a crash mid-write never leaves a torn settings file. The tmp file is
// created in path's directory (rename is only atomic within one
// filesystem) and removed on any failure.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("hooks: create temp file in %s: %w", dir, err)
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("hooks: write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("hooks: fsync %s: %w", tmp, err)
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("hooks: chmod %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("hooks: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return fmt.Errorf("hooks: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
