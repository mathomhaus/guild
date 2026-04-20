// Package install implements `guild init` — per-project scaffold.
//
// Flow: detect repo + project name, show a checklist of planned actions,
// prompt once (Y/n) unless --yes or stdin is not a TTY, then execute.
// AGENTS.md writes are idempotent: append only if the section marker is absent.
package install

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/quest"
	"github.com/mathomhaus/guild/internal/storage"
	"github.com/mattn/go-isatty"
)

// agentsSectionMarker is the heading that makes AGENTS.md merges idempotent.
const agentsSectionMarker = "## guild workflow"

// agentsMDTemplate is the section appended to AGENTS.md. Carries the
// critical autonomy rules directly so clients that don't surface the MCP
// instructions field still deliver them. <NAME> is replaced at runtime.
const agentsMDTemplate = `## guild workflow

guild coordinates tasks (quest) and persistent knowledge (lore) across sessions and agents.

**BEFORE ANY OTHER ACTION** — before reading files, editing code, or
responding to the user — call the MCP tool ` + "`guild_session_start(project=\"<NAME>\")`" + `.
It returns the full agent contract, active principles (oath), and the
current top bounty. Follow what it returns.

If ` + "`guild_session_start`" + ` is not visible in your tool list, run your
host's tool-search for ` + "`guild`" + ` first — some hosts lazy-load MCP tools.
Do NOT fall back to CLI; the MCP server is available.

### Core rules (full contract is returned by session_start)

- **Never use built-in task tools** (TaskCreate / TaskUpdate / TaskList) —
  they're session-scoped. Use ` + "`quest_post`" + ` / ` + "`quest_accept`" + ` / ` + "`quest_list`" + ` instead.
- **Accept before working on a quest** — ` + "`quest_accept(quest_id=...)`" + ` prevents
  parallel-agent collisions.
- **Appraise before researching** — ` + "`lore_appraise(query=..., all_projects=true)`" + `
  first. If current entries exist, use them.
- **Brief before session end** — when wrapping up or compaction is near,
  call ` + "`quest_brief(\"what was done, what's next, gotchas\")`" + ` without being asked.

MCP namespace: ` + "`mcp__guild__*`" + `. CLI fallback: ` + "`guild --help`" + ` (last resort only).
`

// renderSection returns the AGENTS.md template with <NAME> replaced.
func renderSection(projectName string) string {
	return strings.ReplaceAll(agentsMDTemplate, "<NAME>", projectName)
}

// InitOptions controls how Init behaves.
type InitOptions struct {
	// Yes accepts all defaults non-interactively (CI mode).
	Yes bool
	// DryRun prints the plan without executing any writes or DB registration.
	DryRun bool
	// PrintAgentsMD emits only the template snippet to Out (for piping).
	PrintAgentsMD bool

	// Out is where output is written. Defaults to os.Stdout when nil.
	Out io.Writer
	// In is the reader used for the interactive prompt. Defaults to os.Stdin.
	In io.Reader

	// LoreDBPath overrides the default ~/.guild/lore.db path. Used by tests.
	LoreDBPath string
	// QuestDBPath overrides the default ~/.guild/quest.db path. Used by tests.
	QuestDBPath string

	// clients overrides detected MCP clients in tests. Nil → use detectHosts().
	clients []Client
	// execCmdFn is passed through to MCPInstall when invoking MCP registration.
	// Nil → real exec.Command. Injected in tests to capture the registration call.
	execCmdFn func(name string, arg ...string) *exec.Cmd
	// executableFn is passed through to MCPInstall for binary-path resolution.
	// Nil → os.Executable. Injected in tests so CI runners (which have no
	// durable guild binary installed) can resolve to a temp-file path and
	// avoid the "binary not found in any durable location" error.
	executableFn func() (string, error)
}

// InitResult carries what Init accomplished so callers can inspect or log.
type InitResult struct {
	ProjectName  string
	RepoPath     string
	AgentsMDPath string
	// Written is true when AGENTS.md was created or appended.
	Written bool
	// DBRegistered is true when the project was registered in lore.db + quest.db.
	DBRegistered bool
}

