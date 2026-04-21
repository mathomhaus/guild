package hints

// Rule is one launch-set hint definition: trigger/follow-through logic
// paired with the DB-backed metadata (severity, template, cooldown). The
// Engine composes rules with the DB row loaded at startup — DB fields may
// mutate (enabled flag, demoted severity) without recompiling.
//
// Implementations live in detectors.go. Each rule is pure — no IO, no DB —
// so the table-driven tests in rules_test.go can run without a database.
type Rule struct {
	// ID is the stable string id (e.g. "inscribe-looks-like-quest").
	// Matches the hints.rule_id DB column. Kept human-readable on purpose
	// so hint_fires logs are legible without a join.
	ID string

	// TriggerTool is the tool name that gates evaluation. "*" matches
	// every tool (used by no-session-start and session-end-without-brief).
	TriggerTool string

	// Trigger is the fire-or-not detector for this rule. Called only
	// after TriggerTool matches the event's tool name. Pure function.
	Trigger func(ctx *Context, ev CallEvent) bool

	// FollowThrough is the did-the-agent-do-the-suggested-action check.
	// Called against events AFTER the fire — the Engine tracks a pending
	// fire for 10 subsequent calls, and scores hit=true on the first
	// event where FollowThrough returns true.
	FollowThrough func(ctx *Context, ev CallEvent) bool

	// Caveat is documentation-only text embedded in the rule so operators
	// reading rules.Definitions() see the known pitfalls. Mirrors the
	// QUEST-58 acceptance "Per-rule caveats to document in code".
	Caveat string
}

// Definitions returns the 9 launch-set rules with their Trigger /
// FollowThrough closures bound. The slice is constructed fresh on each
// call — tests may mutate elements without affecting other test cases.
//
// Rule order here is not semantically meaningful: the Engine keys on
// Rule.ID after loading DB metadata via Store.LoadRules.
func Definitions() []Rule {
	return []Rule{
		// 💡 hint (keep) — 6 rules.
		{
			ID:            "inscribe-looks-like-quest",
			TriggerTool:   "lore_inscribe",
			Trigger:       triggerInscribeLooksLikeQuest,
			FollowThrough: followQuestPostAfterInscribe,
			Caveat:        "Pair-matching is loose — any subsequent quest_post counts as hit regardless of topic relevance; true precision <85.7% per ENTRY-29.",
		},
		{
			ID:            "no-session-start",
			TriggerTool:   "*",
			Trigger:       triggerNoSessionStart,
			FollowThrough: followSessionBootstrap,
			Caveat:        "Expected near-zero fires in the MCP era; kept for Bash-CLI users per ENTRY-29.",
		},
		{
			ID:            "session-end-without-brief",
			TriggerTool:   "*",
			Trigger:       triggerSessionEndWithoutBrief,
			FollowThrough: followQuestBrief,
			Caveat:        "Fires once at the 30-call mark; cooldown=10 prevents immediate re-fire.",
		},
		{
			ID:            "slug-query",
			TriggerTool:   "lore_appraise",
			Trigger:       triggerSlugQuery,
			FollowThrough: followQuestListOrScroll,
			Caveat:        "Preserves the existing AppraiseOutput.MissHint behavior — the hint-engine rule is the canonical fire path now.",
		},
		{
			ID:            "journal-outside-accepted",
			TriggerTool:   "quest_journal",
			Trigger:       triggerJournalOutsideAccepted,
			FollowThrough: followQuestAccept,
			Caveat:        "Over-fires on continuation sessions (quest accepted in a prior session); cross-session state is out of scope for v1.",
		},
		{
			ID:            "no-brief-24h",
			TriggerTool:   "quest_fulfill",
			Trigger:       triggerNoBrief24h,
			FollowThrough: followQuestBrief,
			Caveat:        "Era-aware severity: 💡 hint on MCP, ℹ️ fyi on Bash CLI (18.7pp gap in ENTRY-29 calibration). TriggerTool is canonical quest_fulfill; quest_clear alias is handled by quest.CanonicalToolName in the engine.",
		},

		// ℹ️ fyi (demote) — 3 rules.
		{
			ID:            "inscribe-without-appraise",
			TriggerTool:   "lore_inscribe",
			Trigger:       triggerInscribeWithoutAppraise,
			FollowThrough: followLoreAppraise,
			Caveat:        "Demoted to fyi per ENTRY-29; 18.8% base hit rate means signal is weak but positive.",
		},
		{
			ID:            "clear-without-report-detail",
			TriggerTool:   "quest_fulfill",
			Trigger:       triggerClearWithoutReportDetail,
			FollowThrough: followQuestUpdateOrJournal,
			Caveat:        "Demoted to fyi; short reports are legitimate for trivial clears. TriggerTool is canonical quest_fulfill; quest_clear alias is handled by quest.CanonicalToolName in the engine.",
		},
		{
			ID:            "principle-too-long",
			TriggerTool:   "lore_inscribe",
			Trigger:       triggerPrincipleTooLong,
			FollowThrough: followPrincipleShortened,
			Caveat:        "Demoted to fyi; the 60-word bound mirrors lore's PrincipleMaxWordsDefault hygiene warning.",
		},
	}
}
