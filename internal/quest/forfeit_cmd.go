package quest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
)

type ForfeitInput struct {
	QuestID string `json:"quest_id" jsonschema:"QUEST-NNN to release"`
	Note    string `json:"note,omitempty" jsonschema:"reason the claim is being released (optional)"`
	Project string `json:"project,omitempty"`
}

type ForfeitOutput struct {
	Quest       *Quest `json:"quest"`
	HasNote     bool   `json:"has_note"`
	AlreadyNext bool   `json:"already_next,omitempty"`
}

var ForfeitCommand = &command.Command[ForfeitInput, ForfeitOutput]{
	Name:    "quest_forfeit",
	CLIPath: []string{"quest", "forfeit"},
	Short:   "release a claim without completing",
	Long:    "Release a claimed quest back to the queue. Use when blocked or ceding to another agent. Only acts on status=in_progress quests — refuses on done and no-ops on next.",
	Args: []command.ArgSpec{
		{
			Name:     "quest_id",
			Kind:     command.ArgPositional,
			Type:     command.ArgString,
			Required: true,
			Help:     "QUEST-NNN to release",
		},
		{
			Name: "note",
			Kind: command.ArgFlag,
			Type: command.ArgString,
			Help: "reason the claim is being released (optional)",
		},
		{
			Name:  "project",
			Short: "p",
			Kind:  command.ArgFlag,
			Type:  command.ArgString,
			Help:  "project override",
		},
	},
	Handler: func(ctx context.Context, d command.Deps, in ForfeitInput) (ForfeitOutput, error) {
		if strings.TrimSpace(in.QuestID) == "" {
			return ForfeitOutput{}, errors.New("quest_id required")
		}
		db, err := d.OpenDB(ctx)
		if err != nil {
			return ForfeitOutput{}, err
		}
		defer func() { _ = db.Close() }()

		pid, err := d.ResolveProj(ctx, in.Project)
		if err != nil {
			return ForfeitOutput{}, err
		}
		res, err := Forfeit(ctx, db, pid, in.QuestID, in.Note)
		if err != nil {
			return ForfeitOutput{}, err
		}
		return ForfeitOutput{
			Quest:       res.Quest,
			HasNote:     !res.AlreadyNext && strings.TrimSpace(in.Note) != "",
			AlreadyNext: res.AlreadyNext,
		}, nil
	},
	CLIFormat:      func(s command.CLISink, o ForfeitOutput) string { return formatForfeited(s, o) },
	MCPFormat:      func(s command.MCPSink, o ForfeitOutput) string { return formatForfeited(s, o) },
	CLIErrorFormat: func(s command.CLISink, err error) (string, bool) { return formatForfeitError(s, err) },
	MCPErrorFormat: func(s command.MCPSink, err error) (string, bool) { return formatForfeitError(s, err) },
}

func formatForfeited(s lineListSink, o ForfeitOutput) string {
	if o.AlreadyNext {
		msg := fmt.Sprintf("%s is already unclaimed — nothing to forfeit", o.Quest.ID)
		return strings.TrimRight(s.Line("✅", "[ok]", msg), "\n")
	}
	tail := ""
	if o.HasNote {
		tail = " (note saved)"
	}
	msg := fmt.Sprintf("forfeited %s — back to 'next'%s", o.Quest.ID, tail)
	return strings.TrimRight(s.Line("↩️", "[forfeited]", msg), "\n")
}

func formatForfeitError(s lineListSink, err error) (string, bool) {
	if errors.Is(err, ErrAlreadyDone) {
		msg := fmt.Sprintf("quest_forfeit: %v — use quest post to rework, or reopen explicitly", err)
		return strings.TrimRight(s.Line("❌", "[err]", msg), "\n"), true
	}
	if errors.Is(err, ErrNotFound) {
		msg := fmt.Sprintf("quest_forfeit: %v", err)
		return strings.TrimRight(s.Line("❌", "[err]", msg), "\n"), true
	}
	return "", false
}
