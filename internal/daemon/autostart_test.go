//go:build unix

package daemon

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// autostartHelperOpts builds AutostartOptions that re-exec this test
// binary as the helper daemon (the same GUILD_LIFECYCLE_TEST_DAEMON
// dispatch lifecycle_test.go's startHelper uses), so Autostart spawns a
// real detached process serving a real socket under the test-controlled
// $HOME. The helper daemon stamps version "dev", which SelfVersion
// matches so the result is RunningMatch.
func autostartHelperOpts() AutostartOptions {
	return AutostartOptions{
		SelfVersion: "dev",
		Exe:         os.Args[0],
		Args:        []string{}, // helper mode dispatches on env, not argv
		Wait:        10 * time.Second,
		// Env nil inherits the parent's environment, which carries the
		// t.Setenv(helperDaemonEnv, "serve") and the test HOME.
	}
}

// killByDiscovery best-effort kills whatever daemon daemon.json names,
// so a spawned helper never outlives the test.
func killByDiscovery(t *testing.T) {
	t.Helper()
	d, err := ReadDiscovery()
	if err != nil || d == nil {
		return
	}
	if alive, aerr := processAlive(d.PID); aerr == nil && alive {
		_ = killProcess(d.PID)
	}
}

func TestLockPath(t *testing.T) {
	home := setShortHome(t)
	got, err := LockPath()
	if err != nil {
		t.Fatalf("LockPath: %v", err)
	}
	want := filepath.Join(home, ".guild", lockFileName)
	if got != want {
		t.Fatalf("LockPath = %q, want %q", got, want)
	}
}

// TestFlockExclusiveSemantics: a second exclusive lock attempt on the
// same file fails while the first holds it, and succeeds once the first
// releases. This is the election primitive the autostart race relies on.
func TestFlockExclusiveSemantics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, lockFileName)

	f1, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, lockFileMode)
	if err != nil {
		t.Fatalf("open f1: %v", err)
	}
	defer func() { _ = f1.Close() }()
	f2, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, lockFileMode)
	if err != nil {
		t.Fatalf("open f2: %v", err)
	}
	defer func() { _ = f2.Close() }()

	won1, err := tryLockExclusive(f1)
	if err != nil || !won1 {
		t.Fatalf("f1 tryLockExclusive = (%v, %v), want (true, nil)", won1, err)
	}

	// A separate descriptor (a separate process in production) must lose.
	won2, err := tryLockExclusive(f2)
	if err != nil {
		t.Fatalf("f2 tryLockExclusive err: %v", err)
	}
	if won2 {
		t.Fatal("f2 won the lock while f1 still held it; the election would elect two spawners")
	}

	if err := unlock(f1); err != nil {
		t.Fatalf("unlock f1: %v", err)
	}

	// After release the next contender wins.
	won2, err = tryLockExclusive(f2)
	if err != nil || !won2 {
		t.Fatalf("f2 tryLockExclusive after f1 unlock = (%v, %v), want (true, nil)", won2, err)
	}
	_ = unlock(f2)
}

// TestAutostartSpawnsDaemon: on an empty HOME a single Autostart spawns
// a daemon, returns RunningMatch with the live socket, and the daemon
// pid is recorded in daemon.json.
func TestAutostartSpawnsDaemon(t *testing.T) {
	home := setShortHome(t)
	t.Setenv(helperDaemonEnv, "serve")
	t.Cleanup(func() { killByDiscovery(t) })

	res, live, err := Autostart(autostartHelperOpts())
	if err != nil {
		t.Fatalf("Autostart: %v", err)
	}
	if res != RunningMatch {
		t.Fatalf("Autostart result = %v, want running_match", res)
	}
	if live.PID <= 0 {
		t.Fatalf("Autostart live pid = %d, want > 0", live.PID)
	}
	wantSock := filepath.Join(home, ".guild", socketFileName)
	if live.SocketPath != wantSock {
		t.Fatalf("Autostart socket = %q, want %q", live.SocketPath, wantSock)
	}

	d, err := ReadDiscovery()
	if err != nil || d == nil {
		t.Fatalf("ReadDiscovery after Autostart = (%+v, %v)", d, err)
	}
	if d.PID != live.PID {
		t.Fatalf("daemon.json pid = %d, want %d", d.PID, live.PID)
	}

	// Stop cleanly so the test leaves nothing behind.
	if _, serr := Stop(StopOptions{}); serr != nil {
		t.Fatalf("Stop: %v", serr)
	}
}

// TestAutostartIdempotentUnderLock: when a daemon is already running,
// Autostart's double-check under the lock finds it and returns it
// without spawning a second one.
func TestAutostartIdempotentUnderLock(t *testing.T) {
	setShortHome(t)
	t.Setenv(helperDaemonEnv, "serve")
	t.Cleanup(func() { killByDiscovery(t) })

	// First start via the lifecycle helper.
	first := startHelper(t, "serve")

	res, live, err := Autostart(autostartHelperOpts())
	if err != nil {
		t.Fatalf("Autostart with daemon already up: %v", err)
	}
	if res != RunningMatch {
		t.Fatalf("Autostart result = %v, want running_match", res)
	}
	if live.PID != first.PID {
		t.Fatalf("Autostart pid = %d, want the existing daemon's pid %d (no second spawn)", live.PID, first.PID)
	}

	if _, serr := Stop(StopOptions{}); serr != nil {
		t.Fatalf("Stop: %v", serr)
	}
}

