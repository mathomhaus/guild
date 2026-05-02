package lore

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Stopword regression suite (Phase 0 of ADR-003)
//
// Verifies two things:
//   1. Natural-language queries retrieve the correct entries even when the
//      query is dominated by stopword mass. Before stopword filtering, these
//      queries returned entries matching "did"/"we"/"work"/"last"/"week"
//      rather than the actual topic.
//   2. Exact-technical identifier queries (SQLITE_BUSY, withImmediateTx,
//      retryablehttp) still retrieve correct entries at rank 1. The spike
//      measured BM25+stopwords = 0.627 on exact-technical, identical to
//      plain BM25 = 0.627, confirming no regression.
//
// The fixture corpus is synthetic but structurally representative of the
// real 315-entry guild lore corpus used in the 2026-04-23 spike
// (lares-spikes/guild-embedding-purego, RESULTS.md). Numeric Recall@5
// targets below are conservative versions of the measured spike numbers:
//   Exact-technical: spike measured 0.627, fixture verifies >= 0.60
//   Natural-language: spike measured 0.327 with stopwords vs 0.203 without;
//                     fixture verifies >= 0.50 (synthetic corpus is smaller,
//                     less noisy, so absolute recall is higher)
// ---------------------------------------------------------------------------

// stopwordRegressionCorpus is the synthetic lore fixture. It covers four
// topic clusters (concurrency, HTTP retry, onboarding, rate limiting) and
// includes deliberate "noise" entries whose titles share stopword tokens with
// the natural-language queries but not the actual topic.
func stopwordRegressionCorpus() []fixtureEntry {
	return []fixtureEntry{
		// Concurrency cluster
		{"proj", "decision", "SQLITE_BUSY retry ceremony with BEGIN IMMEDIATE", "Use BEGIN IMMEDIATE to eliminate SQLITE_BUSY under concurrent writers. The retry loop uses exponential backoff with jitter.", "concurrency,sqlite"},
		{"proj", "decision", "withImmediateTx helper wraps BEGIN IMMEDIATE boilerplate", "withImmediateTx centralizes the BEGIN IMMEDIATE + retry pattern so every write path gets the same concurrency-safety guarantee without copy-paste drift.", "concurrency,helper"},
		{"proj", "decision", "Concurrent writers use BEGIN IMMEDIATE not BEGIN DEFERRED", "Switching from DEFERRED to IMMEDIATE eliminates the window where two writers can both obtain a read lock and then deadlock on the write-lock upgrade.", "concurrency,sqlite"},
		{"proj", "observation", "SQLITE_BUSY surfaces under high-frequency MCP tool calls", "Observed SQLITE_BUSY errors in the test suite when two goroutines call INSERT concurrently. Root cause: default journal mode + DEFERRED transaction.", "concurrency,bug"},
		// HTTP retry cluster
		{"proj", "decision", "retryablehttp adopted for HTTP retry logic in the linkcheck probe", "github.com/hashicorp/go-retryablehttp provides exponential backoff with jitter out of the box. RetryMax=3, WaitMin=1s, WaitMax=5s.", "http,retry,linkcheck"},
		{"proj", "decision", "RetryMax set to 3 in the probe HTTP client", "Three retries balances correctness (transient network errors) against cost (wall-clock time per batch). Measured p99 latency at 12s with RetryMax=3.", "http,retry,probe"},
		{"proj", "observation", "HTTP retry with exponential backoff reduces 503 errors by 80 percent", "After enabling retryablehttp's exponential backoff the 503 rate in staging dropped from 12% to 2%. The remaining 2% are genuine upstream failures.", "http,retry,reliability"},
		// Onboarding cluster
		{"proj", "observation", "guild init registers the project on first run automatically", "Running guild init in a git repo auto-detects the toplevel and registers the project. Skip-re-register logic makes re-running idempotent.", "init,onboarding"},
		{"proj", "decision", "guild mcp add wires the MCP server into the harness config on first run", "Users who run guild mcp add get the MCP server registered. Subsequent harness restarts pick it up automatically. No manual JSON editing needed.", "init,mcp,onboarding"},
		{"proj", "observation", "First-dogfood session showed guild init UX needs ordering fix", "The schema migration output printed after the preview prose, making the first-run experience feel choppy. Fixed by suppressing migration output during init.", "init,ux,onboarding"},
		// Rate limiting cluster
		{"proj", "decision", "Token bucket algorithm chosen for rate limiting at 10 rps with burst 20", "Token bucket handles burst traffic better than fixed-window. golang.org/x/time/rate provides the implementation.", "rate-limit,algorithm"},
		{"proj", "observation", "throttle_ratio gauge tracks the ratio of throttled to total requests", "Introduced throttle_ratio as a Prometheus gauge. Values above 0.05 trigger an alert. gauge name follows the existing metric naming convention.", "rate-limit,observability"},
		// Noise entries: titles share stopword tokens but cover unrelated topics
		{"proj", "observation", "Did the deploy succeed last week on staging", "Weekly deploy status. Last week all three staging deploys succeeded. The deploy pipeline is healthy.", "deploy,status"},
		{"proj", "observation", "We reviewed the work items for the sprint", "Sprint review notes. Work items triaged. No blocking issues found. We closed 8 tickets this week.", "sprint,review"},
		{"proj", "observation", "There are things to consider about database indexing", "General notes on index hygiene. Things like bloom filters and partial indexes are worth considering for the analytics tables.", "index,notes"},
		{"proj", "observation", "How we handle stuff in the async queue", "The async queue uses a worker pool. Things like backpressure and circuit-breaking are handled at the queue layer. We retry on transient errors.", "queue,async"},
	}
}

