package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/daemon"
	"github.com/mathomhaus/guild/internal/session"
)

// serverName is the MCP server identity the host sees in its
// `tools/list` response and logs. Lowercase, stable — agents rely on
// the literal string "guild" in tool-prefix disambiguation
// (`mcp__guild__*`).
const serverName = "guild"

// serverVersion is advertised to the host on connect. Bumped manually
// on protocol-visible changes (new tools, INSTRUCTIONS edits, schema
// shifts). Wire to the build-time VCS stamp when release tooling
// matures; until then, hand-updated.
const serverVersion = "0.1.0-dev"

// SessionStore is the session-identity seam: how one server instance
// resolves and persists its active project. The stdio server defaults
// to the per-process store (the ~/.guild/sessions/<pid>.json file keyed
// by os.Getpid()); a host serving several sessions from one process
// supplies one store per connection so concurrent sessions cannot
// clobber each other's active project.
//
// session.Manager satisfies this interface, so callers can pass
// session.Manager{BaseDir: ..., PID: ...} directly.
type SessionStore interface {
	// ResolveForMCP returns the project id for a tool call given the
	// optional explicit arg and the GUILD_PROJECT env value. See
	// session.ResolveForMCP for the resolution order.
	ResolveForMCP(ctx context.Context, arg, env string) (string, error)
	// SetActiveProject persists the active project for this session.
	SetActiveProject(ctx context.Context, name string) error
}

// Compile-time check: session.Manager is a valid SessionStore, so
// callers can inject per-connection identities without an adapter.
var _ SessionStore = session.Manager{}

// processSessionStore is the default SessionStore. It delegates to the
// package-level session helpers, which resolve $HOME and os.Getpid() on
// every call; that lazy resolution is load-bearing for tests that move
// HOME between calls (see session/state.go defaultManager).
type processSessionStore struct{}

func (processSessionStore) ResolveForMCP(ctx context.Context, arg, env string) (string, error) {
	return session.ResolveForMCP(ctx, arg, env)
}

func (processSessionStore) SetActiveProject(ctx context.Context, name string) error {
	return session.SetActiveProject(ctx, name)
}

// serverCore carries the per-server-instance seams every tool handler
// closes over: which session identity resolves and persists the active
// project, which provider bundle supplies embedders and hints, and
// which working directory anchors cwd-based project inference.
// One core per constructed server; the handlers registered on a server
// reference exactly one core for their whole lifetime.
type serverCore struct {
	sessions  SessionStore
	providers *Providers

	// cwd anchors project auto-inference for this server instance.
	// Empty means the process working directory (the stdio default);
	// the daemon sets it to the shim's preamble cwd so concurrent
	// sessions from different checkouts resolve independently.
	cwd string

	// lease is the per-session quest-lease port threaded onto
	// command.Deps.Lease for this server's quest handlers (ADR-005 Phase
	// 3). Nil for the stdio and in-process fallback paths (the
	// byte-identical no-daemon contract); only the daemon sets it, bound
	// to the connection's session identity.
	lease any

	// presence is the daemon's live-session presence source (ADR-005 Phase
	// 3). Non-nil ONLY when this server is served inside a running daemon:
	// guild_session_start reads it to append the "N agents active" line.
	// Nil for the stdio and in-process fallback paths, where no session
	// registry exists, so session_start emits no presence line and stays
	// byte-identical to today. It is read from the in-memory registry
	// snapshot only, never the db, so it adds no round-trip to session_start.
	presence PresenceSource
}

// PresenceSource is the seam guild_session_start reads to render the
// daemon-only "N agents active" line. The daemon wires its session
// registry behind it; the stdio and in-process paths leave it nil, which is
// the byte-identical no-daemon contract: no registry, no presence line. It
// is satisfied by *daemon.Registry without an adapter (see daemon.go).
type PresenceSource interface {
	// Presence returns the live-session count and the on-lease session
	// count as a single in-memory read (no db round-trip).
	Presence() daemon.Presence
}

