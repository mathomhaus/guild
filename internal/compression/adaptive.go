package compression

import (
	"bytes"
	"compress/zlib"
	"hash/fnv"
	"math/bits"
	"strings"
)

// Adaptive compression sizing via information-saturation detection. Port of
// Headroom's adaptive_sizer.rs / adaptive_sizer.py. The log and search
// compressors use ComputeOptimalK to decide how many items to keep:
// statistically, by detecting the knee point of an information-saturation
// curve rather than a fixed cap.
//
// Three-tier decision:
//  1. Fast path: trivial cases (n <= 8 keep all) and near-total redundancy
//     (<= 3 unique-by-simhash groups keep that count).
//  2. Standard: Kneedle on the cumulative unique-bigram coverage curve.
//  3. Validation: zlib-ratio sanity check; if keeping k produces a far-more-
//     redundant subset than the full set, bump k by 20%.
//
// SimHash uses MD5 of character 4-grams (stdlib crypto/md5); the zlib check
// uses compress/zlib at best-speed (level 1), the closest stdlib analogue to
// Python's zlib level=1.

// ComputeOptimalK returns the number of items to keep. items are string
// representations in importance order; bias multiplies the knee (>1 keeps
// more, <1 compresses harder); minK is the floor; maxK caps (0 means "no cap"
// = len(items)).
func ComputeOptimalK(items []string, bias float64, minK, maxK int) int {
	n := len(items)
	effectiveMax := maxK
	if effectiveMax <= 0 {
		effectiveMax = n
	}

	if n <= 8 {
		return n
	}

	uniqueCount := countUniqueSimhash(items, 3)
	if uniqueCount <= 3 {
		k := maxInt(minK, uniqueCount)
		return minInt(k, effectiveMax)
	}

	curve := computeUniqueBigramCurve(items)
	knee, hasKnee := findKnee(curve)

	diversityRatio := float64(uniqueCount) / float64(n)

	var kneeVal int
	switch {
	case !hasKnee:
		keepFraction := 0.3 + 0.7*diversityRatio
		kneeVal = maxInt(minK, int(float64(n)*keepFraction))
	case diversityRatio > 0.7:
		floor := maxInt(minK, int(float64(n)*(0.3+0.7*diversityRatio)))
		kneeVal = maxInt(knee, floor)
	default:
		kneeVal = knee
	}

	k := maxInt(minK, int(float64(kneeVal)*bias))
	k = minInt(k, effectiveMax)

	k = validateWithZlib(items, k, effectiveMax, 0.15)

	return maxInt(minK, minInt(k, effectiveMax))
}

// findKnee finds the knee in a monotonically-increasing curve (Kneedle).
// Returns the 1-indexed count and true, or (0, false) when no clear knee.
func findKnee(curve []int) (int, bool) {
	n := len(curve)
	if n < 3 {
		return 0, false
	}
	yMin := float64(curve[0])
	yMax := float64(curve[n-1])
	if yMax == yMin {
		// Flat curve: all items identical. Headroom returns literal 1.
		return 1, true
	}
	xRange := float64(n - 1)
	yRange := yMax - yMin

	maxDiff := -1.0
	kneeIdx := -1
	for i, y := range curve {
		xNorm := float64(i) / xRange
		yNorm := (float64(y) - yMin) / yRange
		diff := yNorm - xNorm
		if diff > maxDiff {
			maxDiff = diff
			kneeIdx = i
		}
	}
	if maxDiff < 0.05 {
		return 0, false
	}
	return kneeIdx + 1, true
}

// computeUniqueBigramCurve returns the cumulative count of unique word-level
// bigrams after seeing items[0..=k]. Single-word and empty items contribute
// one synthetic (word, "") bigram.
func computeUniqueBigramCurve(items []string) []int {
	type bigram struct{ a, b string }
	seen := make(map[bigram]struct{})
	curve := make([]int, 0, len(items))
	for _, item := range items {
		words := strings.Fields(strings.ToLower(item))
		if len(words) < 2 {
			first := ""
			if len(words) == 1 {
				first = words[0]
			}
			seen[bigram{first, ""}] = struct{}{}
		} else {
			for j := 0; j < len(words)-1; j++ {
				seen[bigram{words[j], words[j+1]}] = struct{}{}
			}
		}
		curve = append(curve, len(seen))
	}
	return curve
}

// simhash returns a 64-bit SimHash fingerprint. Iterates character 4-grams
// (sliding window; a single iteration for inputs of <=3 chars), hashes each
// gram with 64-bit FNV-1a, votes per bit, and sets bit j iff its vote is
// strictly positive. Headroom hashed grams with MD5; FNV-1a is the right
// stdlib non-cryptographic choice for a similarity fingerprint (no security
// property is needed here) and preserves SimHash's behavior — determinism,
// lowercasing, bit-voting, and near-duplicate clustering.
func simhash(text string) uint64 {
	chars := []rune(strings.ToLower(text))
	n := len(chars)
	iterCount := 1
	if n > 3 {
		iterCount = n - 3
	}
	var votes [64]int
	for i := 0; i < iterCount; i++ {
		end := i + 4
		if end > n {
			end = n
		}
		gram := string(chars[i:end])
		h := fnv64a(gram)
		for j := uint(0); j < 64; j++ {
			if (h>>j)&1 == 1 {
				votes[j]++
			} else {
				votes[j]--
			}
		}
	}
	var fp uint64
	for j := uint(0); j < 64; j++ {
		if votes[j] > 0 {
			fp |= 1 << j
		}
	}
	return fp
}

func fnv64a(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// countUniqueSimhash counts items with distinct content via SimHash plus
// greedy clustering: two items cluster when their fingerprints are within
// threshold Hamming distance.
func countUniqueSimhash(items []string, threshold int) int {
	if len(items) == 0 {
		return 0
	}
	var clusters []uint64
	for _, s := range items {
		fp := simhash(s)
		matched := false
		for _, rep := range clusters {
			if bits.OnesCount64(fp^rep) <= threshold {
				matched = true
				break
			}
		}
		if !matched {
			clusters = append(clusters, fp)
		}
	}
	return len(clusters)
}

// validateWithZlib bumps k by 20% when the subset items[..k] compresses much
// better per byte than the full set (a sign the subset is missing diversity).
func validateWithZlib(items []string, k, maxK int, tolerance float64) int {
	if k >= len(items) || k >= maxK {
		return k
	}
	fullText := strings.Join(items, "\n")
	subsetText := strings.Join(items[:k], "\n")
	if len(fullText) < 200 {
		return k
	}
	fullCompressed := zlibLen([]byte(fullText))
	subsetCompressed := zlibLen([]byte(subsetText))

	fullRatio := 1.0
	if fullText != "" {
		fullRatio = float64(fullCompressed) / float64(len(fullText))
	}
	subsetRatio := 1.0
	if subsetText != "" {
		subsetRatio = float64(subsetCompressed) / float64(len(subsetText))
	}
	ratioDiff := fullRatio - subsetRatio
	if ratioDiff < 0 {
		ratioDiff = -ratioDiff
	}
	if ratioDiff > tolerance {
		adjusted := int(float64(k) * 1.2)
		return minInt(adjusted, maxK)
	}
	return k
}

// zlibLen returns the length of b compressed with zlib at best-speed (the
// stdlib analogue of Python's zlib level=1). Writes to an in-memory buffer.
func zlibLen(b []byte) int {
	var buf bytes.Buffer
	w, err := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	if err != nil {
		// BestSpeed is always a valid level; fall back to raw length.
		return len(b)
	}
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Len()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
