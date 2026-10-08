// daemon_test.go covers `guild daemon install|uninstall` (daemon.go).
//
// All flow tests are hermetic: homes come from t.TempDir, PATH lookups
// and loader invocations are injected, and no launchctl/systemctl call
// ever leaves the test process. The only test that talks to a real
// service manager is TestDaemonInstallUninstall_Integration, which is
// double-guarded (env opt-in + OS/loader presence) and uses a
// test-only unit label so it cannot collide with a real guild install.
package install

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// goldenBinPath is the placeholder binary path baked into the golden
// files so no personal paths land in testdata.
const goldenBinPath = "/usr/local/bin/guild"

// TestDaemonUnit_RenderGoldens locks the rendered unit content for both
// platforms. Regenerate after an intentional template change with:
//
//	GUILD_GOLDEN_UPDATE=1 go test ./internal/install -run RenderGoldens
func TestDaemonUnit_RenderGoldens(t *testing.T) {
	cases := []struct {
		goos   string
		golden string
	}{
		{"darwin", "guild-daemon.plist.golden"},
		{"linux", "guild-daemon.service.golden"},
	}
	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			unit := daemonUnits[tc.goos]
			got, err := unit.render(unit.label, goldenBinPath)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			path := filepath.Join("testdata", tc.golden)
			if os.Getenv("GUILD_GOLDEN_UPDATE") == "1" {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil { //nolint:gosec // testdata fixture
					t.Fatalf("update golden: %v", err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run with GUILD_GOLDEN_UPDATE=1 to create): %v", err)
			}
			if got != string(want) {
				t.Errorf("rendered unit differs from %s:\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
			}
		})
	}
}

// TestDaemonUnit_RenderExecLine pins the load-bearing acceptance detail:
// the unit execs the absolute binary path with argv `daemon run`.
func TestDaemonUnit_RenderExecLine(t *testing.T) {
	darwin, err := daemonUnits["darwin"].render(DaemonLabelDarwin, goldenBinPath)
	if err != nil {
		t.Fatalf("render darwin: %v", err)
	}
	for _, want := range []string{
		"<string>" + goldenBinPath + "</string>",
		"<string>daemon</string>",
		"<string>run</string>",
		"<string>" + DaemonLabelDarwin + "</string>",
	} {
		if !strings.Contains(darwin, want) {
			t.Errorf("plist missing %q:\n%s", want, darwin)
		}
	}

	linux, err := daemonUnits["linux"].render(DaemonLabelLinux, goldenBinPath)
	if err != nil {
		t.Fatalf("render linux: %v", err)
	}
	if want := `ExecStart="` + goldenBinPath + `" daemon run`; !strings.Contains(linux, want) {
		t.Errorf("service unit missing %q:\n%s", want, linux)
	}
}

// TestDaemonUnit_RenderEscaping verifies hostile path characters cannot
// corrupt the unit syntax: XML entities in the plist, %-specifiers in
// the systemd unit.
func TestDaemonUnit_RenderEscaping(t *testing.T) {
	darwin, err := daemonUnits["darwin"].render(DaemonLabelDarwin, "/opt/we&ird/guild")
	if err != nil {
		t.Fatalf("render darwin: %v", err)
	}
	if !strings.Contains(darwin, "/opt/we&amp;ird/guild") {
		t.Errorf("plist did not XML-escape '&':\n%s", darwin)
	}
	if strings.Contains(darwin, "we&ird") {
		t.Errorf("plist contains raw '&':\n%s", darwin)
	}

	linux, err := daemonUnits["linux"].render(DaemonLabelLinux, "/opt/100%full/guild")
	if err != nil {
		t.Fatalf("render linux: %v", err)
	}
	if !strings.Contains(linux, "/opt/100%%full/guild") {
		t.Errorf("service unit did not double '%%':\n%s", linux)
	}
}

