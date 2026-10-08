package daemon

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// This file is the daemon's session registry and lease-heartbeat tick
// (ADR-005 Part 1, "Why a daemon" item 3; phasing table Phase 3). It is
// the liveness signal that turns a bare claim into a heartbeated one: a
// daemon-mediated accept writes a task_leases row (the lease-schema
// quest), and as long as the session that took the work keeps its
// unix-socket connection alive, the daemon refreshes that session's
// leases on a tick. When the agent process dies its shim dies, the
// connection drops, the session unregisters, the heartbeat stops, and the
// lease lapses one TTL later so a reaper can forfeit the stale claim.
//
// The shim stays a dumb pipe (ADR-005 open question 3 ruling: the proxy
// forwards raw stdio JSON-RPC frames). The heartbeat is carried by the
// connection's liveness, not by new protocol frames: no side-channel, no
// shim changes, the byte-identical dumb-pipe contract intact.
//
// internal/daemon is a deliberate leaf (see the package comment in
// discovery.go) and must not import internal/quest, where the lease SQL
// lives. So the registry never touches a database directly: the host
// (internal/mcp's DaemonHost) supplies the RenewFunc and BootGraceFunc
// seams that open quest.db and call the lease primitives, exactly the
// split the idle scheduler's PassFunc and the watch pipeline's ProcessFunc
// already use.
//
// Hard invariants from ADR-005 carried here:
//
//   - Zero impact on the no-daemon path: the registry and its tick live
//     ONLY inside a running daemon. Without a daemon there are no sessions
//     to register, no ticks, and no lease rows exist in the first place
//     (leases are written only by daemon-mediated accepts). Behavior is
//     exactly today's: no leases, no false forfeits.
//   - A renewal failure (SQLITE_BUSY, a transient db error) is logged and
//     retried on the next tick, never fatal to the session or the daemon.
//   - The snapshot is in-memory only (no db round-trip) so presence and
//     daemon-status reads stay cheap.

// defaultHeartbeatInterval is the registry's tick cadence when none is
// configured. It mirrors the lease layer's built-in heartbeat default and
// is deliberately well below the lease TTL so several missed ticks never
// expire a live session. The daemon resolves the configured value from
// config.DaemonConfig; this constant only floors the zero case.
const defaultHeartbeatInterval = 30 * time.Second

// defaultLeaseTTL is the lease validity window the registry stamps on each
// renewal when none is configured. Generous relative to the heartbeat
// interval on purpose (ADR-005 Phase 3 liveness margin).
const defaultLeaseTTL = 10 * time.Minute

// minHeartbeatInterval floors the tick cadence so a tiny configured
// interval (or a fast-clock test) cannot spin the renewal loop.
const minHeartbeatInterval = time.Second

// Session is one live shim connection's registry entry: the daemon's
// per-session identity, the project it resolved to, and its liveness
// timestamps. Snapshot returns copies of these for presence and
// daemon-status consumers (the reaper reads expiry from the lease rows,
// not from here).
type Session struct {
	// ID is the daemon's per-session identity (the shim's pid as a
	// string), the same value the session's leases carry in
	// task_leases.session_id. The renewal tick passes it to RenewFunc.
	ID string
	// Project is the active project the session resolved to, or empty
	// when the session has not bound one yet. Carried for presence
	// readouts; the lease rows are the source of truth for what a session
	// holds.
	Project string
	// ConnectedAt is when the session attached.
	ConnectedAt time.Time
	// LastHeartbeat is when the renewal tick last refreshed this session's
	// leases, or the connect time before the first tick. The presence
	// readout surfaces it as "last seen".
	LastHeartbeat time.Time
	// HeldQuests is the set of quest ids whose leases this session holds, as
	// last reconciled by the heartbeat tick (and seeded on a session's first
	// accept). It is in-memory only: the lease rows in the db are the source
	// of truth, but the presence readout and the daemon-status detail read
	// this copy so neither path takes a db round-trip. Ordered for stable
	// output. Empty for a session holding no leases.
	HeldQuests []string
}

// OnLease reports whether the session currently holds at least one lease,
// per the last reconciled in-memory view.
func (s Session) OnLease() bool { return len(s.HeldQuests) > 0 }

// RenewFunc refreshes every lease the identified session holds: heartbeat
// to now, expiry to now+TTL. The host wires it over quest.RenewLeasesForSession
// with a fresh quest.db handle, so internal/daemon never imports
// internal/quest. The registry calls it once per live session per tick and
// once on a session's clean detach (a final refresh before the connection
// drops, narrowing the window an in-flight reaper could observe). It must
// honor ctx cancellation (daemon shutdown). An error is logged and the
// next tick retries; a single failed renewal only shortens the lease by
// one interval, well inside the TTL margin.
type RenewFunc func(ctx context.Context, sessionID string) error

// BootGraceFunc renews EVERY task_leases row once, before any reaper could
// run. The host wires it over quest.RenewAllLeases. The registry calls it
// exactly once at the start of Run so a lease left behind by a crashed
// daemon gets one grace window in which its session can re-dial and
// re-establish a heartbeat (ADR-005 hard invariant: a daemon crash
// mid-session loses nothing). An error is logged and the daemon keeps
// serving; the generous TTL still bounds the false-forfeit window.
type BootGraceFunc func(ctx context.Context) error

