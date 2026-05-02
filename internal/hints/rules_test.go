package hints

import "testing"

// TestDefinitions_LaunchSet10 asserts the definitions list matches the
// 10-rule set — launch-9 plus QUEST-167's thin-citation rule. Guards
// against drift — if a rule is added or removed, this test forces an
// update in lock-step with the seed migration.
func TestDefinitions_LaunchSet10(t *testing.T) {
	defs := Definitions()
	want := map[string]bool{
		"inscribe-looks-like-quest":           true,
		"no-session-start":                    true,
		"session-end-without-brief":           true,
		"slug-query":                          true,
		"journal-outside-accepted":            true,
		"no-brief-24h":                        true,
		"inscribe-without-appraise":           true,
		"clear-without-report-detail":         true,
		"inscribe-without-transfer-reasoning": true,
	}
	if len(defs) != len(want) {
		t.Errorf("Definitions() len = %d, want %d", len(defs), len(want))
	}
	for _, d := range defs {
		if !want[d.ID] {
			t.Errorf("unexpected rule id %q", d.ID)
			continue
		}
		delete(want, d.ID)
		if d.Trigger == nil {
			t.Errorf("%s: Trigger nil", d.ID)
		}
		if d.FollowThrough == nil {
			t.Errorf("%s: FollowThrough nil", d.ID)
		}
	}
	for missing := range want {
		t.Errorf("missing rule id %q", missing)
	}
}
