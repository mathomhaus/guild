package hints

import (
	"strings"
	"testing"
)

// TestTrigger_InscribeLooksLikeQuest is the table-driven spec for the
// TODO/should-fix phrase detector.
func TestTrigger_InscribeLooksLikeQuest(t *testing.T) {
	cases := []struct {
		name    string
		title   string
		summary string
		want    bool
	}{
		{"plain title", "wal pragma notes", "details", false},
		{"todo in title", "TODO migrate to WAL", "notes", true},
		{"todo in summary", "wal pragma notes", "TODO follow up on PRAGMA wait", true},
		{"need to phrase", "something", "we need to fix the migration order", true},
		{"should fix phrase", "bug", "should fix race in clear cmd", true},
		{"must fix phrase", "bug", "must fix flaky test asap", true},
		{"we should phrase", "rationale", "we should consider batching writes", true},
		{"empty both", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := CallEvent{
				Tool: "lore_inscribe",
				Args: map[string]any{
					"title":   c.title,
					"summary": c.summary,
				},
			}
			got := triggerInscribeLooksLikeQuest(nil, ev)
			if got != c.want {
				t.Errorf("got %t, want %t", got, c.want)
			}
		})
	}
}

// TestTrigger_NoSessionStart spans the bootstrap/non-bootstrap matrix.
func TestTrigger_NoSessionStart(t *testing.T) {
	// Fresh session — non-bootstrap call should trigger.
	c := NewContext("sess", EraMCP)
	if !triggerNoSessionStart(c, CallEvent{Tool: "lore_inscribe"}) {
		t.Error("expected trigger on fresh session")
	}

	// Bootstrap tools themselves do NOT self-trigger.
	if triggerNoSessionStart(c, CallEvent{Tool: "guild_session_start"}) {
		t.Error("trigger should not fire on guild_session_start itself")
	}
	if triggerNoSessionStart(c, CallEvent{Tool: "quest_bounties"}) {
		t.Error("trigger should not fire on quest_bounties itself")
	}

	// After seeing a session_start, subsequent calls are exempt.
	c.RecordEvent(CallEvent{Tool: "guild_session_start"})
	if triggerNoSessionStart(c, CallEvent{Tool: "lore_inscribe"}) {
		t.Error("trigger should suppress after session_start")
	}
}

// TestTrigger_SessionEndWithoutBrief verifies the 30-call threshold.
func TestTrigger_SessionEndWithoutBrief(t *testing.T) {
	c := NewContext("sess", EraMCP)
	// Below threshold.
	for i := 0; i < 10; i++ {
		c.RecordEvent(CallEvent{Tool: "x"})
	}
	if triggerSessionEndWithoutBrief(c, CallEvent{Tool: "y"}) {
		t.Error("should not trigger below call threshold")
	}
	// Reach threshold.
	for i := 0; i < 25; i++ {
		c.RecordEvent(CallEvent{Tool: "x"})
	}
	if !triggerSessionEndWithoutBrief(c, CallEvent{Tool: "y"}) {
		t.Error("should trigger above threshold with no brief")
	}
	// A brief in history suppresses.
	c.RecordEvent(CallEvent{Tool: "quest_brief"})
	if triggerSessionEndWithoutBrief(c, CallEvent{Tool: "y"}) {
		t.Error("should not trigger after a quest_brief was recorded")
	}
	// Self-suppression: quest_brief call itself does not trigger.
	c2 := NewContext("sess2", EraMCP)
	for i := 0; i < 40; i++ {
		c2.RecordEvent(CallEvent{Tool: "x"})
	}
	if triggerSessionEndWithoutBrief(c2, CallEvent{Tool: "quest_brief"}) {
		t.Error("quest_brief should not trigger itself")
	}
}