// fakeRunner records loader invocations and fails the ones matched by
// failPrefix, so tests can exercise optional/fallback semantics without
// any real launchctl/systemctl.
type fakeRunner struct {
	calls        [][]string
	failPrefixes []string
}

func (f *fakeRunner) run(name string, arg ...string) ([]byte, error) {
	argv := append([]string{name}, arg...)
	f.calls = append(f.calls, argv)
	joined := strings.Join(argv, " ")
	for _, p := range f.failPrefixes {
		if strings.HasPrefix(joined, p) {
			return []byte("simulated failure"), errors.New("exit status 1")
		}
	}
	return nil, nil
}

// call returns the i-th recorded invocation joined for assertions.
func (f *fakeRunner) call(i int) string {
	if i >= len(f.calls) {
		return ""
	}
	return strings.Join(f.calls[i], " ")
}

// writeFakeBin creates a fake guild binary in a directory that is NOT
// under os.TempDir: t.TempDir lives under the real TMPDIR, which would
// otherwise trip the transient-path warning in every flow test. TMPDIR
// is redirected to a sibling directory instead.
func writeFakeBin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(root, "tmp"))
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin dir: %v", err)
	}
	p := filepath.Join(binDir, "guild")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // fake binary fixture
		t.Fatalf("write fake bin: %v", err)
	}
	return p
}

// loaderOnPath is a lookPathFn stub: the loader CLIs resolve, anything
// else (notably "guild", used by binary-path resolution) does not.
func loaderOnPath(name string) (string, error) {
	if name == "launchctl" || name == "systemctl" {
		return "/usr/bin/" + name, nil
	}
	return "", exec.ErrNotFound
}

// nothingOnPath is a lookPathFn stub that resolves nothing.
func nothingOnPath(string) (string, error) { return "", exec.ErrNotFound }

func TestDaemonInstall_Darwin_WritesUnitAndLoads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binPath := writeFakeBin(t)
	wantBin := evalSymlinkOrFallback(binPath)

	fake := &fakeRunner{
		// First-install reality: nothing to boot out yet.
		failPrefixes: []string{"launchctl bootout"},
	}
	var out bytes.Buffer
	res, err := DaemonInstall(DaemonUnitOptions{
		Out:          &out,
		goos:         "darwin",
		executableFn: func() (string, error) { return binPath, nil },
		lookPathFn:   loaderOnPath,
		runCmdFn:     fake.run,
	})
	if err != nil {
		t.Fatalf("DaemonInstall: %v", err)
	}

	wantPath := filepath.Join(home, "Library", "LaunchAgents", DaemonLabelDarwin+".plist")
	if res.UnitPath != wantPath {
		t.Errorf("UnitPath = %q; want %q", res.UnitPath, wantPath)
	}
	if res.BinPath != wantBin {
		t.Errorf("BinPath = %q; want %q", res.BinPath, wantBin)
	}
	if !res.Loaded || res.Manual || res.TransientWarning {
		t.Errorf("flags = loaded=%v manual=%v transient=%v; want loaded only", res.Loaded, res.Manual, res.TransientWarning)
	}

	content, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("unit file not written: %v", err)
	}
	if string(content) != res.Rendered {
		t.Errorf("file content differs from Rendered")
	}
	if !strings.Contains(string(content), "<string>"+wantBin+"</string>") {
		t.Errorf("plist missing binary path %q:\n%s", wantBin, content)
	}

	domain := launchdDomain()
	if got, want := fake.call(0), "launchctl bootout "+domain+"/"+DaemonLabelDarwin; got != want {
		t.Errorf("call 0 = %q; want %q", got, want)
	}
	if got, want := fake.call(1), "launchctl bootstrap "+domain+" "+wantPath; got != want {
		t.Errorf("call 1 = %q; want %q", got, want)
	}
	if len(fake.calls) != 2 {
		t.Errorf("loader calls = %d; want 2:\n%v", len(fake.calls), fake.calls)
	}
	if !strings.Contains(out.String(), "unit written to "+wantPath) {
		t.Errorf("output missing unit path:\n%s", out.String())
	}
	if strings.Contains(out.String(), "warning:") {
		t.Errorf("unexpected transient warning:\n%s", out.String())
	}
}

