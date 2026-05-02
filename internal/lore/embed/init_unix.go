// Unix constructor seam for PrepareAndProbe. BGEEmbedder itself is unix-
// only (runtime_unix.go + bge_unix.go); this file wraps it so init.go
// (which is platform-agnostic) can call newBGEEmbedderFromExt without
// build-tagging the whole init flow.

//go:build unix

package embed

import "fmt"

func newBGEEmbedderFromExt(ext *ExtractResult) (Embedder, func(), error) {
	if ext == nil {
		return nil, func() {}, fmt.Errorf("embed: newBGEEmbedderFromExt: nil ExtractResult")
	}
	// NumThreads=1 during the probe run eliminates intra-op thread
	// scheduling as a source of float32 accumulation-order drift. The
	// reference fixture was captured single-threaded; mismatching that
	// reintroduces the cross-machine noise QUEST-215 was filed to kill.
	// Backfill and real inference do not share this handle; they go
	// through embed_wiring.go which constructs its own embedder with the
	// production thread count.
	emb, err := NewBGEEmbedder(RuntimeConfig{
		LibraryPath: ext.LibraryPath,
		ModelPath:   ext.ModelPath,
		VocabPath:   ext.VocabPath,
		NumThreads:  1,
	})
	if err != nil {
		return nil, func() {}, err
	}
	return emb, emb.Close, nil
}
