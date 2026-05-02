package lore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mathomhaus/guild/internal/lore/embed"
)

// AppraiseParams bundles the inputs to Appraise. All fields are optional
// except Query; zero-valued fields fall back to spec defaults.
//
// The struct exists so CLI callers (per-query knobs) and the MCP
// wrapper can both provide the same surface without argument churn.
type AppraiseParams struct {
	// Query is the user's raw search string. Required.
	Query string

	// Limit caps the number of returned results. Defaults to 10 when 0.
	// Appraise over-fetches 3× before re-ranking — LIMIT 30 → re-rank
	// → cut to 10.
	Limit int

	// AllProjects makes the search cross-project (recommended for
	// research queries). When false, Project must be non-empty.
	AllProjects bool

	// Project, when AllProjects is false, scopes the search to a
	// single project id.
	Project string

	// Since, when non-zero, filters to entries created within the
	// window. CLI parses "7d|2w|1m" into a time.Duration.
	Since time.Duration

	// IncludeAll widens the status filter from the default
	// ("current"/"seed"/"exploring"/"imported") to every status.
	IncludeAll bool

	// Scoring is the (per-query-overridable) ranking config. Zero
	// value is treated as DefaultScoring() for MCP callers who don't
	// pass knobs.
	Scoring ScoringConfig

	// Now is the reference "now" for recency decay. Tests inject a
	// fixed timestamp; production callers pass time.Now().UTC().
	Now time.Time

	// Embed is the optional embeddings pipeline. When nil or not
	// Enabled, Appraise is identical to the Phase 0 BM25+stopwords
	// path and never constructs a vector arm. Enabled + coverage >=
	// CoverageThreshold triggers the RRF fusion branch per ADR-003
	// "Partial coverage and deterministic fallback".
	Embed *EmbedDeps

	// ProjectIDs is reserved for future use. The cross-project RRF path
	// now runs a single global BM25+vector query rather than a per-project
	// fan-out, so this field is no longer read. Kept for API compatibility.
	ProjectIDs []string
}

// AppraiseResult is one ranked row with the score exposed for
// diagnostics and for the CLI's "linked-entries footer" logic.
type AppraiseResult struct {
	Entry *Entry
	Score float64
	// BM25 is the raw FTS5 bm25() value (more-negative = better).
	// Zero when the result came from the LIKE fallback branch.
	BM25 float64
}

// AppraiseOutput is the full response shape, not just the results
// slice, so callers have one place to find the miss-hint and the
// "fell back to LIKE" signal without re-inspecting the params.
type AppraiseOutput struct {
	Results []AppraiseResult
	// MissHint is a non-empty human-readable string when the query
	// matched a slug-like shape AND returned zero results.
	MissHint string
	// FellBackToLIKE is true when FTS5 returned 0 rows and the LIKE
	// fallback branch produced the displayed results.
	FellBackToLIKE bool
	// ProjectCounts is populated only when AllProjects=true. Keys
	// are project ids; values are row counts in the final ranked
	// set. CLI uses this for the "N entries across M projects" line.
	ProjectCounts map[string]int
}

// ErrEmptyQuery is returned when AppraiseParams.Query trims to "".
var ErrEmptyQuery = errors.New("lore: appraise: empty query")

const defaultAppraiseLimit = 10
const appraiseOverfetch = 3

// CoverageThreshold is the floor below which Appraise refuses to
// construct the vector arm and serves BM25+stopwords deterministically.
// Named constant per the "no magic numbers" bar; the value 0.90 comes
// from ADR-003 "Partial coverage and deterministic fallback" and was
// chosen to tolerate ~10% of a freshly upgraded corpus missing vectors
// during init-backfill without exposing the user to mixed-mode ranking.
const CoverageThreshold = 0.90

// RRFTopK is the size of the per-arm top-K slice both the BM25 ranker
// and the vector arm produce before Reciprocal Rank Fusion. Mirrors
// the k=60 fusion constant on the algorithm side; the ranker also
// produces exactly 60 candidates so every item in both lists has a
// finite rank contribution.
const RRFTopK = embed.RRFK

