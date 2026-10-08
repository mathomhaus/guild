package compression

import "strings"

// Line-level classification helpers for the log compressor: level detection
// (word-boundary aware), summary-line detection, and the stack-trace state
// machine. Ported from log_compressor.rs (the LevelClassifier, is_summary_line,
// and StackTraceDetector).

type levelEntry struct {
	level logLevel
	words []string
}

// levelTable is ordered: ERROR before FAIL before WARN, longest-pattern-wins
// is handled by checking the longest matching keyword per scan.
var levelTable = []levelEntry{
	{lvlError, []string{"ERROR", "error", "Error", "FATAL", "fatal", "Fatal", "CRITICAL", "critical"}},
	{lvlFail, []string{"FAILED", "failed", "Failed", "FAIL", "fail", "Fail"}},
	{lvlWarn, []string{"WARNING", "warning", "Warning", "WARN", "warn", "Warn"}},
	{lvlInfo, []string{"INFO", "info", "Info"}},
	{lvlDebug, []string{"DEBUG", "debug", "Debug"}},
	{lvlTrace, []string{"TRACE", "trace", "Trace"}},
}

// classifyLevel returns the line's level, using a leftmost word-boundary
// match. Patterns are tried in table order (ERROR first) and longest-first
// within a level so "warning" wins over "warn".
func classifyLevel(line string) logLevel {
	bestPos := -1
	best := lvlUnknown
	for _, entry := range levelTable {
		for _, w := range entry.words {
			idx := indexWordBoundary(line, w)
			if idx < 0 {
				continue
			}
			if bestPos < 0 || idx < bestPos {
				bestPos = idx
				best = entry.level
			}
			// Only need the earliest hit for this level; table order then
			// breaks ties (ERROR before FAIL) because we keep the strictly
			// smaller position and earlier levels are scanned first.
			break
		}
	}
	return best
}

// indexWordBoundary returns the index of the first occurrence of word in line
// that sits on word boundaries, or -1.
func indexWordBoundary(line, word string) int {
	from := 0
	for {
		rel := strings.Index(line[from:], word)
		if rel < 0 {
			return -1
		}
		start := from + rel
		end := start + len(word)
		if isWordBoundary(line, start, end) {
			return start
		}
		from = start + 1
		if from >= len(line) {
			return -1
		}
	}
}

func isWordBoundary(s string, start, end int) bool {
	leftOK := start == 0 || !isWordByte(s[start-1])
	rightOK := end == len(s) || !isWordByte(s[end])
	return leftOK && rightOK
}

func isWordByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}

// isSummaryLine reports whether a line is a test/build summary line.
func isSummaryLine(line string) bool {
	if strings.HasPrefix(line, "===") || strings.HasPrefix(line, "---") {
		return true
	}
	leadingDigits := 0
	for leadingDigits < len(line) && line[leadingDigits] >= '0' && line[leadingDigits] <= '9' {
		leadingDigits++
	}
	if leadingDigits > 0 && leadingDigits < len(line) && line[leadingDigits] == ' ' {
		rest := line[leadingDigits+1:]
		for _, kw := range []string{"passed", "failed", "skipped", "error", "warning"} {
			if strings.HasPrefix(rest, kw) {
				return true
			}
		}
	}
	for _, prefix := range []string{"Test ", "Tests ", "Tests:", "Test:", "Suite ", "Suites ", "Suites:", "Suite:"} {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			trimmed := strings.TrimLeft(rest, " \t")
			return trimmed != "" && trimmed[0] >= '0' && trimmed[0] <= '9'
		}
	}
	for _, prefix := range []string{"TOTAL", "Total", "Summary"} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	for _, prefix := range []string{"Build", "Compile", "Test"} {
		if strings.HasPrefix(line, prefix) {
			for _, outcome := range []string{"succeeded", "failed", "complete"} {
				if strings.Contains(line, outcome) {
					return true
				}
			}
		}
	}
	return false
}

type traceFlavor int

const (
	flavorNone traceFlavor = iota
	flavorPythonTraceback
	flavorJS
	flavorJava
	flavorRustError
	flavorGo
)

// flavorFor returns the stack-trace flavor a line opens, or flavorNone.
func flavorFor(line string) traceFlavor {
	trimmed := strings.TrimLeft(line, " \t")
	switch {
	case strings.HasPrefix(trimmed, "Traceback (most recent call last)") || isPythonFileFrame(trimmed):
		return flavorPythonTraceback
	case isJSAtFrame(trimmed):
		return flavorJS
	case isJavaAtFrame(trimmed):
		return flavorJava
	case strings.HasPrefix(trimmed, "--> ") && hasLineColSuffix(trimmed):
		return flavorRustError
	case isGoFrame(line):
		return flavorGo
	default:
		return flavorNone
	}
}

func isPythonFileFrame(s string) bool {
	return strings.HasPrefix(s, `File "`) && strings.Contains(s, `", line `) &&
		s != "" && s[len(s)-1] >= '0' && s[len(s)-1] <= '9'
}

func isJSAtFrame(s string) bool {
	return strings.HasPrefix(s, "at ") && strings.Contains(s, "(") && strings.Contains(s, ")") && hasLineColSuffix(s)
}

func isJavaAtFrame(s string) bool {
	if !strings.HasPrefix(s, "at ") || !strings.Contains(s, "(") {
		return false
	}
	paren := strings.Index(s, "(")
	body := s[3:paren]
	if body == "" {
		return false
	}
	for _, c := range body {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == '$') {
			return false
		}
	}
	return true
}

func hasLineColSuffix(s string) bool {
	b := []byte(s)
	for i := 0; i+2 < len(b); i++ {
		if b[i] == ':' && b[i+1] >= '0' && b[i+1] <= '9' {
			j := i + 1
			for j < len(b) && b[j] >= '0' && b[j] <= '9' {
				j++
			}
			if j < len(b) && b[j] == ':' && j+1 < len(b) && b[j+1] >= '0' && b[j+1] <= '9' {
				return true
			}
		}
	}
	return false
}

func isGoFrame(s string) bool {
	trimmed := strings.TrimLeft(s, " \t")
	i := 0
	for i < len(trimmed) && trimmed[i] >= '0' && trimmed[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(trimmed) || trimmed[i] != ':' {
		return false
	}
	i++
	for i < len(trimmed) && trimmed[i] == ' ' {
		i++
	}
	rest := trimmed[i:]
	if !strings.HasPrefix(rest, "0x") {
		return false
	}
	for _, c := range rest[2:] {
		return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
	}
	return false
}

// traceTerminates reports whether line ends the current trace flavor's run.
func traceTerminates(flavor traceFlavor, line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	switch flavor {
	case flavorPythonTraceback:
		isIndentedOrBlank := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || line == ""
		isContinuation := strings.HasPrefix(trimmed, "Traceback") ||
			strings.HasPrefix(trimmed, "File ") ||
			strings.HasPrefix(trimmed, "During handling") ||
			strings.HasPrefix(trimmed, "The above exception")
		if isIndentedOrBlank || isContinuation {
			return false
		}
		return !startsUpper(trimmed)
	case flavorJS, flavorJava:
		return !strings.HasPrefix(trimmed, "at ") && line != ""
	case flavorRustError:
		return !strings.HasPrefix(trimmed, "--> ") && line != ""
	case flavorGo:
		return !(trimmed != "" && trimmed[0] >= '0' && trimmed[0] <= '9') && line != ""
	default:
		return true
	}
}

func startsUpper(s string) bool {
	for _, r := range s {
		return r >= 'A' && r <= 'Z'
	}
	return false
}