// inferProject resolves the project from this server's connection cwd:
// the shim preamble cwd for daemon sessions, the process cwd for the
// stdio server. See inferProjectFromDir.
func (c *serverCore) inferProject(ctx context.Context) (projectID string, viaFallback bool, err error) {
	return inferProjectFromDir(ctx, c.cwd)
}

// Options configures server construction. The zero value reproduces the
// stdio server's defaults exactly; hosts that serve more than one
// session per OS process override the seams they need.
type Options struct {
	// Instructions is the INSTRUCTIONS string advertised to the host on
	// initialize. Empty means the static contract (instructions.md)
	// without baked principles, the same default the test and docgen
	// paths use. The stdio path passes the dynamically resolved string.
	Instructions string

	// Sessions is the session-identity store handlers resolve and
	// persist the active project through. Nil means the per-process
	// default (~/.guild/sessions/<os.Getpid()>.json).
	Sessions SessionStore

	// Providers is an externally owned provider bundle. Nil means the
	// server constructs a fresh default bundle (the stdio behavior). A
	// multi-session host builds one bundle via NewProviders and passes
	// it to every per-connection NewServer call so the embedder, hints
	// engine, and backfill trigger are constructed once per process,
	// not once per connection. Building against a supplied bundle never
	// resets or closes the bundle's state.
	//
	// Concurrent NewServer calls must supply Providers: the default
	// bundle construction updates the per-process tracker and is not
	// safe to race (same constraint the package-level singletons had
	// before the bundle existed).
	Providers *Providers

	// CWD anchors cwd-based project auto-inference for this server
	// instance (guild_session_start with no arg and the implicit
	// auto-bootstrap path). Empty means the process working directory,
	// which is correct for the stdio server. The daemon passes each
	// connection's shim preamble cwd so sessions resolve against the
	// directory the agent is actually working in, not wherever the
	// daemon happened to start.
	CWD string

	// Lease is the optional per-session quest-lease port (ADR-005 Phase
	// 3), threaded onto command.Deps.Lease for the quest handlers. The
	// daemon constructs one value per connection, bound to that session's
	// identity, so an accept under the session writes a lease and a
	// mutating call renews it. The field is `any` for the same reason
	// command.Deps.Lease is: internal/mcp avoids forcing a quest type into
	// the server-construction surface. Nil (the stdio and in-process
	// fallback default) is the byte-identical no-daemon path: no lease row
	// is ever written.
	Lease any

	// Presence is the optional daemon session-presence source (ADR-005
	// Phase 3). The daemon passes its session registry so
	// guild_session_start can append the "N agents active" line; the stdio
	// and in-process fallback paths leave it nil so session_start emits no
	// presence line and stays byte-identical to today. Read in-memory only,
	// so it adds no db round-trip to the session_start hot path.
	Presence PresenceSource
}

// NewServer constructs a fully registered guild MCP server from opts.
// This is the construction seam for hosts that build several servers in
// one process; Serve, build, and BuildForDocs all route through it with
// default options.
func NewServer(opts Options) (*sdkmcp.Server, error) {
	instructions := opts.Instructions
	if instructions == "" {
		instructions = staticInstructions
	}
	sessions := opts.Sessions
	if sessions == nil {
		sessions = processSessionStore{}
	}
	providers := opts.Providers
	if providers == nil {
		providers = newProcessProviders()
	}

	logger := newLogger()

	s := sdkmcp.NewServer(
		&sdkmcp.Implementation{
			Name:    serverName,
			Version: serverVersion,
		},
		&sdkmcp.ServerOptions{
			// Instructions is the agent-visible contract loaded at
			// initialize. See instructions.go for the journal of
			// adjustments.
			Instructions: instructions,
			// Logger routes SDK-internal events (session connect,
			// handler panics, malformed messages) through our
			// stderr-bound slog handler. Stdout remains JSON-RPC
			// only.
			Logger: logger,
		},
	)

	// Register all tools (bootstrap + always-on) against this server's
	// core. See register.go.
	registerAll(s, &serverCore{sessions: sessions, providers: providers, cwd: opts.CWD, lease: opts.Lease, presence: opts.Presence})

	return s, nil
}