func TestDaemonInstall_Darwin_RepeatIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binPath := writeFakeBin(t)

	opts := func(f *fakeRunner) DaemonUnitOptions {
		return DaemonUnitOptions{
			Out:          &bytes.Buffer{},
			goos:         "darwin",
			executableFn: func() (string, error) { return binPath, nil },
			lookPathFn:   loaderOnPath,
			runCmdFn:     f.run,
		}
	}

	if _, err := DaemonInstall(opts(&fakeRunner{failPrefixes: []string{"launchctl bootout"}})); err != nil {
		t.Fatalf("first install: %v", err)
	}
	// Second install: the agent is loaded now, so bootout succeeds and
	// bootstrap re-loads the re-rendered plist. No duplicate unit files,
	// no error.
	second := &fakeRunner{}
	if _, err := DaemonInstall(opts(second)); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if len(second.calls) != 2 {
		t.Errorf("second install loader calls = %d; want 2 (bootout + bootstrap):\n%v", len(second.calls), second.calls)
	}

	dir := filepath.Join(home, "Library", "LaunchAgents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read unit dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("unit dir has %d entries; want exactly 1", len(entries))
	}
}

func TestDaemonInstall_Darwin_BootstrapFallsBackToLegacyLoad(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	binPath := writeFakeBin(t)

	fake := &fakeRunner{failPrefixes: []string{"launchctl bootout", "launchctl bootstrap"}}
	res, err := DaemonInstall(DaemonUnitOptions{
		Out:          &bytes.Buffer{},
		goos:         "darwin",
		executableFn: func() (string, error) { return binPath, nil },
		lookPathFn:   loaderOnPath,
		runCmdFn:     fake.run,
	})
	if err != nil {
		t.Fatalf("DaemonInstall: %v", err)
	}
	if !res.Loaded {
		t.Errorf("Loaded = false; want true via legacy fallback")
	}
	if got := fake.call(2); !strings.HasPrefix(got, "launchctl load -w ") {
		t.Errorf("call 2 = %q; want legacy 'launchctl load -w'", got)
	}
}

func TestDaemonInstall_Darwin_LoadFailureSurfaces(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	binPath := writeFakeBin(t)

	fake := &fakeRunner{failPrefixes: []string{"launchctl"}} // everything fails
	_, err := DaemonInstall(DaemonUnitOptions{
		Out:          &bytes.Buffer{},
		goos:         "darwin",
		executableFn: func() (string, error) { return binPath, nil },
		lookPathFn:   loaderOnPath,
		runCmdFn:     fake.run,
	})
	if err == nil || !strings.Contains(err.Error(), "load unit") {
		t.Fatalf("err = %v; want load-unit failure", err)
	}
}

func TestDaemonInstall_ManualWhenLoaderAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binPath := writeFakeBin(t)

	fake := &fakeRunner{}
	var out bytes.Buffer
	res, err := DaemonInstall(DaemonUnitOptions{
		Out:          &out,
		goos:         "linux",
		executableFn: func() (string, error) { return binPath, nil },
		lookPathFn:   nothingOnPath,
		runCmdFn:     fake.run,
	})
	if err != nil {
		t.Fatalf("DaemonInstall: %v", err)
	}
	if !res.Manual || res.Loaded {
		t.Errorf("flags = manual=%v loaded=%v; want manual only", res.Manual, res.Loaded)
	}
	if len(fake.calls) != 0 {
		t.Errorf("loader invoked despite being absent: %v", fake.calls)
	}
	// The unit file is still written (render + write only)...
	if _, err := os.Stat(res.UnitPath); err != nil {
		t.Errorf("unit file not written on manual path: %v", err)
	}
	// ...and the command prints the rendered unit plus manual steps.
	for _, want := range []string{
		"systemctl not found on PATH",
		"systemctl --user enable --now " + DaemonLabelLinux + ".service",
		res.Rendered,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("manual output missing %q:\n%s", want, out.String())
		}
	}
}

