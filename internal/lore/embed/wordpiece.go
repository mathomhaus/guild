package embed

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"unicode"
)

// WordPieceTokenizer is a pure-Go port of the subset of HuggingFace
// BertTokenizer used by BAAI/bge-small-en-v1.5 and MiniLM variants.
// Configuration is hard-coded to match bge-small's tokenizer_config.json:
//
//	do_lower_case=true
//	tokenize_chinese_chars=true
//	strip_accents=None
//	do_basic_tokenize=true
//
// F7 from the spike's friction log: the Python reference pipeline's
// pooling layer is a separate concern; this tokenizer's job is to emit
// the same input_ids as transformers' BertTokenizer so the ONNX graph
// sees bit-identical inputs. Spike-verified against reference_vectors
// for five guild-anchored strings before this port.
type WordPieceTokenizer struct {
	Vocab        map[string]int
	UnkToken     string
	ClsToken     string
	SepToken     string
	PadToken     string
	MaxInputChar int
}

// LoadVocab reads a vocab.txt file (one token per line, zero-indexed)
// into a map suitable for NewWordPieceTokenizer. Uses a large scanner
// buffer because some vocab lines exceed the default 64KB cap on
// scanner buffers in pathological cases.
func LoadVocab(path string) (map[string]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("wordpiece: open vocab: %w", err)
	}
	defer f.Close()
	vocab := make(map[string]int, 32000)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	i := 0
	for scanner.Scan() {
		tok := scanner.Text()
		vocab[tok] = i
		i++
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("wordpiece: scan vocab: %w", err)
	}
	return vocab, nil
}

// NewWordPieceTokenizer constructs the tokenizer with bge-small defaults.
func NewWordPieceTokenizer(vocab map[string]int) *WordPieceTokenizer {
	return &WordPieceTokenizer{
		Vocab:        vocab,
		UnkToken:     "[UNK]",
		ClsToken:     "[CLS]",
		SepToken:     "[SEP]",
		PadToken:     "[PAD]",
		MaxInputChar: 100,
	}
}

// Encode runs BasicTokenizer + WordPiece, prepends [CLS] and appends
// [SEP], and truncates to maxLen (inclusive of specials). Returns token
// ids, attention mask (all ones for real tokens), and token_type_ids
// (all zeros for single-segment input).
func (t *WordPieceTokenizer) Encode(text string, maxLen int) (ids, mask, typeIDs []int64) {
	pieces := t.tokenize(text)
	cls := int64(t.Vocab[t.ClsToken])
	sep := int64(t.Vocab[t.SepToken])

	ids = append(ids, cls)
	for _, p := range pieces {
		if id, ok := t.Vocab[p]; ok {
			ids = append(ids, int64(id))
		} else {
			ids = append(ids, int64(t.Vocab[t.UnkToken]))
		}
	}
	ids = append(ids, sep)

	if maxLen > 0 && len(ids) > maxLen {
		ids = ids[:maxLen]
		ids[maxLen-1] = sep
	}
	mask = make([]int64, len(ids))
	typeIDs = make([]int64, len(ids))
	for i := range ids {
		mask[i] = 1
	}
	return ids, mask, typeIDs
}

// tokenize runs BasicTokenizer then WordPiece over each produced token.
func (t *WordPieceTokenizer) tokenize(text string) []string {
	basics := basicTokenize(text)
	var out []string
	for _, b := range basics {
		out = append(out, t.wordpiece(b)...)
	}
	return out
}

// basicTokenize is a Go port of
// transformers.BertTokenizer.BasicTokenizer.tokenize with
// do_lower_case=true, tokenize_chinese_chars=true, strip_accents=None.
func basicTokenize(text string) []string {
	text = cleanText(text)
	text = tokenizeCJK(text)
	whiteSpace := strings.Fields(text)
	var out []string
	for _, tok := range whiteSpace {
		tok = strings.ToLower(tok)
		out = append(out, splitOnPunc(tok)...)
	}
	var joined []string
	for _, o := range out {
		for _, s := range strings.Fields(o) {
			if s != "" {
				joined = append(joined, s)
			}
		}
	}
	return joined
}