// naturalLanguageQueries are representative of the natural-language failure
// mode: the query is dominated by stopwords, so plain BM25 drifts to entries
// containing those stopwords rather than the actual topic tokens.
//
// Each query lists the entry title substrings that MUST appear in the top-5
// results. These are verified as the "correct" answers.
var naturalLanguageQueries = []struct {
	query      string
	mustHaveIn []string // entry title substrings that must appear in top-5
	desc       string
}{
	{
		"did we work on retry logic last week",
		[]string{"retryablehttp", "HTTP retry"},
		"HTTP retry entries must surface despite stopword mass in query",
	},
	{
		"is there anything about concurrency correctness",
		[]string{"SQLITE_BUSY", "BEGIN IMMEDIATE", "withImmediateTx"},
		"concurrency entries must surface despite stopword mass",
	},
	{
		"what happened with the rate limiter algorithm we chose",
		[]string{"Token bucket", "throttle_ratio"},
		"rate-limit entries must surface past stopword noise",
	},
	{
		"how should we handle the first run experience",
		[]string{"guild init", "guild mcp add"},
		"onboarding entries must surface past stopword noise",
	},
}

// exactTechnicalQueries are identifier-level queries that must NOT regress.
// These queries contain no stopwords, so stripping is a no-op and BM25
// dominates as before.
var exactTechnicalQueries = []struct {
	query   string
	mustTop string // entry title substring that must be in the top result
	desc    string
}{
	{"SQLITE_BUSY", "SQLITE_BUSY", "exact identifier at rank 1"},
	{"withImmediateTx", "withImmediateTx", "exact identifier at rank 1"},
	{"retryablehttp", "retryablehttp", "exact library name at rank 1"},
	{"throttle_ratio gauge", "throttle_ratio", "exact metric name at rank 1"},
	{"BEGIN IMMEDIATE", "BEGIN IMMEDIATE", "exact SQL phrase at rank 1"},
}

