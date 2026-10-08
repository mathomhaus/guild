package compression

import (
	"strings"
	"testing"

	"github.com/mathomhaus/guild/internal/lore"
)

// makeDossierText builds a long, line-structured dossier blob the log
// strategy can meaningfully compact (must clear MinLinesForCCR=50 and the
// 0.5 ratio threshold).
func makeDossierText() string {
	var b strings.Builder
	b.WriteString("=== PROJECT DOSSIER: demo ===\n\n")
	b.WriteString("PRINCIPLES (follow these):\n")
	for i := 0; i < 90; i++ {
		b.WriteString("  • routine boilerplate principle line that repeats with minor variation number ")
		b.WriteByte(byte('0' + i%10))
		b.WriteByte('\n')
	}
	b.WriteString("ERROR a genuinely important decision the agent must not lose\n")
	return b.String()
}

func TestDossierDisabledIsNoOp(t *testing.T) {
	resetSettingsForTest()
	// Default settings: DossierCompact == false.
	out := &lore.DossierOutput{Project: "demo", Text: makeDossierText()}
	got, ok := compactDossier(out)
	if ok {
		t.Fatalf("compactDossier should decline when DossierCompact is off; got %q", got)
	}
}

func TestDossierHookIsTheRegisteredTransform(t *testing.T) {
	// The package init wires lore.DossierTransform to the compression hook.
	if lore.DossierTransform == nil {
		t.Fatal("compression init should set lore.DossierTransform")
	}
	resetSettingsForTest()
	// With the transform installed but DossierCompact off, the transform is
	// a strict no-op, so a dossier built through lore stays byte-identical.
	out := &lore.DossierOutput{Project: "demo", Text: makeDossierText()}
	text, ok := lore.DossierTransform(out)
	if ok {
		t.Fatalf("installed transform must be a no-op by default; got %q", text)
	}
}

func TestDossierEnabledCompactsAndRetrieves(t *testing.T) {
	resetSettingsForTest()
	enableDossierCompactForTest(t)

	original := makeDossierText()
	out := &lore.DossierOutput{Project: "demo", Text: original}
	compact, ok := compactDossier(out)
	if !ok {
		t.Fatal("compactDossier should compact when DossierCompact is on")
	}
	if len(compact) >= len(original) {
		t.Errorf("compact dossier should be smaller: %d >= %d", len(compact), len(original))
	}
	if !strings.Contains(compact, "compression_retrieve(hash=") {
		t.Error("compact dossier should advertise the retrieve affordance")
	}
	// The retrieve hint's hash must resolve to the exact original.
	hash := ExtractMarkerHash(compact)
	if hash == "" {
		t.Fatal("compact dossier carries no resolvable hash")
	}
	got, found := SharedStore().Get(hash)
	if !found || got != original {
		t.Fatalf("retrieve(hash) should reproduce the original dossier; found=%v", found)
	}
}

// enableDossierCompactForTest flips the package settings snapshot to enable
// the dossier compaction path, restoring it after the test.
func enableDossierCompactForTest(t *testing.T) {
	t.Helper()
	settingsMu.Lock()
	settings.DossierCompact = true
	settingsMu.Unlock()
	t.Cleanup(resetSettingsForTest)
}
