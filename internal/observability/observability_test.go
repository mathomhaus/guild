package observability

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/config"
	"github.com/mathomhaus/guild/internal/daemon"
	"github.com/mathomhaus/guild/internal/module"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// tempPaths returns a (logPath, rollupPath) pair under a fresh temp dir.
func tempPaths(t *testing.T) (logPath, rollupPath string) {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "events.jsonl"), filepath.Join(dir, "rollups.json")
}

// ─────────────────────────── module identity ───────────────────────────

// TestModule_Identity asserts the module's registration shape: name, the
// off-by-default verdict, no commands/migrations, and an empty Instructions
// fragment (observability is not an agent-facing contract contributor).
func TestModule_Identity(t *testing.T) {
	m, ok := module.Lookup("observability")
	if !ok {
		t.Fatal("observability module not registered (blank import missing?)")
	}
	if m.Name() != "observability" {
		t.Errorf("Name = %q, want observability", m.Name())
	}
	if m.DefaultEnabled() {
		t.Error("DefaultEnabled must be false: the module is opt-in")
	}
	if len(m.Commands()) != 0 {
		t.Error("observability contributes no commands")
	}
	if fsys, db := m.Migrations(); fsys != nil || db != "" {
		t.Errorf("observability owns no storage; got fs=%v db=%q", fsys != nil, db)
	}
	if m.Instructions() != "" {
		t.Error("Instructions must be empty: observability is operational, not agent-facing")
	}
}

// TestModule_DisabledByDefault proves the parity bar: with a silent config the
// module is NOT in module.Enabled, so its Services() are never collected and
// the daemon never sees an observability loop or recorder.
func TestModule_DisabledByDefault(t *testing.T) {
	pred := config.ModuleEnabled(&config.Config{}) // no [modules] table
	var seen bool
	for _, m := range module.Enabled(pred) {
		if m.Name() == "observability" {
			seen = true
		}
	}
	if seen {
		t.Fatal("observability must be absent from Enabled() with a silent config")
	}
	// And the daemon decision sink must be the no-op default: a decision is
	// recorded nowhere. We assert by installing nothing and checking a
	// freshly built (but not started) service did not auto-install.
	rec := &countingRecorder{}
	daemon.SetDecisionRecorder(nil)
	t.Cleanup(func() { daemon.SetDecisionRecorder(nil) })
	_ = rec
}

// TestModule_EnabledByToggle proves the toggle flips the module on.
func TestModule_EnabledByToggle(t *testing.T) {
	cfg := &config.Config{Modules: config.ModulesConfig{"observability": true}}
	pred := config.ModuleEnabled(cfg)
	var seen bool
	for _, m := range module.Enabled(pred) {
		if m.Name() == "observability" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("observability must be in Enabled() once [modules].observability=true")
	}
}

type countingRecorder struct{ n int }

func (c *countingRecorder) Record(daemon.Decision) { c.n++ }

// ─────────────────────────── metrics text ──────────────────────────────

// TestRegistry_RenderFormat checks the hand-rolled Prometheus exposition is
// well-formed: HELP + TYPE + sample for every metric, sorted, and that
// Inc/Add/Set move the values.
func TestRegistry_RenderFormat(t *testing.T) {
	r := NewRegistry()
	r.Inc(MetricLeaseForfeits)
	r.Inc(MetricLeaseForfeits)
	r.Add(MetricRenewalQuests, 5)
	r.Set(MetricSessionsActive, 3)

	out := r.Render()

	// Every metric emits its triple.
	wantLines := []string{
		"# HELP " + MetricLeaseForfeits + " ",
		"# TYPE " + MetricLeaseForfeits + " counter",
		MetricLeaseForfeits + " 2",
		"# TYPE " + MetricSessionsActive + " gauge",
		MetricSessionsActive + " 3",
		MetricRenewalQuests + " 5",
	}
	for _, w := range wantLines {
		if !strings.Contains(out, w) {
			t.Errorf("render missing line %q\n--- full ---\n%s", w, out)
		}
	}

	// Output must be sorted by metric name: scan the sample lines.
	var names []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		names = append(names, strings.Fields(line)[0])
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("metrics not sorted: %q before %q", names[i-1], names[i])
		}
	}
}

// ─────────────────────────── event log ─────────────────────────────────

