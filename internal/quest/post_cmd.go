package quest

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/lore"
)

// specHintWordThreshold is the exclusive lower bound: more than this many
// words across all acceptance bullets fires the soft-warn hint.
// Boundary semantics: >150 (strictly greater), not >=150.
const specHintWordThreshold = 150

// specHintBulletThreshold is the inclusive lower bound: this many or more
// bullets fires the soft-warn hint.
// Boundary semantics: >=5 (five or more).
const specHintBulletThreshold = 5

// specHintKeywordRe matches design-language words that suggest rich
// rationale is present. Case-insensitive word boundary match.
var specHintKeywordRe = regexp.MustCompile(`(?i)\b(approach|propose|schema|protocol|algorithm)\b`)

type PostInput struct {
	Subject    string   `json:"subject" jsonschema:"one-line task summary"`
	Priority   string   `json:"priority,omitempty" jsonschema:"P0|P1|P2|P3 (default P2)"`
	Campaign   string   `json:"campaign,omitempty" jsonschema:"campaign name to group related quests"`
	Epic       string   `json:"epic,omitempty" jsonschema:"campaign name (alias for campaign)"`
	Effort     string   `json:"effort,omitempty" jsonschema:"effort label"`
	Files      []string `json:"files,omitempty" jsonschema:"file paths the task will touch"`
	Acceptance []string `json:"acceptance,omitempty" jsonschema:"acceptance criteria"`
	DependsOn  []string `json:"depends_on,omitempty" jsonschema:"QUEST-IDs this depends on"`
	Rework     string   `json:"rework_of,omitempty" jsonschema:"QUEST-X this is rework of"`
	Spec       string   `json:"spec,omitempty" jsonschema:"optional design rationale — atomically inscribes a kind=decision lore entry with full context"`
	Project    string   `json:"project,omitempty"`
}

type PostOutput struct {
	Quest     *Quest `json:"quest"`
	SpecEntry *int64 `json:"spec_entry_id,omitempty"`
	HintLine  string `json:"hint_line,omitempty"`
}