// ftsQuery converts a raw user query into a safe FTS5 expression.
// Strips common English stopwords (see stopwords.go) before tokenizing,
// then strips every non-word char (FTS5 reserves `-`, `"`, `*`, etc.),
// drops tokens shorter than 2 characters, and returns an OR-of-prefixes.
// Stopword stripping is applied before the length check so that a query
// composed entirely of stopwords falls through to the original tokens
// (via stripStopwords's no-op fallback), preserving exact-technical
// query behavior.
// Returns "" when no usable tokens survive, signalling the caller to
// skip FTS entirely.
func ftsQuery(userQuery string) string {
	filtered := stripStopwords(userQuery)
	tokens := wordRE.FindAllString(strings.ToLower(filtered), -1)
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

// slugRE matches hyphenated-lowercase strings (e.g. "cross-project-dedup").
// Paired with questRE, the two cover the "agent typed a slug into lore
// appraise" confusion mode — the caller gets a hint pointing at the
// right tool.
var slugRE = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)+$`)
var questRE = regexp.MustCompile(`^QUEST-\d+$`)

// slugHint returns the human-readable hint string when query looks
// like a quest or entry slug and returned zero hits. Returns "" when
// no hint applies so the caller can do `if hint != ""`.
func slugHint(query string) string {
	q := strings.TrimSpace(query)
	if q == "" || strings.ContainsAny(q, " \t\n") {
		return ""
	}
	if questRE.MatchString(q) {
		return fmt.Sprintf("(did you mean 'quest scroll %s'? slug-like query)", q)
	}
	if slugRE.MatchString(strings.ToLower(q)) {
		return "(did you mean 'quest list'? slug-like query)"
	}
	return ""
}

// Appraise runs the BM25+recency+title-boost pipeline against the lore
// database and returns a ranked AppraiseOutput.
//
// Pipeline:
//  1. Build FTS5 query from user query (ftsQuery).
//  2. Over-fetch 3× the limit via `SELECT ..., bm25(entries_fts) ...`.
//  3. If FTS5 returns zero rows, retry via LIKE fallback to handle
//     tokenization mismatches.
//  4. Re-rank every over-fetched row with Score(...) so the
//     title-match boost can promote exact-title hits.
//  5. Trim to Limit and bump access counters.
//
// Callers never pass raw SQL; all database parameters go through ? binds.
//
//nolint:gocritic // hugeParam: AppraiseParams is the public API surface; value semantics let callers build one inline without a temporary
func Appraise(ctx context.Context, db *sql.DB, params AppraiseParams) (*AppraiseOutput, error) {
	if db == nil {
		return nil, fmt.Errorf("lore: appraise: nil db")
	}
	q := strings.TrimSpace(params.Query)
	if q == "" {
		return nil, ErrEmptyQuery
	}
	limit := params.Limit
	if limit <= 0 {
		limit = defaultAppraiseLimit
	}
	overfetch := limit * appraiseOverfetch
	scoring := params.Scoring
	if scoring == (ScoringConfig{}) {
		scoring = DefaultScoring()
	}
	now := params.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	// ADR-003 gate: when the embedder is enabled AND coverage clears
	// CoverageThreshold, appraise runs the hybrid RRF path. Otherwise
	// it runs the Phase 0 BM25+stopwords path identical to the
	// pre-Phase-1 code. The decision is local to this function: no
	// global state, no caller coordination required. Cross-project
	// appraise (AllProjects=true) delegates to appraiseCrossProject
	// which fans out per-project and re-fuses the results.
	if params.AllProjects && params.Embed.Enabled() {
		out, handled, err := appraiseCrossProject(ctx, db, &params, now, scoring, limit)
		if err != nil {
			return nil, err
		}
		if handled {
			return out, nil
		}
	}
	if !params.AllProjects && params.Embed.Enabled() {
		out, handled, err := appraiseRRF(ctx, db, &params, now, scoring, limit)
		if err != nil {
			return nil, err
		}
		if handled {
			return out, nil
		}
	}

	rows, bm25s, err := runFTSQuery(ctx, db, q, &params, now, overfetch)
	if err != nil {
		return nil, err
	}
	fellBack := false
	if len(rows) == 0 {
		rows, bm25s, err = runLIKEFallback(ctx, db, q, &params, now, overfetch)
		if err != nil {
			return nil, err
		}
		fellBack = true
	}

	out := &AppraiseOutput{}
	if len(rows) == 0 {
		out.MissHint = slugHint(q)
		return out, nil
	}

	results := make([]AppraiseResult, len(rows))
	for i := range rows {
		// When fellBack, use recency alone (no BM25 contribution).
		var base float64
		if fellBack {
			base = NormalizeRecency(daysBetween(rows[i].CreatedAt, now), scoring.HalfLifeDays)
		} else {
			base = CombineScore(bm25s[i], daysBetween(rows[i].CreatedAt, now), scoring)
		}
		score := base + TitleBoost(rows[i].Title, q, scoring)
		results[i] = AppraiseResult{
			Entry: rows[i],
			Score: score,
			BM25:  bm25s[i],
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if len(results) > limit {
		results = results[:limit]
	}
	out.Results = results
	out.FellBackToLIKE = fellBack

	if params.AllProjects {
		counts := map[string]int{}
		for _, r := range results {
			counts[r.Entry.ProjectID]++
		}
		out.ProjectCounts = counts
	}

	if err := bumpAccessCounters(ctx, db, now, results); err != nil {
		// Telemetry-class error — don't fail the query just because
		// access counts couldn't be incremented. Surface via caller's
		// slog when we expose a logger later; keep the data path clean.
		out.Results = results
	}
	return out, nil
}

// runFTSQuery executes the BM25 match and returns the entries plus
// their per-row bm25() scores in matching order.
func runFTSQuery(ctx context.Context, db *sql.DB, query string, params *AppraiseParams, refNow time.Time, overfetch int) ([]*Entry, []float64, error) {
	fts := ftsQuery(query)
	if fts == "" {
		return nil, nil, nil
	}
	where, args := buildWhereClause(params, refNow)

	// NOTE: the SQL string is constant from the caller's perspective;
	// `where` only concatenates a hard-coded set of fragments. No
	// user-controlled text reaches the SQL text. All user values go
	// through `args`.
	//nolint:gosec // G202: entryColumns + where are constants built from a fixed whitelist; no user input reaches the SQL text
	sqlText := `SELECT ` + entryColumns + `, bm25(entries_fts) AS bm25_score
		FROM entries_fts
		JOIN entries e ON e.id = entries_fts.rowid
		WHERE entries_fts MATCH ?` + where + `
		ORDER BY entries_fts.rank
		LIMIT ?`
	all := append([]any{fts}, args...)
	all = append(all, overfetch)

	rows, err := db.QueryContext(ctx, sqlText, all...) //sqlcheck:ignore // sqlText is a constant template; buildWhereClause concatenates only hard-coded fragments
	if err != nil {
		return nil, nil, fmt.Errorf("lore: appraise: fts query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []*Entry
	var scores []float64
	for rows.Next() {
		e := &Entry{}
		var bm25 sql.NullFloat64
		if err := scanEntryWithBM25(rows, e, &bm25); err != nil {
			return nil, nil, err
		}
		entries = append(entries, e)
		scores = append(scores, bm25.Float64)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("lore: appraise: fts iterate: %w", err)
	}
	return entries, scores, nil
}

// runLIKEFallback handles the LIKE-based fallback when FTS5 returns zero
// rows.
func runLIKEFallback(ctx context.Context, db *sql.DB, query string, params *AppraiseParams, refNow time.Time, overfetch int) ([]*Entry, []float64, error) {
	like := "%" + query + "%"
	where, args := buildWhereClause(params, refNow)
	//nolint:gosec // G202: entryColumns + where are constants; no user input reaches the SQL text
	sqlText := `SELECT ` + entryColumns + `, 0.0 AS bm25_score
		FROM entries e
		WHERE (e.title LIKE ? OR e.summary LIKE ? OR COALESCE(e.tags,'') LIKE ?)` + where + `
		ORDER BY e.created_at DESC
		LIMIT ?`
	all := []any{like, like, like}
	all = append(all, args...)
	all = append(all, overfetch)

	rows, err := db.QueryContext(ctx, sqlText, all...) //sqlcheck:ignore // sqlText is a constant template; buildWhereClause concatenates only hard-coded fragments
	if err != nil {
		return nil, nil, fmt.Errorf("lore: appraise: like fallback: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []*Entry
	var scores []float64
	for rows.Next() {
		e := &Entry{}
		var bm25 sql.NullFloat64
		if err := scanEntryWithBM25(rows, e, &bm25); err != nil {
			return nil, nil, err
		}
		entries = append(entries, e)
		scores = append(scores, 0.0)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("lore: appraise: like iterate: %w", err)
	}
	return entries, scores, nil
}

// buildWhereClause concatenates the non-query filters (status, project,
// since) into a prefix-with-AND string plus the parameterized args slice.
// The caller pastes this into a SQL template that already has its first
// WHERE term (the MATCH or LIKE clause).
//
// refNow is the already-resolved reference timestamp (from Appraise's
// single time.Now() call). Passing it in keeps filtering and scoring on
// the same clock — critical when AppraiseParams.Now is injected for
// deterministic tests.
//
// Returns: (whereFragment, args). whereFragment begins with " AND " when
// non-empty so callers can unconditionally concatenate it after the
// template's initial MATCH/LIKE clause.
func buildWhereClause(params *AppraiseParams, refNow time.Time) (whereFragment string, args []any) {
	var parts []string

	if !params.IncludeAll {
		parts = append(parts, "e.status IN ('current','seed','exploring','imported')")
	}
	if !params.AllProjects && params.Project != "" {
		parts = append(parts, "e.project_id = ?")
		args = append(args, params.Project)
	}
	if params.Since > 0 {
		// datetime('now', '-N days') — we compute N from the duration
		// and bind it as a parameterized fragment via a literal
		// substitution into the SQL template. Since Since is a
		// time.Duration (not user text), this is safe from SQL
		// injection, but we still use parameterized binding via
		// julianday comparison so sqlcheck stays happy.
		parts = append(parts, "e.created_at >= datetime(?, 'utc')")
		cutoff := refNow.Add(-params.Since).Format(time.RFC3339)
		args = append(args, cutoff)
	}
	if len(parts) == 0 {
		return "", args
	}
	return " AND " + strings.Join(parts, " AND "), args
}

// bumpAccessCounters atomically increments access_count and stamps
// last_accessed_at on every returned entry.
func bumpAccessCounters(ctx context.Context, db *sql.DB, now time.Time, results []AppraiseResult) error {
	if len(results) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lore: bump access: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`UPDATE entries SET access_count = access_count + 1, last_accessed_at = ? WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("lore: bump access: prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	ts := now.Format(time.RFC3339)
	for _, r := range results {
		if _, err := stmt.ExecContext(ctx, ts, r.Entry.ID); err != nil {
			return fmt.Errorf("lore: bump access: exec: %w", err)
		}
	}
	return tx.Commit()
}

