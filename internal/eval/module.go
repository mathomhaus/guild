// Package eval is guild's evaluation capability module (ADR-006 Phase 6). It
// proves the lore recall/ranking pipeline is not gamed by poisoned memory and
// has not drifted since it was last locked, using a deterministic, no-LLM
// adversarial grid plus a golden-fixture parity harness.
//
// Two surfaces, both deterministic:
//
//   - Adversarial grid (grid.go): seeds a fixed scratch corpus of benign
//     entries shadowed by adversarial poisons (keyword-stuffed, injection-
//     shaped, near-duplicate) into an isolated in-memory lore database, runs
//     guild's real Appraise ranker, and emits RED/GREEN verdicts — RED when a
//     poison outranks the genuine answer or the answer falls out of rank 1.
//   - Golden-fixture parity (parity.go): records the full ranked output for a
//     fixed probe set into a committed JSON fixture and fails on any drift,
//     locking the determinism of the BM25 + recency + title-boost stack
//     across versions. The fixture is regenerated under GUILD_EVAL_UPDATE.
//
// The module ships OFF by default (DefaultEnabled()==false): with a silent
// config it contributes no CLI verb, no MCP tool, no daemon loop, and no
// INSTRUCTIONS fragment, so the default guild surface is byte-identical to a
// build without eval. It activates only when [modules].eval = true (or the
// GUILD_MODULE_EVAL / --module eval=true overrides), at which point the
// eval_run tool and `guild eval run` verb appear.
//
// Importing this package (directly or via internal/modules) runs the init()
// below, which self-registers the Module and its [eval] config section,
// exactly like the core pillars.
package eval

import (
	"io/fs"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/module"
)

// evalModule is the eval capability expressed as an ADR-006 Module.
type evalModule struct{}

// init self-registers the module and its config section. Both are pure
// init-time registration, identical in posture to the core pillars and to
// the RegisterModuleConfig examples in the ADR.
func init() {
	module.Register(evalModule{})
	registerConfig()
}

// Name is the stable identifier and the [modules] config key.
func (evalModule) Name() string { return "eval" }

// DefaultEnabled is false: eval is an opt-in capability. A silent config
// leaves it off so the default CLI / MCP / INSTRUCTIONS surface is unchanged.
func (evalModule) DefaultEnabled() bool { return false }

// Commands returns the eval verbs. The sole verb (eval_run / `guild eval
// run`) binds to both surfaces; it carries no MCPOnly/CLIOnly flag, so when
// the module is enabled it appears as both the eval_run tool and the CLI
// subcommand, and when disabled it appears on neither (the kernel never
// iterates a disabled module's Commands).
func (evalModule) Commands() []command.Registrant {
	return []command.Registrant{RunCommand}
}

// Migrations returns (nil, "") because the eval module owns no persistent
// database: every run seeds and tears down its own ephemeral in-memory corpus
// (grid.go's openScratchDB). Nothing of eval's lands in any on-disk database.
func (evalModule) Migrations() (fsys fs.FS, dbName string) { return nil, "" }

// Services returns nil: the eval module contributes no daemon background
// loops. Evaluation is an on-demand verb, not a supervised loop.
func (evalModule) Services() []module.Service { return nil }

// Instructions returns "": the eval module contributes no fragment to the MCP
// INSTRUCTIONS contract. Like the core pillars in Phase 2 the contract stays
// the monolithic instructions.md, and because eval is off by default its
// presence must not perturb the contract bytes either way.
func (evalModule) Instructions() string { return "" }