// RegistryConfig parameterizes a Registry. The zero value is a usable
// registry whose tick never renews (no RenewFunc), so a host wiring gap
// degrades to "no heartbeats" instead of crashing the daemon.
type RegistryConfig struct {
	// HeartbeatInterval is the tick cadence. Non-positive uses
	// defaultHeartbeatInterval; it is floored at minHeartbeatInterval.
	HeartbeatInterval time.Duration
	// LeaseTTL is carried for diagnostics/logging; the host's RenewFunc
	// and BootGraceFunc own the actual now+TTL math (they bind the same
	// TTL the daemon resolved from config). Non-positive uses
	// defaultLeaseTTL for the logged value.
	LeaseTTL time.Duration
	// Renew refreshes one session's leases. Nil disables the tick's
	// renewal (the registry still tracks sessions for presence), so a
	// wiring gap is inert.
	Renew RenewFunc
	// BootGrace renews all pre-existing leases once on Run start. Nil
	// skips the grace pass.
	BootGrace BootGraceFunc
	// Logger receives tick/boot/degradation diagnostics. Nil falls back
	// to slog.Default(); never logs to stdout.
	Logger *slog.Logger
	// clock is injected by tests. nil uses the real clock (the same
	// scheduler clock seam in sleep_scheduler.go).
	clock clock
}