// Init registers the git repo rooted at repoRoot in both lore.db and quest.db,
// and creates or merges a guild section into AGENTS.md, based on opts.
//
// repoRoot must be an absolute path to a git work-tree root.
func Init(ctx context.Context, repoRoot string, opts InitOptions) (*InitResult, error) {
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.In == nil {
		opts.In = os.Stdin
	}

	projectName := filepath.Base(repoRoot)
	if projectName == "" || projectName == "." || projectName == "/" {
		return nil, fmt.Errorf("install: cannot derive project name from %q", repoRoot)
	}

	// --print-agents-md: emit template only, no other output.
	if opts.PrintAgentsMD {
		fmt.Fprint(opts.Out, renderSection(projectName))
		return &InitResult{ProjectName: projectName, RepoPath: repoRoot}, nil
	}

	agentsPath := filepath.Join(repoRoot, "AGENTS.md")
	result := &InitResult{
		ProjectName:  projectName,
		RepoPath:     repoRoot,
		AgentsMDPath: agentsPath,
	}

	// Determine what AGENTS.md action is needed.
	renderedSection := renderSection(projectName)
	agentsAction, err := agentsMDAction(agentsPath, renderedSection)
	if err != nil {
		return nil, fmt.Errorf("install: check AGENTS.md: %w", err)
	}

	// Detect which MCP host(s) are present. opts.clients lets tests
	// inject a fake set without running real detection.
	var detected []Client
	if opts.clients != nil {
		for _, c := range opts.clients {
			if c.Detected() {
				detected = append(detected, c)
			}
		}
	} else {
		detected = detectHosts()
	}
	binPath := "guild"
	if exe, err := resolveAbsBinPath(os.Executable); err == nil {
		binPath = exe
	}

	// Print the plan.
	fmt.Fprintf(opts.Out, "guild init — %s (project: %s)\n\n", repoRoot, projectName)
	fmt.Fprintln(opts.Out, "Will perform:")
	if !opts.DryRun {
		fmt.Fprintf(opts.Out, "  [✓] register %q in ~/.guild/ databases\n", projectName)
	} else {
		fmt.Fprintf(opts.Out, "  [?] register %q in ~/.guild/ databases\n", projectName)
	}
	switch agentsAction {
	case agentsCreate:
		fmt.Fprintln(opts.Out, "  [?] AGENTS.md — not found → create")
	case agentsAppend:
		fmt.Fprintln(opts.Out, "  [?] AGENTS.md — found, guild section absent → append")
	case agentsRefresh:
		fmt.Fprintln(opts.Out, "  [?] AGENTS.md — guild section present but outdated → refresh")
	case agentsSkip:
		fmt.Fprintln(opts.Out, "  [✓] AGENTS.md — guild section up-to-date → skip")
	}
	if len(detected) > 0 {
		for _, c := range detected {
			fmt.Fprintf(opts.Out, "  [?] register guild MCP — detected: %s\n        command: %s\n",
				c.Name, c.InstallCmdDisplay(binPath))
		}
	} else {
		fmt.Fprintln(opts.Out, "  [?] register guild MCP — no host detected; see `guild mcp install` for options")
	}
	fmt.Fprintln(opts.Out)

	if opts.DryRun {
		fmt.Fprintln(opts.Out, "dry-run: no changes made.")
		return result, nil
	}

	// Determine whether to prompt.
	shouldPrompt := !opts.Yes && isInteractiveTTY(opts.In)
	if shouldPrompt {
		fmt.Fprint(opts.Out, "Continue? [Y/n] ")
		reader := bufio.NewReader(opts.In)
		answer, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("install: read prompt: %w", err)
		}
		answer = strings.TrimSpace(answer)
		if answer != "" && !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
			fmt.Fprintln(opts.Out, "aborted.")
			return result, nil
		}
		fmt.Fprintln(opts.Out)
	}

	// --- DB registration -------------------------------------------------------
	loreDBPath, questDBPath, err := resolveDBPaths(opts)
	if err != nil {
		return nil, err
	}

	loreDB, err := storage.Open(ctx, loreDBPath)
	if err != nil {
		return nil, fmt.Errorf("install: open lore.db: %w", err)
	}
	defer func() { _ = loreDB.Close() }()
	if err := storage.Migrate(ctx, loreDB, "lore"); err != nil {
		return nil, fmt.Errorf("install: migrate lore.db: %w", err)
	}

	questDB, err := storage.Open(ctx, questDBPath)
	if err != nil {
		return nil, fmt.Errorf("install: open quest.db: %w", err)
	}
	defer func() { _ = questDB.Close() }()
	if err := storage.Migrate(ctx, questDB, "quest"); err != nil {
		return nil, fmt.Errorf("install: migrate quest.db: %w", err)
	}

	restore := lore.SwapGitToplevelForTest(func(_ context.Context, _ string) (string, error) {
		return repoRoot, nil
	})
	_, err = lore.InitFrom(ctx, loreDB, repoRoot)
	lore.SwapGitToplevelForTest(restore)
	if err != nil {
		return nil, fmt.Errorf("install: lore init: %w", err)
	}

	if _, err := quest.Init(ctx, questDB, projectName, repoRoot, ""); err != nil {
		return nil, fmt.Errorf("install: quest init: %w", err)
	}
	result.DBRegistered = true
	fmt.Fprintf(opts.Out, "  ✓ registered %q in lore + quest\n", projectName)

	// --- AGENTS.md -------------------------------------------------------------
	switch agentsAction {
	case agentsCreate:
		if err := os.WriteFile(agentsPath, []byte(renderedSection), 0o644); err != nil { //nolint:gosec // G306: 0o644 correct for project docs
			return nil, fmt.Errorf("install: write AGENTS.md: %w", err)
		}
		result.Written = true
		fmt.Fprintln(opts.Out, "  ✓ created AGENTS.md")
	case agentsAppend:
		if err := appendSection(agentsPath, renderedSection); err != nil {
			return nil, fmt.Errorf("install: append AGENTS.md: %w", err)
		}
		result.Written = true
		fmt.Fprintln(opts.Out, "  ✓ appended guild section to AGENTS.md")
	case agentsRefresh:
		if err := refreshSection(agentsPath, renderedSection); err != nil {
			return nil, fmt.Errorf("install: refresh AGENTS.md: %w", err)
		}
		result.Written = true
		fmt.Fprintln(opts.Out, "  ✓ refreshed guild section in AGENTS.md (template updated)")
	case agentsSkip:
		fmt.Fprintln(opts.Out, "  ✓ AGENTS.md guild section up-to-date — skipped")
	}

	// --- MCP registration ------------------------------------------------------
	// Delegate to MCPInstall with Run: true so each detected client gets a
	// per-client [Y/n] confirm and the registration command actually executes.
	// No-client path keeps the manual-setup hint.
	if len(detected) > 0 {
		fmt.Fprintln(opts.Out)
		execFn := opts.executableFn
		if execFn == nil {
			execFn = os.Executable
		}
		mcpOpts := MCPInstallOptions{
			Run:          true,
			Yes:          opts.Yes,
			Out:          opts.Out,
			In:           opts.In,
			clients:      detected,
			executableFn: execFn,
			execCmdFn:    opts.execCmdFn,
		}
		if _, err := MCPInstall(ctx, mcpOpts); err != nil {
			return nil, fmt.Errorf("install: mcp register: %w", err)
		}
	} else {
		fmt.Fprintln(opts.Out)
		fmt.Fprintln(opts.Out, "  [i] no MCP client detected — see `guild mcp install` for manual setup")
	}

	fmt.Fprintln(opts.Out)
	fmt.Fprintln(opts.Out, "Next: open this repo in your AI agent — it will read AGENTS.md and bootstrap guild on first use.")
	_ = projectName

	return result, nil
}

