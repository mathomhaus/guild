package daemon

import (
	"fmt"
	"strings"
	"time"
)

// This file is the daemon-status presence detail (ADR-005 Part 1, "Why a
// daemon" item 3; phasing table Phase 3). It turns the in-memory session
// registry snapshot and the lease reaper's lifetime counter into the
// per-session readout `guild daemon status` renders: one line per live
// session (short id, project, connected age, last-heartbeat age, held quest
// ids) plus the lifetime reap count.
//
// It lives in its own file (not server.go, not lifecycle.go) so the presence
// readout stays separable from the listener and lifecycle surfaces it sits
// between. The two snapshot mappers feed the status wire shape; the line
// formatters are pure (no Server, no clock) so cmd/guild can render the
// detail block and a test can pin the exact strings without standing up a
// daemon.

// sessionStatuses maps the registry's live-session snapshot onto the status
// wire shape. A nil Registry (a Phase 1 daemon, or a wiring gap) reports no
// per-session detail, so the status line degrades to the active-count
// summary alone. In-memory only: Snapshot takes no db round-trip.
func (s *Server) sessionStatuses() []SessionStatus {
	if s.cfg.Registry == nil {
		return nil
	}
	snap := s.cfg.Registry.Snapshot()
	if len(snap) == 0 {
		return nil
	}
	out := make([]SessionStatus, 0, len(snap))
	for _, sess := range snap {
		out = append(out, SessionStatus{
			ID:            sess.ID,
			Project:       sess.Project,
			ConnectedAt:   sess.ConnectedAt,
			LastHeartbeat: sess.LastHeartbeat,
			HeldQuests:    sess.HeldQuests,
		})
	}
	return out
}

// leasesReaped reports the reaper's lifetime forfeit count for the status
// line. A nil Reaper (a Phase 1 daemon, or a wiring gap) reports zero.
func (s *Server) leasesReaped() int64 {
	if s.cfg.Reaper == nil {
		return 0
	}
	return s.cfg.Reaper.TotalForfeited()
}

// FormatLeasesLine renders the lease reaper's lifetime forfeit counter line
// for `guild daemon status`: how many crashed agents' claims the daemon has
// returned to the board since it started.
func FormatLeasesLine(reaped int64) string {
	return fmt.Sprintf("  leases: reaped=%d", reaped)
}

// FormatSessionLines renders one line per live session for `guild daemon
// status`: short session id, project, connected age, last-heartbeat age, and
// held quest ids, all relative to now. An empty slice (no live sessions)
// renders a single "sessions: none active" line so the readout never goes
// blank. Pure: callers pass now so the output is deterministic in tests.
func FormatSessionLines(sessions []SessionStatus, now time.Time) []string {
	if len(sessions) == 0 {
		return []string{"  sessions: none active"}
	}
	lines := make([]string, 0, len(sessions))
	for _, s := range sessions {
		project := s.Project
		if project == "" {
			project = "(unbound)"
		}
		held := "none"
		if len(s.HeldQuests) > 0 {
			held = strings.Join(s.HeldQuests, ",")
		}
		lines = append(lines, fmt.Sprintf("  session %s: project=%s connected=%s last_seen=%s held=%s",
			ShortSessionID(s.ID), project,
			formatAge(now, s.ConnectedAt), formatAge(now, s.LastHeartbeat), held))
	}
	return lines
}

// ShortSessionID trims a session id (the shim pid, possibly a longer
// composite in future) to a compact form for the readout. A short id is
// returned unchanged; a long one is truncated with an ellipsis so the line
// stays scannable.
func ShortSessionID(id string) string {
	const maxLen = 12
	if len(id) <= maxLen {
		return id
	}
	return id[:maxLen] + "…"
}

// formatAge renders the whole-second age between now and t as a compact
// duration ("3s", "5m2s"). A zero or future timestamp renders "0s" so a
// just-connected session never shows a negative age.
func formatAge(now, t time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	return d.Truncate(time.Second).String()
}