// TestTrigger_SlugQuery preserves the existing slug-regex behavior on
// top of the zero-result gate (QUEST-73).
func TestTrigger_SlugQuery(t *testing.T) {
	cases := []struct {
		q    string
		want bool
	}{
		{"", false},
		{"multi token query", false}, // whitespace disqualifies
		{"hyphen-slug-term", true},   // slug
		{"CamelCase", false},         // not slug
		{"QUEST-42", true},           // quest id
		{"quest-42", true},           // lowercase quest-42 is slug-matching
		{"simple", false},            // single token but no hyphen
	}
	for _, c := range cases {
		// Shape cases assume the zero-result signal is present. That is
		// what production plumbs for a miss; the regex check is the second
		// line of defense.
		got := triggerSlugQuery(nil, CallEvent{
			Args: map[string]any{
				"query":       c.q,
				zeroResultKey: true,
			},
		})
		if got != c.want {
			t.Errorf("q=%q zero=true: got %t, want %t", c.q, got, c.want)
		}
	}
}

// TestTrigger_SlugQuery_RequiresZeroResultSignal locks in the QUEST-73
// contract: even a slug-shaped query must NOT fire when the search
// succeeded or when the handler forgot to set the signal.
func TestTrigger_SlugQuery_RequiresZeroResultSignal(t *testing.T) {
	mk := func(args map[string]any) CallEvent {
		args["query"] = "QUEST-42"
		return CallEvent{Args: args}
	}
	// Signal absent — no fire (handler not wired; old behavior).
	if triggerSlugQuery(nil, mk(map[string]any{})) {
		t.Error("no zero-result key → should not fire")
	}
	// Signal present but false (hits returned) — no fire.
	if triggerSlugQuery(nil, mk(map[string]any{zeroResultKey: false})) {
		t.Error("zero-result=false → should not fire on successful search")
	}
	// Signal present and true — fire.
	if !triggerSlugQuery(nil, mk(map[string]any{zeroResultKey: true})) {
		t.Error("zero-result=true on slug-shaped query → should fire")
	}
}

// TestTrigger_JournalOutsideAccepted checks the session-scoped match.
func TestTrigger_JournalOutsideAccepted(t *testing.T) {
	c := NewContext("sess", EraMCP)
	// Journaling a quest with no prior accept → fire.
	if !triggerJournalOutsideAccepted(c, CallEvent{
		Args: map[string]any{"quest_id": "QUEST-10"},
	}) {
		t.Error("expected fire on journal without accept")
	}
	// Accept a different quest → still fire on our target.
	c.RecordEvent(CallEvent{Tool: "quest_accept",
		Args: map[string]any{"quest_id": "QUEST-99"}})
	if !triggerJournalOutsideAccepted(c, CallEvent{
		Args: map[string]any{"quest_id": "QUEST-10"},
	}) {
		t.Error("accept on unrelated quest should still fire")
	}
	// Accept the target → suppressed.
	c.RecordEvent(CallEvent{Tool: "quest_accept",
		Args: map[string]any{"quest_id": "QUEST-10"}})
	if triggerJournalOutsideAccepted(c, CallEvent{
		Args: map[string]any{"quest_id": "QUEST-10"},
	}) {
		t.Error("accept on target should suppress")
	}
	// Case-insensitive match.
	if triggerJournalOutsideAccepted(c, CallEvent{
		Args: map[string]any{"quest_id": "quest-10"},
	}) {
		t.Error("case mismatch should still suppress")
	}
}

// TestTrigger_NoBrief24h relies on the handler-stuffed Extras signal.
func TestTrigger_NoBrief24h(t *testing.T) {
	// Missing key → no fire.
	if triggerNoBrief24h(nil, CallEvent{}) {
		t.Error("unset signal should not fire")
	}
	// Key present, false → no fire.
	if triggerNoBrief24h(nil, CallEvent{
		Args: map[string]any{briefHintSessionKey: false},
	}) {
		t.Error("false signal should not fire")
	}
	// Key present, true → fire.
	if !triggerNoBrief24h(nil, CallEvent{
		Args: map[string]any{briefHintSessionKey: true},
	}) {
		t.Error("true signal should fire")
	}
}

