package quest

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/lore/embed"
)

// questWordRE extracts word tokens for the FTS query builder.
// Mirrors lore.wordRE (internal/lore/score.go) to avoid importing the
// unexported symbol. Both use the same \w+ pattern.
var questWordRE = regexp.MustCompile(`\w+`)

// questCoverageGate is the minimum vector coverage fraction required to
// enable the RRF vector arm. Matches lore.CoverageThreshold (ADR-003
// "Partial coverage and deterministic fallback").
const questCoverageGate = 0.90

// questFTSQuery converts a raw user query into an FTS5 MATCH expression
// for tasks_fts. Applies lore.BM25Stopwords filter and an OR-prefix
// scheme identical to lore.ftsQuery so agents learn one mental model.
// Returns "" when no usable tokens survive the filter.
func questFTSQuery(userQuery string) string {
	lower := strings.ToLower(userQuery)
	rawTokens := questWordRE.FindAllString(lower, -1)
	filtered := make([]string, 0, len(rawTokens))
	for _, t := range rawTokens {
		if _, stop := lore.BM25Stopwords[t]; !stop {
			filtered = append(filtered, t)
		}
	}
	tokens := filtered
	if len(tokens) == 0 {
		// All tokens were stopwords: fall back to raw tokens so a
		// purely-stopword query still produces a MATCH expression.
		tokens = rawTokens
	}
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if len(t) < 2 {
			continue
		}
		out = append(out, t+"*")
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, " OR ")
}

// QuestSearchResult is one hit from quest_search.
type QuestSearchResult struct {
	// QuestID is the canonical QUEST-N identifier.
	QuestID string `json:"quest_id"`
	// Subject is the one-line task summary from the [spec] note.
	Subject string `json:"subject"`
	// Status is the current task_status value (next/in_progress/blocked/done).
	Status string `json:"status"`
	// Epic is the campaign/epic tag if set on this quest.
	Epic string `json:"epic,omitempty"`
	// Score is the RRF fusion score or BM25 rank proxy.
	// Exposed for diagnostics; agents should use QuestID + Subject.
	Score float64 `json:"score,omitempty"`
}

// SearchInput is the typed input for quest_search / `guild quest search`.
type SearchInput struct {
	// Query is the natural-language search string. Required.
	Query string `json:"query" jsonschema:"natural-language search query"`
	// Limit caps the number of results returned. Defaults to 10 when 0.
	Limit int `json:"limit,omitempty" jsonschema:"max results (default 10)"`
	// Project is the project override. When empty the active project is used.
	Project string `json:"project,omitempty"`
}

// SearchOutput is the full response for quest_search.
type SearchOutput struct {
	// Results is the ranked list of matching quests, most relevant first.
	Results []QuestSearchResult `json:"results"`
	// Query echoes the original query string.
	Query string `json:"query"`
	// Arm describes the retrieval arm used: "bm25" or "rrf".
	Arm string `json:"arm"`
	// Coverage is the quest vector coverage fraction at query time.
	// 0.0 when the embedder is disabled or no vectors exist.
	Coverage float64 `json:"coverage,omitempty"`
}

const defaultSearchLimit = 10

// questRRFTopK is the per-arm over-fetch size for RRF fusion. Matches
// lore's RRFTopK (embed.RRFK = 60) for consistent ranking quality.
const questRRFTopK = embed.RRFK

