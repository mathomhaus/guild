// Manifest: the static identity + SHA record for an embedded runtime
// bundle. Exposes the values guild init writes into meta (model_id,
// tokenizer_hash, runtime_version, dim) and the per-asset SHA-256 digests
// the extractor verifies before writing to the cache.
//
// Two flavors of this package exist, selected by Go build tag:
//
//	default build:       HasAssets()=false, zero-length asset bytes.
//	`-tags=withembed`:   HasAssets()=true,  asset bytes populated by
//	                     per-platform go:embed directives in
//	                     assets_bundled_<goos>_<goarch>.go.
//
// This split keeps `make check` green on fresh clones (no 65 MB of
// binaries in git) while letting release CI stage the assets via
// `make assets` and then build with `-tags=withembed` for a true
// single-binary ship.

package embed

// ManifestEntry carries one bundled asset's identity: its on-disk name
// after extraction, the SHA-256 hex digest the extractor verifies
// against, and the raw bytes (empty outside `-tags=withembed` builds).
type ManifestEntry struct {
	// Name is the on-disk filename written under the cache dir.
	// Stable across platforms; the dylib/.so extension difference is
	// encoded in Name itself (set per-platform in the embed file).
	Name string
	// SHA256 is the hex-encoded SHA-256 of the embedded bytes. Empty
	// when HasAssets()=false.
	SHA256 string
	// Bytes is the raw embedded asset. Empty when HasAssets()=false.
	Bytes []byte
	// Mode is the mode bits applied to the extracted file. 0o755 for
	// the dylib/.so so dlopen succeeds on kernels that honor it; 0o644
	// for the model and vocab.
	Mode uint32
}

// ManifestIdentity carries the embedder-identity rows guild init writes
// into meta when the probe succeeds. Values come from the bundling
// step (see assets/README.md) so the binary and the DB agree on what
// model is live.
type ManifestIdentity struct {
	// ModelID is the canonical embedder name written into
	// meta.embedder_model_id. Used for the upgrade check in
	// backfill.go (invariant 2 of ADR-003): if stored model_id differs
	// from this, every vector is invalidated and re-backfilled.
	ModelID string
	// TokenizerHash is a digest of the vocab file. Lets upgrade logic
	// catch a drift in the tokenizer even when ModelID is unchanged.
	TokenizerHash string
	// RuntimeVersion is the ONNX Runtime version string (e.g.
	// "onnxruntime-1.23.0"). Informational; written into meta but not
	// used for identity comparison.
	RuntimeVersion string
	// Dim is the embedding dimension (always Dim for bge-small).
	Dim int
	// PlatformTag is the triple this bundle targets, e.g.
	// "darwin-arm64". Used for the cache subdirectory so two arches on
	// the same machine do not step on each other's extracted bytes.
	PlatformTag string
}

// Manifest is the top-level record exposed to callers: identity plus
// the per-asset entries (library, model, vocab). Index into Assets by
// AssetLibrary / AssetModel / AssetVocab.
type Manifest struct {
	Identity ManifestIdentity
	Assets   []ManifestEntry
}

// Asset index constants. Keep in sync with the order in which
// assetsBundled populates the slice.
const (
	AssetLibrary = 0
	AssetModel   = 1
	AssetVocab   = 2
)

// HasAssets reports whether this build carries embedded asset bytes.
// False on default builds and on unsupported platforms even when
// `-tags=withembed` is set. Callers (guild init, the probe path) use
// this to decide whether to attempt extraction.
func HasAssets() bool {
	return manifestHasAssets()
}

// CurrentManifest returns the manifest bound to this binary. On builds
// without assets the returned manifest still has Identity populated
// with canonical defaults so the meta seeds remain consistent across
// builds; Assets is empty.
func CurrentManifest() Manifest {
	return currentManifest()
}

// DefaultIdentity returns the canonical identity used when no bundled
// runtime is present. Matches the seed rows in migration 003 so a
// non-bundled binary reporting identity does not drift from the
// schema's embedder_model_id row.
func DefaultIdentity() ManifestIdentity {
	return ManifestIdentity{
		ModelID:        "bge-small-en-v1.5-int8-cls",
		TokenizerHash:  "",
		RuntimeVersion: "onnxruntime-1.23.x",
		Dim:            Dim,
		PlatformTag:    "",
	}
}
