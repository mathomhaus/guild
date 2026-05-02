package lore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// nearDupWindowDays is the age cutoff for near-duplicate candidates.
// Only entries created within this window are considered. 14 days covers
// the "audit-style run" pattern from LORE-402: agents doing topical audits
// in a single sprint often write observations within days of each other.
// Extending beyond 2 weeks risks surfacing intentional re-assessments of
// slowly-evolving topics (false positives increase faster than recall
// improves beyond this window).
const nearDupWindowDays = 14

// nearDupJaccardTitleThreshold is the minimum Jaccard similarity on
// title tokens required for a candidate to be flagged. Value 0.40 means
// 40% of the union of title-word sets must overlap.
//
// Threshold rationale (erring toward false-positive per LORE-402 spec):
//   - LORE-399/LORE-400 reproducer pair shares 5/6 significant title
//     tokens: "MCP output UX friction inventory" vs "MCP output UX audit
//     findings". Observed Jaccard ~0.55 on the content tokens.
//   - Lowering to 0.30 would catch even looser paraphrases but fires on
//     entries that merely share a topic abbreviation (e.g. "MCP").
//   - 0.40 keeps the false-positive rate manageable while reliably catching
//     the LORE-399/400-class reproducer.
const nearDupJaccardTitleThreshold = 0.40

// nearDupJaccardSummaryThreshold is the minimum Jaccard similarity on
// summary trigrams. Trigrams on raw summary text catch structural
// similarity (same bullet-point shape, same sentence fragments) even when
// individual words differ.
//
// Value 0.20 is intentionally low because summaries are longer than titles
// and share fewer verbatim trigrams even for near-identical content. The
// combined OR of title-Jaccard or summary-Jaccard gives either detector a
// chance to fire independently, so a low summary threshold does not
// meaningfully increase false positives when the title check is also
// running.
const nearDupJaccardSummaryThreshold = 0.20

// NearDupCandidate is a recent entry that scored above the lexical-
// similarity threshold and is being surfaced to the caller as a potential
// near-duplicate of the just-inscribed entry.
type NearDupCandidate struct {
	EntryID   int64
	ProjectID string
	Kind      Kind
	Title     string
	// MatchReason is a short human-readable string naming which signal(s)
	// triggered the match: "same topic/tags", "same topic/prompted_by",
	// "same tags", etc.
	MatchReason string
}

