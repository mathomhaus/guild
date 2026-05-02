// Tests for the platform-agnostic extract + cache-dir plumbing. These
// run on every build (no -tags=withembed) by hand-constructing a
// Manifest with synthetic bytes.

package embed

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveCacheDir_EnvOverrideWins(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(CachePathOverrideEnv, tmp)
	m := Manifest{Identity: ManifestIdentity{PlatformTag: "darwin-arm64"}}
	got, err := ResolveCacheDir(m)
	if err != nil {
		t.Fatalf("ResolveCacheDir: %v", err)
	}
	want := filepath.Join(tmp, "darwin-arm64")
	if got != want {
		t.Errorf("cache dir: got %q want %q", got, want)
	}
}

func TestResolveCacheDir_DefaultPath(t *testing.T) {
	t.Setenv(CachePathOverrideEnv, "")
	m := Manifest{Identity: ManifestIdentity{PlatformTag: "linux-amd64"}}
	got, err := ResolveCacheDir(m)
	if err != nil {
		t.Fatalf("ResolveCacheDir: %v", err)
	}
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("UserCacheDir: %v", err)
	}
	want := filepath.Join(base, "guild", "runtime", "linux-amd64")
	if got != want {
		t.Errorf("cache dir: got %q want %q", got, want)
	}
}

func TestResolveCacheDir_NoPlatformTag(t *testing.T) {
	m := Manifest{Identity: ManifestIdentity{PlatformTag: ""}}
	_, err := ResolveCacheDir(m)
	if !errors.Is(err, ErrNoAssets) {
		t.Fatalf("want ErrNoAssets, got %v", err)
	}
}

// fakeManifest builds a Manifest with three synthetic asset entries so
// Extract can be exercised without real ONNX bytes.
func fakeManifest(libBytes, modelBytes, vocabBytes []byte) Manifest {
	hh := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	return Manifest{
		Identity: ManifestIdentity{
			ModelID:        "test-model",
			TokenizerHash:  hh(vocabBytes),
			RuntimeVersion: "test-rt",
			Dim:            Dim,
			PlatformTag:    "test-triple",
		},
		Assets: []ManifestEntry{
			{Name: "libonnxruntime.dylib", SHA256: hh(libBytes), Bytes: libBytes, Mode: 0o755},
			{Name: "model.onnx", SHA256: hh(modelBytes), Bytes: modelBytes, Mode: 0o644},
			{Name: "vocab.txt", SHA256: hh(vocabBytes), Bytes: vocabBytes, Mode: 0o644},
		},
	}
}

func TestExtract_FreshWrite(t *testing.T) {
	dir := t.TempDir()
	m := fakeManifest([]byte("fake-dylib"), []byte("fake-model"), []byte("fake-vocab"))
	res, err := Extract(m, dir)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !res.Extracted {
		t.Error("expected Extracted=true on fresh dir")
	}
	if got, _ := os.ReadFile(res.LibraryPath); !bytes.Equal(got, []byte("fake-dylib")) {
		t.Errorf("library contents: got %q", got)
	}
	if got, _ := os.ReadFile(res.ModelPath); !bytes.Equal(got, []byte("fake-model")) {
		t.Errorf("model contents: got %q", got)
	}
	if got, _ := os.ReadFile(res.VocabPath); !bytes.Equal(got, []byte("fake-vocab")) {
		t.Errorf("vocab contents: got %q", got)
	}
}

func TestExtract_Idempotent_NoRewrite(t *testing.T) {
	dir := t.TempDir()
	m := fakeManifest([]byte("a"), []byte("b"), []byte("c"))
	if _, err := Extract(m, dir); err != nil {
		t.Fatalf("first extract: %v", err)
	}
	// Record mtime
	lib := filepath.Join(dir, "libonnxruntime.dylib")
	fi1, err := os.Stat(lib)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	res, err := Extract(m, dir)
	if err != nil {
		t.Fatalf("second extract: %v", err)
	}
	if res.Extracted {
		t.Error("second extract should be a no-op but Extracted=true")
	}
	fi2, err := os.Stat(lib)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Error("mtime changed on no-op second extract")
	}
}

func TestExtract_DriftForcesRewrite(t *testing.T) {
	dir := t.TempDir()
	m := fakeManifest([]byte("a"), []byte("b"), []byte("c"))
	if _, err := Extract(m, dir); err != nil {
		t.Fatalf("initial extract: %v", err)
	}
	// Corrupt the library file.
	if err := os.WriteFile(filepath.Join(dir, "libonnxruntime.dylib"), []byte("CORRUPT"), 0o600); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	res, err := Extract(m, dir)
	if err != nil {
		t.Fatalf("re-extract: %v", err)
	}
	if !res.Extracted {
		t.Error("expected Extracted=true after drift")
	}
	got, _ := os.ReadFile(res.LibraryPath)
	if !bytes.Equal(got, []byte("a")) {
		t.Errorf("library not restored: got %q", got)
	}
}

func TestExtract_NoAssets_ReturnsErrNoAssets(t *testing.T) {
	m := Manifest{Identity: ManifestIdentity{PlatformTag: "test"}}
	_, err := Extract(m, t.TempDir())
	if !errors.Is(err, ErrNoAssets) {
		t.Fatalf("want ErrNoAssets, got %v", err)
	}
}

// Quantize/Dequantize round-trip tests live in cosine_test.go. The
// legacy per-vector-scale helpers (QuantizeInt8 / DequantizeInt8) that
// once lived in backfill.go have been removed in favor of the canonical
// symmetric-127 convention in cosine.go; see the rewrite notes in
// backfill.go insertVectorRow.
