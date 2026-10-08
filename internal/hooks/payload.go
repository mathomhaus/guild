// Package hooks renders the bounded stdout payloads that harness
// lifecycle hooks inject into agent context.
//
// # Stdout contract (load-bearing)
//
// Stdout is the harness's inject channel: every byte a hook command
// prints to stdout becomes part of the agent's next-turn context.
// The CLI verbs that render these payloads (`guild quest brief --auto`,
// `guild lore appraise --inject`) therefore obey a stricter contract
// than the rest of the CLI:
//
//   - exit 0 on success, with the payload as the only stdout bytes
//   - on error: exit non-zero AND emit empty stdout. Stack traces,
//     debug logging, and partial output go to stderr (or a log file),
//     never stdout, so a failed hook cannot poison agent context
//   - a zero-match `appraise --inject` prints empty stdout and exits 0
//     so the harness silently drops the inject
//
// # Format
//
// Payloads are plaintext markdown (headers + bullets), injected
// verbatim; there is no JSON envelope. Every payload's first line
// carries the version marker "[guild-payload v1]" so downstream
// consumers can detect format changes.
//
// Session payload (`guild quest brief --auto`):
//
//	## guild session priming [guild-payload v1]
//
//	**oath (top N):**
//	- <title>: <summary>
//
//	**last brief [<timestamp> by <agent>]:**
//	<text>
//
//	**top bounties:**
//	- QUEST-N [P1] <subject>
//
// Appraise payload (`guild lore appraise --inject`):
//
//	## relevant lore [guild-payload v1] (3 of 9 matches)
//	- LORE-N (kind, 14d) <title>: <summary>
//
// # Budgets
//
// The inject payload budget caps the session payload at ~800 tokens
// (~3500 bytes: oath ~300 tok, last brief ~300 tok, bounties ~200 tok)
// and the appraise payload at ~200 tokens (~800 bytes). Renderers
// enforce the byte ceilings structurally: per-section line caps,
// per-line truncation, and a final whole-payload clamp. Each renderer
// returns at most its ceiling minus one byte so the trailing newline
// the CLI printer appends still fits inside the budget.
package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// PayloadVersion is the format marker embedded in every payload header.
// Bump the trailing number on any breaking change to the rendered shape.
const PayloadVersion = "guild-payload v1"

// Byte and line budgets per the inject payload design. Bytes
// approximate tokens at ~4 bytes/token.
const (
	// MaxSessionBytes caps the whole session-priming payload (~800 tok).
	MaxSessionBytes = 3500
	// MaxOathBytes caps the oath section (~300 tok).
	MaxOathBytes = 1200
	// MaxOathLines caps the number of oath entries rendered.
	MaxOathLines = 12
	// MaxBriefBytes caps the last-brief section (~300 tok).
	MaxBriefBytes = 1200
	// MaxBountyBytes caps the top-bounties section (~200 tok).
	MaxBountyBytes = 800
	// MaxBountyLines caps the number of bounties rendered.
	MaxBountyLines = 3
	// MaxAppraiseBytes caps the whole appraise payload (~200 tok).
	MaxAppraiseBytes = 800
	// MaxAppraiseLines caps the number of pointer cites rendered.
	MaxAppraiseLines = 3
)

// Per-line truncation caps. Sized so the per-section byte budgets hold
// even at the line-count caps.
const (
	maxOathLineBytes     = 140
	maxBountyLineBytes   = 200
	maxAppraiseLineBytes = 230
)

// maxEnvelopeBytes guards PromptFromHookEnvelope against an unbounded
// stdin read. 1 MiB is far above any real harness envelope.
const maxEnvelopeBytes = 1 << 20

// OathLine is one active principle for the session payload.
type OathLine struct {
	Title   string
	Summary string
}

// BountyLine is one open quest for the session payload.
type BountyLine struct {
	ID       string // "QUEST-N"
	Priority string // "P1" etc.; may be empty
	Subject  string
}

// SessionData is the input to RenderSession: the oath wall, the most
// recent brief, and the top open bounties. Every field is optional;
// empty sections render "(none)" placeholders so the payload shape
// stays stable.
type SessionData struct {
	Oath       []OathLine
	BriefAt    string // e.g. "2006-01-02T15:04"; may be empty
	BriefAgent string // may be empty
	BriefText  string
	Bounties   []BountyLine
}

// RenderSession renders the bounded session-priming payload for
// `guild quest brief --auto`. The result carries no trailing newline
// and is guaranteed to be at most MaxSessionBytes-1 bytes so the
// printer's trailing newline keeps the total within budget.
func RenderSession(d SessionData) string {
	var b strings.Builder
	b.WriteString("## guild session priming [" + PayloadVersion + "]\n\n")
	b.WriteString(renderOathSection(d.Oath))
	b.WriteString("\n")
	b.WriteString(renderBriefSection(d.BriefAt, d.BriefAgent, d.BriefText))
	b.WriteString("\n")
	b.WriteString(renderBountySection(d.Bounties))
	return clampPayload(b.String(), MaxSessionBytes-1)
}

// AppraiseLine is one pointer cite for the appraise payload. Summary
// carries the entry's one-line summary; bodies never appear in the
// payload.
type AppraiseLine struct {
	ID      string // "LORE-N"
	Kind    string // "decision", "principle", ...
	AgeDays int
	Title   string
	Summary string
}

