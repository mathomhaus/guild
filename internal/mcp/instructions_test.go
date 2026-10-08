package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"

	"github.com/mathomhaus/guild/internal/storage"
)

// wantStaticSHA is the SHA-256 of the embedded instructions.md (the
// static portion of INSTRUCTIONS). Any edit to instructions.md changes
// this hash — edits are changes to the agent-visible contract, so the
// reviewer must consciously update this constant. Treat a failing diff
// here as intentional: update the hash in the same commit that edits
// instructions.md.
//
// Scope: tripwire hashes staticInstructions only (NOT the dynamic
// principles suffix appended by buildInstructions). Adding or removing
// principles via lore_inscribe does NOT cause this test to fail — only
// direct edits to instructions.md do. See QUEST-57 for the dynamic
// build path and its separate tests.
//
// Last updated when instructions.md was reordered so its first 2,048
// bytes carry the load-bearing contract (session_start mandate, core
// loop, worked demonstrations). Hosts that truncate
// initialize.instructions deliver only that prefix;
// TestInstructionsFirst2KB gates its composition.
const wantStaticSHA = "8248295e193d5defa541e5c91e029bd54e2eccc93e04077981cb43d88f08a543"

func TestStaticInstructions_Embedded(t *testing.T) {
	if staticInstructions == "" {
		t.Fatal("staticInstructions is empty — embed failed")
	}
	if !strings.Contains(staticInstructions, "guild_session_start") {
		t.Fatal("staticInstructions missing expected onboarding anchor (guild_session_start)")
	}
}

func TestStaticInstructions_ContractHash(t *testing.T) {
	sum := sha256.Sum256([]byte(staticInstructions))
	got := hex.EncodeToString(sum[:])
	if got != wantStaticSHA {
		t.Fatalf("staticInstructions contract hash drift:\n  want %s\n  got  %s\n\nIf you intentionally edited instructions.md, update wantStaticSHA in the same commit.",
			wantStaticSHA, got)
	}
}

// instructionsTruncationEnvelope is the byte budget some MCP hosts
// deliver from initialize.instructions before truncating (Claude Code
// measured cutting at 2,048 characters; other hosts drop the field
// entirely). Whatever lands past this prefix is invisible to those
// hosts, so the load-bearing contract must fit inside it.
const instructionsTruncationEnvelope = 2048

// TestInstructionsFirst2KB gates the truncated-prefix contract, in the
// spirit of TestDescriptionBudget (budget_test.go): the first 2,048
// BYTES of staticInstructions must carry, on their own:
//
//  1. the guild_session_start mandate
//  2. the core loop (quest_bounties -> quest_accept -> work -> quest_fulfill)
//  3. at least one complete worked tool invocation with arguments
//
// Bytes, not runes: the file contains multibyte typography, and
// byte-gating is the stricter, host-agnostic bound whether a host
// counts characters or bytes. The test logs the measured prefix
// composition so drift is visible in test output before it breaches.
func TestInstructionsFirst2KB(t *testing.T) {
	prefix := staticInstructions
	if len(prefix) > instructionsTruncationEnvelope {
		prefix = prefix[:instructionsTruncationEnvelope]
	}

	t.Logf("=== 2KB envelope: file=%d bytes, prefix=%d bytes (%.0f%% delivered) ===",
		len(staticInstructions), len(prefix),
		float64(len(prefix))/float64(len(staticInstructions))*100)

	checks := []struct {
		label string
		want  string
	}{
		{"session_start mandate", "Before any other guild tool will work, call:"},
		{"session_start invocation", "guild_session_start()"},
		{"core loop", "quest_bounties → quest_accept → work → quest_fulfill(report=...)"},
	}
	for _, c := range checks {
		pos := strings.Index(prefix, c.want)
		if pos < 0 {
			t.Errorf("first %d bytes of instructions.md missing %s %q; reorder so it lands before the cut",
				instructionsTruncationEnvelope, c.label, c.want)
			continue
		}
		t.Logf("  %-25s at byte %d", c.label, pos)
	}

	// A complete worked demonstration: a word-boundary tool invocation
	// (mirroring doc_coverage_test.go's invocation pattern) tightened to
	// require a named argument bound to a quoted string, so the bare
	// core-loop line quest_fulfill(report=...) cannot satisfy it alone.
	demoRe := regexp.MustCompile(`(?s)\b(?:guild|lore|quest)_[a-z_]+\([^()]*[a-z_]+="[^"]*"`)
	loc := demoRe.FindStringIndex(prefix)
	if loc == nil {
		t.Errorf("first %d bytes of instructions.md contain no worked tool invocation with arguments (want match for %s); move a demonstration above the cut",
			instructionsTruncationEnvelope, demoRe)
	} else {
		snip := strings.Join(strings.Fields(prefix[loc[0]:loc[1]]), " ")
		if len(snip) > 80 {
			snip = snip[:80]
		}
		t.Logf("  %-25s at byte %d: %q", "worked demonstration", loc[0], snip)
	}
}

