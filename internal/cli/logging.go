package cli

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// logLevelEnv names the environment variable that controls the minimum
// slog level for CLI invocations. Recognized values (case-insensitive):
// "debug", "info", "warn" (or "warning"), "error". Any unrecognized or
// empty value defaults to Warn so that scripted agents see only real
// signal on stderr (not warm-start diagnostics emitted on every call).
const logLevelEnv = "GUILD_LOG_LEVEL"

// newCLILogger returns a text-format slog.Logger writing to stderr,
// gated at the level read from GUILD_LOG_LEVEL (default: Warn).
//
// CLI callers (short-lived processes) use text format. The MCP server
// has its own GUILD_MCP_LOG_LEVEL / GUILD_MCP_LOG_FORMAT pair; see
// internal/mcp/logging.go.
func newCLILogger() *slog.Logger {
	return newCLILoggerTo(os.Stderr, os.Getenv(logLevelEnv))
}

// newCLILoggerTo is the injectable variant used by tests. Callers pass
// any io.Writer so tests can capture output without touching stderr.
func newCLILoggerTo(w io.Writer, level string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseCLILogLevel(level)}
	return slog.New(slog.NewTextHandler(w, opts))
}

// parseCLILogLevel maps a GUILD_LOG_LEVEL string to a slog.Level.
// Unrecognized values (including empty) default to Warn so CLI is quiet
// by default and only real state-transition events surface to the user.
func parseCLILogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "error":
		return slog.LevelError
	default: // "warn", "warning", "", or unrecognized
		return slog.LevelWarn
	}
}
