// Package mcp implements the guild MCP stdio server — the agent-facing
// contract. Tool handlers in this package are registered on the
// [modelcontextprotocol/go-sdk] Server and exchange JSON-RPC over stdin/
// stdout; everything else in the binary (lore, quest, storage, session)
// stays transport-agnostic.
//
// The INSTRUCTIONS string delivered to the host at initialize is built
// dynamically at connect time: static contract (embedded instructions.md)
// concatenated with the active project's current principles so agents
// receive the oath wall without an explicit lore_oath call.
package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "embed"

	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/module"
)

//go:embed instructions.md
var staticInstructions string

// buildInstructions builds the full INSTRUCTIONS string for one MCP
// connect. The shape is always:
//
//	<static contract>
//
//	## Active Principles (oath wall)
//	- <title> — <summary>
//	…
//
// The static contract is the content of instructions.md, embedded at
// build time (unchanged source-of-truth for the Anthropic prompt-cache
// prefix — it never changes within a session, keeping the cache hit rate
// high). The principles section is appended last so a change between
// sessions only invalidates the tail of the cached string.
//
// When project is empty, or loreDB is nil, or no current principles
// exist for the project, INSTRUCTIONS = static contract only (no
// principles section). This covers:
//   - fresh MCP server starts (no active project yet at initialize time)
//   - multi-project host environments where the active project isn't
//     known until guild_session_start is called
//
// Kind filter: only kind='principle' AND status='current' entries
// are included. Sorted by created_at ASC (oldest first) to maintain a
// stable rendering order across sessions — same order as lore_oath
// reversed (lore.Oath returns DESC; we reverse here for ASC).
//
// Called from buildWithContext in server.go at each MCP server start.
func buildInstructions(ctx context.Context, loreDB *sql.DB, project string) string {
	base := contractBody()
	if strings.TrimSpace(project) == "" || loreDB == nil {
		return base
	}

	// lore.Oath returns kind=principle AND status=current, sorted DESC
	// (newest first). The spec for QUEST-57 requires ASC (oldest first)
	// for stable ordering. We reverse the slice in-place.
	entries, err := lore.Oath(ctx, loreDB, project)
	if err != nil || len(entries) == 0 {
		return base
	}

	// Reverse to get ASC order (lore.Oath returns DESC).
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}

	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n\n## Active Principles (oath wall)\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "- %s — %s\n", e.Title, e.Summary)
	}
	return b.String()
}

// contractBody returns the static INSTRUCTIONS contract with disabled
// modules' fragments excluded (ADR-006 Phase 3). The contract is one
// monolithic, prompt-cache-prefix string embedded from instructions.md;
// it has no per-module section markers, so a module's "fragment" is
// whatever non-empty string its Instructions() returns. The exclusion
// mechanism is purely subtractive:
//
//   - For every ENABLED module, its Instructions() fragment is part of
//     the contract and left untouched.
//   - For every DISABLED module, if its Instructions() returns a
//     non-empty fragment that appears verbatim in the embedded contract,
//     that fragment is removed so a disabled capability never reaches the
//     agent's contract.
//
// Today every core module returns "" (the full contract lives in
// instructions.md as a single authored block, no module owns a fragment),
// so this function returns staticInstructions UNCHANGED on the all-on
// path: the emitted bytes are byte-identical to before, preserving the
// golden-pinned 17107-byte / sha256:8248295e... contract. The seam exists
// so that the moment a capability module DOES author an Instructions()
// fragment, disabling that module drops its contribution from the
// contract automatically — no edit to instructions.go required.
//
// Decomposing the monolith into per-module sections now would change the
// all-on emitted bytes (whitespace, ordering, the cache prefix), which the
// parity bar forbids; so the contract stays monolithic and exclusion is
// expressed as fragment removal, documented here.
func contractBody() string {
	body := staticInstructions
	for _, m := range disabledModulesWithFragments() {
		frag := strings.TrimSpace(m.Instructions())
		if frag == "" {
			continue
		}
		// Remove the fragment wherever it appears verbatim. A leading or
		// trailing blank line bordering the fragment is collapsed so the
		// surrounding contract reads cleanly after removal.
		body = removeFragment(body, frag)
	}
	return body
}

// disabledModulesWithFragments returns the registered modules the operator
// has DISABLED via config. It is the complement of module.Enabled under
// the same config-backed predicate the tool/verb surfaces use, so the
// instruction contract, the MCP tools, and the CLI verbs all agree on
// which modules are off. A config-load failure degrades to "nothing
// disabled" (every module enabled), matching the swallow-and-degrade
// posture of moduleEnabledPredicate.
func disabledModulesWithFragments() []module.Module {
	pred := moduleEnabledPredicate()
	enabled := map[string]bool{}
	for _, m := range module.Enabled(pred) {
		enabled[m.Name()] = true
	}
	var off []module.Module
	for _, m := range module.All() {
		if !enabled[m.Name()] {
			off = append(off, m)
		}
	}
	return off
}

// removeFragment deletes the first verbatim occurrence of frag from body,
// trimming a single bordering blank line so the contract does not leave a
// double gap where a section used to be. Returns body unchanged when frag
// is absent.
func removeFragment(body, frag string) string {
	idx := strings.Index(body, frag)
	if idx < 0 {
		return body
	}
	before, after := body[:idx], body[idx+len(frag):]
	// Collapse a blank-line seam: if the removed fragment was bracketed by
	// "\n\n" on both sides, leave a single "\n\n", not "\n\n\n\n".
	if strings.HasSuffix(before, "\n\n") && strings.HasPrefix(after, "\n\n") {
		after = strings.TrimPrefix(after, "\n\n")
	}
	return before + after
}
