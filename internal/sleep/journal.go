package sleep

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// The journal is the audit trail that keeps the HYBRID mutation policy
// honest: every op a sleep pass applies (or declines to apply) lands in
// sleep_ops with its policy verdict, and auto-applied ops carry a JSON
// inverse payload describing how to manually reverse them. The
// canonical home for both tables is lore.db; the migration also lands
// in quest.db (the migrations directory is shared) where the copy
// stays inert.
//
// Write side: BeginPass, RecordOp, EndPass.
// Read side: UnnarratedPasses, PassOps, MarkNarrated, LastPassEndedAt,
// consumed by session-start narration and the autopass scheduler.

// Trigger names what started a sleep pass.
type Trigger string

const (
	// TriggerDaemonIdle is a pass started by the daemon's idle
	// scheduler (ADR-005 Phase 2).
	TriggerDaemonIdle Trigger = "daemon-idle"

	// TriggerAutopass is a pass started by the degraded in-process
	// autopass when no daemon is running.
	TriggerAutopass Trigger = "autopass"
)

// valid reports whether t is one of the defined trigger values. The
// sleep_passes CHECK constraint enforces the same set; validating here
// gives a clearer error than a raw constraint failure.
func (t Trigger) valid() bool {
	return t == TriggerDaemonIdle || t == TriggerAutopass
}

// Pass is one row of sleep_passes.
type Pass struct {
	ID         int64
	StartedAt  time.Time
	EndedAt    time.Time // zero while the pass is still running
	Trigger    Trigger
	Budget     time.Duration
	NarratedAt time.Time // zero until a session narrates the pass
}

// Op is one row of sleep_ops: a mutation (or attempted mutation)
// recorded by a step during a pass.
type Op struct {
	ID     int64
	PassID int64
	// Step is the Step.Name() that produced the op.
	Step string
	// Policy is the gate verdict the step acted under: PolicyAuto ops
	// were applied directly; PolicyApproval ops were turned into
	// approval quest posts instead of mutations.
	Policy Policy
	// Kind is the op taxonomy value (see policy.go). The column is
	// free text so runner bookkeeping rows can use unexported kinds.
	Kind OpKind
	// Target holds display ids, e.g. "LORE-12<-LORE-40" or "QUEST-401".
	Target string
	// Detail is a JSON payload describing what happened. Optional.
	Detail string
	// Inverse is a JSON payload recording what is needed to manually
	// reverse an auto-applied op (prior status value, the supersedes
	// edge inserted, ...). Optional; it keeps the "reversible" half of
	// the HYBRID ruling honest, so auto mutation ops should set it.
	Inverse string
	// Applied reports whether the mutation actually landed. False for
	// approval posts (the proposed change did not run) and for runner
	// bookkeeping rows.
	Applied bool
}

// BeginPass inserts a sleep_passes row and returns its id. budget is
// the wall budget the pass will run under, persisted as budget_ms.
// A zero now defaults to time.Now().UTC().
func BeginPass(ctx context.Context, db *sql.DB, trigger Trigger, budget time.Duration, now time.Time) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("sleep: begin pass: nil db")
	}
	if !trigger.valid() {
		return 0, fmt.Errorf("sleep: begin pass: invalid trigger %q", trigger)
	}
	if budget <= 0 {
		return 0, fmt.Errorf("sleep: begin pass: non-positive budget %v", budget)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	res, err := db.ExecContext(ctx,
		`INSERT INTO sleep_passes (started_at, "trigger", budget_ms)
		 VALUES (?, ?, ?)`,
		now.Format(time.RFC3339), string(trigger), budget.Milliseconds(),
	)
	if err != nil {
		return 0, fmt.Errorf("sleep: begin pass: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("sleep: begin pass: last insert id: %w", err)
	}
	return id, nil
}

// RecordOp inserts a sleep_ops row for op and returns its id. Empty
// Detail / Inverse persist as NULL.
func RecordOp(ctx context.Context, db *sql.DB, op Op) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("sleep: record op: nil db")
	}
	if op.PassID <= 0 {
		return 0, fmt.Errorf("sleep: record op: missing pass id")
	}
	if strings.TrimSpace(op.Step) == "" {
		return 0, fmt.Errorf("sleep: record op: missing step")
	}
	if op.Policy != PolicyAuto && op.Policy != PolicyApproval {
		return 0, fmt.Errorf("sleep: record op: invalid policy %q", op.Policy)
	}
	if strings.TrimSpace(string(op.Kind)) == "" {
		return 0, fmt.Errorf("sleep: record op: missing op kind")
	}
	if strings.TrimSpace(op.Target) == "" {
		return 0, fmt.Errorf("sleep: record op: missing target")
	}

	applied := 0
	if op.Applied {
		applied = 1
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO sleep_ops (pass_id, step, policy, op, target, detail, inverse, applied)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		op.PassID, op.Step, string(op.Policy), string(op.Kind), op.Target,
		nullable(op.Detail), nullable(op.Inverse), applied,
	)
	if err != nil {
		return 0, fmt.Errorf("sleep: record op: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("sleep: record op: last insert id: %w", err)
	}
	return id, nil
}

