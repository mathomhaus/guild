package e2e

// Golden transcripts.
//
// Each scenario records the deterministic surface of its MCP exchange
// into a plain-text transcript, runs a scrub pass over it (timestamps,
// versions, embedder progress, and other legitimate nondeterminism are
// replaced with stable placeholders), and compares the result against
// test/e2e/golden/<scenario>.golden byte-for-byte.
//
// Why bytes and not substring assertions: the daemon program needs to
// prove that putting a daemon between the client and the state changes
// nothing observable. Byte-identical scrubbed transcripts across modes
// is that proof. Substring checks would let envelope drift through.
//
// Regeneration: GUILD_E2E_UPDATE=1 (or `make e2e-update`) rewrites the
// goldens from a live run. Review the diff like any other code change.

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// transcript accumulates one scenario's recorded exchange.
type transcript struct {
	b strings.Builder
	// scrubIDs additionally normalizes ENTRY-N / QUEST-N / LORE-N ids.
	// Off for the baseline scenario (ids are deterministic from a fresh
	// state and worth pinning); on for the concurrency scenario (id
	// assignment depends on goroutine interleaving).
	scrubIDs bool
}

// step records one labeled exchange. The body is indented so transcript
// structure survives any leading/trailing whitespace in tool output.
func (tr *transcript) step(label, body string) {
	fmt.Fprintf(&tr.b, "=== %s\n", label)
	body = strings.TrimRight(body, "\n")
	for _, line := range strings.Split(body, "\n") {
		fmt.Fprintf(&tr.b, "  %s\n", strings.TrimRight(line, " \t"))
	}
}

// scrubRule rewrites one class of nondeterminism.
type scrubRule struct {
	re  *regexp.Regexp
	rep string
}

// scrubRules covers the nondeterminism observed in real guild output.
// Order matters: line-level rules before token rules, timestamps before
// bare dates, ages before durations.
var scrubRules = []scrubRule{
	// onnxruntime emits platform-dependent stderr noise during the
	// embedder probe (e.g. a cpuid warning inside arm64 containers that
	// never appears on amd64). Drop such lines entirely so one golden
	// serves macOS-hosted and CI-hosted runs.
	{regexp.MustCompile(`(?m)^[ \t]*onnxruntime [^\n]*\n`), ""},
	// Embedder progress depends on backfill timing and hardware speed:
	// "embedder: backfilling (coverage 40%, ETA ~2s)" vs "embedder: ready".
	{regexp.MustCompile(`(?m)^(\s*)embedder: .*$`), "${1}embedder: <EMBEDDER-STATUS>"},
	// Probe cosine similarity is float math on whatever SIMD path the
	// host CPU exposes; pin the shape, not the digits.
	{regexp.MustCompile(`cosine=\d+(\.\d+)?`), "cosine=<COSINE>"},
	// RFC3339-ish timestamps, with or without seconds/zone: 2026-06-12T19:10:33Z.
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?(\.\d+)?(Z|[+-]\d{2}:?\d{2})?\b`), "<TIMESTAMP>"},
	// Bare dates: 2026-06-12.
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`), "<DATE>"},
	// Wall-clock times: 19:10 or 19:10:33.
	{regexp.MustCompile(`\b\d{2}:\d{2}(:\d{2})?\b`), "<TIME>"},
	// Version stamps: v0.3.2, 0.1.0-dev, v0.3.1-17-g890abf8.
	{regexp.MustCompile(`\bv?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?\b`), "<VERSION>"},
	// Relative ages: "3d ago", "today" stays (deterministic within a run).
	{regexp.MustCompile(`\b\d+[smhd] ago\b`), "<AGE> ago"},
	// Durations: 12ms, 3.4ms, 1.2s, 1m30s.
	{regexp.MustCompile(`\b(\d+h)?(\d+m)?\d+(\.\d+)?(ns|µs|us|ms|s)\b`), "<DUR>"},
	// Process ids: "pid 1234".
	{regexp.MustCompile(`\bpid \d+\b`), "pid <PID>"},
}

var idScrubRules = []scrubRule{
	{regexp.MustCompile(`\bENTRY-\d+\b`), "ENTRY-<N>"},
	{regexp.MustCompile(`\bLORE-\d+\b`), "LORE-<N>"},
	{regexp.MustCompile(`\bQUEST-\d+\b`), "QUEST-<N>"},
}

// daemonOnlyLinePrefixes is the closed, documented allowlist of output line
// prefixes that exist ONLY when a session is served by a running daemon (the
// goldens are recorded in direct mode, where no such line is emitted). Phase
// 3 introduces the first one: the guild_session_start presence line ("N
// agents active"), which the in-process path cannot produce because no
// session registry exists there. A daemon-mode run strips these lines before
// comparing, so the single shared golden serves both modes; in direct mode
// the strip is a no-op (the line never appears), keeping the no-daemon
// transcript a strict match. Everything else stays byte-compared, so any
// other daemon-induced drift still fails the gate, and a future additive
// daemon-only line must register its prefix here consciously rather than
// slip past. The 👥 prefix is the presence line's stable marker. Indentation
// is stripped before the prefix check because step() indents every body
// line by two spaces.
var daemonOnlyLinePrefixes = []string{
	"👥 ",
}

