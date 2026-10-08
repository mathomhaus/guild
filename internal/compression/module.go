package compression

import (
	"io/fs"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/module"
)

// compressionModule is the ADR-006 Phase 7 capability expressed as a Module.
// It self-registers in init() so importing the package (via the
// internal/modules aggregator's one blank-import line) makes the kernel aware
// of it. DefaultEnabled is FALSE: with a silent config the module is absent
// everywhere — no CLI verb, no MCP tool, no daemon loop, no INSTRUCTIONS
// fragment — and the default lore_dossier path is byte-identical. The module
// only contributes when the operator sets [modules].compression = true.
type compressionModule struct{}

func init() { module.Register(compressionModule{}) }

// Name is the stable identifier and the [modules] config key.
func (compressionModule) Name() string { return "compression" }

// DefaultEnabled is false: compression is a heavy, opt-in capability per
// ADR-006. The kernel leaves it off unless config turns it on.
func (compressionModule) DefaultEnabled() bool { return false }

// Commands returns the compress + retrieve verbs. Both generate a CLI verb
// and an MCP tool; the kernel binds them only for an enabled module, so they
// never appear on the default surface.
func (compressionModule) Commands() []command.Registrant {
	return []command.Registrant{
		CompressCommand,
		RetrieveCommand,
	}
}

// Migrations returns (nil, "") because compression owns no SQLite database:
// the CCR store is an in-memory process-local store, not a logical DB.
func (compressionModule) Migrations() (fsys fs.FS, dbName string) { return nil, "" }

// Services returns nil: the module runs no daemon background loop. A future
// CCR-reaper or persistent-store loop would land here.
func (compressionModule) Services() []module.Service { return nil }

// Instructions returns "" by design, the same stance the core pillars take.
//
// The agent-facing INSTRUCTIONS contract (internal/mcp/instructions.md) is one
// monolithic, golden-hashed block (sha256:8248295e..., 17107 bytes). The
// kernel composes the contract by REMOVING a disabled module's fragment from
// that block (contractBody / disabledModulesWithFragments), so a fragment only
// participates if it is authored verbatim into instructions.md. Authoring a
// compression fragment into the file would move the golden hash on the DEFAULT
// path (compression is off by default, so the contract the parity oracle pins
// is the compression-disabled one), which the ADR-006 parity bar forbids.
//
// Returning "" therefore keeps the default contract byte-identical and is the
// only correct choice until the contract grows explicit per-module sections (a
// deferred follow-up shared with quest/lore/session). The module's verbs still
// document themselves through their command Short/Long, which the MCP tool
// schema surfaces when the module is enabled.
func (compressionModule) Instructions() string { return "" }
