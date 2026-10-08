package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/config"
	"github.com/mathomhaus/guild/internal/guildpath"
	"github.com/mathomhaus/guild/internal/hooks"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/project"
	"github.com/mathomhaus/guild/internal/storage"
	"github.com/mathomhaus/guild/internal/telemetry"
)

// loreDBPath returns the canonical lore.db location: ~/.guild/lore.db.
// Exposed as a variable so tests can stub it.
var loreDBPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cli: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".guild", "lore.db"), nil
}

// openLoreDB opens lore.db and applies migrations — the standard
// prelude for every `lore <verb>` CLI invocation. Each verb calls this
// after resolving its flags so migrations self-heal on binary upgrade.
func openLoreDB(ctx context.Context) (*sql.DB, error) {
	path, err := loreDBPath()
	if err != nil {
		return nil, err
	}
	if err := guildpath.EnsureDir(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("cli: %w", err)
	}
	db, err := storage.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := guildpath.TightenDBPerms(path); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cli: %w", err)
	}
	if err := storage.Migrate(ctx, db, "lore"); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// resolveProjectName resolves the caller's active project id, honoring
// --project and GUILD_PROJECT before falling back to git toplevel.
// Returns empty string when the resolver errors — callers decide
// whether empty is fatal for their verb.
func resolveProjectName(ctx context.Context, db *sql.DB, flag string) (string, error) {
	p, err := project.Resolve(ctx, db, flag)
	if err != nil {
		return "", err
	}
	return p.ID, nil
}

// loadCLIConfig builds the layered Config + applies the current
// command's flag set so --w-fts / --w-recency / --no-emoji survive.
func loadCLIConfig(cmd *cobra.Command) (*config.Config, error) {
	return config.Load(cmd.Flags())
}

// ---------------------------------------------------------------------------
// appraise
// ---------------------------------------------------------------------------

// Search loads an index; write commands use their separate lightweight wiring.
var wireAppraiseEmbedDeps = lore.WireEmbedDeps

type appraiseFlags struct {
	Project     string
	Limit       int
	AllProjects bool
	IncludeAll  bool
	Since       string
	WFTS        float64
	WRecency    float64
	// Hook-mode flags. --inject prints the bounded pointer-cite payload
	// for harness context injection; --query and --from-stdin-json are
	// its alternative query sources. See internal/hooks/payload.go for
	// the stdout contract.
	Inject        bool
	Query         string
	FromStdinJSON bool
}

// newAppraiseCmd builds `guild lore appraise QUERY ...`. The alias
// `check` is registered in init().
func newAppraiseCmd() *cobra.Command {
	var f appraiseFlags
	cmd := &cobra.Command{
		Use:     "appraise QUERY",
		Aliases: []string{"check"},
		Short:   "search lexical and semantic evidence candidates",
		// Hook mode (--inject) may source the query from --query or the
		// stdin JSON envelope, so the positional becomes optional there.
		// Every other invocation keeps the legacy at-least-one-arg rule.
		Args: func(cmd *cobra.Command, args []string) error {
			if inject, _ := cmd.Flags().GetBool("inject"); inject {
				return nil
			}
			return cobra.MinimumNArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAppraise(cmd, args, f)
		},
	}
	cmd.Flags().StringVarP(&f.Project, "project", "p", "", "project id (defaults to git toplevel)")
	cmd.Flags().IntVar(&f.Limit, "limit", 10, "max results")
	cmd.Flags().BoolVar(&f.AllProjects, "all-projects", true, "search across every project (default)")
	cmd.Flags().BoolVar(&f.IncludeAll, "all", false, "include archived/superseded entries")
	cmd.Flags().StringVar(&f.Since, "since", "", "only entries created within: Nd|Nw|Nm")
	cmd.Flags().Float64Var(&f.WFTS, "w-fts", 0, "override scoring weight for BM25 (0..1)")
	cmd.Flags().Float64Var(&f.WRecency, "w-recency", 0, "override scoring weight for recency (0..1)")
	cmd.Flags().BoolVar(&f.Inject, "inject", false, "hook mode: print a bounded top-3 pointer-cite payload for context injection")
	cmd.Flags().StringVar(&f.Query, "query", "", "query text (hook mode alternative to the positional QUERY)")
	cmd.Flags().BoolVar(&f.FromStdinJSON, "from-stdin-json", false, "read the harness hook JSON envelope from stdin and use its .prompt as the query (hook mode)")
	return cmd
}