func TestDaemonInstall_Linux_WritesUnitAndEnables(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	binPath := writeFakeBin(t)

	fake := &fakeRunner{}
	res, err := DaemonInstall(DaemonUnitOptions{
		Out:          &bytes.Buffer{},
		goos:         "linux",
		executableFn: func() (string, error) { return binPath, nil },
		lookPathFn:   loaderOnPath,
		runCmdFn:     fake.run,
	})
	if err != nil {
		t.Fatalf("DaemonInstall: %v", err)
	}

	wantPath := filepath.Join(home, ".config", "systemd", "user", DaemonLabelLinux+".service")
	if res.UnitPath != wantPath {
		t.Errorf("UnitPath = %q; want %q", res.UnitPath, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("unit file not written: %v", err)
	}
	if got, want := fake.call(0), "systemctl --user daemon-reload"; got != want {
		t.Errorf("call 0 = %q; want %q", got, want)
	}
	if got, want := fake.call(1), "systemctl --user enable --now "+DaemonLabelLinux+".service"; got != want {
		t.Errorf("call 1 = %q; want %q", got, want)
	}
}

func TestDaemonInstall_Linux_HonorsXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	xdg := filepath.Join(home, "xdg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	binPath := writeFakeBin(t)

	res, err := DaemonInstall(DaemonUnitOptions{
		Out:          &bytes.Buffer{},
		goos:         "linux",
		executableFn: func() (string, error) { return binPath, nil },
		lookPathFn:   loaderOnPath,
		runCmdFn:     (&fakeRunner{}).run,
	})
	if err != nil {
		t.Fatalf("DaemonInstall: %v", err)
	}
	want := filepath.Join(xdg, "systemd", "user", DaemonLabelLinux+".service")
	if res.UnitPath != want {
		t.Errorf("UnitPath = %q; want %q", res.UnitPath, want)
	}
}

func TestDaemonInstall_WarnsOnTransientBinPath(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", home)
	// Neutralize every durable probe so resolution must fall back to
	// the transient executable path.
	t.Setenv("GOBIN", filepath.Join(root, "no-gobin"))
	t.Setenv("GOPATH", filepath.Join(root, "no-gopath"))

	tmp := filepath.Join(root, "tmp")
	t.Setenv("TMPDIR", tmp)
	binDir := filepath.Join(tmp, "go-build-ish", "exe")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin dir: %v", err)
	}
	binPath := filepath.Join(binDir, "guild")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // fake binary fixture
		t.Fatalf("write fake bin: %v", err)
	}

	var out bytes.Buffer
	res, err := DaemonInstall(DaemonUnitOptions{
		Out:          &out,
		goos:         "darwin",
		executableFn: func() (string, error) { return binPath, nil },
		lookPathFn:   loaderOnPath, // "guild" not resolvable, launchctl is
		runCmdFn:     (&fakeRunner{}).run,
	})
	if err != nil {
		t.Fatalf("DaemonInstall: %v", err)
	}
	if !res.TransientWarning {
		t.Errorf("TransientWarning = false; want true for bin under TMPDIR")
	}
	if !res.Loaded {
		t.Errorf("Loaded = false; the warning must not block the install")
	}
	if !strings.Contains(out.String(), "warning:") {
		t.Errorf("output missing transient-path warning:\n%s", out.String())
	}
}