// TestStopwordFilter_NaturalLanguageRecall verifies that natural-language
// queries with heavy stopword mass retrieve the correct topic entries in the
// top-5. This is the primary regression guard for Phase 0 ADR-003.
func TestStopwordFilter_NaturalLanguageRecall(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	corpus := stopwordRegressionCorpus()
	seedCorpus(t, ctx, db, corpus)

	now := time.Now().UTC()
	hits := 0
	total := 0

	for _, q := range naturalLanguageQueries {
		q := q
		t.Run(q.desc, func(t *testing.T) {
			out, err := Appraise(ctx, db, AppraiseParams{
				Query:       q.query,
				Limit:       5,
				AllProjects: true,
				Scoring:     DefaultScoring(),
				Now:         now,
			})
			if err != nil {
				t.Fatalf("appraise %q: %v", q.query, err)
			}

			top5Titles := make([]string, 0, len(out.Results))
			for _, r := range out.Results {
				top5Titles = append(top5Titles, r.Entry.Title)
			}

			matched := 0
			for _, mustHave := range q.mustHaveIn {
				for _, title := range top5Titles {
					if strings.Contains(title, mustHave) {
						matched++
						break
					}
				}
			}

			total += len(q.mustHaveIn)
			hits += matched

			if matched < len(q.mustHaveIn) {
				t.Errorf("query %q: found %d/%d required entries in top-5\ntop-5: %v",
					q.query, matched, len(q.mustHaveIn), top5Titles)
			}
		})
	}

	recallAt5 := 0.0
	if total > 0 {
		recallAt5 = float64(hits) / float64(total)
	}
	t.Logf("natural-language Recall@5 = %d/%d = %.3f", hits, total, recallAt5)

	// Conservative floor: spike measured 0.327 on natural-language Recall@5
	// with stopwords. The synthetic corpus has less noise, so 0.50 is achievable.
	const recallFloor = 0.50
	if recallAt5 < recallFloor {
		t.Errorf("natural-language Recall@5 = %.3f, want >= %.3f", recallAt5, recallFloor)
	}
}

// TestStopwordFilter_ExactTechnicalNoRegression verifies that exact-technical
// identifier queries still return the correct entry at rank 1 after stopword
// filtering is applied. The spike measured BM25+stopwords = 0.627 on the
// exact-technical bucket, equal to plain BM25 = 0.627 (no regression).
func TestStopwordFilter_ExactTechnicalNoRegression(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	corpus := stopwordRegressionCorpus()
	seedCorpus(t, ctx, db, corpus)

	now := time.Now().UTC()
	hits := 0

	for _, q := range exactTechnicalQueries {
		q := q
		t.Run(q.desc, func(t *testing.T) {
			out, err := Appraise(ctx, db, AppraiseParams{
				Query:       q.query,
				Limit:       5,
				AllProjects: true,
				Scoring:     DefaultScoring(),
				Now:         now,
			})
			if err != nil {
				t.Fatalf("appraise %q: %v", q.query, err)
			}

			if len(out.Results) == 0 {
				t.Errorf("query %q: zero results (want rank-1 hit for %q)",
					q.query, q.mustTop)
				return
			}

			top := out.Results[0].Entry.Title
			if strings.Contains(top, q.mustTop) {
				hits++
			} else {
				t.Errorf("query %q: rank-1 = %q, want title containing %q",
					q.query, top, q.mustTop)
			}
		})
	}

	recallAt1 := float64(hits) / float64(len(exactTechnicalQueries))
	t.Logf("exact-technical Recall@1 = %d/%d = %.3f", hits, len(exactTechnicalQueries), recallAt1)

	// Must match the spike's exact-technical floor (spike: 0.627, fixture
	// target: >= 0.60 to allow for small corpus differences).
	const recallFloor = 0.60
	if recallAt1 < recallFloor {
		t.Errorf("exact-technical Recall@1 = %.3f, want >= %.3f", recallAt1, recallFloor)
	}
}

// TestStopwordFilter_StripIsNoOpOnTechnicalTokens verifies that stripping
// stopwords from exact-technical queries leaves the query intact. This is the
// unit invariant that guarantees exact-technical recall cannot regress due to
// over-filtering.
func TestStopwordFilter_StripIsNoOpOnTechnicalTokens(t *testing.T) {
	cases := []struct {
		query string
		// At least one token from the original query must survive stripping.
		wantToken string
	}{
		{"SQLITE_BUSY", "sqlite_busy"},
		{"BEGIN IMMEDIATE", "begin"},
		{"withImmediateTx", "withimmediatetx"},
		{"retryablehttp", "retryablehttp"},
		{"throttle_ratio gauge", "throttle_ratio"},
		{"FTS5", "fts5"},
		{"errgroup SetLimit", "errgroup"},
		{"entry_links informs", "entry_links"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.query, func(t *testing.T) {
			filtered := stripStopwords(tc.query)
			lower := strings.ToLower(filtered)
			if !strings.Contains(lower, tc.wantToken) {
				t.Errorf("stripStopwords(%q) = %q, want it to contain %q",
					tc.query, filtered, tc.wantToken)
			}
		})
	}
}

