package sleep

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/quest"
	"github.com/mathomhaus/guild/internal/storage"
)

// openConsolidateDB opens a fresh migrated DB under t.TempDir() and
// registers the given project ids (entries and task_status both carry
// a projects FK). Used for both the lore and the quest side; the
// migration chain is shared.
func openConsolidateDB(t *testing.T, name string, projects ...string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), name)
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := storage.MigrateTo(ctx, db, "", io.Discard); err != nil {
		t.Fatalf("migrate %s: %v", name, err)
	}
	for _, pid := range projects {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO projects (id, path) VALUES (?, ?)`, pid, "/fake/"+pid,
		); err != nil {
			t.Fatalf("register project %q in %s: %v", pid, name, err)
		}
	}
	return db
}

// seedEntry inserts one status=current entry and returns its id.
func seedEntry(t *testing.T, db *sql.DB, project, kind, title, summary string) int64 {
	t.Helper()
	res, err := db.ExecContext(context.Background(),
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, created_at, updated_at)
		 VALUES (?, 'test', ?, ?, ?, 'current', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		project, kind, title, summary,
	)
	if err != nil {
		t.Fatalf("seed entry: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("seed entry id: %v", err)
	}
	return id
}

// entryState returns (status, kind) for one entry.
func entryState(t *testing.T, db *sql.DB, id int64) (status, kind string) {
	t.Helper()
	if err := db.QueryRowContext(context.Background(),
		`SELECT status, kind FROM entries WHERE id = ?`, id,
	).Scan(&status, &kind); err != nil {
		t.Fatalf("entry state %d: %v", id, err)
	}
	return status, kind
}

// runConsolidate executes one full pass with just the consolidation
// step and returns the pass result. Fails the test on substrate errors
// or a step error.
func runConsolidate(t *testing.T, loreDB, questDB *sql.DB, step *ConsolidateStep, caps Caps) *PassResult {
	t.Helper()
	pc := &PassContext{
		LoreDB:  loreDB,
		QuestDB: questDB,
		Trigger: TriggerAutopass,
		Caps:    caps,
	}
	res, err := Run(context.Background(), pc, []Step{step}, time.Minute)
	if err != nil {
		t.Fatalf("run pass: %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(res.Steps))
	}
	if res.Steps[0].Err != nil {
		t.Fatalf("step error: %v", res.Steps[0].Err)
	}
	return res
}

// passOps fetches the journal rows of one pass.
func passOps(t *testing.T, db *sql.DB, passID int64) []Op {
	t.Helper()
	ops, err := PassOps(context.Background(), db, passID)
	if err != nil {
		t.Fatalf("pass ops: %v", err)
	}
	return ops
}

// questSubjects returns every quest's subject (any status) keyed by
// "project/QUEST-N", resolved through the canonical spec replay.
func questSubjects(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, `SELECT project_id, task_id FROM task_status`)
	if err != nil {
		t.Fatalf("list quests: %v", err)
	}
	defer func() { _ = rows.Close() }()
	type ref struct{ project, id string }
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.project, &r.id); err != nil {
			t.Fatalf("scan quest ref: %v", err)
		}
		refs = append(refs, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate quest refs: %v", err)
	}
	out := make(map[string]string, len(refs))
	for _, r := range refs {
		q, err := quest.Load(ctx, db, r.project, r.id)
		if err != nil {
			t.Fatalf("load quest %s/%s: %v", r.project, r.id, err)
		}
		out[r.project+"/"+r.id] = q.Subject
	}
	return out
}

// detailMap parses an op's JSON detail payload.
func detailMap(t *testing.T, op Op) map[string]any {
	t.Helper()
	m := make(map[string]any)
	if err := json.Unmarshal([]byte(op.Detail), &m); err != nil {
		t.Fatalf("parse detail %q: %v", op.Detail, err)
	}
	return m
}

// wordBlock builds n distinct words from a vocabulary prefix, so test
// texts can be made word-count-heavy without accidentally creating
// Jaccard similarity across unrelated entries.
func wordBlock(prefix string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%s%03d ", prefix, i)
	}
	return strings.TrimSpace(b.String())
}

// TestConsolidate_ExactDuplicates_AutoMelded seeds an exact duplicate
// pair across two projects and asserts the older entry is superseded
// by the newer via the canonical reforge (status flip + supersedes
// edge), journaled as an auto op with an inverse payload. A second
// pass is a no-op: the superseded entry no longer surfaces in meld.
func TestConsolidate_ExactDuplicates_AutoMelded(t *testing.T) {
	loreDB := openConsolidateDB(t, "lore.db", "alpha", "beta")
	questDB := openConsolidateDB(t, "quest.db", "alpha", "beta")

	title := "agents must appraise before inscribing to avoid duplicate lore"
	summary := "always search existing lore before writing new entries to prevent knowledge bloat"
	older := seedEntry(t, loreDB, "alpha", "decision", title, summary)
	newer := seedEntry(t, loreDB, "beta", "decision", title, summary)

	res := runConsolidate(t, loreDB, questDB, &ConsolidateStep{}, Caps{})

	if got := res.Steps[0].Report.OpsApplied; got != 1 {
		t.Errorf("OpsApplied = %d, want 1", got)
	}
	if status, _ := entryState(t, loreDB, older); status != "superseded" {
		t.Errorf("older status = %q, want superseded", status)
	}
	if status, _ := entryState(t, loreDB, newer); status != "current" {
		t.Errorf("newer status = %q, want current", status)
	}

	var edges int
	if err := loreDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM entry_links WHERE from_id = ? AND to_id = ? AND relation = 'supersedes'`,
		newer, older,
	).Scan(&edges); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if edges != 1 {
		t.Errorf("supersedes edges newer->older = %d, want 1", edges)
	}

	ops := passOps(t, loreDB, res.PassID)
	if len(ops) != 1 {
		t.Fatalf("ops = %d, want 1: %+v", len(ops), ops)
	}
	op := ops[0]
	if op.Step != StepNameConsolidate || op.Policy != PolicyAuto || op.Kind != OpMeldExactSupersede || !op.Applied {
		t.Errorf("op = %+v, want consolidate/auto/meld_exact_supersede/applied", op)
	}
	wantTarget := fmt.Sprintf("LORE-%d<-LORE-%d", newer, older)
	if op.Target != wantTarget {
		t.Errorf("target = %q, want %q", op.Target, wantTarget)
	}
	var inv meldInverse
	if err := json.Unmarshal([]byte(op.Inverse), &inv); err != nil {
		t.Fatalf("parse inverse %q: %v", op.Inverse, err)
	}
	if inv.PriorStatus != "current" || inv.EdgeFrom != newer || inv.EdgeTo != older || inv.Relation != "supersedes" {
		t.Errorf("inverse = %+v, want {current %d %d supersedes}", inv, newer, older)
	}
	if n := len(questSubjects(t, questDB)); n != 0 {
		t.Errorf("quests posted = %d, want 0", n)
	}

	// Second pass: nothing left to do.
	res2 := runConsolidate(t, loreDB, questDB, &ConsolidateStep{}, Caps{})
	if ops2 := passOps(t, loreDB, res2.PassID); len(ops2) != 0 {
		t.Errorf("second pass ops = %d, want 0: %+v", len(ops2), ops2)
	}
}

