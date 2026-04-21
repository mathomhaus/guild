// Package quest implements the quest CLI lifecycle: init, post, accept,
// clear (with cascade-unblock), forfeit, update, epic.
//
// Storage strategy (event-sourcing): task_status holds the authoritative
// STATE of each quest (status, claimed_by, claimed_at). Everything else —
// subject, priority, epic, effort, files, acceptance criteria, depends_on,
// blocks, rework — is carried in task_notes rows tagged with the `[spec]` /
// `[spec-replace]` prefix pattern. The canonical Quest shape is derived by
// replaying every `[spec]*` note in ascending `id` order (file creation order)
// and then overlaying the task_status row.
//
// Context propagation and parameterized SQL only are enforced throughout.
package quest

import (
	"errors"
	"time"
)

// Status enumerates the legal task_status.status values: {next, in_progress,
// blocked, done}. Typed so callers don't pass random strings through
// SetStatus-like code paths.
type Status string

// The four legal statuses, stored as plain lowercase strings in SQLite.
const (
	StatusNext       Status = "next"
	StatusInProgress Status = "in_progress"
	StatusBlocked    Status = "blocked"
	StatusDone       Status = "done"
)

// Priority is the free-form P0/P1/P2/... tag. We don't enumerate here
// because users can introduce custom priority labels; validating them
// would break back-compat with existing data. The sort precedence is
// encoded in the comparator below.
type Priority string

