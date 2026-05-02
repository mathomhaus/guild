package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// Last updated for QUEST-243: added lore_unlink(from_id=..., to_id=...) to the
// canonical invocation examples section so doc-coverage passes for the
// new provenance edge removal tool (LORE-401).
const wantStaticSHA = "3612ee1770080d0382887a60530cc138f90d26d2974ed434feeeb6f3c419d7e4"

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
