package command

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/spf13/cobra"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Command is the single declarative spec for a guild verb. The same
// value generates both a *cobra.Command and a *sdkmcp.Tool. I is the
// input struct (plain Go with `json:"..."` + `jsonschema:"..."` tags);
// O is the domain result. Handler holds the business logic; Format
// renders O into a Sink; ErrorFormat optionally overrides error
// narration for structured errors.
type Command[I, O any] struct {
	// Name is the MCP tool wire name, e.g. "quest_accept". The CLI
	// adapter uses the last segment of CLIPath for cobra's Use field.
	Name string
	// CLIPath is the cobra tree path, e.g. ["quest", "accept"]. The
	// caller attaches to the parent command matching CLIPath[:len-1].
	CLIPath []string
	// Short is the one-line help shown in cobra's Use/Short and in the
	// MCP tool's Description when Long is empty. Lowercase fragment is
	// fine, matching existing cobra idiom.
	Short string
	// Long is the extended help for cobra's Long field and the MCP
	// tool's Description. If empty, Short is used for both.
	Long string
	// MCPOnly suppresses CLI registration (e.g. guild_session_start).
	MCPOnly bool
	// CLIOnly suppresses MCP registration (e.g. guild mcp-install).
	CLIOnly bool
	// CLIAliases are alternate cobra command names — `lore oath` also
	// accepts `lore principles`. Ignored by the MCP surface (tool
	// discovery uses the canonical Name only).
	CLIAliases []string
	// Args describes every argument the command accepts. Positional
	// args come first (in declaration order), then flags.
	Args []ArgSpec
	// Handler is the surface-neutral business logic. Called after arg
	// parsing / schema validation with a populated I.
	Handler func(ctx context.Context, d Deps, in I) (O, error)
	// CLIFormat renders the handler's output for the cobra surface.
	// Receives a concrete CLISink so verbs can use rich primitives
	// (tables, sections, separators) without going through a
	// lowest-common-denominator interface. Required unless MCPOnly=true.
	CLIFormat func(s CLISink, o O) string
	// CLIWarnings, when non-nil, is called after CLIFormat on the CLI
	// surface. Its return value is written to stderr so non-fatal
	// warnings (dedup hits, bloat notices) don't pollute stdout while
	// the command still exits 0. MCP callers never invoke this; they
	// encode warnings inside their structured MCPFormat output instead.
	CLIWarnings func(s CLISink, o O) string
	// CLIAliasDeprecations maps a cobra alias name to a deprecation notice
	// that should be written to stderr when the command is invoked via that
	// alias. The notice is emitted after CLIWarnings, only in human-rendering
	// mode (not --json), so scripted consumers see no output change.
	CLIAliasDeprecations map[string]string
	// MCPFormat renders the handler's output for the MCP surface.
	// Receives a concrete MCPSink with compact-output primitives.
	// Required unless CLIOnly=true.
	MCPFormat func(s MCPSink, o O) string
	// CLIErrorFormat optionally handles bespoke error narration on the
	// CLI surface. Return ok=true to substitute the string for the
	// default error rendering; ok=false falls through to cobra's
	// default "Error: ..." output.
	CLIErrorFormat func(s CLISink, err error) (msg string, ok bool)
	// MCPErrorFormat optionally handles bespoke error narration on the
	// MCP surface. Return ok=true to substitute the string for
	// toolErrorf; ok=false falls through to "quest_X: <err>".
	MCPErrorFormat func(s MCPSink, err error) (msg string, ok bool)
}

