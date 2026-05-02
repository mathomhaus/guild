// embedder_outcome_test.go: regression tests for printEmbedderOutcome.
//
// Tests exercise the human-stdout format and structured slog fields
// for both the probe-pass and probe-fail paths (LORE-368 / QUEST-217).
// No ORT or dylib is needed: tests drive printEmbedderOutcome directly
// with canned InitOutcome values.

package install

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/lore/embed"
)

// captureLog returns a *slog.Logger whose JSON output is written to buf.
// JSON format makes key presence easy to assert with strings.Contains.
func captureLog(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// passingOutcome returns an InitOutcome that represents a successful probe.
// Timings and SHAs are non-zero so field presence is verifiable.
func passingOutcome() embed.InitOutcome {
	return embed.InitOutcome{
		State:                "enabled",
		Reason:               "ok",
		ProbeCosine:          1.0000,
		ProbeFloor:           0.999,
		ExtractDuration:      36 * time.Millisecond,
		DylibExtractDuration: 20 * time.Millisecond,
		ModelExtractDuration: 14 * time.Millisecond,
		VocabExtractDuration: 2 * time.Millisecond,
		ProbeDuration:        4 * time.Millisecond,
		DylibSHA256:          "aabbccddeeff00112233445566778899aabbccddeeff001122334455",
		ModelSHA256:          "bbccddee00112233445566778899aabbccddeeff001122334455aabb",
		VocabSHA256:          "ccddeeff112233445566778899aabbccddeeff001122334455aabbcc",
		Identity: embed.ManifestIdentity{
			ModelID:       "bge-small-en-v1.5-int8-cls",
			TokenizerHash: "deadbeef1234",
		},
	}
}

// failingOutcome returns an InitOutcome that represents a probe_mismatch.
func failingOutcome() embed.InitOutcome {
	return embed.InitOutcome{
		State:                "disabled",
		Reason:               "probe_mismatch",
		ProbeCosine:          0.712345,
		ProbeFloor:           0.999,
		ExtractDuration:      38 * time.Millisecond,
		DylibExtractDuration: 22 * time.Millisecond,
		ModelExtractDuration: 14 * time.Millisecond,
		VocabExtractDuration: 2 * time.Millisecond,
		ProbeDuration:        5 * time.Millisecond,
		DylibSHA256:          "aabbccddeeff00112233445566778899aabbccddeeff001122334455",
		ModelSHA256:          "bbccddee00112233445566778899aabbccddeeff001122334455aabb",
		VocabSHA256:          "ccddeeff112233445566778899aabbccddeeff001122334455aabbcc",
		Identity: embed.ManifestIdentity{
			ModelID:       "bge-small-en-v1.5-int8-cls",
			TokenizerHash: "deadbeef1234",
		},
	}
}

// ---------------------------------------------------------------------------
// Pass path
// ---------------------------------------------------------------------------

// TestPrintEmbedderOutcome_Pass_StdoutCompact verifies the success path
// emits exactly one line of human output containing cosine, extract ms,
// and probe ms. No multi-line block on success.
func TestPrintEmbedderOutcome_Pass_StdoutCompact(t *testing.T) {
	var stdout, logbuf bytes.Buffer
	outcome := passingOutcome()
	printEmbedderOutcome(&stdout, captureLog(&logbuf), outcome)

	out := stdout.String()

	// Single-line success line must be present.
	if !strings.Contains(out, "embedder enabled") {
		t.Errorf("stdout missing 'embedder enabled'; got:\n%s", out)
	}
	if !strings.Contains(out, "cosine=1.0000") {
		t.Errorf("stdout missing 'cosine=1.0000'; got:\n%s", out)
	}
	if !strings.Contains(out, "extract=36ms") {
		t.Errorf("stdout missing 'extract=36ms'; got:\n%s", out)
	}
	if !strings.Contains(out, "probe=4ms") {
		t.Errorf("stdout missing 'probe=4ms'; got:\n%s", out)
	}

	// Success must be one line only.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("success stdout should be exactly 1 line, got %d:\n%s", len(lines), out)
	}
}

// TestPrintEmbedderOutcome_Pass_SlogFields verifies all required structured
// fields appear in the JSON log for the pass path.
func TestPrintEmbedderOutcome_Pass_SlogFields(t *testing.T) {
	var stdout, logbuf bytes.Buffer
	printEmbedderOutcome(&stdout, captureLog(&logbuf), passingOutcome())
	log := logbuf.String()

	requiredFields := []string{
		`"probe_cosine_observed"`,
		`"probe_cosine_floor"`,
		`"extract_duration_ms_dylib"`,
		`"extract_duration_ms_model"`,
		`"extract_duration_ms_vocab"`,
		`"extract_duration_ms_total"`,
		`"probe_duration_ms"`,
		`"model_id"`,
		`"tokenizer_hash"`,
	}
	for _, f := range requiredFields {
		if !strings.Contains(log, f) {
			t.Errorf("slog missing field %s; log:\n%s", f, log)
		}
	}

	// State field must be "enabled".
	if !strings.Contains(log, `"enabled"`) {
		t.Errorf("slog missing state=enabled; log:\n%s", log)
	}
}

// ---------------------------------------------------------------------------
// Fail path (probe_mismatch)
// ---------------------------------------------------------------------------