// entryColumns is the fixed SELECT list for the `entries` table. Declared
// once so every query that returns *Entry uses the same column order
// (which scanEntry relies on).
const entryColumns = `
	e.id, e.project_id, e.topic, e.kind, e.title, e.summary,
	COALESCE(e.tags,''), COALESCE(e.file_path,''),
	COALESCE(e.source,''), e.status,
	e.valid_days, e.needs_review, COALESCE(e.prompted_by,''),
	e.created_at, e.updated_at, e.access_count, e.last_accessed_at`

// scanEntry populates an Entry from the current row (expects
// entryColumns ordering). Nullable columns are read into sql.Null*
// temporaries and converted to Go-friendly zero values.
func scanEntry(row interface {
	Scan(dest ...any) error
}, e *Entry) error {
	var tagsStr, filePath, source, promptedBy string
	var validDays sql.NullInt64
	var needsReviewInt int64
	var createdAt, updatedAt string
	var lastAccessed sql.NullString

	if err := row.Scan(
		&e.ID, &e.ProjectID, &e.Topic, &e.Kind, &e.Title, &e.Summary,
		&tagsStr, &filePath, &source, &e.Status,
		&validDays, &needsReviewInt, &promptedBy,
		&createdAt, &updatedAt, &e.AccessCount, &lastAccessed,
	); err != nil {
		return fmt.Errorf("lore: scan entry: %w", err)
	}

	e.Tags = splitTags(tagsStr)
	e.FilePath = filePath
	e.Source = source
	e.PromptedBy = promptedBy
	if validDays.Valid {
		v := int(validDays.Int64)
		e.ValidDays = &v
	}
	e.NeedsReview = needsReviewInt != 0
	e.CreatedAt = parseSQLiteTime(createdAt)
	e.UpdatedAt = parseSQLiteTime(updatedAt)
	if lastAccessed.Valid {
		t := parseSQLiteTime(lastAccessed.String)
		e.LastAccessedAt = &t
	}
	return nil
}

