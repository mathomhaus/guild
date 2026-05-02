package lore

import (
	"context"
	"testing"
	"time"
)

// TestNearDup_FiresOnLORE399_400Reproducer is the canonical acceptance test
// for QUEST-244. It reconstructs the LORE-399/LORE-400 audit-pair scenario
// in a fixture DB and asserts that the near-duplicate hint fires on the
// second inscribe.
//
// LORE-399: MCP output-UX friction inventory (kind=observation, topic=mcp-output-ux,
//
//	prompted_by=QUEST-27, tags: mcp,output,ux,friction,v0.3,audit)
//
// LORE-400: near-identical title and summary written ~30s later with the same
// topic, same kind, same prompted_by, 5/6 overlapping tags.
func TestNearDup_FiresOnLORE399_400Reproducer(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "guild")

	base := time.Date(2026, 4, 24, 10, 0, 0, 0, time.UTC)

	// Inscribe the first entry (LORE-399 equivalent).
	first, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID:  "guild",
		Kind:       KindObservation,
		Title:      "MCP output UX friction inventory for inline rendering clients",
		Summary:    "Six friction points found during the QUEST-27 audit: collapsible blocks hide structured output, emoji prefixes render inconsistently, nested JSON is not human-readable in collapsed view, line-wrapped long strings truncate, multi-tool response ordering is undefined, and tool-name labels are omitted from collapsed view. All six are addressable at the format layer without protocol changes.",
		Topic:      "mcp-output-ux",
		Tags:       []string{"mcp", "output", "ux", "friction", "v0.3", "audit"},
		PromptedBy: "QUEST-27",
		Now:        base,
	})
	if err != nil {
		t.Fatalf("first inscribe: %v", err)
	}
	if first.NearDupHint != "" {
		t.Errorf("first inscribe should have no near-dup hint (no prior entry), got: %q", first.NearDupHint)
	}

	// Inscribe the second entry (LORE-400 equivalent): same topic, same
	// prompted_by, 5/6 overlapping tags, structurally identical summary.
	second, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID:  "guild",
		Kind:       KindObservation,
		Title:      "MCP output UX audit findings for inline rendering agents",
		Summary:    "Five UX audit findings for MCP inline rendering: collapsible blocks suppress structured output, emoji prefixes are inconsistent across clients, nested JSON is unreadable when collapsed, long strings wrap and truncate, and multi-tool response order is non-deterministic. Addressable at the formatter layer without spec changes.",
		Topic:      "mcp-output-ux",
		Tags:       []string{"mcp", "output", "ux", "audit", "v0.3", "rendering"},
		PromptedBy: "QUEST-27",
		Now:        base.Add(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("second inscribe: %v", err)
	}

	if second.NearDupHint == "" {
		t.Errorf("expected near-dup hint to fire on LORE-399/400 reproducer pair, got empty hint")
	}
	// The hint must reference the first entry's ID.
	firstID := formatEntryID(first.Entry.ID)
	if !containsStr(second.NearDupHint, firstID) {
		t.Errorf("near-dup hint should mention %s; got: %q", firstID, second.NearDupHint)
	}
}

// TestNearDup_NoFireOnUnrelatedEntry verifies that an entry with a completely
// different topic, no overlapping tags, no shared prompted_by, and different
// title/summary does NOT trigger a near-dup hint.
func TestNearDup_NoFireOnUnrelatedEntry(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	base := time.Date(2026, 4, 24, 10, 0, 0, 0, time.UTC)

	_, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha",
		Kind:      KindResearch,
		Title:     "database indexing strategies for write-heavy workloads",
		Summary:   "Write-heavy workloads benefit from deferred index builds and partial indexes. Covering indexes reduce heap fetches. BRIN indexes suit append-only time-series data with minimal overhead.",
		Topic:     "database",
		Tags:      []string{"postgres", "index", "performance"},
		Now:       base,
	})
	if err != nil {
		t.Fatalf("first inscribe: %v", err)
	}

	second, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha",
		Kind:      KindDecision,
		Title:     "use oauth2 for third party auth integration",
		Summary:   "OAuth2 with PKCE is the recommended flow for public clients. We adopt the authorization code flow for the web client and client credentials for service-to-service calls. No custom token format needed.",
		Topic:     "auth",
		Tags:      []string{"oauth2", "security", "integration"},
		Now:       base.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("second inscribe: %v", err)
	}

	if second.NearDupHint != "" {
		t.Errorf("expected no near-dup hint for unrelated entries, got: %q", second.NearDupHint)
	}
}