// TestPrintEmbedderOutcome_Fail_StdoutTriageBlock verifies the failure path
// emits a triage block: state line + timings line + cosine line +
// sha256 line + identity line. All four extra lines must be present.
func TestPrintEmbedderOutcome_Fail_StdoutTriageBlock(t *testing.T) {
	var stdout, logbuf bytes.Buffer
	outcome := failingOutcome()
	printEmbedderOutcome(&stdout, captureLog(&logbuf), outcome)

	out := stdout.String()

	// State line.
	if !strings.Contains(out, "embedder disabled") {
		t.Errorf("stdout missing 'embedder disabled'; got:\n%s", out)
	}
	if !strings.Contains(out, "probe_mismatch") {
		t.Errorf("stdout missing 'probe_mismatch'; got:\n%s", out)
	}

	// Timings line: total + per-asset + probe.
	if !strings.Contains(out, "extract=38ms") {
		t.Errorf("stdout missing 'extract=38ms'; got:\n%s", out)
	}
	if !strings.Contains(out, "dylib=22ms") {
		t.Errorf("stdout missing 'dylib=22ms'; got:\n%s", out)
	}
	if !strings.Contains(out, "model=14ms") {
		t.Errorf("stdout missing 'model=14ms'; got:\n%s", out)
	}
	if !strings.Contains(out, "vocab=2ms") {
		t.Errorf("stdout missing 'vocab=2ms'; got:\n%s", out)
	}
	if !strings.Contains(out, "probe=5ms") {
		t.Errorf("stdout missing 'probe=5ms'; got:\n%s", out)
	}

	// Cosine line: observed vs floor.
	if !strings.Contains(out, "cosine observed=0.712345") {
		t.Errorf("stdout missing observed cosine; got:\n%s", out)
	}
	if !strings.Contains(out, "floor=0.999000") {
		t.Errorf("stdout missing floor cosine; got:\n%s", out)
	}

	// SHA fingerprint line: first 12 chars only.
	if !strings.Contains(out, "sha256:") {
		t.Errorf("stdout missing sha256 line; got:\n%s", out)
	}
	if !strings.Contains(out, "dylib=aabbccddeeff") {
		t.Errorf("stdout missing dylib sha prefix; got:\n%s", out)
	}
	if !strings.Contains(out, "model=bbccddee0011") {
		t.Errorf("stdout missing model sha prefix; got:\n%s", out)
	}
	if !strings.Contains(out, "vocab=ccddeeff1122") {
		t.Errorf("stdout missing vocab sha prefix; got:\n%s", out)
	}

	// Identity line.
	if !strings.Contains(out, "model_id=bge-small-en-v1.5-int8-cls") {
		t.Errorf("stdout missing model_id; got:\n%s", out)
	}
	if !strings.Contains(out, "tokenizer_hash=deadbeef1234") {
		t.Errorf("stdout missing tokenizer_hash; got:\n%s", out)
	}
}

// TestPrintEmbedderOutcome_Fail_SlogFields verifies all required structured
// fields appear in the JSON log for the fail path. Full-length SHAs in slog.
func TestPrintEmbedderOutcome_Fail_SlogFields(t *testing.T) {
	var stdout, logbuf bytes.Buffer
	printEmbedderOutcome(&stdout, captureLog(&logbuf), failingOutcome())
	log := logbuf.String()

	requiredFields := []string{
		`"probe_cosine_observed"`,
		`"probe_cosine_floor"`,
		`"extract_duration_ms_dylib"`,
		`"extract_duration_ms_model"`,
		`"extract_duration_ms_vocab"`,
		`"extract_duration_ms_total"`,
		`"probe_duration_ms"`,
		`"extracted_dylib_sha256"`,
		`"extracted_model_sha256"`,
		`"extracted_vocab_sha256"`,
		`"model_id"`,
		`"tokenizer_hash"`,
	}
	for _, f := range requiredFields {
		if !strings.Contains(log, f) {
			t.Errorf("slog missing field %s; log:\n%s", f, log)
		}
	}

	// Full SHA must appear in slog (not truncated).
	if !strings.Contains(log, "aabbccddeeff00112233445566778899aabbccddeeff001122334455") {
		t.Errorf("slog should contain full dylib SHA; log:\n%s", log)
	}

	// State must be "disabled".
	if !strings.Contains(log, `"disabled"`) {
		t.Errorf("slog missing state=disabled; log:\n%s", log)
	}
}

// TestPrintEmbedderOutcome_Fail_NoTriageBlockForOtherReasons verifies that
// the triage block does NOT appear for non-probe failure reasons (e.g.
// no_assets). Those paths have no cosine or SHA to show.
func TestPrintEmbedderOutcome_Fail_NoTriageBlockForOtherReasons(t *testing.T) {
	outcome := embed.InitOutcome{
		State:  "disabled",
		Reason: "no_assets",
	}
	var stdout bytes.Buffer
	printEmbedderOutcome(&stdout, slog.New(slog.NewTextHandler(io.Discard, nil)), outcome)

	out := stdout.String()
	if strings.Contains(out, "cosine") {
		t.Errorf("cosine triage line must not appear for no_assets; got:\n%s", out)
	}
	if strings.Contains(out, "sha256") {
		t.Errorf("sha256 triage line must not appear for no_assets; got:\n%s", out)
	}
	if !strings.Contains(out, "no_assets") {
		t.Errorf("stdout should name the reason; got:\n%s", out)
	}
}

// TestShaShort verifies truncation and passthrough.
func TestShaShort(t *testing.T) {
	full := "aabbccddeeff001122334455"
	if got := shaShort(full); got != "aabbccddeeff" {
		t.Errorf("shaShort(%q) = %q; want %q", full, got, "aabbccddeeff")
	}
	short := "abc"
	if got := shaShort(short); got != short {
		t.Errorf("shaShort(%q) = %q; want passthrough %q", short, got, short)
	}
}
