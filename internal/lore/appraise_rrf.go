package lore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/mathomhaus/guild/internal/lore/embed"
)

// appraiseRRF is the single-project RRF-fused retrieval path. It is
// called only from Appraise, only when params.Embed is Enabled and
// params.AllProjects is false. Returns handled=false when the coverage
// gate falls below CoverageThreshold (caller falls through to the
// classic BM25+stopwords path) or when the embedder returns an error
// (same fallback). handled=true means the returned *AppraiseOutput is
// the final result and the caller should NOT run the legacy pipeline.
//
// Pipeline per ADR-003 "Dataflow: MCP surface":
//
//  1. Read embedder_state and coverage = num/den from meta. If state
//     != 'enabled' or coverage < CoverageThreshold, return handled=false.
//  2. CheckAndReload the in-process index so any cross-process vector
//     writes we have not seen get picked up.
//  3. Encode the query once via Embedder.Embed and Quantize to int8.
//  4. Run BM25+stopwords top-RRFTopK as the lexical arm.
//  5. Run Index.TopK(RRFTopK) as the vector arm.
//  6. Fuse both arms at k=RRFK; truncate to limit.
//  7. Load full Entry rows for the fused ids (respecting the same
//     filters the BM25 path applies: status, since, project).
//  8. Score each result via the existing Score() function so the
//     AppraiseResult Score field has a comparable magnitude to the
//     Phase 0 output (callers and CLI rendering do not change).
//
// The function never falls through on a rank-path SQL error: any
// unexpected error propagates with %w wrapping. The "graceful
// fallback on gate miss" is deliberately narrow (two specific
// conditions).
func appraiseRRF(
	ctx context.Context,
	db *sql.DB,
	params *AppraiseParams,
	now time.Time,
	scoring ScoringConfig,
	limit int,
) (*AppraiseOutput, bool, error) {
	state, cov, err := readCoverage(ctx, db)
	if err != nil {
		return nil, false, err
	}
	if state != "enabled" || cov < CoverageThreshold {
		return nil, false, nil
	}

	logger := params.Embed.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Keep the index fresh. CheckAndReload is a single indexed
	// integer read on the common path; the reload path runs only
	// when some other process bumped the epoch since our last load.
	if params.Embed.Index != nil {
		if _, err := params.Embed.Index.CheckAndReload(ctx, db); err != nil {
			// A reload failure is not fatal: log and fall back to
			// BM25-only. The alternative (hard-erroring) would make
			// a momentary SQLite hiccup propagate to every MCP
			// query, which is worse for UX than a one-query-deep
			// ranking regression.
			logger.Warn("lore: appraise: index CheckAndReload failed; falling back",
				"err", err,
			)
			return nil, false, nil
		}
	}

	// Encode the query. One embed call per Appraise; no caching.
	fvec, err := params.Embed.Embedder.Embed(ctx, params.Query)
	if err != nil {
		// Same principle as the reload failure path: graceful
		// fallback to BM25 rather than propagate to the caller.
		logger.Warn("lore: appraise: query embed failed; falling back",
			"err", err,
		)
		return nil, false, nil
	}
	qvec := embed.Quantize(fvec)
	if qvec == nil {
		logger.Warn("lore: appraise: quantize returned nil; falling back",
			"float_dim", len(fvec),
		)
		return nil, false, nil
	}

	// BM25 arm: top-RRFTopK ids only. We need the ordinal list for
	// RRF; full entry hydration happens after fusion.
	bm25Ids, err := runBM25TopN(ctx, db, params, now, RRFTopK)
	if err != nil {
		return nil, false, fmt.Errorf("lore: appraise: bm25 arm: %w", err)
	}

	// Vector arm: gate on Index availability.
	var vecIds embed.Ranked
	if params.Embed.Index != nil {
		scored, topErr := params.Embed.Index.TopK(qvec, RRFTopK)
		if topErr != nil {
			// Index unloaded or wrong shape. Fall back to BM25-only
			// rather than surface.
			logger.Warn("lore: appraise: Index.TopK failed; falling back",
				"err", topErr,
			)
			return nil, false, nil
		}
		vecIds = make(embed.Ranked, 0, len(scored))
		for _, s := range scored {
			vecIds = append(vecIds, s.EntryID)
		}
	}

	fused := embed.Fuse(embed.Ranked(bm25Ids), vecIds, limit*appraiseOverfetch)
	if len(fused) == 0 {
		// No hits from either arm. Return an empty, non-nil output
		// so callers can distinguish from the "nothing was searched"
		// legacy path.
		return &AppraiseOutput{Results: []AppraiseResult{}}, true, nil
	}

	results, err := hydrateRankedEntries(ctx, db, params, now, scoring, fused)
	if err != nil {
		return nil, false, err
	}
	if len(results) > limit {
		results = results[:limit]
	}

	out := &AppraiseOutput{Results: results}
	if params.AllProjects {
		counts := map[string]int{}
		for _, r := range results {
			counts[r.Entry.ProjectID]++
		}
		out.ProjectCounts = counts
	}

	// Keep the historical side effect: appraise bumps access
	// counters so dossier and echoes can spot hot entries. Best
	// effort; a failure here is telemetry class.
	_ = bumpAccessCounters(ctx, db, now, results)
	return out, true, nil
}