// appraiseQuery resolves the search query for runAppraise. Hook mode
// (--inject) accepts three sources in priority order: --query, the
// stdin JSON envelope (--from-stdin-json), then the positional QUERY.
// Outside hook mode the hook-only flags are rejected and the joined
// positional args are the query, as before.
func appraiseQuery(cmd *cobra.Command, args []string, f appraiseFlags) (string, error) {
	if !f.Inject {
		if f.Query != "" || f.FromStdinJSON {
			return "", errors.New("--query and --from-stdin-json require --inject")
		}
		return strings.Join(args, " "), nil
	}
	switch {
	case f.Query != "" && f.FromStdinJSON:
		return "", errors.New("--query and --from-stdin-json are mutually exclusive")
	case f.Query != "":
		return f.Query, nil
	case f.FromStdinJSON:
		return hooks.PromptFromHookEnvelope(cmd.InOrStdin())
	case len(args) > 0:
		return strings.Join(args, " "), nil
	}
	return "", errors.New("appraise --inject needs a query: pass --query, --from-stdin-json, or a positional QUERY")
}

func runAppraise(cmd *cobra.Command, args []string, f appraiseFlags) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	start := time.Now()
	cfg, err := loadCLIConfig(cmd)
	if err != nil {
		return err
	}
	// Resolve the query before opening the DB so a malformed stdin
	// envelope fails fast with empty stdout (hook contract).
	query, err := appraiseQuery(cmd, args, f)
	if err != nil {
		return err
	}
	db, err := openLoreDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	var projectID string
	if !f.AllProjects || f.Project != "" {
		// Project-scoped: require a resolvable project.
		projectID, err = resolveProjectName(ctx, db, f.Project)
		if err != nil && !f.AllProjects {
			return err
		}
	}

	var since time.Duration
	if f.Since != "" {
		since, err = lore.ParseSince(f.Since)
		if err != nil {
			return err
		}
	}

	scoring := lore.ScoringConfig{
		WFTS:            cfg.Scoring.WFTS,
		WRecency:        cfg.Scoring.WRecency,
		HalfLifeDays:    cfg.Scoring.HalfLifeDays,
		TitleMatchBoost: cfg.Scoring.TitleMatchBoost,
		TitleTokenBoost: cfg.Scoring.TitleTokenBoost,
	}
	// Per-query overrides.
	if cmd.Flags().Changed("w-fts") {
		scoring.WFTS = f.WFTS
	}
	if cmd.Flags().Changed("w-recency") {
		scoring.WRecency = f.WRecency
	}

	// Search requires the in-memory vector index. Reuse this connection for
	// metadata and index loading; unavailable assets/runtime still select the
	// lexical fallback. CLI writes use separate wiring without index loading.
	embedDeps, _, _ := wireAppraiseEmbedDeps(ctx, db, lore.EmbedWireOptions{Async: false, LoadIndex: true, Logger: newCLILogger()})
	out, err := lore.Appraise(ctx, db, lore.AppraiseParams{
		Query:       query,
		Limit:       f.Limit,
		AllProjects: f.AllProjects && f.Project == "",
		Project:     projectID,
		Since:       since,
		IncludeAll:  f.IncludeAll,
		Scoring:     scoring,
		Now:         time.Now().UTC(),
		Embed:       embedDeps,
	})
	if err != nil {
		return err
	}

	w := cmd.OutOrStdout()
	if f.Inject {
		renderAppraiseInject(w, out)
	} else {
		renderAppraiseOutput(w, out, query, cfg)
	}

	if len(out.Results) == 0 {
		// Record the miss — query text IS logged here (privacy contract).
		recordProject := projectID
		if recordProject == "" {
			recordProject = "all"
		}
		_ = telemetry.RecordMiss(ctx, cfg, recordProject, query)
	}

	_ = telemetry.Record(ctx, cfg, projectID, "lore appraise", 0, time.Since(start), 0)
	return nil
}

// renderAppraiseInject writes the bounded pointer-cite payload for
// `lore appraise --inject`. Zero matches write nothing: empty stdout
// plus exit 0 is the hook contract, so the harness silently drops the
// inject. Pointer cites only; entry bodies never reach stdout.
func renderAppraiseInject(w io.Writer, out *lore.AppraiseOutput) {
	if len(out.Results) == 0 {
		return
	}
	lines := make([]hooks.AppraiseLine, 0, hooks.MaxAppraiseLines)
	for i, r := range out.Results {
		if i >= hooks.MaxAppraiseLines {
			break
		}
		age := 0
		if !r.Entry.CreatedAt.IsZero() {
			if d := int(time.Since(r.Entry.CreatedAt).Hours() / 24); d > 0 {
				age = d
			}
		}
		lines = append(lines, hooks.AppraiseLine{
			ID:      lore.EntryID(r.Entry.ID),
			Kind:    string(r.Entry.Kind),
			AgeDays: age,
			Title:   r.Entry.Title,
			Summary: r.Entry.Summary,
		})
	}
	if payload := hooks.RenderAppraise(lines, len(out.Results)); payload != "" {
		fmt.Fprintln(w, payload)
	}
}

