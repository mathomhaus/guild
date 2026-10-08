package eval

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"

	"github.com/mathomhaus/guild/internal/lore"
)

// parity.go is the golden-fixture parity harness. It records the FULL ranked
// recall output (ordered, scored) for the fixed probe set against the fixed
// corpus into a committed JSON fixture, then re-runs and reports any drift.
// Where the grid asks "is the ranking gamed?", parity asks "did the ranking
// pipeline change AT ALL since we last locked it?" — a determinism lock on
// the whole BM25 + recency + title-boost stack across versions.
//
// The fixture is the source of truth; the harness fails (and a gating test
// exits non-zero) on any divergence: a reordered result, a changed score, a
// new or dropped row. An intentional change is adopted by regenerating the
// fixture under the GUILD_EVAL_UPDATE gate (TestParity_MatchesGolden rewrites
// the file when GUILD_EVAL_UPDATE=1), exactly the env-gated path the e2e
// golden suite uses (GUILD_E2E_UPDATE / make e2e-update).
//
// Scores are rounded to a fixed number of decimals before recording so the
// fixture is stable across floating-point backends — the same posture
// score.go already takes with its pinned pyLn2 constant.

// scoreDecimals is the rounding precision applied to recorded scores. Six
// decimals is far finer than any ranking decision needs yet coarse enough to
// absorb last-bit float differences between platforms.
const scoreDecimals = 6

// RankedRow is one recorded result row: the entry's stable slug, its 1-based
// rank, and its rounded final score. Slug (not the autoincrement id) is
// recorded so the fixture is stable across seed-order changes.
type RankedRow struct {
	Rank  int     `json:"rank"`
	Slug  string  `json:"slug"`
	Score float64 `json:"score"`
}

// ProbeRanking is the full ordered result list for one probe.
type ProbeRanking struct {
	Probe string      `json:"probe"`
	Query string      `json:"query"`
	Rows  []RankedRow `json:"rows"`
}

// Fixture is the complete recorded ranking snapshot: one ProbeRanking per
// probe, in probe declaration order. Marshalled to the golden JSON file.
type Fixture struct {
	// Version is a schema marker so a future fixture-format change is an
	// explicit, reviewable bump rather than a silent mismatch.
	Version int            `json:"version"`
	Probes  []ProbeRanking `json:"probes"`
}

// fixtureVersion is the current fixture schema version.
const fixtureVersion = 1

// ComputeFixture seeds the fixed corpus into an isolated database and records
// the full ranked output for every probe at default scoring. It is the single
// producer of both the committed golden and the live value the harness diffs,
// so "record" and "verify" can never drift in how they compute the snapshot.
func ComputeFixture(ctx context.Context) (Fixture, error) {
	db, err := openScratchDB(ctx)
	if err != nil {
		return Fixture{}, err
	}
	defer func() { _ = db.Close() }()

	slugByID, err := seedScratchCorpus(ctx, db)
	if err != nil {
		return Fixture{}, err
	}

	scoring := lore.DefaultScoring()
	probes := gridProbes()
	fx := Fixture{Version: fixtureVersion, Probes: make([]ProbeRanking, 0, len(probes))}
	for _, p := range probes {
		pr, err := rankProbe(ctx, db, p, scoring, slugByID)
		if err != nil {
			return Fixture{}, err
		}
		fx.Probes = append(fx.Probes, pr)
	}
	return fx, nil
}

// rankProbe runs one probe and records its full ranked output with rounded
// scores. It reuses the exact Appraise call the grid uses so the two harnesses
// observe the same pipeline.
func rankProbe(ctx context.Context, db *sql.DB, p probe, scoring lore.ScoringConfig, slugByID map[int64]string) (ProbeRanking, error) {
	out, err := lore.Appraise(ctx, db, lore.AppraiseParams{
		Query:       p.Query,
		Limit:       10,
		AllProjects: true,
		Scoring:     scoring,
		Now:         referenceNow,
	})
	if err != nil {
		return ProbeRanking{}, fmt.Errorf("eval: parity probe %q: appraise: %w", p.Name, err)
	}
	pr := ProbeRanking{Probe: p.Name, Query: p.Query, Rows: make([]RankedRow, 0, len(out.Results))}
	for i, r := range out.Results {
		pr.Rows = append(pr.Rows, RankedRow{
			Rank:  i + 1,
			Slug:  slugByID[r.Entry.ID],
			Score: roundScore(r.Score),
		})
	}
	return pr, nil
}

// MarshalFixture renders a fixture to the canonical committed form: indented
// JSON with a trailing newline, so the file diffs cleanly and a regeneration
// produces a reviewable patch.
func MarshalFixture(fx Fixture) ([]byte, error) {
	b, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("eval: marshal fixture: %w", err)
	}
	return append(b, '\n'), nil
}

// CompareFixtures returns a human-readable description of the FIRST drift
// between want (the golden) and got (the freshly computed snapshot), or "" when
// they are identical. Comparing field by field (rather than raw bytes) lets the
// message point at the exact probe/rank that moved, which is what a maintainer
// needs from a CI log.
func CompareFixtures(want, got Fixture) string {
	if want.Version != got.Version {
		return fmt.Sprintf("fixture version changed: golden=%d got=%d", want.Version, got.Version)
	}
	if len(want.Probes) != len(got.Probes) {
		return fmt.Sprintf("probe count changed: golden=%d got=%d", len(want.Probes), len(got.Probes))
	}
	for i := range want.Probes {
		w, g := want.Probes[i], got.Probes[i]
		if w.Probe != g.Probe {
			return fmt.Sprintf("probe[%d] name changed: golden=%q got=%q", i, w.Probe, g.Probe)
		}
		if len(w.Rows) != len(g.Rows) {
			return fmt.Sprintf("probe %q row count changed: golden=%d got=%d",
				w.Probe, len(w.Rows), len(g.Rows))
		}
		for j := range w.Rows {
			wr, gr := w.Rows[j], g.Rows[j]
			if wr.Slug != gr.Slug {
				return fmt.Sprintf("probe %q rank %d slug changed: golden=%q got=%q",
					w.Probe, wr.Rank, wr.Slug, gr.Slug)
			}
			if wr.Score != gr.Score {
				return fmt.Sprintf("probe %q rank %d (%s) score changed: golden=%g got=%g",
					w.Probe, wr.Rank, wr.Slug, wr.Score, gr.Score)
			}
		}
	}
	return ""
}

// roundScore rounds to scoreDecimals so the recorded value is stable across
// floating-point backends. Mirrors the pinned-constant posture score.go takes.
func roundScore(f float64) float64 {
	p := math.Pow(10, scoreDecimals)
	return math.Round(f*p) / p
}
