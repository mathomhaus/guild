package eval

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/storage"
)

// grid.go runs the adversarial recall/ranking grid: it seeds the fixed
// scratch corpus into an isolated in-memory lore database, runs guild's real
// Appraise pipeline (the same BM25 + recency + title-boost ranker the lore
// module ships) for every probe, and turns the ranked output into RED/GREEN
// verdicts.
//
// What the grid actually catches:
//
//   - Poison-outranks-answer: a keyword-stuffed, injection-shaped, or
//     near-duplicate entry that shares a probe's terms ending up ABOVE the
//     genuine answer in the ranking. This is the recall-poisoning failure
//     mode — "win the ranking by gaming term frequency / recency" — and is
//     the grid's primary signal. Each such occurrence is one RED cell.
//   - Benign-recall-regression: the genuine answer falling out of rank 1 for
//     its probe (even if no specific poison beat it). A drop here means the
//     ranker stopped surfacing the right entry first, which is a recall
//     regression regardless of cause.
//
// Determinism: the corpus is fixed code, Appraise is seeded with a fixed
// reference now (so the recency arm never reads the wall clock), the embedder
// is left nil (pure BM25 + stopwords, no ANN, no model), and the database is
// :memory: with a deterministic seed order. Same corpus + same config =>
// same verdicts, every run, on every machine.

// referenceNow is the fixed clock the grid hands Appraise. Pinned so recency
// decay is identical run to run. The exact instant is arbitrary; only its
// fixedness matters.
var referenceNow = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// Verdict is one RED/GREEN cell of the grid: the outcome of one probe against
// the seeded corpus. A cell is GREEN when the genuine answer ranked first and
// no poison outranked it; RED otherwise. The struct is JSON-serialisable so
// the same shape feeds both the CLI/MCP surface and the golden fixtures.
type Verdict struct {
	// Probe is the probe name (grid row key).
	Probe string `json:"probe"`
	// Query is the raw search string, echoed for legibility.
	Query string `json:"query"`
	// Green reports the overall pass/fail for the cell.
	Green bool `json:"green"`
	// WantSlug is the corpus slug that should have won.
	WantSlug string `json:"want_slug"`
	// WinnerSlug is the slug that actually ranked first ("" when the probe
	// returned no results, which is itself a RED).
	WinnerSlug string `json:"winner_slug"`
	// WantRank is the 1-based rank the genuine answer landed at, or -1 when
	// it was absent from the results entirely.
	WantRank int `json:"want_rank"`
	// PoisonsAhead lists, in rank order, the poison slugs that outranked the
	// genuine answer. Empty on a GREEN cell.
	PoisonsAhead []string `json:"poisons_ahead,omitempty"`
	// Reason is a short human-readable explanation of a RED verdict, "" when
	// GREEN.
	Reason string `json:"reason,omitempty"`
}

// GridResult is the full grid outcome: every probe's verdict plus a rollup.
// Serialised verbatim into golden fixtures and rendered by the CLI/MCP
// surface.
type GridResult struct {
	// Verdicts is one cell per probe, in probe declaration order (stable).
	Verdicts []Verdict `json:"verdicts"`
	// Total is len(Verdicts); GreenCount is how many passed. RedCount is the
	// derived failure count.
	Total      int `json:"total"`
	GreenCount int `json:"green"`
	RedCount   int `json:"red"`
}

// AllGreen reports whether every cell passed. The CLI/MCP surface uses it to
// pick the headline glyph; callers that gate on the grid (CI) exit non-zero
// when this is false.
func (r GridResult) AllGreen() bool { return r.RedCount == 0 }

// RunGrid seeds the fixed corpus into an isolated lore database and evaluates
// every probe, returning the RED/GREEN grid. The scoring config is supplied
// so a caller can prove the grid is sensitive to ranking knobs (the default
// is lore.DefaultScoring()). It opens, migrates, seeds, and closes its own
// :memory: database; the real ~/.guild is never touched.
func RunGrid(ctx context.Context, scoring lore.ScoringConfig) (GridResult, error) {
	db, err := openScratchDB(ctx)
	if err != nil {
		return GridResult{}, err
	}
	defer func() { _ = db.Close() }()
	return runGridOn(ctx, db, scoring)
}

// runGridOn evaluates the probes against an already-seeded database. Split
// from RunGrid so tests can seed once and re-run, and so the parity harness
// shares the exact same evaluation path.
func runGridOn(ctx context.Context, db *sql.DB, scoring lore.ScoringConfig) (GridResult, error) {
	slugByID, err := seedScratchCorpus(ctx, db)
	if err != nil {
		return GridResult{}, err
	}

	probes := gridProbes()
	res := GridResult{Verdicts: make([]Verdict, 0, len(probes)), Total: len(probes)}
	for _, p := range probes {
		v, err := evalProbe(ctx, db, p, scoring, slugByID)
		if err != nil {
			return GridResult{}, err
		}
		res.Verdicts = append(res.Verdicts, v)
		if v.Green {
			res.GreenCount++
		} else {
			res.RedCount++
		}
	}
	return res, nil
}

