package compression

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Unified-diff compressor. Port of Headroom's diff_compressor.rs. Compresses
// verbose `git diff` output by parsing the unified-diff format, capping the
// file and per-file hunk counts (keeping first + last + top-scored middle),
// trimming context lines around each change, and (when compression saved
// >20% of lines) emitting a CCR retrieval marker keyed by a hash of the
// original. This is a LOSSY-WITH-CCR strategy: the inline output is a compact
// view; the full original is recoverable via retrieve(hash).
//
// Short diffs (below MinLinesForCCR) and non-diff input pass through
// unchanged, matching Headroom's information-preservation short-circuits.

// DiffConfig mirrors Headroom's DiffCompressorConfig defaults.
type DiffConfig struct {
	MaxContextLines           int
	MaxHunksPerFile           int
	MaxFiles                  int
	MinLinesForCCR            int
	MinCompressionRatioForCCR float64
}

// DefaultDiffConfig returns the Headroom defaults.
func DefaultDiffConfig() DiffConfig {
	return DiffConfig{
		MaxContextLines:           2,
		MaxHunksPerFile:           10,
		MaxFiles:                  20,
		MinLinesForCCR:            50,
		MinCompressionRatioForCCR: 0.8,
	}
}

// DiffResult is the diff compressor's structured output.
type DiffResult struct {
	Compressed          string
	OriginalLineCount   int
	CompressedLineCount int
	FilesAffected       int
	Additions           int
	Deletions           int
	HunksKept           int
	HunksRemoved        int
	CacheKey            string // empty when no CCR marker was emitted
}

// diffStrategy adapts DiffCompressor to the Strategy interface.
type diffStrategy struct{ c *DiffCompressor }

func init() {
	RegisterStrategy("diff", func() Strategy { return diffStrategy{c: NewDiffCompressor(DefaultDiffConfig())} })
}

func (diffStrategy) Name() string   { return "diff" }
func (diffStrategy) Lossless() bool { return false }

func (s diffStrategy) Compress(content, context string, store Store) (Result, error) {
	r := s.c.Compress(content, context, store)
	return Result{
		Compressed:      r.Compressed,
		Lossless:        false,
		CacheKey:        r.CacheKey,
		OriginalBytes:   len(content),
		CompressedBytes: len(r.Compressed),
	}, nil
}

// DiffCompressor holds the diff config. Cheap to construct.
type DiffCompressor struct{ config DiffConfig }

// NewDiffCompressor constructs a compressor with the given config.
func NewDiffCompressor(config DiffConfig) *DiffCompressor { return &DiffCompressor{config: config} }