// agentsMDVerb indicates what AGENTS.md action is needed.
type agentsMDVerb int

const (
	agentsCreate  agentsMDVerb = iota // file absent → create
	agentsAppend                      // file present, marker absent → append
	agentsRefresh                     // marker present, content drifted from current template → replace
	agentsSkip                        // marker present, content matches current template → no-op
)

// agentsMDAction inspects agentsPath and reports what action is required
// to reconcile the file with expectedSection. It returns agentsSkip only
// when the existing section's content byte-matches expectedSection after
// trailing-whitespace normalization.
func agentsMDAction(agentsPath, expectedSection string) (agentsMDVerb, error) {
	data, err := os.ReadFile(agentsPath)
	if os.IsNotExist(err) {
		return agentsCreate, nil
	}
	if err != nil {
		return agentsCreate, err
	}
	start, end := findGuildSection(data)
	if start < 0 {
		return agentsAppend, nil
	}
	current := strings.TrimRight(string(data[start:end]), "\n")
	want := strings.TrimRight(expectedSection, "\n")
	if current == want {
		return agentsSkip, nil
	}
	return agentsRefresh, nil
}

// findGuildSection returns the byte offsets [start, end) of the guild
// section in data, or (-1, -1) if the marker heading is absent. end is
// either the first byte of the next top-level H2 heading or len(data).
func findGuildSection(data []byte) (start, end int) {
	marker := []byte(agentsSectionMarker)
	idx := bytes.Index(data, marker)
	if idx < 0 {
		return -1, -1
	}
	if idx > 0 && data[idx-1] != '\n' {
		return -1, -1
	}
	afterHeader := idx + len(marker)
	next := bytes.Index(data[afterHeader:], []byte("\n## "))
	if next < 0 {
		return idx, len(data)
	}
	return idx, afterHeader + next + 1
}