// appraiseCrossProject is the cross-project RRF path. It runs a global
// BM25 query across all projects and a global vector TopK, then fuses
// those two arms via Reciprocal Rank Fusion. The result is the true
// globally top-N entries: if one project genuinely dominates for the
// query, all N slots may come from that project.
//
// The previous design ran one per-project BM25+vector pair and then
// merged the per-project lists via FuseMany. That emergent architecture
// enforced roughly one result per project in the final top-N because
// every project's rank-1 entry received an equal RRF score regardless of
// how many matching entries that project had. The observation in LORE-374
// / LORE-371 (guild-shaped query returned zero guild entries across
// five spike projects) was the symptom.
//
// handled=false is returned only when the embedder is flat-out disabled
// at the meta level; any other branch runs the global fusion and returns
// handled=true.
func appraiseCrossProject(
	ctx context.Context,
	db *sql.DB,
	params *AppraiseParams,
	now time.Time,
	scoring ScoringConfig,
	limit int,
) (*AppraiseOutput, bool, error) {
	state, _, err := readCoverage(ctx, db)
	if err != nil {
		return nil, false, err
	}
	if state != "enabled" {
		return nil, false, nil
	}

	logger := params.Embed.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Keep the index fresh. A failure here is not fatal: log and fall
	// back to the BM25-only path, matching the single-project arm's
	// policy. QUEST-219: without this, cross-process vector writes from
	// `guild init` or an embed-rebuild are invisible until server restart.
	if params.Embed.Index != nil {
		if _, err := params.Embed.Index.CheckAndReload(ctx, db); err != nil {
			logger.Warn("lore: appraise (all_projects): index CheckAndReload failed; falling back",
				"err", err,
			)
			return nil, false, nil
		}
	}

	// Encode once for the global vector arm.
	fvec, err := params.Embed.Embedder.Embed(ctx, params.Query)
	if err != nil {
		logger.Warn("lore: appraise (all_projects): query embed failed; falling back",
			"err", err,
		)
		return nil, false, nil
	}
	qvec := embed.Quantize(fvec)
	if qvec == nil {
		return nil, false, nil
	}

	// Global BM25 arm: params.AllProjects=true causes buildWhereClause
	// to omit the project_id filter, so this returns the globally
	// ranked BM25 top-K across every project.
	bm25Ids, err := runBM25TopN(ctx, db, params, now, RRFTopK)
	if err != nil {
		return nil, false, fmt.Errorf("lore: appraise (all_projects): bm25 arm: %w", err)
	}

	// Global vector arm. The shared index holds vectors from the active
	// project; cross-project indexing lands in a later quest. Use it
	// without project-scoping: lore_entries.id is globally unique in
	// this DB so there are no false-positive cross-project id collisions.
	var vecIds embed.Ranked
	if params.Embed.Index != nil {
		scored, topErr := params.Embed.Index.TopK(qvec, RRFTopK)
		if topErr == nil {
			vecIds = make(embed.Ranked, 0, len(scored))
			for _, s := range scored {
				vecIds = append(vecIds, s.EntryID)
			}
		}
	}

	fused := embed.Fuse(embed.Ranked(bm25Ids), vecIds, limit*appraiseOverfetch)
	if len(fused) == 0 {
		return &AppraiseOutput{Results: []AppraiseResult{}}, true, nil
	}

	results, err := hydrateRankedEntries(ctx, db, params, now, scoring, fused)
	if err != nil {
		return nil, false, err
	}
	if len(results) > limit {
		results = results[:limit]
	}

	out := &AppraiseOutput{Results: results}
	counts := map[string]int{}
	for _, r := range results {
		counts[r.Entry.ProjectID]++
	}
	out.ProjectCounts = counts

	_ = bumpAccessCounters(ctx, db, now, results)
	return out, true, nil
}