// TestBuildInstructions_StaticOnly asserts that buildInstructions returns
// the static contract when called with an empty project or nil DB.
func TestBuildInstructions_StaticOnly(t *testing.T) {
	ctx := context.Background()

	got := buildInstructions(ctx, nil, "")
	if got != staticInstructions {
		t.Errorf("expected static-only INSTRUCTIONS when project is empty; got different result")
	}

	got = buildInstructions(ctx, nil, "someproject")
	if got != staticInstructions {
		t.Errorf("expected static-only INSTRUCTIONS when loreDB is nil; got different result")
	}
}

// TestBuildInstructions_WithPrinciples asserts that when a project has
// current principles, buildInstructions appends "## Active Principles
// (oath wall)" with the principle entries to the static contract.
func TestBuildInstructions_WithPrinciples(t *testing.T) {
	ctx := context.Background()

	// Spin up an in-memory lore DB and insert a test principle.
	loreDB, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open in-memory lore DB: %v", err)
	}
	defer func() { _ = loreDB.Close() }()
	if err := storage.Migrate(ctx, loreDB, "lore"); err != nil {
		t.Fatalf("migrate lore DB: %v", err)
	}

	// Register a project and insert a principle entry.
	const proj = "testproject"
	if _, err := loreDB.ExecContext(ctx,
		`INSERT OR IGNORE INTO projects (id, path) VALUES (?, ?)`, proj, "/tmp/"+proj,
	); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	title := "Always appraise before researching"
	summary := "Call lore_appraise before external search to avoid re-deriving cached facts."
	if _, err := loreDB.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status)
		 VALUES (?, 'lore', 'principle', ?, ?, 'current')`,
		proj, title, summary,
	); err != nil {
		t.Fatalf("insert principle: %v", err)
	}

	got := buildInstructions(ctx, loreDB, proj)

	// Must start with the static contract (cache prefix stability).
	if !strings.HasPrefix(got, staticInstructions) {
		t.Errorf("buildInstructions result does not start with staticInstructions")
	}

	// Must contain the principles section header.
	if !strings.Contains(got, "## Active Principles (oath wall)") {
		t.Errorf("buildInstructions missing '## Active Principles (oath wall)' section")
	}

	// Must contain the principle entry in "- <title> — <summary>" format.
	wantLine := "- " + title + " — " + summary
	if !strings.Contains(got, wantLine) {
		t.Errorf("buildInstructions missing principle line %q; got:\n%s", wantLine, got[len(staticInstructions):])
	}

	// Must NOT be equal to staticInstructions (principles were appended).
	if got == staticInstructions {
		t.Errorf("expected dynamic INSTRUCTIONS with principles; got static-only")
	}
}

// TestBuildInstructions_NoPrinciplesForProject asserts that when a project
// exists but has no current principles, buildInstructions returns
// static-only INSTRUCTIONS (no empty principles section appended).
func TestBuildInstructions_NoPrinciplesForProject(t *testing.T) {
	ctx := context.Background()

	loreDB, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open in-memory lore DB: %v", err)
	}
	defer func() { _ = loreDB.Close() }()
	if err := storage.Migrate(ctx, loreDB, "lore"); err != nil {
		t.Fatalf("migrate lore DB: %v", err)
	}

	const proj = "emptyproject"
	if _, err := loreDB.ExecContext(ctx,
		`INSERT OR IGNORE INTO projects (id, path) VALUES (?, ?)`, proj, "/tmp/"+proj,
	); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	// Insert a non-principle entry (kind=decision, not kind=principle).
	if _, err := loreDB.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status)
		 VALUES (?, 'arch', 'decision', 'Some decision', 'body', 'current')`,
		proj,
	); err != nil {
		t.Fatalf("insert decision: %v", err)
	}

	got := buildInstructions(ctx, loreDB, proj)

	if got != staticInstructions {
		t.Errorf("expected static-only INSTRUCTIONS when no principles exist; got:\n%s",
			got[len(staticInstructions):])
	}
}

