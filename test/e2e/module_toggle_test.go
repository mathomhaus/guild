package e2e

import (
	"context"
	"strings"
	"testing"
)

// TestE2EModuleToggle is the ADR-006 Phase 3 acceptance proof: disabling
// the lore module via GUILD_MODULE_LORE=0 removes it from EVERY surface in
// a real container — its MCP tools vanish from tools/list, its CLI verbs
// are gone from the command tree, and its INSTRUCTIONS fragment is absent
// — while quest and session keep working. It runs in both GUILD_E2E_MODE
// matrices (direct and daemon) like the golden scenarios, so the toggle is
// proven on the no-daemon path AND through the daemon shim.
//
// There is no golden transcript here: this scenario asserts STRUCTURAL
// absence/presence (set membership of tools and verbs), which is robust to
// the byte-level rendering the golden suite pins. The golden scenarios
// remain the parity oracle for the all-modules-on default; this one is the
// disable oracle.
//
// To run only this proof against a built image:
//
//	make docker-build
//	GUILD_E2E_DOCKER=1 GUILD_E2E_MODE=direct go test -count=1 -run TestE2EModuleToggle -v ./test/e2e/
//	GUILD_E2E_DOCKER=1 GUILD_E2E_MODE=daemon go test -count=1 -run TestE2EModuleToggle -v ./test/e2e/
//
// or via the Makefile convenience target:
//
//	make e2e-module-toggle
func TestE2EModuleToggle(t *testing.T) {
	requireE2E(t)
	ctx := context.Background()

	// Fresh container with lore disabled for every guild process it hosts.
	c := startContainer(ctx, t)
	c.env = []string{"GUILD_MODULE_LORE=0"}

	// init still registers the project (init runs DB setup regardless; the
	// toggle gates the SURFACE, not the on-disk databases) and, in daemon
	// mode, starts the in-container daemon with lore disabled.
	initOut := c.initProject(ctx, t)
	if !strings.Contains(initOut, "e2eproj") {
		t.Fatalf("init did not register the project:\n%s", initOut)
	}

	// ── MCP surface: lore tools absent, quest/session tools present ──────
	s := c.openSession(ctx, t)
	ir := s.initialize()

	tools := s.listToolNames()
	toolSet := toSet(tools)

	// Every lore_* tool must be gone.
	for _, name := range tools {
		if strings.HasPrefix(name, "lore_") {
			t.Errorf("lore module disabled but MCP tool %q is still advertised", name)
		}
	}
	// A representative lore tool is specifically absent.
	if toolSet["lore_inscribe"] || toolSet["lore_appraise"] {
		t.Error("lore tools (lore_inscribe / lore_appraise) must be absent from tools/list when lore is disabled")
	}
	// Quest + session surfaces still present.
	for _, must := range []string{"quest_post", "quest_accept", "quest_fulfill", "guild_session_start"} {
		if !toolSet[must] {
			t.Errorf("non-lore tool %q must still be advertised when only lore is disabled", must)
		}
	}

	// ── INSTRUCTIONS fragment: lore's contribution (if any) excluded ────
	// Core modules return Instructions()=="" today, so the contract is the
	// monolithic instructions.md either way; the exclusion seam is proven
	// by the unit tests (TestRemoveFragment / TestContractBody...). Here we
	// assert the contract is still delivered and well-formed with lore off
	// (the session start mandate survives), demonstrating the disable does
	// not corrupt the contract.
	if !strings.Contains(ir.Instructions, "guild_session_start") {
		t.Error("INSTRUCTIONS must still carry the session-start mandate with lore disabled")
	}

	// ── quest still works end to end with lore off ──────────────────────
	s.sessionStart("e2eproj")
	postOut := s.callTool("quest_post", map[string]any{
		"subject": "toggle-proof task",
	})
	if !strings.Contains(strings.ToLower(postOut), "quest") {
		t.Errorf("quest_post should still succeed with lore disabled:\n%s", postOut)
	}
	s.close()

	// ── CLI surface: lore registry verbs absent, quest verbs present ────
	// The lore MODULE's verbs are its registry-bound subcommands
	// (inscribe, oath, list, echoes, whispers, catalog, seal, link, ...),
	// the ones bindModuleVerbs mounts. Disabling lore drops every one of
	// them from `guild lore --help`. (The lore command GROUP itself, plus
	// the bespoke, hand-rolled CLI-only subcommands appraise/study/archive/
	// restore/init that predate the module system and are not part of the
	// module's Commands(), are outside the module loop and intentionally
	// survive; the module-bound verbs are the toggle's CLI surface.)
	loreHelp := c.guild(ctx, t, "lore", "--help")
	for _, verb := range []string{"inscribe", "oath", "echoes", "whispers", "catalog", "seal", "reforge"} {
		// Match the cobra help row "  <verb>   <desc>". Anchoring on the
		// two-space indent + verb + run of spaces avoids matching a
		// description that merely mentions the word.
		if strings.Contains(loreHelp, "\n  "+verb+" ") {
			t.Errorf("lore disabled but registry verb `lore %s` still mounted:\n%s", verb, loreHelp)
		}
	}

	// `guild quest --help` must still list quest's registry verbs.
	questHelp := c.guild(ctx, t, "quest", "--help")
	for _, verb := range []string{"post", "accept", "fulfill", "journal"} {
		if !strings.Contains(questHelp, "\n  "+verb+" ") {
			t.Errorf("quest enabled but registry verb `quest %s` is missing:\n%s", verb, questHelp)
		}
	}

	// `guild quest list` must still work end to end (quest enabled). The
	// container has no git, so pass the project explicitly with -p (the
	// same explicit-project path the MCP sessionStart uses).
	_ = c.guild(ctx, t, "quest", "list", "-p", "e2eproj") // exit 0 is the assertion; guild() fails t on non-zero.

	// ── daemon loop wiring: a disabled module contributes no Services ───
	// No core module ships a daemon loop yet, so the observable daemon
	// proof is that the daemon still runs healthily with lore disabled (in
	// daemon mode) and serves the surviving surface. The Service-registry
	// exclusion mechanism itself is unit-pinned
	// (TestCollectServices_ModuleServicesAppended + enabledModuleServices
	// walking module.Enabled). In daemon mode the session above already
	// routed through the daemon shim, so a working quest_post proves the
	// daemon serves correctly with lore off.
}

// toSet collapses a slice into a membership set.
func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}