// EndPass stamps ended_at on passID. Ending an already-ended pass is a
// no-op (the original ended_at wins), so the runner can call it from a
// defer without double-stamping. A zero now defaults to time.Now().UTC().
func EndPass(ctx context.Context, db *sql.DB, passID int64, now time.Time) error {
	if db == nil {
		return fmt.Errorf("sleep: end pass: nil db")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	res, err := db.ExecContext(ctx,
		`UPDATE sleep_passes SET ended_at = ? WHERE id = ? AND ended_at IS NULL`,
		now.Format(time.RFC3339), passID,
	)
	if err != nil {
		return fmt.Errorf("sleep: end pass: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sleep: end pass: rows affected: %w", err)
	}
	if n == 0 {
		// Either the pass does not exist (error) or it already ended
		// (no-op). One existence probe disambiguates.
		var exists int
		err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sleep_passes WHERE id = ?`, passID,
		).Scan(&exists)
		if err != nil {
			return fmt.Errorf("sleep: end pass: existence probe: %w", err)
		}
		if exists == 0 {
			return fmt.Errorf("sleep: end pass: pass %d not found", passID)
		}
	}
	return nil
}

// UnnarratedPasses returns every ended pass that no session has
// narrated yet, oldest first. In-flight passes (ended_at IS NULL) are
// excluded: narration describes completed work.
func UnnarratedPasses(ctx context.Context, db *sql.DB) ([]Pass, error) {
	if db == nil {
		return nil, fmt.Errorf("sleep: unnarrated passes: nil db")
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, started_at, ended_at, "trigger", budget_ms, narrated_at
		   FROM sleep_passes
		  WHERE ended_at IS NOT NULL AND narrated_at IS NULL
		  ORDER BY id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("sleep: unnarrated passes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Pass
	for rows.Next() {
		p, err := scanPass(rows)
		if err != nil {
			return nil, fmt.Errorf("sleep: unnarrated passes: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sleep: unnarrated passes: %w", err)
	}
	return out, nil
}

// PassOps returns the ops journaled under passID in insertion order.
// Narration uses this to describe what a pass did.
func PassOps(ctx context.Context, db *sql.DB, passID int64) ([]Op, error) {
	if db == nil {
		return nil, fmt.Errorf("sleep: pass ops: nil db")
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, pass_id, step, policy, op, target, detail, inverse, applied
		   FROM sleep_ops
		  WHERE pass_id = ?
		  ORDER BY id ASC`,
		passID,
	)
	if err != nil {
		return nil, fmt.Errorf("sleep: pass ops: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Op
	for rows.Next() {
		var (
			op              Op
			policy, kind    string
			detail, inverse sql.NullString
			applied         int
		)
		if err := rows.Scan(&op.ID, &op.PassID, &op.Step, &policy, &kind,
			&op.Target, &detail, &inverse, &applied); err != nil {
			return nil, fmt.Errorf("sleep: pass ops: scan: %w", err)
		}
		op.Policy = Policy(policy)
		op.Kind = OpKind(kind)
		op.Detail = detail.String
		op.Inverse = inverse.String
		op.Applied = applied != 0
		out = append(out, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sleep: pass ops: %w", err)
	}
	return out, nil
}

// MarkNarrated stamps narrated_at on passID and reports whether THIS
// call did the stamping. The WHERE narrated_at IS NULL guard makes the
// mark atomic: two racing callers (e.g. two sessions starting at once)
// see exactly one true. A zero now defaults to time.Now().UTC().
func MarkNarrated(ctx context.Context, db *sql.DB, passID int64, now time.Time) (bool, error) {
	if db == nil {
		return false, fmt.Errorf("sleep: mark narrated: nil db")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	res, err := db.ExecContext(ctx,
		`UPDATE sleep_passes SET narrated_at = ? WHERE id = ? AND narrated_at IS NULL`,
		now.Format(time.RFC3339), passID,
	)
	if err != nil {
		return false, fmt.Errorf("sleep: mark narrated: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sleep: mark narrated: rows affected: %w", err)
	}
	return n == 1, nil
}

// LastPassEndedAt returns when the most recent pass ended. ok is false
// when no pass has ever completed; the scheduler reads that as "a pass
// is due".
func LastPassEndedAt(ctx context.Context, db *sql.DB) (ended time.Time, ok bool, err error) {
	if db == nil {
		return time.Time{}, false, fmt.Errorf("sleep: last pass ended at: nil db")
	}
	var raw string
	err = db.QueryRowContext(ctx,
		`SELECT ended_at
		   FROM sleep_passes
		  WHERE ended_at IS NOT NULL
		  ORDER BY ended_at DESC, id DESC
		  LIMIT 1`,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("sleep: last pass ended at: %w", err)
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("sleep: last pass ended at: parse %q: %w", raw, err)
	}
	return t, true, nil
}

// scanPass scans one sleep_passes row from rows.
func scanPass(rows *sql.Rows) (Pass, error) {
	var (
		p                 Pass
		trigger           string
		budgetMS          int64
		startedAt         string
		endedAt, narrated sql.NullString
	)
	if err := rows.Scan(&p.ID, &startedAt, &endedAt, &trigger, &budgetMS, &narrated); err != nil {
		return Pass{}, fmt.Errorf("scan: %w", err)
	}
	started, err := time.Parse(time.RFC3339, startedAt)
	if err != nil {
		return Pass{}, fmt.Errorf("parse started_at %q: %w", startedAt, err)
	}
	p.StartedAt = started
	if endedAt.Valid {
		t, err := time.Parse(time.RFC3339, endedAt.String)
		if err != nil {
			return Pass{}, fmt.Errorf("parse ended_at %q: %w", endedAt.String, err)
		}
		p.EndedAt = t
	}
	if narrated.Valid {
		t, err := time.Parse(time.RFC3339, narrated.String)
		if err != nil {
			return Pass{}, fmt.Errorf("parse narrated_at %q: %w", narrated.String, err)
		}
		p.NarratedAt = t
	}
	p.Trigger = Trigger(trigger)
	p.Budget = time.Duration(budgetMS) * time.Millisecond
	return p, nil
}

// nullable maps "" to NULL so optional JSON payload columns stay NULL
// rather than empty strings.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
