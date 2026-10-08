package quest

import (
	"context"
	"fmt"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
)

type ListInput struct {
	Campaign    string `json:"campaign,omitempty" jsonschema:"filter by campaign"`
	Epic        string `json:"epic,omitempty" jsonschema:"filter by campaign (alias for campaign)"`
	Status      string `json:"status,omitempty" jsonschema:"filter by status: next|in_progress|blocked|done"`
	ShowBlocked bool   `json:"show_blocked,omitempty" jsonschema:"include blocked quests in default view"`
	Files       bool   `json:"files,omitempty" jsonschema:"include Files column (CLI table)"`
	Deps        bool   `json:"deps,omitempty" jsonschema:"include Depends and Blocks columns (CLI table)"`
	Project     string `json:"project,omitempty"`
}

type ListOutput struct {
	Quests []*Quest  `json:"quests"`
	In     ListInput `json:"-"`
}

// RestoreInput re-attaches the CLI-only render switches (In.Files,
// In.Deps) that the `json:"-"` tag drops across the daemon's JSON-exec
// round trip. The cobra adapter calls this with the locally-parsed
// input so a routed `quest list --files` renders the same table a
// direct run would. See command.InputRestorer.
func (o *ListOutput) RestoreInput(in ListInput) { o.In = in }

var ListCommand = &command.Command[ListInput, ListOutput]{
	Name:    "quest_list",
	CLIPath: []string{"quest", "list"},
	Short:   "list open quests",
	Long:    "All open tasks. Use after guild_session_start to spot parallelism — unshared files + no deps → spawn agents.",
	Args: []command.ArgSpec{
		{Name: "campaign", Kind: command.ArgFlag, Type: command.ArgString, Help: "filter by campaign"},
		{Name: "epic", Kind: command.ArgFlag, Type: command.ArgString, Help: "filter by campaign (alias for --campaign)"},
		{Name: "status", Kind: command.ArgFlag, Type: command.ArgString, Help: "filter by status"},
		{Name: "show_blocked", Kind: command.ArgFlag, Type: command.ArgBool, Help: "include blocked quests", CLIOnly: true},
		{Name: "files", Kind: command.ArgFlag, Type: command.ArgBool, Help: "include Files column (CLI table)", CLIOnly: true},
		{Name: "deps", Kind: command.ArgFlag, Type: command.ArgBool, Help: "include Depends and Blocks columns (CLI table)", CLIOnly: true},
		{Name: "project", Short: "p", Kind: command.ArgFlag, Type: command.ArgString, Help: "project override"},
	},
	Handler: func(ctx context.Context, d command.Deps, in ListInput) (ListOutput, error) {
		db, err := d.OpenDB(ctx)
		if err != nil {
			return ListOutput{}, err
		}
		defer func() { _ = db.Close() }()
		pid, err := d.ResolveProj(ctx, in.Project)
		if err != nil {
			return ListOutput{}, err
		}
		campaign := in.Campaign
		if campaign == "" {
			campaign = in.Epic
		}
		qs, err := List(ctx, db, pid, ListFilters{
			Epic:        campaign,
			Status:      in.Status,
			ShowBlocked: in.ShowBlocked,
		})
		if err != nil {
			return ListOutput{}, err
		}
		return ListOutput{Quests: qs, In: in}, nil
	},
	CLIFormat: formatQuestListCLI,
	MCPFormat: formatQuestListMCP,
}

func formatQuestListCLI(s command.CLISink, o ListOutput) string {
	if len(o.Quests) == 0 {
		return strings.TrimRight(s.Line("✅", "[ok]", "no open tasks"), "\n")
	}
	return renderQuestTable(o.Quests, o.In.Files, o.In.Deps)
}

func formatQuestListMCP(s command.MCPSink, o ListOutput) string {
	if len(o.Quests) == 0 {
		return "✅ no quests"
	}
	var b strings.Builder
	b.WriteString(s.Line("⚔️", "", fmt.Sprintf("%d quest(s):", len(o.Quests))))
	for _, q := range o.Quests {
		owner := ""
		if q.Owner != "" {
			owner = " · " + q.Owner
		}
		fmt.Fprintf(&b, "  %s [%s · %s%s]  %s\n", q.ID, q.Priority, q.Status, owner, q.Subject)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderQuestTable produces a fixed-width CLI table. Extracted from the
// legacy internal/cli printQuestTable helper so the registry Format is
// self-contained.
func renderQuestTable(qs []*Quest, showFiles, showDeps bool) string {
	headers := []string{"ID", "Priority", "Subject", "Campaign", "Status", "Owner"}
	if showFiles {
		headers = append(headers, "Files")
	}
	if showDeps {
		headers = append(headers, "Depends", "Blocks")
	}
	type row struct{ cols []string }
	rows := make([]row, 0, len(qs))
	for _, q := range qs {
		subj := q.Subject
		if len(subj) > 60 {
			subj = subj[:60]
		}
		cols := []string{q.ID, string(q.Priority), subj, q.Epic, string(q.Status), q.Owner}
		if showFiles {
			files := strings.Join(q.Files, ",")
			if len(files) > 50 {
				files = files[:50]
			}
			if files == "" {
				files = "—"
			}
			cols = append(cols, files)
		}
		if showDeps {
			deps := strings.Join(q.DependsOn, ",")
			if deps == "" {
				deps = "—"
			}
			blocks := strings.Join(q.Blocks, ",")
			if blocks == "" {
				blocks = "—"
			}
			cols = append(cols, deps, blocks)
		}
		rows = append(rows, row{cols})
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, r := range rows {
		for i, c := range r.cols {
			if i < len(widths) && len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	var b strings.Builder
	writeRow := func(cols []string) {
		for i, c := range cols {
			fmt.Fprintf(&b, "%-*s  ", widths[i], c)
		}
		b.WriteString("\n")
	}
	writeRow(headers)
	sepCols := make([]string, len(headers))
	for i := range headers {
		sepCols[i] = strings.Repeat("-", widths[i])
	}
	writeRow(sepCols)
	for _, r := range rows {
		writeRow(r.cols)
	}
	return strings.TrimRight(b.String(), "\n")
}