// refreshSection replaces the existing guild section in the file at path
// with section, preserving everything outside the section's byte range.
func refreshSection(path, section string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read existing file: %w", err)
	}
	start, end := findGuildSection(data)
	if start < 0 {
		return fmt.Errorf("guild section not found in %s", path)
	}
	body := strings.TrimRight(section, "\n") + "\n"
	var merged string
	switch {
	case end >= len(data):
		merged = string(data[:start]) + body
	default:
		merged = string(data[:start]) + body + string(data[end:])
	}
	return os.WriteFile(path, []byte(merged), 0o644) //nolint:gosec // G306: 0o644 correct for project docs
}

// appendSection appends the guild section to the existing file at path.
func appendSection(path, section string) error {
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read existing file: %w", err)
	}
	merged := strings.TrimRight(string(existing), "\n") + "\n\n" + section
	return os.WriteFile(path, []byte(merged), 0o644) //nolint:gosec // G306: 0o644 correct for project docs
}

// detectHosts returns the subset of Clients that are detected on this machine.
func detectHosts() []Client {
	var found []Client
	for _, c := range Clients {
		if c.Detected() {
			found = append(found, c)
		}
	}
	return found
}

// resolveDBPaths returns the lore.db and quest.db paths, creating ~/.guild/ as needed.
func resolveDBPaths(opts InitOptions) (loreDB, questDB string, err error) {
	loreDB = opts.LoreDBPath
	questDB = opts.QuestDBPath
	if loreDB != "" && questDB != "" {
		return loreDB, questDB, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("install: resolve home dir: %w", err)
	}
	guildDir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(guildDir, 0o755); err != nil {
		return "", "", fmt.Errorf("install: create ~/.guild: %w", err)
	}
	if loreDB == "" {
		loreDB = filepath.Join(guildDir, "lore.db")
	}
	if questDB == "" {
		questDB = filepath.Join(guildDir, "quest.db")
	}
	return loreDB, questDB, nil
}

// isInteractiveTTY reports whether r is a real terminal.
func isInteractiveTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// IsInteractiveTTYStdin reports whether os.Stdin is a real terminal.
// Used by init_cmd.go to auto-switch to --yes mode when piped.
func IsInteractiveTTYStdin() bool {
	return isInteractiveTTY(os.Stdin)
}