// Serve constructs the guild MCP server and runs it on the stdio
// transport until ctx is cancelled or the client closes the session.
//
// Construction steps, in order:
//
//  1. Build a stderr-bound [slog.Logger] (stdout is the JSON-RPC
//     transport; logging there corrupts the protocol).
//  2. [sdkmcp.NewServer] with the dynamically built INSTRUCTIONS string
//     (static contract + active project's principles) in
//     [sdkmcp.ServerOptions] so the host loads the full discipline
//     contract during `initialize`.
//  3. Register all tools against a default per-process core (per-PID
//     session identity, fresh provider bundle). See register.go.
//  4. Run over [sdkmcp.StdioTransport] — the SDK handles the JSON-RPC
//     framing, keep-alive, and cancellation plumbing.
//
// Run blocks until ctx is cancelled (via signal.NotifyContext in
// cmd/guild/mcp.go) or the peer closes the connection. A cancelled ctx
// is NOT an error from the server's perspective — the operator asked us
// to stop — so we suppress the resulting ctx.Err() and return nil.
func Serve(ctx context.Context) error {
	log := newLogger()

	// Stale session cleanup runs exactly once per server start.
	// Best-effort: a probe error or missing directory is never fatal —
	// we log the failure and continue serving. The sessions dir may not
	// exist yet on the very first run (CleanupStale treats it as a no-op).
	if err := session.CleanupStale(ctx); err != nil {
		log.Warn("stale session cleanup failed (non-fatal)", "err", err)
	}

	s, err := buildWithContext(ctx)
	if err != nil {
		return err
	}

	if err := s.Run(ctx, &sdkmcp.StdioTransport{}); err != nil {
		// A context-cancelled exit is the expected happy-path shutdown
		// — signal.NotifyContext cancelled us because the operator
		// sent SIGINT/SIGTERM. Returning that error would make the
		// cobra RunE layer print "error: context canceled" to stderr,
		// which misrepresents a clean stop as a failure.
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("mcp: serve: %w", err)
	}
	return nil
}

// ServeIO runs the guild MCP server over explicit byte streams instead
// of process stdio. It is the in-process continuation path for the
// daemon shim (daemon.ShimConfig.Fallback): when the daemon dies
// mid-session and the one re-dial fails, the shim replays the retained
// handshake frames at the head of r and hands the session here so the
// harness keeps a working server without respawning. Construction
// matches Serve exactly (stale-session cleanup, dynamic INSTRUCTIONS,
// per-process session identity); only the transport differs.
//
// EOF on r is the peer hanging up: a clean session end, not an error,
// mirroring DaemonHost.ServeSession's treatment of the same condition.
func ServeIO(ctx context.Context, r io.Reader, w io.Writer) error {
	log := newLogger()

	if err := session.CleanupStale(ctx); err != nil {
		log.Warn("stale session cleanup failed (non-fatal)", "err", err)
	}

	s, err := buildWithContext(ctx)
	if err != nil {
		return err
	}

	if err := s.Run(ctx, &sdkmcp.IOTransport{Reader: io.NopCloser(r), Writer: nopWriteCloser{w}}); err != nil {
		if ctx.Err() != nil || errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("mcp: serve: %w", err)
	}
	return nil
}

// nopWriteCloser adapts an io.Writer to the io.WriteCloser the SDK's
// IOTransport requires. Close is a no-op: the shim owns the lifetime
// of the underlying stdout stream.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// BuildForDocs returns a fully constructed MCP server for doc generation.
// cmd/docgen uses it to render docs/generated/mcp.md.
func BuildForDocs() (*sdkmcp.Server, error) {
	return build()
}

// build constructs the server with static-only INSTRUCTIONS. Used by
// tests and BuildForDocs so they don't touch the real lore DB.
func build() (*sdkmcp.Server, error) {
	return NewServer(Options{})
}

