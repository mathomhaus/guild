package compression

import (
	"fmt"
	"sort"
	"strings"
)

// Search-results compressor. Port of Headroom's search_compressor.rs.
// Compresses grep/ripgrep/ag output: parses into {file: [(line, content)]},
// scores each match on relevance, caps files, applies an adaptive total, runs
// per-file first/last + score-fill selection, formats file:line:content with
// "[... and N more matches in file]" summaries, and emits a CCR marker when
// thresholds clear. LOSSY-WITH-CCR: the full original is recoverable via
// retrieve(hash). The hand-rolled parser handles Windows drive paths and
// filenames containing '-', matching Headroom's parser fixes.

// SearchConfig mirrors Headroom's SearchCompressorConfig defaults.
type SearchConfig struct {
	MaxMatchesPerFile         int
	AlwaysKeepFirst           bool
	AlwaysKeepLast            bool
	MaxTotalMatches           int
	MaxFiles                  int
	ContextKeywords           []string
	BoostErrors               bool
	MinMatchesForCCR          int
	MinCompressionRatioForCCR float64
}

// DefaultSearchConfig returns the Headroom defaults.
func DefaultSearchConfig() SearchConfig {
	return SearchConfig{
		MaxMatchesPerFile:         5,
		AlwaysKeepFirst:           true,
		AlwaysKeepLast:            true,
		MaxTotalMatches:           30,
		MaxFiles:                  15,
		BoostErrors:               true,
		MinMatchesForCCR:          10,
		MinCompressionRatioForCCR: 0.8,
	}
}

type searchMatch struct {
	file       string
	lineNumber uint64
	content    string
	score      float64
}

type fileMatches struct {
	file    string
	matches []*searchMatch
}

func (fm *fileMatches) totalScore() float64 {
	var s float64
	for _, m := range fm.matches {
		s += m.score
	}
	return s
}

// SearchResult is the search compressor's structured output.
type SearchResult struct {
	Compressed           string
	OriginalMatchCount   int
	CompressedMatchCount int
	FilesAffected        int
	CompressionRatio     float64
	CacheKey             string
}

type searchStrategy struct{ c *SearchCompressor }

func init() {
	RegisterStrategy("search", func() Strategy { return searchStrategy{c: NewSearchCompressor(DefaultSearchConfig())} })
}

func (searchStrategy) Name() string   { return "search" }
func (searchStrategy) Lossless() bool { return false }

func (s searchStrategy) Compress(content, context string, store Store) (Result, error) {
	r := s.c.Compress(content, context, 0, store)
	return Result{
		Compressed:      r.Compressed,
		Lossless:        false,
		CacheKey:        r.CacheKey,
		OriginalBytes:   len(content),
		CompressedBytes: len(r.Compressed),
	}, nil
}

// SearchCompressor holds the search config.
type SearchCompressor struct{ config SearchConfig }

// NewSearchCompressor constructs a compressor with the given config.
func NewSearchCompressor(config SearchConfig) *SearchCompressor {
	return &SearchCompressor{config: config}
}

// Compress compresses grep-style content. context is an optional query string
// for relevance scoring; bias tunes the adaptive cap. When store is non-nil
// and a CCR marker is emitted, the full original is stashed under the key.
func (sc *SearchCompressor) Compress(content, context string, bias float64, store Store) SearchResult {
	parsed := sc.parse(content)
	if len(parsed) == 0 {
		return SearchResult{
			Compressed:       content,
			CompressionRatio: 1.0,
		}
	}

	originalCount := 0
	for _, fm := range parsed {
		originalCount += len(fm.matches)
	}

	sc.score(parsed, context)
	selected := sc.selectMatches(parsed, bias)

	compressed, _ := sc.formatOutput(selected, parsed)
	compressedCount := 0
	for _, fm := range selected {
		compressedCount += len(fm.matches)
	}

	denom := len(content)
	if denom == 0 {
		denom = 1
	}
	ratio := float64(len(compressed)) / float64(denom)

	var cacheKey string
	if originalCount >= sc.config.MinMatchesForCCR && ratio < sc.config.MinCompressionRatioForCCR {
		key := ccrKey(content)
		compressed += fmt.Sprintf("\n[%d matches compressed to %d. Retrieve more: hash=%s]",
			originalCount, compressedCount, key)
		if store != nil {
			store.Put(key, content)
		}
		cacheKey = key
	}

	return SearchResult{
		Compressed:           compressed,
		OriginalMatchCount:   originalCount,
		CompressedMatchCount: compressedCount,
		FilesAffected:        len(parsed),
		CompressionRatio:     ratio,
		CacheKey:             cacheKey,
	}
}

