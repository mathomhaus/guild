// Package adapters defines the per-harness adapter interface for guild
// lifecycle hooks plus the self-registration registry adapters join via
// init(). The abstract base config (internal/hooks.Config) is the
// source of truth; each adapter renders it into its harness's native
// settings format.
package adapters

import (
	"github.com/mathomhaus/guild/internal/hooks"
)

// Config is the abstract hook configuration handed to adapters.
// Alias of internal/hooks.Config so adapter implementations only need
// this package on their import line.
type Config = hooks.Config

// Hook is the flattened scan view returned by Scan.
// Alias of internal/hooks.Hook.
type Hook = hooks.Hook

// Adapter renders the abstract guild hook config into one harness's
// native settings format and reads it back.
//
// Install and Sync receive the UNsubstituted base config; the adapter
// applies its own Substitute while rendering. Both must be idempotent
// (no write when the rendered state already matches) and must preserve
// foreign content in the settings file: hook groups not owned by guild
// (see hooks.GroupIsGuildOwned) and unrelated top-level fields stay
// byte-for-byte intact. All writes go through tmp + fsync + rename
// (hooks.WriteFileAtomic).
type Adapter interface {
	// Name is the stable registry key, e.g. "claude-code", "codex".
	Name() string

	// Detect reports whether this harness exists on this machine.
	Detect() (bool, error)

	// SettingsPath returns the settings file this adapter manages.
	SettingsPath() (string, error)

	// Install performs first-time setup of the guild hooks.
	Install(base Config) error

	// Sync regenerates the guild-owned hook groups from base.
	Sync(base Config) error

	// Scan reads the current settings file and returns every hook in
	// it, guild-owned and foreign alike. A missing file yields an
	// empty list, not an error.
	Scan() ([]Hook, error)

	// Substitute replaces adapter-specific placeholders in one command
	// string. Adapters without placeholders return cmd unchanged.
	Substitute(cmd string) string
}

// Renderer is an optional interface for adapters whose harness cannot
// represent the base config one-to-one. Render maps an
// already-substituted config onto the harness's capabilities: dropping
// events the harness has no hook for, narrowing matcher vocabularies
// to the values the harness dispatches on, and adding
// harness-specific presentation fields. It returns an error for
// configs the harness must reject (e.g. a matcher on an event that
// does not support one) rather than silently writing a hook that can
// never fire.
//
// Render must not mutate cfg; it returns a fresh Config. Adapters that
// implement Renderer apply it inside Install/Sync, and the CLI applies
// it when computing desired state, so drift status agrees with what
// the adapter actually writes.
type Renderer interface {
	Render(cfg Config) (Config, error)
}