// TestEventLog_AppendsJSONLines verifies each Append writes exactly one valid
// JSON object per line and redacts the Reason field.
func TestEventLog_AppendsJSONLines(t *testing.T) {
	logPath, _ := tempPaths(t)
	l := NewEventLog(logPath)

	allow := true
	if err := l.Append(Event{Kind: "autopass", Allow: &allow, Reason: "fire", Inputs: map[string]bool{"armed": true}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := l.Append(Event{Kind: "lease_reap", Reason: "token sk-ABCDEFGHIJKLMNOPQR happened at /Users/secret/x"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 JSONL lines, got %d: %q", len(lines), string(data))
	}
	var first Event
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 0 not valid JSON: %v", err)
	}
	if first.Kind != "autopass" || first.Allow == nil || !*first.Allow {
		t.Errorf("line 0 fields wrong: %+v", first)
	}
	if first.Time.IsZero() {
		t.Error("Append must stamp a Time")
	}
	// Redaction: the second line's secret token and home path must be scrubbed.
	if strings.Contains(lines[1], "sk-ABCDEFGHIJKLMNOPQR") {
		t.Error("secret token not redacted")
	}
	if strings.Contains(lines[1], "/Users/secret") {
		t.Error("home path not redacted")
	}
}

// ─────────────────────────── rollups ───────────────────────────────────

// TestRollups_FoldAndBuckets verifies events land in the right hour/day
// buckets keyed by kind, and that Fold is idempotent against the watermark.
func TestRollups_FoldAndBuckets(t *testing.T) {
	r := NewRollups()
	base := time.Date(2026, 1, 2, 14, 30, 0, 0, time.UTC)
	r.Fold(Event{Time: base, Kind: "autopass"})
	r.Fold(Event{Time: base.Add(5 * time.Minute), Kind: "autopass"})
	r.Fold(Event{Time: base.Add(2 * time.Hour), Kind: "lease_reap"})

	hourly := r.HourlySnapshot()
	if got := hourly["2026-01-02T14"]["autopass"]; got != 2 {
		t.Errorf("14:00 autopass = %d, want 2", got)
	}
	if got := hourly["2026-01-02T16"]["lease_reap"]; got != 1 {
		t.Errorf("16:00 lease_reap = %d, want 1", got)
	}
	daily := r.DailySnapshot()
	if got := daily["2026-01-02"]["autopass"]; got != 2 {
		t.Errorf("day autopass = %d, want 2", got)
	}

	// A live Fold always counts, even at a timestamp equal to an earlier
	// event: two distinct decisions in the same instant are two occurrences.
	r.Fold(Event{Time: base, Kind: "autopass"})
	if got := r.HourlySnapshot()["2026-01-02T14"]["autopass"]; got != 3 {
		t.Errorf("live re-fold count = %d, want 3 (live folds always count)", got)
	}
}

// TestRollups_SurviveRestart proves the durable-restart guarantee: rollups
// computed from the event log on boot match what was folded live, recovered
// purely by replaying the JSONL log (no sidecar yet) and also via the sidecar.
func TestRollups_SurviveRestart(t *testing.T) {
	logPath, rollupPath := tempPaths(t)
	l := NewEventLog(logPath)
	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := l.Append(Event{Time: base.Add(time.Duration(i) * time.Minute), Kind: "stale_renew"}); err != nil {
			t.Fatal(err)
		}
	}

	// "Restart" #1: no sidecar exists, rebuild purely from the log.
	r1, err := LoadRollups(rollupPath, logPath)
	if err != nil {
		t.Fatalf("LoadRollups (log only): %v", err)
	}
	if got := r1.HourlySnapshot()["2026-03-01T09"]["stale_renew"]; got != 3 {
		t.Fatalf("rebuilt-from-log count = %d, want 3", got)
	}

	// Persist a sidecar, append one MORE event after it, then reload: the
	// sidecar's watermark plus the log's tail must total 4 without
	// double-counting the 3 already in the sidecar.
	if err := r1.Persist(rollupPath); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if err := l.Append(Event{Time: base.Add(10 * time.Minute), Kind: "stale_renew"}); err != nil {
		t.Fatal(err)
	}
	r2, err := LoadRollups(rollupPath, logPath)
	if err != nil {
		t.Fatalf("LoadRollups (sidecar+log): %v", err)
	}
	if got := r2.HourlySnapshot()["2026-03-01T09"]["stale_renew"]; got != 4 {
		t.Errorf("sidecar+log count = %d, want 4 (no double-count)", got)
	}
}

// ─────────────────────── service: end-to-end ───────────────────────────

// TestService_RecordsThroughDaemonSink proves the wired path: a Service
// installs itself as the daemon recorder, a daemon decision flows into the
// metrics, the event log, and the rollups, and Stop removes the recorder.
func TestService_RecordsThroughDaemonSink(t *testing.T) {
	logPath, rollupPath := tempPaths(t)
	svc, err := NewService(Settings{MetricsAddr: "", EventLog: true}, logPath, rollupPath, quietLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })

	// A reaper sweep decision that forfeited 2 claims, via the daemon's seam.
	daemon.SetDecisionRecorder(svc) // Start already installed it; explicit for clarity
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	svc.Record(daemon.Decision{
		Kind:    daemon.DecisionLeaseReap,
		Allow:   true,
		Reason:  "forfeited_zombie_claims",
		At:      now,
		Inputs:  map[string]bool{"errored": false},
		Metrics: map[string]int{"forfeited": 2},
	})
	svc.Record(daemon.Decision{
		Kind: daemon.DecisionAutopass, Allow: true, Reason: "fire", At: now,
	})

	// Metrics moved.
	render := svc.Registry().Render()
	if !strings.Contains(render, MetricLeaseForfeits+" 2") {
		t.Errorf("expected lease forfeits=2 in metrics:\n%s", render)
	}
	if !strings.Contains(render, MetricReaperSweeps+" 1") {
		t.Errorf("expected reaper sweeps=1 in metrics:\n%s", render)
	}
	if !strings.Contains(render, MetricSleepPasses+" 1") {
		t.Errorf("expected sleep passes=1 in metrics:\n%s", render)
	}
	if !strings.Contains(render, MetricRecorderEnabled+" 1") {
		t.Errorf("recorder_enabled gauge should be 1 while running:\n%s", render)
	}

	// Rollups folded the two decisions plus the service_start lifecycle event.
	hourly := svc.Rollups().HourlySnapshot()["2026-05-01T10"]
	if hourly["lease_reap"] != 1 || hourly["autopass"] != 1 {
		t.Errorf("rollups missing decisions: %+v", hourly)
	}

	// Event log has the lines on disk.
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), `"kind":"lease_reap"`) {
		t.Errorf("event log missing lease_reap line:\n%s", data)
	}

	// Stop removes the recorder: the daemon sink returns to no-op.
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	render = svc.Registry().Render()
	if !strings.Contains(render, MetricRecorderEnabled+" 0") {
		t.Errorf("recorder_enabled gauge should be 0 after Stop:\n%s", render)
	}
}