// evalProbe runs one probe through Appraise and computes its verdict. The
// poison set is consulted by slug so the verdict names rows the way the
// corpus and fixtures do, independent of autoincrement ids.
func evalProbe(ctx context.Context, db *sql.DB, p probe, scoring lore.ScoringConfig, slugByID map[int64]string) (Verdict, error) {
	out, err := lore.Appraise(ctx, db, lore.AppraiseParams{
		Query:       p.Query,
		Limit:       10,
		AllProjects: true,
		Scoring:     scoring,
		Now:         referenceNow,
	})
	if err != nil {
		return Verdict{}, fmt.Errorf("eval: probe %q: appraise: %w", p.Name, err)
	}

	v := Verdict{
		Probe:    p.Name,
		Query:    p.Query,
		WantSlug: p.WantSlug,
		WantRank: -1,
	}
	poison := make(map[string]bool, len(p.PoisonSlugs))
	for _, s := range p.PoisonSlugs {
		poison[s] = true
	}

	// Walk the ranked results once, recording the winner, the want-rank, and
	// any poison that sits ahead of the genuine answer.
	var poisonsAhead []string
	wantSeen := false
	for i, r := range out.Results {
		slug := slugByID[r.Entry.ID]
		if i == 0 {
			v.WinnerSlug = slug
		}
		if slug == p.WantSlug {
			v.WantRank = i + 1
			wantSeen = true
			continue
		}
		// A poison ranked before the genuine answer is the failure we hunt.
		if poison[slug] && !wantSeen {
			poisonsAhead = append(poisonsAhead, slug)
		}
	}
	v.PoisonsAhead = poisonsAhead

	switch {
	case v.WantRank == -1:
		v.Green = false
		v.Reason = "benign answer absent from results (recall miss)"
	case len(poisonsAhead) > 0:
		v.Green = false
		v.Reason = fmt.Sprintf("%d poison(s) outranked the answer (answer at rank %d)",
			len(poisonsAhead), v.WantRank)
	case v.WantRank != 1:
		v.Green = false
		v.Reason = fmt.Sprintf("benign answer ranked %d, not 1", v.WantRank)
	default:
		v.Green = true
	}
	return v, nil
}

// openScratchDB opens and migrates an isolated in-memory lore database. Pure
// in-memory (:memory:) so nothing touches the real ~/.guild and no temp file
// is left behind; the schema comes from the shared migration corpus, exactly
// what a real lore.db carries.
func openScratchDB(ctx context.Context) (*sql.DB, error) {
	db, err := storage.Open(ctx, ":memory:")
	if err != nil {
		return nil, fmt.Errorf("eval: open scratch db: %w", err)
	}
	// MigrateTo with io.Discard mutes the "🔧 applied schema migration ..."
	// upgrade notices: a throwaway scratch DB applies every migration on every
	// run, and those lines are noise on stderr (they would otherwise dwarf the
	// verdict output). The real ~/.guild path keeps using the stderr-logging
	// Migrate; only this ephemeral corpus is silent.
	if err := storage.MigrateTo(ctx, db, "eval-scratch", io.Discard); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("eval: migrate scratch db: %w", err)
	}
	return db, nil
}

// seedScratchCorpus inserts every fixed corpus row and returns a map from the
// inserted primary key back to its stable slug, so verdicts and fixtures can
// refer to rows by slug rather than autoincrement id. Projects are registered
// on first sight. created_at is derived from the row's AgeDays offset back
// from referenceNow, keeping recency deterministic.
func seedScratchCorpus(ctx context.Context, db *sql.DB) (map[int64]string, error) {
	corpus := scratchCorpus()
	seenProj := map[string]bool{}
	for _, e := range corpus {
		if seenProj[e.ProjectID] {
			continue
		}
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO projects (id, path) VALUES (?, ?)`,
			e.ProjectID, "/eval/"+e.ProjectID); err != nil {
			return nil, fmt.Errorf("eval: seed project %q: %w", e.ProjectID, err)
		}
		seenProj[e.ProjectID] = true
	}

	slugByID := make(map[int64]string, len(corpus))
	for _, e := range corpus {
		createdAt := referenceNow.AddDate(0, 0, -e.AgeDays).Format(time.RFC3339)
		res, err := db.ExecContext(ctx,
			`INSERT INTO entries
			 (project_id, topic, kind, title, summary, tags, status, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, 'current', ?, ?)`,
			e.ProjectID, "eval-scratch", e.Kind, e.Title, e.Summary, e.Tags, createdAt, createdAt)
		if err != nil {
			return nil, fmt.Errorf("eval: seed entry %q: %w", e.Slug, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("eval: seed entry %q: last insert id: %w", e.Slug, err)
		}
		slugByID[id] = e.Slug
	}
	return slugByID, nil
}

// sortedSlugs returns the corpus slugs in sorted order. Used by tests and the
// parity harness for stable iteration where map order would otherwise leak.
func sortedSlugs() []string {
	corpus := scratchCorpus()
	out := make([]string, 0, len(corpus))
	for _, e := range corpus {
		out = append(out, e.Slug)
	}
	sort.Strings(out)
	return out
}
