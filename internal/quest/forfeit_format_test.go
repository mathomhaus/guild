package quest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mathomhaus/guild/internal/command"
)

// TestForfeit_Format_InProgress checks the happy-path render: ↩️
// success line with the "back to 'next'" phrasing.
func TestForfeit_Format_InProgress(t *testing.T) {
	out := ForfeitOutput{
		Quest:   &Quest{ID: "QUEST-1", Status: StatusNext},
		HasNote: true,
	}
	got := formatForfeited(command.MCPSink{}, out)
	if !strings.Contains(got, "↩️") {
		t.Errorf("in_progress render missing ↩️ glyph:\n%s", got)
	}
	if !strings.Contains(got, "forfeited QUEST-1") {
		t.Errorf("missing forfeited phrase:\n%s", got)
	}
	if !strings.Contains(got, "(note saved)") {
		t.Errorf("note-saved tail missing:\n%s", got)
	}
}

// TestForfeit_Format_AlreadyNext is the QUEST-135 regression:
// AlreadyNext=true must render a neutral ✅/[ok] line, NOT the
// misleading ↩️ success glyph.
func TestForfeit_Format_AlreadyNext(t *testing.T) {
	out := ForfeitOutput{
		Quest:       &Quest{ID: "QUEST-7", Status: StatusNext},
		AlreadyNext: true,
	}
	got := formatForfeited(command.MCPSink{}, out)

	if strings.Contains(got, "↩️") {
		t.Errorf("already-next render must NOT use ↩️ (that implies a state change):\n%s", got)
	}
	if !strings.Contains(got, "already unclaimed") {
		t.Errorf("expected 'already unclaimed' phrasing:\n%s", got)
	}
	if !strings.Contains(got, "QUEST-7") {
		t.Errorf("quest id missing from render:\n%s", got)
	}

	// CLI rendering (no-emoji variant) must also avoid the [forfeited]
	// success tag — it lies about the state transition.
	gotCLI := formatForfeited(command.CLISink{NoEmoji: true}, out)
	if strings.Contains(gotCLI, "[forfeited]") {
		t.Errorf("no-emoji render leaked [forfeited] tag:\n%s", gotCLI)
	}
	if !strings.Contains(gotCLI, "[ok]") {
		t.Errorf("no-emoji render missing [ok] tag:\n%s", gotCLI)
	}
}

// TestForfeit_Format_AlreadyDoneError is the QUEST-135 regression on
// the error path: ErrAlreadyDone must render as a clear ❌ line that
// tells the agent to use quest post to rework.
func TestForfeit_Format_AlreadyDoneError(t *testing.T) {
	err := fmt.Errorf("%w: QUEST-9 — use quest post to rework", ErrAlreadyDone)
	got, ok := formatForfeitError(command.MCPSink{}, err)
	if !ok {
		t.Fatalf("formatForfeitError did not handle ErrAlreadyDone")
	}
	for _, want := range []string{"❌", "quest_forfeit", "QUEST-9", "rework"} {
		if !strings.Contains(got, want) {
			t.Errorf("error render missing %q:\n%s", want, got)
		}
	}
}
