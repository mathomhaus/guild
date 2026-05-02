package lore

import "strings"

// BM25Stopwords is the validated English stopword set used to filter query
// tokens before the FTS5 MATCH expression is built. The set is sourced from
// the 2026-04-23 primary embedding spike:
//
//	lares-spikes/guild-embedding-purego/pkg/corpus/corpus.go defaultStopwords
//
// Measured effect: +12.4pp Recall@5 on natural-language queries against the
// 315-entry guild lore corpus (42-query eval set, 2026-04-23). No regression
// on exact-technical queries (BM25+stop = 0.627, same as plain BM25 = 0.627).
//
// Words like "work", "stuff", "thing", "things", "anything", "last", "week"
// are included because they appear as dominant tokens in natural-language
// queries like "did we work on retry logic last week" and drag BM25 toward
// irrelevant entries.
var BM25Stopwords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "any": {}, "are": {}, "about": {},
	"as": {}, "at": {}, "be": {}, "by": {}, "do": {}, "does": {},
	"did": {}, "for": {}, "from": {}, "have": {}, "has": {}, "i": {},
	"if": {}, "in": {}, "is": {}, "it": {}, "its": {}, "last": {},
	"like": {}, "me": {}, "my": {}, "of": {}, "on": {}, "one": {},
	"or": {}, "our": {}, "out": {}, "over": {}, "so": {}, "some": {},
	"that": {}, "the": {}, "their": {}, "them": {}, "then": {},
	"there": {}, "these": {}, "they": {}, "this": {}, "to": {},
	"too": {}, "up": {}, "us": {}, "was": {}, "we": {}, "week": {},
	"were": {}, "what": {}, "when": {}, "where": {}, "which": {},
	"while": {}, "who": {}, "why": {}, "will": {}, "with": {},
	"would": {}, "you": {}, "your": {}, "how": {}, "anything": {},
	"stuff": {}, "thing": {}, "things": {},
}

// stripStopwords removes tokens present in BM25Stopwords from q, returning
// the remaining tokens joined by spaces. If all tokens are stopwords, returns
// the original q unchanged so callers always have a non-empty MATCH expression
// to hand to FTS5.
func stripStopwords(q string) string {
	tokens := wordRE.FindAllString(strings.ToLower(q), -1)
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if _, stop := BM25Stopwords[t]; !stop {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return q
	}
	return strings.Join(out, " ")
}
