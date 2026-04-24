// Package install — mcp_install.go implements the MCPInstall orchestrator.
//
// MCPInstall detects all MCP clients present on the machine and prints the
// recommended `mcp add` command for each. It never writes client config files
// directly — it delegates to each client's official CLI instead.
//
// Default UX (no flags):
//
//	guild binary: /usr/local/bin/guild
//
//	Detected agent clients:
//	  ✓ Claude Code
//	  ✗ Cursor  (not detected)
//
//	Run the command for each agent you use:
//
//	  # Claude Code
//	  claude mcp add guild --scope user -- /usr/local/bin/guild mcp serve
//
// With --run: shells out to each detected client's CLI with a per-command
// confirmation prompt.
//
// With --run --yes: shells out without prompting.
//
// With --print-config: prints only the JSON snippet for manual paste.
package install

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mattn/go-isatty"
)

// MCPInstallOptions controls the MCPInstall orchestrator.
type MCPInstallOptions struct {
	// PrintConfig writes only the manual JSON snippet to Out; no detection output.
	PrintConfig bool

	// Run shells out to each detected client's CLI (with per-command confirmation
	// unless Yes is also true).
	Run bool

	// Yes skips per-command confirmation when combined with Run.
	Yes bool

	// Skill is a placeholder for future Claude Code skill installation.
	Skill bool

	// Out is where banner lines are printed (defaults to os.Stdout).
	Out io.Writer

	// In is the reader used for interactive prompts (defaults to os.Stdin).
	In io.Reader

	// clients overrides Clients in tests. Nil → use Clients.
	clients []Client

	// executableFn resolves the running binary path. Defaults to os.Executable.
	executableFn func() (string, error)

	// execCmdFn creates an exec.Cmd for shelling out. Defaults to exec.Command.
	// Injected in tests to capture or simulate CLI invocations.
	execCmdFn func(name string, arg ...string) *exec.Cmd

	// lookPathFn resolves a binary name via PATH. Defaults to exec.LookPath.
	// Injected in tests so fixtures that use synthetic argv[0] names
	// ("claude", "cursor") don't need those binaries on the runner's PATH.
	lookPathFn func(string) (string, error)
}

// ClientInstruction is the computed install command for one detected client.
type ClientInstruction struct {
	Name string
	Cmd  string   // shell-safe display string for printing / confirmation prompts
	Argv []string // structured argv for exec; never re-parsed from Cmd
}

// MCPInstallResult reports what was done / printed.
type MCPInstallResult struct {
	// Instructions are the CLI commands printed for detected clients.
	Instructions []ClientInstruction
	// NotDetected is the list of client names not found on this machine.
	NotDetected []string
	// Ran is the list of client names that were executed via --run.
	Ran []string
	// SkippedMissingCLI is the list of client names that were detected
	// via a config-file probe but whose CLI binary was not on PATH at
	// --run time. Attempting to exec them would fail, so we skip the
	// install step and leave the user to install the CLI first.
	SkippedMissingCLI []string
}