func renderAppraiseOutput(w io.Writer, out *lore.AppraiseOutput, query string, cfg *config.Config) {
	emoji := pickEmoji(cfg, emojiAppraised, asciiAppraised)
	if len(out.Results) == 0 {
		fmt.Fprintf(w, "%s nothing found for %q — research needed\n", emoji, query)
		if out.MissHint != "" {
			fmt.Fprintf(w, "   %s\n", out.MissHint)
		}
		return
	}
	if out.ProjectCounts != nil {
		parts := make([]string, 0, len(out.ProjectCounts))
		for p, n := range out.ProjectCounts {
			parts = append(parts, fmt.Sprintf("%s: %d", p, n))
		}
		// Deterministic ordering (map iteration is random in Go).
		sortStrings(parts)
		fmt.Fprintf(w, "%s %d entry(ies) appraised across %d projects:\n   %s\n\n",
			emoji, len(out.Results), len(out.ProjectCounts), strings.Join(parts, " · "))
	} else {
		fmt.Fprintf(w, "%s %d entry(ies) appraised:\n\n", emoji, len(out.Results))
	}
	fmt.Fprintln(w, "  Evidence candidates; answer relevance is unverified.")
	for _, r := range out.Results {
		writeEntryBrief(w, r.Entry, out.ProjectCounts != nil)
		fmt.Fprintln(w)
	}
}

func writeEntryBrief(w io.Writer, e *lore.Entry, showProject bool) {
	age := ageString(e.CreatedAt)
	projectLabel := ""
	if showProject {
		projectLabel = e.ProjectID + "/"
	}
	fmt.Fprintf(w, "  %s  [%s%s · %s · %s]\n", lore.EntryID(e.ID), projectLabel, e.Kind, e.Status, age)
	fmt.Fprintf(w, "  %s\n", e.Title)
	summary := e.Summary
	if len(summary) > 200 {
		summary = summary[:200]
	}
	fmt.Fprintf(w, "  %s\n", summary)
	if len(e.Tags) > 0 {
		fmt.Fprintf(w, "  tags: %s\n", strings.Join(e.Tags, ","))
	}
	if e.FilePath != "" {
		fmt.Fprintf(w, "  file: %s\n", e.FilePath)
	}
	if e.Source != "" {
		fmt.Fprintf(w, "  source: %s\n", e.Source)
	}
}

// ---------------------------------------------------------------------------
// study
// ---------------------------------------------------------------------------

func newStudyCmd() *cobra.Command {
	var projectFlag string
	cmd := &cobra.Command{
		Use:     "study LORE-N",
		Aliases: []string{"show"},
		Short:   "full detail + linked entries graph",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			start := time.Now()
			cfg, err := loadCLIConfig(cmd)
			if err != nil {
				return err
			}
			id, err := parseEntryID(args[0])
			if err != nil {
				return err
			}
			db, err := openLoreDB(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			result, err := lore.Study(ctx, db, id)
			if err != nil {
				if errors.Is(err, lore.ErrEntryNotFound) {
					fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
					return err
				}
				return err
			}
			renderStudy(cmd.OutOrStdout(), result, cfg)
			_ = telemetry.Record(ctx, cfg, projectFlag, "lore study", 0, time.Since(start), 0)
			return nil
		},
	}
	cmd.Flags().StringVarP(&projectFlag, "project", "p", "", "project id (defaults to git toplevel)")
	return cmd
}