// readCoverage returns (embedder_state, coverage_ratio, err). A zero
// denominator yields coverage=1.0 (the "no active entries" state is
// treated as "nothing to embed, so coverage is fully satisfied"). This
// avoids a divide-by-zero that would otherwise mask a freshly
// initialized DB as "below threshold" and hide it behind BM25 for no
// good reason.
func readCoverage(ctx context.Context, db *sql.DB) (state string, coverage float64, err error) {
	scanErr := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'embedder_state'`,
	).Scan(&state)
	switch {
	case errors.Is(scanErr, sql.ErrNoRows):
		state = "disabled"
	case scanErr != nil:
		return "", 0, fmt.Errorf("lore: appraise: read embedder_state: %w", scanErr)
	}

	num, err := readMetaInt(ctx, db, "vector_coverage_num")
	if err != nil {
		return "", 0, err
	}
	den, err := readMetaInt(ctx, db, "vector_coverage_den")
	if err != nil {
		return "", 0, err
	}
	if den <= 0 {
		return state, 1.0, nil
	}
	return state, float64(num) / float64(den), nil
}

// readMetaInt reads a meta row and parses its decimal value. Missing
// rows yield zero (every caller today tolerates that).
func readMetaInt(ctx context.Context, db *sql.DB, key string) (int64, error) {
	var s string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, key,
	).Scan(&s)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("lore: appraise: read meta %q: %w", key, err)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("lore: appraise: parse meta %q=%q: %w", key, s, err)
	}
	return n, nil
}

// runBM25TopN is the ordinal-only BM25 ranker used as the lexical arm
// of the RRF fusion. Produces up to n ids in FTS5 rank order. Respects
// the same status/project/since filters the legacy Appraise path
// applies via buildWhereClause.
//
// Deliberately does NOT use the re-rank/recency/title-boost pipeline
// the classic path runs: RRF operates on ordinals only, so richer
// scoring on the BM25 arm would be double-counted. Title boost and
// recency still apply on the hydration pass (Score() is called per
// result after fusion) so cross-arm ties break consistently.
func runBM25TopN(ctx context.Context, db *sql.DB, params *AppraiseParams, now time.Time, n int) ([]int64, error) {
	fts := ftsQuery(params.Query)
	if fts == "" {
		return nil, nil
	}
	where, args := buildWhereClause(params, now)
	//nolint:gosec // G202: entryColumns + where are constants; no user input reaches the SQL text
	sqlText := `SELECT e.id
		FROM entries_fts
		JOIN entries e ON e.id = entries_fts.rowid
		WHERE entries_fts MATCH ?` + where + `
		ORDER BY entries_fts.rank
		LIMIT ?`
	all := append([]any{fts}, args...)
	all = append(all, n)

	rows, err := db.QueryContext(ctx, sqlText, all...) //sqlcheck:ignore // sqlText is a constant template; buildWhereClause concatenates only hard-coded fragments
	if err != nil {
		return nil, fmt.Errorf("lore: appraise bm25 topN: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("lore: appraise bm25 topN scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// hydrateRankedEntries loads full Entry rows for a ranked id list,
// preserves the input order in the output, and computes a Score()
// per result. Applies the same status filter the legacy BM25 path
// applies (honoring IncludeAll) so sealed/superseded entries never
// leak into the final Results slice even if they snuck into the
// vector or BM25 top-K.
func hydrateRankedEntries(
	ctx context.Context,
	db *sql.DB,
	params *AppraiseParams,
	now time.Time,
	scoring ScoringConfig,
	ids []int64,
) ([]AppraiseResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	// Build placeholder list and args.
	placeholders := make([]string, len(ids))
	idArgs := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		idArgs[i] = id
	}

	// Apply the same filtering the legacy path uses so the fusion
	// output is never a superset of what BM25 would have returned.
	var filterParts []string
	var filterArgs []any
	if !params.IncludeAll {
		filterParts = append(filterParts, "e.status IN ('current','seed','exploring','imported')")
	}
	if !params.AllProjects && strings.TrimSpace(params.Project) != "" {
		filterParts = append(filterParts, "e.project_id = ?")
		filterArgs = append(filterArgs, params.Project)
	}
	if params.Since > 0 {
		filterParts = append(filterParts, "e.created_at >= datetime(?, 'utc')")
		cutoff := now.Add(-params.Since).Format(time.RFC3339)
		filterArgs = append(filterArgs, cutoff)
	}
	filterClause := ""
	if len(filterParts) > 0 {
		filterClause = " AND " + strings.Join(filterParts, " AND ")
	}

	//nolint:gosec // G202: entryColumns + static IN (?) placeholders; no user input reaches SQL
	sqlText := `SELECT ` + entryColumns + `
		FROM entries e
		WHERE e.id IN (` + strings.Join(placeholders, ",") + `)` + filterClause

	args := make([]any, 0, len(idArgs)+len(filterArgs))
	args = append(args, idArgs...)
	args = append(args, filterArgs...)
	rows, err := db.QueryContext(ctx, sqlText, args...) //sqlcheck:ignore // sqlText is a constant template plus ? placeholders
	if err != nil {
		return nil, fmt.Errorf("lore: appraise: hydrate: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byID := make(map[int64]*Entry, len(ids))
	for rows.Next() {
		e := &Entry{}
		if err := scanEntry(rows, e); err != nil {
			return nil, err
		}
		byID[e.ID] = e
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lore: appraise: hydrate iterate: %w", err)
	}

	results := make([]AppraiseResult, 0, len(ids))
	for _, id := range ids {
		e, ok := byID[id]
		if !ok {
			// Filtered out by status/project/since; skip silently.
			continue
		}
		// RRF does not produce a bm25 score per result. Use a
		// stand-in score derived from the rank position so the
		// Score field stays comparable to the legacy output. The
		// caller and CLI renderer do not depend on exact
		// magnitudes beyond "higher is better."
		base := 1.0 / float64(embed.RRFK+len(results)+1)
		score := base + TitleBoost(e.Title, params.Query, scoring)
		results = append(results, AppraiseResult{
			Entry: e,
			Score: score,
			// BM25 stays zero: the RRF branch does not surface a
			// raw BM25 number per result. Callers that need the
			// legacy BM25 column can disable the embedder.
			BM25: 0,
		})
	}
	return results, nil
}
