// Package observability is the ADR-006 Phase 5 capability module: the
// Prometheus + JSONL-event-log + durable-rollup triad for the guild daemon,
// plus the decision-gate value objects that record (never change) the
// daemon's autopass / lease-reap / staleness-renewal decisions.
//
// The module is OFF by default (DefaultEnabled returns false). With a silent
// config it contributes nothing: its daemon Service is never collected (the
// kernel drops a disabled module before reading Services()), so no metrics
// port is opened, no event log is written, and the daemon's decision sink
// stays nil, leaving daemon behavior byte-identical to a build without this
// module. It is activated by one of: [modules].observability = true,
// GUILD_MODULE_OBSERVABILITY=1, or --module observability=true, exactly the
// ADR-006 toggle seam every module shares.
//
// It contributes no MCP tools or CLI verbs (observability is operational, not
// agent-facing) and no SQLite storage (its state is the JSONL log + the JSON
// rollup sidecar under ~/.guild/observability/). Its only surface is the
// daemon Service. Instructions() returns "" deliberately: observability is
// not part of the agent-facing INSTRUCTIONS contract, so there is no fragment
// to include or exclude (the contractBody machinery in internal/mcp is
// therefore untouched and the golden contract stays byte-identical).
package observability

import (
	"io/fs"
	"log/slog"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/config"
	"github.com/mathomhaus/guild/internal/module"
)

// observabilityModule is the observability capability expressed as an ADR-006
// Module. It self-registers in init() (module registry) and registers its
// config section on the Phase-3 RegisterModuleConfig seam, so importing this
// package via the internal/modules aggregator makes the kernel aware of both
// the module and its [observability] config without any edit to
// internal/config core sections.
type observabilityModule struct{}

func init() {
	module.Register(observabilityModule{})
	config.RegisterModuleConfig("observability", mergeConfig, applyDefaults)
}

// Name is the stable identifier and the [modules] config key.
func (observabilityModule) Name() string { return "observability" }

// DefaultEnabled is false: observability is an opt-in operational capability,
// off unless the operator turns it on. This is the parity guarantee: a silent
// config leaves the module absent everywhere.
func (observabilityModule) DefaultEnabled() bool { return false }

// Commands returns nil: observability contributes no agent-facing verbs.
func (observabilityModule) Commands() []command.Registrant { return nil }

// Migrations returns (nil, "") because observability owns no SQLite database;
// its durable state is the JSONL event log and the JSON rollup sidecar.
func (observabilityModule) Migrations() (fsys fs.FS, dbName string) { return nil, "" }

// Services returns the daemon Service (the metrics endpoint + decision
// recorder + rollup loop), built from the resolved [observability] settings.
// It is only ever consulted for an enabled module, so a disabled
// observability never opens a port or installs a recorder. A construction
// failure (e.g. the guild home is unwritable) degrades to no service with a
// logged warning rather than crashing daemon startup.
func (observabilityModule) Services() []module.Service {
	svc, err := NewService(CurrentSettings(), "", "", slog.Default())
	if err != nil {
		slog.Warn("observability: could not build daemon service; observability disabled for this run", "err", err.Error())
		return nil
	}
	return []module.Service{svc}
}

// Instructions returns "" by design: observability is operational, not an
// agent-facing contract contributor, so it owns no INSTRUCTIONS fragment.
// Leaving it empty keeps the monolithic contract in internal/mcp/instructions.md
// byte-identical and means the contractBody fallback paths need no change.
func (observabilityModule) Instructions() string { return "" }
