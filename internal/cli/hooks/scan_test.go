package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	hookcfg "github.com/mathomhaus/guild/internal/hooks"
	"github.com/mathomhaus/guild/internal/hooks/adapters"
)

// validatingAdapter is a file-backed test adapter that implements both
// adapters.Adapter and the scan-side validator interface. Its Validate
// mirrors the real harness adapters' dead-matcher contract: a matcher
// on a no-matcher event is a hard error, and a matcher that matches none
// of an event's closed vocabulary is a written-through-but-never-fires
// warning. The settings file lives under HOME (a t.TempDir in tests).
type validatingAdapter struct{ name string }

func (validatingAdapter) Detect() (bool, error)        { return true, nil }
func (validatingAdapter) Substitute(cmd string) string { return cmd }

func (a validatingAdapter) SettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".guild", a.name+"-settings.json"), nil
}

func (a validatingAdapter) Scan() ([]adapters.Hook, error) {
	path, err := a.SettingsPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // test-controlled temp path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return hookcfg.ScanSettingsDoc(raw, "hooks")
}

// Install/Sync are unused by scan; satisfy the interface only.
func (a validatingAdapter) Install(base adapters.Config) error { return nil }
func (a validatingAdapter) Sync(base adapters.Config) error    { return nil }

func (a validatingAdapter) Name() string { return a.name }

// closedVocab is the test adapter's matcher vocabulary, matching the
// real SessionStart source set.
var closedVocab = map[string][]string{
	"SessionStart": {"startup", "resume", "clear", "compact"},
}

// noMatcher marks events that reject a matcher entirely.
var noMatcher = map[string]bool{"UserPromptSubmit": true}

// Validate implements the scan-side validator interface.
func (validatingAdapter) Validate(cfg hookcfg.Config) (warnings []string, err error) {
	events := make([]string, 0, len(cfg))
	for ev := range cfg {
		events = append(events, ev)
	}
	sort.Strings(events)
	for _, ev := range events {
		for _, g := range cfg[ev] {
			if g.Matcher == "" {
				continue
			}
			if noMatcher[ev] {
				return warnings, fmt.Errorf("event %s does not support a matcher (got %q)", ev, g.Matcher)
			}
			re, rerr := regexp.Compile(g.Matcher)
			if rerr != nil {
				return warnings, fmt.Errorf("event %s: matcher %q is not a valid regular expression", ev, g.Matcher)
			}
			vocab, closed := closedVocab[ev]
			if !closed {
				continue
			}
			fires := false
			for _, v := range vocab {
				if re.MatchString(v) {
					fires = true
					break
				}
			}
			if !fires {
				warnings = append(warnings, fmt.Sprintf(
					"event %s: matcher %q matches none of the documented values (%s); this hook group will never fire",
					ev, g.Matcher, strings.Join(vocab, ", ")))
			}
		}
	}
	return warnings, nil
}

// newValidatingAdapter returns a validating adapter whose Name reports n.
func newValidatingAdapter(n string) adapters.Adapter { return validatingAdapter{name: n} }

// writeSettings writes a Claude-Code-shaped settings document with the
// given event groups under the validating adapter's settings path.
func writeSettings(t *testing.T, home, adapterName string, cfg hookcfg.Config) {
	t.Helper()
	dir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"hooks": cfg}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, adapterName+"-settings.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func validatingDeps(out *bytes.Buffer, ads ...adapters.Adapter) deps {
	return deps{adapters: ads, out: out}
}

// TestScan_FlagsDeadMatcher: a settings file carrying an unknown matcher
// value (one that parses as a regex but matches none of the event's
// documented vocabulary) produces a dead-matcher warning, surfaced
// through the adapter's Validate without scan knowing the harness name.
func TestScan_FlagsDeadMatcher(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ad := newValidatingAdapter("fakeharness")
	writeSettings(t, home, "fakeharness", hookcfg.Config{
		"SessionStart": {
			{Matcher: "frobnicate", Hooks: []hookcfg.Command{{Type: "command", Command: "guild quest brief --auto"}}},
		},
	})

	var out bytes.Buffer
	if err := runScan(validatingDeps(&out, ad), false, false); err != nil {
		t.Fatalf("runScan: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "frobnicate") || !strings.Contains(got, "never fire") {
		t.Errorf("scan did not surface the dead-matcher warning:\n%s", got)
	}
	// The hook itself is still inventoried.
	if !strings.Contains(got, "SessionStart [frobnicate]") {
		t.Errorf("scan dropped the hook from the inventory:\n%s", got)
	}
}

// TestScan_DeadMatcherInJSON: the warning rides along in machine output.
func TestScan_DeadMatcherInJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ad := newValidatingAdapter("fakeharness")
	writeSettings(t, home, "fakeharness", hookcfg.Config{
		"SessionStart": {
			{Matcher: "frobnicate", Hooks: []hookcfg.Command{{Type: "command", Command: "guild quest brief --auto"}}},
		},
	})

	var out bytes.Buffer
	if err := runScan(validatingDeps(&out, ad), false, true); err != nil {
		t.Fatalf("runScan --json: %v", err)
	}
	var reports []scanReport
	if err := json.Unmarshal(out.Bytes(), &reports); err != nil {
		t.Fatalf("scan --json not valid JSON: %v\n%s", err, out.String())
	}
	if len(reports) != 1 {
		t.Fatalf("got %d reports; want 1", len(reports))
	}
	if len(reports[0].Warnings) != 1 || !strings.Contains(reports[0].Warnings[0], "frobnicate") {
		t.Errorf("JSON report missing dead-matcher warning: %+v", reports[0])
	}
}