// cleanText removes control characters and normalizes whitespace to a
// single ASCII space. Matches BertTokenizer._clean_text.
func cleanText(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		if r == 0 || r == 0xfffd || isControl(r) {
			continue
		}
		if isWhitespace(r) {
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// tokenizeCJK adds spaces around CJK ideographs so they become standalone
// tokens. Not strictly needed for an English guild corpus but required
// to stay bit-identical to HuggingFace BertTokenizer.
func tokenizeCJK(text string) string {
	var b strings.Builder
	b.Grow(len(text) + 8)
	for _, r := range text {
		if isCJK(r) {
			b.WriteRune(' ')
			b.WriteRune(r)
			b.WriteRune(' ')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// splitOnPunc splits a token on punctuation, emitting each punctuation
// character as its own token. Mirrors BertTokenizer._run_split_on_punc.
func splitOnPunc(text string) []string {
	var out []string
	var cur []rune
	for _, r := range text {
		if isPunctuation(r) {
			if len(cur) > 0 {
				out = append(out, string(cur))
				cur = cur[:0]
			}
			out = append(out, string(r))
			continue
		}
		cur = append(cur, r)
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}

// wordpiece implements HuggingFace WordPiece: greedy longest-match from
// the left, subword tokens prefixed "##". Returns [UNK] for tokens that
// cannot be segmented at all or exceed MaxInputChar runes.
func (t *WordPieceTokenizer) wordpiece(token string) []string {
	runes := []rune(token)
	if len(runes) > t.MaxInputChar {
		return []string{t.UnkToken}
	}
	var pieces []string
	start := 0
	for start < len(runes) {
		end := len(runes)
		var matched string
		for end > start {
			sub := string(runes[start:end])
			if start > 0 {
				sub = "##" + sub
			}
			if _, ok := t.Vocab[sub]; ok {
				matched = sub
				break
			}
			end--
		}
		if matched == "" {
			return []string{t.UnkToken}
		}
		pieces = append(pieces, matched)
		start = end
	}
	return pieces
}

// isWhitespace matches BertTokenizer's whitespace predicate: ASCII
// whitespace plus anything unicode.IsSpace thinks is whitespace. Note
// \t \n \r are explicitly whitespace even though some are technically
// control chars elsewhere.
func isWhitespace(r rune) bool {
	if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
		return true
	}
	return unicode.IsSpace(r)
}

// isControl matches BertTokenizer's control predicate. \t \n \r are NOT
// control here (they are whitespace above); this ordering is the subtle
// F7 fix that keeps tokenizer output bit-parity against the reference.
func isControl(r rune) bool {
	if r == '\t' || r == '\n' || r == '\r' {
		return false
	}
	return unicode.IsControl(r)
}

// isPunctuation matches BertTokenizer: ASCII non-alphanumeric printable
// characters (the four ASCII punctuation ranges) plus unicode Punct/
// Symbol categories. Matching both sets is the subtle F7 fix that
// handles stray symbols inside English tokens correctly.
func isPunctuation(r rune) bool {
	switch {
	case r >= 33 && r <= 47:
		return true
	case r >= 58 && r <= 64:
		return true
	case r >= 91 && r <= 96:
		return true
	case r >= 123 && r <= 126:
		return true
	}
	return unicode.IsPunct(r) || unicode.IsSymbol(r)
}

// isCJK matches the CJK Unified Ideograph ranges BertTokenizer uses for
// tokenize_chinese_chars=true.
func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF:
		return true
	case r >= 0x3400 && r <= 0x4DBF:
		return true
	case r >= 0x20000 && r <= 0x2A6DF:
		return true
	case r >= 0x2A700 && r <= 0x2B73F:
		return true
	case r >= 0x2B740 && r <= 0x2B81F:
		return true
	case r >= 0x2B820 && r <= 0x2CEAF:
		return true
	case r >= 0xF900 && r <= 0xFAFF:
		return true
	case r >= 0x2F800 && r <= 0x2FA1F:
		return true
	}
	return false
}

// assertVocabHasSpecials is called from BGEEmbedder's constructor so a
// vocab missing required specials fails loudly at setup rather than
// silently UNK-ing every token at inference time. Wraps
// ErrVocabMissingSpecial with the offending token name for diagnostics.
func (t *WordPieceTokenizer) assertVocabHasSpecials() error {
	for _, s := range []string{t.ClsToken, t.SepToken, t.PadToken, t.UnkToken} {
		if _, ok := t.Vocab[s]; !ok {
			return fmt.Errorf("%w: %q", ErrVocabMissingSpecial, s)
		}
	}
	return nil
}
