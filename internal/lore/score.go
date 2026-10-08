package lore

import (
	"math"
	"regexp"
	"strings"
	"time"
)

// ScoringConfig holds the hybrid-ranking knobs used by Appraise.
// Redeclared here so the lore package does not take a compile-time
// dependency on config. Callers populate it with ScoringFromConfig.
//
// Per-query overrides (`--w-fts` / `--w-recency`) are applied by
// mutating a copy of this struct before passing it to Score / Appraise.
type ScoringConfig struct {
	// WFTS is the weight applied to the normalized BM25 signal.
	WFTS float64
	// WRecency is the weight applied to the recency-decay signal.
	WRecency float64
	// HalfLifeDays controls the exponential recency decay:
	// score halves every HalfLifeDays days since created_at.
	HalfLifeDays float64
	// TitleMatchBoost is added to the total when the query exactly
	// matches the entry title (whitespace + case normalized).
	TitleMatchBoost float64
	// TitleTokenBoost is added to the total when every query token
	// is a subset of the entry's title tokens (order-independent).
	TitleTokenBoost float64
}

// DefaultScoring ranks durable evidence by relevance. Recency is opt-in
// through ScoringConfig or a since filter, rather than an implicit penalty
// on old knowledge.
// DefaultScoring returns the built-in scoring knobs.
// Tests and callers that want the "no config, just score" path use this.
func DefaultScoring() ScoringConfig {
	return ScoringConfig{
		WFTS:            0.7,
		WRecency:        0,
		HalfLifeDays:    30,
		TitleMatchBoost: 1.0,
		TitleTokenBoost: 0.5,
	}
}

// pyLn2 is 0.693, a fixed approximation of ln(2) used in recency
// normalization. The value is pinned so tests that assert on specific
// decay outputs remain stable across floating-point backends.
const pyLn2 = 0.693

// whitespaceRE collapses runs of whitespace to a single space.
var whitespaceRE = regexp.MustCompile(`\s+`)

// wordRE extracts \w+ tokens.
var wordRE = regexp.MustCompile(`\w+`)

// NormalizeFTS maps negative FTS5 BM25 scores to nonnegative evidence.
// log1p preserves the ordering even at large magnitudes, unlike the old
// sigmoid which rounded strong matches to the same value. BM25 depends on
// the query and corpus; this value is a ranking signal, not a probability.
func NormalizeFTS(bm25 float64) float64 {
	if bm25 >= 0 {
		return 0
	}
	return math.Log1p(-bm25)
}

// NormalizeRecency returns an exponential-decay weight in [0, 1] given the
// entry's age in days. half_life_days controls how fast old entries decay.
func NormalizeRecency(ageDays, halfLifeDays float64) float64 {
	if halfLifeDays <= 0 {
		// Defensive: avoid divide-by-zero; treat as "no decay".
		return 1.0
	}
	return math.Exp(-pyLn2 * ageDays / halfLifeDays)
}

// CombineScore hybridizes BM25 and recency into a single ranking number.
func CombineScore(bm25, ageDays float64, cfg ScoringConfig) float64 {
	return cfg.WFTS*NormalizeFTS(bm25) + cfg.WRecency*NormalizeRecency(ageDays, cfg.HalfLifeDays)
}

// normalizeQuery returns the lower-cased, whitespace-collapsed form of
// s. Used both for exact-title comparison and token extraction so the
// two signals agree on what "the query" is.
func normalizeQuery(s string) string {
	normalized := strings.ToLower(strings.TrimSpace(s))
	// Most titles already use single spaces. Preserve the regexp's exact
	// whitespace semantics while avoiding its scan and output allocation.
	if !strings.Contains(normalized, "  ") && !strings.ContainsAny(normalized, "\t\n\r\f") {
		return normalized
	}
	return whitespaceRE.ReplaceAllString(normalized, " ")
}

// tokenSet returns the set of \w+ tokens in s, lower-cased.
func tokenSet(s string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, t := range wordRE.FindAllString(strings.ToLower(s), -1) {
		out[t] = struct{}{}
	}
	return out
}

// isSubset returns true when every element of a appears in b.
// Empty a returns false so that an empty query can never "match all".
func isSubset(a, b map[string]struct{}) bool {
	if len(a) == 0 {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// TitleBoost returns the exact-title / all-tokens-in-title additive boost
// for one entry against the user's query. The two boost levels are
// additive with the base BM25+recency score.
func TitleBoost(title, query string, cfg ScoringConfig) float64 {
	return prepareTitleQuery(query).boost(title, cfg)
}

type preparedTitleQuery struct {
	normalized string
	tokens     map[string]struct{}
}

// prepareTitleQuery shares query normalization and token extraction across
// a candidate batch; title normalization remains per distinct entry.
func prepareTitleQuery(query string) preparedTitleQuery {
	normalized := normalizeQuery(query)
	return preparedTitleQuery{normalized: normalized, tokens: tokenSet(normalized)}
}

func (q preparedTitleQuery) boost(title string, cfg ScoringConfig) float64 {
	if q.normalized == "" {
		return 0
	}
	normalizedTitle := normalizeQuery(title)
	if normalizedTitle == q.normalized {
		return cfg.TitleMatchBoost
	}
	if isSubset(q.tokens, tokenSet(normalizedTitle)) {
		return cfg.TitleTokenBoost
	}
	return 0
}

// Score computes the final hybrid ranking number for one entry against
// the user's query, at time `now`. BM25 is supplied by the caller — the
// FTS5 MATCH query returns it via `bm25(entries_fts)`. ageDays is derived
// from entry.CreatedAt relative to `now` (zero-valued CreatedAt is
// treated as "today"). The formula is pinned so regression tests
// anchored to specific weight combinations remain stable.
func Score(entry *Entry, query string, bm25 float64, cfg ScoringConfig, now time.Time) float64 {
	if entry == nil {
		return 0
	}
	ageDays := daysBetween(entry.CreatedAt, now)
	base := CombineScore(bm25, ageDays, cfg)
	return base + TitleBoost(entry.Title, query, cfg)
}

// daysBetween returns the number of whole days between a (older) and b
// (newer), truncating toward zero. A zero created timestamp returns 0 days
// so an unset CreatedAt behaves like "today".
func daysBetween(a, b time.Time) float64 {
	if a.IsZero() {
		return 0
	}
	return math.Trunc(b.Sub(a).Hours() / 24)
}
