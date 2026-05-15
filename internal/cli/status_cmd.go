// status_cmd.go wires `guild status` at the root level — a mid-session
// reorientation command that wraps quest bounties. Same data, same DB
// cost; the value is ergonomic (single command at root, not three
// levels deep under `guild quest bounties`).
package cli

import (
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:     "status",
	Short:   "dashboard: last briefing, oath, top bounty, and parallelism",
	GroupID: "core",
	Long: `Mid-session reorientation — shows the same snapshot the session-start
briefing prints, on demand.

Run this when you:
  - return to a repo after a break and need to catch up
  - want to see what the next agent would see on session_start
  - need to check oath drift or stale lore without re-running session_start

Same cost as quest bounties: one quest DB read + one lore DB read. No
new scans.`,
	Args: cobra.NoArgs,
	// Delegate to the existing runQuestBounties — identical surface,
	// identical cost. Aliasing avoids duplicate render code.
	RunE: runQuestBounties,
}

func init() {
	// --brief inherits the same flag as `quest bounties --brief`.
	statusCmd.Flags().BoolVar(&qbBrief, "brief", false, "show only the last briefing")
	rootCmd.AddCommand(statusCmd)
}
