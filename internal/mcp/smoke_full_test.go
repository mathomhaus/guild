package mcp

// TestTools_ArgVariantSmoke exercises every registered MCP tool with a
// non-trivial value per ArgSpec, using command.SynthArgValues to generate
// the input map. This catches ArgSpec.Type ↔ struct-field-kind mismatches
// and runtime setField-style bugs that the default zero-value smoke test
// (TestTools_SmokeRoundTrip) misses.
//
// Regression gate: commit b6ae7e0 (2026-04-19) fixed a panic in lore_meld
// where Threshold was declared ArgString but the struct field was float64.
// The existing SmokeRoundTrip passed because it sent an empty Threshold,
// which skipped the strconv.ParseFloat call. This test ensures that non-
// empty values reach the handler for every ArgSpec, surfacing such bugs
// before they reach users.
//
// Assertion contract (mirrors TestTools_SmokeRoundTrip):
//   - No Go panic in the SDK handler (protocol/schema reject is acceptable).
//   - If IsError=true, body must contain "[error]" or "[fatal]" prefix.

import (
	"context"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/quest"
)

// toolArgSpecs maps each MCP tool name to its ArgSpec slice. The two
// bootstrap tools (guild_session_start, guild_set_project) and
// quest_bounties are bespoke (not registry-generated), so they get
// hand-written minimal args below.
var toolArgSpecs = map[string][]command.ArgSpec{
	// --- lore ---
	"lore_appraise": lore.AppraiseCommand.Args,
	"lore_catalog":  lore.CatalogCommand.Args,
	"lore_commune":  lore.CommuneCommand.Args,
	"lore_dossier":  lore.DossierCommand.Args,
	"lore_echoes":   lore.EchoesCommand.Args,
	"lore_inquest":  lore.InquestCommand.Args,
	"lore_inscribe": lore.InscribeCommand.Args,
	"lore_link":     lore.LinkCommand.Args,
	"lore_unlink":   lore.UnlinkCommand.Args,
	"lore_list":     lore.ListCommand.Args,
	"lore_meld":     lore.MeldCommand.Args,
	"lore_oath":     lore.OathCommand.Args,
	"lore_reforge":  lore.ReforgeCommand.Args,
	"lore_seal":     lore.SealCommand.Args,
	"lore_study":    lore.StudyCommand.Args,
	"lore_update":   lore.UpdateCommand.Args,
	"lore_ripples":  lore.RipplesCommand.Args,
	"lore_whispers": lore.WhispersCommand.Args,
	// --- quest ---
	"quest_accept":   quest.AcceptCommand.Args,
	"quest_active":   quest.ActiveCommand.Args,
	"quest_brief":    quest.BriefCommand.Args,
	"quest_campfire": quest.CampfireCommand.Args,
	"quest_clear":    quest.ClearCommand.Args,
	"quest_epic":     quest.EpicCommand.Args,
	"quest_forfeit":  quest.ForfeitCommand.Args,
	"quest_fulfill":  quest.FulfillCommand.Args,
	"quest_guild":    quest.GuildCommand.Args,
	"quest_journal":  quest.JournalCommand.Args,
	"quest_list":     quest.ListCommand.Args,
	"quest_orders":   quest.OrdersCommand.Args,
	"quest_post":     quest.PostCommand.Args,
	"quest_pulse":    quest.PulseCommand.Args,
	"quest_scroll":   quest.ScrollCommand.Args,
	"quest_summon":   quest.SummonCommand.Args,
	"quest_search":   quest.SearchCommand.Args,
	"quest_update":   quest.UpdateCommand.Args,
}

// flexIntOverrides provides integer values for FlexInt64 input fields.
// FlexInt64 fields are declared ArgString in the ArgSpec (for CLI path
// coercion) but the MCP JSON schema requires an integer or digit-only
// string ("^-?[0-9]+$"). Using a bare integer sidesteps schema
// validation and lets the handler actually run, which is what we want
// for execution-time coverage.
var flexIntOverrides = map[string]map[string]any{
	"lore_study":   {"entry_id": 999999},
	"lore_seal":    {"entry_id": 999999},
	"lore_update":  {"entry_id": 999999},
	"lore_link":    {"from_id": 999998, "to_id": 999999},
	"lore_unlink":  {"from_id": 999998, "to_id": 999999},
	"lore_reforge": {"old_id": 999998, "new_id": 999999},
}

