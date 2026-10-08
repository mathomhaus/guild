package compression

import (
	"fmt"
	"sort"
	"strings"
)

// Log/build-output compressor. Port of Headroom's log_compressor.rs.
// Compresses build and test output (pytest, npm, cargo, jest, make, generic):
// classifies each line's level, detects stack-trace and summary membership,
// scores lines, selects errors/fails/warnings (deduped)/stack traces/
// summaries with a context window, applies an adaptive total-lines cap, and
// emits a CCR marker when compression cleared the ratio threshold. LOSSY-
// WITH-CCR: the full original is recoverable via retrieve(hash).

// LogConfig mirrors Headroom's LogCompressorConfig defaults.
type LogConfig struct {
	MaxErrors                 int
	ErrorContextLines         int
	KeepFirstError            bool
	KeepLastError             bool
	MaxStackTraces            int
	StackTraceMaxLines        int
	MaxWarnings               int
	DedupeWarnings            bool
	KeepSummaryLines          bool
	MaxTotalLines             int
	MinLinesForCCR            int
	MinCompressionRatioForCCR float64
}

// DefaultLogConfig returns the Headroom defaults.
func DefaultLogConfig() LogConfig {
	return LogConfig{
		MaxErrors:                 10,
		ErrorContextLines:         3,
		KeepFirstError:            true,
		KeepLastError:             true,
		MaxStackTraces:            3,
		StackTraceMaxLines:        20,
		MaxWarnings:               5,
		DedupeWarnings:            true,
		KeepSummaryLines:          true,
		MaxTotalLines:             100,
		MinLinesForCCR:            50,
		MinCompressionRatioForCCR: 0.5,
	}
}

type logLevel int

const (
	lvlUnknown logLevel = iota
	lvlError
	lvlFail
	lvlWarn
	lvlInfo
	lvlDebug
	lvlTrace
)

type logLine struct {
	lineNumber   int
	content      string
	level        logLevel
	isStackTrace bool
	isSummary    bool
	score        float64
}

// LogResult is the log compressor's structured output.
type LogResult struct {
	Compressed          string
	OriginalLineCount   int
	CompressedLineCount int
	CompressionRatio    float64
	CacheKey            string
}

type logStrategy struct{ c *LogCompressor }

func init() {
	RegisterStrategy("log", func() Strategy { return logStrategy{c: NewLogCompressor(DefaultLogConfig())} })
}

func (logStrategy) Name() string   { return "log" }
func (logStrategy) Lossless() bool { return false }

func (s logStrategy) Compress(content, _ string, store Store) (Result, error) {
	r := s.c.Compress(content, 0, store)
	return Result{
		Compressed:      r.Compressed,
		Lossless:        false,
		CacheKey:        r.CacheKey,
		OriginalBytes:   len(content),
		CompressedBytes: len(r.Compressed),
	}, nil
}

// LogCompressor holds the log config.
type LogCompressor struct{ config LogConfig }

// NewLogCompressor constructs a compressor with the given config.
func NewLogCompressor(config LogConfig) *LogCompressor { return &LogCompressor{config: config} }

// Compress compresses content. bias tunes the adaptive cap (>1 keeps more).
// When store is non-nil and a CCR marker is emitted, the full original is
// stashed under the marker key.
func (lc *LogCompressor) Compress(content string, bias float64, store Store) LogResult {
	lines := strings.Split(content, "\n")
	originalLineCount := len(lines)

	if originalLineCount < lc.config.MinLinesForCCR {
		return LogResult{
			Compressed:          content,
			OriginalLineCount:   originalLineCount,
			CompressedLineCount: originalLineCount,
			CompressionRatio:    1.0,
		}
	}

	parsed := lc.parseLines(lines)
	selected := lc.selectLines(parsed, bias)
	compressed := lc.formatOutput(selected, parsed)

	denom := len(content)
	if denom == 0 {
		denom = 1
	}
	ratio := float64(len(compressed)) / float64(denom)

	var cacheKey string
	if ratio < lc.config.MinCompressionRatioForCCR {
		key := ccrKey(content)
		compressed += fmt.Sprintf("\n[%d lines compressed to %d. Retrieve more: hash=%s]",
			originalLineCount, len(selected), key)
		if store != nil {
			store.Put(key, content)
		}
		cacheKey = key
	}

	return LogResult{
		Compressed:          compressed,
		OriginalLineCount:   originalLineCount,
		CompressedLineCount: len(selected),
		CompressionRatio:    ratio,
		CacheKey:            cacheKey,
	}
}

