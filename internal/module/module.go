// Package module is the guild capability-module SDK (ADR-006).
//
// A Module is a self-contained capability — quest, lore, session today;
// observability, eval, compression tomorrow — that contributes its verbs,
// storage, daemon loops, and a fragment of the agent-facing INSTRUCTIONS
// contract. Modules self-register in an init() func, exactly like
// internal/hooks/adapters, so importing a module package is all it takes to
// make the kernel aware of it:
//
//	func init() { module.Register(loreModule{}) }
//
// The kernel (internal/mcp + internal/cli wiring) walks Enabled(...) at
// startup and only wires modules the operator has left on. A disabled
// module is absent everywhere: no MCP tools, no CLI verbs, no daemon loop,
// no instruction fragment.
//
// Phase boundaries (ADR-006):
//   - Phase 1 (this package's first cut): the interface, the registry, and
//     Enabled. No module implements Module yet; nothing is wired. The point
//     is to introduce the abstraction with zero behavior change.
//   - Phase 2: quest/lore/session become the first Modules; the kernel binds
//     them via a loop and the hand-maintained registration lists are removed.
//     The Capabilities bundle that replaces command.Deps's any-typed fields
//     lands then.
//   - Phase 3: the daemon consumes Service; config drives Enabled.
package module

import (
	"context"
	"io/fs"

	"github.com/mathomhaus/guild/internal/command"
)

// Module is one guild capability. Every method is pure and cheap to call:
// the kernel may invoke Name/DefaultEnabled/Migrations/Instructions during
// startup wiring before deciding what to activate.
type Module interface {
	// Name is the stable identifier and the config key under [modules],
	// e.g. "quest". Must be non-empty and unique across registered modules.
	Name() string

	// DefaultEnabled reports whether the module is active when config is
	// silent about it. Core pillars (quest, lore, session) return true;
	// heavy or opt-in capabilities (compression) return false.
	DefaultEnabled() bool

	// Commands returns the module's verbs as surface-neutral specs. Each
	// already generates a *cobra.Command and/or *sdkmcp.Tool through
	// command.Registrant; the kernel binds them with surface-appropriate
	// dependencies at wiring time. Return nil for a module with no verbs.
	Commands() []command.Registrant

	// Migrations returns the module's own embedded migration corpus and the
	// logical database it owns (dbName, e.g. "lore" or "quest"), so each
	// module's schema is isolated rather than sharing one global corpus.
	// The returned fs.FS must contain a top-level "migrations/" directory of
	// NNN_description.up.sql files (the same convention storage.MigrateFS
	// reads). Return (nil, "") for a module with no storage of its own.
	Migrations() (fsys fs.FS, dbName string)

	// Services returns daemon background loops to run while the module is
	// enabled AND the daemon is up. Return nil for a module with no loops.
	// The daemon consumes these in Phase 3.
	Services() []Service

	// Instructions returns the module's fragment of the MCP INSTRUCTIONS
	// contract, included only when the module is enabled. Return "" for
	// none.
	Instructions() string
}

// Service is a daemon-hosted background loop contributed by a module. It
// generalizes the per-loop fields hand-wired onto internal/daemon.Config
// today (watch, scheduler, reaper, registry) into a uniform interface the
// daemon's Run can range over. Defined here, not in internal/daemon, so the
// daemon can import module to collect services without module importing the
// daemon (which would cycle once the daemon also iterates Enabled modules).
type Service interface {
	// Name identifies the loop in logs and daemon status, e.g. "reaper".
	Name() string
	// Start launches the loop. It should return promptly, doing long-running
	// work in its own goroutine, and honor ctx cancellation for shutdown.
	Start(ctx context.Context) error
	// Stop requests a graceful shutdown and blocks until the loop has drained
	// or ctx is cancelled.
	Stop(ctx context.Context) error
}