// TestScan_LiveMatcherNoWarning: a matcher that does fire on the
// documented vocabulary produces no warning.
func TestScan_LiveMatcherNoWarning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ad := newValidatingAdapter("fakeharness")
	writeSettings(t, home, "fakeharness", hookcfg.Config{
		"SessionStart": {
			{Matcher: "startup|resume", Hooks: []hookcfg.Command{{Type: "command", Command: "guild quest brief --auto"}}},
		},
	})

	var out bytes.Buffer
	if err := runScan(validatingDeps(&out, ad), false, false); err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if got := out.String(); strings.Contains(got, "never fire") || strings.Contains(got, "  ! ") {
		t.Errorf("scan emitted a spurious warning for a live matcher:\n%s", got)
	}
}

// TestScan_HardViolationSurfacedNotFatal: a matcher on a no-matcher
// event is a hard contract violation in Validate, but scan is a
// read-only inventory: it reports the violation as a warning line and
// still completes (and still lists the offending hook) rather than
// erroring out on a settings file it did not write.
func TestScan_HardViolationSurfacedNotFatal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ad := newValidatingAdapter("fakeharness")
	writeSettings(t, home, "fakeharness", hookcfg.Config{
		"UserPromptSubmit": {
			{Matcher: "startup", Hooks: []hookcfg.Command{{Type: "command", Command: "guild lore appraise --inject"}}},
		},
	})

	var out bytes.Buffer
	if err := runScan(validatingDeps(&out, ad), false, false); err != nil {
		t.Fatalf("runScan should not fail on a malformed existing file: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "does not support a matcher") {
		t.Errorf("scan did not surface the hard violation:\n%s", got)
	}
	if !strings.Contains(got, "UserPromptSubmit") {
		t.Errorf("scan dropped the offending hook:\n%s", got)
	}
}

// TestScan_NonValidatingAdapterNoWarnings: an adapter that does not
// implement validator contributes no warnings, and scan output is
// unchanged from the plain inventory.
func TestScan_NonValidatingAdapterNoWarnings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pre := []byte(`{
  "hooks": {
    "SessionStart": [
      {"matcher": "frobnicate", "hooks": [{"type": "command", "command": "guild quest brief --auto"}]}
    ]
  }
}`)
	if err := os.WriteFile(settingsPath(t, home), pre, 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	// testDeps wires the stub adapter, which has no Validate method.
	if err := runScan(testDeps(&out), false, false); err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if got := out.String(); strings.Contains(got, "  ! ") {
		t.Errorf("non-validating adapter produced a warning line:\n%s", got)
	}
}

// TestConfigFromHooks_DedupesAndSorts: the flattened-to-nested rebuild
// keys on (event, matcher), drops duplicates, and emits matchers in
// sorted order so Validate sees a deterministic view.
func TestConfigFromHooks_DedupesAndSorts(t *testing.T) {
	hs := []hookcfg.Hook{
		{Event: "SessionStart", Matcher: "resume", Command: "guild a"},
		{Event: "SessionStart", Matcher: "resume", Command: "guild b"}, // dup (event,matcher)
		{Event: "SessionStart", Matcher: "clear", Command: "guild c"},
		{Event: "PreCompact", Matcher: "auto", Command: "guild d"},
	}
	cfg := configFromHooks(hs)
	if len(cfg["SessionStart"]) != 2 {
		t.Fatalf("SessionStart groups = %d; want 2 (deduped)", len(cfg["SessionStart"]))
	}
	if cfg["SessionStart"][0].Matcher != "clear" || cfg["SessionStart"][1].Matcher != "resume" {
		t.Errorf("SessionStart matchers not sorted: %+v", cfg["SessionStart"])
	}
	if len(cfg["PreCompact"]) != 1 || cfg["PreCompact"][0].Matcher != "auto" {
		t.Errorf("PreCompact group wrong: %+v", cfg["PreCompact"])
	}
}
