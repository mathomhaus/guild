package lore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/storage"
)

// openTestDB opens a fresh migrated lore DB under t.TempDir() and
// registers the one-or-more project ids the test needs. The returned
// *sql.DB closes automatically at test-end.
func openTestDB(t *testing.T, projectIDs ...string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lore.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := storage.MigrateTo(ctx, db, "test", nil); err != nil {
		t.Fatalf("storage.Migrate: %v", err)
	}

	for _, pid := range projectIDs {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO projects (id, path) VALUES (?, ?)`,
			pid, "/fake/"+pid,
		); err != nil {
			t.Fatalf("insert project %q: %v", pid, err)
		}
	}

	return db
}

// TestInscribe_HappyPath_PerKind exercises every kind once with the
// minimum required fields + a couple of optional extras.
func TestInscribe_HappyPath_PerKind(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	kinds := AllKinds()
	for i, k := range kinds {
		k := k
		i := i
		t.Run(string(k), func(t *testing.T) {
			params := InscribeParams{
				ProjectID: "alpha",
				Kind:      k,
				Title:     "title " + string(k) + " entry",
				Summary:   "a two sentence summary that is long enough.",
				Topic:     "test-topic",
				Tags:      []string{"t1", "t2"},
				Now:       time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC),
			}
			res, err := Inscribe(ctx, db, &params)
			if err != nil {
				t.Fatalf("inscribe %q: %v", k, err)
			}
			if res.Entry.ID != int64(i+1) {
				t.Errorf("want id %d, got %d", i+1, res.Entry.ID)
			}
			wantStatus := StatusCurrent
			if k == KindIdea {
				wantStatus = StatusSeed
			}
			if res.Entry.Status != wantStatus {
				t.Errorf("kind=%s status: want %s, got %s", k, wantStatus, res.Entry.Status)
			}
			// valid_days should match the kind default.
			want := kindValidDays(k)
			if (want == nil) != (res.Entry.ValidDays == nil) {
				t.Errorf("kind=%s valid_days: want nil=%v, got nil=%v",
					k, want == nil, res.Entry.ValidDays == nil)
			}
			if want != nil && res.Entry.ValidDays != nil && *want != *res.Entry.ValidDays {
				t.Errorf("kind=%s valid_days: want %d, got %d", k, *want, *res.Entry.ValidDays)
			}
			// No dedup hits on a fresh row of this kind.
			if len(res.DedupHits) != 0 {
				t.Errorf("want no dedup hits, got %d", len(res.DedupHits))
			}
		})
	}
}

// TestInscribe_RequiresFields verifies validation happens before any DB IO.
func TestInscribe_RequiresFields(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")
	base := InscribeParams{
		ProjectID: "alpha",
		Kind:      KindDecision,
		Title:     "t",
		Summary:   "s",
		Topic:     "x",
	}
	cases := []struct {
		name string
		mut  func(*InscribeParams)
		want error
	}{
		{"no-project", func(p *InscribeParams) { p.ProjectID = "" }, ErrMissingField},
		{"no-title", func(p *InscribeParams) { p.Title = "" }, ErrMissingField},
		{"no-summary", func(p *InscribeParams) { p.Summary = "" }, ErrMissingField},
		{"no-topic", func(p *InscribeParams) { p.Topic = "" }, ErrMissingField},
		{"bad-kind", func(p *InscribeParams) { p.Kind = "whatever" }, ErrInvalidKind},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			p := base
			c.mut(&p)
			_, err := Inscribe(ctx, db, &p)
			if !errors.Is(err, c.want) {
				t.Errorf("want %v, got %v", c.want, err)
			}
		})
	}
}

// TestInscribe_CrossProjectDedup_Default verifies the default behavior
// (StrictProject=false): an entry with the same title in project BETA
// surfaces the original ENTRY-1 (created in project ALPHA) as a dedup
// hit. This is THE acceptance test for the quest's cross-project-dedup
// requirement.
func TestInscribe_CrossProjectDedup_Default(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha", "beta")

	first, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha",
		Kind:      KindResearch,
		Title:     "shared cross project dedup regression test title",
		Summary:   "first inscribed in alpha.",
		Topic:     "retrieval",
	})
	if err != nil {
		t.Fatalf("first inscribe: %v", err)
	}
	t.Logf("first entry: ENTRY-%d in project %s", first.Entry.ID, first.Entry.ProjectID)

	second, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "beta",
		Kind:      KindResearch,
		Title:     "shared cross project dedup regression test title",
		Summary:   "second inscribed in beta, SAME title.",
		Topic:     "retrieval",
	})
	if err != nil {
		t.Fatalf("second inscribe: %v", err)
	}
	t.Logf("second entry: ENTRY-%d in project %s", second.Entry.ID, second.Entry.ProjectID)

	if len(second.DedupHits) == 0 {
		t.Fatalf("want dedup hits, got none — cross-project dedup NOT default")
	}
	found := false
	for _, h := range second.DedupHits {
		t.Logf("dedup hit: ENTRY-%d [project=%s] title=%q", h.EntryID, h.ProjectID, h.Title)
		if h.EntryID == first.Entry.ID && h.ProjectID == "alpha" {
			found = true
		}
	}
	if !found {
		t.Errorf("dedup hits did not contain the first (alpha) entry")
	}
}

// TestInscribe_StrictProject_OptsOut verifies --strict-project suppresses
// the cross-project dedup and only surfaces same-project hits.
func TestInscribe_StrictProject_OptsOut(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha", "beta")

	_, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha",
		Kind:      KindResearch,
		Title:     "strict project dedup harness demo title",
		Summary:   "in alpha.",
		Topic:     "retrieval",
	})
	if err != nil {
		t.Fatalf("first inscribe: %v", err)
	}

	second, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID:     "beta",
		Kind:          KindResearch,
		Title:         "strict project dedup harness demo title",
		Summary:       "same title in beta, but strict-project on.",
		Topic:         "retrieval",
		StrictProject: true,
	})
	if err != nil {
		t.Fatalf("second inscribe: %v", err)
	}
	if len(second.DedupHits) != 0 {
		t.Errorf("want zero dedup hits under --strict-project, got %d", len(second.DedupHits))
	}
}

// TestInscribe_PrincipleBloatWarning verifies a 70-word principle gets
// the bloat flag set in the result, but the entry is still inserted.
func TestInscribe_PrincipleBloatWarning(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	// Build a >60-word title+summary combo.
	// Build a summary >60 words — test with 70 words combined title+summary.
	long := "principles bloat the oath wall when their combined title and summary exceed sixty words which happens often when agents try to encode policy as prose rather than as short memorable rules that actually fit in a session start context window without burning tokens on every session start call across every single project in the workspace today"
	res, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha",
		Kind:      KindPrinciple,
		Title:     "tooooo long principle header goes here for oath hygiene",
		Summary:   long,
		Topic:     "hygiene",
	})
	if err != nil {
		t.Fatalf("inscribe: %v", err)
	}
	if !res.BloatWarned {
		t.Errorf("want BloatWarned=true for long principle (words=%d)", res.BloatWords)
	}
	if res.BloatWords <= PrincipleMaxWordsDefault {
		t.Errorf("want >%d word count, got %d", PrincipleMaxWordsDefault, res.BloatWords)
	}
	if res.Entry == nil || res.Entry.ID == 0 {
		t.Errorf("entry should still be inserted even when bloat warned")
	}
}

// TestInscribe_NoWarn_SuppressesBloat verifies --no-warn skips the
// ≤60-word check entirely.
func TestInscribe_NoWarn_SuppressesBloat(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	long := "principles bloat the oath wall when their combined title and summary exceed sixty words which happens often when agents try to encode policy as prose rather than as short memorable rules that actually fit in a session start context window without burning tokens on every session start call across every single project in the workspace today"
	res, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha",
		Kind:      KindPrinciple,
		Title:     "tooooo long principle header goes here for oath hygiene",
		Summary:   long,
		Topic:     "hygiene",
		NoWarn:    true,
	})
	if err != nil {
		t.Fatalf("inscribe: %v", err)
	}
	if res.BloatWarned {
		t.Errorf("--no-warn should suppress BloatWarned (got words=%d)", res.BloatWords)
	}
}

// TestFtsDedupQuery_ShortTitlesReturnEmpty ensures the dedup query
// builder refuses to construct a query from a title that won't
// distinguish entries — fewer than 3 usable content tokens.
func TestFtsDedupQuery_ShortTitlesReturnEmpty(t *testing.T) {
	cases := []string{
		"",
		"short",
		"the and for",
		"a b c d e", // all too short (<4 chars)
	}
	for _, tc := range cases {
		if got := ftsDedupQuery(tc); got != "" {
			t.Errorf("ftsDedupQuery(%q) = %q, want empty", tc, got)
		}
	}
}

func TestFtsDedupQuery_BuildsANDExpression(t *testing.T) {
	got := ftsDedupQuery("shared cross project dedup regression test title")
	// Expect AND of prefixes, capped at 5 content tokens.
	if got == "" {
		t.Fatalf("expected non-empty query")
	}
	// Must include AND separators, not OR.
	for _, fragment := range []string{" AND ", "*"} {
		if !contains(got, fragment) {
			t.Errorf("query missing %q fragment: %q", fragment, got)
		}
	}
}

// TestInscribe_Informs_ZeroSources verifies no regression when Informs is nil
// (existing callers that don't pass informs must behave identically).
func TestInscribe_Informs_ZeroSources(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	res, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha",
		Kind:      KindDecision,
		Title:     "informs zero sources regression test",
		Summary:   "should insert without errors and return a valid entry.",
		Topic:     "test",
	})
	if err != nil {
		t.Fatalf("inscribe: %v", err)
	}
	if res.Entry == nil || res.Entry.ID == 0 {
		t.Fatal("expected a valid entry")
	}
}

// TestInscribe_Informs_OneSource verifies a single informs ID creates a provenance edge.
func TestInscribe_Informs_OneSource(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	// Insert the source entry first.
	src, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha",
		Kind:      KindResearch,
		Title:     "source entry for informs single test",
		Summary:   "the source knowledge that will inform another entry.",
		Topic:     "test",
	})
	if err != nil {
		t.Fatalf("source inscribe: %v", err)
	}

	dst, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha",
		Kind:      KindDecision,
		Title:     "destination entry informed by one source",
		Summary:   "decision that builds on the source research above.",
		Topic:     "test",
		Informs:   []int64{src.Entry.ID},
	})
	if err != nil {
		t.Fatalf("inscribe with informs: %v", err)
	}

	// Verify the edge exists in entry_links.
	var fromID, toID int64
	var rel string
	err = db.QueryRowContext(ctx,
		`SELECT from_id, to_id, relation FROM entry_links WHERE from_id = ? AND to_id = ?`,
		src.Entry.ID, dst.Entry.ID,
	).Scan(&fromID, &toID, &rel)
	if err != nil {
		t.Fatalf("entry_links lookup: %v", err)
	}
	if fromID != src.Entry.ID || toID != dst.Entry.ID || rel != string(RelationInforms) {
		t.Errorf("unexpected edge: from=%d to=%d rel=%s", fromID, toID, rel)
	}
}

// TestInscribe_Informs_ThreeSources verifies multiple informs IDs all create edges.
func TestInscribe_Informs_ThreeSources(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	var srcIDs []int64
	for i := 0; i < 3; i++ {
		s, err := Inscribe(ctx, db, &InscribeParams{
			ProjectID: "alpha",
			Kind:      KindResearch,
			Title:     fmt.Sprintf("multi source research entry number %d", i),
			Summary:   "source knowledge for the multi-informs test.",
			Topic:     "test",
		})
		if err != nil {
			t.Fatalf("source %d inscribe: %v", i, err)
		}
		srcIDs = append(srcIDs, s.Entry.ID)
	}

	dst, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha",
		Kind:      KindDecision,
		Title:     "decision informed by three research entries result",
		Summary:   "synthesises all three sources into a single decision.",
		Topic:     "test",
		Informs:   srcIDs,
	})
	if err != nil {
		t.Fatalf("inscribe with 3 informs: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entry_links WHERE to_id = ? AND relation = 'informs'`,
		dst.Entry.ID,
	).Scan(&count); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if count != 3 {
		t.Errorf("want 3 informs edges, got %d", count)
	}
}

