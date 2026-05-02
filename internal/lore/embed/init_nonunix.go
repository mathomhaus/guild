// Non-unix fallback for newBGEEmbedderFromExt. Windows cannot load the
// ORT dylib via purego (spike friction F11), so this file compiles in
// place of init_unix.go and returns ErrEmbedderDisabled so the init
// flow writes meta.embedder_state='disabled' with reason
// 'unsupported_platform' without ever trying the ORT path.

//go:build !unix

package embed

func newBGEEmbedderFromExt(_ *ExtractResult) (Embedder, func(), error) {
	return nil, func() {}, ErrEmbedderDisabled
}