// TestBuildInstructions_PrinciplesOrderASC asserts that multiple principles
// are appended in created_at ASC order (oldest first).
func TestBuildInstructions_PrinciplesOrderASC(t *testing.T) {
	ctx := context.Background()

	loreDB, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open in-memory lore DB: %v", err)
	}
	defer func() { _ = loreDB.Close() }()
	if err := storage.Migrate(ctx, loreDB, "lore"); err != nil {
		t.Fatalf("migrate lore DB: %v", err)
	}

	const proj = "orderproject"
	if _, err := loreDB.ExecContext(ctx,
		`INSERT OR IGNORE INTO projects (id, path) VALUES (?, ?)`, proj, "/tmp/"+proj,
	); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	// Insert two principles with explicit created_at times; "older" goes first.
	if _, err := loreDB.ExecContext(ctx,
		`INSERT INTO entries (project_id, topic, kind, title, summary, status, created_at)
		 VALUES
		   (?, 'core', 'principle', 'Older principle', 'oldest', 'current', '2024-01-01T00:00:00Z'),
		   (?, 'core', 'principle', 'Newer principle', 'newest', 'current', '2024-06-01T00:00:00Z')`,
		proj, proj,
	); err != nil {
		t.Fatalf("insert principles: %v", err)
	}

	got := buildInstructions(ctx, loreDB, proj)

	olderPos := strings.Index(got, "Older principle")
	newerPos := strings.Index(got, "Newer principle")

	if olderPos < 0 || newerPos < 0 {
		t.Fatalf("missing principles in output; got:\n%s", got[len(staticInstructions):])
	}
	if olderPos > newerPos {
		t.Errorf("principles not in ASC order: 'Older principle' at %d, 'Newer principle' at %d",
			olderPos, newerPos)
	}
}

// TestContractBody_AllOnByteIdentical pins the ADR-006 Phase 3 parity
// bar: with every module enabled (the default set, all returning
// Instructions()==""), contractBody returns the embedded contract
// byte-for-byte. The exclusion seam must be a no-op on the all-on path so
// the golden-pinned 17107-byte contract never moves.
func TestContractBody_AllOnByteIdentical(t *testing.T) {
	got := contractBody()
	if got != staticInstructions {
		t.Fatalf("contractBody changed the all-on contract:\n  len(static)=%d len(got)=%d\nThe exclusion seam must not alter bytes when every module is enabled.",
			len(staticInstructions), len(got))
	}
}

// TestRemoveFragment exercises the subtractive exclusion mechanism: a
// disabled module's verbatim fragment is removed and the surrounding
// contract is left clean, while an absent fragment leaves the body
// untouched.
func TestRemoveFragment(t *testing.T) {
	const body = "## Core\nalways here\n\n## Compression\nheavy capability text\n\n## Tail\nstill here\n"
	const frag = "## Compression\nheavy capability text"

	got := removeFragment(body, frag)
	if strings.Contains(got, "heavy capability text") {
		t.Errorf("fragment not removed:\n%s", got)
	}
	if !strings.Contains(got, "## Core") || !strings.Contains(got, "## Tail") {
		t.Errorf("removeFragment dropped surrounding contract:\n%s", got)
	}
	// The blank-line seam between Compression and Tail must collapse to a
	// single "\n\n", not leave a double gap.
	if strings.Contains(got, "\n\n\n\n") {
		t.Errorf("removeFragment left a double blank-line seam:\n%q", got)
	}

	// Absent fragment: body unchanged.
	if out := removeFragment(body, "## Nope\nnot present"); out != body {
		t.Errorf("removeFragment with absent fragment changed the body")
	}
}
