package eval

import (
	"context"
	"testing"

	"github.com/mathomhaus/guild/internal/lore"
)

// TestRunGrid_LocksExpectedVerdicts is the headline acceptance. The grid is
// NOT aspirational: it records what guild's REAL ranker does against each
// poison, and the per-probe Expect field is the locked ground truth. The mix
// is deliberate (see corpus.go): the exact-title probes (dedup, lease) are
// GREEN because the +1.0 title boost defends the genuine answer against
// keyword-stuffing, injection text, and a near-duplicate; the recency probe
// is RED because a natural-language query with no title anchor lets a
// term-stuffed poison win on raw BM25 — a real, caught vulnerability. A change
// in ANY probe's verdict (a defense that broke, or a vulnerability that got
// fixed) trips this test, which is exactly the regression signal we want.
func TestRunGrid_LocksExpectedVerdicts(t *testing.T) {
	ctx := context.Background()
	res, err := RunGrid(ctx, lore.DefaultScoring())
	if err != nil {
		t.Fatalf("RunGrid: %v", err)
	}
	if res.Total != len(gridProbes()) {
		t.Fatalf("grid total = %d, want %d", res.Total, len(gridProbes()))
	}
	want := map[string]bool{}
	for _, p := range gridProbes() {
		want[p.Name] = p.Expect
	}
	for _, v := range res.Verdicts {
		exp, ok := want[v.Probe]
		if !ok {
			t.Errorf("unexpected probe in result: %q", v.Probe)
			continue
		}
		if v.Green != exp {
			t.Errorf("probe %q verdict = green:%v, want green:%v (reason=%q, winner=%q, poisons ahead=%v)",
				v.Probe, v.Green, exp, v.Reason, v.WinnerSlug, v.PoisonsAhead)
		}
	}
	// The grid must contain BOTH a green and a red cell, proving it neither
	// rubber-stamps (all green regardless) nor is permanently broken (all red).
	if res.GreenCount == 0 {
		t.Error("grid has no green cells: the ranker defends nothing, or the corpus is mis-tuned")
	}
	if res.RedCount == 0 {
		t.Error("grid has no red cells: it is not exercising a real vulnerability and would not catch regressions")
	}
}

// TestRunGrid_Deterministic proves "same corpus + same config => same
// verdicts": two independent runs produce byte-identical grid results.
func TestRunGrid_Deterministic(t *testing.T) {
	ctx := context.Background()
	a, err := RunGrid(ctx, lore.DefaultScoring())
	if err != nil {
		t.Fatalf("RunGrid a: %v", err)
	}
	b, err := RunGrid(ctx, lore.DefaultScoring())
	if err != nil {
		t.Fatalf("RunGrid b: %v", err)
	}
	if a.Total != b.Total || a.GreenCount != b.GreenCount || a.RedCount != b.RedCount {
		t.Fatalf("rollups differ: a=%+v b=%+v", a, b)
	}
	if len(a.Verdicts) != len(b.Verdicts) {
		t.Fatalf("verdict counts differ: %d vs %d", len(a.Verdicts), len(b.Verdicts))
	}
	for i := range a.Verdicts {
		va, vb := a.Verdicts[i], b.Verdicts[i]
		if va.Probe != vb.Probe || va.Green != vb.Green ||
			va.WinnerSlug != vb.WinnerSlug || va.WantRank != vb.WantRank {
			t.Errorf("verdict %d differs across runs:\n a=%+v\n b=%+v", i, va, vb)
		}
	}
}

// TestGrid_CatchesPoison is the negative control that proves the grid ACTUALLY
// CATCHES a poison rather than always reporting green. We hand the ranker a
// degenerate scoring config that zeroes the BM25 weight and disables the
// title boost, leaving recency as the only signal. The adversarial rows are
// deliberately more recent than their genuine answers, so under recency-only
// ranking a poison should climb above the answer and turn at least one cell
// red. A grid that reports green here would be inert.
func TestGrid_CatchesPoison(t *testing.T) {
	ctx := context.Background()
	recencyOnly := lore.ScoringConfig{
		WFTS:            0.0,
		WRecency:        1.0,
		HalfLifeDays:    30,
		TitleMatchBoost: 0.0,
		TitleTokenBoost: 0.0,
	}
	res, err := RunGrid(ctx, recencyOnly)
	if err != nil {
		t.Fatalf("RunGrid: %v", err)
	}
	if res.AllGreen() {
		t.Fatalf("grid stayed all-green under recency-only scoring; it is not catching poisons")
	}
	// At least one red cell must be attributable to a poison outranking the
	// answer (not merely a recall miss), proving the poison-detection path.
	sawPoisonAhead := false
	for _, v := range res.Verdicts {
		if !v.Green && len(v.PoisonsAhead) > 0 {
			sawPoisonAhead = true
			t.Logf("caught: probe %q had poisons ahead: %v", v.Probe, v.PoisonsAhead)
		}
	}
	if !sawPoisonAhead {
		t.Fatalf("no cell reported a poison outranking the answer; the poison detector is inert")
	}
}

// TestCorpus_PoisonSlugsResolve guards the corpus/probe wiring: every probe's
// WantSlug and PoisonSlugs must name a real corpus row, so a typo can never
// silently disable a check.
func TestCorpus_PoisonSlugsResolve(t *testing.T) {
	known := map[string]bool{}
	for _, e := range scratchCorpus() {
		if known[e.Slug] {
			t.Fatalf("duplicate corpus slug %q", e.Slug)
		}
		known[e.Slug] = true
	}
	for _, p := range gridProbes() {
		if !known[p.WantSlug] {
			t.Errorf("probe %q WantSlug %q is not a corpus row", p.Name, p.WantSlug)
		}
		for _, s := range p.PoisonSlugs {
			if !known[s] {
				t.Errorf("probe %q poison %q is not a corpus row", p.Name, s)
			}
		}
	}
	// sortedSlugs is a stable helper; assert it returns the full set.
	if got := len(sortedSlugs()); got != len(known) {
		t.Errorf("sortedSlugs len = %d, want %d", got, len(known))
	}
}