// TestConsolidate_ChainOfThreeCollapsesToNewest verifies chain
// behavior: three identical entries collapse onto the newest copy with
// two reforges, never re-superseding an entry twice in one pass.
func TestConsolidate_ChainOfThreeCollapsesToNewest(t *testing.T) {
	loreDB := openConsolidateDB(t, "lore.db", "alpha")
	questDB := openConsolidateDB(t, "quest.db", "alpha")

	title := "shared exact duplicate title for the chain"
	summary := "identical summary text shared by all three copies of this entry"
	a := seedEntry(t, loreDB, "alpha", "decision", title, summary)
	b := seedEntry(t, loreDB, "alpha", "decision", title, summary)
	c := seedEntry(t, loreDB, "alpha", "decision", title, summary)

	res := runConsolidate(t, loreDB, questDB, &ConsolidateStep{}, Caps{})

	if got := res.Steps[0].Report.OpsApplied; got != 2 {
		t.Errorf("OpsApplied = %d, want 2", got)
	}
	for _, id := range []int64{a, b} {
		if status, _ := entryState(t, loreDB, id); status != "superseded" {
			t.Errorf("entry %d status = %q, want superseded", id, status)
		}
	}
	if status, _ := entryState(t, loreDB, c); status != "current" {
		t.Errorf("newest status = %q, want current", status)
	}
}