// scanEntryWithBM25 extends scanEntry to also capture the FTS5 bm25()
// score that accompanies each row in appraise queries.
func scanEntryWithBM25(row interface {
	Scan(dest ...any) error
}, e *Entry, bm25 *sql.NullFloat64) error {
	var tagsStr, filePath, source, promptedBy string
	var validDays sql.NullInt64
	var needsReviewInt int64
	var createdAt, updatedAt string
	var lastAccessed sql.NullString

	if err := row.Scan(
		&e.ID, &e.ProjectID, &e.Topic, &e.Kind, &e.Title, &e.Summary,
		&tagsStr, &filePath, &source, &e.Status,
		&validDays, &needsReviewInt, &promptedBy,
		&createdAt, &updatedAt, &e.AccessCount, &lastAccessed,
		bm25,
	); err != nil {
		return fmt.Errorf("lore: scan entry w/ bm25: %w", err)
	}

	e.Tags = splitTags(tagsStr)
	e.FilePath = filePath
	e.Source = source
	e.PromptedBy = promptedBy
	if validDays.Valid {
		v := int(validDays.Int64)
		e.ValidDays = &v
	}
	e.NeedsReview = needsReviewInt != 0
	e.CreatedAt = parseSQLiteTime(createdAt)
	e.UpdatedAt = parseSQLiteTime(updatedAt)
	if lastAccessed.Valid {
		t := parseSQLiteTime(lastAccessed.String)
		e.LastAccessedAt = &t
	}
	return nil
}

