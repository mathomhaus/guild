package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/daemon"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/session"
	"github.com/mathomhaus/guild/internal/sleep"
)

// DaemonHost is the per-process state behind `guild daemon run`: the
// one provider bundle every connection's MCP server shares, so the
// embedder, hints engine, and auto-backfill trigger are constructed
// once per daemon process instead of once per attached harness.
//
// Each accepted connection still gets its OWN *sdkmcp.Server (tool
// registration is cheap and in-memory) with its own session identity
// (the shim's pid) and its own INSTRUCTIONS (resolved against the
// shim's cwd). Only the provider bundle is shared: exactly the
// multi-session contract Options.Providers documents.
type DaemonHost struct {
	providers *Providers
	logger    *slog.Logger

	// registry is the daemon's session registry + lease-heartbeat tick
	// (ADR-005 Phase 3). ServeSession registers each connection on it and
	// wires the connection's per-session lease acquirer; the daemon Server
	// drives registry.Run. leaseTTL is the resolved lease validity window
	// the per-session acquirers and the heartbeat seam both bind, so a tick
	// and an accept stamp the same expiry math.
	registry *daemon.Registry
	leaseTTL time.Duration

	// reaper is the daemon's lease reaper (ADR-005 Phase 3). Its sweep
	// consults registry.IsLive (a still-registered session is a missed
	// heartbeat, not a crash) and forfeits the zombie claim behind any
	// genuinely lapsed lease. The daemon Server drives reaper.Run.
	reaper *daemon.Reaper
}

// Compile-time check: ServeSession satisfies the daemon package's
// session-handler seam without an adapter.
var _ daemon.SessionHandler = (*DaemonHost)(nil).ServeSession

// NewDaemonHost constructs the shared bundle for one daemon process with
// the default lease timings (built-in TTL, heartbeat interval, and reap
// interval). Call once per `guild daemon run`; pass ServeSession and
// EmbedderState into daemon.Config, Registry() into daemon.Config.Registry,
// and Reaper() into daemon.Config.Reaper.
func NewDaemonHost() *DaemonHost {
	return NewDaemonHostWithLeases(defaultDaemonLeaseTTL, defaultDaemonHeartbeatInterval, defaultDaemonReapInterval)
}

// NewDaemonHostWithLeases constructs the shared bundle with explicit lease
// timings resolved from config. A non-positive value falls back to the
// built-in default so a misconfigured daemon still heartbeats and reaps.
func NewDaemonHostWithLeases(leaseTTL, heartbeatInterval, reapInterval time.Duration) *DaemonHost {
	if leaseTTL <= 0 {
		leaseTTL = defaultDaemonLeaseTTL
	}
	if heartbeatInterval <= 0 {
		heartbeatInterval = defaultDaemonHeartbeatInterval
	}
	if reapInterval <= 0 {
		reapInterval = defaultDaemonReapInterval
	}
	h := &DaemonHost{
		providers: NewProviders(),
		logger:    newLogger(),
		leaseTTL:  leaseTTL,
	}
	h.registry = daemon.NewRegistry(daemon.RegistryConfig{
		HeartbeatInterval: heartbeatInterval,
		LeaseTTL:          leaseTTL,
		Renew:             h.renewLeasesSeam(),
		BootGrace:         h.bootGraceSeam(),
		Logger:            h.logger,
	})
	h.reaper = daemon.NewReaper(daemon.ReaperConfig{
		Interval: reapInterval,
		Reap:     h.reapSeam(),
		Logger:   h.logger,
	})
	return h
}

// Registry exposes the host's session registry so cmd/guild can wire it
// into daemon.Config.Registry. The daemon Server drives Registry.Run for
// the daemon's lifetime.
func (h *DaemonHost) Registry() *daemon.Registry { return h.registry }

// Compile-time check: *daemon.Registry satisfies PresenceSource, so a
// daemon session can hand its registry to guild_session_start without an
// adapter.
var _ PresenceSource = (*daemon.Registry)(nil)

// presenceSourceOrNil returns reg as a PresenceSource, or a true nil
// interface when reg is a nil pointer. Assigning a typed-nil *Registry
// straight into the interface field would make the nil check in
// handleSessionStart see a non-nil PresenceSource and then call Presence on
// a nil receiver (which reads a nil map). Returning an untyped nil keeps the
// no-registry path on the byte-identical no-presence branch.
func presenceSourceOrNil(reg *daemon.Registry) PresenceSource {
	if reg == nil {
		return nil
	}
	return reg
}

// Reaper exposes the host's lease reaper so cmd/guild can wire it into
// daemon.Config.Reaper. The daemon Server drives Reaper.Run for the
// daemon's lifetime.
func (h *DaemonHost) Reaper() *daemon.Reaper { return h.reaper }