func TestDaemonUninstall_Darwin_RemovesAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binPath := writeFakeBin(t)

	install := DaemonUnitOptions{
		Out:          &bytes.Buffer{},
		goos:         "darwin",
		executableFn: func() (string, error) { return binPath, nil },
		lookPathFn:   loaderOnPath,
		runCmdFn:     (&fakeRunner{failPrefixes: []string{"launchctl bootout"}}).run,
	}
	res, err := DaemonInstall(install)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	fake := &fakeRunner{}
	var out bytes.Buffer
	unres, err := DaemonUninstall(DaemonUnitOptions{
		Out:        &out,
		goos:       "darwin",
		lookPathFn: loaderOnPath,
		runCmdFn:   fake.run,
	})
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !unres.Removed || !unres.Unloaded {
		t.Errorf("flags = removed=%v unloaded=%v; want both", unres.Removed, unres.Unloaded)
	}
	if got, want := fake.call(0), "launchctl bootout "+launchdDomain()+"/"+DaemonLabelDarwin; got != want {
		t.Errorf("call 0 = %q; want %q", got, want)
	}
	if _, err := os.Stat(res.UnitPath); !os.IsNotExist(err) {
		t.Errorf("unit file still present after uninstall (stat err = %v)", err)
	}

	// Second uninstall: nothing loaded (bootout fails), nothing to
	// remove; still exit-0 idempotent.
	again, err := DaemonUninstall(DaemonUnitOptions{
		Out:        &out,
		goos:       "darwin",
		lookPathFn: loaderOnPath,
		runCmdFn:   (&fakeRunner{failPrefixes: []string{"launchctl"}}).run,
	})
	if err != nil {
		t.Fatalf("second uninstall: %v", err)
	}
	if again.Removed {
		t.Errorf("second uninstall Removed = true; want false")
	}
	if !strings.Contains(out.String(), "no unit installed") {
		t.Errorf("second uninstall output missing 'no unit installed':\n%s", out.String())
	}
}

func TestDaemonUninstall_Linux_DisablesAndRemoves(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	binPath := writeFakeBin(t)

	if _, err := DaemonInstall(DaemonUnitOptions{
		Out:          &bytes.Buffer{},
		goos:         "linux",
		executableFn: func() (string, error) { return binPath, nil },
		lookPathFn:   loaderOnPath,
		runCmdFn:     (&fakeRunner{}).run,
	}); err != nil {
		t.Fatalf("install: %v", err)
	}

	fake := &fakeRunner{}
	unres, err := DaemonUninstall(DaemonUnitOptions{
		Out:        &bytes.Buffer{},
		goos:       "linux",
		lookPathFn: loaderOnPath,
		runCmdFn:   fake.run,
	})
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !unres.Removed {
		t.Errorf("Removed = false; want true")
	}
	if got, want := fake.call(0), "systemctl --user disable --now "+DaemonLabelLinux+".service"; got != want {
		t.Errorf("call 0 = %q; want %q", got, want)
	}
	if got, want := fake.call(1), "systemctl --user daemon-reload"; got != want {
		t.Errorf("call 1 = %q; want %q", got, want)
	}
}

func TestDaemonUninstall_ManualWhenLoaderAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binPath := writeFakeBin(t)

	if _, err := DaemonInstall(DaemonUnitOptions{
		Out:          &bytes.Buffer{},
		goos:         "darwin",
		executableFn: func() (string, error) { return binPath, nil },
		lookPathFn:   nothingOnPath,
		runCmdFn:     (&fakeRunner{}).run,
	}); err != nil {
		t.Fatalf("install: %v", err)
	}

	fake := &fakeRunner{}
	var out bytes.Buffer
	unres, err := DaemonUninstall(DaemonUnitOptions{
		Out:        &out,
		goos:       "darwin",
		lookPathFn: nothingOnPath,
		runCmdFn:   fake.run,
	})
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !unres.Manual || !unres.Removed {
		t.Errorf("flags = manual=%v removed=%v; want both", unres.Manual, unres.Removed)
	}
	if len(fake.calls) != 0 {
		t.Errorf("loader invoked despite being absent: %v", fake.calls)
	}
	if !strings.Contains(out.String(), "launchctl not found on PATH") {
		t.Errorf("output missing manual unload instructions:\n%s", out.String())
	}
}

