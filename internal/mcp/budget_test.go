package mcp

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Token budget constants. The 4-chars-per-token heuristic is the
// accepted approximation for English prose — good enough for a
// regression gate, not accurate enough to replace a tokenizer.
const (
	// charsPerToken is the English-prose estimator. A real tokenizer
	// would be tighter; we bias pessimistic (~3.5 chars/token) to keep
	// ourselves honest.
	charsPerToken = 4

	// perToolMaxTokens caps each tool's description token count.
	perToolMaxTokens = 100

	// totalMaxTokens is the overall static-cost ceiling across all
	// registered tools (descriptions + input schemas). Raised to 4500
	// in QUEST-101 to accommodate the 5 previously-deferred tools
	// (lore_seal, lore_catalog, quest_epic, quest_active, quest_forfeit)
	// now in the always-on tier. Raised to 4700 in QUEST-106 when
	// quest_fulfill was added alongside quest_clear as a backward-compat
	// alias — net +~100 tokens for the duplicate tool. Expected actual:
	// ~4400-4600 tokens.
	totalMaxTokens = 4700
)

// TestDescriptionBudget enforces the token budgets against the full
// registered tool surface. For every tool, asserts:
//
//  1. description ≤ perToolMaxTokens
//  2. total static cost (descriptions + serialized input schemas) ≤ totalMaxTokens
//
// The test also logs per-tool numbers + total so budget drift is easy to spot.
func TestDescriptionBudget(t *testing.T) {
	isolateHome(t)

	s, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, clientSession, cleanup := connectInMemory(t, s)
	defer cleanup()

	ctx := context.Background()
	res, err := clientSession.ListTools(ctx, &sdkmcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) == 0 {
		t.Fatal("no tools registered")
	}

	type row struct {
		name        string
		descTokens  int
		schemTokens int
		totalTokens int
	}
	rows := make([]row, 0, len(res.Tools))
	var total int
	var overPerTool []string

	for _, tool := range res.Tools {
		descChars := len(tool.Description)
		descTokens := estimateTokens(descChars)

		schemaJSON, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal schema for %s: %v", tool.Name, err)
		}
		schemaTokens := estimateTokens(len(schemaJSON))

		toolTotal := descTokens + schemaTokens
		total += toolTotal

		rows = append(rows, row{
			name:        tool.Name,
			descTokens:  descTokens,
			schemTokens: schemaTokens,
			totalTokens: toolTotal,
		})

		if descTokens > perToolMaxTokens {
			overPerTool = append(overPerTool,
				toolBudgetLine(tool.Name, descTokens, len(tool.Description)))
		}
	}

	// Stable alphabetical reporting — easier to read diffs.
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	t.Logf("=== token budget (perTool≤%d, total≤%d) ===",
		perToolMaxTokens, totalMaxTokens)
	for _, r := range rows {
		t.Logf("  %-22s  desc=%3d  schema=%3d  total=%3d",
			r.name, r.descTokens, r.schemTokens, r.totalTokens)
	}
	t.Logf("=== total=%d tokens across %d tools ===", total, len(rows))

	if len(overPerTool) > 0 {
		t.Errorf("per-tool budget breach (≤%d tokens):\n  %s",
			perToolMaxTokens, strings.Join(overPerTool, "\n  "))
	}
	if total > totalMaxTokens {
		t.Errorf("total static cost %d > ceiling %d",
			total, totalMaxTokens)
	}
}

// estimateTokens applies the 4-chars-per-token heuristic, rounding up
// so a 1-char description still reads as 1 token (not 0).
func estimateTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return (chars + charsPerToken - 1) / charsPerToken
}

// toolBudgetLine formats a single breach line for the test-fail message.
func toolBudgetLine(name string, tokens, chars int) string {
	return name + ": " + itoa(tokens) + " tokens (" + itoa(chars) + " chars)"
}

// itoa wraps strconv without the extra import so this file stays self
// contained and doesn't pull a heavyweight dep for a one-line debug.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	neg := n < 0
	if neg {
		n = -n
	}
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}
