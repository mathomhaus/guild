package quest

import "testing"

// TestEventConstants_Values pins every exported note-prefix and event-kind
// constant to its on-disk value. Existing rows in ~/.guild/quest.db match
// these exact strings — any change here is a wire-contract break, and the
// diff on this test is the canary that forces a reviewer to consciously
// accept it (and ship a migration if needed).
func TestEventConstants_Values(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"NotePrefixSpec", NotePrefixSpec, "[spec] "},
		{"NotePrefixSpecReplace", NotePrefixSpecReplace, "[spec-replace] "},
		{"NotePrefixRework", NotePrefixRework, "[rework] of: "},
		{"NotePrefixCheckpoint", NotePrefixCheckpoint, "[checkpoint] "},
		{"NotePrefixCompleted", NotePrefixCompleted, "[completed] "},
		{"EventCreated", EventCreated, "created"},
		{"EventNoted", EventNoted, "noted"},
		{"EventClaimed", EventClaimed, "claimed"},
		{"EventDone", EventDone, "done"},
		{"EventUnblocked", EventUnblocked, "unblocked"},
		{"EventPMNextCalled", EventPMNextCalled, "pm_next_called"},
		{"EventReleased", EventReleased, "released"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q — changing this value is a wire-contract break; migrate existing rows or bump the DB schema version.",
				tc.name, tc.got, tc.want)
		}
	}
}

func TestIsSystemNote(t *testing.T) {
	cases := []struct {
		note string
		want bool
	}{
		{"[spec] subject: foo", true},
		{"[spec-replace] files: a, b", true},
		{"[rework] of: QUEST-3", true},
		{"[checkpoint] hypothesis: foo", true},
		{"[completed] done in abc123", true},
		{"free-form agent journal note", false},
		{"", false},
		{"[unknown-prefix] foo", false},
	}
	for _, tc := range cases {
		if got := IsSystemNote(tc.note); got != tc.want {
			t.Errorf("IsSystemNote(%q) = %v, want %v", tc.note, got, tc.want)
		}
	}
}
