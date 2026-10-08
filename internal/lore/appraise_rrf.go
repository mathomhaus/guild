package lore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/mathomhaus/guild/internal/lore/embed"
)

// Scoped and global retrieval share one policy. Coverage comes from actual
// compatible, fresh vectors, never lifecycle counters. Partial indexes are
// useful: missing vectors remain eligible through the independent lexical arm.
func appraiseRRF(ctx context.Context, db *sql.DB, params *AppraiseParams, now time.Time, scoring ScoringConfig, limit int) (*AppraiseOutput, bool, error) {
	if params.Embed.Index == nil {
		return nil, false, nil
	}
	// A previously wired runtime must still respect an operator disable.
	var state string
	if err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='embedder_state'`).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("lore: appraise: embedder state: %w", err)
	}
	if state != "enabled" {
		return nil, false, nil
	}
	allowed, exact, err := eligibleEntryIDs(ctx, db, params, now)
	if err != nil {
		return nil, false, err
	}
	coverage, err := embed.ReadCoverage(ctx, db, embed.LoreCorpus{}, params.Embed.ModelID, func(id int64) bool { return allowed[id] })
	if err != nil {
		return nil, false, err
	}
	if coverage.Fresh == 0 {
		return nil, false, nil
	}
	logger := params.Embed.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if _, err := params.Embed.Index.CheckAndReload(ctx, db); err != nil {
		logger.Warn("lore: appraise: index reload failed; falling back", "err", err)
		return nil, false, nil
	}
	fvec, err := params.Embed.Embedder.Embed(ctx, params.Query)
	if err != nil {
		logger.Warn("lore: appraise: query embed failed; falling back", "err", err)
		return nil, false, nil
	}
	if !validQueryVector(fvec) {
		return nil, false, nil
	}
	qvec := embed.Quantize(fvec)
	if qvec == nil {
		return nil, false, nil
	}
	scored, err := params.Embed.Index.TopKFiltered(qvec, RRFTopK, func(id int64) bool { return coverage.FreshIDs[id] })
	if err != nil {
		logger.Warn("lore: appraise: filtered vector search failed; falling back", "err", err)
		return nil, false, nil
	}
	if len(scored) == 0 {
		return nil, false, nil
	}
	lexical, _, err := lexicalCandidates(ctx, db, params, now, scoring, RRFTopK, exact)
	if err != nil {
		return nil, false, err
	}
	a, b := make(embed.Ranked, 0, len(lexical)), make(embed.Ranked, 0, len(scored))
	lexicalByID := make(map[int64]AppraiseResult, len(lexical))
	for _, r := range lexical {
		a = append(a, r.Entry.ID)
		lexicalByID[r.Entry.ID] = r
	}
	vectorByID := make(map[int64]int32, len(scored))
	for _, r := range scored {
		b = append(b, r.EntryID)
		vectorByID[r.EntryID] = r.Score
	}
	// Fuse the complete candidate union before hydration/title ordering. An
	// exact lexical match must not disappear behind a premature fused limit.
	fused := embed.FuseBestRank(a, b, 0)
	results, err := hydrateRankedEntries(ctx, db, params, now, fused)
	if err != nil {
		return nil, false, err
	}
	scores := embed.BestRankScores(a, b)
	for i := range results {
		r := &results[i]
		r.Score = scores[r.Entry.ID]
		if lexical, ok := lexicalByID[r.Entry.ID]; ok {
			r.BM25 = lexical.BM25
			r.LexicalMatch = true
		}
		if dot, ok := vectorByID[r.Entry.ID]; ok {
			r.SemanticMatch = true
			r.VectorSimilarity = float64(dot) / (127 * 127)
		}
	}
	sortAppraiseResults(results, params.Query, scoring)
	if len(results) > limit {
		results = results[:limit]
	}
	out := &AppraiseOutput{Results: results, RetrievalMode: "hybrid", Answerability: AnswerabilityUnknown}
	if len(results) == 0 {
		out.Answerability = AnswerabilityNoMatch
		out.MissHint = slugHint(params.Query)
	}
	populateProjectCounts(out, params.AllProjects)
	_ = bumpAccessCounters(ctx, db, now, results)
	return out, true, nil
}

func validQueryVector(vec []float32) bool {
	if len(vec) != embed.Dim {
		return false
	}
	var norm float64
	for _, value := range vec {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false
		}
		norm += float64(value) * float64(value)
	}
	return norm > 0
}

// eligibleEntryIDs applies every constraint before either bounded vector
// search or exact-title selection. Query normalization matches TitleBoost.
func eligibleEntryIDs(ctx context.Context, db *sql.DB, params *AppraiseParams, now time.Time) (allowed map[int64]bool, exact []int64, err error) {
	where, args := buildWhereClause(params, now)
	//nolint:gosec // fixed columns and whitelist-built predicates; bound values
	rows, err := db.QueryContext(ctx, `SELECT e.id, e.title FROM entries e WHERE 1=1`+where+` ORDER BY e.id`, args...) //sqlcheck:ignore // static template with whitelisted predicates
	if err != nil {
		return nil, nil, fmt.Errorf("lore: appraise: eligible entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	allowed = map[int64]bool{}
	queryNorm := normalizeQuery(params.Query)
	for rows.Next() {
		var id int64
		var title string
		if err := rows.Scan(&id, &title); err != nil {
			return nil, nil, err
		}
		allowed[id] = true
		if normalizeQuery(title) == queryNorm {
			exact = append(exact, id)
		}
	}
	return allowed, exact, rows.Err()
}

func lexicalCandidates(ctx context.Context, db *sql.DB, params *AppraiseParams, now time.Time, scoring ScoringConfig, n int, exact []int64) ([]AppraiseResult, bool, error) {
	entries, bm25s, err := runFTSQuery(ctx, db, params.Query, params, now, n)
	if err != nil {
		return nil, false, err
	}
	fellBack := false
	if len(entries) == 0 {
		entries, bm25s, err = runLIKEFallback(ctx, db, params.Query, params, now, n)
		if err != nil {
			return nil, false, err
		}
		fellBack = true
	}
	results := make([]AppraiseResult, 0, len(entries)+len(exact))
	seen := map[int64]bool{}
	query := prepareTitleQuery(params.Query)
	for i, e := range entries {
		results = append(results, AppraiseResult{Entry: e, Score: CombineScore(bm25s[i], daysBetween(e.CreatedAt, now), scoring) + query.boost(e.Title, scoring), BM25: bm25s[i], LexicalMatch: true})
		seen[e.ID] = true
	}
	exactRows, err := hydrateRankedEntries(ctx, db, params, now, exact)
	if err != nil {
		return nil, false, err
	}
	for _, r := range exactRows {
		if !seen[r.Entry.ID] {
			r.Score = CombineScore(0, daysBetween(r.Entry.CreatedAt, now), scoring) + query.boost(r.Entry.Title, scoring)
			r.LexicalMatch = true
			results = append(results, r)
		}
	}
	sortAppraiseResults(results, params.Query, scoring)
	if len(results) > n {
		results = results[:n]
	}
	return results, fellBack, nil
}

func sortAppraiseResults(results []AppraiseResult, query string, scoring ScoringConfig) {
	var exact map[*Entry]bool
	if scoring.TitleMatchBoost > 0 {
		exact = make(map[*Entry]bool, len(results))
		queryNorm := normalizeQuery(query)
		for _, result := range results {
			exact[result.Entry] = normalizeQuery(result.Entry.Title) == queryNorm
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		if scoring.TitleMatchBoost > 0 {
			ie, je := exact[results[i].Entry], exact[results[j].Entry]
			if ie != je {
				return ie
			}
		}
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Entry.ID < results[j].Entry.ID
	})
}

func populateProjectCounts(out *AppraiseOutput, all bool) {
	if !all {
		return
	}
	out.ProjectCounts = map[string]int{}
	for _, r := range out.Results {
		out.ProjectCounts[r.Entry.ProjectID]++
	}
}

// Hydration retains filters as a second safety check if rows change while
// retrieval runs. Candidate selection, rather than this check, prevents
// excluded rows from consuming a limited arm's slots. Callers supply the
// score; computing a placeholder here would immediately be discarded.
func hydrateRankedEntries(ctx context.Context, db *sql.DB, params *AppraiseParams, now time.Time, ids []int64) ([]AppraiseResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	where, filters := buildWhereClause(params, now)
	args = append(args, filters...)
	//nolint:gosec // fixed projection, generated placeholders and whitelisted predicates
	rows, err := db.QueryContext(ctx, `SELECT `+entryColumns+` FROM entries e WHERE e.id IN (`+strings.Join(placeholders, ",")+`)`+where, args...) //sqlcheck:ignore // constant template with placeholders
	if err != nil {
		return nil, fmt.Errorf("lore: appraise: hydrate: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byID := map[int64]*Entry{}
	for rows.Next() {
		e := &Entry{}
		if err := scanEntry(rows, e); err != nil {
			return nil, err
		}
		byID[e.ID] = e
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	results := make([]AppraiseResult, 0, len(ids))
	for _, id := range ids {
		if e, ok := byID[id]; ok {
			results = append(results, AppraiseResult{Entry: e})
		}
	}
	return results, nil
}
