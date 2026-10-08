package daemon

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// setHome points $HOME at a fresh t.TempDir() so every test runs
// against a hermetic ~/.guild and never the live one. t.Setenv also
// guards against t.Parallel, which matters because some tests swap
// the package-level processAliveFn seam.
func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// shortSocketPath returns a unix socket path short enough for the
// sun_path limit (104 bytes on darwin, 108 on linux). t.TempDir()
// paths embed the full test name and can blow past it on macOS, so
// the socket lives in its own short-named temp dir instead.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gd")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p := filepath.Join(dir, "d.sock")
	if len(p) > 100 {
		t.Skipf("temp socket path too long for sun_path: %s", p)
	}
	return p
}

// listenUnix opens a real unix listener at path and closes it on test
// cleanup. Probe only dials and closes, so no accept loop is needed:
// the connection completes from the listen backlog.
func listenUnix(t *testing.T, path string) net.Listener {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen(unix, %s): %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func TestWriteReadRoundTrip(t *testing.T) {
	home := setHome(t)

	want := Discovery{
		PID:        os.Getpid(),
		Version:    "v0.3.2",
		SocketPath: "/tmp/guild-daemon.sock",
		StartedAt:  time.Now().UTC(),
	}
	if err := WriteDiscovery(want); err != nil {
		t.Fatalf("WriteDiscovery: %v", err)
	}

	got, err := ReadDiscovery()
	if err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if got == nil {
		t.Fatal("ReadDiscovery returned nil for a freshly written file")
	}
	if got.PID != want.PID || got.Version != want.Version || got.SocketPath != want.SocketPath {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, want)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt round-trip: got %v, want %v", got.StartedAt, want.StartedAt)
	}

	path := filepath.Join(home, ".guild", discoveryFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != discoveryFileMode {
			t.Errorf("daemon.json mode = %o, want %o", perm, discoveryFileMode)
		}
	}

	// started_at must serialize as RFC 3339 on the wire, and the
	// atomic write must not leave temp siblings behind.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read raw %s: %v", path, err)
	}
	var wire struct {
		StartedAt string `json:"started_at"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, wire.StartedAt); err != nil {
		t.Errorf("started_at %q is not RFC 3339: %v", wire.StartedAt, err)
	}
	entries, err := os.ReadDir(filepath.Join(home, ".guild"))
	if err != nil {
		t.Fatalf("read ~/.guild: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file after WriteDiscovery: %s", e.Name())
		}
	}
}

func TestWriteDiscoveryOverwrites(t *testing.T) {
	setHome(t)

	first := Discovery{PID: 100, Version: "v0.3.1", SocketPath: "/tmp/a.sock", StartedAt: time.Now().UTC()}
	second := Discovery{PID: 200, Version: "v0.3.2", SocketPath: "/tmp/b.sock", StartedAt: time.Now().UTC()}
	if err := WriteDiscovery(first); err != nil {
		t.Fatalf("first WriteDiscovery: %v", err)
	}
	if err := WriteDiscovery(second); err != nil {
		t.Fatalf("second WriteDiscovery: %v", err)
	}

	got, err := ReadDiscovery()
	if err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if got.PID != 200 || got.Version != "v0.3.2" {
		t.Errorf("rename did not replace prior record: got %+v", got)
	}
}

func TestReadDiscoveryMissingFile(t *testing.T) {
	setHome(t)

	got, err := ReadDiscovery()
	if err != nil {
		t.Fatalf("ReadDiscovery on missing file: %v", err)
	}
	if got != nil {
		t.Errorf("ReadDiscovery on missing file = %+v, want nil", got)
	}
}

func TestReadDiscoveryCorruptFile(t *testing.T) {
	home := setHome(t)

	path := filepath.Join(home, ".guild", discoveryFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}

	_, err := ReadDiscovery()
	if !errors.Is(err, ErrCorruptDiscovery) {
		t.Errorf("ReadDiscovery on corrupt file: err = %v, want ErrCorruptDiscovery", err)
	}
}

func TestProbeNoFile(t *testing.T) {
	setHome(t)

	res, d, err := Probe("v0.3.2", time.Second)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res != NotRunning {
		t.Errorf("Probe with no discovery file = %s, want %s", res, NotRunning)
	}
	if d != (Discovery{}) {
		t.Errorf("Probe NotRunning carried non-zero Discovery: %+v", d)
	}
}

func TestProbeStalePidCleanup(t *testing.T) {
	home := setHome(t)
	restore := swapProcessAlive(func(int) (bool, error) { return false, nil })
	t.Cleanup(restore)

	rec := Discovery{PID: 12345, Version: "v0.3.2", SocketPath: shortSocketPath(t), StartedAt: time.Now().UTC()}
	if err := WriteDiscovery(rec); err != nil {
		t.Fatalf("WriteDiscovery: %v", err)
	}

	res, _, err := Probe("v0.3.2", time.Second)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res != NotRunning {
		t.Errorf("Probe with dead pid = %s, want %s", res, NotRunning)
	}

	path := filepath.Join(home, ".guild", discoveryFileName)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale daemon.json not removed: stat err = %v", err)
	}
}

func TestProbeUndialableSocketCleanup(t *testing.T) {
	home := setHome(t)
	restore := swapProcessAlive(func(int) (bool, error) { return true, nil })
	t.Cleanup(restore)

	// Pid is "alive" but nothing listens at the socket path.
	rec := Discovery{PID: 12345, Version: "v0.3.2", SocketPath: shortSocketPath(t), StartedAt: time.Now().UTC()}
	if err := WriteDiscovery(rec); err != nil {
		t.Fatalf("WriteDiscovery: %v", err)
	}

	res, _, err := Probe("v0.3.2", time.Second)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res != NotRunning {
		t.Errorf("Probe with undialable socket = %s, want %s", res, NotRunning)
	}

	path := filepath.Join(home, ".guild", discoveryFileName)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale daemon.json not removed: stat err = %v", err)
	}
}

func TestProbeCorruptFileCleanup(t *testing.T) {
	home := setHome(t)

	path := filepath.Join(home, ".guild", discoveryFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}

	res, _, err := Probe("v0.3.2", time.Second)
	if err != nil {
		t.Fatalf("Probe on corrupt file: %v", err)
	}
	if res != NotRunning {
		t.Errorf("Probe on corrupt file = %s, want %s", res, NotRunning)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("corrupt daemon.json not removed: stat err = %v", err)
	}
}

func TestProbeRunningMatch(t *testing.T) {
	home := setHome(t)

	sock := shortSocketPath(t)
	listenUnix(t, sock)

	// Our own pid is alive by definition, so the real probe runs.
	rec := Discovery{PID: os.Getpid(), Version: "v0.3.2", SocketPath: sock, StartedAt: time.Now().UTC()}
	if err := WriteDiscovery(rec); err != nil {
		t.Fatalf("WriteDiscovery: %v", err)
	}

	res, d, err := Probe("v0.3.2", time.Second)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res != RunningMatch {
		t.Errorf("Probe = %s, want %s", res, RunningMatch)
	}
	if d.Version != "v0.3.2" || d.SocketPath != sock || d.PID != os.Getpid() {
		t.Errorf("Probe returned wrong Discovery: %+v", d)
	}

	// A live daemon's discovery file must survive the probe.
	path := filepath.Join(home, ".guild", discoveryFileName)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("daemon.json removed for a live daemon: %v", err)
	}
}

func TestProbeRunningMismatch(t *testing.T) {
	setHome(t)

	sock := shortSocketPath(t)
	listenUnix(t, sock)

	cases := []struct {
		name           string
		daemonV, selfV string
	}{
		{"older daemon", "v0.3.1", "v0.3.2"},
		{"newer daemon", "v0.3.3", "v0.3.2"},
		{"dev daemon vs release self", "dev", "v0.3.2"},
		{"release daemon vs dev self", "v0.3.2", "dev"},
		{"dev-suffix differs", "v0.3.2-1-gabcdef0", "v0.3.2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := Discovery{PID: os.Getpid(), Version: tc.daemonV, SocketPath: sock, StartedAt: time.Now().UTC()}
			if err := WriteDiscovery(rec); err != nil {
				t.Fatalf("WriteDiscovery: %v", err)
			}
			res, d, err := Probe(tc.selfV, time.Second)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if res != RunningMismatch {
				t.Errorf("Probe(%q vs daemon %q) = %s, want %s", tc.selfV, tc.daemonV, res, RunningMismatch)
			}
			if d.Version != tc.daemonV {
				t.Errorf("mismatch result carries daemon version %q, want %q", d.Version, tc.daemonV)
			}
		})
	}
}

func TestProbeExactEqualityDevBuilds(t *testing.T) {
	setHome(t)

	sock := shortSocketPath(t)
	listenUnix(t, sock)

	// Two dev builds with the identical stamp are a match: the rule is
	// exact string equality, not semver validity.
	rec := Discovery{PID: os.Getpid(), Version: "dev", SocketPath: sock, StartedAt: time.Now().UTC()}
	if err := WriteDiscovery(rec); err != nil {
		t.Fatalf("WriteDiscovery: %v", err)
	}
	res, _, err := Probe("dev", time.Second)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res != RunningMatch {
		t.Errorf("Probe(dev vs dev) = %s, want %s", res, RunningMatch)
	}
}

func TestUnresolvableHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("HOME-based UserHomeDir failure is a unix behavior")
	}
	// An empty $HOME makes os.UserHomeDir fail, which every entry
	// point must surface as an error rather than touching a guessed
	// location.
	t.Setenv("HOME", "")

	if err := WriteDiscovery(Discovery{PID: 1}); err == nil {
		t.Error("WriteDiscovery with unresolvable home: want error, got nil")
	}
	if _, err := ReadDiscovery(); err == nil {
		t.Error("ReadDiscovery with unresolvable home: want error, got nil")
	}
	if _, _, err := Probe("v0.3.2", time.Second); err == nil {
		t.Error("Probe with unresolvable home: want error, got nil")
	}
}

func TestUnreadableDiscoveryFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("0000 file modes are not enforced on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	home := setHome(t)

	path := filepath.Join(home, ".guild", discoveryFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o000); err != nil {
		t.Fatalf("write unreadable file: %v", err)
	}

	if _, err := ReadDiscovery(); err == nil {
		t.Error("ReadDiscovery on unreadable file: want error, got nil")
	} else if errors.Is(err, ErrCorruptDiscovery) {
		t.Errorf("permission error misclassified as corrupt: %v", err)
	}
	if _, _, err := Probe("v0.3.2", time.Second); err == nil {
		t.Error("Probe on unreadable file: want error, got nil")
	}

	// The file must NOT be removed: an unreadable record is an
	// environmental fault, not proof the daemon is gone.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("unreadable daemon.json was removed: %v", err)
	}
}

func TestProbeResultString(t *testing.T) {
	cases := map[ProbeResult]string{
		NotRunning:      "not_running",
		RunningMatch:    "running_match",
		RunningMismatch: "running_mismatch",
		ProbeResult(99): "ProbeResult(99)",
	}
	for r, want := range cases {
		if got := r.String(); got != want {
			t.Errorf("ProbeResult(%d).String() = %q, want %q", int(r), got, want)
		}
	}
}

func TestFormatSkewNudge(t *testing.T) {
	cases := []struct {
		name           string
		daemonV, selfV string
		wantSubstr     string
	}{
		{"daemon newer", "v0.3.3", "v0.3.2", "newer than this binary"},
		{"daemon older", "v0.3.1", "v0.3.2", "older than this binary"},
		{"dev daemon", "dev", "v0.3.2", "does not match this binary"},
		{"dev self", "v0.3.2", "dev", "does not match this binary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatSkewNudge(tc.daemonV, tc.selfV)
			if !strings.Contains(got, tc.wantSubstr) {
				t.Errorf("FormatSkewNudge(%q, %q) = %q, want substring %q", tc.daemonV, tc.selfV, got, tc.wantSubstr)
			}
			if !strings.Contains(got, tc.daemonV) || !strings.Contains(got, tc.selfV) {
				t.Errorf("nudge %q must name both versions %q and %q", got, tc.daemonV, tc.selfV)
			}
			if strings.Contains(got, "\n") {
				t.Errorf("nudge must be a single line, got %q", got)
			}
		})
	}

	if got := FormatSkewNudge("v0.3.2", "v0.3.2"); got != "" {
		t.Errorf("FormatSkewNudge with equal versions = %q, want empty", got)
	}
}
