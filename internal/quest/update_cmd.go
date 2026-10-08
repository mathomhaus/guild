package quest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
)

type UpdateInput struct {
	QuestID  string `json:"quest_id" jsonschema:"QUEST-NNN to update"`
	Subject  string `json:"subject,omitempty" jsonschema:"new subject"`
	Priority string `json:"priority,omitempty" jsonschema:"new priority (P0|P1|P2|P3)"`
	Campaign string `json:"campaign,omitempty" jsonschema:"new campaign name"`
	Epic     string `json:"epic,omitempty" jsonschema:"new campaign name (alias for campaign)"`
	Effort   string `json:"effort,omitempty" jsonschema:"new effort label"`

	// Append lists.
	Files      []string `json:"files,omitempty" jsonschema:"append to files"`
	Acceptance []string `json:"acceptance,omitempty" jsonschema:"append to acceptance criteria"`
	DependsOn  []string `json:"depends_on,omitempty" jsonschema:"append to depends_on"`
	Blocks     []string `json:"blocks,omitempty" jsonschema:"append to blocks"`

	// Replace lists. Conflicts with the append equivalent if both set.
	ReplaceFiles      []string `json:"replace_files,omitempty" jsonschema:"replace files list entirely"`
	ReplaceAcceptance []string `json:"replace_acceptance,omitempty" jsonschema:"replace acceptance list entirely"`
	ReplaceDependsOn  []string `json:"replace_depends_on,omitempty" jsonschema:"replace depends_on list entirely"`
	ReplaceBlocks     []string `json:"replace_blocks,omitempty" jsonschema:"replace blocks list entirely"`

	// Clear flags.
	ClearFiles      bool `json:"clear_files,omitempty" jsonschema:"clear files list"`
	ClearAcceptance bool `json:"clear_acceptance,omitempty" jsonschema:"clear acceptance list"`
	ClearDependsOn  bool `json:"clear_depends_on,omitempty" jsonschema:"clear depends_on list"`
	ClearBlocks     bool `json:"clear_blocks,omitempty" jsonschema:"clear blocks list"`

	Project string `json:"project,omitempty"`
}

type UpdateOutput struct {
	Quest *Quest `json:"quest"`
}

var UpdateCommand = &command.Command[UpdateInput, UpdateOutput]{
	Name:    "quest_update",
	CLIPath: []string{"quest", "update"},
	Short:   "modify a quest's spec after post",
	Long:    "Modify a quest's spec after post. Append lists via --files/-a/--depends-on/--blocks; replace via --replace-*; clear via --clear-*.",
	Args: []command.ArgSpec{
		{Name: "quest_id", Kind: command.ArgPositional, Type: command.ArgString, Required: true, Help: "QUEST-NNN"},
		{Name: "subject", Kind: command.ArgFlag, Type: command.ArgString, Help: "new subject"},
		{Name: "priority", Kind: command.ArgFlag, Type: command.ArgString, Help: "new priority"},
		{Name: "campaign", Kind: command.ArgFlag, Type: command.ArgString, Help: "new campaign name"},
		{Name: "epic", Kind: command.ArgFlag, Type: command.ArgString, Help: "new campaign name (alias for --campaign)"},
		{Name: "effort", Kind: command.ArgFlag, Type: command.ArgString, Help: "new effort"},
		{Name: "files", Kind: command.ArgFlag, Type: command.ArgStringSlice, Repeatable: true, Help: "append file (repeatable)"},
		{Name: "acceptance", Short: "a", Kind: command.ArgFlag, Type: command.ArgStringSlice, Repeatable: true, Help: "append acceptance criterion (repeatable)"},
		{Name: "depends_on", Kind: command.ArgFlag, Type: command.ArgStringSlice, Repeatable: true, Help: "append depends_on (repeatable)"},
		{Name: "blocks", Kind: command.ArgFlag, Type: command.ArgStringSlice, Repeatable: true, Help: "append blocks (repeatable)"},
		{Name: "replace_files", Kind: command.ArgFlag, Type: command.ArgStringSlice, Repeatable: true, Help: "replace files entirely"},
		{Name: "replace_acceptance", Kind: command.ArgFlag, Type: command.ArgStringSlice, Repeatable: true, Help: "replace acceptance entirely"},
		{Name: "replace_depends_on", Kind: command.ArgFlag, Type: command.ArgStringSlice, Repeatable: true, Help: "replace depends_on entirely"},
		{Name: "replace_blocks", Kind: command.ArgFlag, Type: command.ArgStringSlice, Repeatable: true, Help: "replace blocks entirely"},
		{Name: "clear_files", Kind: command.ArgFlag, Type: command.ArgBool, Help: "clear files list"},
		{Name: "clear_acceptance", Kind: command.ArgFlag, Type: command.ArgBool, Help: "clear acceptance list"},
		{Name: "clear_depends_on", Kind: command.ArgFlag, Type: command.ArgBool, Help: "clear depends_on list"},
		{Name: "clear_blocks", Kind: command.ArgFlag, Type: command.ArgBool, Help: "clear blocks list"},
		{Name: "project", Short: "p", Kind: command.ArgFlag, Type: command.ArgString, Help: "project override"},
	},
	Handler: func(ctx context.Context, d command.Deps, in UpdateInput) (UpdateOutput, error) {
		if strings.TrimSpace(in.QuestID) == "" {
			return UpdateOutput{}, errors.New("quest_id required")
		}
		db, err := d.OpenDB(ctx)
		if err != nil {
			return UpdateOutput{}, err
		}
		defer func() { _ = db.Close() }()
		pid, err := d.ResolveProj(ctx, in.Project)
		if err != nil {
			return UpdateOutput{}, err
		}
		campaign := in.Campaign
		if campaign == "" {
			campaign = in.Epic
		}
		params := UpdateParams{
			Subject:           in.Subject,
			Priority:          Priority(in.Priority),
			Epic:              campaign,
			Effort:            in.Effort,
			Files:             in.Files,
			Acceptance:        in.Acceptance,
			DependsOn:         in.DependsOn,
			Blocks:            in.Blocks,
			ReplaceFiles:      in.ReplaceFiles,
			ReplaceAcceptance: in.ReplaceAcceptance,
			ReplaceDependsOn:  in.ReplaceDependsOn,
			ReplaceBlocks:     in.ReplaceBlocks,
			ClearFiles:        in.ClearFiles,
			ClearAcceptance:   in.ClearAcceptance,
			ClearDependsOn:    in.ClearDependsOn,
			ClearBlocks:       in.ClearBlocks,
		}
		q, err := Update(ctx, db, pid, in.QuestID, params)
		if err != nil {
			return UpdateOutput{}, err
		}
		// Activity renewal: a daemon-mediated update on a quest this session
		// leased refreshes that lease. No-op without a daemon.
		renewLeaseActivity(ctx, d, pid, q.ID)
		return UpdateOutput{Quest: q}, nil
	},
	CLIFormat: func(s command.CLISink, o UpdateOutput) string { return formatUpdated(s, o) },
	MCPFormat: func(s command.MCPSink, o UpdateOutput) string { return formatUpdated(s, o) },
}

func formatUpdated(s lineListSink, o UpdateOutput) string {
	msg := fmt.Sprintf("updated %s", o.Quest.ID)
	return strings.TrimRight(s.Line("✏️", "[updated]", msg), "\n")
}
