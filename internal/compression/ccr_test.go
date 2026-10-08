package compression

import (
	"testing"
	"time"
)

func TestComputeKeyDeterministicAndDistinct(t *testing.T) {
	a := ComputeKey("the same payload")
	b := ComputeKey("the same payload")
	if a != b {
		t.Fatalf("ComputeKey not deterministic: %q != %q", a, b)
	}
	if len(a) != KeyHexLen {
		t.Fatalf("ComputeKey length = %d, want %d", len(a), KeyHexLen)
	}
	if ComputeKey("alpha") == ComputeKey("beta") {
		t.Fatal("ComputeKey collided on distinct payloads")
	}
	for _, c := range a {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("ComputeKey produced non-lowercase-hex char %q", c)
		}
	}
}

func TestMarkerFormatAndExtraction(t *testing.T) {
	if got := MarkerFor("abc123"); got != "<<ccr:abc123>>" {
		t.Fatalf("MarkerFor = %q, want <<ccr:abc123>>", got)
	}
	cases := map[string]string{
		"<<ccr:deadbeefcafe>>":                             "deadbeefcafe",
		"some text <<ccr:deadbeefcafe,base64,2.1KB>> x":    "deadbeefcafe",
		"footer\n[10 lines compressed. hash=abcdef012345]": "abcdef012345",
		"no marker here":                                   "",
	}
	for in, want := range cases {
		if got := ExtractMarkerHash(in); got != want {
			t.Errorf("ExtractMarkerHash(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMemStorePutGet(t *testing.T) {
	s := NewMemStore()
	key := ComputeKey("original payload")
	s.Put(key, "original payload")
	got, ok := s.Get(key)
	if !ok || got != "original payload" {
		t.Fatalf("Get after Put = (%q, %v), want (original payload, true)", got, ok)
	}
	if _, ok := s.Get("missing"); ok {
		t.Fatal("Get of missing key returned ok=true")
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
	// Idempotent re-store keeps one entry.
	s.Put(key, "original payload")
	if s.Len() != 1 {
		t.Fatalf("Len after re-store = %d, want 1", s.Len())
	}
}

func TestMemStoreTTLExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }
	s := NewMemStoreWith(10, 5*time.Minute, clock)
	s.Put("k", "v")
	if _, ok := s.Get("k"); !ok {
		t.Fatal("entry should be live immediately after Put")
	}
	// Advance past the TTL.
	now = now.Add(6 * time.Minute)
	if _, ok := s.Get("k"); ok {
		t.Fatal("entry should be expired past its TTL")
	}
	// Lazy expiry dropped it.
	if s.Len() != 0 {
		t.Fatalf("Len after expiry-on-read = %d, want 0", s.Len())
	}
}

func TestMemStoreCapacityEviction(t *testing.T) {
	s := NewMemStoreWith(3, time.Hour, nil)
	for i, p := range []string{"a", "b", "c", "d", "e"} {
		s.Put(ComputeKey(p), p)
		_ = i
	}
	if s.Len() != 3 {
		t.Fatalf("Len = %d, want 3 (capacity bound)", s.Len())
	}
	// Oldest two (a, b) evicted; newest three (c, d, e) survive.
	if _, ok := s.Get(ComputeKey("a")); ok {
		t.Error("oldest entry 'a' should have been evicted")
	}
	if _, ok := s.Get(ComputeKey("e")); !ok {
		t.Error("newest entry 'e' should survive")
	}
}

func TestMarkerRoundTripThroughStore(t *testing.T) {
	s := NewMemStore()
	original := "the full original payload that got stashed"
	key := ComputeKey(original)
	s.Put(key, original)
	marker := MarkerFor(key)
	// Simulate pasting a block that embeds the marker.
	block := "compact view ...\n" + marker + "\n"
	hash := ExtractMarkerHash(block)
	if hash != key {
		t.Fatalf("extracted hash %q != key %q", hash, key)
	}
	got, ok := s.Get(hash)
	if !ok || got != original {
		t.Fatalf("retrieve via marker = (%q, %v), want original", got, ok)
	}
}