func (lc *LogCompressor) parseLines(lines []string) []*logLine {
	out := make([]*logLine, 0, len(lines))
	var active traceFlavor = flavorNone
	traceLines := 0
	for i, line := range lines {
		e := &logLine{lineNumber: i, content: line}
		e.level = classifyLevel(line)
		e.isSummary = isSummaryLine(line)

		if active != flavorNone {
			if traceLines >= lc.config.StackTraceMaxLines || traceTerminates(active, line) {
				active = flavorNone
				traceLines = 0
				if nf := flavorFor(line); nf != flavorNone {
					active = nf
					traceLines = 1
					e.isStackTrace = true
				}
			} else {
				e.isStackTrace = true
				traceLines++
			}
		} else if nf := flavorFor(line); nf != flavorNone {
			active = nf
			traceLines = 1
			e.isStackTrace = true
		}

		e.score = scoreLogLine(e)
		out = append(out, e)
	}
	return out
}

func scoreLogLine(l *logLine) float64 {
	var score float64
	switch l.level {
	case lvlError, lvlFail:
		score = 1.0
	case lvlWarn:
		score = 0.6
	case lvlInfo:
		score = 0.3
	default:
		score = 0.1
	}
	if l.isStackTrace {
		score += 0.3
	}
	if l.isSummary {
		score += 0.4
	}
	if score > 1.0 {
		score = 1.0
	}
	return score
}

func (lc *LogCompressor) selectLines(logLines []*logLine, bias float64) []*logLine {
	allStrings := make([]string, len(logLines))
	for i, l := range logLines {
		allStrings[i] = l.content
	}
	adaptiveMax := ComputeOptimalK(allStrings, bias, 10, lc.config.MaxTotalLines)

	var errors, fails, warnings, summaries []*logLine
	var stackTraces [][]*logLine
	var current []*logLine
	for _, l := range logLines {
		switch l.level {
		case lvlError:
			errors = append(errors, l)
		case lvlFail:
			fails = append(fails, l)
		case lvlWarn:
			warnings = append(warnings, l)
		}
		if l.isStackTrace {
			current = append(current, l)
		} else if len(current) > 0 {
			stackTraces = append(stackTraces, current)
			current = nil
		}
		if l.isSummary {
			summaries = append(summaries, l)
		}
	}
	if len(current) > 0 {
		stackTraces = append(stackTraces, current)
	}

	selected := make(map[int]*logLine)
	add := func(l *logLine) { selected[l.lineNumber] = l }

	for _, l := range lc.selectWithFirstLast(errors, lc.config.MaxErrors) {
		add(l)
	}
	for _, l := range lc.selectWithFirstLast(fails, lc.config.MaxErrors) {
		add(l)
	}

	w := warnings
	if lc.config.DedupeWarnings {
		w = dedupeSimilar(w)
	}
	if len(w) > lc.config.MaxWarnings {
		w = w[:lc.config.MaxWarnings]
	}
	for _, l := range w {
		add(l)
	}

	for si, stack := range stackTraces {
		if si >= lc.config.MaxStackTraces {
			break
		}
		for li, l := range stack {
			if li >= lc.config.StackTraceMaxLines {
				break
			}
			add(l)
		}
	}

	if lc.config.KeepSummaryLines {
		for _, l := range summaries {
			add(l)
		}
	}

	// Context window around every selected line.
	selectedIdx := make([]int, 0, len(selected))
	for idx := range selected {
		selectedIdx = append(selectedIdx, idx)
	}
	contextIdx := make(map[int]struct{})
	for _, idx := range selectedIdx {
		lo := idx - lc.config.ErrorContextLines
		if lo < 0 {
			lo = 0
		}
		hi := idx + lc.config.ErrorContextLines + 1
		if hi > len(logLines) {
			hi = len(logLines)
		}
		for i := lo; i < hi; i++ {
			if i != idx {
				contextIdx[i] = struct{}{}
			}
		}
	}
	for idx := range contextIdx {
		if _, ok := selected[idx]; !ok && idx < len(logLines) {
			selected[idx] = logLines[idx]
		}
	}

	ordered := make([]*logLine, 0, len(selected))
	for _, l := range selected {
		ordered = append(ordered, l)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].lineNumber < ordered[j].lineNumber })

	if len(ordered) > adaptiveMax {
		byScore := make([]*logLine, len(ordered))
		copy(byScore, ordered)
		sort.SliceStable(byScore, func(i, j int) bool {
			if byScore[i].score != byScore[j].score {
				return byScore[i].score > byScore[j].score
			}
			return byScore[i].lineNumber < byScore[j].lineNumber
		})
		byScore = byScore[:adaptiveMax]
		sort.Slice(byScore, func(i, j int) bool { return byScore[i].lineNumber < byScore[j].lineNumber })
		ordered = byScore
	}
	return ordered
}

