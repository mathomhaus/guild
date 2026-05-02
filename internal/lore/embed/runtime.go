package embed

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// RuntimeConfig points at the three on-disk artifacts the BGE unix path
// needs. Exposed here (not in a unix-only file) so callers on every
// platform can construct and serialize the same config object; only the
// actual loader is unix-gated.
//
//	LibraryPath: absolute path to libonnxruntime.dylib/.so
//	ModelPath:   absolute path to model.onnx
//	VocabPath:   absolute path to vocab.txt
//
// All three MUST be absolute. Relative paths defeat the extract-to-tmp
// packaging story (F3 from the spike friction log: ORT's own default
// library resolution against DYLD/LD paths is undocumented and fails in
// most environments; absolute path is the only supported contract).
type RuntimeConfig struct {
	LibraryPath string
	ModelPath   string
	VocabPath   string
	// MaxLen caps the token sequence including [CLS] and [SEP]. Zero
	// or negative picks the bge-small default of 512 at construction
	// time. Values larger than the model's position-embedding table
	// produce undefined ONNX behavior; stick to 512 unless the model
	// supports longer context.
	MaxLen int
	// NumThreads is the intra-op thread count passed to ORT's session
	// options. Zero or negative defers to ORT's default (runtime.NumCPU
	// on most builds). Set to 1 for tests or deterministic benchmarks.
	NumThreads int
}

// ORTAPIVersion is the ORT C API version this package pins to via
// shota3506/onnxruntime-purego's supportedAPIVersions check. F2 from the
// spike friction log: the library hard-codes this to 23, rejects any
// other value, and thereby ties us to ORT 1.23.x. Bumping this requires
// an explicit ADR.
const ORTAPIVersion = 23

// CachePathOverrideEnv is the environment variable callers may set to
// relocate the runtime cache directory. Documented in ADR-003 as the
// safety valve for read-only HOME environments.
const CachePathOverrideEnv = "GUILD_EMBEDDED_CACHE"

// ErrNoAssets signals that the current build carries no bundled asset
// bytes. Callers fall through to NullEmbedder and write
// meta.embedder_state='disabled' with a structured reason.
var ErrNoAssets = errors.New("embed: no bundled assets in this build")

// ErrAssetSHAMismatch signals that after extraction (or after a cache
// hit) the on-disk file's SHA did not match the manifest's SHA. Only
// returned from Extract when re-extraction itself failed to reconcile.
var ErrAssetSHAMismatch = errors.New("embed: asset SHA mismatch")

// ExtractResult describes what Extract just did. Callers log Extracted
// (wrote bytes) vs reused (cache hit) for the init-timing diagnostic.
type ExtractResult struct {
	// CacheDir is the per-platform directory the three assets live
	// under. Always absolute.
	CacheDir string
	// LibraryPath / ModelPath / VocabPath are the absolute paths to
	// each extracted asset. Empty when Extract returned a non-nil err.
	LibraryPath string
	ModelPath   string
	VocabPath   string
	// Extracted is true when Extract wrote at least one byte to disk
	// (fresh bundle or checksum drift). False when every file matched
	// its manifest SHA on entry.
	Extracted bool
	// Per-asset wall-clock durations for the init-timing diagnostic
	// (LORE-368). Each covers only its own verifyOrWrite call.
	// Structured slog field names:
	//   extract_duration_ms_dylib  - libonnxruntime dylib / .so
	//   extract_duration_ms_model  - model.onnx
	//   extract_duration_ms_vocab  - vocab.txt
	//   extract_duration_ms_total  - sum of all three
	DylibDuration time.Duration
	ModelDuration time.Duration
	VocabDuration time.Duration
	TotalDuration time.Duration
}

// ResolveCacheDir returns the absolute cache directory for the current
// platform. Resolution order: GUILD_EMBEDDED_CACHE env var, then
// os.UserCacheDir() (e.g. ~/Library/Caches on macOS, $XDG_CACHE_HOME or
// ~/.cache on Linux). The platform tag from the manifest is appended so
// two builds for different arches on the same host do not collide.
//
// Returns ("", ErrNoAssets) when the manifest reports no platform tag
// (unsupported build). Callers branch on that to skip extraction.
func ResolveCacheDir(m Manifest) (string, error) {
	if m.Identity.PlatformTag == "" {
		return "", ErrNoAssets
	}
	if v := os.Getenv(CachePathOverrideEnv); v != "" {
		return filepath.Join(v, m.Identity.PlatformTag), nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("embed: resolve user cache dir: %w", err)
	}
	return filepath.Join(base, "guild", "runtime", m.Identity.PlatformTag), nil
}

