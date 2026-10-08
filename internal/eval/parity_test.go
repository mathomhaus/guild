package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// goldenRelPath is the committed fixture the parity harness locks against and
// the embedded goldenFixture mirrors.
const goldenRelPath = "testdata/golden/parity.json"

// TestParity_MatchesGolden is the determinism lock: the live ranking snapshot
// must equal the committed golden fixture exactly. On an intentional ranking
// change, regenerate with GUILD_EVAL_UPDATE=1 and review the diff.
//
// Regeneration path (mirrors the e2e GUILD_E2E_UPDATE / make e2e-update
// pattern): when GUILD_EVAL_UPDATE=1 is set, this test rewrites the fixture
// instead of asserting, so the same test both verifies and updates.
func TestParity_MatchesGolden(t *testing.T) {
	ctx := context.Background()
	got, err := ComputeFixture(ctx)
	if err != nil {
		t.Fatalf("ComputeFixture: %v", err)
	}

	if os.Getenv("GUILD_EVAL_UPDATE") == "1" {
		b, err := MarshalFixture(got)
		if err != nil {
			t.Fatalf("MarshalFixture: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(goldenRelPath), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(goldenRelPath, b, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("golden fixture updated: %s (%d bytes)", goldenRelPath, len(b))
		return
	}

	raw, err := os.ReadFile(goldenRelPath)
	if err != nil {
		t.Fatalf("read golden %s: %v\n(run `GUILD_EVAL_UPDATE=1 go test ./internal/eval/...` to generate it)", goldenRelPath, err)
	}
	var golden Fixture
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	if drift := CompareFixtures(golden, got); drift != "" {
		t.Fatalf("ranking drifted from golden %s: %s\n(if intentional, regenerate with GUILD_EVAL_UPDATE=1 and review the diff)",
			goldenRelPath, drift)
	}
}

// TestParity_EmbeddedMatchesFile guards that the binary-embedded golden and
// the on-disk golden are the same bytes, so the runtime parity check (which
// reads the embedded copy) and the test (which reads the file) can never
// silently disagree.
func TestParity_EmbeddedMatchesFile(t *testing.T) {
	raw, err := os.ReadFile(goldenRelPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(raw, goldenFixture) {
		t.Fatalf("embedded golden differs from %s; rebuild after regenerating the fixture", goldenRelPath)
	}
}

// TestParity_DetectsInjectedDrift proves the harness FAILS on drift rather
// than rubber-stamping: we mutate a copy of the golden (reorder two rows of a
// probe) and assert CompareFixtures reports a divergence. Without this, a
// harness that always returns "" would pass its own fixture and catch nothing.
func TestParity_DetectsInjectedDrift(t *testing.T) {
	ctx := context.Background()
	base, err := ComputeFixture(ctx)
	if err != nil {
		t.Fatalf("ComputeFixture: %v", err)
	}
	// Sanity: a snapshot must match itself.
	if drift := CompareFixtures(base, base); drift != "" {
		t.Fatalf("snapshot did not match itself: %s", drift)
	}

	// Inject drift: swap the slugs of the first two rows of the first probe
	// that has at least two rows. This models a ranking-order regression.
	mutated := deepCopyFixture(base)
	injected := false
	for i := range mutated.Probes {
		if len(mutated.Probes[i].Rows) >= 2 {
			mutated.Probes[i].Rows[0].Slug, mutated.Probes[i].Rows[1].Slug =
				mutated.Probes[i].Rows[1].Slug, mutated.Probes[i].Rows[0].Slug
			injected = true
			break
		}
	}
	if !injected {
		t.Fatal("no probe had >=2 rows to inject drift into; corpus too small")
	}
	if drift := CompareFixtures(base, mutated); drift == "" {
		t.Fatal("CompareFixtures returned no drift for a reordered fixture; the harness is inert")
	}

	// A changed score must also be caught, not just a reorder.
	scoreMut := deepCopyFixture(base)
	scoreMut.Probes[0].Rows[0].Score += 0.5
	if drift := CompareFixtures(base, scoreMut); drift == "" {
		t.Fatal("CompareFixtures returned no drift for a changed score; score drift is undetected")
	}

	// A dropped row must be caught.
	dropMut := deepCopyFixture(base)
	dropMut.Probes[0].Rows = dropMut.Probes[0].Rows[:len(dropMut.Probes[0].Rows)-1]
	if drift := CompareFixtures(base, dropMut); drift == "" {
		t.Fatal("CompareFixtures returned no drift for a dropped row; row-count drift is undetected")
	}
}

// deepCopyFixture clones a fixture so a mutation in one test does not bleed
// into the shared computed value.
func deepCopyFixture(fx Fixture) Fixture {
	out := Fixture{Version: fx.Version, Probes: make([]ProbeRanking, len(fx.Probes))}
	for i, p := range fx.Probes {
		rows := make([]RankedRow, len(p.Rows))
		copy(rows, p.Rows)
		out.Probes[i] = ProbeRanking{Probe: p.Probe, Query: p.Query, Rows: rows}
	}
	return out
}