// parse returns matches grouped by file, in file insertion order preserved
// via a sorted key set for determinism.
func (sc *SearchCompressor) parse(content string) map[string]*fileMatches {
	out := make(map[string]*fileMatches)
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		file, lineNo, body, ok := parseMatchLine(line)
		if !ok {
			continue
		}
		fm, exists := out[file]
		if !exists {
			fm = &fileMatches{file: file}
			out[file] = fm
		}
		fm.matches = append(fm.matches, &searchMatch{file: file, lineNumber: lineNo, content: body})
	}
	return out
}

func (sc *SearchCompressor) score(files map[string]*fileMatches, context string) {
	contextLower := strings.ToLower(context)
	var contextWords []string
	for _, w := range strings.Fields(contextLower) {
		if len(w) > 2 {
			contextWords = append(contextWords, w)
		}
	}
	for _, fm := range files {
		for _, m := range fm.matches {
			var score float64
			contentLower := strings.ToLower(m.content)
			for _, w := range contextWords {
				if strings.Contains(contentLower, w) {
					score += 0.3
				}
			}
			if sc.config.BoostErrors {
				score += searchErrorBoost(contentLower)
			}
			for _, kw := range sc.config.ContextKeywords {
				if strings.Contains(contentLower, strings.ToLower(kw)) {
					score += 0.4
				}
			}
			if score > 1.0 {
				score = 1.0
			}
			m.score = score
		}
	}
}

// searchErrorBoost maps priority signals to the same boosts Headroom's
// importance detector produces for the Search context (error 0.5, warning
// 0.4, importance 0.3). Only the strongest match boosts (one per line).
func searchErrorBoost(contentLower string) float64 {
	for _, pat := range diffPriorityPatterns[:1] { // error/exception/fail/...
		if pat.MatchString(contentLower) {
			return 0.5
		}
	}
	if strings.Contains(contentLower, "warn") {
		return 0.4
	}
	for _, pat := range diffPriorityPatterns[1:2] { // important/note/todo/...
		if pat.MatchString(contentLower) {
			return 0.3
		}
	}
	return 0.0
}