// splitTags splits the comma-separated tags column into a []string,
// trimming whitespace and dropping empties.
func splitTags(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseSQLiteTime parses the SQLite datetime() format or RFC3339 form.
// Returns the zero time on parse failure.
func parseSQLiteTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// ParseSince converts the CLI "--since 7d|2w|1m" notation into a
// time.Duration. Returns 0 + error when input is malformed so callers
// can surface a clean usage error to stderr.
func ParseSince(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("lore: parse since: empty value")
	}
	m := sinceRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("lore: parse since: invalid format %q (want Nd|Nw|Nm)", s)
	}
	n, err := strconvAtoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("lore: parse since: %w", err)
	}
	switch m[2] {
	case "d":
		return time.Duration(n) * 24 * time.Hour, nil
	case "w":
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	case "m":
		// "m" = 30 days.
		return time.Duration(n) * 30 * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("lore: parse since: unknown unit %q", m[2])
}

var sinceRE = regexp.MustCompile(`^(\d+)([dwm])$`)

// strconvAtoi wraps strconv.Atoi so we don't import strconv just for
// this one call from ParseSince. Lives here to keep the public surface
// clean. (Separate function so tests can mock if ever needed.)
func strconvAtoi(s string) (int, error) {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("lore: parse int: non-digit %q", ch)
		}
		n = n*10 + int(ch-'0')
	}
	return n, nil
}
