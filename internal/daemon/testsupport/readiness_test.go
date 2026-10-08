package testsupport

import (
	"testing"
	"time"
)

func TestReadinessBudgetAlwaysCapped(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		remaining   time.Duration
		hasDeadline bool
		want        time.Duration
	}{
		{"no deadline", 0, false, 30 * time.Second},
		{"default package timeout", 10 * time.Minute, true, 30 * time.Second},
		{"short deadline", 5 * time.Second, true, 3 * time.Second},
		{"margin exhausted", time.Second, true, readyTick},
		{"deadline elapsed", -time.Second, true, readyTick},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := readinessBudget(now, now.Add(tc.remaining), tc.hasDeadline); got != tc.want {
				t.Fatalf("readiness budget=%v want=%v", got, tc.want)
			}
		})
	}
}