// TestConsolidate_NearAndDriftPairs_NeverMutated_ApprovalPosted covers
// the gated class: a near-match pair (score < 1.0) and an exact-text
// pair with kind drift. Neither is mutated; each gets one approval
// quest, journaled with policy=approval and applied=0.
func TestConsolidate_NearAndDriftPairs_NeverMutated_ApprovalPosted(t *testing.T) {
	loreDB := openConsolidateDB(t, "lore.db", "alpha", "beta")
	questDB := openConsolidateDB(t, "quest.db", "alpha", "beta")

	// Near pair: one differing token, cross-project.
	nearSummary := "appraise first then research once and inscribe findings"
	n1 := seedEntry(t, loreDB, "alpha", "decision",
		"agents should always appraise lore before researching anything new", nearSummary)
	n2 := seedEntry(t, loreDB, "beta", "decision",
		"agents should always appraise lore before researching anything today", nearSummary)

	// Drift pair: identical text, different kinds (distinct vocabulary
	// so it cannot near-match the first pair).
	driftTitle := "daemon socket shim routes stdio frames through unix listener"
	driftSummary := "frames travel across local transport boundaries without copying payload bytes"
	d1 := seedEntry(t, loreDB, "alpha", "decision", driftTitle, driftSummary)
	d2 := seedEntry(t, loreDB, "alpha", "research", driftTitle, driftSummary)

	step := &ConsolidateStep{NearThreshold: 0.5}
	res := runConsolidate(t, loreDB, questDB, step, Caps{})

	for _, tc := range []struct {
		id   int64
		kind string
	}{{n1, "decision"}, {n2, "decision"}, {d1, "decision"}, {d2, "research"}} {
		status, kind := entryState(t, loreDB, tc.id)
		if status != "current" || kind != tc.kind {
			t.Errorf("entry %d = (%s, %s), want (current, %s): gated pairs must never be mutated",
				tc.id, status, kind, tc.kind)
		}
	}

	if got := res.Steps[0].Report.OpsPosted; got != 2 {
		t.Errorf("OpsPosted = %d, want 2", got)
	}
	subjects := questSubjects(t, questDB)
	if len(subjects) != 2 {
		t.Fatalf("quests = %d, want 2: %v", len(subjects), subjects)
	}
	wantNear := fmt.Sprintf("approve meld: LORE-%d + LORE-%d", n1, n2)
	wantDrift := fmt.Sprintf("approve meld: LORE-%d + LORE-%d", d1, d2)
	var sawNear, sawDrift bool
	for key, subj := range subjects {
		switch subj {
		case wantNear:
			sawNear = true
			// Cross-project pair posts to the lower-id entry's project.
			if !strings.HasPrefix(key, "alpha/") {
				t.Errorf("near-pair quest posted under %q, want project alpha", key)
			}
		case wantDrift:
			sawDrift = true
		}
	}
	if !sawNear || !sawDrift {
		t.Errorf("subjects = %v, want %q and %q", subjects, wantNear, wantDrift)
	}

	ops := passOps(t, loreDB, res.PassID)
	if len(ops) != 2 {
		t.Fatalf("ops = %d, want 2: %+v", len(ops), ops)
	}
	for _, op := range ops {
		if op.Policy != PolicyApproval || op.Kind != OpNearMeld || op.Applied {
			t.Errorf("op = %+v, want approval/near_meld/not-applied", op)
		}
		d := detailMap(t, op)
		if q, _ := d["quest"].(string); !strings.HasPrefix(q, "QUEST-") {
			t.Errorf("detail quest = %v, want QUEST-N in %q", d["quest"], op.Detail)
		}
	}
}