// TestStopwordFilter_PureStopwordQueryFallsThrough verifies that a query
// composed entirely of stopwords falls back to the original query text so
// FTS5 still has something to MATCH against (avoids empty-expression error).
func TestStopwordFilter_PureStopwordQueryFallsThrough(t *testing.T) {
	cases := []string{
		"is there anything",
		"did we do",
		"what is the",
		"how are we",
	}
	for _, q := range cases {
		q := q
		t.Run(q, func(t *testing.T) {
			result := stripStopwords(q)
			if result == "" {
				t.Errorf("stripStopwords(%q) returned empty string, want fallback to original", q)
			}
		})
	}
}

// TestFTSQuery_StopwordsStripped verifies that ftsQuery removes stopwords
// from a natural-language query before building the MATCH expression. The
// resulting expression must contain the topic tokens but not the stopwords.
func TestFTSQuery_StopwordsStripped(t *testing.T) {
	cases := []struct {
		in          string
		mustContain []string
		mustAbsent  []string
	}{
		{
			"did we work on retry logic last week",
			[]string{"retry*", "logic*"},
			[]string{"did*", "we*", "last*", "week*"},
		},
		{
			"is there anything about concurrency correctness",
			[]string{"concurrency*", "correctness*"},
			[]string{"is*", "there*", "anything*", "about*"},
		},
		{
			"what is the rate limiter algorithm we chose",
			[]string{"rate*", "limiter*", "algorithm*", "chose*"},
			[]string{"what*", "is*", "the*", "we*"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			got := ftsQuery(tc.in)
			if got == "" {
				t.Fatalf("ftsQuery(%q) returned empty string", tc.in)
			}
			for _, want := range tc.mustContain {
				if !strings.Contains(got, want) {
					t.Errorf("ftsQuery(%q) = %q, want it to contain %q",
						tc.in, got, want)
				}
			}
			for _, absent := range tc.mustAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("ftsQuery(%q) = %q, want it NOT to contain %q",
						tc.in, got, absent)
				}
			}
		})
	}
}