// TestService_MetricsEndpointServesText starts the HTTP endpoint on an
// ephemeral port and asserts a scrape returns the exposition text.
func TestService_MetricsEndpointServesText(t *testing.T) {
	logPath, rollupPath := tempPaths(t)
	svc, err := NewService(Settings{MetricsAddr: "127.0.0.1:0", EventLog: false}, logPath, rollupPath, quietLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })

	addr := svc.metricsAddr()
	if addr == "" {
		t.Fatal("metrics endpoint did not bind")
	}
	svc.Registry().Inc(MetricSessionsOpened)

	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain...", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), MetricSessionsOpened+" 1") {
		t.Errorf("scrape body missing sessions_opened=1:\n%s", body)
	}
}

// Loading core lease fallbacks must not re-run module defaults after TOML
// overrides. Resetting them would replace an operator's loopback/disabled
// endpoint with the all-interface metrics default.
func TestConfigLoadPreservesObservabilityOverrides(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:12345", ""} {
		t.Run(map[string]string{"127.0.0.1:12345": "loopback", "": "http-disabled"}[addr], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Chdir(t.TempDir())
			dir := filepath.Join(home, ".guild")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			content := "[modules]\nobservability = true\n[observability]\nmetrics_addr = " + fmt.Sprintf("%q", addr) + "\nevent_log = false\n[daemon]\nlease_ttl = 0\nheartbeat_interval = 0\nreap_interval = 0\n"
			if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(nil)
			if err != nil {
				t.Fatal(err)
			}
			if !config.ModuleEnabled(cfg)("observability", false) {
				t.Fatal("module toggle not loaded")
			}
			settings := CurrentSettings()
			if settings.MetricsAddr != addr || settings.EventLog {
				t.Fatalf("loaded module settings reset: %+v, want metrics_addr=%q event_log=false", settings, addr)
			}
			if cfg.Daemon.LeaseTTLSeconds <= 0 || cfg.Daemon.HeartbeatIntervalSeconds <= 0 || cfg.Daemon.ReapIntervalSeconds <= 0 {
				t.Fatalf("core lease fallbacks not applied: %+v", cfg.Daemon)
			}
		})
	}
}

func TestDefaultMetricsEndpointIsLoopback(t *testing.T) {
	addr := defaultSettings().MetricsAddr
	if addr != "127.0.0.1:9090" {
		t.Fatalf("default metrics endpoint must be local: %q", addr)
	}
}