// TestConsolidate_OathDemotion_NeverAutoApplied seeds one severe-bloat
// principle and one bloat-but-not-severe principle. The step must not
// change either kind (Commune runs with fix=false); the severe one gets
// exactly one approval quest naming the entry and the command to apply.
func TestConsolidate_OathDemotion_NeverAutoApplied(t *testing.T) {
	loreDB := openConsolidateDB(t, "lore.db", "alpha")
	questDB := openConsolidateDB(t, "quest.db", "alpha")

	severe := seedEntry(t, loreDB, "alpha", "principle",
		"severe oath wall principle", wordBlock("sev", 130))
	mild := seedEntry(t, loreDB, "alpha", "principle",
		"mildly bloated oath principle", wordBlock("blo", 70))

	res := runConsolidate(t, loreDB, questDB, &ConsolidateStep{}, Caps{})

	if _, kind := entryState(t, loreDB, severe); kind != "principle" {
		t.Errorf("severe kind = %q, want principle: demotion must never auto-apply during sleep", kind)
	}
	if _, kind := entryState(t, loreDB, mild); kind != "principle" {
		t.Errorf("mild kind = %q, want principle", kind)
	}

	subjects := questSubjects(t, questDB)
	if len(subjects) != 1 {
		t.Fatalf("quests = %d, want 1 (only the severe entry): %v", len(subjects), subjects)
	}
	want := fmt.Sprintf("approve oath demotion: LORE-%d (principle exceeds severe boundary)", severe)
	for _, subj := range subjects {
		if subj != want {
			t.Errorf("subject = %q, want %q", subj, want)
		}
	}

	ops := passOps(t, loreDB, res.PassID)
	if len(ops) != 1 {
		t.Fatalf("ops = %d, want 1: %+v", len(ops), ops)
	}
	op := ops[0]
	if op.Policy != PolicyApproval || op.Kind != OpOathDemotion || op.Applied {
		t.Errorf("op = %+v, want approval/oath_demotion/not-applied", op)
	}
	d := detailMap(t, op)
	if wc, _ := d["word_count"].(float64); int(wc) <= DefaultSevereBoundary {
		t.Errorf("detail word_count = %v, want > %d", d["word_count"], DefaultSevereBoundary)
	}
}

// TestConsolidate_AutoCap_OverflowDeferred seeds three exact-duplicate
// groups with MaxAutoOps=1: one meld applies, the other two are
// journaled as deferred (not silently dropped) and not mutated.
func TestConsolidate_AutoCap_OverflowDeferred(t *testing.T) {
	loreDB := openConsolidateDB(t, "lore.db", "alpha")
	questDB := openConsolidateDB(t, "quest.db", "alpha")

	var pairs [][2]int64
	for _, vocab := range []string{"first", "second", "third"} {
		title := vocab + " duplicate group title"
		summary := wordBlock(vocab, 8)
		l := seedEntry(t, loreDB, "alpha", "decision", title, summary)
		r := seedEntry(t, loreDB, "alpha", "decision", title, summary)
		pairs = append(pairs, [2]int64{l, r})
	}

	res := runConsolidate(t, loreDB, questDB, &ConsolidateStep{}, Caps{MaxAutoOps: 1})

	if got := res.Steps[0].Report.OpsApplied; got != 1 {
		t.Errorf("OpsApplied = %d, want 1 (cap)", got)
	}
	if status, _ := entryState(t, loreDB, pairs[0][0]); status != "superseded" {
		t.Errorf("first pair older status = %q, want superseded", status)
	}
	for _, p := range pairs[1:] {
		for _, id := range []int64{p[0], p[1]} {
			if status, _ := entryState(t, loreDB, id); status != "current" {
				t.Errorf("capped entry %d status = %q, want current", id, status)
			}
		}
	}

	ops := passOps(t, loreDB, res.PassID)
	applied, deferred := 0, 0
	for _, op := range ops {
		if op.Kind != OpMeldExactSupersede {
			t.Errorf("unexpected op kind %q", op.Kind)
			continue
		}
		if op.Applied {
			applied++
			continue
		}
		deferred++
		d := detailMap(t, op)
		if def, _ := d["deferred"].(bool); !def {
			t.Errorf("non-applied op missing deferred flag: %q", op.Detail)
		}
		if reason, _ := d["reason"].(string); reason != reasonAutoOpCap {
			t.Errorf("reason = %q, want %q", reason, reasonAutoOpCap)
		}
	}
	if applied != 1 || deferred != 2 {
		t.Errorf("applied/deferred = %d/%d, want 1/2", applied, deferred)
	}
}

