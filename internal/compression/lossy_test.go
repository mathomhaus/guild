package compression

import (
	"fmt"
	"strings"
	"testing"
)

// assertCCRRestore checks that a lossy strategy's emitted marker, fed back
// through the store, reproduces the exact original content.
func assertCCRRestore(t *testing.T, name, content, context string) {
	t.Helper()
	store := NewMemStore()
	s, err := BuildStrategy(name)
	if err != nil {
		t.Fatal(err)
	}
	if s.Lossless() {
		t.Fatalf("%s should be lossy-with-CCR", name)
	}
	res, err := s.Compress(content, context, store)
	if err != nil {
		t.Fatal(err)
	}
	if res.CacheKey == "" {
		t.Fatalf("%s did not emit a CCR key for content it should compress", name)
	}
	hash := ExtractMarkerHash(res.Compressed)
	if hash != res.CacheKey {
		t.Fatalf("%s marker hash %q != CacheKey %q", name, hash, res.CacheKey)
	}
	original, ok := store.Get(hash)
	if !ok {
		t.Fatalf("%s CCR store missing the stashed original", name)
	}
	if original != content {
		t.Fatalf("%s CCR restore is not the exact original\n got:  %q\n want: %q", name, original, content)
	}
}

func TestDiffPassThroughShort(t *testing.T) {
	in := "diff --git a/x b/x\n@@ -1 +1 @@\n-a\n+b"
	r := NewDiffCompressor(DefaultDiffConfig()).Compress(in, "", nil)
	if r.Compressed != in {
		t.Fatalf("short diff should pass through unchanged")
	}
	if r.CacheKey != "" {
		t.Fatal("pass-through must not mint a CacheKey")
	}
}

func TestDiffNonDiffPassThrough(t *testing.T) {
	in := strings.Repeat("this is not a diff\n", 60)
	r := NewDiffCompressor(DefaultDiffConfig()).Compress(in, "", nil)
	if r.FilesAffected != 0 {
		t.Fatal("non-diff input should not parse as a diff")
	}
	if r.Compressed != in {
		t.Fatal("non-diff input should pass through verbatim")
	}
}

func TestDiffCompressesAndRestores(t *testing.T) {
	content := buildSyntheticDiff(8)
	r := NewDiffCompressor(DefaultDiffConfig()).Compress(content, "", NewMemStore())
	if r.FilesAffected != 8 {
		t.Errorf("files affected = %d, want 8", r.FilesAffected)
	}
	if r.Additions != 40 || r.Deletions != 24 {
		t.Errorf("+/- = %d/%d, want 40/24", r.Additions, r.Deletions)
	}
	if r.CacheKey == "" {
		t.Error("expected a CCR key for an 8-file diff")
	}
	assertCCRRestore(t, "diff", content, "")
}

func TestDiffPerFileHunkCap(t *testing.T) {
	cfg := DefaultDiffConfig()
	cfg.MaxHunksPerFile = 10
	content := buildNHunkDiff(15)
	r := NewDiffCompressor(cfg).Compress(content, "", nil)
	if r.HunksKept != 10 {
		t.Errorf("hunks kept = %d, want 10", r.HunksKept)
	}
	if r.HunksRemoved != 5 {
		t.Errorf("hunks removed = %d, want 5", r.HunksRemoved)
	}
}

func TestLogCompressesAndRestores(t *testing.T) {
	var b strings.Builder
	b.WriteString("=== test session starts ===\n")
	for i := 0; i < 80; i++ {
		b.WriteString(fmt.Sprintf("INFO line %d doing routine work\n", i))
	}
	b.WriteString("ERROR something exploded at module X\n")
	b.WriteString("Traceback (most recent call last)\n")
	b.WriteString("  File \"x.py\", line 4\n")
	b.WriteString("ValueError: boom\n")
	for i := 0; i < 80; i++ {
		b.WriteString(fmt.Sprintf("INFO more chatter %d\n", i))
	}
	b.WriteString("=== 1 failed, 159 passed ===\n")
	content := b.String()

	r := NewLogCompressor(DefaultLogConfig()).Compress(content, 0, NewMemStore())
	if r.CompressedLineCount >= r.OriginalLineCount {
		t.Errorf("log should compress: %d → %d lines", r.OriginalLineCount, r.CompressedLineCount)
	}
	if r.CacheKey == "" {
		t.Fatal("expected a CCR key for a long noisy log")
	}
	assertCCRRestore(t, "log", content, "")
	if !strings.Contains(r.Compressed, "ERROR something exploded") {
		t.Error("the error line should survive compression")
	}
}

