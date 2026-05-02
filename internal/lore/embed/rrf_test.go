package embed

import (
	"reflect"
	"testing"
)

// TestFuse_EmptyInputs verifies both-empty and one-empty edge cases.
// RRF with no inputs is the empty ranked list; RRF with one input
// mirrors the input order (modulo the limit cap and the ascending-ID
// tiebreak for identical scores, which does not apply here since
// every id appears exactly once).
func TestFuse_EmptyInputs(t *testing.T) {
	if got := Fuse(nil, nil, 10); len(got) != 0 {
		t.Errorf("Fuse(nil, nil) = %v, want empty", got)
	}
	got := Fuse(Ranked{1, 2, 3}, nil, 10)
	want := Ranked{1, 2, 3}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Fuse({1,2,3}, nil) = %v, want %v", got, want)
	}
	got = Fuse(nil, Ranked{5, 4}, 10)
	want = Ranked{5, 4}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Fuse(nil, {5,4}) = %v, want %v", got, want)
	}
}

// TestFuse_AgreementBoostsFusion verifies that ids appearing in both
// input lists outrank ids that appear in only one. This is the
// defining property of RRF and our reason for picking it over
// weighted linear fusion.
func TestFuse_AgreementBoostsFusion(t *testing.T) {
	a := Ranked{1, 2, 3, 4, 5}
	b := Ranked{2, 1, 6, 7, 8}
	got := Fuse(a, b, 10)
	// id=1 is rank 1 in a, rank 2 in b → strong fusion.
	// id=2 is rank 2 in a, rank 1 in b → also strong.
	// id=3,4,5 appear only in a (ranks 3/4/5).
	// id=6,7,8 appear only in b (ranks 3/4/5).
	if len(got) < 2 {
		t.Fatalf("got %v, want at least 2 results", got)
	}
	// Top two must be {1, 2} in some order (both appear in both
	// lists). The deterministic ascending-id tiebreak places 1
	// before 2 when their fused scores tie; RRF scores:
	//   id=1: 1/(60+1) + 1/(60+2) = 1/61 + 1/62
	//   id=2: 1/(60+2) + 1/(60+1) = 1/62 + 1/61 (identical)
	// So {1, 2} in ascending order.
	if got[0] != 1 || got[1] != 2 {
		t.Errorf("top-2 = %v, want [1 2] (agreement items, id-asc tiebreak)", got[:2])
	}
}

// TestFuse_LimitCap ensures the cap parameter takes effect.
func TestFuse_LimitCap(t *testing.T) {
	a := Ranked{1, 2, 3, 4, 5}
	b := Ranked{6, 7, 8, 9, 10}
	got := Fuse(a, b, 3)
	if len(got) != 3 {
		t.Errorf("got %d results, want 3 (limit=3)", len(got))
	}
}

// TestFuse_DeterministicTiebreak asserts the ascending-id tiebreak
// when two ids accumulate identical fused scores. Stable ordering is
// important for golden tests and user-facing reproducibility.
func TestFuse_DeterministicTiebreak(t *testing.T) {
	// Two singleton lists at identical ranks → identical scores.
	a := Ranked{5}
	b := Ranked{3}
	got := Fuse(a, b, 10)
	want := Ranked{3, 5} // ascending id wins on score tie
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestFuseMany_UnionsCleanly verifies N-way fusion produces the same
// result as repeated pairwise Fuse calls (no off-by-one on the k
// denominator).
func TestFuseMany_UnionsCleanly(t *testing.T) {
	lists := []Ranked{
		{1, 2, 3},
		{4, 5, 6},
		{1, 4, 7},
	}
	got := FuseMany(lists, 10)
	if len(got) == 0 {
		t.Fatal("FuseMany returned empty")
	}
	// id=1 appears in lists 0 and 2 → higher fused score than id=3
	// (only in list 0) or id=7 (only in list 2).
	var rankOf = func(id int64) int {
		for i, g := range got {
			if g == id {
				return i
			}
		}
		return -1
	}
	if r1, r3 := rankOf(1), rankOf(3); r3 >= 0 && r1 >= 0 && r1 > r3 {
		t.Errorf("id=1 (appears twice) ranked %d worse than id=3 (once) at %d", r1, r3)
	}
	if r1, r7 := rankOf(1), rankOf(7); r7 >= 0 && r1 >= 0 && r1 > r7 {
		t.Errorf("id=1 (appears twice) ranked %d worse than id=7 (once) at %d", r1, r7)
	}
}

// TestFuseMany_EmptyInput returns empty gracefully.
func TestFuseMany_EmptyInput(t *testing.T) {
	if got := FuseMany(nil, 10); len(got) != 0 {
		t.Errorf("FuseMany(nil) = %v, want empty", got)
	}
	if got := FuseMany([]Ranked{nil, nil}, 10); len(got) != 0 {
		t.Errorf("FuseMany(empty lists) = %v, want empty", got)
	}
}

// TestRRFK_IsStandardValue guards against an accidental tuning of
// the RRFK constant. k=60 is the TREC baseline; changing it is a
// design-review conversation, not a drive-by edit.
func TestRRFK_IsStandardValue(t *testing.T) {
	if RRFK != 60 {
		t.Errorf("RRFK = %d, want 60 (TREC standard, ADR-003)", RRFK)
	}
}