func TestTools_ArgVariantSmoke(t *testing.T) {
	// isolateProject sets $HOME, registers testproj, and activates it
	// so downstream tools can open the DBs.
	isolateProject(t)

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, client, cleanup := connectInMemory(t, s)
	defer cleanup()

	for _, e := range expectedTools {
		name := e.name
		t.Run(name, func(t *testing.T) {
			args := variantArgsFor(name)
			res, err := client.CallTool(context.Background(),
				&sdkmcp.CallToolParams{Name: name, Arguments: args})
			if err != nil {
				// A protocol-layer reject (schema validation, unexpected
				// property, etc.) means the SDK rejected the call before
				// the handler ran. No panic occurred — that's the
				// no-panic assertion passing. Log and move on.
				t.Logf("%s: protocol-layer reject (acceptable): %v", name, err)
				return
			}
			body := textOf(res.Content)
			if res.IsError {
				// Domain errors are expected for most tools (no such
				// quest, entry not found, etc.). Verify the recoverable-
				// error contract: every IsError=true response must carry
				// an [error] or [fatal] prefix.
				if !strings.Contains(body, "[error]") && !strings.Contains(body, "[fatal]") {
					t.Errorf("%s: IsError=true but missing [error]/[fatal] prefix: %q",
						name, body)
				}
			}
			// No panic occurred reaching this point — the primary goal of
			// this test is satisfied. We don't assert on success vs. error
			// shape beyond the prefix contract above.
		})
	}
}

// variantArgsFor builds the argument map for each tool using
// command.SynthArgValues, then applies tool-specific overrides:
//
//  1. project="testproj" is always injected so tools that call
//     ResolveProj don't fail on missing-project before exercising their
//     input paths.
//  2. guild_session_start and guild_set_project get minimal args
//     (project only — they have no ArgSpec registry entry).
//  3. quest_bounties is bespoke (not registry-generated); use the same
//     minimal args as smokeArgsFor.
//  4. FlexInt64 fields get integer overrides (see flexIntOverrides).
//  5. lore_meld gets threshold="0.7" to exercise the ParseFloat path
//     that b6ae7e0 fixed — the regression gate for this whole test.
//  6. quest_campfire needs at least one of hypothesis/tried/next set to
//     avoid the "at least one required" domain error.
func variantArgsFor(name string) map[string]any {
	switch name {
	case "guild_session_start":
		return map[string]any{"project": "testproj"}
	case "guild_set_project":
		return map[string]any{"project": "testproj"}
	case "quest_bounties":
		return map[string]any{"project": "testproj"}
	}

	specs, ok := toolArgSpecs[name]
	if !ok {
		// Unknown tool — return minimal base so the call reaches the
		// bootstrap resolver at minimum.
		return map[string]any{"project": "testproj"}
	}

	args := command.SynthArgValues(specs)

	// Always set project so ResolveProj resolves to the registered project.
	args["project"] = "testproj"

	// Apply FlexInt64 overrides so those fields receive integers rather
	// than the "QUEST-1"/"ENTRY-1" strings that SynthArgValues produces
	// for ArgString/_id fields. The MCP JSON schema for FlexInt64 accepts
	// integer|string(digit-only), so "QUEST-1" would fail schema
	// validation; integers always pass.
	if overrides, ok := flexIntOverrides[name]; ok {
		for k, v := range overrides {
			args[k] = v
		}
	}

	// lore_meld: exercise the threshold parse path — the regression gate
	// for commit b6ae7e0. SynthArgValues emits "x" for the threshold
	// ArgString field; override with a valid numeric string so the handler
	// actually calls strconv.ParseFloat and we confirm no panic.
	if name == "lore_meld" {
		args["threshold"] = "0.7"
	}

	// quest_campfire: the handler returns an error if all optional fields
	// are empty. Ensure hypothesis is set so the handler proceeds past
	// the domain guard and exercises the full write path.
	if name == "quest_campfire" {
		args["hypothesis"] = "test hypothesis"
		// quest_id is already "QUEST-1" from SynthArgValues (ends in _id).
	}

	return args
}