// Extract writes the manifest's three assets to cacheDir, verifying
// SHA-256 for every file. Existing files matching their manifest SHA
// are left in place (fast warm path); mismatching files are rewritten
// atomically (temp file + fsync + rename). Returns an ExtractResult
// describing the outcome.
//
// Extract is idempotent: running it twice in a row on an unchanged
// bundle touches nothing the second time. Safe to call from
// long-running processes; on a concurrent writer the rename loses the
// race but the winning file is still the correct SHA.
func Extract(m Manifest, cacheDir string) (*ExtractResult, error) {
	if !m.hasAssetBytes() {
		return nil, ErrNoAssets
	}
	if cacheDir == "" {
		return nil, fmt.Errorf("embed: Extract: empty cacheDir")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("embed: mkdir %s: %w", cacheDir, err)
	}

	res := &ExtractResult{CacheDir: cacheDir}
	extractStart := time.Now()
	for i, entry := range m.Assets {
		path := filepath.Join(cacheDir, entry.Name)
		assetStart := time.Now()
		wrote, err := verifyOrWrite(path, entry.Bytes, entry.SHA256, os.FileMode(entry.Mode))
		dur := time.Since(assetStart)
		if err != nil {
			return nil, fmt.Errorf("embed: extract %q: %w", entry.Name, err)
		}
		if wrote {
			res.Extracted = true
		}
		switch i {
		case AssetLibrary:
			res.LibraryPath = path
			res.DylibDuration = dur
		case AssetModel:
			res.ModelPath = path
			res.ModelDuration = dur
		case AssetVocab:
			res.VocabPath = path
			res.VocabDuration = dur
		}
	}
	res.TotalDuration = time.Since(extractStart)
	return res, nil
}

// hasAssetBytes reports whether every manifest entry has non-empty
// bytes. Zero-length bytes with a non-empty platform tag indicates a
// staging misconfiguration (e.g. `-tags=withembed` built against an
// empty assets/ dir); fail fast rather than extract empty files.
func (m Manifest) hasAssetBytes() bool {
	if len(m.Assets) == 0 {
		return false
	}
	for _, a := range m.Assets {
		if len(a.Bytes) == 0 {
			return false
		}
	}
	return true
}

// verifyOrWrite returns (true, nil) if it wrote bytes to path and
// (false, nil) if the existing file already matched wantSHA. Any other
// return value is an error.
//
// Atomic rename pattern: write to path+".tmp-<pid>", fsync, rename.
// This prevents partial extraction if the process dies mid-write. The
// temp file is removed on any error path.
func verifyOrWrite(path string, bytes []byte, wantSHA string, mode os.FileMode) (bool, error) {
	if got, err := fileSHA256Hex(path); err == nil && got == wantSHA {
		return false, nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return false, fmt.Errorf("open tmp: %w", err)
	}
	if _, err := f.Write(bytes); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return false, fmt.Errorf("write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return false, fmt.Errorf("fsync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("close tmp: %w", err)
	}
	if mode != 0 {
		if err := os.Chmod(tmp, mode); err != nil {
			_ = os.Remove(tmp)
			return false, fmt.Errorf("chmod tmp: %w", err)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("rename tmp: %w", err)
	}
	// Post-rename SHA verify: guards against a concurrent writer that
	// won the rename race with different bytes.
	if got, err := fileSHA256Hex(path); err != nil {
		return true, fmt.Errorf("post-rename sha: %w", err)
	} else if got != wantSHA {
		return true, fmt.Errorf("%w: %s: got %s want %s", ErrAssetSHAMismatch, path, got, wantSHA)
	}
	return true, nil
}

// fileSHA256Hex returns the hex SHA-256 of the file at path. Returns
// fs.ErrNotExist wrapped unchanged so callers can errors.Is-check.
func fileSHA256Hex(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