func TestLogShortPassThrough(t *testing.T) {
	in := "INFO a\nINFO b\nERROR c\n"
	r := NewLogCompressor(DefaultLogConfig()).Compress(in, 0, nil)
	if r.Compressed != in || r.CacheKey != "" {
		t.Fatal("short log should pass through with no CCR key")
	}
}

func TestSearchCompressesAndRestores(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString(fmt.Sprintf("src/file_%d.py:%d:def handler_%d():\n", i%5, i+10, i))
	}
	content := b.String()
	r := NewSearchCompressor(DefaultSearchConfig()).Compress(content, "handler", 0, NewMemStore())
	if r.OriginalMatchCount != 40 {
		t.Errorf("original matches = %d, want 40", r.OriginalMatchCount)
	}
	if r.CompressedMatchCount >= r.OriginalMatchCount {
		t.Errorf("search should drop matches: %d → %d", r.OriginalMatchCount, r.CompressedMatchCount)
	}
	if r.CacheKey == "" {
		t.Fatal("expected a CCR key for 40 matches")
	}
	assertCCRRestore(t, "search", content, "handler")
}

func TestParseMatchLine(t *testing.T) {
	cases := []struct {
		in   string
		file string
		line uint64
		body string
		ok   bool
	}{
		{"src/main.py:42:def main():", "src/main.py", 42, "def main():", true},
		{"src/main.py-43-context after", "src/main.py", 43, "context after", true},
		{"pre-commit-config.yaml-7-value", "pre-commit-config.yaml", 7, "value", true},
		{`C:\Users\foo\bar.py:9:line`, `C:\Users\foo\bar.py`, 9, "line", true},
		{"no line number here", "", 0, "", false},
	}
	for _, c := range cases {
		f, n, body, ok := parseMatchLine(c.in)
		if ok != c.ok || (ok && (f != c.file || n != c.line || body != c.body)) {
			t.Errorf("parseMatchLine(%q) = (%q,%d,%q,%v), want (%q,%d,%q,%v)",
				c.in, f, n, body, ok, c.file, c.line, c.body, c.ok)
		}
	}
}

func TestDetectStrategy(t *testing.T) {
	cases := map[string]string{
		`[{"a":1},{"a":2}]`:                       "json",
		"diff --git a/x b/x\n@@ -1 +1 @@\n-a\n+b": "diff",
		"src/a.py:1:hit\nsrc/b.py:2:hit\n":        "search",
		"INFO chatter\nrandom text\n":             "log",
	}
	for in, want := range cases {
		if got := DetectStrategy(in); got != want {
			t.Errorf("DetectStrategy(%q) = %q, want %q", in, got, want)
		}
	}
}

// ─── synthetic builders (mirrors Headroom's diff test fixtures) ─────────

func buildSyntheticDiff(nFiles int) string {
	var s strings.Builder
	for i := 0; i < nFiles; i++ {
		fmt.Fprintf(&s, "diff --git a/file_%d.py b/file_%d.py\n--- a/file_%d.py\n+++ b/file_%d.py\n@@ -1,10 +1,12 @@\n", i, i, i, i)
		for k := 0; k < 5; k++ {
			fmt.Fprintf(&s, " context_%d_%d\n", k, i)
		}
		for k := 0; k < 3; k++ {
			fmt.Fprintf(&s, "-removed_%d_%d\n", k, i)
		}
		for k := 0; k < 5; k++ {
			fmt.Fprintf(&s, "+added_%d_%d\n", k, i)
		}
		for k := 0; k < 5; k++ {
			fmt.Fprintf(&s, " tail_%d_%d\n", k, i)
		}
	}
	s.WriteString("# variant 1")
	return s.String()
}

func buildNHunkDiff(n int) string {
	var s strings.Builder
	s.WriteString("diff --git a/big.py b/big.py\n--- a/big.py\n+++ b/big.py\n")
	for i := 0; i < n; i++ {
		start := i*100 + 1
		fmt.Fprintf(&s, "@@ -%d,6 +%d,6 @@\n", start, start)
		fmt.Fprintf(&s, " ctx_a_%d\n ctx_b_%d\n-old_%d\n+new_%d\n ctx_c_%d\n ctx_d_%d\n", i, i, i, i, i, i)
	}
	return s.String()
}
