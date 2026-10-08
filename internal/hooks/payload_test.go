package hooks

import (
	"strings"
	"testing"
)

// worstSessionData builds a SessionData far above every budget: long
// multibyte titles and summaries, an oversized brief, and more oath
// and bounty lines than the line caps allow.
func worstSessionData() SessionData {
	longé := strings.Repeat("wordé ", 200) // ~1200 bytes, multibyte runes
	d := SessionData{
		BriefAt:    "2026-06-12T19:10",
		BriefAgent: "agent",
		BriefText:  strings.Repeat(longé, 10), // ~12000 bytes
	}
	for i := 0; i < 40; i++ {
		d.Oath = append(d.Oath, OathLine{Title: longé, Summary: longé})
	}
	for i := 0; i < 10; i++ {
		d.Bounties = append(d.Bounties, BountyLine{
			ID: "QUEST-999", Priority: "P0", Subject: longé,
		})
	}
	return d
}

func TestRenderSession_WorstCaseStaysUnderByteCeiling(t *testing.T) {
	got := RenderSession(worstSessionData())
	// +1 accounts for the trailing newline the CLI printer appends.
	if len(got)+1 > MaxSessionBytes {
		t.Errorf("payload %d bytes (+newline) exceeds ceiling %d", len(got)+1, MaxSessionBytes)
	}
	if !strings.HasPrefix(got, "## guild session priming ["+PayloadVersion+"]") {
		t.Errorf("missing version-marked header, got prefix %q", got[:60])
	}
}

func TestRenderSession_SectionCaps(t *testing.T) {
	got := RenderSession(worstSessionData())
	oathLines := 0
	bountyLines := 0
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "- wordé") {
			oathLines++
		}
		if strings.HasPrefix(line, "- QUEST-999") {
			bountyLines++
		}
	}
	if oathLines > MaxOathLines {
		t.Errorf("oath lines = %d, cap is %d", oathLines, MaxOathLines)
	}
	if bountyLines > MaxBountyLines {
		t.Errorf("bounty lines = %d, cap is %d", bountyLines, MaxBountyLines)
	}
}

func TestRenderSession_HappyPath(t *testing.T) {
	got := RenderSession(SessionData{
		Oath:       []OathLine{{Title: "Verify in source", Summary: "cite file:line"}},
		BriefAt:    "2026-06-12T19:10",
		BriefAgent: "agent",
		BriefText:  "shipped the payload verbs, next is hook install",
		Bounties: []BountyLine{
			{ID: "QUEST-314", Priority: "P1", Subject: "hook payload verbs"},
			{ID: "QUEST-315", Priority: "P1", Subject: "hook install"},
		},
	})
	for _, want := range []string{
		"[" + PayloadVersion + "]",
		"**oath (top 1):**",
		"- Verify in source: cite file:line",
		"**last brief [2026-06-12T19:10 by agent]:**",
		"shipped the payload verbs, next is hook install",
		"**top bounties:**",
		"- QUEST-314 [P1] hook payload verbs",
		"- QUEST-315 [P1] hook install",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("payload missing %q:\n%s", want, got)
		}
	}
	if strings.HasSuffix(got, "\n") {
		t.Error("payload must not carry a trailing newline (printer adds it)")
	}
}

func TestRenderSession_EmptyDataRendersPlaceholders(t *testing.T) {
	got := RenderSession(SessionData{})
	for _, want := range []string{
		"**oath:** (none)",
		"**last brief:** (none)",
		"**top bounties:** (none)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("payload missing placeholder %q:\n%s", want, got)
		}
	}
	if len(got)+1 > MaxSessionBytes {
		t.Errorf("empty payload %d bytes exceeds ceiling", len(got))
	}
}

func TestRenderSession_MultilineInputsCollapseToOneBullet(t *testing.T) {
	got := RenderSession(SessionData{
		Oath:      []OathLine{{Title: "line one\nline two", Summary: "sum\nmary"}},
		BriefText: "first\nsecond",
	})
	if strings.Contains(got, "one\nline") {
		t.Errorf("oath title newline leaked into payload:\n%s", got)
	}
	if !strings.Contains(got, "- line one line two: sum mary") {
		t.Errorf("expected collapsed oath bullet:\n%s", got)
	}
	if !strings.Contains(got, "first second") {
		t.Errorf("expected collapsed brief text:\n%s", got)
	}
}

