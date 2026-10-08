package compression

import (
	"fmt"

	"github.com/mathomhaus/guild/internal/lore"
)

// Optional lore_dossier compaction path (ADR-006 Phase 7 deliverable 4).
//
// When the compression module is enabled AND [compression].dossier_compact is
// set, the dossier text is run through the log strategy (the dossier is
// line-structured prose, so the log compressor's line-selection + CCR pairing
// is the right fit), the full original is stashed in the CCR store, and a
// retrieve affordance is appended so an agent can expand it back with
// compression_retrieve(hash=...).
//
// The whole path is gated: DossierCompact is false by default, so even with
// the module imported (it always is, via the aggregator) the hook returns
// ok=false and the dossier text is byte-identical to today. Only an explicit
// opt-in engages the compaction.

func init() {
	// Install the seam unconditionally; the closure self-gates on config so
	// installing it is a no-op until the operator opts in. This keeps the
	// default lore_dossier output byte-identical (DossierTransform returns
	// ok=false, leaving out.Text exactly as renderDossier produced it).
	lore.DossierTransform = compactDossier
}

// compactDossier is the DossierTransform implementation. It returns ok=false
// (no change) unless [compression].dossier_compact is on, in which case it
// returns a compact form plus a retrieve hint and stashes the original.
func compactDossier(out *lore.DossierOutput) (string, bool) {
	if out == nil {
		return "", false
	}
	if !CurrentSettings().DossierCompact {
		return "", false // default path: byte-identical, no compaction
	}

	original := out.Text
	strat, err := BuildStrategy("log")
	if err != nil {
		return "", false
	}
	res, err := strat.Compress(original, "", SharedStore())
	if err != nil || res.Compressed == "" {
		return "", false
	}

	// The log strategy passes short input through unchanged and only mints a
	// CacheKey when it actually compressed. If nothing was compacted, leave
	// the dossier as-is rather than emit a marker that points nowhere.
	if res.CacheKey == "" {
		return "", false
	}

	compact := res.Compressed + fmt.Sprintf(
		"\n[dossier compacted; retrieve the full text with compression_retrieve(hash=%s)]",
		res.CacheKey,
	)
	return compact, true
}