// MCPInstall detects MCP clients, prints the recommended install commands,
// and optionally shells out to each client's CLI (--run).
func MCPInstall(ctx context.Context, opts MCPInstallOptions) (*MCPInstallResult, error) {
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.executableFn == nil {
		opts.executableFn = os.Executable
	}
	if opts.execCmdFn == nil {
		opts.execCmdFn = exec.Command
	}
	if opts.lookPathFn == nil {
		opts.lookPathFn = exec.LookPath
	}

	// --skill stub (not yet implemented).
	if opts.Skill {
		return nil, fmt.Errorf("not yet implemented: skill install")
	}

	// Resolve the running binary's absolute path.
	binPath, err := resolveAbsBinPath(opts.executableFn)
	if err != nil {
		return nil, fmt.Errorf("mcp install: resolve binary path: %w", err)
	}

	// --print-config: emit only the manual JSON snippet and return.
	if opts.PrintConfig {
		if err := printManualSnippet(opts.Out, binPath); err != nil {
			return nil, fmt.Errorf("mcp install: print-config: %w", err)
		}
		return &MCPInstallResult{}, nil
	}

	clients := opts.clientList()

	// Partition into detected / not-detected.
	var detected []Client
	var notDetected []string
	for _, c := range clients {
		if c.Detected() {
			detected = append(detected, c)
		} else {
			notDetected = append(notDetected, c.Name)
		}
	}

	result := &MCPInstallResult{NotDetected: notDetected}

	// Print binary path header.
	fmt.Fprintf(opts.Out, "guild binary: %s\n", binPath)
	fmt.Fprintln(opts.Out)

	// Detection summary — in --run mode, collapse to a one-liner (the
	// full command previews and manual JSON block are noise when the
	// user has already committed to shelling out). QUEST-10.
	if opts.Run {
		var names []string
		for _, c := range detected {
			names = append(names, c.Name)
		}
		if len(names) == 0 {
			fmt.Fprintln(opts.Out, "No MCP client detected; install one and re-run `guild mcp install`.")
			return result, nil
		}
		fmt.Fprintf(opts.Out, "Detected: %s\n\n", strings.Join(names, ", "))
	} else {
		fmt.Fprintln(opts.Out, "Detected agent clients:")
		for _, c := range clients {
			if c.Detected() {
				fmt.Fprintf(opts.Out, "  ✓ %s\n", c.Name)
			} else {
				fmt.Fprintf(opts.Out, "  ✗ %s  (not detected)\n", c.Name)
			}
		}
		fmt.Fprintln(opts.Out)
	}

	if len(detected) == 0 {
		fmt.Fprintln(opts.Out, "No MCP client detected; install one and re-run `guild mcp install`.")
		fmt.Fprintln(opts.Out)
		printManualBlock(opts.Out, binPath, clients)
		return result, nil
	}

	// Build instruction list. Argv is canonical; Cmd is derived for display only.
	var instructions []ClientInstruction
	for _, c := range detected {
		instructions = append(instructions, ClientInstruction{
			Name: c.Name,
			Argv: c.InstallArgv(binPath),
			Cmd:  c.InstallCmdDisplay(binPath),
		})
	}
	result.Instructions = instructions

	// Print recommended commands + manual JSON snippet — skipped in
	// --run mode where they'd just duplicate the upcoming prompts.
	if !opts.Run {
		fmt.Fprintln(opts.Out, "Run the command for each agent you use:")
		fmt.Fprintln(opts.Out)
		for _, instr := range instructions {
			fmt.Fprintf(opts.Out, "  # %s\n", instr.Name)
			fmt.Fprintf(opts.Out, "  %s\n", instr.Cmd)
			fmt.Fprintln(opts.Out)
		}

		// Manual snippet for non-detected / other clients.
		printManualBlock(opts.Out, binPath, clients)
	}

	// --run: shell out to each detected client's CLI with confirmation.
	if opts.Run {
		if !opts.Yes && !isInteractive(opts.In) {
			fmt.Fprintln(opts.Out, "stdin is not a TTY; use --run --yes to skip prompts")
			return result, nil
		}

		for _, instr := range instructions {
			// Argv is pre-split; never re-parse Cmd with strings.Fields,
			// which would shred binary paths that contain spaces.
			if len(instr.Argv) == 0 {
				continue
			}

			// A client can pass Detected() via its config-file probe even
			// when the CLI we're about to exec isn't on PATH. Running the
			// install would then fail with an opaque "exec: <name>: not
			// found" error. Short-circuit with a one-line notice so the
			// user knows which CLI to install (issue #48).
			binaryName := instr.Argv[0]
			if _, err := opts.lookPathFn(binaryName); err != nil {
				fmt.Fprintf(opts.Out, "skipping %s: %s not on PATH\n", instr.Name, binaryName)
				result.SkippedMissingCLI = append(result.SkippedMissingCLI, instr.Name)
				continue
			}

			if !opts.Yes {
				fmt.Fprintf(opts.Out, "Run: %s  [y/N] ", instr.Cmd)
				scanner := bufio.NewScanner(opts.In)
				scanner.Scan()
				answer := strings.TrimSpace(scanner.Text())
				if answer == "" || (!strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes")) {
					fmt.Fprintf(opts.Out, "skipped %s\n", instr.Name)
					continue
				}
			}

			//nolint:gosec // argv is composed from Clients[].InstallArgv + our own binPath
			cmd := opts.execCmdFn(instr.Argv[0], instr.Argv[1:]...)
			cmd.Stdout = opts.Out
			cmd.Stderr = opts.Out
			if err := cmd.Run(); err != nil {
				fmt.Fprintf(opts.Out, "error running %q: %v\n", instr.Cmd, err)
				continue
			}
			result.Ran = append(result.Ran, instr.Name)
		}
	} else {
		// Hint for --run.
		fmt.Fprintln(opts.Out, "Optional: run these commands for you (prompts per command):")
		fmt.Fprintln(opts.Out, "  guild mcp install --run")
	}

	return result, nil
}

