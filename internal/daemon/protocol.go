package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"time"

	"github.com/mathomhaus/guild/internal/guildpath"
)

// socketFileName is the basename of the daemon's unix socket under
// ~/.guild/.
const socketFileName = "daemon.sock"

// socketFileMode is the mode bits applied to the unix socket after a
// successful listen. The parent ~/.guild dir is already 0700, but the
// socket is an IPC endpoint into the user's whole guild state, so it
// gets the same belt-and-suspenders treatment as the DB files.
const socketFileMode = 0o600

// preambleMaxBytes caps the first line read on an accepted connection.
// A legitimate preamble is a pid, a version string, and one filesystem
// path, well under a kilobyte. Anything larger is a misdialed client
// (or garbage) and the connection is dropped before it can balloon the
// read buffer.
const preambleMaxBytes = 16 * 1024

// preambleTimeout bounds how long an accepted connection may sit silent
// before its first line arrives. The discovery probe dials and closes
// without writing, so silent connections are routine; the deadline
// keeps them from pinning goroutines.
const preambleTimeout = 10 * time.Second

// SocketPath returns the daemon's unix socket location
// (~/.guild/daemon.sock), ensuring ~/.guild exists with 0700 first.
// The daemon and every dialer resolve the path through this function
// so the two sides can never disagree on where the socket lives.
func SocketPath() (string, error) {
	dir, err := guildpath.EnsureGuildDir()
	if err != nil {
		return "", fmt.Errorf("daemon: resolve socket path: %w", err)
	}
	return filepath.Join(dir, socketFileName), nil
}

// ShimPreamble is the identity line a stdio shim sends as the first
// ndjson line after dialing the daemon socket, wrapped on the wire as
// {"guild_shim":{...}}. The daemon impersonates this context for the
// whole session: PID keys the per-session state file (and becomes the
// presence/lease hook for later phases), CWD drives project
// auto-inference exactly as the stdio server's own cwd would, and
// Version lets the daemon log skew that slipped past the shim's probe.
type ShimPreamble struct {
	// Version is the shim binary's ldflags-stamped version.
	Version string `json:"version"`
	// CWD is the shim process's working directory.
	CWD string `json:"cwd"`
	// PID is the shim's process id.
	PID int `json:"pid"`
}

// Status is the one-line JSON response to a status-request preamble
// ({"guild_status_request":{}}). The daemon writes exactly one of these
// and closes the connection.
type Status struct {
	// PID is the daemon's process id.
	PID int `json:"pid"`
	// Version is the daemon's ldflags-stamped build version.
	Version string `json:"version"`
	// StartedAt is the daemon's startup time, serialized RFC 3339.
	StartedAt time.Time `json:"started_at"`
	// ActiveSessions counts the MCP sessions currently being served.
	// Status connections are not sessions and are never counted.
	ActiveSessions int `json:"active_sessions"`
	// EmbedderState mirrors meta.embedder_state from lore.db
	// ("enabled", "disabled", ...), or "unknown" when it cannot be
	// read.
	EmbedderState string `json:"embedder_state"`
	// Watch reports the watch -> staleness -> renewal pipeline's state:
	// whether it is enabled, currently watching, how many project roots,
	// and the cumulative events / signals / renewal quests since the
	// daemon started. Always present; a daemon with watch disabled
	// reports Enabled=false and zero counters.
	Watch WatchStatus `json:"watch"`
	// Sessions is the per-session presence detail from the registry
	// snapshot: one entry per live shim connection (ADR-005 Phase 3),
	// ordered by session id. Empty (or omitted on the wire) when no
	// sessions are live or the daemon was built without a registry. The
	// active count above is the authoritative connection count; this slice
	// is the registry's view of the same sessions plus the project, ages,
	// and held quest ids each one carries.
	Sessions []SessionStatus `json:"sessions,omitempty"`
	// LeasesReaped is the lifetime count of zombie claims the reaper has
	// forfeited since the daemon started (ADR-005 Phase 3). Zero when no
	// reaper is wired or none have lapsed yet.
	LeasesReaped int64 `json:"leases_reaped"`
}

// SessionStatus is the status-line view of one live session in the
// registry, mirroring [Session] across the socket so `guild daemon status`
// can render per-session presence detail. Keys are stable and append-only.
type SessionStatus struct {
	// ID is the daemon's per-session identity (the shim's pid as a string).
	ID string `json:"id"`
	// Project is the active project the session resolved to, empty when it
	// has not bound one yet.
	Project string `json:"project,omitempty"`
	// ConnectedAt is when the session attached, serialized RFC 3339.
	ConnectedAt time.Time `json:"connected_at"`
	// LastHeartbeat is when the heartbeat tick last refreshed this session's
	// leases (or its connect time before the first tick), serialized RFC
	// 3339. The readout surfaces it as "last seen".
	LastHeartbeat time.Time `json:"last_heartbeat"`
	// HeldQuests is the quest ids whose leases this session holds, per the
	// registry's last in-memory reconciliation. Empty for a session holding
	// no leases.
	HeldQuests []string `json:"held_quests,omitempty"`
}

