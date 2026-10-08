package cli

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCLI_Inscribe_ValidDaysConfig_EchoTiming is the end-to-end check
// that [inscribe.valid_days] in config.toml reaches the write path: an
// entry inscribed before the config change keeps the built-in research
// window (30d), an entry inscribed after stamps the configured window
// (1d), and `lore echoes` flags only the configured one once both are
// two days old.
func TestCLI_Inscribe_ValidDaysConfig_EchoTiming(t *testing.T) {
	db, home := cliSetup(t, "alpha")
	t.Setenv("GUILD_NO_EMOJI", "1")
	t.Setenv("GUILD_NO_USAGE_LOG", "1")
	ctx := context.Background()

	// Entry A: inscribed with no valid_days config anywhere, so the
	// built-in research default (30d) applies.
	if _, errOut, err := execCmd(t,
		"lore", "inscribe", "entry stamped before the window change",
		"--project", "alpha",
		"--kind", "research",
		"--summary", "stamped under the built-in thirty day window",
		"--topic", "validity",
	); err != nil {
		t.Fatalf("inscribe before config: %v (stderr=%q)", err, errOut.String())
	}

	// Operator tightens the research window to 1 day in the user-wide
	// config. The CLI Deps read config lazily per invocation, so the
	// next inscribe must observe this file.
	cfgPath := filepath.Join(home, ".guild", "config.toml")
	if err := os.WriteFile(cfgPath,
		[]byte("[inscribe.valid_days]\nresearch = 1\n"), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}

	// Entry B: inscribed after the config change, stamps the 1-day window.
	if _, errOut, err := execCmd(t,
		"lore", "inscribe", "entry stamped after the window change",
		"--project", "alpha",
		"--kind", "research",
		"--summary", "stamped under the configured one day window",
		"--topic", "validity",
	); err != nil {
		t.Fatalf("inscribe after config: %v (stderr=%q)", err, errOut.String())
	}

	// Stored windows: A keeps 30 (stamped value is immutable; config
	// changes never rewrite existing rows), B carries the configured 1.
	storedValidDays := func(id int64) *int {
		t.Helper()
		var v sql.NullInt64
		if err := db.QueryRowContext(ctx,
			`SELECT valid_days FROM entries WHERE id = ?`, id).Scan(&v); err != nil {
			t.Fatalf("query valid_days for %d: %v", id, err)
		}
		if !v.Valid {
			return nil
		}
		n := int(v.Int64)
		return &n
	}
	if got := storedValidDays(1); got == nil || *got != 30 {
		t.Fatalf("entry 1 valid_days: got %v want 30 (built-in default)", got)
	}
	if got := storedValidDays(2); got == nil || *got != 1 {
		t.Fatalf("entry 2 valid_days: got %v want 1 (configured window)", got)
	}

	// Age both entries by two days: past the configured 1-day window,
	// well inside the built-in 30-day one.
	twoDaysAgo := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	if _, err := db.ExecContext(ctx,
		`UPDATE entries SET created_at = ?`, twoDaysAgo); err != nil {
		t.Fatalf("backdate entries: %v", err)
	}

	out, errOut, err := execCmd(t, "lore", "echoes", "--project", "alpha")
	if err != nil {
		t.Fatalf("echoes: %v (stderr=%q)", err, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "LORE-2") || !strings.Contains(got, "valid: 1d") {
		t.Errorf("echoes should flag LORE-2 with its 1-day window, got:\n%s", got)
	}
	if strings.Contains(got, "LORE-1") {
		t.Errorf("echoes must not flag LORE-1 (30-day window, 2 days old), got:\n%s", got)
	}
}

// TestCLI_Inscribe_ValidDaysConfig_ZeroConfigDefault pins zero-config
// behavior: with no config file at all, inscribe stamps the built-in
// kind defaults (research=30).
func TestCLI_Inscribe_ValidDaysConfig_ZeroConfigDefault(t *testing.T) {
	db, _ := cliSetup(t, "alpha")
	t.Setenv("GUILD_NO_EMOJI", "1")
	t.Setenv("GUILD_NO_USAGE_LOG", "1")

	if _, errOut, err := execCmd(t,
		"lore", "inscribe", "zero config research entry default window",
		"--project", "alpha",
		"--kind", "research",
		"--summary", "no config file exists for this home",
		"--topic", "validity",
	); err != nil {
		t.Fatalf("inscribe: %v (stderr=%q)", err, errOut.String())
	}

	var v sql.NullInt64
	if err := db.QueryRowContext(context.Background(),
		`SELECT valid_days FROM entries WHERE id = 1`).Scan(&v); err != nil {
		t.Fatalf("query valid_days: %v", err)
	}
	if !v.Valid || v.Int64 != 30 {
		t.Errorf("valid_days: got %+v want 30 (built-in research default)", v)
	}
}
