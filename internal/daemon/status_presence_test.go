package daemon

import (
	"strings"
	"testing"
	"time"
)

// TestFormatSessionLines_PerSessionDetail asserts each live session renders
// one line carrying the short id, project, connected/last-seen ages, and the
// held quest ids, computed relative to the supplied now.
func TestFormatSessionLines_PerSessionDetail(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sessions := []SessionStatus{
		{
			ID:            "4242",
			Project:       "alpha",
			ConnectedAt:   now.Add(-5 * time.Minute),
			LastHeartbeat: now.Add(-30 * time.Second),
			HeldQuests:    []string{"QUEST-1", "QUEST-7"},
		},
	}
	lines := FormatSessionLines(sessions, now)
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1: %v", len(lines), lines)
	}
	line := lines[0]
	for _, want := range []string{
		"session 4242",
		"project=alpha",
		"connected=5m0s",
		"last_seen=30s",
		"held=QUEST-1,QUEST-7",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("session line missing %q; got: %q", want, line)
		}
	}
}

// TestFormatSessionLines_EmptyRendersCleanly asserts an empty registry
// renders a single "none active" line so the readout never goes blank.
func TestFormatSessionLines_EmptyRendersCleanly(t *testing.T) {
	lines := FormatSessionLines(nil, time.Now())
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "none active") {
		t.Errorf("empty readout = %q, want a 'none active' line", lines[0])
	}
}

// TestFormatSessionLines_UnboundProjectAndNoLeases asserts a session with no
// bound project renders "(unbound)" and a session holding no leases renders
// "held=none", so the degraded states stay readable.
func TestFormatSessionLines_UnboundProjectAndNoLeases(t *testing.T) {
	now := time.Now()
	lines := FormatSessionLines([]SessionStatus{{
		ID:            "7",
		ConnectedAt:   now,
		LastHeartbeat: now,
	}}, now)
	if !strings.Contains(lines[0], "project=(unbound)") {
		t.Errorf("missing unbound marker; got: %q", lines[0])
	}
	if !strings.Contains(lines[0], "held=none") {
		t.Errorf("missing held=none; got: %q", lines[0])
	}
}

// TestFormatLeasesLine asserts the lifetime reap counter renders.
func TestFormatLeasesLine(t *testing.T) {
	if got, want := FormatLeasesLine(0), "  leases: reaped=0"; got != want {
		t.Errorf("FormatLeasesLine(0) = %q, want %q", got, want)
	}
	if got, want := FormatLeasesLine(3), "  leases: reaped=3"; got != want {
		t.Errorf("FormatLeasesLine(3) = %q, want %q", got, want)
	}
}

// TestShortSessionID asserts a short id passes through and a long one is
// truncated with an ellipsis.
func TestShortSessionID(t *testing.T) {
	if got := ShortSessionID("4242"); got != "4242" {
		t.Errorf("ShortSessionID(short) = %q, want unchanged", got)
	}
	long := "abcdefghijklmnopqrstuvwxyz"
	got := ShortSessionID(long)
	if !strings.HasPrefix(got, long[:12]) || !strings.HasSuffix(got, "…") {
		t.Errorf("ShortSessionID(long) = %q, want 12-char prefix + ellipsis", got)
	}
}

// TestFormatAge_NeverNegative asserts a future timestamp clamps to 0s rather
// than rendering a negative age.
func TestFormatAge_NeverNegative(t *testing.T) {
	now := time.Now()
	if got := formatAge(now, now.Add(time.Hour)); got != "0s" {
		t.Errorf("formatAge(future) = %q, want 0s", got)
	}
}
