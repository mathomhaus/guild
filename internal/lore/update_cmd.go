package lore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
)

type UpdateInput struct {
	EntryID command.FlexInt64 `json:"entry_id" jsonschema:"entry to update"`
	Title   string            `json:"title,omitempty" jsonschema:"new title"`
	Summary string            `json:"summary,omitempty" jsonschema:"new summary"`
	Topic   string            `json:"topic,omitempty" jsonschema:"new topic slug"`
	Kind    string            `json:"kind,omitempty" jsonschema:"reclassify: idea|research|decision|observation|principle"`
	Tags    string            `json:"tags,omitempty" jsonschema:"replace tags (comma-separated)"`
	Status  string            `json:"status,omitempty" jsonschema:"new status (current|stale|...)"`
	Project string            `json:"project,omitempty"`
}

type UpdateCmdOutput struct {
	Entry *Entry `json:"entry"`
}

var UpdateCommand = &command.Command[UpdateInput, UpdateCmdOutput]{
	Name:    "lore_update",
	CLIPath: []string{"lore", "update"},
	Short:   "edit fields or reclassify an entry",
	Long:    "Edit an entry's title, status, tags, kind, topic, or summary.",
	Args: []command.ArgSpec{
		{Name: "entry_id", Kind: command.ArgPositional, Type: command.ArgString, Required: true, Help: "entry id (LORE-N or bare N)"},
		{Name: "title", Kind: command.ArgFlag, Type: command.ArgString, Help: "new title"},
		{Name: "summary", Kind: command.ArgFlag, Type: command.ArgString, Help: "new summary"},
		{Name: "topic", Kind: command.ArgFlag, Type: command.ArgString, Help: "new topic slug"},
		{Name: "kind", Kind: command.ArgFlag, Type: command.ArgString, Help: "reclassify kind"},
		{Name: "tags", Kind: command.ArgFlag, Type: command.ArgString, Help: "replace tags (comma-separated)"},
		{Name: "status", Kind: command.ArgFlag, Type: command.ArgString, Help: "new status"},
		{Name: "project", Short: "p", Kind: command.ArgFlag, Type: command.ArgString, Help: "project override"},
	},
	Handler: func(ctx context.Context, d command.Deps, in UpdateInput) (UpdateCmdOutput, error) {
		id := in.EntryID.Int64()
		if id <= 0 {
			return UpdateCmdOutput{}, errors.New("entry_id required")
		}
		db, err := d.OpenDB(ctx)
		if err != nil {
			return UpdateCmdOutput{}, err
		}
		defer func() { _ = db.Close() }()
		pid, err := d.ResolveProj(ctx, in.Project)
		if err != nil {
			return UpdateCmdOutput{}, err
		}
		p := UpdateParams{ProjectID: pid, Embed: embedFromDeps(ctx, d)}
		if in.Title != "" {
			v := in.Title
			p.Title = &v
		}
		if in.Summary != "" {
			v := in.Summary
			p.Summary = &v
		}
		if in.Topic != "" {
			v := in.Topic
			p.Topic = &v
		}
		if in.Kind != "" {
			v := Kind(in.Kind)
			p.Kind = &v
		}
		if in.Tags != "" {
			v := in.Tags
			p.Tags = &v
		}
		if in.Status != "" {
			v := Status(in.Status)
			p.Status = &v
		}
		e, err := Update(ctx, db, id, &p)
		if err != nil {
			return UpdateCmdOutput{}, err
		}
		return UpdateCmdOutput{Entry: e}, nil
	},
	CLIFormat: func(s command.CLISink, o UpdateCmdOutput) string {
		return strings.TrimRight(s.Line("✅", "[ok]", fmt.Sprintf("updated %s", formatEntryID(o.Entry.ID))), "\n")
	},
	MCPFormat: func(s command.MCPSink, o UpdateCmdOutput) string {
		return strings.TrimRight(s.Line("✅", "[ok]", fmt.Sprintf("updated %s", formatEntryID(o.Entry.ID))), "\n")
	},
	CLIErrorFormat: func(s command.CLISink, err error) (string, bool) {
		if errors.Is(err, ErrNoChanges) {
			return strings.TrimRight(s.Line("❌", "[err]", "lore_update: provide at least one field to update"), "\n"), true
		}
		return "", false
	},
	MCPErrorFormat: func(s command.MCPSink, err error) (string, bool) {
		if errors.Is(err, ErrNoChanges) {
			return strings.TrimRight(s.Line("❌", "[err]", "lore_update: provide at least one field to update"), "\n"), true
		}
		return "", false
	},
}