// TestStopwordFilter_Baseline measures Recall@5 of stopword-filtered appraise
// against the synthetic corpus across all natural-language query types and
// prints a summary table. This is the primary numeric verification of the
// Phase 0 win. The test fails only if any query type falls below its floor.
//
// Expected results from the 2026-04-23 spike (315-entry corpus, 42 queries):
//
//	Exact-technical  Recall@5: 0.627 (BM25+stop = BM25, no regression)
//	Natural-language Recall@5: 0.327 (vs 0.203 plain BM25, +12.4pp)
//	Overall          Recall@5: 0.391 (vs 0.344 plain BM25, +4.7pp)
func TestStopwordFilter_RecallSummary(t *testing.T) {
	type queryCase struct {
		query       string
		relevantIDs []int
		qtype       string
	}

	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	corpus := stopwordRegressionCorpus()
	ids := seedCorpus(t, ctx, db, corpus)

	titleToID := map[string]int64{}
	for i, fx := range corpus {
		titleToID[fx.Title] = ids[i]
	}

	queries := []queryCase{
		// Exact-technical bucket
		{
			"SQLITE_BUSY",
			indexesWhere(corpus, func(e fixtureEntry) bool {
				return strings.Contains(e.Title, "SQLITE_BUSY")
			}),
			"exact_technical",
		},
		{
			"BEGIN IMMEDIATE",
			indexesWhere(corpus, func(e fixtureEntry) bool {
				return strings.Contains(e.Summary, "BEGIN IMMEDIATE")
			}),
			"exact_technical",
		},
		{
			"withImmediateTx",
			indexesWhere(corpus, func(e fixtureEntry) bool {
				return strings.Contains(e.Title, "withImmediateTx")
			}),
			"exact_technical",
		},
		{
			"retryablehttp RetryMax",
			indexesWhere(corpus, func(e fixtureEntry) bool {
				return strings.Contains(e.Title, "retryablehttp") || strings.Contains(e.Title, "RetryMax")
			}),
			"exact_technical",
		},
		// Natural-language bucket
		{
			"did we work on retry logic last week",
			indexesWhere(corpus, func(e fixtureEntry) bool {
				return strings.Contains(e.Tags, "retry") || strings.Contains(e.Tags, "http")
			}),
			"natural_language",
		},
		{
			"is there anything about concurrency correctness",
			indexesWhere(corpus, func(e fixtureEntry) bool {
				return strings.Contains(e.Tags, "concurrency") || strings.Contains(e.Tags, "sqlite")
			}),
			"natural_language",
		},
		{
			"what happened with the rate limiter algorithm we chose",
			indexesWhere(corpus, func(e fixtureEntry) bool {
				return strings.Contains(e.Tags, "rate-limit")
			}),
			"natural_language",
		},
		{
			"how should we handle the first run experience",
			indexesWhere(corpus, func(e fixtureEntry) bool {
				return strings.Contains(e.Tags, "onboarding") || strings.Contains(e.Tags, "init")
			}),
			"natural_language",
		},
	}

	now := time.Now().UTC()
	type bucketStat struct {
		hits  int
		total int
	}
	stats := map[string]*bucketStat{}

	for _, q := range queries {
		if stats[q.qtype] == nil {
			stats[q.qtype] = &bucketStat{}
		}
		out, err := Appraise(ctx, db, AppraiseParams{
			Query:       q.query,
			Limit:       5,
			AllProjects: true,
			Scoring:     DefaultScoring(),
			Now:         now,
		})
		if err != nil {
			t.Fatalf("appraise %q: %v", q.query, err)
		}
		top5IDs := make(map[int64]bool)
		for _, r := range out.Results {
			top5IDs[r.Entry.ID] = true
		}
		for _, idx := range q.relevantIDs {
			id := ids[idx]
			if top5IDs[id] {
				stats[q.qtype].hits++
			}
			stats[q.qtype].total++
		}
	}

	// Print summary table.
	types := []string{"exact_technical", "natural_language"}
	sort.Strings(types)
	t.Log("Recall@5 summary:")
	t.Logf("  %-20s  %s", "query type", "recall@5")
	for _, qt := range types {
		s := stats[qt]
		if s == nil || s.total == 0 {
			t.Logf("  %-20s  n/a", qt)
			continue
		}
		recall := float64(s.hits) / float64(s.total)
		t.Logf("  %-20s  %d/%d = %.3f", qt, s.hits, s.total, recall)
	}

	// Floor assertions by bucket.
	floors := map[string]float64{
		"exact_technical":  0.60,
		"natural_language": 0.50,
	}
	for qt, floor := range floors {
		s := stats[qt]
		if s == nil || s.total == 0 {
			continue
		}
		recall := float64(s.hits) / float64(s.total)
		if recall < floor {
			t.Errorf("%s Recall@5 = %.3f, want >= %.3f (Phase 0 ADR-003 floor)",
				qt, recall, floor)
		}
	}
}

// indexesWhere returns the slice indices (not IDs) of corpus entries that
// satisfy pred. Used to build relevantIDs for the recall summary test.
func indexesWhere(corpus []fixtureEntry, pred func(fixtureEntry) bool) []int {
	var out []int
	for i, e := range corpus {
		if pred(e) {
			out = append(out, i)
		}
	}
	return out
}

// TestStopwords_BM25StopwordsConst verifies the stopword set contains the
// key high-noise tokens observed in the 2026-04-23 spike failure reproduction.
// If any of these is accidentally removed, the natural-language recall
// degrades back toward the pre-Phase-0 baseline.
//
// Tokens sourced from the spike's defaultStopwords in
// lares-spikes/guild-embedding-purego/pkg/corpus/corpus.go.
func TestStopwords_BM25StopwordsConst(t *testing.T) {
	required := []string{
		// Core English stopwords
		"a", "an", "and", "or", "the", "is", "are", "was", "were",
		// Query-noise tokens observed in the 2026-04-23 failure reproduction
		"did", "we", "last", "week",
		"there", "anything",
		"what", "how", "why", "when",
		// Guild-specific noise tokens
		"stuff", "thing", "things",
	}
	for _, w := range required {
		if _, ok := BM25Stopwords[w]; !ok {
			t.Errorf("BM25Stopwords is missing required token %q", w)
		}
	}
	if len(BM25Stopwords) < 50 {
		t.Errorf("BM25Stopwords has only %d entries, want >= 50", len(BM25Stopwords))
	}
}
