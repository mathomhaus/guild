// daemon.go implements `guild daemon install` / `guild daemon uninstall`:
// rendering and registering a login service for the guild daemon, a
// launchd agent on macOS, a systemd user unit on Linux.
//
// The file follows the data-driven shape of clients.go: everything
// platform-specific (label, unit path, template, loader argv) lives in a
// daemonUnit descriptor; the install/uninstall flows are generic over it.
// Like clients.go's ManualSnippet, a missing service-manager CLI
// (launchctl/systemctl off PATH) degrades to printing the rendered unit
// plus manual load instructions instead of failing.
//
// Scope guard: everything here is user-scoped: user launchd domain
// (gui/$UID), systemd --user, no sudo, no system daemons. The unit files
// live in OS-standard user directories with conventional permissions;
// only ~/.guild itself is private (0700, see internal/guildpath).
package install

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"
)

//go:embed templates/guild-daemon.plist.tmpl
var daemonPlistTemplate string

//go:embed templates/guild-daemon.service.tmpl
var daemonServiceTemplate string

// DaemonLabelDarwin is the launchd label of the guild daemon agent; the
// plist is written as <label>.plist under ~/Library/LaunchAgents.
const DaemonLabelDarwin = "com.mathomhaus.guild.daemon"

// DaemonLabelLinux is the systemd unit base name; the unit file is
// written as <label>.service under ~/.config/systemd/user (or
// $XDG_CONFIG_HOME/systemd/user when set).
const DaemonLabelLinux = "guild-daemon"

// ErrDaemonUnitsUnsupported is returned on platforms without a daemon
// login service (currently windows). The text matches the lifecycle
// verbs' ErrLifecycleUnsupported so every daemon verb fails with the
// same operator-facing line.
var ErrDaemonUnitsUnsupported = errors.New("daemon mode is not yet supported on this platform")

// unitCommand is one service-manager invocation. Commands run in order;
// a failing required command aborts the flow, while optional ones (e.g.
// unloading a unit that is not loaded) are tolerated for idempotency.
type unitCommand struct {
	argv []string
	// fallback is a legacy alternative tried when argv fails (e.g.
	// `launchctl load -w` for pre-bootstrap macOS). Empty means none.
	fallback []string
	// optional tolerates failure of argv (and fallback, when set).
	optional bool
}

// daemonUnit describes one platform's login service. All fields are
// data; render is the only method.
type daemonUnit struct {
	goos string

	// label identifies the unit to the service manager. On darwin it is
	// the launchd Label; on linux the systemd unit base name.
	label string

	// fileSuffix is appended to label to form the unit file name
	// (".plist" / ".service").
	fileSuffix string

	// unitDir returns the directory the unit file is written to.
	unitDir func() (string, error)

	// template is the text/template source of the unit file.
	template string

	// escape sanitizes values before template substitution (XML
	// entities for plist, %-specifier doubling for systemd).
	escape func(string) string

	// loader is the service-manager CLI probed via PATH. When absent,
	// install/uninstall degrade to the manual-instructions path.
	loader string

	// loadArgv/unloadArgv return the loader invocations for (re)loading
	// and unloading the unit.
	loadArgv   func(label, unitPath string) []unitCommand
	unloadArgv func(label, unitPath string) []unitCommand

	// manualLoad/manualUnload are the copy-paste instructions printed
	// when loader is not on PATH.
	manualLoad   func(label, unitPath string) string
	manualUnload func(label, unitPath string) string
}

// render executes the unit template with the escaped label + binary path.
func (u daemonUnit) render(label, binPath string) (string, error) {
	tmpl, err := template.New(u.label + u.fileSuffix).Parse(u.template)
	if err != nil {
		return "", fmt.Errorf("parse unit template: %w", err)
	}
	data := struct{ Label, BinPath string }{
		Label:   u.escape(label),
		BinPath: u.escape(binPath),
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("render unit template: %w", err)
	}
	return sb.String(), nil
}

// unitPath returns the unit file's destination for the given label.
func (u daemonUnit) unitPath(label string) (string, error) {
	dir, err := u.unitDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, label+u.fileSuffix), nil
}

// xmlEscape replaces the five XML entities so a binary path containing
// e.g. '&' cannot corrupt the plist.
func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}

// systemdEscape doubles '%' so a path containing one is not interpreted
// as a systemd specifier.
func systemdEscape(s string) string {
	return strings.ReplaceAll(s, "%", "%%")
}

// launchdDomain returns the per-user launchd domain target (gui/<uid>).
func launchdDomain() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