// printManualBlock prints the manual JSON snippet for clients that don't
// have a detected CLI or for other MCP clients not in our list.
func printManualBlock(w io.Writer, binPath string, clients []Client) {
	// Collect names of clients without detected CLIs or not in list.
	var otherNames []string
	for _, c := range clients {
		if !c.Detected() {
			otherNames = append(otherNames, c.Name)
		}
	}
	extra := "Windsurf, Zed, VS Code extensions, etc."
	others := extra
	if len(otherNames) > 0 {
		others = strings.Join(otherNames, ", ") + ", " + extra
	}

	fmt.Fprintf(w, "For other MCP clients (%s),\n", others)
	fmt.Fprintln(w, "add this to their MCP config file:")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  {")
	fmt.Fprintln(w, `    "mcpServers": {`)
	fmt.Fprintln(w, `      "guild": {`)
	fmt.Fprintf(w, "        \"command\": %q,\n", binPath)
	fmt.Fprintln(w, `        "args": ["mcp", "serve"]`)
	fmt.Fprintln(w, "      }")
	fmt.Fprintln(w, "    }")
	fmt.Fprintln(w, "  }")
	fmt.Fprintln(w)
}

// printManualSnippet prints just the JSON snippet for --print-config.
func printManualSnippet(w io.Writer, binPath string) error {
	_, err := fmt.Fprintf(w, "{\n  \"mcpServers\": {\n    \"guild\": {\n      \"command\": %q,\n      \"args\": [\"mcp\", \"serve\"]\n    }\n  }\n}\n", binPath)
	return err
}

// clientList returns opts.clients when set (test injection), otherwise Clients.
func (o *MCPInstallOptions) clientList() []Client {
	if o.clients != nil {
		return o.clients
	}
	return Clients
}

// resolveAbsBinPath resolves the running binary to a durable absolute path.
// When the process was started via `go run`, os.Executable returns a path
// inside the Go build cache (e.g. /var/folders/.../go-build.../exe/guild).
// That path exists at call-time but is deleted when Go GCs the cache, so it
// must never be written into MCP client configs.
//
// Detection order for a durable path:
//  1. os.Executable → abs — accept only when the file exists AND is not under
//     the Go build cache.
//  2. $GOBIN/guild, $GOPATH/bin/guild — explicit install prefix probes.
//  3. exec.LookPath("guild") — PATH scan.
//
// Returns an error when no durable path is found so callers can surface a
// clear message rather than silently writing a cache path.
func resolveAbsBinPath(execFn func() (string, error)) (string, error) {
	raw, err := execFn()
	if err != nil {
		return "", fmt.Errorf("os.Executable: %w", err)
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("filepath.Abs(%q): %w", raw, err)
	}

	if _, statErr := os.Stat(abs); statErr == nil && !isGoBuildCache(abs) {
		return abs, nil
	}

	// Probe durable install locations before falling back to PATH scan.
	for _, candidate := range goBinCandidates() {
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, nil
		}
	}

	if found, lookErr := exec.LookPath("guild"); lookErr == nil {
		return found, nil
	}

	return "", fmt.Errorf("guild binary not found in any durable location; run `go install` or install via your package manager")
}

// isGoBuildCache reports whether p is inside the Go build cache.
func isGoBuildCache(p string) bool {
	// Fast string-based check for the well-known directory component name.
	if strings.Contains(p, "/go-build") {
		return true
	}
	// Check against the authoritative GOCACHE value when available.
	if gc := os.Getenv("GOCACHE"); gc != "" && strings.HasPrefix(p, gc) {
		return true
	}
	// Fallback: check against os.UserCacheDir()/go-build.
	if cacheDir, err := os.UserCacheDir(); err == nil {
		if strings.HasPrefix(p, filepath.Join(cacheDir, "go-build")) {
			return true
		}
	}
	return false
}

// goBinCandidates returns a list of paths where `guild` is likely installed
// when the developer has used `go install`.
func goBinCandidates() []string {
	var candidates []string
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		candidates = append(candidates, filepath.Join(gobin, "guild"))
	}
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		candidates = append(candidates, filepath.Join(gopath, "bin", "guild"))
	}
	// Honour the default GOPATH when GOPATH is unset.
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "go", "bin", "guild"))
	}
	return candidates
}

// isInteractive reports whether r is a real terminal (stdin TTY).
func isInteractive(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}