// Compress compresses content. context is an optional query string used for
// relevance scoring when the per-file hunk cap fires. When store is non-nil
// and a CCR marker is emitted, the full original is stashed under the marker
// key so retrieve(hash) reproduces it.
func (d *DiffCompressor) Compress(content, context string, store Store) DiffResult {
	lines := strings.Split(content, "\n")
	originalLineCount := len(lines)

	if originalLineCount < d.config.MinLinesForCCR {
		return passThroughDiff(content, originalLineCount)
	}

	preDiff, files := parseDiff(lines)
	if len(files) == 0 {
		return passThroughDiff(content, originalLineCount)
	}

	scoreHunks(files, context)

	// File cap: keep the heaviest files by total change count.
	if len(files) > d.config.MaxFiles {
		sort.SliceStable(files, func(i, j int) bool {
			return files[i].totalChanges() > files[j].totalChanges()
		})
		files = files[:d.config.MaxFiles]
	}

	var compressedFiles []*diffFile
	var totalAdd, totalDel, hunksKept, hunksRemoved int
	for _, f := range files {
		totalAdd += f.totalAdditions()
		totalDel += f.totalDeletions()
		originalHunkCount := len(f.hunks)

		selected := selectHunks(f.hunks, d.config.MaxHunksPerFile)

		var trimmed []*diffHunk
		for _, h := range selected {
			trimmed = append(trimmed, reduceContext(h, d.config.MaxContextLines))
		}
		hunksKept += len(trimmed)
		hunksRemoved += originalHunkCount - len(trimmed)

		nf := *f
		nf.hunks = trimmed
		compressedFiles = append(compressedFiles, &nf)
	}

	filesAffected := len(compressedFiles)
	compressed := formatDiffOutput(preDiff, compressedFiles, filesAffected, totalAdd, totalDel, hunksRemoved)
	compressedLineCount := len(strings.Split(compressed, "\n"))

	var cacheKey string
	if float64(compressedLineCount) < float64(originalLineCount)*d.config.MinCompressionRatioForCCR {
		key := ccrKey(content)
		compressed += fmt.Sprintf("\n[%d lines compressed to %d. Retrieve full diff: hash=%s]",
			originalLineCount, compressedLineCount, key)
		if store != nil {
			store.Put(key, content)
		}
		cacheKey = key
	}

	return DiffResult{
		Compressed:          compressed,
		OriginalLineCount:   originalLineCount,
		CompressedLineCount: compressedLineCount,
		FilesAffected:       filesAffected,
		Additions:           totalAdd,
		Deletions:           totalDel,
		HunksKept:           hunksKept,
		HunksRemoved:        hunksRemoved,
		CacheKey:            cacheKey,
	}
}

type diffHunk struct {
	header    string
	lines     []string
	additions int
	deletions int
	score     float64
}

type diffFile struct {
	header        string
	oldFile       string
	newFile       string
	hunks         []*diffHunk
	isBinary      bool
	isNewFile     bool
	isDeletedFile bool
	renameLines   []string
}

func (f *diffFile) totalAdditions() int {
	n := 0
	for _, h := range f.hunks {
		n += h.additions
	}
	return n
}

func (f *diffFile) totalDeletions() int {
	n := 0
	for _, h := range f.hunks {
		n += h.deletions
	}
	return n
}

func (f *diffFile) totalChanges() int { return f.totalAdditions() + f.totalDeletions() }

var (
	diffGitRE      = regexp.MustCompile(`^diff --git a/(.+) b/(.+)$`)
	diffCombinedRE = regexp.MustCompile(`^diff --combined (.+)$`)
	diffCcRE       = regexp.MustCompile(`^diff --cc (.+)$`)
	oldFileRE      = regexp.MustCompile(`^--- (a/(.+)|/dev/null)$`)
	newFileRE      = regexp.MustCompile(`^\+{3} (b/(.+)|/dev/null)$`)
	binaryRE       = regexp.MustCompile(`^Binary files .+ differ$`)
	hunkHeaderRE   = regexp.MustCompile(
		`^(?:@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@` +
			`|@@@ -\d+(?:,\d+)? -\d+(?:,\d+)? \+\d+(?:,\d+)? @@@` +
			`|@@@@ -\d+(?:,\d+)? -\d+(?:,\d+)? -\d+(?:,\d+)? \+\d+(?:,\d+)? @@@@)(.*)$`)
	hunkNewRangeRE = regexp.MustCompile(`\+(\d+)`)
)

func isDiffHeader(line string) bool {
	return diffGitRE.MatchString(line) || diffCombinedRE.MatchString(line) || diffCcRE.MatchString(line)
}