// TestConsolidate_QuestPostCap_OverflowDeferred seeds two near pairs
// plus one severe oath with MaxQuestPosts=1: exactly one approval quest
// posts; the remaining candidates are journaled as deferred.
func TestConsolidate_QuestPostCap_OverflowDeferred(t *testing.T) {
	loreDB := openConsolidateDB(t, "lore.db", "alpha")
	questDB := openConsolidateDB(t, "quest.db", "alpha")

	seedNearPair := func(vocab string) {
		summary := wordBlock(vocab, 10)
		seedEntry(t, loreDB, "alpha", "decision", vocab+" near pair variant one", summary)
		seedEntry(t, loreDB, "alpha", "decision", vocab+" near pair variant two", summary)
	}
	seedNearPair("apple")
	seedNearPair("mango")
	seedEntry(t, loreDB, "alpha", "principle", "severe principle", wordBlock("sev", 130))

	step := &ConsolidateStep{NearThreshold: 0.5}
	res := runConsolidate(t, loreDB, questDB, step, Caps{MaxQuestPosts: 1})

	if got := res.Steps[0].Report.OpsPosted; got != 1 {
		t.Errorf("OpsPosted = %d, want 1 (cap)", got)
	}
	if n := len(questSubjects(t, questDB)); n != 1 {
		t.Errorf("quests = %d, want 1", n)
	}

	ops := passOps(t, loreDB, res.PassID)
	posted, deferred := 0, 0
	for _, op := range ops {
		if op.Applied {
			t.Errorf("approval-path op must never be applied: %+v", op)
		}
		d := detailMap(t, op)
		if def, _ := d["deferred"].(bool); def {
			deferred++
			if reason, _ := d["reason"].(string); reason != reasonQuestPostCap {
				t.Errorf("reason = %q, want %q", reason, reasonQuestPostCap)
			}
		} else {
			posted++
		}
	}
	if posted != 1 || deferred != 2 {
		t.Errorf("posted/deferred ops = %d/%d, want 1/2: %+v", posted, deferred, ops)
	}
}

// TestConsolidate_ApprovalDedupe_RerunPostsNothing runs the step twice
// over the same gated candidates: the second pass sees the open
// approval quests and posts nothing new.
func TestConsolidate_ApprovalDedupe_RerunPostsNothing(t *testing.T) {
	loreDB := openConsolidateDB(t, "lore.db", "alpha")
	questDB := openConsolidateDB(t, "quest.db", "alpha")

	summary := wordBlock("pear", 10)
	seedEntry(t, loreDB, "alpha", "decision", "pear near pair variant one", summary)
	seedEntry(t, loreDB, "alpha", "decision", "pear near pair variant two", summary)
	seedEntry(t, loreDB, "alpha", "principle", "severe principle", wordBlock("sev", 130))

	step := &ConsolidateStep{NearThreshold: 0.5}
	res1 := runConsolidate(t, loreDB, questDB, step, Caps{})
	if got := res1.Steps[0].Report.OpsPosted; got != 2 {
		t.Fatalf("first run OpsPosted = %d, want 2", got)
	}

	res2 := runConsolidate(t, loreDB, questDB, step, Caps{})
	if got := res2.Steps[0].Report.OpsPosted; got != 0 {
		t.Errorf("second run OpsPosted = %d, want 0 (deduped against open quests)", got)
	}
	if n := len(questSubjects(t, questDB)); n != 2 {
		t.Errorf("quests after rerun = %d, want 2", n)
	}
	if ops := passOps(t, loreDB, res2.PassID); len(ops) != 0 {
		t.Errorf("second pass ops = %d, want 0: %+v", len(ops), ops)
	}
}