// Logger exposes the env-configured stderr logger (GUILD_MCP_LOG_FORMAT
// / GUILD_MCP_LOG_LEVEL) so cmd/guild can hand the daemon listener the
// same logger the per-session servers use.
func (h *DaemonHost) Logger() *slog.Logger { return h.logger }

// QuestEmbedSource exposes the host's shared quest-side embed provider
// as an opaque command.Deps.Embed value, so daemon-routed CLI verbs
// (the JSON-exec dispatch in internal/cli) search through the SAME
// embedder every MCP session uses instead of wiring a second one.
// Returns a true nil interface when the provider is absent so callers'
// nil checks behave.
func (h *DaemonHost) QuestEmbedSource() any {
	if h.providers == nil || h.providers.questEmbed == nil {
		return nil
	}
	return h.providers.questEmbed
}

// LoreEmbedSource is the lore-side sibling of QuestEmbedSource.
func (h *DaemonHost) LoreEmbedSource() any {
	if h.providers == nil || h.providers.embed == nil {
		return nil
	}
	return h.providers.embed
}

// ServeSession serves one complete MCP session over conn (the raw
// newline-delimited JSON-RPC stream that follows the shim preamble)
// and blocks until the peer disconnects or ctx is cancelled. It is the
// production daemon.SessionHandler.
//
// Identity impersonation per the daemon contract: the session file is
// keyed by the SHIM's pid (so a reconnecting shim resumes its own
// active project, and concurrent shims never clobber each other), and
// INSTRUCTIONS plus project auto-inference anchor on the SHIM's cwd.
func (h *DaemonHost) ServeSession(ctx context.Context, shim daemon.ShimPreamble, conn io.ReadWriteCloser) error {
	store, err := daemonSessionStore(shim.PID)
	if err != nil {
		return err
	}

	instructions := resolveInstructionsFor(ctx, h.logger, store, shim.CWD)

	// Session identity (ADR-005 Phase 3): the registry entry and every
	// lease this session takes are keyed by the shim's pid, so the
	// heartbeat tick refreshes exactly the rows this session's accepts
	// wrote. Register on attach and unregister on detach (defer), so the
	// registry snapshot tracks live connections and the tick stops
	// heartbeating the moment the agent process (and its shim) dies. The
	// resolved project is best-effort at attach time; the lease rows are
	// the source of truth for what the session holds.
	sessionID := daemonSessionID(shim.PID)
	var lease any
	if h.registry != nil {
		proj := h.resolveSessionProject(ctx, store)
		h.registry.Register(sessionID, proj, time.Now().UTC())
		// The acquirer owns a long-lived quest.db handle; detachSession
		// closes it (and runs the final renewal + unregister) when the
		// connection drops. A nil acquirer (db-open failure on attach)
		// degrades the session to the no-lease path; detach handles nil.
		acquirer := h.newLeaseAcquirer(ctx, sessionID)
		defer h.detachSession(ctx, sessionID, acquirer)
		// Threaded onto Deps.Lease as the documented `any` seam. A nil
		// *DBLeaseAcquirer must become a true nil interface so the
		// leaseFromDeps type assertion sees absence, not a typed-nil that
		// would panic on use; only wire a non-nil acquirer.
		if acquirer != nil {
			lease = acquirer
		}
	}

	srv, err := NewServer(Options{
		Instructions: instructions,
		Sessions:     store,
		Providers:    h.providers,
		CWD:          shim.CWD,
		Lease:        lease,
		// Presence (ADR-005 Phase 3): the registry is this session's source
		// for "who else is here", so guild_session_start can append the
		// daemon-only "N agents active" line. A nil registry (a host wiring
		// gap) leaves Presence nil, which is the byte-identical no-presence
		// path; presenceSourceOrNil keeps a typed-nil registry from becoming
		// a non-nil interface that would read a nil map.
		Presence: presenceSourceOrNil(h.registry),
	})
	if err != nil {
		return fmt.Errorf("mcp: daemon session (shim pid %d): %w", shim.PID, err)
	}

	// IOTransport speaks the same ndjson framing as StdioTransport, so
	// the byte stream after the preamble is exactly what the stdio
	// server would have read on stdin, with no re-framing layer.
	if err := srv.Run(ctx, &sdkmcp.IOTransport{Reader: conn, Writer: conn}); err != nil {
		// Cancelled ctx is daemon shutdown; EOF is the peer hanging up.
		// Both are clean ends of a session, not failures.
		if ctx.Err() != nil || errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("mcp: daemon session (shim pid %d): %w", shim.PID, err)
	}
	return nil
}

