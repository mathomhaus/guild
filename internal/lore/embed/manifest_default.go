// Default manifest source: no bundled assets. Active on every build
// that does NOT pass `-tags=withembed`.
//
// This file keeps `make check` green on a fresh clone (no binary
// artifacts required at build time). Under `-tags=withembed` a
// per-platform manifest_bundled_<goos>_<goarch>.go takes over on
// supported triples, and manifest_withembed_unsupported.go takes over
// on every other (goos, goarch) pair so the package still links.

//go:build !withembed

package embed

func manifestHasAssets() bool { return false }

func currentManifest() Manifest {
	return Manifest{
		Identity: DefaultIdentity(),
		Assets:   nil,
	}
}
