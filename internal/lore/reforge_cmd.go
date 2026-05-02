package lore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mathomhaus/guild/internal/command"
)

type ReforgeInput struct {
	OldID   command.FlexInt64 `json:"old_id" jsonschema:"the entry being superseded"`
	NewID   command.FlexInt64 `json:"new_id" jsonschema:"the newer entry that replaces it"`
	Project string            `json:"project,omitempty"`
}

type ReforgeOutput struct {
	OldID int64 `json:"old_id"`
	NewID int64 `json:"new_id"`
}

var ReforgeCommand = &command.Command[ReforgeInput, ReforgeOutput]{
	Name:       "lore_reforge",
	CLIPath:    []string{"lore", "reforge"},
	CLIAliases: []string{"supersede"},
	Short:      "mark entry superseded by a newer one (atomic)",
	Long:       "Mark an older entry superseded by a newer one. Preserves history — do NOT delete.",
	Args: []command.ArgSpec{
		{Name: "old_id", Kind: command.ArgPositional, Type: command.ArgString, Required: true, Help: "the entry being superseded (LORE-N or bare N)"},
		{Name: "new_id", CLIFlagName: "with", Kind: command.ArgFlag, Type: command.ArgString, Required: true, Help: "the newer entry that replaces it (LORE-N or bare N)"},
		{Name: "project", Short: "p", Kind: command.ArgFlag, Type: command.ArgString, Help: "project override"},
	},
	Handler: func(ctx context.Context, d command.Deps, in ReforgeInput) (ReforgeOutput, error) {
		oldID, newID := in.OldID.Int64(), in.NewID.Int64()
		if oldID <= 0 || newID <= 0 {
			return ReforgeOutput{}, errors.New("old_id and new_id required")
		}
		db, err := d.OpenDB(ctx)
		if err != nil {
			return ReforgeOutput{}, err
		}
		defer func() { _ = db.Close() }()
		if _, err := d.ResolveProj(ctx, in.Project); err != nil {
			return ReforgeOutput{}, err
		}
		// QUEST-213 wires d.Embed (carried as `any` in command.Deps to
		// avoid the command↔lore import cycle). When the adapter layer
		// did not construct an EmbedDeps, embedFromDeps returns nil and
		// Reforge behaves exactly like the pre-Phase-1 path.
		if err := Reforge(ctx, db, oldID, newID, time.Time{}, embedFromDeps(ctx, d)); err != nil {
			return ReforgeOutput{}, err
		}
		return ReforgeOutput{OldID: oldID, NewID: newID}, nil
	},
	CLIFormat: func(s command.CLISink, o ReforgeOutput) string { return formatReforged(s, o) },
	MCPFormat: func(s command.MCPSink, o ReforgeOutput) string { return formatReforged(s, o) },
}

func formatReforged(s lineSink, o ReforgeOutput) string {
	msg := fmt.Sprintf("reforged %s -> %s", formatEntryID(o.OldID), formatEntryID(o.NewID))
	return strings.TrimRight(s.Line("🔨", "[reforged]", msg), "\n")
}
