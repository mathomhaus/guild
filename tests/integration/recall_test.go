// recall_test.go — regression guard for recall@1 = 100% on exact-title queries.
//
// Asserts that BM25 + recency + title-boost ranks every fixture entry at
// position 1 when queried by its verbatim title. Runs end-to-end against the
// compiled binary and real SQLite FTS5 so scoring, storage, and CLI output
// are all exercised.
//
// Corpus: 32 entries from fixtures.RecallCorpus, each with a distinctive
// multi-word title. The test fails if any exact-title query fails to land
// at rank 1 — hard 100% bar, not "mostly".
package integration_test

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mathomhaus/guild/tests/integration/fixtures"
)

// TestRecall_ExactTitleRank1 seeds 32 entries and verifies that querying each
// one by verbatim title returns it at rank 1 (first appraise result).
//
// Log line format: "recall@1: N/32 (100%)" so CI output is parseable.
func TestRecall_ExactTitleRank1(t *testing.T) {
	ctx := context.Background()
	homeDir := t.TempDir()
	projDir := filepath.Join(homeDir, "recall-proj")

	_ = initProject(ctx, t, homeDir, projDir)

	corpus := fixtures.RecallCorpus
	t.Logf("seeding %d entries for recall@1 test", len(corpus))

	// Seed all entries.
	for i, e := range corpus {
		inv := inscribe(ctx, t, homeDir, projDir, e.Title, e.Kind, e.Summary, e.Topic)
		if inv.ExitCode != 0 {
			t.Fatalf("seed entry %d %q: exit %d\nstdout: %s\nstderr: %s",
				i, e.Title, inv.ExitCode, inv.Stdout, inv.Stderr)
		}
		t.Logf("seeded ENTRY-%d: %s", i+1, e.Title)
	}

	// Query each entry by its verbatim title and record hits.
	hits := 0
	misses := 0
	for i, e := range corpus {
		inv := appraise(ctx, t, homeDir, projDir, e.Title, "--limit", "1")
		if inv.ExitCode != 0 {
			t.Logf("MISS [%d/%d] appraise exit=%d for %q:\n  stdout: %s\n  stderr: %s",
				i+1, len(corpus), inv.ExitCode, e.Title, inv.Stdout, inv.Stderr)
			misses++
			continue
		}

		// The header echoes the query even when no matching entry exists.
		// Only the first result row can establish a rank-one hit.
		if firstAppraiseTitle(inv.Stdout) == normalizeTitle(e.Title) {
			hits++
			t.Logf("HIT  [%d/%d] %q", i+1, len(corpus), e.Title)
		} else {
			misses++
			t.Logf("MISS [%d/%d] %q\n  appraise output: %s",
				i+1, len(corpus), e.Title, inv.Stdout)
		}
	}

	total := len(corpus)
	pct := 100.0 * float64(hits) / float64(total)
	t.Logf("recall@1: %d/%d (%.0f%%)", hits, total, pct)
	fmt.Printf("=== recall@1: %d/%d (%.0f%%) ===\n", hits, total, pct)

	// Hard requirement: 100% — every entry must be at rank 1.
	if misses > 0 {
		t.Errorf("recall@1 = %d/%d: %d exact-title queries missed rank-1 (want 0 misses)",
			hits, total, misses)
	}
}

var appraiseResultRow = regexp.MustCompile(`^[ \t]*(?:LORE|ENTRY)-[0-9]+[ \t]+\[[^\]\r\n]*\][ \t]*([^\r\n]*)$`)

func firstAppraiseTitle(output string) string {
	lines := strings.Split(output, "\n")
	for i, line := range lines {
		match := appraiseResultRow.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		if match[1] != "" {
			return normalizeTitle(match[1])
		}
		// The legacy CLI puts the title on the next line; the command
		// registry and MCP render it on the same line as the metadata.
		if i+1 < len(lines) {
			return normalizeTitle(lines[i+1])
		}
		return ""
	}
	return ""
}

func normalizeTitle(title string) string {
	return strings.ToLower(normalizeWS(title))
}

func TestRecallResultParsing_RejectsQueryEcho(t *testing.T) {
	const title = "Durable retry policy"
	for _, output := range []string{
		`nothing found for "Durable retry policy"`,
		"1 result(s) for \"Durable retry policy\":\n  LORE-2 [example/decision · current · 1d ago]  Other policy\n  Durable retry policy is mentioned in the summary",
		"2 result(s) for \"Durable retry policy\":\n  LORE-2 [example/decision] Other policy\n  LORE-1 [example/decision] Durable retry policy",
	} {
		if firstAppraiseTitle(output) == normalizeTitle(title) {
			t.Errorf("query echo or lower-ranked title counted as rank one: %q", output)
		}
	}
	output := "1 result(s):\n  LORE-1 [example/decision · current · 1d ago]  Durable retry policy\n  Summary"
	if got := firstAppraiseTitle(output); got != normalizeTitle(title) {
		t.Errorf("first result title = %q, want %q", got, normalizeTitle(title))
	}
	output = "1 entry(ies) appraised:\n  LORE-1 [example/decision · current · today]\n  Durable retry policy\n  Summary"
	if got := firstAppraiseTitle(output); got != normalizeTitle(title) {
		t.Errorf("multiline first result title = %q, want %q", got, normalizeTitle(title))
	}
}

// normalizeWS collapses runs of whitespace to a single space and trims
// leading/trailing whitespace.
func normalizeWS(s string) string {
	// Replace all whitespace runs with a single space.
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}