// TestInscribe_Informs_InvalidID verifies that a non-existent source ID causes
// the inscribe to fail and rolls back the new entry.
func TestInscribe_Informs_InvalidID(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	const nonExistentID = int64(9999)
	_, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha",
		Kind:      KindDecision,
		Title:     "should not persist due to bad informs target",
		Summary:   "this entry should be rolled back if informs id is invalid.",
		Topic:     "test",
		Informs:   []int64{nonExistentID},
	})
	if err == nil {
		t.Fatal("expected error for non-existent informs id, got nil")
	}

	// Verify the entry was deleted (rolled back).
	var n int
	if scanErr := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entries WHERE title = 'should not persist due to bad informs target'`,
	).Scan(&n); scanErr != nil {
		t.Fatalf("entries count: %v", scanErr)
	}
	if n != 0 {
		t.Errorf("want 0 entries after rollback, got %d", n)
	}
}

// TestInscribe_Informs_SelfLink verifies that referencing the new entry's own
// ID is rejected. Since the ID is not known before insert, this can only be
// triggered by passing the ID after a previous inscribe.
func TestInscribe_Informs_SelfLink(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "alpha")

	// First inscribe to get a real ID, then try to inscribe a second entry
	// that informs itself — use a known future ID trick by inserting a
	// placeholder and using its ID.
	first, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha",
		Kind:      KindDecision,
		Title:     "placeholder entry to determine next id for self link test",
		Summary:   "we use this to infer the next auto-increment id.",
		Topic:     "test",
	})
	if err != nil {
		t.Fatalf("placeholder inscribe: %v", err)
	}
	nextID := first.Entry.ID + 1

	_, err = Inscribe(ctx, db, &InscribeParams{
		ProjectID: "alpha",
		Kind:      KindDecision,
		Title:     "entry that attempts to inform itself via predicted id",
		Summary:   "the self-link guard should reject this and roll back.",
		Topic:     "test",
		Informs:   []int64{nextID}, // this IS the new entry's id
	})
	if err == nil {
		t.Fatal("expected self-link error, got nil")
	}
	if !errors.Is(err, ErrSelfLink) {
		t.Errorf("want ErrSelfLink, got: %v", err)
	}
}

// TestInscribe_Body_PersistsAndStudyReturnsIt verifies the verbatim body
// survives the write and comes back intact on the deep read — the whole
// point of the field. summary is the search snippet; body is the payload.
func TestInscribe_Body_PersistsAndStudyReturnsIt(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "bodyproj")

	const fullBody = "# Investigation\n\nTokens expire at exactly 3600s. The refresh\nendpoint returns a new pair; the old refresh token is revoked on use.\nSee the trace in run-4417 for the 401 → 200 sequence."

	res, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "bodyproj",
		Kind:      KindObservation,
		Title:     "auth token refresh lifetime",
		Summary:   "tokens expire at 1h; refresh rotates the pair.",
		Body:      fullBody,
		Topic:     "auth",
		Now:       time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("inscribe: %v", err)
	}
	if res.Entry.Body != fullBody {
		t.Errorf("returned Entry.Body = %q, want %q", res.Entry.Body, fullBody)
	}

	// The read surface (study) must return the full body verbatim.
	study, err := Study(ctx, db, res.Entry.ID)
	if err != nil {
		t.Fatalf("study: %v", err)
	}
	if study.Entry.Body != fullBody {
		t.Errorf("studied Entry.Body = %q, want %q", study.Entry.Body, fullBody)
	}
	// summary is independent of body — it stays the short distillation.
	if study.Entry.Summary == study.Entry.Body {
		t.Error("summary and body should be distinct; body must not overwrite summary")
	}
}

// TestInscribe_Body_DefaultsEmpty verifies an inscribe with no body leaves
// the column at the empty-string default (pre-009 / no-body behavior) and
// reads back as "" rather than erroring on the NOT NULL column.
func TestInscribe_Body_DefaultsEmpty(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, "bodyproj")

	res, err := Inscribe(ctx, db, &InscribeParams{
		ProjectID: "bodyproj",
		Kind:      KindResearch,
		Title:     "entry with no body supplied",
		Summary:   "a summary long enough to pass validation.",
		Topic:     "test",
		Now:       time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("inscribe: %v", err)
	}
	if res.Entry.Body != "" {
		t.Errorf("Entry.Body = %q, want empty string", res.Entry.Body)
	}
	study, err := Study(ctx, db, res.Entry.ID)
	if err != nil {
		t.Fatalf("study: %v", err)
	}
	if study.Entry.Body != "" {
		t.Errorf("studied Entry.Body = %q, want empty string", study.Entry.Body)
	}
}

// --- tiny utility: contains without importing strings from test helpers ---
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
