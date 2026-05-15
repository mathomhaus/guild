package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/config"
	"github.com/mathomhaus/guild/internal/telemetry"
)

// optInHint is surfaced when telemetry is disabled in config so an agent
// or user running `guild telemetry tokens` for the first time understands
// why the numbers are empty.
const optInHint = "telemetry is disabled — enable with [telemetry]\n  usage_log = true in ~/.guild/config.toml to start recording resp_bytes"

var telemetryCmd = &cobra.Command{
	Use:     "telemetry",
	Short:   "telemetry analytics (usage log, token estimates)",
	GroupID: "inspection",
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

var (
	tokensSessionFlag string
	tokensJSONFlag    bool
)

var telemetryTokensCmd = &cobra.Command{
	Use:   "tokens",
	Short: "estimate token cost from usage.log response bytes",
	Long: `Reads ~/.guild/usage.log and estimates MCP response token cost.

Per-tool breakdown: call count, total resp_bytes, estimated tokens (4 chars/token), mean tokens/call.
Session aggregate: total tokens grouped by session (project + minute bucket).

Estimated at 4 chars/token — actual varies ~20%.`,
	Args: cobra.NoArgs,
	RunE: runTelemetryTokens,
}

func init() {
	telemetryTokensCmd.Flags().StringVar(&tokensSessionFlag, "session", "", "filter to one session key (e.g. guild@2026-04-19T15:04); default: most recent session")
	telemetryTokensCmd.Flags().BoolVar(&tokensJSONFlag, "json", false, "output as JSON")
	telemetryCmd.AddCommand(telemetryTokensCmd)
	rootCmd.AddCommand(telemetryCmd)
}

func runTelemetryTokens(cmd *cobra.Command, _ []string) error {
	logPath, err := telemetry.UsageLogPath()
	if err != nil {
		return fmt.Errorf("telemetry tokens: %w", err)
	}

	optedOut := false
	if cfg, cerr := config.Load(nil); cerr == nil && cfg != nil {
		optedOut = cfg.NoUsageLog
	}

	rows, err := telemetry.ParseUsageLogFile(logPath)
	if err != nil {
		return fmt.Errorf("telemetry tokens: parse usage.log: %w", err)
	}
	if len(rows) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "usage.log is empty — no telemetry data yet")
		if optedOut {
			fmt.Fprintln(cmd.OutOrStdout(), optInHint)
		}
		return nil
	}

	sessionFilter := tokensSessionFlag
	if sessionFilter == "" {
		sessionFilter = telemetry.MostRecentSession(rows)
	}

	report := telemetry.Analyze(rows, sessionFilter)

	if tokensJSONFlag {
		return printTokensJSON(cmd, report, sessionFilter, optedOut)
	}
	printTokensText(cmd, report, sessionFilter, optedOut)
	return nil
}

func printTokensText(cmd *cobra.Command, report *telemetry.TokenReport, sessionFilter string, optedOut bool) {
	w := cmd.OutOrStdout()

	// --- per-tool breakdown ---
	fmt.Fprintln(w, "per-tool breakdown (session: "+sessionFilter+")")
	fmt.Fprintln(w, strings.Repeat("-", 72))
	fmt.Fprintf(w, "%-32s %6s %10s %10s %10s\n", "tool", "calls", "bytes", "est_tokens", "mean_tok")
	fmt.Fprintln(w, strings.Repeat("-", 72))

	// Sorted by total bytes descending.
	tools := make([]*telemetry.ToolStats, 0, len(report.ByTool))
	for _, s := range report.ByTool {
		tools = append(tools, s)
	}
	sort.Slice(tools, func(i, j int) bool {
		if tools[i].TotalBytes != tools[j].TotalBytes {
			return tools[i].TotalBytes > tools[j].TotalBytes
		}
		return tools[i].Tool < tools[j].Tool
	})
	for _, s := range tools {
		fmt.Fprintf(w, "%-32s %6d %10d %10d %10d\n",
			s.Tool, s.Calls, s.TotalBytes, s.EstTokens(), s.MeanTokens())
	}
	fmt.Fprintln(w, strings.Repeat("-", 72))

	// --- session aggregate ---
	fmt.Fprintln(w)
	fmt.Fprintln(w, "session aggregate")
	fmt.Fprintln(w, strings.Repeat("-", 72))
	fmt.Fprintf(w, "%-48s %6s %12s\n", "session", "calls", "est_tokens")
	fmt.Fprintln(w, strings.Repeat("-", 72))
	for _, s := range report.Sessions {
		fmt.Fprintf(w, "%-48s %6d %12d\n", s.SessionKey, s.Calls, s.TotalTokens)
	}
	fmt.Fprintln(w, strings.Repeat("-", 72))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "note: estimated at 4 chars/token — actual varies ~20%")
	if optedOut {
		fmt.Fprintln(w, optInHint)
	}
}

// tokensJSONOut is the JSON-serializable view of the report.
type tokensJSONOut struct {
	Session     string        `json:"session"`
	ByTool      []toolJSONRow `json:"by_tool"`
	Sessions    []sessJSONRow `json:"sessions"`
	GeneratedAt string        `json:"generated_at"`
	Note        string        `json:"note"`
	OptedOut    bool          `json:"opted_out,omitempty"`
	OptInHint   string        `json:"opt_in_hint,omitempty"`
}

type toolJSONRow struct {
	Tool       string `json:"tool"`
	Calls      int    `json:"calls"`
	TotalBytes uint   `json:"total_bytes"`
	EstTokens  uint   `json:"est_tokens"`
	MeanTokens uint   `json:"mean_tokens_per_call"`
}

type sessJSONRow struct {
	SessionKey  string `json:"session_key"`
	Project     string `json:"project"`
	Start       string `json:"start"`
	Calls       int    `json:"calls"`
	TotalBytes  uint   `json:"total_bytes"`
	TotalTokens uint   `json:"total_tokens"`
}

func printTokensJSON(cmd *cobra.Command, report *telemetry.TokenReport, sessionFilter string, optedOut bool) error {
	tools := make([]*telemetry.ToolStats, 0, len(report.ByTool))
	for _, s := range report.ByTool {
		tools = append(tools, s)
	}
	sort.Slice(tools, func(i, j int) bool {
		return tools[i].TotalBytes > tools[j].TotalBytes
	})

	rows := make([]toolJSONRow, len(tools))
	for i, s := range tools {
		rows[i] = toolJSONRow{
			Tool:       s.Tool,
			Calls:      s.Calls,
			TotalBytes: s.TotalBytes,
			EstTokens:  s.EstTokens(),
			MeanTokens: s.MeanTokens(),
		}
	}

	sessRows := make([]sessJSONRow, len(report.Sessions))
	for i, s := range report.Sessions {
		sessRows[i] = sessJSONRow{
			SessionKey:  s.SessionKey,
			Project:     s.Project,
			Start:       s.Start.UTC().Format(time.RFC3339),
			Calls:       s.Calls,
			TotalBytes:  s.TotalBytes,
			TotalTokens: s.TotalTokens,
		}
	}

	out := tokensJSONOut{
		Session:     sessionFilter,
		ByTool:      rows,
		Sessions:    sessRows,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Note:        "estimated at 4 chars/token — actual varies ~20%",
	}
	if optedOut {
		out.OptedOut = true
		out.OptInHint = optInHint
	}

	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
