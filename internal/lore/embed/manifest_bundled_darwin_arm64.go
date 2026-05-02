// Bundled manifest for darwin/arm64. Active only under `-tags=withembed`
// AND when GOOS/GOARCH match. Release CI stages the three asset files
// under assets/darwin_arm64/ via `make assets` before building.

//go:build withembed && darwin && arm64

package embed

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
)

//go:embed assets/darwin_arm64/libonnxruntime.dylib
var bundledLibraryBytes []byte

//go:embed assets/darwin_arm64/model.onnx
var bundledModelBytes []byte

//go:embed assets/darwin_arm64/vocab.txt
var bundledVocabBytes []byte

const (
	bundledPlatformTag = "darwin-arm64"
	bundledLibraryName = "libonnxruntime.dylib"
)

func manifestHasAssets() bool { return true }

func currentManifest() Manifest {
	identity := DefaultIdentity()
	identity.PlatformTag = bundledPlatformTag
	identity.TokenizerHash = hexSHA256(bundledVocabBytes)
	return Manifest{
		Identity: identity,
		Assets: []ManifestEntry{
			{Name: bundledLibraryName, SHA256: hexSHA256(bundledLibraryBytes), Bytes: bundledLibraryBytes, Mode: 0o755},
			{Name: "model.onnx", SHA256: hexSHA256(bundledModelBytes), Bytes: bundledModelBytes, Mode: 0o644},
			{Name: "vocab.txt", SHA256: hexSHA256(bundledVocabBytes), Bytes: bundledVocabBytes, Mode: 0o644},
		},
	}
}

func hexSHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