// findNearDupCandidates queries for entries created within nearDupWindowDays
// that share at least one of: the same topic, a non-empty tag-set overlap,
// or the same prompted_by quest id. The candidates are then scored for
// lexical similarity. Only entries that exceed either the title-token
// Jaccard threshold or the summary-trigram Jaccard threshold are returned.
//
// The DB query is intentionally broad (OR of three signals) so the
// similarity filter carries the precision burden. This keeps the SQL simple
// and avoids index-tuning for an advisory hint path.
//
// Active entries only: archived/superseded entries are excluded because
// surfacing them is noise (the old duplicate was already dealt with).
func findNearDupCandidates(
	ctx context.Context,
	db *sql.DB,
	p *InscribeParams,
	newID int64,
	now time.Time,
) ([]NearDupCandidate, error) {
	cutoff := now.AddDate(0, 0, -nearDupWindowDays).Format(time.RFC3339)

	// Normalise the prompted_by value so an empty string never matches
	// NULL rows (SQL's NULL != '' comparisons).
	promptedBy := strings.TrimSpace(p.PromptedBy)

	// Pre-compute tag set for the new entry. If empty, the tag-overlap
	// branch is skipped in post-processing but we still fetch candidates
	// via topic/prompted_by.
	newTagSet := tagSet(p.Tags)

	// The WHERE clause uses OR across three signals so that a single
	// matching dimension is enough to enter the candidate set. The
	// lexical-similarity filter then decides whether the candidate is
	// close enough to emit a hint.
	//
	// Exclude the newly created entry itself (e.id <> ?) and
	// archived/superseded entries (noise after dedup work).
	rows, err := db.QueryContext(ctx,
		`SELECT e.id, e.project_id, e.kind, e.title, e.summary, e.tags,
		        e.topic, e.prompted_by
		   FROM entries e
		  WHERE e.status NOT IN ('archived', 'superseded')
		    AND e.id <> ?
		    AND e.created_at >= ?
		    AND (
		          e.topic = ?
		       OR (? <> '' AND e.prompted_by = ?)
		       OR (e.tags IS NOT NULL AND e.tags <> '')
		       )
		  ORDER BY e.created_at DESC
		  LIMIT 50`,
		newID,
		cutoff,
		p.Topic,
		promptedBy, promptedBy,
	)
	if err != nil {
		return nil, fmt.Errorf("lore: near-dup: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type rawRow struct {
		id        int64
		projectID string
		kind      string
		title     string
		summary   string
		tagsRaw   sql.NullString
		topic     string
		promptedB sql.NullString
	}

	var candidates []NearDupCandidate
	titleTokens := tokeniseTitle(p.Title)

	for rows.Next() {
		var r rawRow
		if err := rows.Scan(
			&r.id, &r.projectID, &r.kind, &r.title, &r.summary,
			&r.tagsRaw, &r.topic, &r.promptedB,
		); err != nil {
			return nil, fmt.Errorf("lore: near-dup: scan: %w", err)
		}

		// Determine which signals match this candidate.
		topicMatch := r.topic == p.Topic
		promptedMatch := promptedBy != "" && r.promptedB.Valid && r.promptedB.String == promptedBy
		candidateTags := parseTags(r.tagsRaw.String)
		tagMatch := len(newTagSet) > 0 && len(candidateTags) > 0 && tagOverlap(newTagSet, tagSet(candidateTags)) >= 1

		// Lexical similarity: Jaccard on title tokens.
		candTitleTokens := tokeniseTitle(r.title)
		titleJ := jaccardStrings(titleTokens, candTitleTokens)

		// Lexical similarity: Jaccard on summary trigrams.
		summaryJ := jaccardTrigrams(p.Summary, r.summary)

		// A candidate fires the hint when either similarity exceeds its
		// threshold AND at least one of the structural signals matches.
		// Requiring a structural signal (topic/tag/prompted_by) prevents
		// purely coincidental lexical overlap from firing on completely
		// unrelated entries.
		structMatch := topicMatch || promptedMatch || tagMatch
		lexMatch := titleJ >= nearDupJaccardTitleThreshold || summaryJ >= nearDupJaccardSummaryThreshold
		if !structMatch || !lexMatch {
			continue
		}

		reason := buildMatchReason(topicMatch, tagMatch, promptedMatch)
		candidates = append(candidates, NearDupCandidate{
			EntryID:     r.id,
			ProjectID:   r.projectID,
			Kind:        Kind(r.kind),
			Title:       r.title,
			MatchReason: reason,
		})

		// Surface at most one near-dup hint per inscribe to keep the
		// output lean. We emit the most-recently-created candidate that
		// passes the structural+lexical filter (SQL orders by created_at
		// DESC, first-match-wins). For an advisory hint, "most recent"
		// is a reasonable proxy for "most likely to be the duplicate
		// the user just meant to update", though it is not the same as
		// "highest-scoring".
		if len(candidates) >= 1 {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lore: near-dup: iterate: %w", err)
	}
	return candidates, nil
}

// buildMatchReason returns a short human-readable string naming the
// signal(s) that connected the candidate to the new entry.
func buildMatchReason(topicMatch, tagMatch, promptedMatch bool) string {
	var parts []string
	if topicMatch {
		parts = append(parts, "same topic")
	}
	if tagMatch {
		parts = append(parts, "overlapping tags")
	}
	if promptedMatch {
		parts = append(parts, "same prompted_by")
	}
	if len(parts) == 0 {
		return "similar content"
	}
	return strings.Join(parts, "/")
}

// tokeniseTitle extracts lowercase word tokens from s, stripping tokens
// shorter than 3 characters and dedupStopwords. This mirrors the FTS dedup
// query builder but produces a set (map) rather than an AND-of-prefixes.
// Min length 3 (vs 4 in ftsDedupQuery) because Jaccard operates on token
// sets and single-character tokens are already filtered by the 3-char floor.
func tokeniseTitle(s string) map[string]struct{} {
	raw := wordRe.FindAllString(strings.ToLower(s), -1)
	out := make(map[string]struct{}, len(raw))
	for _, t := range raw {
		if len(t) < 3 {
			continue
		}
		if _, stop := dedupStopwords[t]; stop {
			continue
		}
		out[t] = struct{}{}
	}
	return out
}

// jaccardStrings computes the Jaccard similarity coefficient between two
// string sets: |intersection| / |union|. Returns 0.0 when either set is
// empty (avoid divide-by-zero; empty title has no similarity to anything).
func jaccardStrings(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0.0
	}
	var inter int
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0.0
	}
	return float64(inter) / float64(union)
}

// trigrams returns the multiset of 3-character substring trigrams of s
// (lowercased, whitespace normalised). Trigrams are a simple structural
// fingerprint: two summaries that share bullet-point phrasing will have
// high trigram overlap even when not word-for-word identical.
//
// The trigram set is represented as a map[string]int (count per trigram)
// so jaccardTrigrams can compute a proper set-Jaccard on the distinct
// trigrams (not a multiset similarity, which would over-weight repeated
// trigrams in verbose summaries).
func trigrams(s string) map[string]struct{} {
	// Normalise: lowercase + collapse whitespace.
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	if len(s) < 3 {
		return nil
	}
	out := make(map[string]struct{}, len(s)-2)
	for i := 0; i+3 <= len(s); i++ {
		out[s[i:i+3]] = struct{}{}
	}
	return out
}

// jaccardTrigrams computes Jaccard similarity between the trigram sets of
// two strings. Returns 0.0 when either string produces fewer than 3 trigrams
// (too short for a meaningful signal).
func jaccardTrigrams(a, b string) float64 {
	ta := trigrams(a)
	tb := trigrams(b)
	if len(ta) < 3 || len(tb) < 3 {
		return 0.0
	}
	return jaccardStrings(ta, tb)
}

// tagSet converts a slice of tag strings into a set map for O(1) lookups.
func tagSet(tags []string) map[string]struct{} {
	s := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(strings.ToLower(t))
		if t != "" {
			s[t] = struct{}{}
		}
	}
	return s
}

// tagOverlap returns the count of tags shared between two tag sets.
func tagOverlap(a, b map[string]struct{}) int {
	var n int
	for k := range a {
		if _, ok := b[k]; ok {
			n++
		}
	}
	return n
}

// parseTags splits a comma-separated tags string into a slice. Returns nil
// for empty/whitespace-only input so callers can distinguish "no tags" from
// an empty tags column.
func parseTags(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