func TestDaemonInstallUninstall_UnsupportedPlatform(t *testing.T) {
	if _, err := DaemonInstall(DaemonUnitOptions{Out: &bytes.Buffer{}, goos: "windows"}); !errors.Is(err, ErrDaemonUnitsUnsupported) {
		t.Errorf("install err = %v; want ErrDaemonUnitsUnsupported", err)
	}
	if _, err := DaemonUninstall(DaemonUnitOptions{Out: &bytes.Buffer{}, goos: "windows"}); !errors.Is(err, ErrDaemonUnitsUnsupported) {
		t.Errorf("uninstall err = %v; want ErrDaemonUnitsUnsupported", err)
	}
}

func TestResolveDaemonBinPath_PrefersDurableInstall(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	tmp := filepath.Join(root, "tmp")
	t.Setenv("TMPDIR", tmp)

	// The "running" executable is transient...
	exeDir := filepath.Join(tmp, "exe")
	if err := os.MkdirAll(exeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	exe := filepath.Join(exeDir, "guild")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil { //nolint:gosec // fixture
		t.Fatalf("write: %v", err)
	}
	// ...but a durable go-install copy exists.
	gobin := filepath.Join(root, "gobin")
	if err := os.MkdirAll(gobin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	durable := filepath.Join(gobin, "guild")
	if err := os.WriteFile(durable, []byte("x"), 0o755); err != nil { //nolint:gosec // fixture
		t.Fatalf("write: %v", err)
	}
	t.Setenv("GOBIN", gobin)

	got, transient, err := resolveDaemonBinPath(
		func() (string, error) { return exe, nil },
		nothingOnPath,
	)
	if err != nil {
		t.Fatalf("resolveDaemonBinPath: %v", err)
	}
	if transient {
		t.Errorf("transient = true; want false (durable GOBIN copy found)")
	}
	if want := evalSymlinkOrFallback(durable); got != want {
		t.Errorf("path = %q; want durable %q", got, want)
	}
}

func TestResolveDaemonBinPath_ErrorWhenNothingFound(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("GOBIN", filepath.Join(root, "no-gobin"))
	t.Setenv("GOPATH", filepath.Join(root, "no-gopath"))

	_, _, err := resolveDaemonBinPath(
		func() (string, error) { return filepath.Join(root, "gone", "guild"), nil },
		nothingOnPath,
	)
	if err == nil {
		t.Fatalf("err = nil; want not-found error")
	}
}

func TestIsTransientPath(t *testing.T) {
	root := t.TempDir()
	tmp := filepath.Join(root, "tmp")
	t.Setenv("TMPDIR", tmp)

	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(tmp, "guild"), true},
		{filepath.Join(tmp, "nested", "deep", "guild"), true},
		{"/home/user/Library/Caches/go-build/ab/cdef", true},
		{filepath.Join(root, "bin", "guild"), false},
		{"/usr/local/bin/guild", false},
	}
	for _, tc := range cases {
		if got := isTransientPath(tc.path); got != tc.want {
			t.Errorf("isTransientPath(%q) = %v; want %v", tc.path, got, tc.want)
		}
	}
}