// parseDiff parses the unified diff into pre-diff content (commit/email
// headers re-emitted verbatim) plus file structures.
func parseDiff(lines []string) (preDiff []string, files []*diffFile) {
	var currentFile *diffFile
	var currentHunk *diffHunk

	flushHunk := func() {
		if currentHunk != nil && currentFile != nil {
			currentFile.hunks = append(currentFile.hunks, currentHunk)
		}
		currentHunk = nil
	}
	flushFile := func() {
		if currentFile != nil {
			files = append(files, currentFile)
		}
		currentFile = nil
	}

	for _, line := range lines {
		if isDiffHeader(line) {
			flushHunk()
			flushFile()
			currentFile = &diffFile{header: line}
			continue
		}
		if currentFile == nil {
			preDiff = append(preDiff, line)
			continue
		}

		switch {
		case strings.HasPrefix(line, "new file mode"):
			currentFile.isNewFile = true
		case strings.HasPrefix(line, "deleted file mode"):
			currentFile.isDeletedFile = true
		case strings.HasPrefix(line, "rename "), strings.HasPrefix(line, "similarity "),
			strings.HasPrefix(line, "copy "), strings.HasPrefix(line, "dissimilarity "):
			currentFile.renameLines = append(currentFile.renameLines, line)
		case binaryRE.MatchString(line):
			currentFile.isBinary = true
		}

		if oldFileRE.MatchString(line) {
			currentFile.oldFile = line
			continue
		}
		if newFileRE.MatchString(line) {
			currentFile.newFile = line
			continue
		}
		if hunkHeaderRE.MatchString(line) {
			flushHunk()
			currentHunk = &diffHunk{header: line}
			continue
		}
		if currentHunk != nil {
			switch {
			case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
				currentHunk.additions++
				currentHunk.lines = append(currentHunk.lines, line)
			case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
				currentHunk.deletions++
				currentHunk.lines = append(currentHunk.lines, line)
			default:
				currentHunk.lines = append(currentHunk.lines, line)
			}
		}
	}
	flushHunk()
	flushFile()
	return preDiff, files
}

var diffPriorityPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(error|exception|fail(?:ed|ure)?|fatal|critical|crash|panic)\b`),
	regexp.MustCompile(`(?i)\b(important|note|todo|fixme|hack|xxx|bug|fix)\b`),
	regexp.MustCompile(`(?i)\b(security|auth|password|secret|token)\b`),
}

func scoreHunks(files []*diffFile, context string) {
	contextWords := strings.Fields(strings.ToLower(context))
	for _, f := range files {
		for _, h := range f.hunks {
			score := float64(h.additions+h.deletions) * 0.03
			if score > 0.3 {
				score = 0.3
			}
			contentLower := strings.ToLower(strings.Join(h.lines, "\n"))
			for _, w := range contextWords {
				if len(w) > 2 && strings.Contains(contentLower, w) {
					score += 0.2
				}
			}
			for _, pat := range diffPriorityPatterns {
				if pat.MatchString(contentLower) {
					score += 0.3
					break
				}
			}
			if score > 1.0 {
				score = 1.0
			}
			h.score = score
		}
	}
}

// selectHunks caps the per-file hunk count, keeping first + last + top-scored
// middle, then restoring appearance order by hunk start line.
func selectHunks(hunks []*diffHunk, maxPerFile int) []*diffHunk {
	if len(hunks) <= maxPerFile {
		return hunks
	}
	first := hunks[0]
	var last *diffHunk
	middle := hunks[1:]
	if len(middle) > 0 {
		last = middle[len(middle)-1]
		middle = middle[:len(middle)-1]
	}

	remainingSlots := maxPerFile - 1
	if last != nil {
		remainingSlots = maxPerFile - 2
	}
	if remainingSlots < 0 {
		remainingSlots = 0
	}

	middleSorted := make([]*diffHunk, len(middle))
	copy(middleSorted, middle)
	sort.SliceStable(middleSorted, func(i, j int) bool {
		return middleSorted[i].score > middleSorted[j].score
	})
	keptMiddle := middleSorted
	if len(keptMiddle) > remainingSlots {
		keptMiddle = keptMiddle[:remainingSlots]
	}

	selected := []*diffHunk{first}
	selected = append(selected, keptMiddle...)
	if last != nil {
		selected = append(selected, last)
	}
	sort.SliceStable(selected, func(i, j int) bool {
		return extractLineNumber(selected[i].header) < extractLineNumber(selected[j].header)
	})
	return selected
}

func extractLineNumber(header string) int {
	if m := hunkNewRangeRE.FindStringSubmatch(header); m != nil {
		n := 0
		for _, c := range m[1] {
			n = n*10 + int(c-'0')
		}
		return n
	}
	return 0
}

// reduceContext keeps each change line plus maxContext lines on either side,
// always preserving backslash-prefixed patch markers.
func reduceContext(hunk *diffHunk, maxContext int) *diffHunk {
	var changePositions []int
	for i, l := range hunk.lines {
		if strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") {
			changePositions = append(changePositions, i)
		}
	}

	if len(changePositions) == 0 {
		take := maxContext
		if take > len(hunk.lines) {
			take = len(hunk.lines)
		}
		return &diffHunk{header: hunk.header, lines: append([]string(nil), hunk.lines[:take]...), score: hunk.score}
	}

	keep := make(map[int]struct{})
	for _, pos := range changePositions {
		keep[pos] = struct{}{}
		lo := pos - maxContext
		if lo < 0 {
			lo = 0
		}
		for i := lo; i < pos; i++ {
			keep[i] = struct{}{}
		}
		hi := pos + maxContext + 1
		if hi > len(hunk.lines) {
			hi = len(hunk.lines)
		}
		for i := pos + 1; i < hi; i++ {
			keep[i] = struct{}{}
		}
	}
	for i, line := range hunk.lines {
		if strings.HasPrefix(line, "\\") {
			keep[i] = struct{}{}
		}
	}

	idxs := make([]int, 0, len(keep))
	for i := range keep {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)

	var newLines []string
	var add, del int
	for _, i := range idxs {
		line := hunk.lines[i]
		newLines = append(newLines, line)
		switch {
		case strings.HasPrefix(line, "+"):
			add++
		case strings.HasPrefix(line, "-"):
			del++
		}
	}
	return &diffHunk{header: hunk.header, lines: newLines, additions: add, deletions: del, score: hunk.score}
}

func formatDiffOutput(preDiff []string, files []*diffFile, filesAffected, totalAdd, totalDel, hunksRemoved int) string {
	var out []string
	out = append(out, preDiff...)

	for _, f := range files {
		out = append(out, f.header)
		out = append(out, f.renameLines...)
		switch {
		case f.isNewFile:
			out = append(out, "new file mode 100644")
		case f.isDeletedFile:
			out = append(out, "deleted file mode 100644")
		}
		if f.isBinary {
			out = append(out, "Binary files differ")
			continue
		}
		if f.oldFile != "" {
			out = append(out, f.oldFile)
		}
		if f.newFile != "" {
			out = append(out, f.newFile)
		}
		for _, h := range f.hunks {
			out = append(out, h.header)
			out = append(out, h.lines...)
		}
	}

	if hunksRemoved > 0 || filesAffected > 0 {
		parts := []string{
			fmt.Sprintf("%d files changed", filesAffected),
			fmt.Sprintf("+%d -%d lines", totalAdd, totalDel),
		}
		if hunksRemoved > 0 {
			parts = append(parts, fmt.Sprintf("%d hunks omitted", hunksRemoved))
		}
		out = append(out, "["+strings.Join(parts, ", ")+"]")
	}

	return strings.Join(out, "\n")
}

func passThroughDiff(content string, lineCount int) DiffResult {
	return DiffResult{
		Compressed:          content,
		OriginalLineCount:   lineCount,
		CompressedLineCount: lineCount,
	}
}

// ccrKey is the CCR cache key the diff/log/search compressors embed in their
// "hash=KEY" retrieval footers. It is ComputeKey (SHA-256[:24]), the same key
// the shared store is addressed by, so store.Put(key, original) and a later
// retrieve(key) stay consistent. Headroom's Python path used MD5[:24]; we use
// the store's canonical SHA-256[:24] since the algorithm is an internal
// content-address detail, not part of the marker GRAMMAR the agent reads.
func ccrKey(s string) string { return ComputeKey(s) }