// Quest is the canonical in-memory shape of a task. It's what Post
// returns after a write and what List/Scroll will return on read.
// JSON tags match the `quest list --json` wire format.
type Quest struct {
	ID         string     `json:"id"`
	Subject    string     `json:"subject"`
	Priority   Priority   `json:"priority,omitempty"`
	Epic       string     `json:"epic,omitempty"`
	Effort     string     `json:"effort,omitempty"`
	Status     Status     `json:"status"`
	Owner      string     `json:"owner,omitempty"`
	ClaimedAt  *time.Time `json:"claimed_at,omitempty"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
	Files      []string   `json:"files,omitempty"`
	Acceptance []string   `json:"acceptance,omitempty"`
	DependsOn  []string   `json:"depends_on,omitempty"`
	Blocks     []string   `json:"blocks,omitempty"`
	ReworkOf   string     `json:"rework_of,omitempty"`
}

// Sentinel errors the package surfaces. Callers use errors.Is to branch
// on these — especially the CLI which maps each to a specific emoji line
// + exit code, and the MCP server which maps each to `isError + hint`.
var (
	// ErrNotFound is returned when a requested quest_id has no row in
	// task_status. Distinct from DB errors so the CLI can print a
	// "quest not found" error line.
	ErrNotFound = errors.New("quest not found")

	// ErrAlreadyClaimed is returned by Accept when the quest already has
	// a claimed_by != NULL OR its status isn't `next`/`blocked`. The
	// CLI prints the owner in the ❌ line. Carries the current claimant
	// via errors.As on a *AlreadyClaimedError for structured recovery.
	ErrAlreadyClaimed = errors.New("quest already claimed")

	// ErrNoChange is returned by Update when the caller provided no
	// field updates.
	ErrNoChange = errors.New("no update fields provided")

	// ErrConflictingUpdate is returned when an Update call sets both the
	// append and replace form of the same list field (e.g. Files AND
	// ReplaceFiles).
	ErrConflictingUpdate = errors.New("conflicting update: cannot set both append and replace for the same field")

	// ErrAlreadyDone is returned by Forfeit when the target quest is
	// status='done'. Forfeit refuses to silently reopen a completed
	// quest — the caller should explicitly rework or reopen.
	ErrAlreadyDone = errors.New("quest already done")
)

// AlreadyClaimedError carries who currently holds the quest so the CLI
// can print a helpful "❌ already accepted: QUEST-N is held by ..." line
// and so MCP callers can route recovery in one call.
type AlreadyClaimedError struct {
	QuestID string
	Owner   string
	Status  Status
}

// Error satisfies error; unwrapping yields ErrAlreadyClaimed so callers
// can branch on errors.Is.
func (e *AlreadyClaimedError) Error() string {
	if e.Owner == "" {
		return "quest " + e.QuestID + " not acceptable (status=" + string(e.Status) + ")"
	}
	return "quest " + e.QuestID + " already held by " + e.Owner
}

// Unwrap makes errors.Is(err, ErrAlreadyClaimed) true.
func (e *AlreadyClaimedError) Unwrap() error { return ErrAlreadyClaimed }

// PostParams carries the inputs to Post: --priority --epic --files
// --acceptance/-a --depends-on --rework.
type PostParams struct {
	Subject    string
	Priority   Priority
	Epic       string
	Effort     string
	Files      []string
	Acceptance []string
	DependsOn  []string
	// ReworkOf is the prior quest id this quest is re-doing. Recorded as
	// a [rework] of: note so pulse queries can surface churn.
	ReworkOf string
	// Agent is the writer of the [spec] note(s). Empty → "agent".
	Agent string
}

// UpdateParams carries inputs to Update. Scalars overwrite, list fields
// append by default, and the Replace* variants cause full replacement.
// Setting both the append form and the Replace* form for the same list
// returns ErrConflictingUpdate.
type UpdateParams struct {
	Subject  string
	Priority Priority
	Epic     string
	Effort   string

	// Append lists.
	Files      []string
	Acceptance []string
	DependsOn  []string
	Blocks     []string

	// Replace lists. If both a replace AND an append are set for the
	// same field, Update returns ErrConflictingUpdate.
	ReplaceFiles      []string
	ReplaceAcceptance []string
	ReplaceDependsOn  []string
	ReplaceBlocks     []string
	// Flags mark a replace that resets the field to an empty list even
	// when Replace*=nil (the only way to say "clear this list entirely").
	ClearFiles      bool
	ClearAcceptance bool
	ClearDependsOn  bool
	ClearBlocks     bool

	Agent string
}

// Empty reports whether the params carry any change. Used by Update to
// short-circuit with ErrNoChange. Pointer
// receiver to avoid the 280-byte struct copy on every call; Empty is
// value-semantic so zero-value p still returns true.
func (p *UpdateParams) Empty() bool {
	if p == nil {
		return true
	}
	if p.Subject != "" || p.Priority != "" || p.Epic != "" || p.Effort != "" {
		return false
	}
	if len(p.Files) > 0 || len(p.Acceptance) > 0 || len(p.DependsOn) > 0 || len(p.Blocks) > 0 {
		return false
	}
	if len(p.ReplaceFiles) > 0 || len(p.ReplaceAcceptance) > 0 ||
		len(p.ReplaceDependsOn) > 0 || len(p.ReplaceBlocks) > 0 {
		return false
	}
	if p.ClearFiles || p.ClearAcceptance || p.ClearDependsOn || p.ClearBlocks {
		return false
	}
	return true
}

// ClearResult is returned by Clear. Cleared is the freshly-done quest;
// Unblocked lists every quest whose `blocked → next` flip was caused
// by this Clear call (cascade-unblock invariant).
type ClearResult struct {
	Cleared   *Quest
	Unblocked []*Quest
}

// ForfeitResult is returned by Forfeit. Quest is the target quest's
// current state. AlreadyNext is true when Forfeit was a no-op because
// the quest was not in_progress — no DB writes happened, no release
// event was emitted, and the caller should render a neutral message
// rather than the ↩️ success line.
type ForfeitResult struct {
	Quest       *Quest
	AlreadyNext bool
}

// PriorityOrder returns a sort rank for p. Lower is higher priority:
// "p0" = 0, "p1" = 1, "p2" = 2, "p3" = 3, everything else = 9.
// Case-insensitive.
func PriorityOrder(p Priority) int {
	switch {
	case eqFold(string(p), "p0"):
		return 0
	case eqFold(string(p), "p1"):
		return 1
	case eqFold(string(p), "p2"):
		return 2
	case eqFold(string(p), "p3"):
		return 3
	default:
		return 9
	}
}

// eqFold is strings.EqualFold inlined to avoid depending on strings in
// this otherwise pure-types file.
func eqFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