// TestTrigger_InscribeWithoutAppraise verifies the 5-call window.
func TestTrigger_InscribeWithoutAppraise(t *testing.T) {
	c := NewContext("sess", EraMCP)
	// No prior appraise → fire.
	c.RecordEvent(CallEvent{Tool: "lore_inscribe"})
	if !triggerInscribeWithoutAppraise(c, CallEvent{Tool: "lore_inscribe"}) {
		t.Error("expected fire with no prior appraise")
	}
	// Recent appraise within window → suppress.
	c.RecordEvent(CallEvent{Tool: "lore_appraise"})
	c.RecordEvent(CallEvent{Tool: "lore_inscribe"})
	if triggerInscribeWithoutAppraise(c, CallEvent{Tool: "lore_inscribe"}) {
		t.Error("expected suppression with recent appraise")
	}
}

// TestTrigger_ClearWithoutReportDetail checks the 20-word boundary.
func TestTrigger_ClearWithoutReportDetail(t *testing.T) {
	short := "done"
	long := strings.Repeat("word ", 25)
	cases := []struct {
		report string
		want   bool
	}{
		{"", true},    // empty → trivially thin
		{short, true}, // 1 word
		{long, false}, // 25 words
	}
	for i, c := range cases {
		got := triggerClearWithoutReportDetail(nil, CallEvent{
			Args: map[string]any{"report": c.report},
		})
		if got != c.want {
			t.Errorf("case %d: got %t, want %t (words=%d)",
				i, got, c.want, wordCount(c.report))
		}
	}
}

