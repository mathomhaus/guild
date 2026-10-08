package quest

import (
	"io/fs"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/module"
	"github.com/mathomhaus/guild/internal/storage"
)

// questModule is the quest capability expressed as an ADR-006 Module. It
// self-registers in init() so importing this package (directly or via the
// internal/modules aggregator) makes the kernel aware of quest through the
// module registry instead of a hand-maintained BindMCP / bindRegistryVerb
// list. The kernel walks module.Enabled(...) and binds Commands() with the
// quest-side Deps bundle keyed by Name() == "quest".
type questModule struct{}

func init() { module.Register(questModule{}) }

// Name is the stable identifier and the [modules] config key.
func (questModule) Name() string { return "quest" }

// DefaultEnabled is true: quest is a core pillar, active unless config
// disables it (config-driven toggling is Phase 3).
func (questModule) DefaultEnabled() bool { return true }

// Commands returns the quest verbs as surface-neutral specs, in the same
// set the hand-maintained lists bound before the cutover. Order is not
// observable: the MCP SDK sorts tools/list alphabetically, cobra sorts
// CLI help, and the daemon-route parity tests diff by set membership, so
// this slice only needs to carry the right SET. ClearCommand is
// MCPOnly=true, so command.BindCobra skips it on the CLI surface and only
// the MCP surface advertises the quest_clear backward-compat alias,
// byte-identical to the pre-cutover wiring.
//
// SummonCommand, OrdersCommand, ScrollCommand, PulseCommand, GuildCommand,
// EpicCommand, ActiveCommand and ForfeitCommand round out the quest set.
func (questModule) Commands() []command.Registrant {
	return []command.Registrant{
		PostCommand,
		UpdateCommand,
		AcceptCommand,
		JournalCommand,
		CampfireCommand,
		FulfillCommand,
		ClearCommand, // MCP-only backward-compat alias for quest_fulfill
		BriefCommand,
		SummonCommand,
		OrdersCommand,
		ListCommand,
		ScrollCommand,
		PulseCommand,
		GuildCommand,
		EpicCommand,
		ActiveCommand,
		ForfeitCommand,
		SearchCommand,
	}
}

// Migrations returns the shared corpus plus the "quest" dbName. This is the
// ADR-006 Phase 2 documented shim: 001_init.up.sql still creates both lore
// and quest tables and existing ~/.guild installs already record versions
// 1..N in both DBs, so a physical per-DB corpus split risks breaking
// upgrades. Returning the shared DefaultMigrationFS() here records the
// structural seam (quest owns the "quest" database) without changing which
// versions land or the byte-identical 🔧 upgrade lines. The kernel keeps
// applying migrations at DB-open time via storage.Migrate(ctx, db,
// "quest"); wiring a startup module-migration loop and physically splitting
// the corpus per database is a deferred Phase 2 follow-up.
func (questModule) Migrations() (fsys fs.FS, dbName string) {
	return storage.DefaultMigrationFS(), "quest"
}

// Services returns nil: daemon background loops move to the module Service
// interface in Phase 3.
func (questModule) Services() []module.Service { return nil }

// Instructions returns "" by design. The agent-facing INSTRUCTIONS contract
// (internal/mcp/instructions.md) is one monolithic, prompt-cache-prefix
// string with no pillar-scoped section boundaries; it is hashed byte-for-byte
// in the golden e2e (17107 bytes). Splitting a quest fragment out of it
// cannot be done without changing the emitted bytes, so instructions.go stays
// the single source of the full contract and every module returns "". A clean
// per-module fragment assembly is a deferred follow-up once the contract grows
// explicit module sections.
func (questModule) Instructions() string { return "" }
