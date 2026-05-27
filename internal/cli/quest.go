package cli

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/config"
	"github.com/mathomhaus/guild/internal/guildpath"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/lore/embed"
	"github.com/mathomhaus/guild/internal/project"
	"github.com/mathomhaus/guild/internal/quest"
	"github.com/mathomhaus/guild/internal/storage"
	"github.com/mathomhaus/guild/internal/telemetry"
)

// questDBPath returns the absolute path to ~/.guild/quest.db. Kept as
// a package-level var so tests that set HOME=<tmpdir> transparently
// redirect; tests that need an in-memory DB set questDBPathOverride.
var questDBPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".guild", "quest.db"), nil
}

// questDBPathOverride, when non-empty, replaces questDBPath's return
// value. Set by tests that need an in-memory DB or a tmpdir file.
// Concurrent quest commands in one test-scope share the same db file;
// that matches real-world single-process behavior.
var questDBPathOverride string

// openQuestDB opens + migrates the quest DB. Wrapped so every quest
// subcommand gets the same handle construction and the same ctx-threaded
// migrate call. Returns a *sql.DB the caller Closes.
func openQuestDB(ctx context.Context) (*sql.DB, error) {
	path := questDBPathOverride
	if path == "" {
		p, err := questDBPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	if path != ":memory:" && !strings.HasPrefix(path, ":memory:") {
		if dir := filepath.Dir(path); dir != "." && dir != "/" {
			if err := guildpath.EnsureDir(dir); err != nil {
				return nil, fmt.Errorf("cli: %w", err)
			}
		}
	}
	db, err := storage.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := guildpath.TightenDBPerms(path); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cli: %w", err)
	}
	if err := storage.Migrate(ctx, db, "quest"); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// --- emoji helpers ---

type emojiSink struct{ noEmoji bool }

func newEmojiSink(cfg *config.Config) emojiSink {
	if cfg == nil {
		return emojiSink{}
	}
	return emojiSink{noEmoji: cfg.NoEmoji}
}

// line renders "emoji prefix" + text. When noEmoji is true, emoji is
// substituted with plain. We keep this as two args instead of one
// because several lines have different plain vs emoji widths.
func (s emojiSink) line(emoji, plain, text string) string {
	if s.noEmoji {
		if plain == "" {
			return text
		}
		return plain + " " + text
	}
	if emoji == "" {
		return text
	}
	return emoji + " " + text
}

func (s emojiSink) updated(id string) string {
	return s.line(emojiUpdated, asciiUpdated, "updated "+id)
}

func (s emojiSink) registered(pid, path string) string {
	return s.line(emojiSuccess, asciiSuccess, "registered project '"+pid+"' at "+path)
}

// --- shared flag state (populated at init()) ---

var (
	qProject string
)

// resetQuestFlagState zeroes the package-level flag vars between
// cobra.Execute calls. Without this, tests that invoke multiple quest
// commands in sequence inherit leftover flag values. Production code
// runs once-per-process so this isn't needed there.
func resetQuestFlagState() {
	qProject = ""
	// Registry-managed slice flags (post --files/--acceptance/--depends-on,
	// epic quest_ids, campfire --tried, update --replace-*, etc.) live
	// inside pflag. pflag's Set implementation appends for slice flags,
	// so multi-Execute tests must wipe between runs.
	command.ResetCobraSliceFlags(questCmd)
}

// --- subcommands ---

var (
	questInitCmd = &cobra.Command{
		Use: "init", Short: "register this repo as a guild project",
		Args: cobra.NoArgs, RunE: runQuestInit,
	}
)

func init() {

	// Universal flags. We attach at the quest-group level so every
	// subcommand inherits. Using PersistentFlags on questCmd keeps
	// per-subcommand flag sets clean.
	// Note: --no-emoji and --no-usage-log are declared on rootCmd as
	// global persistent flags; quest subcommands inherit them from there.
	questCmd.PersistentFlags().StringVarP(&qProject, "project", "p", "", "project name (overrides CWD detection)")
	questCmd.PersistentFlags().Bool("no-usage-log", false, "skip usage.log write")

	questCmd.AddCommand(
		questInitCmd,
	)
	// Registry-generated verbs. Each BindCobra call produces the
	// cobra.Command from the co-located spec (internal/quest/*_cmd.go)
	// and attaches it under questCmd. wrapTelemetry restores per-call
	// usage-log recording that the registry adapter doesn't do (surface
	// concern, not verb concern). See docs/architecture/COMMAND_REGISTRY.md.
	deps := buildCLICommandDeps()
	bindRegistryVerb(questCmd, quest.AcceptCommand, deps, "quest accept")
	bindRegistryVerb(questCmd, quest.FulfillCommand, deps, "quest fulfill")
	bindRegistryVerb(questCmd, quest.ForfeitCommand, deps, "quest forfeit")
	bindRegistryVerb(questCmd, quest.JournalCommand, deps, "quest journal")
	bindRegistryVerb(questCmd, quest.BriefCommand, deps, "quest brief")
	bindRegistryVerb(questCmd, quest.ActiveCommand, deps, "quest active")
	bindRegistryVerb(questCmd, quest.SummonCommand, deps, "quest summon")
	bindRegistryVerb(questCmd, quest.OrdersCommand, deps, "quest orders")
	bindRegistryVerb(questCmd, quest.CampfireCommand, deps, "quest campfire")
	bindRegistryVerb(questCmd, quest.EpicCommand, deps, "quest campaign")
	bindRegistryVerb(questCmd, quest.PostCommand, deps, "quest post")
	bindRegistryVerb(questCmd, quest.UpdateCommand, deps, "quest update")
	bindRegistryVerb(questCmd, quest.ScrollCommand, deps, "quest scroll")
	bindRegistryVerb(questCmd, quest.ListCommand, deps, "quest list")
	bindRegistryVerb(questCmd, quest.GuildCommand, deps, "quest guild")
	bindRegistryVerb(questCmd, quest.PulseCommand, deps, "quest pulse")
	bindRegistryVerb(questCmd, quest.SearchCommand, deps, "quest search")
}

// bindRegistryVerb attaches a Command registry spec to parent and wraps
// its RunE with telemetry. Helper collapses three lines per verb into
// one so the init() stays scannable as the migration grows.
func bindRegistryVerb[I, O any](parent *cobra.Command, spec *command.Command[I, O], deps command.Deps, telemetryLabel string) {
	spec.BindCobra(parent, deps)
	wrapTelemetry(parent, spec.CLIPath[len(spec.CLIPath)-1], telemetryLabel)
}

// wrapTelemetry decorates the named subcommand's RunE with a telemetry
// defer so registry-migrated verbs keep emitting the same usage.log
// rows as the hand-written ones. The wrapper reads cfg from cmd.Flags()
// at run time (cfg isn't stable at Bind time) and records duration +
// outcome after the original RunE returns.
func wrapTelemetry(parent *cobra.Command, name, subcmd string) {
	var sub *cobra.Command
	for _, c := range parent.Commands() {
		if c.Name() == name {
			sub = c
			break
		}
	}
	if sub == nil || sub.RunE == nil {
		return
	}
	original := sub.RunE
	sub.RunE = func(cc *cobra.Command, args []string) (rerr error) {
		ctx := cc.Context()
		if ctx == nil {
			ctx = cc.Root().Context()
		}
		start := time.Now()
		cfg, cfgErr := loadCfg(cc)
		defer func() {
			if cfgErr != nil {
				return
			}
			recordTelemetry(ctx, cfg, cfg.Project, subcmd, start, &rerr)
		}()
		return original(cc, args)
	}
}

// buildCLICommandDeps constructs the Deps bundle for CLI-side command
// registry adapters. Consolidates DB open + project resolution behind
// the surface-neutral Deps shape so Handlers stay ignorant of cobra.
// Embed is wired via wireQuestEmbedDeps so `guild quest search` reaches
// the RRF arm when quest corpus coverage is at or above 0.90 (QUEST-259).
// The per-process Index build cost (~50-200 ms for a ~250-quest corpus)
// is accepted for the CLI surface: the maintainer decided CLI parity
// with MCP is worth the startup overhead.
func buildCLICommandDeps() command.Deps {
	d := command.Deps{
		OpenDB: openQuestDB,
		ResolveProj: func(ctx context.Context, argProject string) (string, error) {
			db, err := openQuestDB(ctx)
			if err != nil {
				return "", err
			}
			defer func() { _ = db.Close() }()
			p, err := project.Resolve(ctx, db, strings.TrimSpace(argProject))
			if err != nil {
				return "", err
			}
			return p.ID, nil
		},
		Now:        time.Now,
		OpenLoreDB: openLoreDB,
	}
	// command.Deps.Embed is `any`; assigning a typed-nil pointer would
	// produce a non-nil interface value and defeat questEmbedFromDeps's
	// nil guard. Assign only when wiring yielded a real *QuestEmbedDeps.
	if e := wireQuestEmbedDeps(); e != nil {
		d.Embed = e
	}
	return d
}

// wireQuestEmbedDeps builds a *quest.QuestEmbedDeps for the CLI surface.
// It mirrors wireCLIEmbedDeps (lore_read.go) but targets the quest corpus:
//  1. Borrows the lore-side Embedder via lore.WireEmbedDeps (the same
//     embedder that generated quest_vectors; no second extraction needed).
//  2. Opens quest.db, verifies quest.embedder_state == "enabled".
//  3. Builds a QuestCorpus Index and loads vectors from quest.db.
//  4. Returns a *quest.QuestEmbedDeps or nil on any failure.
//
// Nil is the BM25-only fallback path; quest_search tolerates it.
// Uses Async=false and LoadIndex=false for the lore side because this
// call is purely for borrowing the Embedder: the quest-side Index is
// built here against quest.db, not lore.db.
func wireQuestEmbedDeps() *quest.QuestEmbedDeps {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Borrow the lore-side embedder. WireEmbedDeps returns nil when the
	// embedder state is not "enabled" or assets are not bundled.
	loreDB, err := openLoreDB(ctx)
	if err != nil {
		return nil
	}
	defer func() { _ = loreDB.Close() }()
	loreDeps, _, _ := lore.WireEmbedDeps(ctx, loreDB, lore.EmbedWireOptions{
		Async:     false,
		LoadIndex: false,
		Logger:    newCLILogger(),
	})
	if loreDeps == nil || !loreDeps.Enabled() {
		return nil
	}

	// Verify quest corpus embedder_state is "enabled" before loading the index.
	qdb, qerr := openQuestDB(ctx)
	if qerr != nil {
		return nil
	}
	defer func() { _ = qdb.Close() }()

	stateKey := embed.QuestCorpus{}.MetaKey(embed.FieldEmbedderState)
	var state string
	if scanErr := qdb.QueryRowContext(ctx,
		`SELECT COALESCE(value, '') FROM meta WHERE key = ?`, stateKey,
	).Scan(&state); scanErr != nil || state != "enabled" {
		return nil
	}

	modelID := loreDeps.ModelID
	idx := embed.NewIndex(embed.QuestCorpus{}, modelID, embed.WithLogger(newCLILogger()))
	if _, loadErr := idx.LoadFromDB(ctx, qdb); loadErr != nil {
		return nil
	}

	return &quest.QuestEmbedDeps{
		Embedder: loreDeps.Embedder,
		Index:    idx,
		ModelID:  modelID,
	}
}

// --- shared helpers ---

func loadCfg(cmd *cobra.Command) (*config.Config, error) {
	return config.Load(cmd.Flags())
}

func resolveProject(ctx context.Context, db *sql.DB, cfg *config.Config) (*project.Project, error) {
	return project.Resolve(ctx, db, strings.TrimSpace(cfg.Project))
}

// recordTelemetry is defer-friendly. errPtr is a *error so defer can
// read the runner's named return value after the function has assigned
// it. Passing by pointer is the idiomatic pattern for this defer trick;
// gocritic flags it because by-value is usually nicer, but we need the
// late read, so keep the pointer. CLI invocations pass 0 for respBytes
// (no structured response body on the CLI surface).
//
//nolint:gocritic // ptrToRefParam — defer must observe the late-bound error
func recordTelemetry(ctx context.Context, cfg *config.Config, projectID, sub string, start time.Time, errPtr *error) {
	exit := 0
	if errPtr != nil && *errPtr != nil {
		exit = 1
	}
	// Cancelled context → skip (telemetry.Record already handles that).
	_ = telemetry.Record(ctx, cfg, projectID, sub, exit, time.Since(start), 0)
}

func ctxFromCmd(cmd *cobra.Command) context.Context {
	if c := cmd.Context(); c != nil {
		return c
	}
	return context.Background()
}

// --- runners ---

func runQuestInit(cmd *cobra.Command, _ []string) (rerr error) {
	ctx := ctxFromCmd(cmd)
	start := time.Now()
	cfg, err := loadCfg(cmd)
	if err != nil {
		return err
	}
	defer recordTelemetry(ctx, cfg, cfg.Project, "quest init", start, &rerr)

	db, err := openQuestDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	name := strings.TrimSpace(cfg.Project)
	var path string
	if name == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		top, err := project.DefaultResolver.GitToplevel(ctx, wd)
		if err != nil {
			return fmt.Errorf("not inside a git repository (cwd: %s)", wd)
		}
		path = top
		name = filepath.Base(top)
	} else {
		if existing, err := project.LookupByName(ctx, db, name); err == nil {
			path = existing.Path
		} else {
			wd, werr := os.Getwd()
			if werr != nil {
				return fmt.Errorf("--project %q requires an existing registration or CWD", name)
			}
			path = wd
		}
	}

	p, err := quest.Init(ctx, db, name, path, "TASKS.md")
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), newEmojiSink(cfg).registered(p.ID, p.Path))
	return nil
}