var PostCommand = &command.Command[PostInput, PostOutput]{
	Name:    "quest_post",
	CLIPath: []string{"quest", "post"},
	Short:   "create a new quest",
	Long: "Create a quest another agent can accept without human follow-up — well-specced quest = no human follow-up needed to execute it. " +
		"Include rich WHY + HOW (rationale, approach, constraints) so a cold agent executes without re-deriving context from chat. " +
		"Pass spec=... to atomically attach a kind=decision lore entry with full rationale (QUEST-63).",
	Args: []command.ArgSpec{
		{
			Name:     "subject",
			Kind:     command.ArgPositional,
			Type:     command.ArgString,
			Required: true,
			Variadic: true,
			Help:     "one-line task summary (remaining positional args joined on CLI)",
		},
		{Name: "priority", Kind: command.ArgFlag, Type: command.ArgString, Help: "priority tag (P0|P1|P2|P3) — default P2"},
		{Name: "campaign", Kind: command.ArgFlag, Type: command.ArgString, Help: "campaign name to group related quests"},
		{Name: "epic", Kind: command.ArgFlag, Type: command.ArgString, Help: "campaign name alias (--epic works as --campaign)"},
		{Name: "effort", Kind: command.ArgFlag, Type: command.ArgString, Help: "effort label"},
		{Name: "files", Kind: command.ArgFlag, Type: command.ArgStringSlice, Repeatable: true, Help: "file path (repeatable)"},
		{Name: "acceptance", Short: "a", Kind: command.ArgFlag, Type: command.ArgStringSlice, Repeatable: true, Help: "acceptance criterion (repeatable)"},
		{Name: "depends_on", Kind: command.ArgFlag, Type: command.ArgStringSlice, Repeatable: true, Help: "QUEST-ID this depends on (repeatable)"},
		{Name: "rework_of", CLIFlagName: "rework", Kind: command.ArgFlag, Type: command.ArgString, Help: "QUEST-X this is rework of"},
		{Name: "spec", Kind: command.ArgFlag, Type: command.ArgString, Help: "design rationale — atomically inscribes a kind=decision lore entry"},
		{Name: "project", Short: "p", Kind: command.ArgFlag, Type: command.ArgString, Help: "project override"},
	},
	Handler: func(ctx context.Context, d command.Deps, in PostInput) (PostOutput, error) {
		subject := strings.TrimSpace(in.Subject)
		if subject == "" {
			return PostOutput{}, errors.New("subject required")
		}
		priority := Priority(in.Priority)
		if priority == "" {
			priority = "P2"
		}
		db, err := d.OpenDB(ctx)
		if err != nil {
			return PostOutput{}, err
		}
		defer func() { _ = db.Close() }()
		pid, err := d.ResolveProj(ctx, in.Project)
		if err != nil {
			return PostOutput{}, err
		}
		campaign := in.Campaign
		if campaign == "" {
			campaign = in.Epic
		}
		q, err := Post(ctx, db, pid, PostParams{
			Subject:    subject,
			Priority:   priority,
			Epic:       campaign,
			Effort:     in.Effort,
			Files:      in.Files,
			Acceptance: in.Acceptance,
			DependsOn:  in.DependsOn,
			ReworkOf:   in.Rework,
		})
		if err != nil {
			return PostOutput{}, err
		}

		spec := strings.TrimSpace(in.Spec)
		if spec != "" && d.OpenLoreDB != nil {
			// Atomic spec dance: inscribe lore entry, then append spec bullet
			// to quest acceptance.
			//
			// Linking strategy: lore.LinkEntries links two lore entries
			// (entry→entry). Quests live in task_status, not entries, so
			// lore→quest linking via LinkEntries is not possible without a
			// placeholder entry. Instead we use prompted_by on the entry
			// (stores "QUEST-N") as the provenance mechanism and append a
			// "spec: LORE-N" bullet to quest acceptance. This matches the
			// quest scroll display of prompted_by and is sufficient for
			// forward/backward navigation between entry and quest.
			ldb, loreErr := d.OpenLoreDB(ctx)
			if loreErr != nil {
				return PostOutput{}, fmt.Errorf("quest: post: open lore db: %w", loreErr)
			}
			defer func() { _ = ldb.Close() }()

			topic := strings.TrimSpace(in.Campaign)
			if topic == "" {
				topic = strings.TrimSpace(in.Epic)
			}
			if topic == "" {
				topic = "quest-spec"
			}
			inscribeRes, inscribeErr := lore.Inscribe(ctx, ldb, &lore.InscribeParams{
				ProjectID:  pid,
				Kind:       lore.KindDecision,
				Title:      subject,
				Summary:    spec,
				Topic:      topic,
				PromptedBy: q.ID,
				NoWarn:     true, // suppress principle-bloat warning (not a principle)
				// Spec entries decay like any other decision entry, so
				// honor the configured per-kind window here too.
				ValidDaysByKind: d.ResolveLoreValidDays(),
			})
			if inscribeErr != nil {
				return PostOutput{}, fmt.Errorf("quest: post: inscribe spec: %w", inscribeErr)
			}
			entryID := inscribeRes.Entry.ID

			// Append "spec: LORE-N" bullet to quest acceptance via Update.
			specBullet := fmt.Sprintf("spec: %s", lore.EntryID(entryID))
			_, updateErr := Update(ctx, db, pid, q.ID, UpdateParams{
				Acceptance: []string{specBullet},
			})
			if updateErr != nil {
				return PostOutput{}, fmt.Errorf("quest: post: append spec bullet: %w", updateErr)
			}

			// Reload quest to reflect the appended acceptance bullet.
			reloaded, reloadErr := Load(ctx, db, pid, q.ID)
			if reloadErr != nil {
				return PostOutput{}, fmt.Errorf("quest: post: reload after spec: %w", reloadErr)
			}
			q = reloaded

			return PostOutput{Quest: q, SpecEntry: &entryID}, nil
		}

		out := PostOutput{Quest: q}

		// Soft-warn heuristic: when spec is absent and acceptance looks
		// rich, emit a single hint line. Agent may ignore — no block.
		// Only fires when spec is absent (empty spec with rich acceptance).
		if spec == "" {
			hint := specHint(in.Acceptance)
			out.HintLine = hint
		}

		return out, nil
	},
	CLIFormat: func(s command.CLISink, o PostOutput) string { return formatPosted(s, o) },
	MCPFormat: func(s command.MCPSink, o PostOutput) string { return formatPosted(s, o) },
}

// specHint returns a hint line when the acceptance criteria look rich
// enough to warrant a spec= param, or "" when no hint should fire.
// Heuristic thresholds (tunable):
//   - word count: strictly >150 words across all bullets
//   - bullet count: >=5 bullets
//   - design keywords: presence of approach|propose|schema|protocol|algorithm
func specHint(acceptance []string) string {
	if len(acceptance) == 0 {
		return ""
	}
	totalWords := 0
	for _, a := range acceptance {
		totalWords += len(strings.Fields(a))
	}
	bulletCount := len(acceptance)

	hasKeyword := false
	for _, a := range acceptance {
		if specHintKeywordRe.MatchString(a) {
			hasKeyword = true
			break
		}
	}

	if totalWords > specHintWordThreshold || bulletCount >= specHintBulletThreshold || hasKeyword {
		return fmt.Sprintf(
			"this acceptance is %d words across %d bullets — consider spec=\"...\" to auto-inscribe the spec as a lore decision entry; the current skeleton loses chat-scroll context otherwise.",
			totalWords, bulletCount,
		)
	}
	return ""
}

func formatPosted(s lineListSink, o PostOutput) string {
	q := o.Quest
	suffix := ""
	if q.ReworkOf != "" {
		suffix = " (rework of " + q.ReworkOf + ")"
	}
	var msg string
	if o.SpecEntry != nil {
		msg = fmt.Sprintf("posted %s with spec %s: %s%s", q.ID, lore.EntryID(*o.SpecEntry), q.Subject, suffix)
	} else {
		msg = fmt.Sprintf("posted %s: %s%s", q.ID, q.Subject, suffix)
	}
	out := strings.TrimRight(s.Line("➕", "[posted]", msg), "\n")
	if o.HintLine != "" {
		out += "\n" + strings.TrimRight(s.Line("💡", "[hint]", o.HintLine), "\n")
	}
	return out
}
