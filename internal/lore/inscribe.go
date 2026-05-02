package lore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// InscribeParams is the typed input shape for Inscribe — one field per
// `lore inscribe` flag. Pointer-free value semantics on strings/slices
// because cobra's flag layer hands us empty strings for missing flags,
// not nils.
type InscribeParams struct {
	ProjectID     string    // resolved project id (NOT the directory path) — required
	Kind          Kind      // one of KindIdea..KindPrinciple; required
	Title         string    // required
	Summary       string    // required
	Topic         string    // required
	Tags          []string  // optional semantic tags
	Informs       []int64   // optional source entry IDs — creates informs edges after insert
	FilePath      string    // optional pointer to full content file
	Source        string    // optional URL or reference
	PromptedBy    string    // optional quest id
	NeedsReview   bool      // default false
	ValidDays     *int      // nil → use kind's default decay; pointer lets 0 mean "never stale" if ever needed
	NoWarn        bool      // suppress the ≤60-word principle warning
	StrictProject bool      // opt-out of cross-project dedup (default: cross-project)
	Now           time.Time // injectable for deterministic tests; zero → time.Now().UTC()

	// Embed is the optional embeddings pipeline. When nil or not Enabled,
	// Inscribe commits the row with vector_state='pending' (the schema
	// default) and skips every vector-write step. When Enabled, Inscribe
	// dispatches a Tx2 after the Tx1 commit per ADR-003 "Mutation semantics":
	// async for MCP (Embed.Async=true) so inscribe latency stays flat, sync
	// for CLI so the process can exit cleanly. Tx2 failures are logged and
	// never surface as caller errors.
	Embed *EmbedDeps
}

// InscribeResult carries the inserted Entry plus any dedup hits surfaced
// during the pre-insert check so the caller can print a friendly
// `⚠️ possible duplicate of ENTRY-N` line without re-querying the DB.
type InscribeResult struct {
	Entry       *Entry
	DedupHits   []DedupHit
	BloatWarned bool // true if the ≤60-word principle rule fired
	BloatWords  int  // word count that triggered the warning (0 if not warned)
	// NearDupHint is non-empty when a recent entry with matching
	// topic/tags/prompted_by exceeded the lexical-similarity threshold.
	// The caller emits this as a 💡 hint line.
	NearDupHint string
}

// DedupHit is one entry that the inscribe-time dedup heuristic flagged as
// a potential duplicate of the about-to-be-inserted entry. The caller
// decides how to surface these (CLI prints them; MCP returns them in the
// structured response).
type DedupHit struct {
	EntryID   int64
	ProjectID string
	Kind      Kind
	Status    Status
	Title     string
}

// kindValidDays maps each kind to its default valid_days.
// nil pointer value = "never expires"; otherwise N days.
func kindValidDays(kind Kind) *int {
	switch kind {
	case KindResearch:
		n := 30
		return &n
	case KindDecision:
		n := 180
		return &n
	default: // idea, observation, principle — never auto-stale
		return nil
	}
}

// ErrInvalidKind is returned by Inscribe/Update when the caller supplies a
// kind that is not in AllKinds(). Callers can errors.Is it to distinguish
// bad-input from DB failures.
var ErrInvalidKind = errors.New("lore: invalid kind")

// ErrMissingField is returned when a required Inscribe field is empty.
var ErrMissingField = errors.New("lore: missing required field")

// PrincipleMaxWordsDefault is the inscribe-time warn threshold for
// principle entries: combined title+summary must be ≤60 words to keep
// the session-start oath wall lean. Exposed so CLI and MCP callers can
// thread cfg.Inscribe.PrincipleMaxWords through explicitly — this package
// does NOT import internal/config to keep the dependency graph pointing
// downward.
const PrincipleMaxWordsDefault = 60

// validateInscribe checks required fields and kind validity before we
// touch the DB. Keeps the Inscribe function body focused on I/O.
func validateInscribe(p *InscribeParams) error {
	if strings.TrimSpace(p.ProjectID) == "" {
		return fmt.Errorf("%w: project id", ErrMissingField)
	}
	if strings.TrimSpace(p.Title) == "" {
		return fmt.Errorf("%w: title", ErrMissingField)
	}
	if strings.TrimSpace(p.Summary) == "" {
		return fmt.Errorf("%w: summary", ErrMissingField)
	}
	if strings.TrimSpace(p.Topic) == "" {
		return fmt.Errorf("%w: topic", ErrMissingField)
	}
	if !isValidKind(p.Kind) {
		return fmt.Errorf("%w: %q (valid: idea, research, decision, observation, principle)",
			ErrInvalidKind, string(p.Kind))
	}
	return nil
}

// isValidKind reports whether k is one of the five canonical kinds.
func isValidKind(k Kind) bool {
	for _, v := range AllKinds() {
		if v == k {
			return true
		}
	}
	return false
}