// TestDaemonInstallUninstall_Integration drives a REAL service manager.
//
// Double-guarded so `make check` and CI never load a system daemon:
// it skips unless GUILD_DAEMON_UNIT_INTEGRATION=1 is set, and skips
// when the platform/loader is unavailable. Run it manually with:
//
//	GUILD_DAEMON_UNIT_INTEGRATION=1 go test ./internal/install -run Integration -v
//
// Safety properties: the unit uses a test-only label (never the real
// guild label, so an actually-installed guild daemon is untouched) and
// execs the system 'true' binary instead of guild, so no guild daemon
// process is ever started by the test.
func TestDaemonInstallUninstall_Integration(t *testing.T) {
	if os.Getenv("GUILD_DAEMON_UNIT_INTEGRATION") != "1" {
		t.Skip("set GUILD_DAEMON_UNIT_INTEGRATION=1 to run against the real service manager")
	}

	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("'true' not on PATH: %v", err)
	}

	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("launchctl"); err != nil {
			t.Skipf("launchctl not on PATH: %v", err)
		}
		integrationDarwin(t, truePath)
	case "linux":
		if _, err := exec.LookPath("systemctl"); err != nil {
			t.Skipf("systemctl not on PATH: %v", err)
		}
		if err := exec.Command("systemctl", "--user", "list-units", "--no-pager").Run(); err != nil {
			t.Skipf("no systemd user manager reachable: %v", err)
		}
		integrationLinux(t, truePath)
	default:
		t.Skipf("no daemon unit support on %s", runtime.GOOS)
	}
}

func integrationDarwin(t *testing.T, truePath string) {
	t.Helper()
	const label = DaemonLabelDarwin + ".itest"
	t.Setenv("HOME", t.TempDir()) // plist lands in a throwaway LaunchAgents dir
	target := launchdDomain() + "/" + label
	t.Cleanup(func() {
		_ = exec.Command("launchctl", "bootout", target).Run() // best-effort
	})

	opts := DaemonUnitOptions{
		Out:          testWriter{t},
		goos:         "darwin",
		label:        label,
		executableFn: func() (string, error) { return truePath, nil },
	}
	res, err := DaemonInstall(opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !res.Loaded {
		t.Fatalf("Loaded = false; want true")
	}
	if err := exec.Command("launchctl", "print", target).Run(); err != nil {
		t.Fatalf("agent not visible to launchd after install: %v", err)
	}

	unres, err := DaemonUninstall(opts)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !unres.Removed || !unres.Unloaded {
		t.Fatalf("uninstall flags = removed=%v unloaded=%v; want both", unres.Removed, unres.Unloaded)
	}
	if err := exec.Command("launchctl", "print", target).Run(); err == nil {
		t.Fatalf("agent still loaded after uninstall")
	}
	if _, err := DaemonUninstall(opts); err != nil {
		t.Fatalf("second uninstall not idempotent: %v", err)
	}
}

func integrationLinux(t *testing.T, truePath string) {
	t.Helper()
	// No HOME/XDG override here: the systemd user manager only scans
	// its own configured unit dirs, so the test-labeled unit must land
	// in the real ~/.config/systemd/user. The test-only name keeps it
	// disjoint from any real guild unit and it is removed on the way out.
	const label = DaemonLabelLinux + "-itest"
	unitName := label + ".service"
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "--user", "disable", "--now", unitName).Run() // best-effort
	})

	opts := DaemonUnitOptions{
		Out:          testWriter{t},
		goos:         "linux",
		label:        label,
		executableFn: func() (string, error) { return truePath, nil },
	}
	res, err := DaemonInstall(opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !res.Loaded {
		t.Fatalf("Loaded = false; want true")
	}
	if err := exec.Command("systemctl", "--user", "is-enabled", unitName).Run(); err != nil {
		t.Fatalf("unit not enabled after install: %v", err)
	}

	unres, err := DaemonUninstall(opts)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !unres.Removed {
		t.Fatalf("Removed = false; want true")
	}
	if err := exec.Command("systemctl", "--user", "is-enabled", unitName).Run(); err == nil {
		t.Fatalf("unit still enabled after uninstall")
	}
	if _, err := DaemonUninstall(opts); err != nil {
		t.Fatalf("second uninstall not idempotent: %v", err)
	}
}

// testWriter forwards command output into the test log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}
