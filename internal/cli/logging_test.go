package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestParseCLILogLevel verifies the mapping from GUILD_LOG_LEVEL string
// values to slog.Level constants.
func TestParseCLILogLevel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  string
	}{
		{"debug", "DEBUG"},
		{"DEBUG", "DEBUG"},
		{"info", "INFO"},
		{"INFO", "INFO"},
		{"warn", "WARN"},
		{"warning", "WARN"},
		{"WARN", "WARN"},
		{"error", "ERROR"},
		{"ERROR", "ERROR"},
		{"", "WARN"},         // default
		{"verbose", "WARN"},  // unrecognized falls to WARN
		{"  warn  ", "WARN"}, // trimmed
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got := parseCLILogLevel(tc.input).String()
			if got != tc.want {
				t.Errorf("parseCLILogLevel(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestNewCLILoggerTo_WarnDefault verifies that at the default level (Warn),
// a Debug-level log line produces no output. This simulates the warm-start
// fast-path: the demoted lines must be invisible to scripted agents.
func TestNewCLILoggerTo_WarnDefault(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := newCLILoggerTo(&buf, "") // empty = default WARN

	logger.Debug("embedder warm-start, probe skipped", "reason", "identity_match")
	logger.Debug("embedder warm-start complete", "reason", "identity_match")

	if got := strings.TrimSpace(buf.String()); got != "" {
		t.Errorf("at default (WARN) level, debug lines should produce no output; got:\n%s", got)
	}
}

// TestNewCLILoggerTo_DebugLevel verifies that at GUILD_LOG_LEVEL=debug the
// demoted warm-start lines reappear. The test checks that both expected
// message strings are present in the output.
func TestNewCLILoggerTo_DebugLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := newCLILoggerTo(&buf, "debug")

	logger.Debug("embedder warm-start, probe skipped", "reason", "identity_match")
	logger.Debug("embedder warm-start complete", "reason", "identity_match")

	out := buf.String()
	for _, want := range []string{
		"embedder warm-start, probe skipped",
		"embedder warm-start complete",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("at debug level, expected %q in output; got:\n%s", want, out)
		}
	}
}
