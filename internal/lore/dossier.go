package lore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// DossierOutput is the structured form of `lore dossier`'s ~2k-token
// project context bundle. The CLI collapses it to a single text blob
// but keeping the sections separate lets the MCP wrapper hand
// individual chunks to subagents without re-parsing free text.
type DossierOutput struct {
	Project string

	// Principles are the current-status `kind=principle` entries.
	// Capped to avoid bloating the dossier when a project accumulates
	// many oaths (top-10 cap).
	Principles []*Entry

	// Decisions are the most recent current-status decisions (top 5).
	Decisions []*Entry

	// Observations are the most recent current-status observations
	// (top 5).
	Observations []*Entry

	// TopAccessed are the most-used current-status entries across
	// kinds — the "what do I read all the time" signal surfaced by
	// access_count.
	TopAccessed []*Entry

	// Whispers are current-pipeline ideas (seed/exploring).
	Whispers []*Entry

	// Text is the rendered form the CLI prints — built once and
	// cached so the CLI doesn't have to rebuild it (and so
	// dossier_test.go can measure token-count without a second pass).
	Text string
}

// Dossier compiles a ~2k-token project context bundle for subagent
// spawns. Sections: oath → decisions → observations → whispers, plus
// top-accessed entries and an explicit Project header.
//
// The function returns an empty (but non-nil) DossierOutput when the
// project has no content to bundle — callers decide whether that's
// a "no-op" or an error to surface.
func Dossier(ctx context.Context, db *sql.DB, project string) (*DossierOutput, error) {
	if db == nil {
		return nil, fmt.Errorf("lore: dossier: nil db")
	}
	if strings.TrimSpace(project) == "" {
		return nil, fmt.Errorf("lore: dossier: project required")
	}

	out := &DossierOutput{Project: project}

	// 1. Principles (oaths) — all current principles, capped at 10.
	principles, err := queryDossier(ctx, db, project, "principle", "current", "created_at ASC", 10)
	if err != nil {
		return nil, err
	}
	out.Principles = principles

	// 2. Decisions — most recent 5 current decisions.
	decisions, err := queryDossier(ctx, db, project, "decision", "current", "created_at DESC", 5)
	if err != nil {
		return nil, err
	}
	out.Decisions = decisions

	// 3. Observations — most recent 5 current observations.
	observations, err := queryDossier(ctx, db, project, "observation", "current", "created_at DESC", 5)
	if err != nil {
		return nil, err
	}
	out.Observations = observations

	// 4. Top-accessed — across kinds, current-like statuses, top 5 by
	// access_count DESC.
	topAccessed, err := queryTopAccessed(ctx, db, project, 5)
	if err != nil {
		return nil, err
	}
	out.TopAccessed = topAccessed

	// 5. Whispers — current pipeline ideas.
	whispers, err := Whispers(ctx, db, project, "")
	if err != nil {
		return nil, err
	}
	// Cap whispers for dossier size.
	if len(whispers) > 10 {
		whispers = whispers[:10]
	}
	out.Whispers = whispers

	out.Text = renderDossier(out)

	// ADR-006 Phase 7 optional compaction seam. DossierTransform is nil by
	// default and the compression module only makes it return a compacted
	// form when [modules].compression AND [compression].dossier_compact are
	// both on. With the default config the hook is nil (or returns ok=false),
	// so out.Text is the byte-identical dossier this function has always
	// produced. This is the ONLY coupling to the optional capability and it
	// is gated to a strict no-op on the default path.
	if DossierTransform != nil {
		if compacted, ok := DossierTransform(out); ok {
			out.Text = compacted
		}
	}
	return out, nil
}

// DossierTransform is an optional, nil-by-default hook that a capability
// module (compression, ADR-006 Phase 7) may set to rewrite the dossier text
// into a compact form plus a retrieve affordance. It is consulted only after
// the canonical dossier text is built; returning ok=false (or leaving it nil)
// preserves the exact bytes lore has always emitted. lore never imports the
// module that sets it, so this stays a one-way seam with no import cycle.
var DossierTransform func(out *DossierOutput) (text string, ok bool)

