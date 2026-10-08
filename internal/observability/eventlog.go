package observability

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/mathomhaus/guild/internal/guildpath"
)

// This file is the durable append-only JSONL event log (ADR-006 Phase 5,
// Headroom's request_logger.py + savings_tracker.py pattern). Every daemon
// decision and metric-worthy event is appended as one redacted JSON object
// per line to ~/.guild/observability/events.jsonl. The log is the durable,
// queryable record that survives a daemon restart; the time-bucketed rollups
// (rollup.go) are recomputed from it on boot.
//
// Durability + atomicity: each append opens the file O_APPEND, writes one
// complete line in a single Write call, and fsyncs. POSIX guarantees an
// O_APPEND write up to PIPE_BUF is atomic against concurrent appenders, and
// a single in-process mutex serializes our own writers, so a line is never
// torn or interleaved. A short write is treated as an error (the line is not
// counted), so the log never contains a partial record a reader would choke
// on.
//
// Redaction: events carry structured fields, never raw user prose. The
// Append path still runs a defensive redactor over every string value to
// strip anything that looks like an absolute home path or a bearer-token-
// shaped secret, so an event that incidentally captured one is scrubbed
// before it lands on disk.

// Event is one record in the JSONL log. Fields are stable and additive: a
// reader (the rollup recomputation) tolerates unknown future fields and
// missing optional ones. Time is RFC3339Nano UTC for lexical sortability.
type Event struct {
	// Time is when the event occurred, UTC.
	Time time.Time `json:"time"`
	// Kind is the event category, e.g. "autopass", "lease_reap",
	// "stale_renew" (mirroring daemon.DecisionKind) or a lifecycle event
	// like "service_start".
	Kind string `json:"kind"`
	// Allow is the yes/no outcome for a decision event. Omitted for
	// non-decision events via the pointer.
	Allow *bool `json:"allow,omitempty"`
	// Reason is the human-readable decisive reason.
	Reason string `json:"reason,omitempty"`
	// Inputs is the decision's boolean inputs (the "what did it see" fields).
	Inputs map[string]bool `json:"inputs,omitempty"`
	// Metrics is any non-boolean scalar the event produced.
	Metrics map[string]int `json:"metrics,omitempty"`
}

// EventLog is a thread-safe append-only JSONL writer.
type EventLog struct {
	path string

	mu sync.Mutex
}

// DefaultEventLogPath returns ~/.guild/observability/events.jsonl, creating
// the observability/ directory (0700) under the guild home. It is the
// canonical log location the Service uses.
func DefaultEventLogPath() (string, error) {
	dir, err := guildpath.EnsureGuildDir()
	if err != nil {
		return "", err
	}
	obsDir := filepath.Join(dir, "observability")
	if err := guildpath.EnsureDir(obsDir); err != nil {
		return "", err
	}
	return filepath.Join(obsDir, "events.jsonl"), nil
}

// NewEventLog returns an EventLog writing to path. The file and its parent
// directory are created lazily on the first Append, so constructing a log
// for a path under a not-yet-created home is cheap and side-effect-free.
func NewEventLog(path string) *EventLog {
	return &EventLog{path: path}
}

// Path returns the log file path.
func (l *EventLog) Path() string { return l.path }

// Append writes one event as a redacted JSON line, fsynced. It serializes
// concurrent writers with a mutex and relies on O_APPEND for atomicity
// against any other process appending to the same file. A write or sync
// error is returned so the caller (the Service) can count failures without
// crashing the daemon.
func (l *EventLog) Append(ev Event) error {
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	} else {
		ev.Time = ev.Time.UTC()
	}
	redactEvent(&ev)

	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("observability: marshal event: %w", err)
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	if err := guildpath.EnsureDir(filepath.Dir(l.path)); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, guildpath.DBPerm)
	if err != nil {
		return fmt.Errorf("observability: open event log: %w", err)
	}
	defer func() { _ = f.Close() }()

	n, err := f.Write(line)
	if err != nil {
		return fmt.Errorf("observability: append event: %w", err)
	}
	if n != len(line) {
		return fmt.Errorf("observability: short append: wrote %d of %d bytes", n, len(line))
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("observability: sync event log: %w", err)
	}
	return nil
}

// redactors scrub values that should never reach disk. The set is
// deliberately conservative: structured events carry no user prose, so the
// goal is only to catch an incidental secret or absolute home path.
var (
	// homePathRe matches an absolute path under a user home directory; the
	// matched home prefix is replaced with "~" so the log keeps the relative
	// shape without leaking the username.
	homePathRe = regexp.MustCompile(`/(?:Users|home)/[^/\s]+`)
	// secretRe matches a bearer-token / api-key shaped run (a long
	// high-entropy-ish token). Replaced wholesale.
	secretRe = regexp.MustCompile(`\b(?:sk|pk|ghp|gho|xox[baprs])[-_][A-Za-z0-9]{16,}\b`)
)

// redactEvent scrubs every string-typed value in the event (Reason and the
// keys/values are structured, but Reason is the one free-text field). The
// boolean Inputs and integer Metrics carry no secrets by construction.
func redactEvent(ev *Event) {
	ev.Reason = redactString(ev.Reason)
}

// redactString applies the redactors to s.
func redactString(s string) string {
	if s == "" {
		return s
	}
	s = secretRe.ReplaceAllString(s, "[redacted]")
	s = homePathRe.ReplaceAllString(s, "~")
	return s
}
