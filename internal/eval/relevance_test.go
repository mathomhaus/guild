package eval

import (
	"context"
	"encoding/json"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/lore/embed"
	"math"
	"testing"
)

func TestRelevanceMetricsCountJudgedAnswers(t *testing.T) {
	js := []RelevanceJudgment{
		{Relevant: []string{"a", "b"}, Retrieved: []string{"x", "a", "b"}, Answerability: lore.AnswerabilityUnknown},
		{Relevant: []string{"c"}, Retrieved: []string{"c"}, Answerability: lore.AnswerabilityUnknown},
		{Retrieved: []string{"x"}, Answerability: lore.AnswerabilityUnknown},
		{Retrieved: []string{}, Answerability: lore.AnswerabilityNoMatch},
	}
	m := measureRelevance(js)
	if m.Positives != 2 || m.Negatives != 2 || m.HitAt1 != 0.5 || m.HitAt5 != 1 || m.RecallAt1 != 0.5 || m.RecallAt5 != 1 || m.MRR != 0.75 || m.NegativeCandidateRate != 0.5 || m.UnknownAnswerabilityRate != 0.75 {
		t.Fatalf("wrong judged metrics: %+v", m)
	}
	if math.Abs(m.PrecisionAt5-0.15) > 1e-9 {
		t.Fatalf("precision=%v", m.PrecisionAt5)
	}
}

func TestRelevanceLexicalGate(t *testing.T) {
	report, err := RunRelevance(context.Background(), RelevanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.RelevancePassed() {
		t.Fatalf("lexical held-out relevance gate failed: %+v", report.Metrics)
	}
	b, _ := json.Marshal(report)
	t.Log(string(b))
	// Normative gates do not lock known failures as successes. Exact symbols,
	// scope collisions and explicit recent windows must retrieve the answer.
	for _, j := range report.Judgments {
		switch j.Name {
		case "exact-symbol", "sandbox-lifetime", "current-session-lifetime", "recent-expiry":
			if j.ReciprocalRank != 1 {
				t.Errorf("%s did not return the judged answer first: %v", j.Name, j.Retrieved)
			}
		}
	}
	if report.Metrics.Positives != 17 || report.Metrics.Negatives != 8 {
		t.Fatalf("labels drifted: %+v", report.Metrics)
	}
	for _, j := range report.Judgments {
		if len(j.Retrieved) > 0 && j.Answerability != lore.AnswerabilityUnknown {
			t.Errorf("%s asserts answerability for unverified candidates: %s", j.Name, j.Answerability)
		}
	}
}

func TestRelevanceDeterministicEmbeddingWiring(t *testing.T) {
	// Hash embeddings are a wiring test only, with no semantic quality claim.
	for _, mode := range []string{"vector", "hybrid"} {
		r, err := RunRelevance(context.Background(), RelevanceOptions{Mode: mode, Embedder: embed.NewDeterministicEmbedder(), ModelID: "eval-wiring"})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Judgments) != 25 {
			t.Fatalf("%s did not exercise every question", mode)
		}
		if mode == "vector" && len(r.ThresholdTrials) != 6 {
			t.Fatal("missing threshold tradeoff diagnostics")
		}
	}
	if _, err := RunRelevance(context.Background(), RelevanceOptions{Mode: "hybrid"}); err == nil {
		t.Fatal("missing model was silently accepted")
	}
}