// daemonUnits is the per-platform registry, keyed by GOOS. Adding a
// platform means adding a descriptor here plus its template + golden.
var daemonUnits = map[string]daemonUnit{
	"darwin": {
		goos:       "darwin",
		label:      DaemonLabelDarwin,
		fileSuffix: ".plist",
		unitDir: func() (string, error) {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve home dir: %w", err)
			}
			return filepath.Join(home, "Library", "LaunchAgents"), nil
		},
		template: daemonPlistTemplate,
		escape:   xmlEscape,
		loader:   "launchctl",
		loadArgv: func(label, unitPath string) []unitCommand {
			domain := launchdDomain()
			return []unitCommand{
				// Drop an already-loaded copy first so a repeat install
				// reloads the freshly rendered plist instead of failing
				// with "service already bootstrapped". Optional: on the
				// first install there is nothing to drop.
				{argv: []string{"launchctl", "bootout", domain + "/" + label}, optional: true},
				{
					argv: []string{"launchctl", "bootstrap", domain, unitPath},
					// Pre-10.11 launchctl has no bootstrap subcommand.
					fallback: []string{"launchctl", "load", "-w", unitPath},
				},
			}
		},
		unloadArgv: func(label, unitPath string) []unitCommand {
			return []unitCommand{
				// Optional both ways: uninstalling a never-loaded (or
				// already-unloaded) agent must stay a no-op.
				{
					argv:     []string{"launchctl", "bootout", launchdDomain() + "/" + label},
					fallback: []string{"launchctl", "unload", "-w", unitPath},
					optional: true,
				},
			}
		},
		manualLoad: func(_, unitPath string) string {
			return "launchctl not found on PATH; load the agent manually once it is available:\n\n" +
				"  launchctl bootstrap gui/$(id -u) " + unitPath + "\n"
		},
		manualUnload: func(label, _ string) string {
			return "launchctl not found on PATH; unload the agent manually once it is available:\n\n" +
				"  launchctl bootout gui/$(id -u)/" + label + "\n"
		},
	},
	"linux": {
		goos:       "linux",
		label:      DaemonLabelLinux,
		fileSuffix: ".service",
		unitDir: func() (string, error) {
			// systemd user units live in $XDG_CONFIG_HOME/systemd/user,
			// defaulting to ~/.config/systemd/user. Resolved by hand
			// (not os.UserConfigDir) so the linux flow stays testable
			// from any development GOOS.
			if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
				return filepath.Join(xdg, "systemd", "user"), nil
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve home dir: %w", err)
			}
			return filepath.Join(home, ".config", "systemd", "user"), nil
		},
		template: daemonServiceTemplate,
		escape:   systemdEscape,
		loader:   "systemctl",
		loadArgv: func(label, _ string) []unitCommand {
			unit := label + ".service"
			return []unitCommand{
				{argv: []string{"systemctl", "--user", "daemon-reload"}},
				{argv: []string{"systemctl", "--user", "enable", "--now", unit}},
			}
		},
		unloadArgv: func(label, _ string) []unitCommand {
			unit := label + ".service"
			return []unitCommand{
				{argv: []string{"systemctl", "--user", "disable", "--now", unit}, optional: true},
				{argv: []string{"systemctl", "--user", "daemon-reload"}, optional: true},
			}
		},
		manualLoad: func(label, _ string) string {
			unit := label + ".service"
			return "systemctl not found on PATH (no systemd?); start the daemon with 'guild daemon start'\n" +
				"instead, or load the unit manually once systemd is available:\n\n" +
				"  systemctl --user daemon-reload\n" +
				"  systemctl --user enable --now " + unit + "\n"
		},
		manualUnload: func(label, _ string) string {
			unit := label + ".service"
			return "systemctl not found on PATH (no systemd?); unload the unit manually once systemd\n" +
				"is available:\n\n" +
				"  systemctl --user disable --now " + unit + "\n"
		},
	},
}

// DaemonUnitOptions configures DaemonInstall / DaemonUninstall. The
// zero value is production behavior on the current platform.
type DaemonUnitOptions struct {
	// Out is where progress lines are printed (defaults to os.Stdout).
	Out io.Writer

	// goos overrides runtime.GOOS so tests exercise the darwin and
	// linux flows from one development machine.
	goos string

	// label overrides the unit label + file name. Used by the guarded
	// integration test so a real loader invocation can never collide
	// with a genuinely installed guild unit.
	label string

	// executableFn resolves the running binary path. Defaults to os.Executable.
	executableFn func() (string, error)

	// lookPathFn resolves a binary name via PATH. Defaults to
	// exec.LookPath. Injected in tests both to force the manual path
	// (loader "absent") and to keep binary-path resolution hermetic.
	lookPathFn func(string) (string, error)

	// runCmdFn runs one loader command and returns its combined output.
	// Defaults to exec.Command(...).CombinedOutput. Injected in tests so
	// no launchctl/systemctl call ever leaves the test process.
	runCmdFn func(name string, arg ...string) ([]byte, error)
}

