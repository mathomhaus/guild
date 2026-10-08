package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/daemon"
)

// TestFormatWatchLine covers the three operator-visible watcher states the
// daemon status line renders: disabled, degraded, and actively watching
// with counters.
func TestFormatWatchLine(t *testing.T) {
	tests := []struct {
		name string
		in   daemon.WatchStatus
		want []string // substrings that must appear
		deny []string // substrings that must NOT appear
	}{
		{
			name: "disabled",
			in:   daemon.WatchStatus{Enabled: false},
			want: []string{"watch: disabled"},
			deny: []string{"projects=", "events="},
		},
		{
			name: "watching with counters",
			in: daemon.WatchStatus{
				Enabled: true, Watching: true,
				ProjectsWatched: 2, EventsSeen: 5, SignalsRecorded: 3, QuestsPosted: 1,
			},
			want: []string{"watch: watching", "projects=2", "events=5", "signals=3", "renewals=1"},
			deny: []string{"degraded", "last_error"},
		},
		{
			name: "degraded surfaces last error",
			in: daemon.WatchStatus{
				Enabled: true, Watching: false, LastError: "start watcher: too many open files",
			},
			want: []string{"degraded (query-time staleness)", `last_error="start watcher: too many open files"`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatWatchLine(tc.in)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("formatWatchLine(%+v) = %q, missing %q", tc.in, got, w)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(got, d) {
					t.Errorf("formatWatchLine(%+v) = %q, must not contain %q", tc.in, got, d)
				}
			}
		})
	}
}

// TestWriteDaemonStatusJSONIncludesWatch proves the --json status view
// carries the watch object when the daemon is running, with the counters
// mapped through from the status reply.
func TestWriteDaemonStatusJSONIncludesWatch(t *testing.T) {
	rep := daemon.StatusReport{
		Running:     true,
		SelfVersion: "v1.2.3",
		SocketPath:  "/tmp/x/.guild/daemon.sock",
		Uptime:      42 * time.Second,
		Status: daemon.Status{
			PID:           4321,
			Version:       "v1.2.3",
			StartedAt:     time.Unix(1_700_000_000, 0).UTC(),
			EmbedderState: "enabled",
			Watch: daemon.WatchStatus{
				Enabled: true, Watching: true,
				ProjectsWatched: 1, EventsSeen: 2, SignalsRecorded: 2, QuestsPosted: 1,
			},
		},
	}

	var buf bytes.Buffer
	if err := writeDaemonStatusJSON(&buf, rep); err != nil {
		t.Fatalf("writeDaemonStatusJSON: %v", err)
	}

	var view struct {
		Running bool             `json:"running"`
		Watch   *daemonWatchView `json:"watch"`
	}
	if err := json.Unmarshal(buf.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal status json: %v\n%s", err, buf.String())
	}
	if view.Watch == nil {
		t.Fatalf("running status --json missing watch object: %s", buf.String())
	}
	if !view.Watch.Enabled || !view.Watch.Watching {
		t.Errorf("watch view enabled/watching wrong: %+v", view.Watch)
	}
	if view.Watch.ProjectsWatched != 1 || view.Watch.EventsSeen != 2 ||
		view.Watch.SignalsRecorded != 2 || view.Watch.QuestsPosted != 1 {
		t.Errorf("watch counters not mapped through: %+v", view.Watch)
	}
}

// TestWriteDaemonStatusJSONIncludesPresence proves the --json status view
// carries the per-session presence detail and the lifetime reap counter when
// the daemon is running, mapped through from the status reply.
func TestWriteDaemonStatusJSONIncludesPresence(t *testing.T) {
	connected := time.Unix(1_700_000_000, 0).UTC()
	rep := daemon.StatusReport{
		Running:     true,
		SelfVersion: "v1.2.3",
		SocketPath:  "/tmp/x/.guild/daemon.sock",
		Status: daemon.Status{
			PID:          4321,
			Version:      "v1.2.3",
			StartedAt:    connected,
			LeasesReaped: 4,
			Sessions: []daemon.SessionStatus{
				{
					ID:            "4242",
					Project:       "alpha",
					ConnectedAt:   connected,
					LastHeartbeat: connected.Add(30 * time.Second),
					HeldQuests:    []string{"QUEST-1"},
				},
			},
		},
	}

	var buf bytes.Buffer
	if err := writeDaemonStatusJSON(&buf, rep); err != nil {
		t.Fatalf("writeDaemonStatusJSON: %v", err)
	}

	var view struct {
		LeasesReaped int64               `json:"leases_reaped"`
		Sessions     []daemonSessionView `json:"sessions"`
	}
	if err := json.Unmarshal(buf.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal status json: %v\n%s", err, buf.String())
	}
	if view.LeasesReaped != 4 {
		t.Errorf("leases_reaped = %d, want 4: %s", view.LeasesReaped, buf.String())
	}
	if len(view.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1: %s", len(view.Sessions), buf.String())
	}
	s := view.Sessions[0]
	if s.ID != "4242" || s.Project != "alpha" {
		t.Errorf("session identity wrong: %+v", s)
	}
	if len(s.HeldQuests) != 1 || s.HeldQuests[0] != "QUEST-1" {
		t.Errorf("held_quests not mapped through: %+v", s.HeldQuests)
	}
}

// TestWriteDaemonStatusJSONOmitsSessionsWhenNotRunning proves a not-running
// report keeps the JSON minimal: no sessions array.
func TestWriteDaemonStatusJSONOmitsSessionsWhenNotRunning(t *testing.T) {
	rep := daemon.StatusReport{Running: false, SelfVersion: "v1.2.3"}

	var buf bytes.Buffer
	if err := writeDaemonStatusJSON(&buf, rep); err != nil {
		t.Fatalf("writeDaemonStatusJSON: %v", err)
	}
	if strings.Contains(buf.String(), `"sessions"`) {
		t.Errorf("not-running status --json should omit sessions: %s", buf.String())
	}
}

// TestWriteDaemonStatusJSONOmitsWatchWhenNotRunning proves a not-running
// report keeps the JSON minimal: no watch object.
func TestWriteDaemonStatusJSONOmitsWatchWhenNotRunning(t *testing.T) {
	rep := daemon.StatusReport{Running: false, SelfVersion: "v1.2.3"}

	var buf bytes.Buffer
	if err := writeDaemonStatusJSON(&buf, rep); err != nil {
		t.Fatalf("writeDaemonStatusJSON: %v", err)
	}
	if strings.Contains(buf.String(), `"watch"`) {
		t.Errorf("not-running status --json should omit watch: %s", buf.String())
	}
}