// TestNearDup_NoFireBeyondWindow verifies that entries older than
// nearDupWindowDays do NOT trigger the hint even when their content is
// lexically similar.
func TestNearDup_NoFireBeyondWindow(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	// Write the first entry with a timestamp older than the window.
	oldTime := time.Now().UTC().AddDate(0, 0, -(nearDupWindowDays + 1))

	_, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha",
		Kind:      KindObservation,
		Title:     "MCP output UX friction inventory for inline rendering clients",
		Summary:   "Six friction points found during the audit: collapsible blocks hide structured output, emoji prefixes render inconsistently, nested JSON is not human-readable in collapsed view, line-wrapped long strings truncate, multi-tool response ordering is undefined, and tool-name labels are omitted from collapsed view.",
		Topic:     "mcp-output-ux",
		Tags:      []string{"mcp", "output", "ux", "friction"},
		Now:       oldTime,
	})
	if err != nil {
		t.Fatalf("old entry inscribe: %v", err)
	}

	// Inscribe the near-duplicate now; the old entry is outside the window.
	second, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha",
		Kind:      KindObservation,
		Title:     "MCP output UX audit findings for inline rendering agents",
		Summary:   "Five UX audit findings for MCP inline rendering: collapsible blocks suppress structured output, emoji prefixes are inconsistent across clients, nested JSON is unreadable when collapsed, long strings wrap and truncate.",
		Topic:     "mcp-output-ux",
		Tags:      []string{"mcp", "output", "ux", "audit"},
		Now:       time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("second inscribe: %v", err)
	}

	if second.NearDupHint != "" {
		t.Errorf("expected no near-dup hint for out-of-window entry, got: %q", second.NearDupHint)
	}
}

// TestNearDup_NoFireOnFirstEntry verifies that the very first inscribe into
// a fresh DB never produces a near-dup hint (trivially: no prior entries).
func TestNearDup_NoFireOnFirstEntry(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	res, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha",
		Kind:      KindObservation,
		Title:     "MCP output UX friction inventory for inline rendering clients",
		Summary:   "Six friction points: collapsed blocks, inconsistent emoji, unreadable JSON, truncated strings, undefined order, missing labels.",
		Topic:     "mcp-output-ux",
		Tags:      []string{"mcp", "output", "ux"},
	})
	if err != nil {
		t.Fatalf("inscribe: %v", err)
	}
	if res.NearDupHint != "" {
		t.Errorf("expected no near-dup hint on first inscribe, got: %q", res.NearDupHint)
	}
}

// --- Jaccard / trigram unit tests ---

// TestJaccardStrings verifies the core Jaccard function for boundary cases
// and a known pair.
func TestJaccardStrings(t *testing.T) {
	cases := []struct {
		name    string
		a, b    []string
		wantMin float64
		wantMax float64
	}{
		{"empty both", nil, nil, 0.0, 0.0},
		{"empty a", nil, []string{"x"}, 0.0, 0.0},
		{"identical", []string{"a", "b", "c"}, []string{"a", "b", "c"}, 1.0, 1.0},
		{"no overlap", []string{"a", "b"}, []string{"c", "d"}, 0.0, 0.0},
		{"half overlap", []string{"a", "b", "c", "d"}, []string{"c", "d", "e", "f"}, 0.3, 0.4},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			as := make(map[string]struct{}, len(c.a))
			for _, v := range c.a {
				as[v] = struct{}{}
			}
			bs := make(map[string]struct{}, len(c.b))
			for _, v := range c.b {
				bs[v] = struct{}{}
			}
			got := jaccardStrings(as, bs)
			if got < c.wantMin || got > c.wantMax {
				t.Errorf("jaccardStrings(%v, %v) = %.3f, want [%.3f, %.3f]",
					c.a, c.b, got, c.wantMin, c.wantMax)
			}
		})
	}
}

// TestJaccardTrigrams verifies that near-identical summaries score above
// the nearDupJaccardSummaryThreshold and very different ones score below.
func TestJaccardTrigrams(t *testing.T) {
	a := "Six friction points found during the audit: collapsible blocks hide structured output and emoji prefixes are inconsistent."
	b := "Six friction findings from the audit: collapsible blocks suppress output and emoji prefixes render inconsistently across clients."
	similar := jaccardTrigrams(a, b)
	if similar < nearDupJaccardSummaryThreshold {
		t.Errorf("expected similar summaries to score >= %.2f, got %.3f", nearDupJaccardSummaryThreshold, similar)
	}

	c := "OAuth2 with PKCE is the recommended flow for public clients; adopt authorization code for web."
	different := jaccardTrigrams(a, c)
	if different >= nearDupJaccardSummaryThreshold {
		t.Errorf("expected unrelated summaries to score < %.2f, got %.3f", nearDupJaccardSummaryThreshold, different)
	}
}

// TestTokeniseTitle checks that the title tokeniser strips stopwords and
// short tokens.
func TestTokeniseTitle(t *testing.T) {
	tokens := tokeniseTitle("MCP output UX friction inventory for inline rendering clients")
	// "for" is a dedupStopword; short tokens "ux", "mcp" etc. are >=3 chars.
	// "for" must be absent.
	if _, ok := tokens["for"]; ok {
		t.Errorf("stopword 'for' should be stripped from title tokens")
	}
	// "friction" must be present.
	if _, ok := tokens["friction"]; !ok {
		t.Errorf("content word 'friction' must be in title tokens; got %v", tokens)
	}
}

// --- helpers ---

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