// fill applies the production defaults for unset fields.
func (o *DaemonUnitOptions) fill() {
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.goos == "" {
		o.goos = runtime.GOOS
	}
	if o.executableFn == nil {
		o.executableFn = os.Executable
	}
	if o.lookPathFn == nil {
		o.lookPathFn = exec.LookPath
	}
	if o.runCmdFn == nil {
		o.runCmdFn = func(name string, arg ...string) ([]byte, error) {
			//nolint:gosec // argv comes from the daemonUnits registry, not user input.
			return exec.Command(name, arg...).CombinedOutput()
		}
	}
}

// unitAndLabel resolves the platform descriptor and effective label.
func (o *DaemonUnitOptions) unitAndLabel() (daemonUnit, string, error) {
	unit, ok := daemonUnits[o.goos]
	if !ok {
		return daemonUnit{}, "", ErrDaemonUnitsUnsupported
	}
	label := unit.label
	if o.label != "" {
		label = o.label
	}
	return unit, label, nil
}

// DaemonInstallResult reports what DaemonInstall did.
type DaemonInstallResult struct {
	// UnitPath is where the unit file was written.
	UnitPath string
	// BinPath is the resolved guild binary path baked into the unit.
	BinPath string
	// Rendered is the unit file content.
	Rendered string
	// Loaded is true when the service manager accepted the unit.
	Loaded bool
	// Manual is true when the loader CLI was absent and manual
	// instructions were printed instead of loading.
	Manual bool
	// TransientWarning is true when BinPath looks transient (temp or
	// go-build directory) and the warning was printed.
	TransientWarning bool
}

// DaemonInstall renders the platform's login service unit for the guild
// daemon, writes it to the OS-standard user location, and loads it via
// the service manager (launchctl bootstrap / systemctl --user enable
// --now). Repeat installs re-render and reload. When the service
// manager CLI is not on PATH the rendered unit plus manual load
// instructions are printed and the install still succeeds.
//
// The daemon process itself is never spawned here: launchd/systemd own
// starting it (RunAtLoad / --now).
func DaemonInstall(opts DaemonUnitOptions) (*DaemonInstallResult, error) {
	opts.fill()
	unit, label, err := opts.unitAndLabel()
	if err != nil {
		return nil, err
	}

	binPath, transient, err := resolveDaemonBinPath(opts.executableFn, opts.lookPathFn)
	if err != nil {
		return nil, fmt.Errorf("resolve binary path: %w", err)
	}

	rendered, err := unit.render(label, binPath)
	if err != nil {
		return nil, err
	}
	unitPath, err := unit.unitPath(label)
	if err != nil {
		return nil, err
	}

	// Unit dirs (~/Library/LaunchAgents, ~/.config/systemd/user) are
	// OS-standard locations read by the service manager, so they get
	// conventional permissions, unlike the private ~/.guild (0700).
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return nil, fmt.Errorf("create unit directory: %w", err)
	}
	if err := os.WriteFile(unitPath, []byte(rendered), 0o644); err != nil { //nolint:gosec // unit file must be readable by the service manager.
		return nil, fmt.Errorf("write unit file: %w", err)
	}

	res := &DaemonInstallResult{
		UnitPath:         unitPath,
		BinPath:          binPath,
		Rendered:         rendered,
		TransientWarning: transient,
	}

	fmt.Fprintf(opts.Out, "guild daemon: unit written to %s\n", unitPath)
	fmt.Fprintf(opts.Out, "guild daemon: binary %s\n", binPath)
	if transient {
		fmt.Fprintf(opts.Out, "warning: the guild binary path looks transient (temp or go-build directory);\n"+
			"the login service will break when that directory is cleaned. Install guild to a\n"+
			"durable location and re-run 'guild daemon install'.\n")
	}

	if _, lookErr := opts.lookPathFn(unit.loader); lookErr != nil {
		res.Manual = true
		fmt.Fprintf(opts.Out, "\n%s\nrendered unit:\n\n%s", unit.manualLoad(label, unitPath), rendered)
		return res, nil
	}

	if err := runUnitCommands(opts.runCmdFn, unit.loadArgv(label, unitPath)); err != nil {
		return nil, fmt.Errorf("load unit: %w", err)
	}
	res.Loaded = true
	fmt.Fprintf(opts.Out, "guild daemon: loaded %s; the daemon now starts at login and is kept alive\n", label)
	fmt.Fprintf(opts.Out, "guild daemon: remove with 'guild daemon uninstall'\n")
	return res, nil
}

