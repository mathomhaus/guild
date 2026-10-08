package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestE2EBaseline drives one full guild loop inside a fresh container:
//
//	guild init → MCP handshake → tools/list → guild_session_start →
//	lore_inscribe → lore_appraise → quest_post → quest_accept →
//	quest_fulfill
//
// Every tool response's actual text is asserted inline AND recorded into
// the golden transcript, so both "the loop works" and "the loop's output
// bytes are stable" are pinned.
func TestE2EBaseline(t *testing.T) {
	requireE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	c := startContainer(ctx, t)
	tr := &transcript{}

	// --- guild init -------------------------------------------------
	initOut := c.initProject(ctx, t)
	if !strings.Contains(initOut, "project: e2eproj") {
		t.Fatalf("guild init did not register project e2eproj:\n%s", initOut)
	}
	tr.step("guild init --yes", initOut)

	// --- MCP handshake ----------------------------------------------
	s := c.openSession(ctx, t)
	ir := s.initialize()
	if ir.ServerInfo.Name != "guild" {
		t.Errorf("serverInfo.name = %q, want %q", ir.ServerInfo.Name, "guild")
	}
	if ir.ProtocolVersion != "2025-06-18" {
		t.Errorf("protocolVersion = %q, want echo of %q", ir.ProtocolVersion, "2025-06-18")
	}
	if ir.Instructions == "" {
		t.Error("initialize returned empty instructions")
	}
	tr.step("initialize", fmt.Sprintf(
		"serverInfo: %s %s\nprotocolVersion: %s\ninstructions: %s",
		ir.ServerInfo.Name, ir.ServerInfo.Version, ir.ProtocolVersion,
		fingerprint(ir.Instructions)))

	// --- tools/list --------------------------------------------------
	tools := s.listToolNames()
	if len(tools) == 0 {
		t.Fatal("tools/list returned no tools")
	}
	for _, required := range []string{
		"guild_session_start", "lore_inscribe", "lore_appraise",
		"quest_post", "quest_accept", "quest_fulfill",
	} {
		found := false
		for _, name := range tools {
			if name == required {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("tools/list missing %q", required)
		}
	}
	tr.step(fmt.Sprintf("tools/list (%d tools)", len(tools)), strings.Join(tools, "\n"))

	// --- guild_session_start -----------------------------------------
	out := s.sessionStart("e2eproj")
	if !strings.Contains(out, "e2eproj") {
		t.Errorf("session_start output does not name the project:\n%s", out)
	}
	tr.step(`tools/call guild_session_start {"project":"e2eproj"}`, out)

	// --- lore inscribe / appraise round-trip --------------------------
	const title = "cobalt heron drydock survey"
	out = s.callTool("lore_inscribe", map[string]any{
		"title":   title,
		"kind":    "research",
		"summary": "Baseline e2e entry proving the MCP write path end to end.",
		"topic":   "e2e-baseline",
	})
	if !strings.Contains(out, "LORE-1") || !strings.Contains(out, title) {
		t.Errorf("inscribe output missing LORE-1 / title:\n%s", out)
	}
	tr.step("tools/call lore_inscribe", out)

	out = s.callTool("lore_appraise", map[string]any{
		"query": "cobalt heron drydock",
		"limit": 1,
	})
	if !strings.Contains(out, "LORE-1") || !strings.Contains(out, title) {
		t.Errorf("appraise did not surface the inscribed entry:\n%s", out)
	}
	tr.step("tools/call lore_appraise", out)

	// --- quest post / accept / fulfill round-trip ---------------------
	const subject = "wire the drydock beacon relay"
	out = s.callTool("quest_post", map[string]any{
		"subject":    subject,
		"priority":   "P1",
		"acceptance": []string{"beacon relay answers a ping from the harness"},
	})
	if !strings.Contains(out, "QUEST-1") || !strings.Contains(out, subject) {
		t.Errorf("quest_post output missing QUEST-1 / subject:\n%s", out)
	}
	tr.step("tools/call quest_post", out)

	out = s.callTool("quest_accept", map[string]any{
		"quest_id": "QUEST-1",
	})
	if !strings.Contains(out, "QUEST-1") || !strings.Contains(out, "in_progress") {
		t.Errorf("quest_accept output missing QUEST-1 / in_progress:\n%s", out)
	}
	tr.step("tools/call quest_accept", out)

	out = s.callTool("quest_fulfill", map[string]any{
		"quest_id": "QUEST-1",
		"report":   "beacon relay wired and verified by the e2e harness",
	})
	if !strings.Contains(out, "QUEST-1") {
		t.Errorf("quest_fulfill output missing QUEST-1:\n%s", out)
	}
	tr.step("tools/call quest_fulfill", out)

	s.close()
	compareGolden(t, "baseline", tr)
}