// SearchCommand is the registry spec for quest_search (MCP) and
// `guild quest search <query>` (CLI).
//
// Pipeline:
//  1. Build FTS5 MATCH expression with BM25Stopwords filter.
//  2. Run BM25 top-questRRFTopK against tasks_fts (lexical arm).
//  3. If a QuestEmbedDeps is wired AND quest vector coverage >= 0.90:
//     a. CheckAndReload the in-process quest index.
//     b. Embed the query and quantize to int8.
//     c. Index.TopK(questRRFTopK) for the vector arm.
//     d. embed.Fuse(bm25Arm, vecArm, limit) at k=60.
//  4. Hydrate task fields for the fused entity IDs.
//  5. Return compact results (agents: focus on quest_id + subject).
//
// Coverage gate: < 0.90 falls back to BM25-only, matching
// lore_appraise's CoverageThreshold contract (ADR-003).
//
// Vector arm note: the quest-specific Index is wired via QuestEmbedDeps
// resolved from command.Deps.Embed at handler entry (QUEST-258). The
// MCP surface resolves a questEmbedProvider that lazily reconstructs
// a QuestCorpus Index when meta.quest.embedder_state is "enabled". The
// CLI surface passes nil (short-lived; no index warm cost).
var SearchCommand = &command.Command[SearchInput, SearchOutput]{
	Name:    "quest_search",
	CLIPath: []string{"quest", "search"},
	Short:   "search quests by keyword or semantic paraphrase",
	Long: "BM25+stopwords full-text search over quest subjects and spec notes. " +
		"When quest vector coverage >= 90%, adds a semantic arm and RRF-fuses " +
		"(k=60, same gate and fusion as lore_appraise). " +
		"Returns up to 10 results. Replaces quest list --all | grep.",
	Args: []command.ArgSpec{
		{
			Name:     "query",
			Kind:     command.ArgPositional,
			Type:     command.ArgString,
			Required: true,
			Variadic: true,
			Help:     "natural-language search query",
		},
		{Name: "limit", Short: "n", Kind: command.ArgFlag, Type: command.ArgInt, Help: "max results (default 10)"},
		{Name: "project", Short: "p", Kind: command.ArgFlag, Type: command.ArgString, Help: "project override"},
	},
	Handler: func(ctx context.Context, d command.Deps, in SearchInput) (SearchOutput, error) {
		query := strings.TrimSpace(in.Query)
		if query == "" {
			return SearchOutput{}, fmt.Errorf("quest search: empty query")
		}
		limit := in.Limit
		if limit <= 0 {
			limit = defaultSearchLimit
		}

		db, err := d.OpenDB(ctx)
		if err != nil {
			return SearchOutput{}, err
		}
		defer func() { _ = db.Close() }()

		pid, err := d.ResolveProj(ctx, in.Project)
		if err != nil {
			return SearchOutput{}, err
		}

		embedDeps := questEmbedFromDeps(ctx, d)
		return RunQuestSearchForProject(ctx, db, query, limit, pid, embedDeps)
	},
	CLIFormat: func(s command.CLISink, o SearchOutput) string { return formatSearch(s, o) },
	MCPFormat: func(s command.MCPSink, o SearchOutput) string { return formatSearch(s, o) },
}

// QuestEmbedDeps carries the optional quest-specific vector pipeline.
// Parallel to lore.EmbedDeps but bound to QuestCorpus and the
// quest_vectors / tasks_fts_rows tables. Nil means BM25-only (graceful
// Phase-0 fallback). Wired at MCP init via questEmbedProvider (QUEST-258).
type QuestEmbedDeps struct {
	// Embedder encodes query text into float32 vectors.
	Embedder embed.Embedder
	// Index is the per-process in-memory quest vector index.
	// nil on the CLI surface (short-lived; no index warm cost).
	Index *embed.Index
	// ModelID is the canonical model_id this index was built with.
	ModelID string
}

// Enabled reports whether the quest vector pipeline is fully wired.
func (d *QuestEmbedDeps) Enabled() bool {
	return d != nil && d.Embedder != nil && d.Index != nil && d.ModelID != ""
}