func (lc *LogCompressor) selectWithFirstLast(lines []*logLine, maxCount int) []*logLine {
	if len(lines) <= maxCount {
		return lines
	}
	var out []*logLine
	seen := make(map[int]struct{})
	push := func(l *logLine) {
		if _, ok := seen[l.lineNumber]; !ok {
			seen[l.lineNumber] = struct{}{}
			out = append(out, l)
		}
	}
	if lc.config.KeepFirstError {
		push(lines[0])
	}
	if lc.config.KeepLastError {
		push(lines[len(lines)-1])
	}
	remaining := maxCount - len(out)
	if remaining > 0 {
		byScore := make([]*logLine, len(lines))
		copy(byScore, lines)
		sort.SliceStable(byScore, func(i, j int) bool {
			if byScore[i].score != byScore[j].score {
				return byScore[i].score > byScore[j].score
			}
			return byScore[i].lineNumber < byScore[j].lineNumber
		})
		for _, l := range byScore {
			if _, ok := seen[l.lineNumber]; !ok {
				push(l)
				if len(out) >= maxCount {
					break
				}
			}
		}
	}
	return out
}

func dedupeSimilar(lines []*logLine) []*logLine {
	seen := make(map[string]struct{})
	var out []*logLine
	for _, l := range lines {
		key := normalizeForDedupe(l.content)
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			out = append(out, l)
		}
	}
	return out
}

// normalizeForDedupe preserves the message prefix (everything before the
// first ':' or '=') and tokenizes only the trailing variable region, so two
// distinct error categories don't accidentally merge. Matches Headroom's
// conservative dedupe fix.
func normalizeForDedupe(content string) string {
	cut := len(content)
	if i := strings.IndexAny(content, ":="); i >= 0 {
		cut = i
	}
	prefix := content[:cut]
	rest := content[cut:]
	var b strings.Builder
	prevTok := false
	for _, r := range rest {
		if r >= '0' && r <= '9' {
			if !prevTok {
				b.WriteByte('#')
				prevTok = true
			}
			continue
		}
		prevTok = false
		b.WriteRune(r)
	}
	return prefix + b.String()
}

func (lc *LogCompressor) formatOutput(selected, allLines []*logLine) string {
	counts := map[logLevel]int{}
	for _, l := range allLines {
		counts[l.level]++
	}
	out := make([]string, 0, len(selected)+1)
	for _, l := range selected {
		out = append(out, l.content)
	}
	omitted := len(allLines) - len(selected)
	if omitted > 0 {
		var parts []string
		for _, lc := range []struct {
			label string
			lvl   logLevel
		}{{"ERROR", lvlError}, {"FAIL", lvlFail}, {"WARN", lvlWarn}, {"INFO", lvlInfo}} {
			if n := counts[lc.lvl]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, lc.label))
			}
		}
		if len(parts) > 0 {
			out = append(out, fmt.Sprintf("[%d lines omitted: %s]", omitted, strings.Join(parts, ", ")))
		}
	}
	return strings.Join(out, "\n")
}
