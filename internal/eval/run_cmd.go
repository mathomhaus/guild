package eval

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/config"
	"github.com/mathomhaus/guild/internal/lore"
)

// run_cmd.go declares the eval module's single verb as a command.Command, so
// the same spec generates both the `guild eval run` CLI subcommand and the
// `eval_run` MCP tool. Per ADR-006 these surface ONLY when the module is
// enabled ([modules].eval = true); with the default-off module nothing here
// mounts and the default surface is unchanged.
//
// The verb is self-contained: its Handler builds, seeds, ranks, and tears
// down its own isolated in-memory corpus (RunGrid / ComputeFixture). It reads
// no command.Deps database — the real ~/.guild is never touched — so the verb
// is safe to run anywhere, deterministic, and needs no project context.

// goldenFixture is the committed parity snapshot, embedded so the running
// binary can verify recall/ranking determinism against the locked baseline
// without a testdata directory at runtime.
//
//go:embed testdata/golden/parity.json
var goldenFixture []byte

// RunInput is the eval_run / `guild eval run` input. Both knobs default to
// the merged [eval] config so a silent invocation honors config; an explicit
// flag overrides it for a one-off run.
type RunInput struct {
	// Strict, when set, forces a non-zero exit / MCP error on any RED grid
	// cell or parity drift, overriding [eval].strict for this invocation.
	Strict bool `json:"strict,omitempty" jsonschema:"fail (non-zero exit) on any red cell or parity drift"`
	// SkipParity runs only the adversarial grid and omits the parity check.
	// Useful when iterating on the corpus before regenerating the fixture.
	SkipParity bool `json:"skip_parity,omitempty" jsonschema:"run only the adversarial grid, skip the golden-parity drift check"`
}

// RunReport is the verb's structured output: the grid result plus the parity
// outcome. Serialised for the --agent / MCP JSON envelope and rendered for
// humans by the formatters below.
type RunReport struct {
	Grid      GridResult      `json:"grid"`
	Relevance RelevanceReport `json:"relevance"`
	// ParityChecked is false when SkipParity was set.
	ParityChecked bool `json:"parity_checked"`
	// ParityDrift is "" when the live ranking matches the golden fixture, or
	// a one-line description of the first divergence otherwise.
	ParityDrift string `json:"parity_drift,omitempty"`
	// Failed reflects the strict-mode verdict: true when strict is on and the
	// run did not meet the green floor or parity drifted.
	Failed bool `json:"failed"`
}

// RunCommand is the eval module's sole verb spec.
var RunCommand = &command.Command[RunInput, RunReport]{
	Name:    "eval_run",
	CLIPath: []string{"eval", "run"},
	Short:   "run the adversarial recall/ranking grid and golden-parity check",
	Long: "Run guild's deterministic, no-LLM evaluation suite over the lore " +
		"recall/ranking pipeline: an adversarial grid (does a poisoned entry " +
		"outrank the genuine answer?) plus a golden-fixture parity check (did " +
		"the ranking pipeline drift since it was last locked?). Seeds and tears " +
		"down its own isolated in-memory corpus; never touches ~/.guild.",
	Args: []command.ArgSpec{
		{Name: "strict", Kind: command.ArgFlag, Type: command.ArgBool, Help: "fail (non-zero exit) on any red cell or parity drift"},
		{Name: "skip_parity", Kind: command.ArgFlag, Type: command.ArgBool, Help: "run only the adversarial grid, skip the golden-parity drift check"},
	},
	Handler:   runHandler,
	CLIFormat: func(s command.CLISink, o RunReport) string { return formatRun(s, o) },
	MCPFormat: func(s command.MCPSink, o RunReport) string { return formatRun(s, o) },
}

// runHandler executes the grid and (unless skipped) the parity check, then
// applies the strict-mode gate. It returns an error only in strict mode, so
// the default path reports verdicts and exits clean.
func runHandler(ctx context.Context, _ command.Deps, in RunInput) (RunReport, error) {
	cfg := loadEvalConfig()
	strict := cfg.Strict || in.Strict

	grid, err := RunGrid(ctx, lore.DefaultScoring())
	if err != nil {
		return RunReport{}, err
	}
	quality, err := RunRelevance(ctx, RelevanceOptions{})
	if err != nil {
		return RunReport{}, err
	}
	rep := RunReport{Grid: grid, Relevance: quality, ParityChecked: !in.SkipParity}

	if !in.SkipParity {
		drift, err := checkParity(ctx)
		if err != nil {
			return RunReport{}, err
		}
		rep.ParityDrift = drift
	}

	// Strict gate: a green floor on the grid plus zero parity drift.
	greenOK := grid.AllGreen()
	if cfg.MinGreen > 0 {
		greenOK = grid.GreenCount >= cfg.MinGreen
	}
	failed := !greenOK || rep.ParityDrift != "" || !quality.RelevancePassed()
	rep.Failed = failed && strict
	if rep.Failed {
		return rep, fmt.Errorf("eval: gate failed: %s", strictReason(grid, rep))
	}
	return rep, nil
}

