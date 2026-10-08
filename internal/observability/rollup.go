package observability

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/mathomhaus/guild/internal/guildpath"
)

// This file is the durable-rollup half of the observability triad (ADR-006
// Phase 5, Headroom's savings_tracker.py durable-JSON pattern). It maintains
// time-bucketed counts of events (per hour and per day, keyed by event kind)
// that survive a daemon restart: on boot the rollups are recomputed by
// replaying the JSONL event log, and they are persisted to a JSON sidecar so
// a reader does not have to replay the whole log every time.
//
// Buckets are keyed by a UTC time-truncation string:
//   - hourly: "2006-01-02T15" (RFC3339-ish hour bucket)
//   - daily:  "2006-01-02"
// and within a bucket by event Kind, so a reader can answer "how many
// autopass decisions fired in the 14:00 hour?" without rescanning the log.

// bucketCounts maps event kind -> count for one time bucket.
type bucketCounts map[string]int

// Rollups holds the hourly and daily buckets plus the watermark of the last
// event folded in, so an incremental update only folds new events. It is
// safe for concurrent use.
type Rollups struct {
	mu sync.Mutex

	// Hourly maps an hour-bucket key to per-kind counts.
	Hourly map[string]bucketCounts `json:"hourly"`
	// Daily maps a day-bucket key to per-kind counts.
	Daily map[string]bucketCounts `json:"daily"`
	// Watermark is the Time of the most recent event already folded into the
	// buckets; a Fold of an event at or before it is ignored (idempotent
	// replay). Zero means nothing folded yet.
	Watermark time.Time `json:"watermark"`
}

// NewRollups returns empty rollups.
func NewRollups() *Rollups {
	return &Rollups{
		Hourly: map[string]bucketCounts{},
		Daily:  map[string]bucketCounts{},
	}
}

// hourKey and dayKey are the UTC bucket keys for t.
func hourKey(t time.Time) string { return t.UTC().Format("2006-01-02T15") }
func dayKey(t time.Time) string  { return t.UTC().Format("2006-01-02") }

// Fold adds one LIVE event to the rollups, always counting it (a live
// decision is a real occurrence even when it shares a timestamp with an
// earlier one). It advances the watermark to the event's time so a later
// log replay knows everything up to here is already counted. Use this for
// every event produced while the daemon runs.
func (r *Rollups) Fold(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.countLocked(ev)
	if t := ev.Time.UTC(); t.After(r.Watermark) {
		r.Watermark = t
	}
}

// foldReplay folds an event read from the log on boot. Unlike Fold it is
// idempotent against the watermark: an event at or before the current
// watermark (already reflected in a loaded sidecar) is skipped so a sidecar
// plus the log tail does not double-count their overlap. The strict
// "after watermark" guard is correct for replay because the watermark is the
// time of the last sidecar-folded event and the log is in time order, so the
// first un-counted log line is the first strictly newer one. (A rare
// same-second boundary event already in the sidecar is conservatively
// skipped; the alternative, double-counting, is worse for a metrics rollup.)
func (r *Rollups) foldReplay(ev Event) {
	t := ev.Time.UTC()
	if !r.Watermark.IsZero() && !t.After(r.Watermark) {
		return
	}
	r.countLocked(ev)
	r.Watermark = t
}

// countLocked adds one event to both bucket maps. Caller holds r.mu.
func (r *Rollups) countLocked(ev Event) {
	t := ev.Time.UTC()
	kind := ev.Kind
	if kind == "" {
		kind = "unknown"
	}
	addCount(r.Hourly, hourKey(t), kind)
	addCount(r.Daily, dayKey(t), kind)
}

// addCount increments buckets[key][kind].
func addCount(buckets map[string]bucketCounts, key, kind string) {
	bc := buckets[key]
	if bc == nil {
		bc = bucketCounts{}
		buckets[key] = bc
	}
	bc[kind]++
}

// HourlySnapshot returns a deep copy of the hourly buckets for a reader, so a
// caller cannot mutate rollup state through the result.
func (r *Rollups) HourlySnapshot() map[string]map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return copyBuckets(r.Hourly)
}

