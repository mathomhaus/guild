package compression

import (
	"fmt"
	"strings"
	"testing"
)

func TestComputeOptimalKSmallReturnsAll(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}
	if got := ComputeOptimalK(items, 1.0, 3, 0); got != 5 {
		t.Fatalf("n<=8 should return n=5, got %d", got)
	}
}

func TestComputeOptimalKLowDiversity(t *testing.T) {
	items := make([]string, 10)
	for i := range items {
		items[i] = "abc"
	}
	if got := ComputeOptimalK(items, 1.0, 3, 0); got != 3 {
		t.Fatalf("10 identical → max(minK=3, unique=1) = 3, got %d", got)
	}
}

func TestComputeOptimalKRespectsBounds(t *testing.T) {
	items := make([]string, 20)
	for i := range items {
		items[i] = fmt.Sprintf("item %d", i)
	}
	if k := ComputeOptimalK(items, 1.0, 3, 10); k > 10 {
		t.Fatalf("k=%d exceeds maxK=10", k)
	}
	identical := make([]string, 20)
	for i := range identical {
		identical[i] = "abc"
	}
	if k := ComputeOptimalK(identical, 1.0, 5, 0); k != 5 {
		t.Fatalf("minK floor should hold: got %d, want 5", k)
	}
}

func TestComputeOptimalKBiasMonotonic(t *testing.T) {
	items := make([]string, 30)
	for i := range items {
		items[i] = fmt.Sprintf("item content %d", i)
	}
	kLow := ComputeOptimalK(items, 0.7, 3, 0)
	kMid := ComputeOptimalK(items, 1.0, 3, 0)
	kHigh := ComputeOptimalK(items, 1.5, 3, 0)
	if !(kLow <= kMid && kMid <= kHigh) {
		t.Fatalf("bias should be monotonic: %d <= %d <= %d", kLow, kMid, kHigh)
	}
}

func TestFindKnee(t *testing.T) {
	if _, ok := findKnee([]int{1, 2}); ok {
		t.Error("curve shorter than 3 has no knee")
	}
	if k, ok := findKnee([]int{5, 5, 5, 5, 5}); !ok || k != 1 {
		t.Errorf("flat curve returns (1,true), got (%d,%v)", k, ok)
	}
	if k, ok := findKnee([]int{1, 5, 8, 9, 10, 10, 10, 10, 10}); !ok || k != 3 {
		t.Errorf("concave curve knee = (3,true), got (%d,%v)", k, ok)
	}
	if _, ok := findKnee([]int{1, 2, 3, 4, 5, 6, 7, 8, 9}); ok {
		t.Error("diagonal curve has no clear knee")
	}
}

func TestSimhashDeterministicAndLowercase(t *testing.T) {
	if simhash("ABC") != simhash("abc") {
		t.Error("simhash should lowercase its input")
	}
	a := simhash("hello world")
	b := simhash(strings.Clone("hello world"))
	if a != b {
		t.Error("simhash should be deterministic")
	}
}

func TestCountUniqueSimhash(t *testing.T) {
	if countUniqueSimhash(nil, 3) != 0 {
		t.Error("empty input has 0 unique")
	}
	if got := countUniqueSimhash([]string{"abc", "abc", "abc"}, 3); got != 1 {
		t.Errorf("all-identical → 1 cluster, got %d", got)
	}
	diverse := []string{
		"the cat sat on the mat",
		"the dog ran in the park",
		"a fish swam in the sea",
	}
	if got := countUniqueSimhash(diverse, 3); got != 3 {
		t.Errorf("diverse items → 3 clusters, got %d", got)
	}
}

func TestBigramCurve(t *testing.T) {
	got := computeUniqueBigramCurve([]string{"the cat", "the dog", "a fish"})
	want := []int{1, 2, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bigram curve = %v, want %v", got, want)
		}
	}
	// Single-word dedupe: third "hello" duplicates the first.
	got2 := computeUniqueBigramCurve([]string{"hello", "world", "hello"})
	if got2[2] != 2 {
		t.Errorf("single-word dedupe curve = %v, want [..2]", got2)
	}
}
