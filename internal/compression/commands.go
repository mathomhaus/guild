package compression

import (
	"context"
	"fmt"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
)

// CLI + MCP verbs for the compression module. Each is a single
// command.Command spec that generates both a cobra verb and an MCP tool, the
// same authoring shape every other guild verb uses. The module binds these
// only when [modules].compression = true, so by default neither surface
// advertises them.

// ─── compress ───────────────────────────────────────────────────────────

// CompressInput is the compress verb's input.
type CompressInput struct {
	Content  string `json:"content" jsonschema:"the text to compress (a diff, log, grep output, or JSON array)"`
	Strategy string `json:"strategy,omitempty" jsonschema:"strategy name: json|diff|log|search; empty auto-detects"`
	Context  string `json:"context,omitempty" jsonschema:"optional query context to bias relevance scoring"`
}

// CompressOutput is the compress verb's structured result.
type CompressOutput struct {
	Strategy        string `json:"strategy"`
	Lossless        bool   `json:"lossless"`
	Compressed      string `json:"compressed"`
	CacheKey        string `json:"cache_key,omitempty"`
	OriginalBytes   int    `json:"original_bytes"`
	CompressedBytes int    `json:"compressed_bytes"`
}

// CompressCommand compresses a blob with a named or auto-detected strategy.
var CompressCommand = &command.Command[CompressInput, CompressOutput]{
	Name:    "compression_compress",
	CLIPath: []string{"compression", "compress"},
	Short:   "compress a diff, log, grep, or JSON blob",
	Long: "Compress verbose tool output. The json strategy is lossless (no retrieval needed); " +
		"diff/log/search are lossy-with-CCR (the full original is stashed and recoverable via compression_retrieve). " +
		"An empty strategy auto-detects from the content.",
	Args: []command.ArgSpec{
		{Name: "content", Kind: command.ArgPositional, Type: command.ArgString, Required: true, Variadic: true, Help: "text to compress"},
		{Name: "strategy", Short: "s", Kind: command.ArgFlag, Type: command.ArgString, Help: "strategy: json|diff|log|search (empty auto-detects)"},
		{Name: "context", Short: "c", Kind: command.ArgFlag, Type: command.ArgString, Help: "query context for relevance scoring"},
	},
	Handler: func(_ context.Context, _ command.Deps, in CompressInput) (CompressOutput, error) {
		name := in.Strategy
		if name == "" {
			name = DetectStrategy(in.Content)
		}
		strat, err := BuildStrategy(name)
		if err != nil {
			return CompressOutput{}, err
		}
		res, err := strat.Compress(in.Content, in.Context, SharedStore())
		if err != nil {
			return CompressOutput{}, err
		}
		return CompressOutput{
			Strategy:        name,
			Lossless:        res.Lossless,
			Compressed:      res.Compressed,
			CacheKey:        res.CacheKey,
			OriginalBytes:   res.OriginalBytes,
			CompressedBytes: res.CompressedBytes,
		}, nil
	},
	CLIFormat: func(s command.CLISink, o CompressOutput) string {
		var b strings.Builder
		b.WriteString(o.Compressed)
		if !strings.HasSuffix(o.Compressed, "\n") {
			b.WriteByte('\n')
		}
		b.WriteString(s.Row("strategy=%s lossless=%v %d→%d bytes", o.Strategy, o.Lossless, o.OriginalBytes, o.CompressedBytes))
		return b.String()
	},
	MCPFormat: func(s command.MCPSink, o CompressOutput) string {
		var b strings.Builder
		b.WriteString(o.Compressed)
		if !strings.HasSuffix(o.Compressed, "\n") {
			b.WriteByte('\n')
		}
		meta := []string{
			fmt.Sprintf("strategy=%s", o.Strategy),
			fmt.Sprintf("lossless=%v", o.Lossless),
			fmt.Sprintf("bytes=%d→%d", o.OriginalBytes, o.CompressedBytes),
		}
		if o.CacheKey != "" {
			meta = append(meta, "hash="+o.CacheKey)
		}
		b.WriteString(s.Meta(meta...))
		return b.String()
	},
}

// ─── retrieve ───────────────────────────────────────────────────────────

// RetrieveInput is the retrieve verb's input.
type RetrieveInput struct {
	Hash string `json:"hash" jsonschema:"a CCR hash, a <<ccr:HASH>> marker, or a compressed block that contains one"`
}

// RetrieveOutput is the retrieve verb's structured result.
type RetrieveOutput struct {
	Hash     string `json:"hash"`
	Found    bool   `json:"found"`
	Original string `json:"original,omitempty"`
}

// RetrieveCommand expands a CCR marker/hash back to the stashed original.
var RetrieveCommand = &command.Command[RetrieveInput, RetrieveOutput]{
	Name:    "compression_retrieve",
	CLIPath: []string{"compression", "retrieve"},
	Short:   "expand a CCR marker/hash back to the original",
	Long: "Look up a CCR hash and return the full original payload that a lossy compress stashed. " +
		"Accepts a bare hash, a <<ccr:HASH>> marker, or a whole compressed block (the first marker is resolved).",
	Args: []command.ArgSpec{
		{Name: "hash", Kind: command.ArgPositional, Type: command.ArgString, Required: true, Variadic: true, Help: "hash, marker, or block containing a marker"},
	},
	Handler: func(_ context.Context, _ command.Deps, in RetrieveInput) (RetrieveOutput, error) {
		hash := ResolveHash(in.Hash)
		if hash == "" {
			return RetrieveOutput{Found: false}, nil
		}
		original, ok := SharedStore().Get(hash)
		return RetrieveOutput{Hash: hash, Found: ok, Original: original}, nil
	},
	CLIFormat: func(s command.CLISink, o RetrieveOutput) string {
		if !o.Found {
			return s.Line("❌", "[x]", fmt.Sprintf("no CCR entry for hash %q (expired or never stored)", o.Hash))
		}
		out := o.Original
		if !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		return out
	},
	MCPFormat: func(s command.MCPSink, o RetrieveOutput) string {
		if !o.Found {
			return s.Line("❌", "", fmt.Sprintf("no CCR entry for hash %q (expired or never stored)", o.Hash))
		}
		out := o.Original
		if !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		return out
	},
}

// ResolveHash turns a user-supplied hash/marker/block into a bare hash. A
// bare lowercase-hex token is returned as-is; otherwise the first embedded
// CCR marker hash (either grammar) is extracted.
func ResolveHash(in string) string {
	in = strings.TrimSpace(in)
	if in == "" {
		return ""
	}
	if isBareHash(in) {
		return in
	}
	return ExtractMarkerHash(in)
}

func isBareHash(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// DetectStrategy picks a strategy name from content shape: a JSON array of
// objects → json (lossless); diff headers → diff; grep file:line: shape →
// search; otherwise log. The auto-detect is a convenience; an explicit
// strategy always overrides it.
func DetectStrategy(content string) string {
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "[") {
		if _, ok := CompactJSON(content); ok {
			return "json"
		}
	}
	for _, line := range strings.Split(content, "\n") {
		if isDiffHeader(line) {
			return "diff"
		}
	}
	searchHits := 0
	scanned := 0
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		scanned++
		if scanned > 20 {
			break
		}
		if _, _, _, ok := parseMatchLine(line); ok {
			searchHits++
		}
	}
	if scanned > 0 && searchHits*2 >= scanned {
		return "search"
	}
	return "log"
}