// TestConsolidate_NilQuestDB_DefersApprovals runs with no quest db
// wired: gated candidates are journaled as deferred, nothing mutates,
// nothing panics.
func TestConsolidate_NilQuestDB_DefersApprovals(t *testing.T) {
	loreDB := openConsolidateDB(t, "lore.db", "alpha")

	summary := wordBlock("kiwi", 10)
	k1 := seedEntry(t, loreDB, "alpha", "decision", "kiwi near pair variant one", summary)
	k2 := seedEntry(t, loreDB, "alpha", "decision", "kiwi near pair variant two", summary)

	step := &ConsolidateStep{NearThreshold: 0.5}
	res := runConsolidate(t, loreDB, nil, step, Caps{})

	for _, id := range []int64{k1, k2} {
		if status, _ := entryState(t, loreDB, id); status != "current" {
			t.Errorf("entry %d status = %q, want current", id, status)
		}
	}
	ops := passOps(t, loreDB, res.PassID)
	if len(ops) != 1 {
		t.Fatalf("ops = %d, want 1: %+v", len(ops), ops)
	}
	d := detailMap(t, ops[0])
	if reason, _ := d["reason"].(string); reason != reasonNoQuestDB {
		t.Errorf("reason = %q, want %q", reason, reasonNoQuestDB)
	}
}

// TestConsolidate_CancelledContext_NoMutation verifies the step aborts
// cleanly under a cancelled context without mutating anything.
func TestConsolidate_CancelledContext_NoMutation(t *testing.T) {
	loreDB := openConsolidateDB(t, "lore.db", "alpha")
	title := "duplicate title for cancellation check"
	summary := "duplicate summary for cancellation check entries"
	a := seedEntry(t, loreDB, "alpha", "decision", title, summary)
	b := seedEntry(t, loreDB, "alpha", "decision", title, summary)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	step := &ConsolidateStep{}
	if _, err := step.Run(ctx, &PassContext{LoreDB: loreDB}); err == nil {
		t.Fatal("Run with cancelled context: want error, got nil")
	}
	for _, id := range []int64{a, b} {
		if status, _ := entryState(t, loreDB, id); status != "current" {
			t.Errorf("entry %d status = %q, want current", id, status)
		}
	}
}

// TestConsolidate_SourceGuards is the test seam for two acceptance
// rules that runtime tests cannot fully pin down:
//
//  1. lore.Seal is never called anywhere in the sleep package.
//  2. Every lore.Commune call passes a literal false for the fix
//     argument (oath demotion is never auto-applied during sleep).
//
// It parses the package's non-test sources and walks the AST, so a
// future edit that sneaks either call in fails this test instead of
// relying on PR review.
func TestConsolidate_SourceGuards(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	communeCalls := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "Seal" {
				t.Errorf("%s: references Seal at %s; sleep must never archive entries",
					name, fset.Position(sel.Pos()))
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || fn.Sel.Name != "Commune" {
				return true
			}
			communeCalls++
			// lore.Commune(ctx, db, projectID, allProjects, fix, ...):
			// fix is argument index 4 and must be the literal false.
			if len(call.Args) < 5 {
				t.Errorf("%s: Commune call at %s has %d args, want >= 5",
					name, fset.Position(call.Pos()), len(call.Args))
				return true
			}
			if ident, ok := call.Args[4].(*ast.Ident); !ok || ident.Name != "false" {
				t.Errorf("%s: Commune call at %s must pass literal false for fix",
					name, fset.Position(call.Pos()))
			}
			return true
		})
	}
	if communeCalls == 0 {
		t.Fatal("expected at least one lore.Commune call in the package sources")
	}
}
