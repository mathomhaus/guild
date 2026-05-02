package lore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
)

// UnlinkInput mirrors LinkInput exactly: same field names, same JSON
// tags, same optional relation. This keeps lore_unlink a predictable
// inverse of lore_link for callers.
type UnlinkInput struct {
	FromID   command.FlexInt64 `json:"from_id" jsonschema:"source entry id"`
	ToID     command.FlexInt64 `json:"to_id" jsonschema:"target entry id (the one that was being informed by from)"`
	Relation string            `json:"relation,omitempty" jsonschema:"informs|supersedes|contradicts (default informs)"`
	Project  string            `json:"project,omitempty"`
}

// UnlinkOutput is the structured result returned to callers.
type UnlinkOutput struct {
	FromID   int64    `json:"from_id"`
	ToID     int64    `json:"to_id"`
	Relation Relation `json:"relation"`
	// Removed is true when a row was actually deleted; false means the
	// edge did not exist (idempotent no-op).
	Removed bool   `json:"removed"`
	Note    string `json:"note"`
}

// UnlinkCommand is the lore_unlink Command[I,O] spec. It mirrors
// LinkCommand's registration pattern so the two verbs form a symmetric pair.
var UnlinkCommand = &command.Command[UnlinkInput, UnlinkOutput]{
	Name:    "lore_unlink",
	CLIPath: []string{"lore", "unlink"},
	Short:   "remove a provenance link between entries",
	Long:    "Remove an informs (or supersedes/contradicts) edge from the provenance graph. Idempotent: removing a non-existent edge returns success with a 'no matching edge' note.",
	Args: []command.ArgSpec{
		{Name: "from_id", Kind: command.ArgPositional, Type: command.ArgString, Required: true, Help: "source entry id (LORE-N or bare N)"},
		{Name: "to_id", CLIFlagName: "informs", Kind: command.ArgFlag, Type: command.ArgString, Required: true, Help: "target entry id (LORE-N or bare N), the one that was being informed"},
		{Name: "relation", Kind: command.ArgFlag, Type: command.ArgString, Help: "link relation to remove: informs (default) | supersedes | contradicts"},
		{Name: "project", Short: "p", Kind: command.ArgFlag, Type: command.ArgString, Help: "project override"},
	},
	Handler: func(ctx context.Context, d command.Deps, in UnlinkInput) (UnlinkOutput, error) {
		fromID, toID := in.FromID.Int64(), in.ToID.Int64()
		if fromID <= 0 || toID <= 0 {
			return UnlinkOutput{}, errors.New("from_id and to_id required")
		}
		rel := Relation(strings.TrimSpace(in.Relation))
		if rel == "" {
			rel = RelationInforms
		}
		db, err := d.OpenDB(ctx)
		if err != nil {
			return UnlinkOutput{}, err
		}
		defer func() { _ = db.Close() }()
		if _, err := d.ResolveProj(ctx, in.Project); err != nil {
			return UnlinkOutput{}, err
		}
		result, err := UnlinkEntries(ctx, db, fromID, toID, rel)
		if err != nil {
			return UnlinkOutput{}, err
		}
		return UnlinkOutput{
			FromID:   fromID,
			ToID:     toID,
			Relation: rel,
			Removed:  result.Removed,
			Note:     result.Note,
		}, nil
	},
	CLIFormat: func(s command.CLISink, o UnlinkOutput) string { return formatUnlinked(s, o) },
	MCPFormat: func(s command.MCPSink, o UnlinkOutput) string { return formatUnlinked(s, o) },
}

func formatUnlinked(s lineSink, o UnlinkOutput) string {
	var msg string
	if o.Removed {
		msg = fmt.Sprintf("unlinked %s %s %s", formatEntryID(o.FromID), o.Relation, formatEntryID(o.ToID))
	} else {
		msg = fmt.Sprintf("no edge %s %s %s: %s", formatEntryID(o.FromID), o.Relation, formatEntryID(o.ToID), o.Note)
	}
	return strings.TrimRight(s.Line("🔗", "[unlinked]", msg), "\n")
}