// loadEvalConfig resolves the merged [eval] policy. It loads the kernel
// config and reads the eval section through ConfigFor (keyed by the loaded
// pointer), so a [eval] edit is honored without a code change. A config-load
// failure degrades to the built-in defaults rather than failing the verb: a
// broken config file must not make eval_run unusable, matching the
// swallow-and-degrade posture the MCP/CLI adapters take elsewhere. A test
// override short-circuits inside ConfigFor regardless of the load.
func loadEvalConfig() EvalConfig {
	cfg, err := config.Load(nil)
	if err != nil {
		return ConfigFor(nil)
	}
	return ConfigFor(cfg)
}

// checkParity computes the live fixture and diffs it against the committed
// golden, returning a one-line drift description ("" when identical).
func checkParity(ctx context.Context) (string, error) {
	var golden Fixture
	if err := json.Unmarshal(goldenFixture, &golden); err != nil {
		return "", fmt.Errorf("eval: parse embedded golden fixture: %w", err)
	}
	got, err := ComputeFixture(ctx)
	if err != nil {
		return "", err
	}
	return CompareFixtures(golden, got), nil
}

// strictReason renders the strict-mode failure cause for the returned error.
func strictReason(grid GridResult, rep RunReport) string {
	var parts []string
	if grid.RedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d grid cells red", grid.RedCount, grid.Total))
	}
	if rep.ParityDrift != "" {
		parts = append(parts, "parity drift: "+rep.ParityDrift)
	}
	if !rep.Relevance.RelevancePassed() {
		parts = append(parts, fmt.Sprintf("held-out quality below hit@5 85%% / MRR 0.80 floors (%.1f%% / %.3f)", 100*rep.Relevance.Metrics.HitAt5, rep.Relevance.Metrics.MRR))
	}
	if len(parts) == 0 {
		return "unmet green floor"
	}
	return strings.Join(parts, "; ")
}

// lineSink is the shared subset of CLISink / MCPSink the formatter needs, so
// one formatRun body renders both surfaces (the same pattern lore's
// formatInquest uses).
type lineSink interface {
	Line(glyph, ascii, text string) string
}

// formatRun renders the report identically on both surfaces.
func formatRun(s lineSink, o RunReport) string {
	var b strings.Builder
	glyph := "✅"
	ascii := "[green]"
	if o.Grid.RedCount > 0 || o.ParityDrift != "" {
		glyph, ascii = "🚨", "[red]"
	}
	b.WriteString(s.Line(glyph, ascii, fmt.Sprintf("eval grid: %d green, %d red of %d",
		o.Grid.GreenCount, o.Grid.RedCount, o.Grid.Total)))
	for _, v := range o.Grid.Verdicts {
		if v.Green {
			b.WriteString(s.Line("·", "  ", fmt.Sprintf("%s: green (answer %s at rank %d)",
				v.Probe, v.WantSlug, v.WantRank)))
			continue
		}
		b.WriteString(s.Line("✗", "  x", fmt.Sprintf("%s: red — %s", v.Probe, v.Reason)))
	}
	m := o.Relevance.Metrics
	fmt.Fprintf(&b, "held-out lexical: hit@1/5/10 %.1f/%.1f/%.1f%%; MRR %.3f; negative candidate rate %.1f%% (answerability unverified)\n", 100*m.HitAt1, 100*m.HitAt5, 100*m.HitAt10, m.MRR, 100*m.NegativeCandidateRate)
	if o.ParityChecked {
		if o.ParityDrift == "" {
			b.WriteString(s.Line("🔒", "[ok]", "parity: ranking matches golden fixture"))
		} else {
			b.WriteString(s.Line("⚠️", "[drift]", "parity: "+o.ParityDrift))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