// RunQuestSearchForProject is the exported entry point for the quest
// search pipeline. embedDeps may be nil (BM25-only). Integration tests
// call this directly with a specific project ID.
func RunQuestSearchForProject(ctx context.Context, db *sql.DB, query string, limit int, projectID string, embedDeps *QuestEmbedDeps) (SearchOutput, error) {
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return SearchOutput{}, fmt.Errorf("quest search: empty query")
	}

	fts := questFTSQuery(query)

	// BM25 arm: fetch up to questRRFTopK entity IDs in BM25 rank order.
	bm25IDs, err := questBM25TopK(ctx, db, fts, questRRFTopK)
	if err != nil {
		return SearchOutput{}, fmt.Errorf("quest search: bm25: %w", err)
	}

	arm := "bm25"
	var coverage float64
	finalIDs := bm25IDs

	// Vector arm (optional). Requires a wired QuestEmbedDeps and
	// sufficient coverage.
	if embedDeps.Enabled() {
		cov, vecIDs, vecErr := questVectorTopK(ctx, db, embedDeps, query, questRRFTopK)
		coverage = cov
		if vecErr == nil && cov >= questCoverageGate && len(vecIDs) > 0 {
			bm25Ranked := make(embed.Ranked, len(bm25IDs))
			for i, id := range bm25IDs {
				bm25Ranked[i] = id
			}
			vecRanked := make(embed.Ranked, len(vecIDs))
			for i, id := range vecIDs {
				vecRanked[i] = id
			}
			finalIDs = []int64(embed.Fuse(bm25Ranked, vecRanked, limit))
			arm = "rrf"
		}
	}

	results, err := hydrateQuestResults(ctx, db, finalIDs, limit, projectID)
	if err != nil {
		return SearchOutput{}, fmt.Errorf("quest search: hydrate: %w", err)
	}

	return SearchOutput{
		Results:  results,
		Query:    query,
		Arm:      arm,
		Coverage: coverage,
	}, nil
}

