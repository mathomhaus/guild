// bloat_warn_test.go — inscribe-time bloat warning for long principles.
//
// Principles auto-load at session start, so long ones are expensive. The
// CLI warns at write time when a principle's combined title + summary
// exceeds 60 words; the entry still persists. This test exercises all three
// paths:
//
//   - A 70+-word principle produces a stderr warning referencing "60" and
//     still exits 0 with the entry persisted.
//   - A ≤60-word principle inscribes cleanly with no warning.
//   - Non-principle kinds (research, decision, etc.) never trigger the
//     warning regardless of word count.
package integration_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestBloatWarn_LongPrinciple verifies that inscribing a principle whose
// combined title+summary exceeds 60 words:
//  1. Exits 0 (entry persists)
//  2. Prints a warning on stderr that references "60" (the word limit)
//  3. The stdout still shows the "inscribed ENTRY-N" success line
func TestBloatWarn_LongPrinciple(t *testing.T) {
	ctx := context.Background()
	homeDir := t.TempDir()
	projDir := filepath.Join(homeDir, "bloat-proj")
	_ = initProject(ctx, t, homeDir, projDir)

	// Build a 70-word principle: title ~10 words + summary ~63 words = 73 words total.
	// This is clearly above the 60-word threshold.
	title := "long principle title that already uses ten distinct words for the test"
	// Exactly 63-word summary to ensure we exceed 60 combined.
	summary := "principles bloat the session-start oath wall when their combined title and summary word count exceeds sixty words which happens frequently when agents encode policy as verbose prose rather than as short memorable behavioral rules that actually fit into a session context window without burning tokens on every single session start call"

	// Count words to confirm our fixture is >60.
	wordCount := len(strings.Fields(title)) + len(strings.Fields(summary))
	t.Logf("fixture word count: %d (want >60)", wordCount)
	if wordCount <= 60 {
		t.Fatalf("test fixture has only %d words — need >60 to trigger bloat warning", wordCount)
	}

	inv := inscribe(ctx, t, homeDir, projDir, title, "principle", summary, "hygiene")

	// Must exit 0 — the entry is still written even when warned.
	assertExitOK(t, inv, "long principle inscribe")

	t.Logf("stdout: %s", inv.Stdout)
	t.Logf("stderr: %s", inv.Stderr)

	// The stderr warning MUST contain "60" (the word-limit reference).
	assertContains(t, inv.Stderr, "60",
		"bloat warning on stderr must reference the 60-word threshold")

	// The stdout must confirm the entry was inserted.
	assertContains(t, inv.Stdout, "inscribed",
		"stdout must show inscribed success even when bloat warned")
}

// TestBloatWarn_ShortPrinciple verifies that a ≤60-word principle inscribes
// cleanly with no warning on stderr.
func TestBloatWarn_ShortPrinciple(t *testing.T) {
	ctx := context.Background()
	homeDir := t.TempDir()
	projDir := filepath.Join(homeDir, "no-bloat-proj")
	_ = initProject(ctx, t, homeDir, projDir)

	// A short principle: title + summary well under 60 words.
	title := "short principle stays clean"
	summary := "Concise principles load fast. Under sixty words total."

	wordCount := len(strings.Fields(title)) + len(strings.Fields(summary))
	t.Logf("fixture word count: %d (want ≤60)", wordCount)
	if wordCount > 60 {
		t.Fatalf("test fixture has %d words — expected ≤60 for this test", wordCount)
	}

	inv := inscribe(ctx, t, homeDir, projDir, title, "principle", summary, "hygiene")
	assertExitOK(t, inv, "short principle inscribe")

	// No bloat warning expected.
	assertNotContains(t, inv.Stderr, "60",
		"no bloat warning on stderr for short principle")
	assertNotContains(t, inv.Stderr, "principle exceeds",
		"no principle-exceeds warning for short principle")
	assertContains(t, inv.Stdout, "inscribed", "stdout must show inscribed success")
}