// WatchStatus is the status-line view of the daemon's watch pipeline. The
// keys are stable and append-only, mirroring [PipelineStatus] across the
// socket so `guild daemon status` can render watcher health.
type WatchStatus struct {
	// Enabled is false when no watcher was ever started (config or env
	// opt-out); every counter below is then zero.
	Enabled bool `json:"enabled"`
	// Watching is true while a watcher is live. False with Enabled true
	// is the visible "degraded to query-time staleness" signal.
	Watching bool `json:"watching"`
	// ProjectsWatched is the number of project roots the live watcher
	// covers.
	ProjectsWatched int `json:"projects_watched"`
	// EventsSeen counts debounced file/git events consumed since start.
	EventsSeen int64 `json:"events_seen"`
	// SignalsRecorded counts staleness signal rows written since start.
	SignalsRecorded int64 `json:"signals_recorded"`
	// QuestsPosted counts renewal quests minted since start.
	QuestsPosted int64 `json:"quests_posted"`
	// LastError is the most recent watcher build or event error, empty
	// when none has occurred.
	LastError string `json:"last_error,omitempty"`
}

// preamble is the wire shape of the first ndjson line on every
// accepted connection. Exactly one of the three fields must be present:
//
//	{"guild_shim":{"version":"v0.3.2","cwd":"/path/to/project","pid":12345}}
//	{"guild_status_request":{}}
//	{"guild_exec":{"tool":"quest_post","version":"v0.3.2","cwd":"/path","pid":12345}}
//
// omitempty keeps the marshaled form (a client's outbound preamble)
// down to exactly the one key that is present.
type preamble struct {
	Shim          *ShimPreamble `json:"guild_shim,omitempty"`
	StatusRequest *struct{}     `json:"guild_status_request,omitempty"`
	Exec          *ExecPreamble `json:"guild_exec,omitempty"`
}

// readPreamble consumes the first newline-terminated line from br and
// parses it into a validated preamble. The bufio.Reader may buffer
// bytes beyond the line; callers that hand the stream onward must keep
// reading through br (see sessionConn).
func readPreamble(br *bufio.Reader) (*preamble, error) {
	line, err := readPreambleLine(br)
	if err != nil {
		return nil, err
	}

	p := &preamble{}
	if err := json.Unmarshal(line, p); err != nil {
		return nil, fmt.Errorf("daemon: parse preamble: %w", err)
	}
	variants := 0
	for _, present := range []bool{p.Shim != nil, p.StatusRequest != nil, p.Exec != nil} {
		if present {
			variants++
		}
	}
	if variants != 1 {
		return nil, fmt.Errorf("daemon: preamble must carry exactly one of guild_shim, guild_status_request, guild_exec (got %d)", variants)
	}
	switch {
	case p.Shim != nil:
		if p.Shim.PID <= 0 {
			return nil, fmt.Errorf("daemon: shim preamble pid %d is not a valid process id", p.Shim.PID)
		}
		if p.Shim.CWD == "" {
			// An empty cwd would silently fall back to the daemon's own
			// working directory during project inference: exactly the
			// context bleed the preamble exists to prevent.
			return nil, errors.New("daemon: shim preamble has empty cwd")
		}
	case p.Exec != nil:
		if p.Exec.Tool == "" {
			return nil, errors.New("daemon: exec preamble has empty tool")
		}
		if p.Exec.CWD == "" {
			// Same context-bleed rationale as the shim preamble: project
			// resolution must anchor on the CLIENT's cwd, never the
			// daemon's.
			return nil, errors.New("daemon: exec preamble has empty cwd")
		}
		if p.Exec.PID <= 0 {
			return nil, fmt.Errorf("daemon: exec preamble pid %d is not a valid process id", p.Exec.PID)
		}
	}
	return p, nil
}

// readPreambleLine reads one '\n'-terminated line from br, enforcing
// preambleMaxBytes. ReadSlice is used (not ReadString) so the size cap
// applies per-fragment instead of letting a hostile peer grow one
// unbounded allocation.
func readPreambleLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		frag, err := br.ReadSlice('\n')
		line = append(line, frag...)
		if len(line) > preambleMaxBytes {
			return nil, fmt.Errorf("daemon: preamble exceeds %d bytes", preambleMaxBytes)
		}
		if err == nil {
			return line, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return nil, fmt.Errorf("daemon: read preamble: %w", err)
	}
}

// sessionConn splices the preamble reader's buffered remainder back
// onto the connection: reads drain through the bufio.Reader (which may
// hold JSON-RPC bytes that arrived in the same packet as the
// preamble), writes and closes go straight to the underlying conn.
type sessionConn struct {
	r    *bufio.Reader
	conn net.Conn
}

// Compile-time check: sessionConn is the io.ReadWriteCloser handed to
// SessionHandler implementations.
var _ io.ReadWriteCloser = (*sessionConn)(nil)

func (c *sessionConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *sessionConn) Write(p []byte) (int, error) { return c.conn.Write(p) }
func (c *sessionConn) Close() error                { return c.conn.Close() }
