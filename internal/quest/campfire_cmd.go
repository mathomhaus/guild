package quest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
)

type CampfireInput struct {
	QuestID      string   `json:"quest_id" jsonschema:"QUEST-NNN to snapshot"`
	Hypothesis   string   `json:"hypothesis,omitempty" jsonschema:"current working hypothesis"`
	Tried        []string `json:"tried,omitempty" jsonschema:"approaches tried so far (array)"`
	Next         string   `json:"next,omitempty" jsonschema:"next step to try"`
	TokenWarning bool     `json:"token_warning,omitempty" jsonschema:"true when context window is running low"`
	Agent        string   `json:"agent,omitempty" jsonschema:"agent id (defaults to 'agent')"`
	Project      string   `json:"project,omitempty"`
}

type CampfireOutput struct {
	QuestID string `json:"quest_id"`
}

var CampfireCommand = &command.Command[CampfireInput, CampfireOutput]{
	Name:    "quest_campfire",
	CLIPath: []string{"quest", "campfire"},
	Short:   "save working state before compaction",
	Long:    "Save a structured working-state checkpoint (hypothesis, tried, next, token warning) before the context compacts. Distinct from journal — campfire is a restart beacon.",
	Args: []command.ArgSpec{
		{Name: "quest_id", Kind: command.ArgPositional, Type: command.ArgString, Required: true, Help: "QUEST-NNN to snapshot"},
		{Name: "hypothesis", Kind: command.ArgFlag, Type: command.ArgString, Help: "current working hypothesis"},
		{Name: "tried", Kind: command.ArgFlag, Type: command.ArgStringSlice, Repeatable: true, Help: "approach tried (repeatable)"},
		{Name: "next", Kind: command.ArgFlag, Type: command.ArgString, Help: "next step to try"},
		{Name: "token_warning", Kind: command.ArgFlag, Type: command.ArgBool, Help: "context window is running low"},
		{Name: "agent", Kind: command.ArgFlag, Type: command.ArgString, Help: "agent id (defaults to 'agent')", CLIOnly: true},
		{Name: "project", Short: "p", Kind: command.ArgFlag, Type: command.ArgString, Help: "project override"},
	},
	Handler: func(ctx context.Context, d command.Deps, in CampfireInput) (CampfireOutput, error) {
		if strings.TrimSpace(in.QuestID) == "" {
			return CampfireOutput{}, errors.New("quest_id required")
		}
		agent := strings.TrimSpace(in.Agent)
		if agent == "" {
			agent = "agent"
		}
		params := CampfireParams{
			Hypothesis:   in.Hypothesis,
			Tried:        in.Tried,
			Next:         in.Next,
			TokenWarning: in.TokenWarning,
			Agent:        agent,
		}
		if params.Empty() {
			return CampfireOutput{}, errors.New("at least one of --hypothesis, --tried, --next, --token-warning required")
		}
		db, err := d.OpenDB(ctx)
		if err != nil {
			return CampfireOutput{}, err
		}
		defer func() { _ = db.Close() }()
		pid, err := d.ResolveProj(ctx, in.Project)
		if err != nil {
			return CampfireOutput{}, err
		}
		if err := Campfire(ctx, db, pid, in.QuestID, params); err != nil {
			return CampfireOutput{}, err
		}
		// Activity renewal: a daemon-mediated campfire write on a quest this
		// session leased refreshes that lease. No-op without a daemon.
		renewLeaseActivity(ctx, d, pid, in.QuestID)
		return CampfireOutput{QuestID: strings.ToUpper(in.QuestID)}, nil
	},
	CLIFormat:      func(s command.CLISink, o CampfireOutput) string { return formatCampfire(s, o) },
	MCPFormat:      func(s command.MCPSink, o CampfireOutput) string { return formatCampfire(s, o) },
	CLIErrorFormat: func(s command.CLISink, err error) (string, bool) { return formatCampfireError(s, err) },
	MCPErrorFormat: func(s command.MCPSink, err error) (string, bool) { return formatCampfireError(s, err) },
}

func formatCampfire(s lineListSink, o CampfireOutput) string {
	return strings.TrimRight(s.Line("🏕️", "[campfire]", "campfire saved for "+o.QuestID), "\n")
}

func formatCampfireError(s lineListSink, err error) (string, bool) {
	if errors.Is(err, ErrNotFound) {
		return strings.TrimRight(s.Line("❌", "[err]", fmt.Sprintf("quest_campfire: %v", err)), "\n"), true
	}
	return "", false
}