// TestBloatWarn_NonPrinciple verifies that a >60-word entry of kind ≠
// principle does NOT trigger the oath-hygiene warning.
func TestBloatWarn_NonPrinciple(t *testing.T) {
	ctx := context.Background()
	homeDir := t.TempDir()
	projDir := filepath.Join(homeDir, "nonprin-proj")
	_ = initProject(ctx, t, homeDir, projDir)

	// 70+ word decision entry — should inscribe without warning.
	title := "architectural decision that is deliberately verbose to exceed sixty words in the combined title and summary for testing"
	summary := "This is a decision entry not a principle entry. The sixty-word oath-hygiene check only fires for kind-equals-principle. A decision can be as long as needed to fully capture the context and rationale of the architectural choice."

	wordCount := len(strings.Fields(title)) + len(strings.Fields(summary))
	t.Logf("fixture word count: %d", wordCount)

	inv := inscribe(ctx, t, homeDir, projDir, title, "decision", summary, "architecture")
	assertExitOK(t, inv, "long decision inscribe")

	// Non-principle kinds must NOT get the oath-hygiene warning.
	assertNotContains(t, inv.Stderr, "principle exceeds",
		"non-principle kind must not receive oath-hygiene warning")
	assertContains(t, inv.Stdout, "inscribed", "stdout must show inscribed success")
}

// TestBloatWarn_NoWarnFlag verifies that --no-warn suppresses the ≤60-word
// check entirely for principles.
func TestBloatWarn_NoWarnFlag(t *testing.T) {
	ctx := context.Background()
	homeDir := t.TempDir()
	projDir := filepath.Join(homeDir, "nowarn-proj")
	_ = initProject(ctx, t, homeDir, projDir)

	title := "suppressed bloat warning via no-warn flag test entry title"
	summary := "principles bloat the oath wall when their combined title and summary exceeds sixty words which happens frequently when agents encode policy as verbose prose rather than as short memorable behavioral rules that actually fit into the session context window without burning tokens on every single session start call across all projects in the workspace"

	wordCount := len(strings.Fields(title)) + len(strings.Fields(summary))
	t.Logf("fixture word count: %d (want >60)", wordCount)
	if wordCount <= 60 {
		t.Fatalf("no-warn fixture has only %d words — needs >60 to meaningfully test --no-warn suppression", wordCount)
	}

	// Use --no-warn flag.
	inv := runArgs(ctx, t, homeDir, projDir, []string{
		"lore", "inscribe", title,
		"--kind", "principle",
		"--summary", summary,
		"--topic", "hygiene",
		"--no-warn",
	})
	assertExitOK(t, inv, "long principle with --no-warn")

	// With --no-warn, stderr must be quiet (no bloat warning).
	assertNotContains(t, inv.Stderr, "principle exceeds",
		"--no-warn must suppress the bloat warning")
	assertContains(t, inv.Stdout, "inscribed", "stdout must confirm insert with --no-warn")
}

// TestBloatWarn_OnlyOneWarning verifies that the inscribe-time ⚠️ warning
// is the only warning surfaced for an over-length principle — the
// hints-engine `principle-too-long` rule has been removed in favor of
// the inline warning. Regression guard for #52.
func TestBloatWarn_OnlyOneWarning(t *testing.T) {
	ctx := context.Background()
	homeDir := t.TempDir()
	projDir := filepath.Join(homeDir, "single-warn-proj")
	_ = initProject(ctx, t, homeDir, projDir)

	// Same fixture as TestBloatWarn_LongPrinciple — guaranteed >60 words.
	title := "long principle title that already uses ten distinct words for the test"
	summary := "principles bloat the session-start oath wall when their combined title and summary word count exceeds sixty words which happens frequently when agents encode policy as verbose prose rather than as short memorable behavioral rules that actually fit into a session context window without burning tokens on every single session start call"

	inv := inscribe(ctx, t, homeDir, projDir, title, "principle", summary, "hygiene")
	assertExitOK(t, inv, "long principle inscribe (single-warning)")

	// Exactly one ⚠️ "principle exceeds" line on stderr.
	exceedCount := strings.Count(inv.Stderr, "principle exceeds")
	if exceedCount != 1 {
		t.Errorf("stderr contains %d 'principle exceeds' lines, want exactly 1\nstderr:\n%s",
			exceedCount, inv.Stderr)
	}

	// No hints-engine principle-too-long fire on either channel.
	for _, name := range []string{"stderr", "stdout"} {
		stream := inv.Stderr
		if name == "stdout" {
			stream = inv.Stdout
		}
		if strings.Contains(stream, "principle-too-long") {
			t.Errorf("%s mentions hints-engine rule 'principle-too-long' (rule was removed):\n%s",
				name, stream)
		}
	}
}
