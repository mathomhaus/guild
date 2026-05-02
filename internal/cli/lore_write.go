package cli

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/config"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/project"
	"github.com/mathomhaus/guild/internal/telemetry"
)

// inscribe/update/seal/link/reforge flags migrated onto internal/lore
// registry specs (QUEST-45).

// loreDBPath and openLoreDB are declared in lore_read.go (package-shared
// between the read and write surfaces via a var-func seam + explicit
// MkdirAll).

// cmdCtx returns a context that cancels on SIGINT/SIGTERM. For CLI
// commands that finish in milliseconds the cancel plumbing is cheap
// insurance.
func cmdCtx() context.Context {
	return context.Background()
}

// subcommandKey is the canonical telemetry tag for a cobra.Command.
// Format: "lore.<verb>" so usage.log scanners can filter on prefix.
func subcommandKey(verb string) string { return "lore." + verb }

// withTelemetry wraps a cobra RunE so every CLI exit records a usage
// line (project, subcommand, exit code, duration). Failures to write
// telemetry are swallowed by telemetry.Record itself — callers never
// see a telemetry-caused error.
func withTelemetry(verb string, getProject func() string, run func(cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		start := time.Now()
		err := run(cmd, args)

		exit := 0
		if err != nil {
			exit = 1
		}

		// Telemetry config: best-effort load; Record handles cfg == nil.
		cfg, _ := config.Load(cmd.Flags())
		proj := getProject()
		_ = telemetry.Record(cmd.Context(), cfg, proj, subcommandKey(verb), exit, time.Since(start), 0)
		return err
	}
}

// resolveNoEmoji reads --no-emoji / GUILD_NO_EMOJI=1 via config.Load.
// Isolated because every write command has the same boilerplate.
func resolveNoEmoji(cmd *cobra.Command) bool {
	cfg, err := config.Load(cmd.Flags())
	if err != nil {
		return false
	}
	return cfg.NoEmoji
}

// resolveProjectID resolves the current project id via the standard CLI
// resolution order. Accepts an explicit --project flag value; "" →
// git-toplevel + lookup.
func resolveProjectID(ctx context.Context, db *sql.DB, flag string) (string, error) {
	p, err := project.Resolve(ctx, db, flag)
	if err != nil {
		return "", err
	}
	return p.ID, nil
}

// parseEntryID is declared in lore_read.go.

// splitTags parses a "a, b ,c" style string into a cleaned slice.
// Empty input → nil slice (not an empty slice with a single "" element).
func splitTags(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// --- cobra command definitions --------------------------------------------

// loreInitCmd registers the current git repo as a guild project. Idempotent.
var loreInitCmd = &cobra.Command{
	Use:   "init",
	Short: "register the current git repository as a guild project",
	Args:  cobra.NoArgs,
	RunE: withTelemetry("init", func() string { return "" }, func(cmd *cobra.Command, _ []string) error {
		ctx := cmdCtx()
		db, err := openLoreDB(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()

		res, err := lore.Init(ctx, db)
		if err != nil {
			return err
		}
		noEmoji := resolveNoEmoji(cmd)
		fmt.Fprintf(cmd.OutOrStdout(), "%s registered project %q at %s\n",
			prefix(emojiSuccess, asciiSuccess, noEmoji), res.Name, res.Path)
		return nil
	}),
}

// lore_inscribe migrated to internal/lore.InscribeCommand (QUEST-45).

// printDedupHits formats the cross-project dedup block. Kept
// as a free function so both CLI and MCP can reuse it.
func printDedupHits(w io.Writer, hits []lore.DedupHit, pid string, noEmoji bool) {
	fmt.Fprintf(w, "%s  similar entries found:\n", prefix(emojiWarn, asciiWarn, noEmoji))
	for _, h := range hits {
		proj := ""
		if h.ProjectID != pid {
			proj = fmt.Sprintf("  (%s)", h.ProjectID)
		}
		fmt.Fprintf(w, "   %s  [%s · %s]%s  %s\n",
			lore.EntryID(h.EntryID), string(h.Kind), string(h.Status), proj, h.Title)
	}
	fmt.Fprintf(w, "   -> If duplicate, run: lore reforge <OLD_ID> --with <NEW_ID>\n")
}

// lore_update migrated to internal/lore.UpdateCommand (QUEST-45).

// seal, link, reforge migrated to internal/lore registry (QUEST-45).

// legacy alias commands — Hidden: true so --help doesn't advertise them,
// but scripts still find them.
func newAliasCmd(alias, canonical string, target *cobra.Command) *cobra.Command {
	c := *target // shallow copy
	c.Use = alias + c.Use[len(canonical):]
	c.Hidden = true
	c.Aliases = nil
	return &c
}

func init() {
	// inscribe/update/seal/link/reforge flags live on their registry
	// specs (QUEST-45).
	loreCmd.AddCommand(
		loreInitCmd,
	)

	// Registry-migrated lore write verbs. See accompanying specs in
	// internal/lore/<verb>_cmd.go.
	deps := buildCLILoreDeps()
	bindLoreRegistryVerb(loreCmd, lore.InscribeCommand, deps, "lore inscribe")
	bindLoreRegistryVerb(loreCmd, lore.UpdateCommand, deps, "lore update")
	bindLoreRegistryVerb(loreCmd, lore.CatalogCommand, deps, "lore catalog")
	bindLoreRegistryVerb(loreCmd, lore.SealCommand, deps, "lore seal")
	bindLoreRegistryVerb(loreCmd, lore.LinkCommand, deps, "lore link")
	bindLoreRegistryVerb(loreCmd, lore.UnlinkCommand, deps, "lore unlink")
	bindLoreRegistryVerb(loreCmd, lore.ReforgeCommand, deps, "lore reforge")
}