// stripDaemonOnlyLines drops every line whose trimmed text begins with an
// allowlisted daemon-only prefix.
func stripDaemonOnlyLines(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		drop := false
		for _, prefix := range daemonOnlyLinePrefixes {
			if strings.HasPrefix(trimmed, prefix) {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// scrubbed returns the transcript text after the scrub pass.
func (tr *transcript) scrubbed() string {
	out := tr.b.String()
	for _, r := range scrubRules {
		out = r.re.ReplaceAllString(out, r.rep)
	}
	if tr.scrubIDs {
		for _, r := range idScrubRules {
			out = r.re.ReplaceAllString(out, r.rep)
		}
	}
	return stripDaemonOnlyLines(out)
}

// fingerprint summarizes a large deterministic blob (the INSTRUCTIONS
// string) as length + sha256 so the golden pins it without inlining
// kilobytes of contract text.
func fingerprint(s string) string {
	return fmt.Sprintf("%d bytes, sha256:%x", len(s), sha256.Sum256([]byte(s)))
}

// goldenPath resolves test/e2e/golden/<name>.golden relative to this
// package (go test runs with the package dir as cwd).
func goldenPath(name string) string {
	return filepath.Join("golden", name+".golden")
}

// compareGolden asserts the scrubbed transcript matches the committed
// golden, or rewrites the golden when GUILD_E2E_UPDATE=1.
func compareGolden(t *testing.T, name string, tr *transcript) {
	t.Helper()
	got := tr.scrubbed()
	path := goldenPath(name)

	if suite.update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		t.Logf("golden updated: %s (%d bytes)", path, len(got))
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v\n(run `make e2e-update` to generate it)", path, err)
	}
	if got == string(want) {
		return
	}
	t.Errorf("transcript diverges from golden %s\n%s\n(if the change is intentional, run `make e2e-update` and review the diff)",
		path, firstDiff(string(want), got))
}

// firstDiff renders the first differing line with context, enough to
// debug a transcript mismatch from CI logs without artifacts.
func firstDiff(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	n := len(wantLines)
	if len(gotLines) < n {
		n = len(gotLines)
	}
	for i := 0; i < n; i++ {
		if wantLines[i] != gotLines[i] {
			return fmt.Sprintf("first divergence at line %d:\n  golden: %q\n  got:    %q",
				i+1, wantLines[i], gotLines[i])
		}
	}
	return fmt.Sprintf("transcripts share %d lines, then lengths diverge: golden %d lines, got %d lines\n  next: %q",
		n, len(wantLines), len(gotLines), nextLine(wantLines, gotLines, n))
}

func nextLine(wantLines, gotLines []string, n int) string {
	if len(wantLines) > n {
		return "golden has: " + wantLines[n]
	}
	if len(gotLines) > n {
		return "got has: " + gotLines[n]
	}
	return ""
}

// TestDaemonOnlyLineStripIsLoadBearing proves the daemon-only-line carve-out
// in the scrub does real work and would fail if removed while the line still
// renders. It builds two transcripts that differ ONLY by the daemon-mode
// presence line (the direct-mode golden has no such line) and asserts:
//
//   - the raw bodies genuinely diverge (the presence line is a real diff), and
//   - after the daemon-only strip the daemon-mode body equals the direct-mode
//     body, so the single shared golden serves both modes.
//
// This guard does not need docker, so it runs in a plain `go test`.
func TestDaemonOnlyLineStripIsLoadBearing(t *testing.T) {
	const presencePrefix = "👥 "
	covered := false
	for _, p := range daemonOnlyLinePrefixes {
		if p == presencePrefix {
			covered = true
			break
		}
	}
	if !covered {
		t.Fatalf("presence prefix %q is not in daemonOnlyLinePrefixes; the carve-out is missing", presencePrefix)
	}

	// step() indents body lines by two spaces; mirror that so the trimmed
	// prefix check is exercised exactly as it is on real transcripts.
	directBody := "  📍 active project: e2eproj\n  📊 board: 0 oaths, 0 bounties, 0 echoes\n"
	daemonBody := "  📍 active project: e2eproj\n  📊 board: 0 oaths, 0 bounties, 0 echoes\n  " + presencePrefix + "1 agent active\n"

	if directBody == daemonBody {
		t.Fatal("test bodies are identical; the presence line is not actually a difference")
	}
	if got, want := stripDaemonOnlyLines(daemonBody), stripDaemonOnlyLines(directBody); got != want {
		t.Fatalf("strip did not reconcile the daemon-only line:\ndirect: %q\ndaemon: %q", want, got)
	}
}
