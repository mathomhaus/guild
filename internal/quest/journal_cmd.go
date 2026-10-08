package quest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
)

type JournalInput struct {
	QuestID string `json:"quest_id" jsonschema:"QUEST-NNN you're working on"`
	Text    string `json:"text" jsonschema:"what you learned while working on THIS quest; dies when the quest clears"`
	Agent   string `json:"agent,omitempty" jsonschema:"agent id (defaults to 'agent')"`
	Project string `json:"project,omitempty"`
}

type JournalOutput struct {
	QuestID string `json:"quest_id"`
}

var JournalCommand = &command.Command[JournalInput, JournalOutput]{
	Name:    "quest_journal",
	CLIPath: []string{"quest", "journal"},
	Short:   "append a task-scoped journal note",
	Long:    "Task-scoped scratchpad. Dies when the quest clears. If it outlives the task, use lore_inscribe.",
	Args: []command.ArgSpec{
		{
			Name:     "quest_id",
			Kind:     command.ArgPositional,
			Type:     command.ArgString,
			Required: true,
			Help:     "QUEST-NNN you're working on",
		},
		{
			Name:     "text",
			Kind:     command.ArgPositional,
			Type:     command.ArgString,
			Required: true,
			Variadic: true,
			Help:     "journal note text — remaining args are joined with spaces on CLI",
		},
		{
			Name:    "agent",
			Kind:    command.ArgFlag,
			Type:    command.ArgString,
			Help:    "agent id (defaults to 'agent')",
			CLIOnly: true, // MCP always journals as 'agent'; CLI can override via $USER-style flag
		},
		{
			Name:  "project",
			Short: "p",
			Kind:  command.ArgFlag,
			Type:  command.ArgString,
			Help:  "project override",
		},
	},
	Handler: func(ctx context.Context, d command.Deps, in JournalInput) (JournalOutput, error) {
		if strings.TrimSpace(in.QuestID) == "" {
			return JournalOutput{}, errors.New("quest_id required")
		}
		if strings.TrimSpace(in.Text) == "" {
			return JournalOutput{}, errors.New("text required")
		}
		db, err := d.OpenDB(ctx)
		if err != nil {
			return JournalOutput{}, err
		}
		defer func() { _ = db.Close() }()

		pid, err := d.ResolveProj(ctx, in.Project)
		if err != nil {
			return JournalOutput{}, err
		}
		agent := strings.TrimSpace(in.Agent)
		if agent == "" {
			agent = "agent"
		}
		if err := Journal(ctx, db, pid, in.QuestID, agent, in.Text); err != nil {
			return JournalOutput{}, err
		}
		// Activity renewal: a daemon-mediated journal write on a quest this
		// session leased refreshes that lease. No-op without a daemon (nil
		// seam), so the no-daemon path stays byte-identical.
		renewLeaseActivity(ctx, d, pid, in.QuestID)
		return JournalOutput{QuestID: in.QuestID}, nil
	},
	CLIFormat:      func(s command.CLISink, o JournalOutput) string { return formatJournaled(s, o) },
	MCPFormat:      func(s command.MCPSink, o JournalOutput) string { return formatJournaled(s, o) },
	CLIErrorFormat: func(s command.CLISink, err error) (string, bool) { return formatJournalError(s, err) },
	MCPErrorFormat: func(s command.MCPSink, err error) (string, bool) { return formatJournalError(s, err) },
}

func formatJournaled(s lineListSink, o JournalOutput) string {
	return strings.TrimRight(s.Line("🗒️", "[journaled]", fmt.Sprintf("journaled on %s", o.QuestID)), "\n")
}

func formatJournalError(s lineListSink, err error) (string, bool) {
	if errors.Is(err, ErrNotFound) {
		return strings.TrimRight(s.Line("❌", "[err]", fmt.Sprintf("quest_journal: %v", err)), "\n"), true
	}
	return "", false
}
