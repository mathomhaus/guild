package compression

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// roundTrip compacts content and restores it, asserting the restored array
// equals the original array as parsed JSON (key order is not significant).
func assertJSONRoundTrip(t *testing.T, content string) string {
	t.Helper()
	block, ok := CompactJSON(content)
	if !ok {
		t.Fatalf("CompactJSON declined for compactable input:\n%s", content)
	}
	restored, ok := RestoreCrushed(block)
	if !ok {
		t.Fatalf("RestoreCrushed declined for block:\n%s", block)
	}
	if !jsonArraysEqual(t, content, restored) {
		t.Fatalf("round-trip mismatch:\n original: %s\n restored: %s\n block:\n%s", content, restored, block)
	}
	return block
}

func jsonArraysEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var va, vb []map[string]any
	if err := json.Unmarshal([]byte(a), &va); err != nil {
		t.Fatalf("unmarshal a: %v", err)
	}
	if err := json.Unmarshal([]byte(b), &vb); err != nil {
		t.Fatalf("unmarshal b: %v", err)
	}
	return reflect.DeepEqual(va, vb)
}

func TestCrusherLosslessHomogeneous(t *testing.T) {
	content := `[{"id":1,"name":"alice","active":true},{"id":2,"name":"bob","active":false},{"id":3,"name":"carol","active":true}]`
	block := assertJSONRoundTrip(t, content)
	if !strings.HasPrefix(block, "[3]{") {
		t.Errorf("expected [3]{...} declaration, got first line: %q", strings.SplitN(block, "\n", 2)[0])
	}
}

func TestCrusherLosslessSparseFields(t *testing.T) {
	// Missing field (row 2 has no "email") must restore as absent, distinct
	// from a present null.
	content := `[{"id":1,"email":"a@x.com"},{"id":2},{"id":3,"email":null}]`
	assertJSONRoundTrip(t, content)
}

func TestCrusherLosslessStringsNeedingQuotes(t *testing.T) {
	content := `[{"id":1,"note":"has, comma and \"quote\""},{"id":2,"note":"plain"}]`
	assertJSONRoundTrip(t, content)
}

func TestCrusherLosslessNumbersAndFloats(t *testing.T) {
	content := `[{"k":1,"f":1.5},{"k":2,"f":2.25},{"k":3,"f":3}]`
	assertJSONRoundTrip(t, content)
}

func TestCrusherLosslessEmptyStringVsMissing(t *testing.T) {
	// Present empty string vs missing field must round-trip distinctly.
	content := `[{"a":"","b":"x"},{"b":"y"}]`
	assertJSONRoundTrip(t, content)
}

func TestCrusherDeclinesNonCompactable(t *testing.T) {
	for _, in := range []string{
		`[1,2,3]`,           // not objects
		`[{"a":1}]`,         // fewer than 2
		`{"a":1}`,           // not an array
		`not json at all`,   // garbage
		`[{"a":1},"mixed"]`, // mixed element kinds
	} {
		if _, ok := CompactJSON(in); ok {
			t.Errorf("CompactJSON should decline %q", in)
		}
	}
}

func TestCrusherStrategyIsLossless(t *testing.T) {
	s, err := BuildStrategy("json")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Lossless() {
		t.Fatal("json strategy should report Lossless() == true")
	}
	content := `[{"id":1,"v":"a"},{"id":2,"v":"b"}]`
	res, err := s.Compress(content, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Lossless || res.CacheKey != "" {
		t.Fatalf("lossless strategy must not mint a CacheKey: %+v", res)
	}
	if res.CompressedBytes >= res.OriginalBytes {
		t.Logf("note: compressed %d vs original %d (small input)", res.CompressedBytes, res.OriginalBytes)
	}
	// The compacted form must still restore.
	if _, ok := RestoreCrushed(res.Compressed); !ok {
		t.Fatal("strategy output should restore")
	}
}