// Deps is the factory-injected dependency bundle. One Deps value is
// constructed per surface (one for CLI, one for MCP) and passed to every
// Command registration. Mirrors gh's cmdutil.Factory / kubectl's
// cmdutil.Factory patterns.
type Deps struct {
	// OpenDB opens the appropriate SQLite database for the verb's
	// domain. Callers must Close the returned *sql.DB.
	OpenDB func(ctx context.Context) (*sql.DB, error)
	// ResolveProj turns an argProject string into a final project ID.
	// Empty argProject means "use the surface default" — CWD+git for
	// CLI, session state for MCP.
	ResolveProj func(ctx context.Context, argProject string) (projectID string, err error)
	// Now returns the current time. Overrideable for deterministic
	// tests.
	Now func() time.Time
	// RecordTelemetry, when non-nil, is called by the MCP adapter after
	// each tool-handler invocation to emit a usage.log row. CLI-side
	// Deps leaves this nil because wrapTelemetry handles CLI telemetry.
	// errPtr is a *error and respBytesPtr is a *uint so a defer-based
	// caller observes the late-bound values after the handler executes.
	//
	//nolint:gocritic // ptrToRefParam — defer must observe the late-bound error
	RecordTelemetry func(ctx context.Context, toolName string, start time.Time, errPtr *error, respBytesPtr *uint)
	// RecordMiss, when non-nil, is called by MCP adapters that perform
	// retrieval (e.g. lore_appraise) when the handler returns zero
	// results. Kept nil on CLI (the CLI handler calls telemetry directly).
	RecordMiss func(ctx context.Context, project, query string)
	// PrependNarration, when true, enables the implicit auto-bootstrap
	// narration injection path in the MCP handler wrapper. The handler
	// places a *string into ctx via mcpWithNarrationPtr; if ResolveProj
	// auto-bootstraps from cwd, it writes the narration line into that
	// pointer. The handler then prepends it to the tool's output body.
	//
	// Set to true only on MCP-side Deps that wire resolveProjectAutoBootstrap
	// as ResolveProj. CLI-side Deps leave this false.
	PrependNarration bool
	// OpenLoreDB, when non-nil, opens the lore SQLite database. Only
	// needed by quest handlers that atomically inscribe lore entries
	// (e.g. quest_post with spec=). Nil means the feature is unavailable
	// for that surface / test setup.
	OpenLoreDB func(ctx context.Context) (*sql.DB, error)
	// OpenQuestDB provides the quest corpus for combined embedding health.
	OpenQuestDB func(ctx context.Context) (*sql.DB, error)
	// EvaluateHints, when non-nil, is called by the MCP handler wrapper
	// after each successful tool invocation. Returns a HintFire the
	// wrapper formats and prepends/appends to the tool's output body.
	// Errors are swallowed inside the callback — hint evaluation must
	// never break a tool call. See internal/hints/engine.go for the
	// reference implementation wired into Register().
	EvaluateHints EvaluateHintsFunc
	// ExecRemote, when non-nil, gives the cobra adapter a transport for
	// running a verb's Handler inside the guild daemon (registry
	// JSON-exec, ADR-005 Part 1). The adapter ships json.Marshal(I) and
	// decodes the returned json.Marshal(O); CLIFormat then renders the
	// round-tripped O locally, so terminal output stays byte-identical.
	//
	// Error contract: returning a *RemoteHandlerError means the Handler
	// ran remotely and failed (final, never re-run locally); any other
	// error means the transport failed and the adapter falls back to the
	// local Handler. Only CLI-side Deps set this; MCP-side Deps and the
	// daemon's own exec Deps leave it nil.
	ExecRemote func(ctx context.Context, req RemoteExecRequest) (json.RawMessage, error)
	// Embed is the optional embedding-pipeline port. The field type is
	// `any` so the command package does not import internal/lore (which
	// would create a cycle: lore registers commands, command depends on
	// lore). Handlers in internal/lore type-assert this to
	// *lore.EmbedDeps via embedFromDeps(d). A nil value (the default)
	// is the Phase-0 fallback: BM25+stopwords only, per ADR-003.
	//
	// Constructed by the adapter: internal/mcp startup (Async=true,
	// warmed Index) and internal/cli bootstrap (Async=false, no Index).
	// See internal/lore/embed_deps.go for the port definition and
	// QUEST-213 for the wiring rationale.
	Embed any
	// LoreValidDays, when non-nil, returns the merged per-kind
	// valid_days windows from config ([inscribe.valid_days]): kind name
	// to window in days, 0 meaning "never stale". Adapters wire a
	// closure over config.Load; the field stays a plain map-returning
	// func so neither this package nor internal/lore below it imports
	// internal/config. Called lazily at handler invocation time, so the
	// long-lived MCP server observes config edits without a restart and
	// CLI verbs that never inscribe pay no config read. A nil func or
	// nil map falls back to the built-in kind defaults in internal/lore.
	LoreValidDays func() map[string]int
	// Lease is the optional quest-lease port (ADR-005 Part 1, daemon
	// Phase 3). The field type is `any` so the command package does not
	// import internal/quest (which would create a cycle: quest registers
	// commands, command depends on quest). The quest_accept handler
	// type-asserts this to a quest.LeaseAcquirer via leaseFromDeps(d) and
	// records a lease after a claim commits, best-effort.
	//
	// A nil value (the default) is the no-daemon path: quest_accept
	// performs exactly today's writes and creates zero task_leases rows,
	// so its DB effects stay byte-identical. Only the daemon constructs
	// Deps with a non-nil Lease, bound to its per-session identity. The
	// in-process stdio server and the plain CLI leave it nil. See
	// internal/quest/lease.go for the port definition.
	Lease any
}

// ResolveLoreValidDays returns the configured per-kind valid_days
// windows, or nil when no provider is wired (the built-in kind defaults
// apply downstream).
func (d Deps) ResolveLoreValidDays() map[string]int {
	if d.LoreValidDays == nil {
		return nil
	}
	return d.LoreValidDays()
}

// Registrant is the erased-handle interface that lets a heterogeneous
// set of Command[I, O] values share registration code. Command[I, O]
// satisfies this via its BindCobra / BindMCP methods.
//
// WireName and CobraPath expose the two identifiers a type-erased caller
// needs to wire a verb without recovering its concrete generic type: the
// MCP tool name (for tracking / parity diffs) and the cobra tree path (for
// deriving the per-verb telemetry label). They are accessors, not the
// exported Name / CLIPath struct fields, because a method may not share a
// name with a field on the same type. Added for the ADR-006 module-loop
// CLI binder, which iterates []Registrant.
type Registrant interface {
	BindCobra(parent *cobra.Command, d Deps)
	BindMCP(server *sdkmcp.Server, d Deps)
	// BindExec registers the verb on a daemon-side ExecRegistry with the
	// given Deps builder (the type-erased RegisterExec). Lets the ADR-006
	// module loop build the daemon JSON-exec dispatch table from a module's
	// Commands() without recovering each command's concrete generic type.
	// Exec-exempt verbs are skipped exactly as RegisterExec skips them.
	BindExec(r *ExecRegistry, deps DepsBuilder)
	// WireName returns the MCP tool wire name (Command.Name).
	WireName() string
	// CobraPath returns the cobra tree path (Command.CLIPath).
	CobraPath() []string
}

// WireName returns the command's MCP tool wire name (the Name field).
func (c *Command[I, O]) WireName() string { return c.Name }

// CobraPath returns the command's cobra tree path (the CLIPath field).
func (c *Command[I, O]) CobraPath() []string { return c.CLIPath }