func TestRenderAppraise_WorstCaseStaysUnderByteCeiling(t *testing.T) {
	long := strings.Repeat("véry long summary ", 100)
	var lines []AppraiseLine
	for i := 0; i < MaxAppraiseLines; i++ {
		lines = append(lines, AppraiseLine{
			ID: "LORE-9999", Kind: "decision", AgeDays: 365,
			Title: long, Summary: long,
		})
	}
	got := RenderAppraise(lines, 1234)
	if len(got)+1 > MaxAppraiseBytes {
		t.Errorf("payload %d bytes (+newline) exceeds ceiling %d", len(got)+1, MaxAppraiseBytes)
	}
	if !strings.HasPrefix(got, "## relevant lore ["+PayloadVersion+"]") {
		t.Errorf("missing version-marked header:\n%s", got)
	}
}

func TestRenderAppraise_HappyPath(t *testing.T) {
	got := RenderAppraise([]AppraiseLine{
		{ID: "LORE-9", Kind: "decision", AgeDays: 14, Title: "Agent-agnostic by design", Summary: "MCP plus local SQLite"},
		{ID: "LORE-12", Kind: "principle", AgeDays: 14, Title: "Many Gates lead to one Guild", Summary: ""},
	}, 17)
	for _, want := range []string{
		"## relevant lore [" + PayloadVersion + "] (2 of 17 matches)",
		"- LORE-9 (decision, 14d) Agent-agnostic by design: MCP plus local SQLite",
		"- LORE-12 (principle, 14d) Many Gates lead to one Guild",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("payload missing %q:\n%s", want, got)
		}
	}
}

func TestRenderAppraise_ZeroMatchesRendersNothing(t *testing.T) {
	if got := RenderAppraise(nil, 0); got != "" {
		t.Errorf("zero matches must render empty payload, got %q", got)
	}
}

func TestRenderAppraise_CapsAtThreeLines(t *testing.T) {
	var lines []AppraiseLine
	for i := 0; i < 5; i++ {
		lines = append(lines, AppraiseLine{ID: "LORE-1", Kind: "research", Title: "t"})
	}
	got := RenderAppraise(lines, 5)
	if n := strings.Count(got, "- LORE-1"); n != MaxAppraiseLines {
		t.Errorf("rendered %d lines, want %d", n, MaxAppraiseLines)
	}
	if !strings.Contains(got, "(3 of 5 matches)") {
		t.Errorf("header should report 3 of 5 matches:\n%s", got)
	}
}

func TestPromptFromHookEnvelope_HarnessFixtures(t *testing.T) {
	// Claude Code and Codex pipe the identical UserPromptSubmit envelope
	// shape; only incidental fields differ. Both must yield .prompt.
	cases := map[string]string{
		"claude-code": `{"session_id":"abc-123","transcript_path":"/tmp/t.jsonl","cwd":"/work/repo","hook_event_name":"UserPromptSubmit","prompt":"how does the splice refusal work?"}`,
		"codex":       `{"session_id":"def-456","cwd":"/work/repo","hook_event_name":"UserPromptSubmit","prompt":"how does the splice refusal work?"}`,
	}
	for name, envelope := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := PromptFromHookEnvelope(strings.NewReader(envelope))
			if err != nil {
				t.Fatalf("PromptFromHookEnvelope: %v", err)
			}
			if want := "how does the splice refusal work?"; got != want {
				t.Errorf("prompt = %q, want %q", got, want)
			}
		})
	}
}

func TestPromptFromHookEnvelope_Errors(t *testing.T) {
	cases := map[string]string{
		"invalid json": `{not json`,
		"empty prompt": `{"prompt":"   ","hook_event_name":"UserPromptSubmit"}`,
		"no prompt":    `{"hook_event_name":"UserPromptSubmit"}`,
		"empty input":  ``,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := PromptFromHookEnvelope(strings.NewReader(in)); err == nil {
				t.Error("want error, got nil")
			}
		})
	}
}

func TestTruncateBytes_RuneSafe(t *testing.T) {
	s := strings.Repeat("é", 100) // 2 bytes per rune
	got := truncateBytes(s, 21)
	if len(got) > 21 {
		t.Errorf("len = %d, want <= 21", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("missing truncation marker: %q", got)
	}
	// Cutting 21-3=18 bytes lands on a rune boundary (9 full runes).
	if !strings.HasPrefix(got, strings.Repeat("é", 9)) {
		t.Errorf("rune boundary violated: %q", got)
	}
}