// RenderAppraise renders the bounded pointer-cite payload for
// `guild lore appraise --inject`. total is the number of matches the
// search found (the header reads "N of M matches"); at most
// MaxAppraiseLines lines are rendered. Zero lines return "" so the
// caller writes nothing to stdout. The result carries no trailing
// newline and is at most MaxAppraiseBytes-1 bytes.
func RenderAppraise(lines []AppraiseLine, total int) string {
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > MaxAppraiseLines {
		lines = lines[:MaxAppraiseLines]
	}
	if total < len(lines) {
		total = len(lines)
	}
	header := fmt.Sprintf("## relevant lore [%s] (%d of %d matches)\n",
		PayloadVersion, len(lines), total)

	var b strings.Builder
	b.WriteString(header)
	budget := MaxAppraiseBytes - 1 - len(header)
	for _, l := range lines {
		line := fmt.Sprintf("- %s (%s, %dd) %s", l.ID, l.Kind, l.AgeDays, oneLine(l.Title))
		if s := oneLine(l.Summary); s != "" {
			line += ": " + s
		}
		line = truncateBytes(line, maxAppraiseLineBytes)
		if len(line)+1 > budget {
			break
		}
		b.WriteString(line + "\n")
		budget -= len(line) + 1
	}
	return clampPayload(b.String(), MaxAppraiseBytes-1)
}

// hookEnvelope is the JSON object harnesses pipe to a hook's stdin on
// UserPromptSubmit. Claude Code and Codex emit the identical shape:
//
//	{"prompt": "...", "session_id": "...", "cwd": "...", "hook_event_name": "...", ...}
//
// Only .prompt is consumed; unknown fields are ignored. If a future
// harness ships a different envelope shape, add a sibling parser (and
// a sibling CLI flag) rather than overloading this one.
type hookEnvelope struct {
	Prompt string `json:"prompt"`
}

// PromptFromHookEnvelope reads the harness hook JSON envelope from r
// and returns its .prompt field. Errors on unreadable input, invalid
// JSON, or an empty prompt; callers must translate any error into
// exit-non-zero with empty stdout.
func PromptFromHookEnvelope(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxEnvelopeBytes))
	if err != nil {
		return "", fmt.Errorf("hooks: read stdin envelope: %w", err)
	}
	var env hookEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return "", fmt.Errorf("hooks: parse stdin envelope: %w", err)
	}
	prompt := strings.TrimSpace(env.Prompt)
	if prompt == "" {
		return "", errors.New("hooks: stdin envelope has empty .prompt")
	}
	return prompt, nil
}

// renderOathSection renders up to MaxOathLines principles inside
// MaxOathBytes. Title plus a truncated summary per line.
func renderOathSection(lines []OathLine) string {
	if len(lines) == 0 {
		return "**oath:** (none)\n"
	}
	if len(lines) > MaxOathLines {
		lines = lines[:MaxOathLines]
	}
	rendered := make([]string, 0, len(lines))
	budget := MaxOathBytes - len("**oath (top NN):**\n")
	for _, l := range lines {
		line := "- " + oneLine(l.Title)
		if s := oneLine(l.Summary); s != "" {
			line += ": " + s
		}
		line = truncateBytes(line, maxOathLineBytes)
		if len(line)+1 > budget {
			break
		}
		rendered = append(rendered, line)
		budget -= len(line) + 1
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("**oath (top %d):**\n", len(rendered)))
	for _, line := range rendered {
		b.WriteString(line + "\n")
	}
	return b.String()
}

// renderBriefSection renders the most recent brief, truncated so the
// whole section fits MaxBriefBytes.
func renderBriefSection(at, agent, text string) string {
	if strings.TrimSpace(text) == "" {
		return "**last brief:** (none)\n"
	}
	header := "**last brief"
	if at != "" {
		header += " [" + at
		if agent != "" {
			header += " by " + agent
		}
		header += "]"
	}
	header += ":**\n"
	body := truncateBytes(oneLine(text), MaxBriefBytes-len(header)-1)
	return header + body + "\n"
}

// renderBountySection renders up to MaxBountyLines open quests inside
// MaxBountyBytes.
func renderBountySection(lines []BountyLine) string {
	if len(lines) == 0 {
		return "**top bounties:** (none)\n"
	}
	if len(lines) > MaxBountyLines {
		lines = lines[:MaxBountyLines]
	}
	header := "**top bounties:**\n"
	var b strings.Builder
	b.WriteString(header)
	budget := MaxBountyBytes - len(header)
	for _, l := range lines {
		line := "- " + l.ID
		if l.Priority != "" {
			line += " [" + l.Priority + "]"
		}
		if s := oneLine(l.Subject); s != "" {
			line += " " + s
		}
		line = truncateBytes(line, maxBountyLineBytes)
		if len(line)+1 > budget {
			break
		}
		b.WriteString(line + "\n")
		budget -= len(line) + 1
	}
	return b.String()
}

// clampPayload trims trailing newlines and applies the whole-payload
// byte ceiling. The section budgets keep payloads well under their
// ceilings; this is the structural backstop.
func clampPayload(s string, maxBytes int) string {
	s = strings.TrimRight(s, "\n")
	return truncateBytes(s, maxBytes)
}

// oneLine collapses every whitespace run (including newlines) to a
// single space so one entry always renders as one markdown bullet.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateBytes cuts s to at most maxBytes bytes at a rune boundary,
// appending "..." when truncation occurred.
func truncateBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	const marker = "..."
	cut := maxBytes - len(marker)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}
