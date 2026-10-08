package lore

import (
	"io/fs"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/module"
	"github.com/mathomhaus/guild/internal/storage"
)

// loreModule is the lore capability expressed as an ADR-006 Module. It
// self-registers in init() so the kernel discovers lore through the module
// registry instead of the hand-maintained BindMCP / bindLoreRegistryVerb
// lists. The kernel binds Commands() with the lore-side Deps bundle keyed by
// Name() == "lore" (lore.db, RecordMiss wired, the lore embed provider).
type loreModule struct{}

func init() { module.Register(loreModule{}) }

// Name is the stable identifier and the [modules] config key.
func (loreModule) Name() string { return "lore" }

// DefaultEnabled is true: lore is a core pillar, active unless config
// disables it (config-driven toggling is Phase 3).
func (loreModule) DefaultEnabled() bool { return true }

// Commands returns the lore verbs as surface-neutral specs, the same set the
// hand lists bound before the cutover (read, write, hygiene, and the
// ADR-003 embedder-health trio). Order is not observable (see questModule);
// only the SET matters. None of these are MCPOnly/CLIOnly, so each binds to
// both surfaces.
func (loreModule) Commands() []command.Registrant {
	return []command.Registrant{
		// read + write, common
		AppraiseCommand,
		StudyCommand,
		OathCommand,
		ListCommand,
		DossierCommand,
		InscribeCommand,
		ReforgeCommand,
		UpdateCommand,
		EchoesCommand,
		WhispersCommand,
		LinkCommand,
		UnlinkCommand,
		RipplesCommand,
		// hygiene
		InquestCommand,
		MeldCommand,
		CommuneCommand,
		SealCommand,
		CatalogCommand,
		// embedder health (Phase 1.6 ADR-003)
		EmbedderHealthCommand,
		EmbedRebuildCommand,
		CoverageReconcileCommand,
	}
}

// Migrations returns the shared corpus plus the "lore" dbName. Documented
// ADR-006 Phase 2 shim, identical rationale to questModule.Migrations: the
// shared corpus stays shared (avoiding upgrade risk for existing installs)
// and this only records that lore owns the "lore" database. The kernel keeps
// applying migrations at DB-open time via storage.Migrate(ctx, db, "lore").
func (loreModule) Migrations() (fsys fs.FS, dbName string) {
	return storage.DefaultMigrationFS(), "lore"
}

// Services returns nil: daemon loops move to module Services in Phase 3.
func (loreModule) Services() []module.Service { return nil }

// Instructions returns "" by design; see questModule.Instructions. The full
// INSTRUCTIONS contract stays in internal/mcp/instructions.md, hashed in the
// golden e2e, and is not split into per-module fragments in Phase 2.
func (loreModule) Instructions() string { return "" }
