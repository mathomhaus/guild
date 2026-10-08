package e2e

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// concurrentTitles are the entries the two parallel sessions inscribe.
// Word sets are pairwise disjoint so each verification appraise has
// exactly one strictly-best match regardless of which retrieval arm
// (BM25 or embedding RRF) answers, keeping the golden deterministic.
var concurrentTitles = [2][5]string{
	{
		"amber falcon causeway ledger",
		"basalt otter signal tower",
		"crimson moth tidal archive",
		"dappled lynx orchard census",
		"ebony wren harbor manifest",
	},
	{
		"fennel ibis quarry beacon",
		"gilded newt summit registry",
		"hollow stag lantern depot",
		"ivory crane meadow almanac",
		"jasper vole canyon gazette",
	},
}

// TestE2EConcurrency opens two parallel MCP stdio sessions against the
// same container state and inscribes from both at once: the regression
// net for the lost-write class of concurrency bugs (two writers racing
// on the same lore index).
//
// Phase 1 (concurrent, asserted in code): sessions A and B each inscribe
// five entries simultaneously, with one mid-stream appraise per writer so
// reads land while the other session is splicing vectors into the shared
// index (read-while-splice coverage). Any error or lost write fails the
// test.
//
// Phase 2 (sequential, golden-recorded): a fresh session appraises every
// title and must get exactly one hit each. Interleaving-dependent values
// (entry id assignment) are scrubbed, so the transcript is deterministic
// even though the write order is not.
func TestE2EConcurrency(t *testing.T) {
	requireE2E(t)
	// Direct-mode only. This scenario's verification phase repairs the
	// detached vector writes the concurrent writers leave pending by
	// relying on the MCP server's once-per-process auto-backfill: a fresh
	// verification session, fresh process, fresh backfill gate, repairs
	// every pending row (see waitForFullEmbedCoverage). Under a shared
	// long-lived daemon there is no fresh process: the backfill gate
	// fires once at daemon startup against the empty corpus and is then
	// spent, so the repair path this scenario depends on does not apply.
	// Daemon-side backfill cadence is a separate concern (the daemon's
	// sleep cycle), not this lost-write regression net. Parity for the
	// full canonical loop is proven byte-identical by TestE2EBaseline,
	// which runs in both modes.
	if suite.mode == modeDaemon {
		t.Skip("concurrency lost-write net is direct-mode only; its per-process backfill repair has no daemon equivalent (baseline proves daemon parity)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	c := startContainer(ctx, t)
	c.initProject(ctx, t)

	// --- Phase 1: two sessions, parallel writers ----------------------
	sessions := [2]*mcpSession{
		c.openSession(ctx, t),
		c.openSession(ctx, t),
	}
	for i, s := range sessions {
		s.initialize()
		out := s.sessionStart("e2eproj")
		if !strings.Contains(out, "e2eproj") {
			t.Fatalf("session %d: session_start did not activate e2eproj:\n%s", i, out)
		}
	}

	var wg sync.WaitGroup
	// Capacity: one inscribe error per title plus one read-while-splice
	// appraise error per writer, so no goroutine ever blocks on send.
	errs := make(chan error, len(concurrentTitles[0])+len(concurrentTitles[1])+len(sessions))
	for i, s := range sessions {
		wg.Add(1)
		go func(i int, s *mcpSession) {
			defer wg.Done()
			for j, title := range concurrentTitles[i] {
				// callToolErr, not callTool: testing.T methods must not
				// be called from non-test goroutines.
				out, err := s.callToolErr("lore_inscribe", map[string]any{
					"title":   title,
					"kind":    "observation",
					"summary": fmt.Sprintf("Concurrency net entry %q from session %d.", title, i),
					"topic":   "e2e-concurrency",
				})
				if err != nil {
					errs <- fmt.Errorf("session %d: inscribe %q: %w", i, title, err)
					continue
				}
				if !strings.Contains(out, title) {
					errs <- fmt.Errorf("session %d: inscribe %q response missing title:\n%s", i, title, out)
				}
				// Mid-stream read: one appraise per writer, fired while the
				// other session is still inscribing (and splicing vectors
				// into the shared index). Asserted in code, never recorded:
				// ranking during partial embed coverage is nondeterministic,
				// but the just-inscribed title must be retrievable and the
				// call must not error against an in-flight splice.
				if j == 2 {
					probe := concurrentTitles[i][0]
					out, err := s.callToolErr("lore_appraise", map[string]any{
						"query": probe,
						"limit": 5,
					})
					if err != nil {
						errs <- fmt.Errorf("session %d: read-while-splice appraise %q: %w", i, probe, err)
					} else if !strings.Contains(out, probe) {
						errs <- fmt.Errorf("session %d: read-while-splice appraise %q missing the entry:\n%s", i, probe, out)
					}
				}
			}
		}(i, s)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.Fatal("concurrent inscribe phase failed")
	}
	sessions[0].close()
	sessions[1].close()

	// --- Phase 2: verification session, deterministic transcript ------
	v := c.openSession(ctx, t)
	v.initialize()
	v.sessionStart("e2eproj")

	// Embedding writes are flushed asynchronously relative to the
	// inscribe responses; until every entry has its vector, appraise
	// ranking mixes a complete BM25 arm with a partial embedding arm and
	// the top hit can flip run to run. Wait for full coverage so the
	// recorded ranking is the deterministic steady state.
	total := len(concurrentTitles[0]) + len(concurrentTitles[1])
	waitForFullEmbedCoverage(t, v, total)

	tr := &transcript{scrubIDs: true}
	found := 0
	for _, group := range concurrentTitles {
		for _, title := range group {
			out := v.callTool("lore_appraise", map[string]any{
				"query": title,
				"limit": 1,
			})
			if strings.Contains(out, title) && strings.Contains(out, "1 result(s)") {
				found++
			} else {
				t.Errorf("lost write: appraise %q did not return the entry:\n%s", title, out)
			}
			tr.step("tools/call lore_appraise "+fmt.Sprintf("%q", title), out)
		}
	}
	if found != total {
		t.Errorf("found %d of %d concurrently inscribed entries", found, total)
	}
	tr.step("summary", fmt.Sprintf("entries verified: %d of %d", found, total))

	v.close()
	compareGolden(t, "concurrency", tr)
}

// coverageRe extracts "coverage: num/den" from lore_health output.
var coverageRe = regexp.MustCompile(`coverage:\s+(\d+)/(\d+)`)

// waitForFullEmbedCoverage polls lore_health until every entry has a
// vector (num == den, den >= want). A write lost by the concurrent
// phase would leave den short of want and fail here with the final
// health report, which is exactly the regression this scenario nets.
//
// The throwaway appraise first is load-bearing: vector writes on the
// MCP surface are detached goroutines, so writer servers that exit
// right after inscribing can leave rows pending. The repair path is
// the server's once-per-process auto-backfill, and that only fires
// when a lore tool resolves the embed provider; lore_health reads
// state without resolving, so polling it alone would wait forever.
func waitForFullEmbedCoverage(t *testing.T, s *mcpSession, want int) {
	t.Helper()
	_ = s.callTool("lore_appraise", map[string]any{
		"query": "coverage warmup probe",
		"limit": 1,
	})
	deadline := time.Now().Add(60 * time.Second)
	for {
		out := s.callTool("lore_health", map[string]any{})
		m := coverageRe.FindStringSubmatch(out)
		if len(m) == 3 && m[1] == m[2] {
			if n, err := strconv.Atoi(m[2]); err == nil && n >= want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("embed coverage did not reach %d/%d entries:\n%s", want, want, out)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