// TestAutostartSpawnFailureReturnsError: when the spawned process exits
// without ever becoming ready, Autostart pairs NotRunning (so the host
// serves in-process) with a non-nil error the host logs as one line.
func TestAutostartSpawnFailureReturnsError(t *testing.T) {
	setShortHome(t)

	res, _, err := Autostart(AutostartOptions{
		SelfVersion: "dev",
		Exe:         "/bin/sh",
		Args:        []string{"-c", "exit 1"},
		Wait:        2 * time.Second,
	})
	if err == nil {
		t.Fatal("Autostart with a child that exits immediately returned nil error")
	}
	if res != NotRunning {
		t.Fatalf("Autostart spawn-failure result = %v, want not_running so the host falls back in-process", res)
	}
}

// TestAutostartConcurrentSingleSpawner is the thundering-herd test:
// N goroutines race Autostart against one empty HOME. The flock must
// elect exactly one spawner, so all N converge on a single daemon pid,
// every caller sees running_match on the same socket, and daemon.json
// records that one pid.
func TestAutostartConcurrentSingleSpawner(t *testing.T) {
	home := setShortHome(t)
	t.Setenv(helperDaemonEnv, "serve")
	t.Cleanup(func() { killByDiscovery(t) })

	const n = 8
	type out struct {
		res  ProbeResult
		live Discovery
		err  error
	}
	results := make([]out, n)

	var ready sync.WaitGroup
	var done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for i := range n {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start // release all goroutines at once for a real race
			res, live, err := Autostart(autostartHelperOpts())
			results[i] = out{res, live, err}
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()

	wantSock := filepath.Join(home, ".guild", socketFileName)
	pids := map[int]struct{}{}
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("goroutine %d Autostart err: %v", i, r.err)
		}
		if r.res != RunningMatch {
			t.Fatalf("goroutine %d result = %v, want running_match", i, r.res)
		}
		if r.live.SocketPath != wantSock {
			t.Fatalf("goroutine %d socket = %q, want %q", i, r.live.SocketPath, wantSock)
		}
		if r.live.PID <= 0 {
			t.Fatalf("goroutine %d pid = %d, want > 0", i, r.live.PID)
		}
		pids[r.live.PID] = struct{}{}
	}
	if len(pids) != 1 {
		t.Fatalf("autostart elected %d distinct daemon pids %v; want exactly one spawner", len(pids), pids)
	}

	// The single elected pid is the one in daemon.json.
	d, err := ReadDiscovery()
	if err != nil || d == nil {
		t.Fatalf("ReadDiscovery = (%+v, %v)", d, err)
	}
	if _, ok := pids[d.PID]; !ok {
		t.Fatalf("daemon.json pid %d is not the elected spawner %v", d.PID, pids)
	}

	if _, serr := Stop(StopOptions{}); serr != nil {
		t.Fatalf("Stop: %v", serr)
	}
}

// TestAutostartLoserWaitsForWinner exercises the lost-the-lock arm
// directly: a held lock forces Autostart down waitForDaemon, where it
// must NOT spawn (it cannot take the lock) and must instead pick up the
// daemon a concurrent winner brings up. Here the "winner" is a daemon
// started up front; the locked Autostart should re-probe and return it.
func TestAutostartLoserWaitsForWinner(t *testing.T) {
	setShortHome(t)
	t.Setenv(helperDaemonEnv, "serve")
	t.Cleanup(func() { killByDiscovery(t) })

	// Winner's daemon is already up.
	winner := startHelper(t, "serve")

	// Hold the election lock so the call below loses the race and takes
	// the loser arm (waitForDaemon) rather than the spawn arm.
	lockPath, err := LockPath()
	if err != nil {
		t.Fatalf("LockPath: %v", err)
	}
	held, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, lockFileMode)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer func() { _ = held.Close() }()
	won, err := tryLockExclusive(held)
	if err != nil || !won {
		t.Fatalf("pre-acquire lock = (%v, %v), want (true, nil)", won, err)
	}

	res, live, err := Autostart(autostartHelperOpts())
	if err != nil {
		t.Fatalf("Autostart (loser): %v", err)
	}
	if res != RunningMatch {
		t.Fatalf("Autostart (loser) result = %v, want running_match", res)
	}
	if live.PID != winner.PID {
		t.Fatalf("Autostart (loser) pid = %d, want the winner's daemon pid %d", live.PID, winner.PID)
	}

	_ = unlock(held)
	if _, serr := Stop(StopOptions{}); serr != nil {
		t.Fatalf("Stop: %v", serr)
	}
}