func renderStudy(w io.Writer, r *lore.StudyResult, cfg *config.Config) {
	statusIcon := studyStatusIcon(r.Entry.Status, cfg)
	fmt.Fprintln(w, strings.Repeat("=", 60))
	fmt.Fprintf(w, "  %s  %s [%s]\n", lore.EntryID(r.Entry.ID), statusIcon, strings.ToUpper(string(r.Entry.Status)))
	fmt.Fprintln(w, strings.Repeat("=", 60))
	fmt.Fprintf(w, "  Kind     : %s\n", r.Entry.Kind)
	fmt.Fprintf(w, "  Topic    : %s\n", r.Entry.Topic)
	fmt.Fprintf(w, "  Title    : %s\n", r.Entry.Title)
	age := int(time.Since(r.Entry.CreatedAt).Hours() / 24)
	if r.Entry.CreatedAt.IsZero() {
		age = 0
	}
	fmt.Fprintf(w, "  Age      : %d days\n", age)
	if r.Entry.ValidDays != nil && *r.Entry.ValidDays > 0 {
		remaining := *r.Entry.ValidDays - age
		label := fmt.Sprintf("%dd remaining", remaining)
		if remaining <= 0 {
			label = "STALE"
		}
		fmt.Fprintf(w, "  Valid    : %dd (%s)\n", *r.Entry.ValidDays, label)
	}
	if len(r.Entry.Tags) > 0 {
		fmt.Fprintf(w, "  Tags     : %s\n", strings.Join(r.Entry.Tags, ","))
	}
	if r.Entry.FilePath != "" {
		fmt.Fprintf(w, "  File     : %s\n", r.Entry.FilePath)
	}
	if r.Entry.Source != "" {
		fmt.Fprintf(w, "  Source   : %s\n", r.Entry.Source)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  SUMMARY")
	fmt.Fprintln(w, "  "+strings.Repeat("-", 40))
	fmt.Fprintf(w, "  %s\n\n", r.Entry.Summary)

	// BODY is the full verbatim source material. Printed only when present
	// so pre-009 entries (and body-less inscribes) keep the lean layout.
	// Not indented per-line: bodies are often multi-line code/notes where
	// reflowing would corrupt the content.
	if r.Entry.Body != "" {
		fmt.Fprintln(w, "  BODY")
		fmt.Fprintln(w, "  "+strings.Repeat("-", 40))
		fmt.Fprintf(w, "%s\n\n", r.Entry.Body)
	}

	if len(r.Linked) > 0 {
		fmt.Fprintln(w, "  LINKED ENTRIES")
		fmt.Fprintln(w, "  "+strings.Repeat("-", 40))
		for _, l := range r.Linked {
			arrow := "→"
			if l.Direction == lore.EdgeIncoming {
				arrow = "←"
			}
			fmt.Fprintf(w, "  %s %s [%s] %s (%s, %s)\n",
				arrow, lore.EntryID(l.Entry.ID), l.Relation, l.Entry.Title, l.Entry.Kind, l.Entry.Status)
		}
		fmt.Fprintln(w)
	}

	// Linked-entries footer for a clear top-1.
	if r.TopLinked != nil {
		arrow := "→"
		if r.TopLinked.Direction == lore.EdgeIncoming {
			arrow = "←"
		}
		fmt.Fprintf(w, "  top link: %s %s [%s] %s\n",
			arrow, lore.EntryID(r.TopLinked.Entry.ID), r.TopLinked.Relation, r.TopLinked.Entry.Title)
	}
}

func studyStatusIcon(s lore.Status, cfg *config.Config) string {
	noEmoji := cfg != nil && cfg.NoEmoji
	switch s {
	case lore.StatusCurrent, lore.StatusPromoted:
		return prefix(emojiSuccess, asciiSuccess, noEmoji)
	case lore.StatusStale:
		return prefix(emojiWarn, asciiWarn, noEmoji)
	case lore.StatusSuperseded:
		return prefix(emojiForfeited, asciiForfeited, noEmoji)
	case lore.StatusArchived:
		return prefix("📦", "[archived]", noEmoji)
	case lore.StatusSeed:
		return prefix(emojiWhispers, asciiWhispers, noEmoji)
	case lore.StatusExploring:
		return prefix("🔍", "[exploring]", noEmoji)
	case lore.StatusParked:
		return prefix("⏸️", "[parked]", noEmoji)
	case lore.StatusImported:
		return prefix("📥", "[imported]", noEmoji)
	}
	return "·"
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

// list migrated to internal/lore.ListCommand (QUEST-45).

// ---------------------------------------------------------------------------
// oath
// ---------------------------------------------------------------------------

// oath migrated to internal/lore.OathCommand (QUEST-45).

// ---------------------------------------------------------------------------
// echoes
// ---------------------------------------------------------------------------

// echoes + whispers migrated to internal/lore registry (QUEST-45).

// dossier migrated to internal/lore.DossierCommand (QUEST-45).

// ---------------------------------------------------------------------------
// Shared helpers (emoji, parsing, formatting)
// ---------------------------------------------------------------------------

// pickEmoji returns glyph when emoji is enabled, or plain when the user
// has opted out (--no-emoji / GUILD_NO_EMOJI).
func pickEmoji(cfg *config.Config, glyph, plain string) string {
	if cfg != nil && cfg.NoEmoji {
		return plain
	}
	return glyph
}

// parseEntryID accepts "LORE-N", "ENTRY-N" (legacy), "entry-N", or a
// bare "N" and returns the numeric id. CLI users type any form; MCP
// callers pass ints directly and skip this helper. Delegates to
// lore.ParseEntryID for consistency.
func parseEntryID(s string) (int64, error) {
	id, err := lore.ParseEntryID(s)
	if err != nil {
		return 0, fmt.Errorf("cli: %w", err)
	}
	return id, nil
}

// ageString returns "today" for <1 day, "Nd ago" otherwise.
func ageString(t time.Time) string {
	if t.IsZero() {
		return "today"
	}
	days := int(time.Since(t).Hours() / 24)
	if days <= 0 {
		return "today"
	}
	return fmt.Sprintf("%dd ago", days)
}

// sortStrings is a tiny helper that keeps renderAppraiseOutput's
// project-counts summary deterministic without pulling in
// sort.Strings at the top of this file for a single call site. It
// inlines insertion sort because the slice is always small (≤ project
// count, usually <10).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// init registers the lore read subcommands that keep a bespoke cobra
// implementation (appraise/study carry CLI-only hook-mode flags the
// registry spec does not). The registry-generated lore verbs are bound by
// the module-registry loop (bindModuleVerbs in modules.go).
func init() {
	loreCmd.AddCommand(
		newAppraiseCmd(),
		newStudyCmd(),
	)
}

// buildCLILoreDeps mirrors buildCLICommandDeps but opens lore.db.
// ExecRemote routes Handlers through a live, version-matched daemon
// when one is up, falling back to local execution on any transport
// failure. Embed defers wireCLIEmbedDeps to first LOCAL Handler use
// via cliLoreEmbedSource, so verbs that never touch the vector path
// (and daemon-routed runs, served by the daemon's shared embedder)
// skip the BGE probe cost entirely. Every lore handler tolerates a nil
// resolved *EmbedDeps per ADR-003 nil-safety.
func buildCLILoreDeps() command.Deps {
	return command.Deps{
		OpenDB:      openLoreDB,
		OpenQuestDB: openQuestDB,
		ResolveProj: func(ctx context.Context, argProject string) (string, error) {
			db, err := openLoreDB(ctx)
			if err != nil {
				return "", err
			}
			defer func() { _ = db.Close() }()
			p, err := project.Resolve(ctx, db, strings.TrimSpace(argProject))
			if err != nil {
				return "", wrapResolveHint(err)
			}
			return p.ID, nil
		},
		Now:           time.Now,
		LoreValidDays: cliLoreValidDays,
		ExecRemote:    remoteExecViaDaemon,
		Embed:         &cliLoreEmbedSource{},
	}
}

// cliLoreValidDays surfaces the merged [inscribe.valid_days] windows to
// lore write verbs via command.Deps.LoreValidDays. Evaluated lazily at
// handler invocation time, NOT at init() when the Deps bundle is built,
// so a HOME swapped by tests and config edits between invocations are
// both observed. Load errors fall back to nil (built-in kind defaults):
// config problems surface on the verbs that check Load's error rather
// than by blocking a write.
func cliLoreValidDays() map[string]int {
	cfg, err := config.Load(nil)
	if err != nil {
		return nil
	}
	return cfg.Inscribe.ValidDays
}

// wireCLIEmbedDeps opens lore.db once at CLI Deps construction time
// and returns the *lore.EmbedDeps (or nil) that every lore verb
// handler will see via command.Deps.Embed. Uses Async=false (short-
// lived process must not fire-and-forget Tx2) and LoadIndex=false
// because write handlers do not need a search index. The appraise CLI
// constructs its own dependencies with LoadIndex=true.
//
// Nil-return is the expected default on fresh clones, on Windows, and
// until the user has run `guild init` against a binary built with
// -tags=withembed. Downstream handlers branch to BM25+stopwords.
func wireCLIEmbedDeps() *lore.EmbedDeps {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := openLoreDB(ctx)
	if err != nil {
		return nil
	}
	defer func() { _ = db.Close() }()
	deps, _, _ := lore.WireEmbedDeps(ctx, db, lore.EmbedWireOptions{
		Async:     false,
		LoadIndex: false,
		Logger:    newCLILogger(),
	})
	return deps
}
