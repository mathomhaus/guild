package quest

import (
	"context"
	"fmt"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
)

type ActiveInput struct {
	// Project is optional. Active() is cross-project by default; an
	// explicit project limits the list to that project.
	Project string `json:"project,omitempty"`
}

type ActiveOutput struct {
	Quests []*Quest `json:"quests"`
}

var ActiveCommand = &command.Command[ActiveInput, ActiveOutput]{
	Name:    "quest_active",
	CLIPath: []string{"quest", "active"},
	Short:   "in-progress tasks across all projects",
	Long:    "List every in-progress quest across all projects. Diagnose contention and orphaned work.",
	Args: []command.ArgSpec{
		{Name: "project", Short: "p", Kind: command.ArgFlag, Type: command.ArgString, Help: "project override"},
	},
	Handler: func(ctx context.Context, d command.Deps, in ActiveInput) (ActiveOutput, error) {
		// The MCP path requires an active session, so ResolveProj runs
		// even without an explicit project; only an explicit one filters.
		pid, err := d.ResolveProj(ctx, in.Project)
		if err != nil {
			return ActiveOutput{}, err
		}
		if in.Project == "" {
			pid = ""
		}
		db, err := d.OpenDB(ctx)
		if err != nil {
			return ActiveOutput{}, err
		}
		defer func() { _ = db.Close() }()
		qs, err := Active(ctx, db, pid)
		if err != nil {
			return ActiveOutput{}, err
		}
		return ActiveOutput{Quests: qs}, nil
	},
	CLIFormat: func(s command.CLISink, o ActiveOutput) string { return formatActive(s, o) },
	MCPFormat: func(s command.MCPSink, o ActiveOutput) string { return formatActive(s, o) },
}

func formatActive(s lineListSink, o ActiveOutput) string {
	if len(o.Quests) == 0 {
		return strings.TrimRight(s.Line("✅", "[ok]", "no active quests"), "\n")
	}
	var b strings.Builder
	b.WriteString(s.Line("⏳", "[active]", fmt.Sprintf("%d active quest(s):", len(o.Quests))))
	for _, q := range o.Quests {
		fmt.Fprintf(&b, "  %s [%s]  %s\n", q.ID, q.Priority, q.Subject)
		if q.Owner != "" {
			fmt.Fprintf(&b, "    owner: %s\n", q.Owner)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