// Registry is the daemon's in-memory map of live shim connections plus the
// goroutine that heartbeats their leases. It is safe for concurrent use:
// Register / Unregister / Touch / Snapshot run on connection goroutines
// while Run owns the tick. Construct with NewRegistry, drive with Run.
type Registry struct {
	cfg      RegistryConfig
	logger   *slog.Logger
	clk      clock
	interval time.Duration
	ttl      time.Duration

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewRegistry validates cfg and returns a runnable Registry.
func NewRegistry(cfg RegistryConfig) *Registry {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clk := cfg.clock
	if clk == nil {
		clk = realClock{}
	}
	interval := cfg.HeartbeatInterval
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	if interval < minHeartbeatInterval {
		interval = minHeartbeatInterval
	}
	ttl := cfg.LeaseTTL
	if ttl <= 0 {
		ttl = defaultLeaseTTL
	}
	return &Registry{
		cfg:      cfg,
		logger:   logger,
		clk:      clk,
		interval: interval,
		ttl:      ttl,
		sessions: make(map[string]*Session),
	}
}

// Register records a live session under sessionID and returns it. A
// repeated Register for the same id (a session that re-dialed under the
// same shim pid) refreshes its project and connect time rather than
// duplicating it: one entry per session id, mirroring the one-lease-per-quest
// invariant the lease schema enforces. An empty sessionID is ignored
// (returns nil): the daemon never registers a session it cannot heartbeat.
func (r *Registry) Register(sessionID, projectID string, connectedAt time.Time) *Session {
	if sessionID == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &Session{
		ID:            sessionID,
		Project:       projectID,
		ConnectedAt:   connectedAt,
		LastHeartbeat: connectedAt,
	}
	r.sessions[sessionID] = s
	return s
}

// Unregister removes the session under sessionID. Called on a connection
// close or error. Removing a session that was never registered (or already
// removed) is a no-op. It does NOT touch the session's lease rows: a clean
// detach renews-then-drops via the host's release path, and a crash relies
// on expiry; the registry only stops heartbeating.
func (r *Registry) Unregister(sessionID string) {
	if sessionID == "" {
		return
	}
	r.mu.Lock()
	delete(r.sessions, sessionID)
	r.mu.Unlock()
}

// Touch records activity on a session: it bumps LastHeartbeat to now so a
// presence readout reflects a session that is actively writing even
// between ticks. The lease itself is refreshed by the mutating handler's
// activity-renewal seam (host side); this only keeps the in-memory
// "last seen" current. A Touch for an unregistered session is a no-op.
func (r *Registry) Touch(sessionID string) {
	if sessionID == "" {
		return
	}
	r.mu.Lock()
	if s, ok := r.sessions[sessionID]; ok {
		s.LastHeartbeat = r.clk.Now()
	}
	r.mu.Unlock()
}

// SetHeldQuests records the set of quest ids whose leases sessionID holds,
// for the in-memory presence and daemon-status views. The host calls it
// from the heartbeat renewal seam (which already opens a per-session db
// handle, so the reconciliation adds no extra round-trip to a hot path) and
// on a session's first accept (so presence reflects a just-taken claim
// before the next tick). It copies and sorts ids so the caller cannot
// mutate registry state through the slice and the readout is deterministic.
// A SetHeldQuests for an unregistered session is a no-op: a detached
// session has no presence entry to annotate.
func (r *Registry) SetHeldQuests(sessionID string, questIDs []string) {
	if sessionID == "" {
		return
	}
	held := make([]string, len(questIDs))
	copy(held, questIDs)
	sort.Strings(held)
	r.mu.Lock()
	if s, ok := r.sessions[sessionID]; ok {
		s.HeldQuests = held
	}
	r.mu.Unlock()
}

// IsLive reports whether sessionID is currently registered (a live shim
// connection). The lease reaper consults it before forfeiting an expired
// lease: a still-registered session is a missed heartbeat under load, not a
// crash, so the reaper leaves that lease for the next tick to renew instead
// of forfeiting a live agent's work. An empty id is never live.
func (r *Registry) IsLive(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	r.mu.Lock()
	_, ok := r.sessions[sessionID]
	r.mu.Unlock()
	return ok
}

// Count returns the number of live sessions. It is the cheap active-count
// seam the presence consumer reads without taking a full snapshot.
func (r *Registry) Count() int {
	r.mu.Lock()
	n := len(r.sessions)
	r.mu.Unlock()
	return n
}

// Presence is the in-memory snapshot the session-start presence line reads:
// how many sessions are live and how many of them currently hold at least
// one lease, per the last reconciled view. It takes the lock once and reads
// only in-memory state (no db round-trip), so the session-start hot path
// stays cheap. The on-lease count never exceeds the session count.
type Presence struct {
	// Sessions is the number of live shim connections.
	Sessions int
	// OnLease is how many of those sessions currently hold at least one
	// lease, per the heartbeat's last in-memory reconciliation.
	OnLease int
}

// Presence returns the active-session count and the on-lease session count
// in a single locked read for the session-start presence line. In-memory
// only: it never touches the db.
func (r *Registry) Presence() Presence {
	r.mu.Lock()
	p := Presence{Sessions: len(r.sessions)}
	for _, s := range r.sessions {
		if len(s.HeldQuests) > 0 {
			p.OnLease++
		}
	}
	r.mu.Unlock()
	return p
}

// Snapshot returns a copy of every live session, ordered by session id for
// deterministic output. It is in-memory only (no db round-trip) so
// presence and daemon-status reads stay cheap, and it returns copies so a
// caller can never mutate registry state through the result.
func (r *Registry) Snapshot() []Session {
	r.mu.Lock()
	out := make([]Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, *s)
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// liveSessionIDs returns the ids of every live session, taken under the
// lock so the tick iterates a stable list without holding the lock across
// the renewal db calls.
func (r *Registry) liveSessionIDs() []string {
	r.mu.Lock()
	ids := make([]string, 0, len(r.sessions))
	for id := range r.sessions {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	return ids
}

// Run drives the heartbeat tick until ctx is cancelled (daemon shutdown).
// On start it runs the boot-grace pass once (renew all pre-existing leases
// before any reaper could observe them), then ticks: each tick renews
// every live session's leases and advances its LastHeartbeat. Run blocks;
// the daemon starts it in its own goroutine alongside the scheduler and
// the watch pipeline.
//
// A nil RenewFunc (host wiring gap) makes the tick track LastHeartbeat
// without touching the db, so the daemon still serves and presence still
// answers; it just never refreshes a lease. The loop always runs so the
// daemon's WaitGroup bookkeeping completes cleanly on shutdown.
func (r *Registry) Run(ctx context.Context) {
	r.bootGrace(ctx)

	r.logger.Info("daemon: session registry started",
		"heartbeat", r.interval.String(),
		"lease_ttl", r.ttl.String(),
	)

	tk := r.clk.NewTicker(r.interval)
	defer tk.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C():
			r.heartbeat(ctx)
		}
	}
}

// bootGrace renews every pre-existing lease once. A nil BootGraceFunc
// skips it; an error is logged and the daemon keeps serving (the generous
// TTL still bounds the false-forfeit window).
func (r *Registry) bootGrace(ctx context.Context) {
	if r.cfg.BootGrace == nil {
		return
	}
	if err := r.cfg.BootGrace(ctx); err != nil {
		// Cancellation is daemon shutdown racing startup, not a failure.
		if ctx.Err() != nil {
			return
		}
		r.logger.Warn("daemon: session registry boot grace failed; relying on lease TTL",
			"err", err.Error())
		return
	}
	r.logger.Info("daemon: session registry boot grace renewed pre-existing leases")
}

// heartbeat renews every live session's leases and advances its
// LastHeartbeat. It snapshots the session ids under the lock, then calls
// RenewFunc per id outside the lock (the db call must not serialize
// Register / Unregister). A per-session renewal failure is logged and
// skipped; the next tick retries. LastHeartbeat advances only when the
// session is still registered at write time, so a session that detached
// mid-tick is not resurrected.
func (r *Registry) heartbeat(ctx context.Context) {
	ids := r.liveSessionIDs()
	if len(ids) == 0 {
		return
	}
	now := r.clk.Now()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if r.cfg.Renew != nil {
			if err := r.cfg.Renew(ctx, id); err != nil {
				if ctx.Err() != nil {
					return
				}
				r.logger.Warn("daemon: session lease heartbeat failed; will retry next tick",
					"session", id, "err", err.Error())
				continue
			}
		}
		r.mu.Lock()
		if s, ok := r.sessions[id]; ok {
			s.LastHeartbeat = now
		}
		r.mu.Unlock()
	}
}
