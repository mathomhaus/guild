// Fallback manifest for `-tags=withembed` on triples that do NOT have
// a bundled asset set (e.g. windows/amd64, freebsd/amd64). Keeps the
// package linkable under the tag so CI matrix builds do not need to
// special-case "drop the tag on windows".
//
// Triples covered by the affirmative bundled files:
//
//	darwin/arm64, darwin/amd64, linux/amd64, linux/arm64
//
// Everything else under `-tags=withembed` falls through to this file.

//go:build withembed && !((darwin && arm64) || (darwin && amd64) || (linux && amd64) || (linux && arm64))

package embed

func manifestHasAssets() bool { return false }

func currentManifest() Manifest {
	return Manifest{
		Identity: DefaultIdentity(),
		Assets:   nil,
	}
}
