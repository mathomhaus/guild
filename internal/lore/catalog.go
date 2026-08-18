package lore

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CatalogResult is the output of Catalog.
type CatalogResult struct {
	Imported int // entries successfully inscribed
	Skipped  int // files already indexed (by file_path) or dedup hit
}

// CatalogParams configures a Catalog run. Mirrors the flags on
// `lore catalog DIR`.
type CatalogParams struct {
	// Dir is the directory to walk for .md files. Required.
	Dir string

	// ProjectID is the resolved project id. Required.
	ProjectID string

	// Topic overrides the per-file topic slug. When empty, the file
	// stem is used as the topic (lowercased).
	Topic string

	// Kind overrides the kind for all imported entries. When empty,
	// Catalog infers from the file path (decision/adr/design → decision,
	// else research).
	Kind Kind

	// Tags is an optional comma-separated tag string applied to all
	// imported entries.
	Tags string

	// Now is injectable for deterministic tests; zero → time.Now().UTC().
	Now time.Time
}

// Catalog walks .md files under p.Dir and inscribes each as a lore entry.
//
// Skip conditions (in order):
//  1. File already indexed: an entry with the same file_path exists in
//     the project (idempotent re-runs). Skipped++ but NOT error.
//  2. The directory does not exist or is not a directory: returns error.
func Catalog(ctx context.Context, db *sql.DB, p *CatalogParams) (*CatalogResult, error) {
	if db == nil {
		return nil, fmt.Errorf("lore: catalog: nil db")
	}
	if p == nil {
		return nil, fmt.Errorf("lore: catalog: nil params")
	}
	if strings.TrimSpace(p.ProjectID) == "" {
		return nil, fmt.Errorf("lore: catalog: projectID required")
	}
	if strings.TrimSpace(p.Dir) == "" {
		return nil, fmt.Errorf("lore: catalog: dir required")
	}
	if p.Kind != "" && !isValidKind(p.Kind) {
		return nil, fmt.Errorf("%w: %q (valid: idea, research, decision, observation, principle)",
			ErrInvalidKind, string(p.Kind))
	}

	info, err := os.Stat(p.Dir)
	if err != nil {
		return nil, fmt.Errorf("lore: catalog: stat dir %q: %w", p.Dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("lore: catalog: %q is not a directory", p.Dir)
	}

	now := p.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	result := &CatalogResult{}

	// Walk all .md files recursively.
	err = filepath.WalkDir(p.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}

		absPath, err := filepath.Abs(path)
		if err != nil {
			absPath = path
		}

		// Skip if already indexed (idempotent re-runs).
		var exists int
		err = db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM entries WHERE project_id = ? AND file_path = ?`,
			p.ProjectID, absPath,
		).Scan(&exists)
		if err != nil {
			return fmt.Errorf("check existing %q: %w", absPath, err)
		}
		if exists > 0 {
			result.Skipped++
			return nil
		}

		// Extract title from filename stem.
		stem := filepath.Base(path)
		stem = strings.TrimSuffix(stem, filepath.Ext(stem))
		title := strings.Title(strings.NewReplacer("-", " ", "_", " ").Replace(stem)) //nolint:staticcheck // strings.Title is deprecated but matches the expected title-case behavior here

		// Extract summary from file content.
		summary := extractMarkdownSummary(absPath, path)

		// Resolve topic.
		topic := p.Topic
		if topic == "" {
			topic = strings.ToLower(stem)
		}

		// Resolve kind.
		kind := p.Kind
		if kind == "" {
			kind = inferKindFromPath(absPath)
		}

		// Resolve valid_days from kind.
		validDays := kindValidDays(kind)

		tags := strings.TrimSpace(p.Tags)

		_, err = db.ExecContext(ctx,
			`INSERT INTO entries
			   (project_id, topic, kind, title, summary, tags, file_path,
			    status, valid_days, needs_review, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.ProjectID,
			topic,
			string(kind),
			title,
			summary,
			nullIfEmpty(tags),
			absPath,
			string(StatusImported),
			nullIfNilInt(validDays),
			0,
			now.Format(time.RFC3339),
			now.Format(time.RFC3339),
		)
		if err != nil {
			return fmt.Errorf("insert %q: %w", absPath, err)
		}
		result.Imported++
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("lore: catalog: walk %q: %w", p.Dir, err)
	}

	return result, nil
}

// extractMarkdownSummary reads the file at absPath and extracts the first
// substantial paragraph as a summary. Skips YAML frontmatter and heading
// lines.
func extractMarkdownSummary(absPath, relPath string) string {
	content, err := os.ReadFile(absPath)
	if err != nil {
		return "Imported from " + relPath
	}
	text := string(content)

	// Skip YAML frontmatter (--- ... ---).
	if strings.HasPrefix(text, "---") {
		end := strings.Index(text[3:], "---")
		if end >= 0 {
			text = strings.TrimSpace(text[3+end+3:])
		}
	}

	// Collect body lines (skip headings and empty lines until we have a
	// substantial paragraph).
	lines := strings.Split(strings.TrimSpace(text), "\n")
	var bodyLines []string
	for _, line := range lines {
		stripped := strings.TrimSpace(line)
		if strings.HasPrefix(stripped, "#") {
			continue
		}
		if stripped == "" {
			if len(bodyLines) > 0 && len(strings.Join(bodyLines, " ")) > 40 {
				break // stop after first substantial paragraph
			}
			continue
		}
		bodyLines = append(bodyLines, stripped)
	}

	summary := strings.Join(bodyLines, " ")
	if len(summary) > 300 {
		summary = summary[:300]
	}
	if summary == "" {
		summary = "Imported from " + relPath
	}
	return summary
}

// inferKindFromPath returns KindDecision when the path contains "decision",
// "adr", or "design" (case-insensitive); otherwise returns KindResearch.
func inferKindFromPath(path string) Kind {
	lower := strings.ToLower(path)
	if strings.Contains(lower, "decision") ||
		strings.Contains(lower, "adr") ||
		strings.Contains(lower, "design") {
		return KindDecision
	}
	return KindResearch
}