// Inscribe creates a new entry. It performs:
//
//  1. Field validation (required fields, kind enum).
//  2. Cross-project dedup check (default) or project-scoped check when
//     StrictProject is true. Surfaces hits via the returned
//     InscribeResult — does NOT block the insert.
//  3. ≤60-word principle warning for kind=principle. Returns
//     BloatWarned=true but still inserts unless NoWarn is set, in which
//     case the check is skipped entirely.
//  4. Single-row INSERT with kind-appropriate defaults (status=seed for
//     kind=idea, else status=current; valid_days from kindValidDays when
//     caller didn't override).
//
// The caller owns presentation: emoji lines, warning output, dedup hit
// formatting. Inscribe's responsibility ends at "row inserted; here's
// what you should know about it."
func Inscribe(ctx context.Context, db *sql.DB, p *InscribeParams) (*InscribeResult, error) {
	if db == nil {
		return nil, fmt.Errorf("lore: inscribe: nil db")
	}
	if p == nil {
		return nil, fmt.Errorf("lore: inscribe: nil params")
	}
	if err := validateInscribe(p); err != nil {
		return nil, err
	}

	now := p.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Dedup check (surfaces hits; never blocks the insert).
	hits, err := findDedupHits(ctx, db, p.Title, p.ProjectID, p.StrictProject)
	if err != nil {
		// FTS MATCH on a degenerate query can error (unlikely — we
		// pre-filter — but surface anyway). Dedup is best-effort;
		// we do NOT fail the insert on dedup-query failure.
		hits = nil
	}

	// ≤60-word principle warning. Only fires for kind=principle and only
	// when NoWarn is false. Returned to caller in the result; caller
	// prints the actual stderr message so we don't touch stderr inside
	// a domain function.
	bloatWarned := false
	bloatWords := 0
	if p.Kind == KindPrinciple && !p.NoWarn {
		words := countWords(p.Title) + countWords(p.Summary)
		if words > PrincipleMaxWordsDefault {
			bloatWarned = true
			bloatWords = words
		}
	}

	// Ideas default to 'seed' status; everything else starts 'current'.
	status := StatusCurrent
	if p.Kind == KindIdea {
		status = StatusSeed
	}

	validDays := p.ValidDays
	if validDays == nil {
		validDays = kindValidDays(p.Kind)
	}

	tags := strings.Join(p.Tags, ",")

	// Prepared INSERT — fixed SQL, bound values.
	res, err := db.ExecContext(ctx,
		`INSERT INTO entries
		   (project_id, topic, kind, title, summary, tags, file_path, source,
		    status, valid_days, needs_review, prompted_by, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ProjectID,
		p.Topic,
		string(p.Kind),
		p.Title,
		p.Summary,
		nullIfEmpty(tags),
		nullIfEmpty(p.FilePath),
		nullIfEmpty(p.Source),
		string(status),
		nullIfNilInt(validDays),
		boolToInt(p.NeedsReview),
		nullIfEmpty(p.PromptedBy),
		now.Format(time.RFC3339),
		now.Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("lore: inscribe: insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("lore: inscribe: last insert id: %w", err)
	}

	// Every newly inscribed entry is active (seed or current; never archived
	// or parked at insert time), so always increment the coverage denominator.
	// This keeps vector_coverage_den in sync with the live active-entry count
	// and prevents num > den drift (QUEST-220 / LORE-373).
	if _, err := db.ExecContext(ctx, sqlBumpCoverageDen); err != nil {
		return nil, fmt.Errorf("lore: inscribe: bump vector_coverage_den: %w", err)
	}

	entry := &Entry{
		ID:          id,
		ProjectID:   p.ProjectID,
		Topic:       p.Topic,
		Kind:        p.Kind,
		Title:       p.Title,
		Summary:     p.Summary,
		Tags:        append([]string(nil), p.Tags...),
		FilePath:    p.FilePath,
		Source:      p.Source,
		Status:      status,
		ValidDays:   validDays,
		NeedsReview: p.NeedsReview,
		PromptedBy:  p.PromptedBy,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// Create informs provenance edges. On any link failure, delete the
	// just-created entry and surface a clean error.
	for _, fromID := range p.Informs {
		if err := LinkEntries(ctx, db, fromID, id, RelationInforms); err != nil {
			_, _ = db.ExecContext(ctx, `DELETE FROM entries WHERE id = ?`, id)
			return nil, fmt.Errorf("lore: inscribe: link %s → %s: %w",
				formatEntryID(fromID), formatEntryID(id), err)
		}
	}

	// Phase 1 vector write. No-op when Embed is nil / disabled; otherwise
	// runs the Tx2 helper async (MCP) or sync (CLI) per ADR-003
	// "Mutation semantics". Tx2 failures log-and-swallow so a broken
	// embedder never blocks a successful inscribe.
	p.Embed.runTx2(ctx, db, entry.ID, entry.Summary)

	// Near-duplicate hint: check recent entries (<=14d) for lexical
	// similarity on title tokens and summary trigrams. Best-effort only;
	// a query failure suppresses the hint without blocking the return.
	nearDupHint := ""
	if ndCands, ndErr := findNearDupCandidates(ctx, db, p, id, now); ndErr == nil && len(ndCands) > 0 {
		c := ndCands[0]
		nearDupHint = fmt.Sprintf(
			"looks similar to %s %q (%s). did you mean to update or link instead of inscribe new?",
			formatEntryID(c.EntryID), c.Title, c.MatchReason,
		)
	}

	return &InscribeResult{
		Entry:       entry,
		DedupHits:   hits,
		BloatWarned: bloatWarned,
		BloatWords:  bloatWords,
		NearDupHint: nearDupHint,
	}, nil
}

// findDedupHits runs the inscribe-time AND-of-content-tokens FTS5 query
// against entries. If strictProject is false (the default), the check
// spans every project's entries so cross-project rename artifacts get
// caught. Archived/superseded entries are excluded — if an old duplicate
// was already reforged, surfacing it again is noise.
//
// Returns up to 3 hits, sorted by FTS rank (most similar first). Empty
// slice when no usable dedup tokens remain (very short/all-stopword
// titles) — in which case the caller SHOULD still insert, because a
// useful check can't be constructed.
func findDedupHits(ctx context.Context, db *sql.DB, title, projectID string, strictProject bool) ([]DedupHit, error) {
	dedupQ := ftsDedupQuery(title)
	if dedupQ == "" {
		return nil, nil
	}

	var (
		rows *sql.Rows
		err  error
	)
	if strictProject {
		rows, err = db.QueryContext(ctx,
			`SELECT e.id, e.project_id, e.kind, e.status, e.title
			   FROM entries e
			   JOIN entries_fts f ON e.id = f.rowid
			  WHERE f.entries_fts MATCH ?
			    AND e.project_id = ?
			    AND e.status NOT IN ('archived', 'superseded')
			  ORDER BY f.rank
			  LIMIT 3`,
			dedupQ, projectID,
		)
	} else {
		rows, err = db.QueryContext(ctx,
			`SELECT e.id, e.project_id, e.kind, e.status, e.title
			   FROM entries e
			   JOIN entries_fts f ON e.id = f.rowid
			  WHERE f.entries_fts MATCH ?
			    AND e.status NOT IN ('archived', 'superseded')
			  ORDER BY f.rank
			  LIMIT 3`,
			dedupQ,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("lore: inscribe: dedup query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var hits []DedupHit
	for rows.Next() {
		var h DedupHit
		var kindStr, statusStr string
		if err := rows.Scan(&h.EntryID, &h.ProjectID, &kindStr, &statusStr, &h.Title); err != nil {
			return nil, fmt.Errorf("lore: inscribe: scan dedup row: %w", err)
		}
		h.Kind = Kind(kindStr)
		h.Status = Status(statusStr)
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lore: inscribe: iterate dedup rows: %w", err)
	}
	return hits, nil
}

// dedupStopwords is the set of noise words stripped before building
// the FTS dedup query so short titles like "lore entries across
// projects" don't match everything.
var dedupStopwords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "not": {}, "with": {}, "from": {},
	"that": {}, "this": {}, "these": {}, "those": {}, "are": {}, "was": {},
	"were": {}, "been": {}, "have": {}, "has": {}, "had": {}, "but": {},
	"all": {}, "can": {}, "any": {}, "will": {}, "into": {}, "when": {},
	"then": {}, "than": {}, "there": {}, "about": {}, "over": {}, "under": {},
	"between": {}, "before": {}, "after": {}, "through": {}, "also": {},
	"only": {}, "just": {}, "lore": {}, "entry": {}, "quest": {},
	"guild": {}, "project": {}, "agent": {}, "agents": {},
}

// wordRe matches word characters (unicode letters + digits + underscore).
var wordRe = regexp.MustCompile(`\w+`)

// ftsDedupQuery builds a
// high-precision AND-of-prefixes FTS5 expression from the content words
// in title. Returns "" when the title has fewer than 3 usable content
// tokens (too short to make a useful match), which skips dedup entirely.
func ftsDedupQuery(title string) string {
	raw := wordRe.FindAllString(strings.ToLower(title), -1)
	content := make([]string, 0, len(raw))
	for _, t := range raw {
		if len(t) < 4 {
			continue
		}
		if _, stop := dedupStopwords[t]; stop {
			continue
		}
		content = append(content, t)
	}
	if len(content) < 3 {
		return ""
	}
	// Cap at 5 to keep the query tractable on very long titles.
	if len(content) > 5 {
		content = content[:5]
	}
	terms := make([]string, len(content))
	for i, t := range content {
		terms[i] = t + "*"
	}
	return strings.Join(terms, " AND ")
}

// countWords returns the number of whitespace-separated tokens in s.
// Used for the ≤60-word principle check. Consecutive whitespace counts
// as one separator (same as strings.Fields semantics).
func countWords(s string) int {
	return len(strings.Fields(s))
}

// --- Small nullable-column helpers (keep INSERT/UPDATE call sites readable) ---

// nullIfEmpty returns a sql.NullString that reflects empty-string → NULL.
// The entries table treats empty tags/file_path/source/prompted_by as NULL
// so downstream queries can COALESCE safely.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullIfNilInt returns nil (stored as NULL) if p is nil, else *p.
func nullIfNilInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// boolToInt maps a Go bool into SQLite's 0/1 integer convention.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
