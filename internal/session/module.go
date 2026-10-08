package session

import (
	"io/fs"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/module"
)

// sessionModule is the session capability expressed as an ADR-006 Module,
// the third core pillar alongside quest and lore. It self-registers in
// init() so module.Enabled(...) consistently reports all three pillars on
// every surface.
//
// session contributes no registry Commands in Phase 2: the agent-facing
// bootstrap tools (guild_session_start, guild_set_project, guild_status,
// quest_bounties) are hand-wired *sdkmcp.Tool registrations in
// internal/mcp (registerBootstrap), not command.Command specs, and the
// 529-line session_start is left exactly as-is to keep its output
// byte-identical. Carrying those into the command/module shape is a
// deferred follow-up. session owns no database (state lives in a per-PID
// JSON sidecar, not SQLite), runs no daemon loops yet, and contributes no
// INSTRUCTIONS fragment. The module therefore returns the empty value from
// every contribution method; its sole Phase 2 role is to make the registry
// list the three pillars uniformly.
type sessionModule struct{}

func init() { module.Register(sessionModule{}) }

// Name is the stable identifier and the [modules] config key.
func (sessionModule) Name() string { return "session" }

// DefaultEnabled is true: session is a core pillar.
func (sessionModule) DefaultEnabled() bool { return true }

// Commands returns nil: the bootstrap tools stay hand-wired in internal/mcp
// for Phase 2 (see the type doc).
func (sessionModule) Commands() []command.Registrant { return nil }

// Migrations returns (nil, "") because session owns no SQLite database.
func (sessionModule) Migrations() (fsys fs.FS, dbName string) { return nil, "" }

// Services returns nil: session daemon loops move to module Services in
// Phase 3.
func (sessionModule) Services() []module.Service { return nil }

// Instructions returns "": the full INSTRUCTIONS contract stays in
// internal/mcp/instructions.md (hashed in the golden e2e), not split into
// per-module fragments in Phase 2.
func (sessionModule) Instructions() string { return "" }
