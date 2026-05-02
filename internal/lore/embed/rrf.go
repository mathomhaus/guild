package embed

import "sort"

// RRFK is the constant reciprocal-rank-fusion "smoothing" denominator
// k used throughout the retrieval pipeline. k=60 is the TREC-standard
// value Cormack et al. landed on in 2009 and is what ADR-002 and
// ADR-003 adopted without further tuning. Exposed as a named constant
// so callers do not repeat the magic number.
//
// ADR-003 "Retrieval" section explicitly rules out weighted-linear
// fusion: it was strictly worse than RRF across every weight tested in
// the 2026-04-23 spike, so there is no weighted variant in this package.
const RRFK = 60

// Ranked is one ranked list of int64 IDs in descending score order. The
// RRF input contract is purely ordinal: rankers hand in their own
// top-N ordering; the numerical scores never enter the fusion.
type Ranked []int64

// Fuse merges two ranked lists under Reciprocal Rank Fusion at k=RRFK.
// Items appear at most once in the output; ordering is by fused score
// descending, with a deterministic ascending-ID tiebreak so identical
// inputs always produce identical output (important for golden tests
// and for user-facing reproducibility).
//
// Either input may be empty; an empty list contributes zero to the
// fusion. The output length is bounded by limit when limit > 0, or by
// the total number of unique IDs when limit <= 0.
//
// Hexagonal: no IO, no embedder or index coupling. Callers translate
// their own rank sources into []int64 ordered slices and take the
// fused result. Tests exercise Fuse directly.
func Fuse(a, b Ranked, limit int) Ranked {
	// Score map: accumulate 1/(k+rank) contributions. Rank is 1-based
	// per the standard RRF formulation, not 0-based.
	scores := make(map[int64]float64, len(a)+len(b))
	for i, id := range a {
		scores[id] += 1.0 / float64(RRFK+(i+1))
	}
	for i, id := range b {
		scores[id] += 1.0 / float64(RRFK+(i+1))
	}
	return finalizeRRF(scores, limit)
}

// FuseMany merges N ranked lists under the same RRF formulation as
// Fuse. Equivalent to repeated pairwise Fuse calls but runs in a
// single O(sum(len(list))) pass. Used by cross-project appraise,
// where each project contributes exactly one ranked list (either its
// BM25+stopwords or its per-project RRF(BM25, vector) output) and the
// server takes the union.
//
// Empty / nil lists are skipped. Output bound follows the same rule
// as Fuse: limit > 0 caps, otherwise every unique ID appears.
func FuseMany(lists []Ranked, limit int) Ranked {
	scores := make(map[int64]float64)
	for _, list := range lists {
		for i, id := range list {
			scores[id] += 1.0 / float64(RRFK+(i+1))
		}
	}
	return finalizeRRF(scores, limit)
}

// finalizeRRF orders the accumulated score map into a descending
// ranked slice with a deterministic ascending-ID tiebreak. Extracted
// so Fuse and FuseMany share the same tail behaviour.
func finalizeRRF(scores map[int64]float64, limit int) Ranked {
	if len(scores) == 0 {
		return Ranked{}
	}
	type scored struct {
		id    int64
		score float64
	}
	ordered := make([]scored, 0, len(scores))
	for id, s := range scores {
		ordered = append(ordered, scored{id: id, score: s})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].score != ordered[j].score {
			return ordered[i].score > ordered[j].score
		}
		// Deterministic ascending-ID tiebreak. Matches Index.TopK's
		// tiebreak rule so a corpus with duplicate scores produces a
		// stable cross-pipeline ordering.
		return ordered[i].id < ordered[j].id
	})
	n := len(ordered)
	if limit > 0 && limit < n {
		n = limit
	}
	out := make(Ranked, n)
	for i := 0; i < n; i++ {
		out[i] = ordered[i].id
	}
	return out
}
