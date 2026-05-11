package quest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
)

type ScrollInput struct {
	QuestID string `json:"quest_id" jsonschema:"QUEST-NNN"`
	Project string `json:"project,omitempty"`
}

type ScrollOutput struct {
	Result *ScrollResult `json:"result"`
}

var ScrollCommand = &command.Command[ScrollInput, ScrollOutput]{
	Name:    "quest_scroll",
	CLIPath: []string{"quest", "scroll"},
	Short:   "full history: status + journal + timeline",
	Long:    "Full quest history: status, journal, timeline. Use to pick up a quest someone else was working on.",
	Args: []command.ArgSpec{
		{Name: "quest_id", Kind: command.ArgPositional, Type: command.ArgString, Required: true, Help: "QUEST-NNN"},
		{Name: "project", Short: "p", Kind: command.ArgFlag, Type: command.ArgString, Help: "project override"},
	},
	Handler: func(ctx context.Context, d command.Deps, in ScrollInput) (ScrollOutput, error) {
		if strings.TrimSpace(in.QuestID) == "" {
			return ScrollOutput{}, errors.New("quest_id required")
		}
		db, err := d.OpenDB(ctx)
		if err != nil {
			return ScrollOutput{}, err
		}
		defer func() { _ = db.Close() }()
		pid, err := d.ResolveProj(ctx, in.Project)
		if err != nil {
			return ScrollOutput{}, err
		}
		res, err := Scroll(ctx, db, pid, in.QuestID)
		if err != nil {
			return ScrollOutput{}, err
		}
		return ScrollOutput{Result: res}, nil
	},
	CLIFormat: formatScrollCLI,
	MCPFormat: formatScrollMCP,
	CLIErrorFormat: func(s command.CLISink, err error) (string, bool) {
		if errors.Is(err, ErrNotFound) {
			return strings.TrimRight(s.Line("❌", "[err]", fmt.Sprintf("quest_scroll: %v", err)), "\n"), true
		}
		return "", false
	},
	MCPErrorFormat: func(s command.MCPSink, err error) (string, bool) {
		if errors.Is(err, ErrNotFound) {
			return strings.TrimRight(s.Line("❌", "[err]", fmt.Sprintf("quest_scroll: %v", err)), "\n"), true
		}
		return "", false
	},
}

// formatScrollCLI renders the rich multi-section human-facing output
// with NOTES + TIMELINE separators — preserved from the pre-registry
// CLI format so guild users keep their nice report.
func formatScrollCLI(s command.CLISink, o ScrollOutput) string {
	r := o.Result
	q := r.Quest
	var b strings.Builder
	b.WriteString(s.Separator())
	icon := scrollStatusIcon(q.Status)
	b.WriteString(s.Row("%s  %s [%s]", q.ID, icon, strings.ToUpper(string(q.Status))))
	b.WriteString(s.Separator())
	if q.Priority != "" {
		b.WriteString(s.Row("Priority : %s", q.Priority))
	}
	if q.Effort != "" {
		b.WriteString(s.Row("Effort   : %s", q.Effort))
	}
	if q.Epic != "" {
		b.WriteString(s.Row("Campaign : %s", q.Epic))
	}
	if q.Subject != "" {
		b.WriteString(s.Row("Subject  : %s", q.Subject))
	}
	if len(q.DependsOn) > 0 {
		b.WriteString(s.Row("Blocks on: %s", strings.Join(q.DependsOn, ", ")))
	}
	if q.Owner != "" && q.ClaimedAt != nil {
		b.WriteString(s.Row("Owner    : %s (since %s)",
			q.Owner, q.ClaimedAt.UTC().Format("2006-01-02T15:04")))
	}
	b.WriteString("\n")
	if len(r.Dependencies) > 0 {
		b.WriteString(s.Section("🔎", "[deps]", "dependency state"))
		for _, dep := range r.Dependencies {
			b.WriteString(s.Row("%s %s", dependencyStateMarker(dep), dependencyStateLine(dep)))
		}
		b.WriteString("\n")
	}
	if len(r.Notes) > 0 {
		b.WriteString(s.Section("📝", "[notes]", "notes"))
		for _, n := range r.Notes {
			ts := ""
			if !n.CreatedAt.IsZero() {
				ts = n.CreatedAt.UTC().Format("2006-01-02T15:04")
			}
			b.WriteString(s.Row("[%s] %s: %s", ts, n.AgentID, n.Note))
		}
		b.WriteString("\n")
	}
	if len(r.Events) > 0 {
		b.WriteString(s.Section("📅", "[timeline]", "timeline"))
		for _, e := range r.Events {
			ts := ""
			if !e.CreatedAt.IsZero() {
				ts = e.CreatedAt.UTC().Format("2006-01-02T15:04")
			}
			b.WriteString(s.Row("[%s] %-10s %s", ts, e.Event, e.AgentID))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatScrollMCP renders the compact MCP block — tools need token
// efficiency, not ASCII separators.
func formatScrollMCP(s command.MCPSink, o ScrollOutput) string {
	r := o.Result
	q := r.Quest
	var b strings.Builder
	b.WriteString(s.Line("📜", "", fmt.Sprintf("%s [%s · %s]  %s", q.ID, q.Priority, q.Status, q.Subject)))
	if q.Owner != "" {
		b.WriteString(s.Indented("owner", q.Owner))
	}
	if len(r.Dependencies) > 0 {
		b.WriteString("  dependencies:\n")
		for _, dep := range r.Dependencies {
			fmt.Fprintf(&b, "    - %s %s\n", dependencyStateMarker(dep), dependencyStateLine(dep))
		}
	}
	if len(q.Files) > 0 {
		b.WriteString(s.Indented("files", strings.Join(q.Files, ", ")))
	}
	for _, a := range q.Acceptance {
		b.WriteString("  ✓ ")
		b.WriteString(a)
		b.WriteString("\n")
	}
	if len(r.Notes) > 0 {
		fmt.Fprintf(&b, "  notes: %d\n", len(r.Notes))
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "    · %s\n", n.Note)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func dependencyStateMarker(dep DependencyState) string {
	if dep.Done {
		return "✓"
	}
	return "×"
}

func dependencyStateLine(dep DependencyState) string {
	if dep.Missing {
		return fmt.Sprintf("%s [missing]", dep.ID)
	}
	subject := strings.TrimSpace(dep.Subject)
	if subject == "" {
		return fmt.Sprintf("%s [%s]", dep.ID, dep.Status)
	}
	return fmt.Sprintf("%s [%s] %s", dep.ID, dep.Status, subject)
}

func scrollStatusIcon(status Status) string {
	switch status {
	case StatusInProgress:
		return "🔄"
	case StatusDone:
		return "✅"
	case StatusBlocked:
		return "🔒"
	default:
		return "·"
	}
}