// EmbedderState reports meta.embedder_state from lore.db for the
// daemon's status line ("enabled", "disabled", ...), or "unknown" when
// the value cannot be read. It deliberately reads the durable meta row
// rather than the provider cache: the status line should reflect what
// the next lore call will resolve, not what the last one did.
func (h *DaemonHost) EmbedderState(ctx context.Context) string {
	db, err := openLoreDB(ctx)
	if err != nil {
		return "unknown"
	}
	defer func() { _ = db.Close() }()

	state, err := readMetaValue(ctx, db, "embedder_state")
	if err != nil || state == "" {
		return "unknown"
	}
	return state
}

// ─────────────── idle dream-pass wiring (ADR-005 Phase 2) ───────────
// Everything below is the host's half of the daemon idle scheduler: it
// turns the host's shared db handles and embed provider into a
// daemon.PassFunc the scheduler can fire. The scheduler itself (WHEN to
// dream) lives in internal/daemon; this seam supplies WHAT one pass
// runs by handing the sleep runner a per-pass context. Keeping it here
// means internal/daemon never imports internal/sleep or internal/mcp.

// sleepPassMaxAutoOps caps how many auto-policy mutations one idle pass
// may apply across all steps; sleepPassMaxQuestPosts caps the quests it
// may post; sleepPassMaxRenewalPosts caps lore-renewal quests per pass.
// They are conservative because an idle pass runs unattended: a small
// cap means a backlog drains over several passes rather than flooding
// the board or the journal in one. The steps enforce them; the runner
// only threads them through.
const (
	sleepPassMaxAutoOps      = 50
	sleepPassMaxQuestPosts   = 10
	sleepPassMaxRenewalPosts = 5
)

// SleepPassRunner returns the daemon.PassFunc the idle scheduler fires.
// Each call opens fresh lore.db and quest.db handles (the same per-call
// open discipline every MCP tool uses; WAL mode makes this cheap),
// resolves the shared embedder, builds a daemon-idle PassContext, and
// runs the registered steps under the scheduler's wall budget. With the
// step registry empty (this campaign's step quests register their own),
// a pass journals a pass row with zero steps and returns cleanly.
//
// The returned func honors ctx cancellation: the runner threads it into
// every step, so a waking session that preempts the pass cancels its
// work and the runner still stamps the pass row ended.
func (h *DaemonHost) SleepPassRunner() daemon.PassFunc {
	return func(ctx context.Context, budget time.Duration) (daemon.PassOutcome, error) {
		loreDB, err := openLoreDB(ctx)
		if err != nil {
			return daemon.PassOutcome{}, fmt.Errorf("mcp: sleep pass: open lore db: %w", err)
		}
		defer func() { _ = loreDB.Close() }()

		questDB, err := openQuestDB(ctx)
		if err != nil {
			return daemon.PassOutcome{}, fmt.Errorf("mcp: sleep pass: open quest db: %w", err)
		}
		defer func() { _ = questDB.Close() }()

		var embed *lore.EmbedDeps
		if h.providers != nil && h.providers.embed != nil {
			// nil deps is the documented BM25-only fallback; the embed
			// step tolerates it as a clean no-op.
			embed = h.providers.embed.ResolveEmbedDeps(ctx)
		}

		pc := &sleep.PassContext{
			LoreDB:  loreDB,
			QuestDB: questDB,
			Embed:   embed,
			Logger:  h.logger,
			Trigger: sleep.TriggerDaemonIdle,
			Caps: sleep.Caps{
				MaxAutoOps:      sleepPassMaxAutoOps,
				MaxQuestPosts:   sleepPassMaxQuestPosts,
				MaxRenewalPosts: sleepPassMaxRenewalPosts,
			},
		}

		res, err := sleep.Run(ctx, pc, sleep.Steps(), budget)
		if err != nil {
			return daemon.PassOutcome{}, fmt.Errorf("mcp: sleep pass: %w", err)
		}
		return daemon.PassOutcome{Partial: res.Partial, Steps: len(res.Steps)}, nil
	}
}

// daemonSessionStore builds the per-connection session identity: the
// standard ~/.guild/sessions layout keyed by the SHIM's pid, mirroring
// what the shim's own in-process MCP server would have used. $HOME is
// resolved per connection (not cached) for the same reason
// session.defaultManager resolves it lazily.
func daemonSessionStore(pid int) (session.Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return session.Manager{}, fmt.Errorf("mcp: daemon session store: resolve home: %w", err)
	}
	return session.Manager{BaseDir: filepath.Join(home, ".guild"), PID: pid}, nil
}
