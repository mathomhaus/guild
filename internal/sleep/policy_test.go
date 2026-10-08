package sleep

import "testing"

// TestClassify locks the HYBRID gate for every defined op kind plus
// the default-deny path for anything outside the taxonomy. Changing a
// verdict here is a policy decision, not a refactor; it must be
// deliberate.
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		op   OpKind
		want Policy
	}{
		// Additive or reversible: auto-apply with journaling.
		{"meld_exact_supersede is auto", OpMeldExactSupersede, PolicyAuto},
		{"embed_backfill is auto", OpEmbedBackfill, PolicyAuto},
		{"renewal_quest_post is auto", OpRenewalQuestPost, PolicyAuto},
		{"approval_quest_post is auto", OpApprovalQuestPost, PolicyAuto},

		// Destructive or judgment-laden: post approval quests instead.
		{"near_meld needs approval", OpNearMeld, PolicyApproval},
		{"reforge needs approval", OpReforge, PolicyApproval},
		{"seal needs approval", OpSeal, PolicyApproval},
		{"oath_demotion needs approval", OpOathDemotion, PolicyApproval},

		// Unknown kinds default-deny: a step inventing a new op kind
		// without extending the gate cannot mutate unattended.
		{"unknown kind default-denies", OpKind("vacuum_entries"), PolicyApproval},
		{"empty kind default-denies", OpKind(""), PolicyApproval},

		// Runner bookkeeping kinds are not part of the taxonomy, so
		// they fall through to default-deny too.
		{"step_partial marker default-denies", opStepPartial, PolicyApproval},
		{"step_error marker default-denies", opStepError, PolicyApproval},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.op); got != tc.want {
				t.Errorf("Classify(%q) = %q, want %q", tc.op, got, tc.want)
			}
		})
	}
}