// queryDossier runs a kind+status-constrained query sorted by
// created_at with a fixed LIMIT. Used by every dossier section that
// wants "top-N of kind K with status current".
func queryDossier(ctx context.Context, db *sql.DB, project, kind, status, orderBy string, limit int) ([]*Entry, error) {
	// orderBy is constrained to a whitelist so we can safely embed it
	// in the SQL text (we cannot parameterize ORDER BY in standard
	// SQL). This keeps sqlcheck happy (no string building from user
	// input — only from hard-coded kind + orderBy picks).
	switch orderBy {
	case "created_at ASC", "created_at DESC":
		// ok
	default:
		return nil, fmt.Errorf("lore: dossier: invalid orderBy %q", orderBy)
	}
	//nolint:gosec // G202: orderBy is whitelist-validated above; entryColumns is a constant
	sqlText := `SELECT ` + entryColumns + `
		FROM entries e
		WHERE e.project_id = ? AND e.kind = ? AND e.status = ?
		ORDER BY ` + orderBy + `
		LIMIT ?`
	rows, err := db.QueryContext(ctx, sqlText, project, kind, status, limit) //sqlcheck:ignore // sqlText is a constant template; orderBy is whitelist-validated
	if err != nil {
		return nil, fmt.Errorf("lore: dossier: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Entry
	for rows.Next() {
		e := &Entry{}
		if err := scanEntry(rows, e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lore: dossier: iterate: %w", err)
	}
	return out, nil
}

// queryTopAccessed returns the top-N current entries for a project by
// access_count DESC.
func queryTopAccessed(ctx context.Context, db *sql.DB, project string, limit int) ([]*Entry, error) {
	//nolint:gosec // G202: entryColumns is a constant; no user input reaches the SQL text
	sqlText := `SELECT ` + entryColumns + `
		FROM entries e
		WHERE e.project_id = ? AND e.status = 'current' AND e.access_count > 0
		ORDER BY e.access_count DESC, e.last_accessed_at DESC
		LIMIT ?`
	rows, err := db.QueryContext(ctx, sqlText, project, limit) //sqlcheck:ignore // sqlText is a constant template
	if err != nil {
		return nil, fmt.Errorf("lore: dossier: top-accessed query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Entry
	for rows.Next() {
		e := &Entry{}
		if err := scanEntry(rows, e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lore: dossier: top-accessed iterate: %w", err)
	}
	return out, nil
}

// renderDossier builds the printable string the CLI emits. Format:
// top header line, blank-line-separated sections, bullet items prefixed
// with `  • `. Kept compact so the overall byte count lands in the
// 1.5k-2.5k token band.
func renderDossier(d *DossierOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== PROJECT DOSSIER: %s ===\n\n", d.Project)

	var sections []string
	if len(d.Principles) > 0 {
		sections = append(sections, renderSection("PRINCIPLES (follow these):", d.Principles, false))
	}
	if len(d.Decisions) > 0 {
		sections = append(sections, renderSection("KEY DECISIONS:", d.Decisions, true))
	}
	if len(d.Observations) > 0 {
		sections = append(sections, renderSection("RECENT OBSERVATIONS:", d.Observations, true))
	}
	if len(d.TopAccessed) > 0 {
		sections = append(sections, renderSection("TOP-ACCESSED:", d.TopAccessed, true))
	}
	if len(d.Whispers) > 0 {
		sections = append(sections, renderSection("CURRENT WHISPERS:", d.Whispers, true))
	}

	if len(sections) == 0 {
		b.WriteString("(no dossier content — inscribe principles, decisions, or observations first)\n")
		return b.String()
	}
	b.WriteString(strings.Join(sections, "\n\n"))
	b.WriteString("\n")
	return b.String()
}

// renderSection produces one titled section. When showTitle is true
// each bullet is rendered as `title: summary` (decisions/observations);
// when false the summary alone suffices (principles).
func renderSection(header string, entries []*Entry, showTitle bool) string {
	var lines []string
	lines = append(lines, header)
	for _, e := range entries {
		if showTitle {
			lines = append(lines, fmt.Sprintf("  • %s: %s", e.Title, compactSummary(e.Summary)))
		} else {
			lines = append(lines, fmt.Sprintf("  • %s", compactSummary(e.Summary)))
		}
	}
	return strings.Join(lines, "\n")
}

// compactSummary collapses newlines + runs of whitespace to single
// spaces so dossier bullets stay single-line. Truncates at 300 chars
// to carry one additional sentence of context while staying within the
// 1500-2500 token band on realistic corpora.
func compactSummary(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = whitespaceRE.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	const maxLen = 300
	if len(s) > maxLen {
		// Truncate on byte boundary with an ellipsis so consumers
		// know the summary was cut.
		return s[:maxLen] + "…"
	}
	return s
}

// ApproxTokens gives a crude token estimate for a Dossier text blob.
// The 4-chars-per-token heuristic is accurate to ~±15% for English
// prose; used by the dossier size test to enforce the 1.5k-2.5k band.
func ApproxTokens(s string) int {
	return len(s) / 4
}