// questBM25TopK runs an FTS5 BM25 query against tasks_fts and returns
// up to k entity IDs (tasks_fts_rows.id integers) in BM25 rank order.
// Returns an empty slice (not an error) when fts is "" or no rows match.
func questBM25TopK(ctx context.Context, db *sql.DB, fts string, k int) ([]int64, error) {
	if fts == "" {
		return nil, nil
	}
	// tasks_fts.rowid == tasks_fts_rows.id (the integer bridge PK).
	// ORDER BY rank sorts by BM25 score ascending (more negative = better).
	rows, err := db.QueryContext(ctx, //nolint:sqlcheck // fts user query flows through ? bind; table name is a literal
		`SELECT tasks_fts.rowid
		 FROM tasks_fts
		 WHERE tasks_fts MATCH ?
		 ORDER BY tasks_fts.rank
		 LIMIT ?`,
		fts, k,
	)
	if err != nil {
		return nil, fmt.Errorf("quest bm25: fts query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("quest bm25: scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// questVectorTopK runs the vector arm of quest search. Returns
// (coverage, rankedEntityIDs, error). On embedder failure callers
// stay on the BM25 arm.
//
//nolint:gocritic // unnamedResult: the three return positions are named in the comment above
func questVectorTopK(ctx context.Context, db *sql.DB, deps *QuestEmbedDeps, query string, k int) (float64, []int64, error) {
	// Read quest coverage from meta.
	var covNum, covDen int64
	if scanErr := db.QueryRowContext(ctx,
		`SELECT COALESCE(CAST(value AS INTEGER), 0) FROM meta WHERE key = 'quest.vector_coverage_num'`,
	).Scan(&covNum); scanErr != nil {
		covNum = 0
	}
	if scanErr := db.QueryRowContext(ctx,
		`SELECT COALESCE(CAST(value AS INTEGER), 0) FROM meta WHERE key = 'quest.vector_coverage_den'`,
	).Scan(&covDen); scanErr != nil {
		covDen = 0
	}
	var cov float64
	if covDen > 0 {
		cov = float64(covNum) / float64(covDen)
	}

	if cov < questCoverageGate {
		return cov, nil, nil
	}

	// CheckAndReload so cross-process vector writes are picked up.
	if _, reloadErr := deps.Index.CheckAndReload(ctx, db); reloadErr != nil {
		return cov, nil, fmt.Errorf("quest vector: reload: %w", reloadErr)
	}

	// Embed the query.
	fvec, embedErr := deps.Embedder.Embed(ctx, query)
	if embedErr != nil {
		return cov, nil, fmt.Errorf("quest vector: embed: %w", embedErr)
	}
	qvec := embed.Quantize(fvec)
	if qvec == nil {
		return cov, nil, fmt.Errorf("quest vector: quantize nil")
	}

	// TopK from the in-process index.
	hits, topkErr := deps.Index.TopK(qvec, k)
	if topkErr != nil {
		return cov, nil, fmt.Errorf("quest vector: topk: %w", topkErr)
	}
	out := make([]int64, len(hits))
	for i, h := range hits {
		out[i] = h.EntryID
	}
	return cov, out, nil
}

// hydrateQuestResults resolves tasks_fts_rows integer IDs back to
// task_ids and loads subject + status + epic via event-sourced Load.
// Returns up to limit results in the same order as ids.
func hydrateQuestResults(ctx context.Context, db *sql.DB, ids []int64, limit int, projectID string) ([]QuestSearchResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	// Resolve integer bridge IDs to task_id strings.
	idToTaskID := make(map[int64]string, len(ids))
	for _, id := range ids {
		var taskID string
		err := db.QueryRowContext(ctx,
			`SELECT task_id FROM tasks_fts_rows WHERE id = ?`, id,
		).Scan(&taskID)
		if err != nil {
			continue // skip missing (deleted mid-search)
		}
		idToTaskID[id] = taskID
	}

	type ranked struct {
		rank   int
		result QuestSearchResult
	}
	seen := make(map[string]bool, len(ids))
	ordered := make([]ranked, 0, len(ids))
	for rank, id := range ids {
		taskID, ok := idToTaskID[id]
		if !ok || seen[taskID] {
			continue
		}
		seen[taskID] = true

		// Load uses event-sourced spec replay to get subject + epic.
		q, err := Load(ctx, db, projectID, taskID)
		if err != nil {
			continue // quest deleted mid-search; skip gracefully
		}
		ordered = append(ordered, ranked{
			rank: rank,
			result: QuestSearchResult{
				QuestID: q.ID,
				Subject: q.Subject,
				Status:  string(q.Status),
				Epic:    q.Epic,
				Score:   1.0 / float64(embed.RRFK+rank+1),
			},
		})
	}

	// Re-sort by original rank order (may have gaps from skipped IDs).
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].rank < ordered[j].rank
	})

	out := make([]QuestSearchResult, 0, len(ordered))
	for _, r := range ordered {
		out = append(out, r.result)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// questEmbedResolver is the lazy-reconstruct interface the MCP adapter
// implements. Parallel to lore's embedResolver (internal/lore/embed_deps.go).
// Declared locally so the quest package does not import internal/mcp.
type questEmbedResolver interface {
	ResolveQuestEmbedDeps(ctx context.Context) *QuestEmbedDeps
}

// questEmbedFromDeps extracts *QuestEmbedDeps from command.Deps.Embed.
// Two shapes are supported:
//
//   - *QuestEmbedDeps: returned unchanged (CLI surface or direct test injection).
//   - questEmbedResolver: calls ResolveQuestEmbedDeps(ctx) for the lazy MCP path.
//
// Unknown types (including the lore *EmbedDeps stored there) produce nil so
// an accidental bad assignment falls back to BM25 instead of panicking.
func questEmbedFromDeps(ctx context.Context, d command.Deps) *QuestEmbedDeps {
	if d.Embed == nil {
		return nil
	}
	switch v := d.Embed.(type) {
	case *QuestEmbedDeps:
		return v
	case questEmbedResolver:
		return v.ResolveQuestEmbedDeps(ctx)
	default:
		return nil
	}
}

// formatSearch renders SearchOutput for both CLI and MCP sinks.
// Kept compact: typical 10-result response is under 200 tokens.
func formatSearch(s lineListSink, o SearchOutput) string {
	var b strings.Builder
	arm := o.Arm
	if arm == "" {
		arm = "bm25"
	}
	b.WriteString(s.Line("🔍", "[quest-search]",
		fmt.Sprintf("query=%q arm=%s results=%d", o.Query, arm, len(o.Results))))
	for _, r := range o.Results {
		status := r.Status
		if status == "" {
			status = "?"
		}
		line := fmt.Sprintf("%s [%s] %s", r.QuestID, status, r.Subject)
		if r.Epic != "" {
			line += fmt.Sprintf(" (epic: %s)", r.Epic)
		}
		b.WriteString("  " + line + "\n")
	}
	if len(o.Results) == 0 {
		b.WriteString("  (no results)\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