// DaemonUninstallResult reports what DaemonUninstall did.
type DaemonUninstallResult struct {
	// UnitPath is the unit file location that was targeted.
	UnitPath string
	// Removed is true when the unit file existed and was deleted.
	Removed bool
	// Unloaded is true when the service manager was asked to drop the
	// unit (regardless of whether it was loaded; the commands are
	// tolerant for idempotency).
	Unloaded bool
	// Manual is true when the loader CLI was absent and manual
	// instructions were printed instead of unloading.
	Manual bool
}

// DaemonUninstall reverses DaemonInstall: asks the service manager to
// drop the unit (launchctl bootout / systemctl --user disable --now)
// and removes the unit file. Idempotent: a missing file or a unit that
// was never loaded is not an error.
func DaemonUninstall(opts DaemonUnitOptions) (*DaemonUninstallResult, error) {
	opts.fill()
	unit, label, err := opts.unitAndLabel()
	if err != nil {
		return nil, err
	}
	unitPath, err := unit.unitPath(label)
	if err != nil {
		return nil, err
	}

	res := &DaemonUninstallResult{UnitPath: unitPath}

	existed := true
	if _, statErr := os.Stat(unitPath); statErr != nil {
		existed = false
	}

	if _, lookErr := opts.lookPathFn(unit.loader); lookErr == nil {
		if err := runUnitCommands(opts.runCmdFn, unit.unloadArgv(label, unitPath)); err != nil {
			return nil, fmt.Errorf("unload unit: %w", err)
		}
		res.Unloaded = true
		fmt.Fprintf(opts.Out, "guild daemon: unloaded %s\n", label)
	} else if existed {
		res.Manual = true
		fmt.Fprintf(opts.Out, "%s\n", unit.manualUnload(label, unitPath))
	}

	switch err := os.Remove(unitPath); {
	case err == nil:
		res.Removed = true
		fmt.Fprintf(opts.Out, "guild daemon: removed %s\n", unitPath)
	case os.IsNotExist(err):
		fmt.Fprintf(opts.Out, "guild daemon: no unit installed at %s\n", unitPath)
	default:
		return nil, fmt.Errorf("remove unit file: %w", err)
	}

	return res, nil
}

// runUnitCommands executes the loader invocations in order, honoring
// fallback and optional semantics.
func runUnitCommands(run func(string, ...string) ([]byte, error), cmds []unitCommand) error {
	for _, c := range cmds {
		out, err := run(c.argv[0], c.argv[1:]...)
		if err != nil && len(c.fallback) > 0 {
			out, err = run(c.fallback[0], c.fallback[1:]...)
		}
		if err != nil && !c.optional {
			return fmt.Errorf("%s: %w: %s", strings.Join(c.argv, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// resolveDaemonBinPath resolves the durable absolute path of the guild
// binary that the unit file will exec at every login.
//
// Unlike resolveAbsBinPath (mcp_install.go), a transient-only result is
// a warning, not an error: the unit is still written so the manual
// recovery is a re-run after installing guild durably, matching the
// "warn, do not block" acceptance for `guild daemon install`.
//
// Order:
//  1. os.Executable → abs → EvalSymlinks: accept when the file exists
//     outside temp/go-build directories.
//  2. $GOBIN/guild, $GOPATH/bin/guild, ~/go/bin/guild: durable install
//     prefix probes (shared goBinCandidates).
//  3. lookPath("guild"): PATH scan.
//  4. The transient path from step 1 with transient=true, when it at
//     least exists; otherwise an error.
func resolveDaemonBinPath(execFn func() (string, error), lookPathFn func(string) (string, error)) (path string, transient bool, err error) {
	raw, err := execFn()
	if err != nil {
		return "", false, fmt.Errorf("os.Executable: %w", err)
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", false, fmt.Errorf("filepath.Abs(%q): %w", raw, err)
	}
	abs = evalSymlinkOrFallback(abs)

	_, statErr := os.Stat(abs)
	if statErr == nil && !isTransientPath(abs) {
		return abs, false, nil
	}

	for _, candidate := range goBinCandidates() {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return evalSymlinkOrFallback(candidate), false, nil
		}
	}
	if found, lookErr := lookPathFn("guild"); lookErr == nil {
		return evalSymlinkOrFallback(found), false, nil
	}

	if statErr != nil {
		return "", false, fmt.Errorf("guild binary not found at %s or any durable location; run 'go install' or install via your package manager", abs)
	}
	return abs, true, nil
}

// isTransientPath reports whether p lives somewhere that is routinely
// cleaned out from under a login service: the Go build cache or the
// OS temp directory.
func isTransientPath(p string) bool {
	if isGoBuildCache(p) {
		return true
	}
	tmp := os.TempDir()
	if tmp == "" {
		return false
	}
	tmpAbs, err := filepath.Abs(tmp)
	if err != nil {
		return false
	}
	tmpAbs = evalSymlinkOrFallback(tmpAbs)
	rel, err := filepath.Rel(tmpAbs, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