// buildWithContext constructs the server with dynamically baked
// INSTRUCTIONS: static contract + current project's principles (if the
// active project can be resolved at connect time). This is the production
// path called from Serve().
//
// Resolution order for the active project:
//  1. Per-PID session file (~/.guild/sessions/<pid>.json) — set by a
//     prior guild_session_start call in the same OS process; empty for a
//     freshly spawned MCP server.
//  2. GUILD_PROJECT env var — operator-level default.
//  3. CWD → git-toplevel auto-inference (same logic as guild_session_start
//     with no args). Covers the common "MCP host launched from project dir"
//     case where the agent hasn't yet called guild_session_start.
//
// If no project can be resolved via any path, INSTRUCTIONS = static
// contract only (graceful fallback per QUEST-57 spec). This is the
// expected state for multi-project hosts where the agent explicitly
// sets the project on first connect.
//
// Principle baking is best-effort: any DB open or query error falls back
// to static-only INSTRUCTIONS so a missing / corrupt lore.db never
// prevents the server from starting.
func buildWithContext(ctx context.Context) (*sdkmcp.Server, error) {
	logger := newLogger()

	instructions := resolveInstructions(ctx, logger)

	return NewServer(Options{Instructions: instructions})
}

// resolveInstructions tries to build the dynamic INSTRUCTIONS string
// for the stdio server: per-process session identity, process cwd.
// Falls back to staticInstructions on any resolution or DB error.
// Logs the outcome at debug level for observability (QUEST-57 measurement hook).
func resolveInstructions(ctx context.Context, logger *slog.Logger) string {
	return resolveInstructionsFor(ctx, logger, processSessionStore{}, "")
}

// resolveInstructionsFor is resolveInstructions generalized over the
// session identity and the inference cwd, so the daemon can build each
// connection's INSTRUCTIONS against the shim's session file and the
// shim's working directory instead of the daemon's own. dir == ""
// means the process working directory (the stdio default).
func resolveInstructionsFor(ctx context.Context, logger *slog.Logger, store SessionStore, dir string) string {
	// Step 1+2: check the session identity's file and GUILD_PROJECT env var.
	project, err := store.ResolveForMCP(ctx, "", os.Getenv(guildProjectEnv))
	if err != nil {
		// No project from session file or env: try cwd auto-inference (step 3).
		// inferProjectFromDir returns (projectID, viaWorktreeFallback, err); the
		// fallback flag is only material to handleSessionStart's narration, not
		// to INSTRUCTIONS building — discard it here.
		inferred, _, ierr := inferProjectFromDir(ctx, dir)
		if ierr != nil {
			// All resolution paths exhausted — static-only fallback.
			logger.Debug("mcp: instructions: no active project at connect; using static-only INSTRUCTIONS",
				"session_err", err, "infer_err", ierr)
			return staticInstructions
		}
		project = inferred
	}

	loreDB, dbErr := openLoreDB(ctx)
	if dbErr != nil {
		logger.Debug("mcp: instructions: could not open lore DB; using static-only INSTRUCTIONS",
			"project", project, "err", dbErr)
		return staticInstructions
	}
	defer func() { _ = loreDB.Close() }()

	instructions := buildInstructions(ctx, loreDB, project)

	// Measurement hook (QUEST-57): log built INSTRUCTIONS length and
	// principle count at debug level so operators can quantify actual
	// per-session cost post-ship. Enable with GUILD_MCP_LOG_LEVEL=debug.
	// Count list items in the appended oath-wall section only; locate it by
	// its header rather than slicing at len(staticInstructions), which no
	// longer holds once a disabled module's fragment is excluded (ADR-006
	// Phase 3 — the all-on contract is unchanged, but the contract may be
	// shorter than the embedded source when a module is off).
	principleCount := 0
	if i := strings.Index(instructions, "## Active Principles (oath wall)"); i >= 0 {
		principleCount = strings.Count(instructions[i:], "\n- ")
	}
	logger.Debug("mcp: instructions: built dynamic INSTRUCTIONS",
		"project", project,
		"instructions_bytes", len(instructions),
		"principle_count", principleCount,
	)

	return instructions
}