// DailySnapshot returns a deep copy of the daily buckets.
func (r *Rollups) DailySnapshot() map[string]map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return copyBuckets(r.Daily)
}

func copyBuckets(src map[string]bucketCounts) map[string]map[string]int {
	out := make(map[string]map[string]int, len(src))
	for k, bc := range src {
		inner := make(map[string]int, len(bc))
		for kind, n := range bc {
			inner[kind] = n
		}
		out[k] = inner
	}
	return out
}

// DefaultRollupPath returns ~/.guild/observability/rollups.json.
func DefaultRollupPath() (string, error) {
	dir, err := guildpath.EnsureGuildDir()
	if err != nil {
		return "", err
	}
	obsDir := filepath.Join(dir, "observability")
	if err := guildpath.EnsureDir(obsDir); err != nil {
		return "", err
	}
	return filepath.Join(obsDir, "rollups.json"), nil
}

// LoadRollups reconstructs the rollups for a daemon boot. It first tries the
// persisted JSON sidecar at rollupPath; whether or not that exists, it then
// replays the JSONL event log at logPath to fold in any events newer than the
// loaded watermark (so a sidecar written before the daemon crashed plus the
// log's tail yield a complete, correct rollup). A missing sidecar or log is
// not an error: the result is simply rebuilt from whatever exists. This is
// the "survive a daemon restart" guarantee.
func LoadRollups(rollupPath, logPath string) (*Rollups, error) {
	r := NewRollups()
	if rollupPath != "" {
		if err := r.loadSidecar(rollupPath); err != nil {
			return nil, err
		}
	}
	if logPath != "" {
		if err := r.replayLog(logPath); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// loadSidecar loads the persisted JSON rollups, if present. A missing file is
// not an error (nothing to load). A corrupt file is reported so the operator
// knows, but the caller can choose to rebuild from the log.
func (r *Rollups) loadSidecar(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("observability: read rollups: %w", err)
	}
	var loaded Rollups
	if err := json.Unmarshal(data, &loaded); err != nil {
		return fmt.Errorf("observability: parse rollups: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if loaded.Hourly != nil {
		r.Hourly = loaded.Hourly
	}
	if loaded.Daily != nil {
		r.Daily = loaded.Daily
	}
	r.Watermark = loaded.Watermark
	return nil
}

// replayLog folds every event in the JSONL log newer than the watermark. A
// missing log is not an error. A malformed line is skipped (the log is
// append-only and fsynced per line, so a malformed tail can only be a torn
// crash-time write, which the next append would not produce; skipping is the
// safe choice). Events are folded in file order, which is time order because
// the log is append-only.
func (r *Rollups) replayLog(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("observability: open event log for replay: %w", err)
	}
	defer func() { _ = f.Close() }()

	r.mu.Lock()
	defer r.mu.Unlock()

	sc := bufio.NewScanner(f)
	// Allow long JSON lines (default 64KiB token cap is plenty, but a busy
	// event with many inputs could approach it; bump the buffer headroom).
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		r.foldReplay(ev)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("observability: scan event log: %w", err)
	}
	return nil
}

// Persist writes the rollups to the JSON sidecar atomically (temp file +
// rename), so a crash mid-write never corrupts the sidecar; the next boot
// then replays the log to recover any events the lost write would have
// included. Called periodically and on shutdown by the Service.
func (r *Rollups) Persist(path string) error {
	r.mu.Lock()
	snapshot := Rollups{
		Hourly:    r.Hourly,
		Daily:     r.Daily,
		Watermark: r.Watermark,
	}
	r.mu.Unlock()

	data, err := json.MarshalIndent(&snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("observability: marshal rollups: %w", err)
	}
	if err := guildpath.EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, guildpath.DBPerm); err != nil {
		return fmt.Errorf("observability: write rollups tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("observability: rename rollups: %w", err)
	}
	return nil
}

// SortedKeys returns the bucket keys of m in ascending (chronological) order;
// a helper for readers that render the rollups deterministically.
func SortedKeys(m map[string]map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