// TestTrigger_InscribeWithoutTransferReasoning exercises the QUEST-167
// rule against the Spike 5 audit corpus (2026-04-22): gold-standard
// entries must NOT fire, anti-pattern entries MUST fire, and the edge
// cases (empty informs, very-short summary, trivial-transfer escape)
// stay suppressed.
func TestTrigger_InscribeWithoutTransferReasoning(t *testing.T) {
	// Full ancestor list — real shape of the reflected []string arg.
	informs := []string{"186"}

	cases := []struct {
		name    string
		informs any
		summary string
		want    bool
	}{
		// --- edge cases (must NOT fire) --------------------------------
		{
			name:    "no informs → no fire",
			informs: []string{},
			summary: "pwdcheck uses stdlib flag; same rationale applies here in this body text, long enough to be meaningful",
			want:    false,
		},
		{
			name:    "nil informs → no fire",
			informs: nil,
			summary: "long enough summary body without any informs attached so the rule has nothing to enforce",
			want:    false,
		},
		{
			name:    "very short summary → no fire (guard)",
			informs: informs,
			summary: "see LORE-186",
			want:    false,
		},
		{
			name:    "trivial-transfer escape — no delta",
			informs: informs,
			summary: "adopts LORE-189 stdlib flag choice; same shape, no delta. Zero-dep bias preserved.",
			want:    false,
		},
		{
			name:    "trivial-transfer escape — same approach",
			informs: informs,
			summary: "pwdcheck uses stdlib testing only, adopted from LORE-190. same approach applied to a parallel Check() surface with no new dependency.",
			want:    false,
		},

		// --- anti-patterns (MUST fire) ---------------------------------
		{
			name:    "LORE-258 anti-pattern (stdlib flag adoption, bare defer)",
			informs: informs,
			summary: "pwdcheck uses stdlib flag, adopted from LORE-189 (passgen). Same rationale applies: 5 flags, single command, zero-dep bias. cobra/pflag put off unless we add sub-commands or GNU-style long flags (neither in Phase 1).",
			want:    true,
		},
		{
			name:    "LORE-259 anti-pattern (stdlib testing adoption)",
			informs: informs,
			summary: "pwdcheck uses stdlib testing only, adopted from LORE-190 (passgen). Same rationale: table tests via t.Run cover scope, testify adds a dep for no functional gain at this size.",
			want:    true,
		},
		{
			name:    "LORE-261 anti-pattern (layout adoption, bare 'adopts')",
			informs: informs,
			summary: "pwdcheck uses cmd/pwdcheck/main.go (thin wire+exit-map) + internal/pwdcheck/{options,symbols,rules,check,input}. Adopts LORE-185 pattern; same rationale applies.",
			want:    true,
		},

		// --- gold-standards (must NOT fire) ----------------------------
		{
			name:    "LORE-265 gold (argv-leakage inversion — 'because' marker)",
			informs: informs,
			summary: "LORE-186 pitfall #4 dismissed argv leakage because passgen's flags aren't secret. pwdcheck's argv positional IS a secret — users passing passwords on the CLI expose them via ps. Phase 1 mitigation: README caveat + recommend stdin for real use. No code change.",
			want:    false,
		},
		{
			name:    "LORE-278 gold (declined adoption — 'delta' marker)",
			informs: informs,
			summary: "Phase 2 architecture is a DELTA from LORE-185. Invariants preserved: cmd/+internal/ layout unchanged. Signature locked; main loops N times and emits results before the threshold warning.",
			want:    false,
		},
		{
			name:    "LORE-262 gold (repurposing — 'repurposed' marker)",
			informs: informs,
			summary: "ReadPasswords takes io.Reader for the input source. Adopts LORE-192's injection discipline repurposed from CSPRNG to input — same testability argument applies.",
			want:    false,
		},

		// --- marker coverage (must NOT fire for each marker) -----------
		{
			name:    "marker: extends (boundary case)",
			informs: informs,
			summary: "Extends LORE-191 discipline to pwdcheck. Phase 2+ features layer on; Check() signature is locked at Phase 1 ship.",
			want:    false,
		},
		{
			name:    "marker: reverses",
			informs: informs,
			summary: "reverses LORE-186's dismissal because the prior domain assumption no longer holds in this binary.",
			want:    false,
		},
		{
			name:    "marker: adapts",
			informs: informs,
			summary: "adapts LORE-192's injection pattern to the reader side — Generate takes a Writer, Check takes a Reader; same testability rationale holds across both seams.",
			want:    false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := CallEvent{
				Tool: "lore_inscribe",
				Args: map[string]any{
					"informs": c.informs,
					"summary": c.summary,
				},
			}
			got := triggerInscribeWithoutTransferReasoning(nil, ev)
			if got != c.want {
				t.Errorf("got %t, want %t — summary=%q", got, c.want, c.summary)
			}
		})
	}
}

// TestTrigger_InscribeWithoutTransferReasoning_InformsShapes verifies
// both argument shapes (typed []string from the Go handler, []any from
// a JSON-unmarshal path) count as "informs is non-empty".
func TestTrigger_InscribeWithoutTransferReasoning_InformsShapes(t *testing.T) {
	summary := "pwdcheck uses stdlib flag, adopted from LORE-189. Same rationale: 5 flags, single command, zero-dep bias."

	cases := []struct {
		name    string
		informs any
		want    bool
	}{
		{"typed []string with entry", []string{"186"}, true},
		{"typed []string empty", []string{}, false},
		{"typed []string whitespace-only entry", []string{"  "}, false},
		{"[]any with entry", []any{"186"}, true},
		{"[]any empty", []any{}, false},
		{"nil arg absent", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := map[string]any{"summary": summary}
			if c.informs != nil {
				args["informs"] = c.informs
			}
			got := triggerInscribeWithoutTransferReasoning(nil, CallEvent{
				Tool: "lore_inscribe",
				Args: args,
			})
			if got != c.want {
				t.Errorf("got %t, want %t", got, c.want)
			}
		})
	}
}
