// Package modules is the blank-import aggregator that activates guild's core
// capability modules (ADR-006). Importing this package runs the init() in
// each pillar package, which calls module.Register, so module.All() /
// module.Enabled() report the full core set (quest, lore, session) on every
// surface that imports modules.
//
// The kernel surfaces (internal/mcp, internal/cli) import this package for
// its side effects so a single line activates every core module, matching
// the ADR-006 "blank-import + config stanza" extension model: a future
// capability (observability, eval, compression) is added here as one more
// underscore import.
//
// It deliberately holds no logic of its own — only the import side effects —
// so it can sit below the kernel without creating an import cycle (the pillar
// packages do not import internal/modules).
package modules

import (
	// Core pillars. Each package's init() self-registers its Module.
	_ "github.com/mathomhaus/guild/internal/lore"
	_ "github.com/mathomhaus/guild/internal/quest"
	_ "github.com/mathomhaus/guild/internal/session"

	// Opt-in capability modules (off by default; the [modules] toggle, the
	// GUILD_MODULE_* env, or --module activates them). Each registers itself
	// and its config section in init().
	_ "github.com/mathomhaus/guild/internal/compression"
	_ "github.com/mathomhaus/guild/internal/eval"
	_ "github.com/mathomhaus/guild/internal/observability"
)
