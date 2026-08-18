package lore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
)

type StudyInput struct {
	EntryID command.FlexInt64 `json:"entry_id" jsonschema:"numeric entry id (e.g. 542 for ENTRY-542)"`
	Project string            `json:"project,omitempty"`
}

type StudyCmdOutput struct {
	Result *StudyResult `json:"result"`
}

var StudyCommand = &command.Command[StudyInput, StudyCmdOutput]{
	Name:       "lore_study",
	CLIPath:    []string{"lore", "study"},
	CLIAliases: []string{"show"},
	Short:      "full detail of one entry",
	Long:       "Get full detail of one entry: summary, metadata, linked entries. Use after appraise for the deep read.",
	Args: []command.ArgSpec{
		{Name: "entry_id", Kind: command.ArgPositional, Type: command.ArgString, Required: true, Help: "entry id (ENTRY-N or bare N)"},
		{Name: "project", Short: "p", Kind: command.ArgFlag, Type: command.ArgString, Help: "project override"},
	},
	Handler: func(ctx context.Context, d command.Deps, in StudyInput) (StudyCmdOutput, error) {
		id := in.EntryID.Int64()
		if id <= 0 {
			return StudyCmdOutput{}, errors.New("entry_id must be positive")
		}
		if _, err := d.ResolveProj(ctx, in.Project); err != nil {
			return StudyCmdOutput{}, err
		}
		db, err := d.OpenDB(ctx)
		if err != nil {
			return StudyCmdOutput{}, err
		}
		defer func() { _ = db.Close() }()
		res, err := Study(ctx, db, id)
		if err != nil {
			return StudyCmdOutput{}, err
		}
		return StudyCmdOutput{Result: res}, nil
	},
	CLIFormat: func(s command.CLISink, o StudyCmdOutput) string { return formatStudyResult(s, o) },
	MCPFormat: func(s command.MCPSink, o StudyCmdOutput) string { return formatStudyResult(s, o) },
	CLIErrorFormat: func(s command.CLISink, err error) (string, bool) {
		if errors.Is(err, ErrEntryNotFound) {
			return strings.TrimRight(s.Line("❌", "[err]", fmt.Sprintf("lore_study: %v", err)), "\n"), true
		}
		return "", false
	},
	MCPErrorFormat: func(s command.MCPSink, err error) (string, bool) {
		if errors.Is(err, ErrEntryNotFound) {
			return strings.TrimRight(s.Line("❌", "[err]", fmt.Sprintf("lore_study: %v", err)), "\n"), true
		}
		return "", false
	},
}

func formatStudyResult(s lineSink, o StudyCmdOutput) string {
	r := o.Result
	e := r.Entry
	var b strings.Builder
	b.WriteString(s.Line("📜", "[study]",
		fmt.Sprintf("ENTRY-%d [%s · %s · %s]", e.ID, e.ProjectID, e.Kind, e.Status)))
	fmt.Fprintf(&b, "  title: %s\n", e.Title)
	fmt.Fprintf(&b, "  topic: %s\n", e.Topic)
	fmt.Fprintf(&b, "  summary: %s\n", e.Summary)
	if e.Body != "" {
		fmt.Fprintf(&b, "  body: %s\n", e.Body)
	}
	if len(e.Tags) > 0 {
		fmt.Fprintf(&b, "  tags: %s\n", strings.Join(e.Tags, ","))
	}
	if e.Source != "" {
		fmt.Fprintf(&b, "  source: %s\n", e.Source)
	}
	if len(r.Linked) > 0 {
		fmt.Fprintf(&b, "  linked: %d entries\n", len(r.Linked))
		for _, l := range r.Linked {
			arrow := "→"
			if l.Direction == EdgeIncoming {
				arrow = "←"
			}
			fmt.Fprintf(&b, "    %s ENTRY-%d [%s] %s\n", arrow, l.Entry.ID, l.Relation, l.Entry.Title)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