func (sc *SearchCompressor) selectMatches(files map[string]*fileMatches, bias float64) map[string]*fileMatches {
	byScore := make([]*fileMatches, 0, len(files))
	for _, fm := range files {
		byScore = append(byScore, fm)
	}
	// Sort by total score desc, file name asc for determinism on ties.
	sort.SliceStable(byScore, func(i, j int) bool {
		si, sj := byScore[i].totalScore(), byScore[j].totalScore()
		if si != sj {
			return si > sj
		}
		return byScore[i].file < byScore[j].file
	})
	if len(byScore) > sc.config.MaxFiles {
		byScore = byScore[:sc.config.MaxFiles]
	}

	var allStrings []string
	for _, fm := range byScore {
		for _, m := range fm.matches {
			allStrings = append(allStrings, fmt.Sprintf("%s:%d:%s", fm.file, m.lineNumber, m.content))
		}
	}
	adaptiveTotal := ComputeOptimalK(allStrings, bias, 5, sc.config.MaxTotalMatches)

	selected := make(map[string]*fileMatches)
	totalSelected := 0

	for _, fm := range byScore {
		if totalSelected >= adaptiveTotal {
			continue
		}
		sorted := make([]*searchMatch, len(fm.matches))
		copy(sorted, fm.matches)
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].score != sorted[j].score {
				return sorted[i].score > sorted[j].score
			}
			return sorted[i].lineNumber < sorted[j].lineNumber
		})

		var fileSelected []*searchMatch
		seen := make(map[uint64]struct{})
		remainingCap := sc.config.MaxMatchesPerFile
		if rem := adaptiveTotal - totalSelected; rem < remainingCap {
			remainingCap = rem
		}
		pushUnique := func(m *searchMatch) {
			key := m.lineNumber ^ hashStr(m.content)
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				fileSelected = append(fileSelected, m)
			}
		}
		if sc.config.AlwaysKeepFirst && len(fm.matches) > 0 && len(fileSelected) < remainingCap {
			pushUnique(fm.matches[0])
		}
		if sc.config.AlwaysKeepLast && len(fm.matches) > 1 && len(fileSelected) < remainingCap {
			pushUnique(fm.matches[len(fm.matches)-1])
		}
		for _, m := range sorted {
			if len(fileSelected) >= remainingCap {
				break
			}
			pushUnique(m)
		}
		sort.SliceStable(fileSelected, func(i, j int) bool { return fileSelected[i].lineNumber < fileSelected[j].lineNumber })

		totalSelected += len(fileSelected)
		selected[fm.file] = &fileMatches{file: fm.file, matches: fileSelected}
	}
	return selected
}

func (sc *SearchCompressor) formatOutput(selected, original map[string]*fileMatches) (rendered string, summaries map[string]string) {
	files := make([]string, 0, len(selected))
	for f := range selected {
		files = append(files, f)
	}
	sort.Strings(files)

	var lines []string
	summaries = make(map[string]string)
	for _, file := range files {
		fm := selected[file]
		for _, m := range fm.matches {
			lines = append(lines, fmt.Sprintf("%s:%d:%s", m.file, m.lineNumber, m.content))
		}
		if orig, ok := original[file]; ok && len(orig.matches) > len(fm.matches) {
			omitted := len(orig.matches) - len(fm.matches)
			summary := fmt.Sprintf("[... and %d more matches in %s]", omitted, file)
			lines = append(lines, summary)
			summaries[file] = summary
		}
	}
	return strings.Join(lines, "\n"), summaries
}

// parseMatchLine parses one grep/ripgrep line into (file, lineNumber,
// content). Returns ok=false for lines that don't match the shape.
func parseMatchLine(line string) (file string, lineNumber uint64, body string, ok bool) {
	b := []byte(line)
	scanStart := 0
	if len(b) >= 3 && isAlpha(b[0]) && b[1] == ':' && (b[2] == '\\' || b[2] == '/') {
		scanStart = 2
	}
	i := scanStart
	for i < len(b) {
		if b[i] == ':' || b[i] == '-' {
			if i > 0 && (b[i-1] == ':' || b[i-1] == '-') {
				i++
				continue
			}
			digitsStart := i + 1
			j := digitsStart
			for j < len(b) && b[j] >= '0' && b[j] <= '9' {
				j++
			}
			if j > digitsStart && j < len(b) && (b[j] == ':' || b[j] == '-') {
				if i == 0 {
					return "", 0, "", false
				}
				var n uint64
				for k := digitsStart; k < j; k++ {
					n = n*10 + uint64(b[k]-'0')
				}
				return line[:i], n, line[j+1:], true
			}
		}
		i++
	}
	return "", 0, "", false
}

func isAlpha(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

// hashStr is a small FNV-1a hash for the per-file dedupe key.
func hashStr(s string) uint64 {
	const offset = 1469598103934665603
	const prime = 1099511628211
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}
