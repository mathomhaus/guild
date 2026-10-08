package compression

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// JSON/SmartCrusher lossless array compaction. Port of the lossless core of
// Headroom's smart_crusher compaction stage (compaction/{compactor,
// formatter}.rs): an array of homogeneous JSON objects is compacted to a
// schema + rows form rendered as a token-efficient CSV+schema block:
//
//	[N]{col:type,col:type?,...}
//	v,v,v
//	v,v,v
//
// This is LOSSLESS: RestoreCrushed reproduces the original array exactly from
// the compact form alone, so no CCR retrieval is needed. The "?" suffix marks
// a nullable/sparse column; an empty cell is a missing field (the field was
// absent in that row), distinct from a literal null. We deliberately exclude
// the lossy row-dropping, opaque-CCR-cell, bucketing, and nested-flatten
// elaborations of the full SmartCrusher: this is the genuinely round-trippable
// piece, which is the deliverable's "lossless render" guarantee.
//
// When the input is not a compactable array of >=2 homogeneous objects, the
// compactor declines and the strategy returns the input unchanged.

// fieldSpec is one column's metadata.
type fieldSpec struct {
	name     string
	typeTag  string // int|float|string|bool|null|json
	nullable bool   // some row had the field absent or null
}

// table is a homogeneous tabular compaction.
type table struct {
	fields []fieldSpec
	// rows holds, per row, a cell per field. A nil *json.RawMessage means
	// the field was absent in that row (Missing); a non-nil one holds the
	// raw JSON value (which may itself be the literal null).
	rows [][]*json.RawMessage
}

type crusherStrategy struct{}

func init() { RegisterStrategy("json", func() Strategy { return crusherStrategy{} }) }

func (crusherStrategy) Name() string   { return "json" }
func (crusherStrategy) Lossless() bool { return true }

func (crusherStrategy) Compress(content, _ string, _ Store) (Result, error) {
	compact, ok := CompactJSON(content)
	if !ok {
		return Result{
			Compressed:      content,
			Lossless:        true,
			OriginalBytes:   len(content),
			CompressedBytes: len(content),
		}, nil
	}
	return Result{
		Compressed:      compact,
		Lossless:        true,
		OriginalBytes:   len(content),
		CompressedBytes: len(compact),
	}, nil
}

// CompactJSON compacts a JSON array-of-objects into the CSV+schema block.
// Returns (block, true) on success, or ("", false) when the input is not a
// compactable array (non-array, fewer than 2 items, or items that aren't all
// objects). The returned block round-trips through RestoreCrushed.
func CompactJSON(content string) (string, bool) {
	var arr []json.RawMessage
	dec := json.NewDecoder(strings.NewReader(content))
	dec.UseNumber()
	if err := dec.Decode(&arr); err != nil {
		return "", false
	}
	if len(arr) < 2 {
		return "", false
	}

	objs := make([]map[string]json.RawMessage, len(arr))
	for i, raw := range arr {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return "", false
		}
		// Reject non-object array elements (e.g. a bare number): Unmarshal
		// into a map errors for those, so reaching here means an object.
		objs[i] = obj
	}

	t := buildTable(objs)
	return renderCSVSchema(t), true
}

func buildTable(objs []map[string]json.RawMessage) table {
	// Column order: descending frequency, then alphabetical (stable).
	freq := map[string]int{}
	for _, o := range objs {
		for k := range o {
			freq[k]++
		}
	}
	keys := make([]string, 0, len(freq))
	for k := range freq {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if freq[keys[i]] != freq[keys[j]] {
			return freq[keys[i]] > freq[keys[j]]
		}
		return keys[i] < keys[j]
	})

	total := len(objs)
	fields := make([]fieldSpec, len(keys))
	for ci, k := range keys {
		nullable := freq[k] < total
		tag := ""
		for _, o := range objs {
			raw, present := o[k]
			if !present {
				continue
			}
			if isJSONNull(raw) {
				nullable = true
				continue
			}
			tag = mergeTypeTag(tag, inferTypeTag(raw))
		}
		if tag == "" {
			tag = "null"
		}
		fields[ci] = fieldSpec{name: k, typeTag: tag, nullable: nullable}
	}

	rows := make([][]*json.RawMessage, len(objs))
	for ri, o := range objs {
		cells := make([]*json.RawMessage, len(keys))
		for ci, k := range keys {
			if raw, present := o[k]; present {
				v := raw
				cells[ci] = &v
			} else {
				cells[ci] = nil // Missing
			}
		}
		rows[ri] = cells
	}
	return table{fields: fields, rows: rows}
}

func renderCSVSchema(t table) string {
	var b strings.Builder
	b.WriteByte('[')
	b.WriteString(strconv.Itoa(len(t.rows)))
	b.WriteString("]{")
	decls := make([]string, len(t.fields))
	for i, f := range t.fields {
		if f.nullable {
			decls[i] = f.name + ":" + f.typeTag + "?"
		} else {
			decls[i] = f.name + ":" + f.typeTag
		}
	}
	b.WriteString(strings.Join(decls, ","))
	b.WriteString("}\n")

	for _, row := range t.rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = formatCell(c)
		}
		b.WriteString(strings.Join(cells, ","))
		b.WriteByte('\n')
	}
	return b.String()
}

// formatCell renders one cell. Missing → empty. A literal null also renders
// empty, which is why a nullable column is needed to disambiguate on restore:
// a column declared nullable that has an empty cell restores to JSON null
// only when the row supplies the field, which CSV alone cannot express.
// To keep the render strictly lossless we instead emit an explicit sentinel
// for a present-null: "\N" (CSV-quoted if it ever collides). Scalars render
// as their JSON literal; strings render bare unless they need CSV quoting.
func formatCell(c *json.RawMessage) string {
	if c == nil {
		return "" // Missing field
	}
	raw := *c
	if isJSONNull(raw) {
		return nullSentinel
	}
	// Strings render unquoted unless they need CSV escaping; everything
	// else (number, bool, nested object/array) renders as compact JSON.
	// A present empty string is quoted ("") so it survives the round-trip
	// distinct from a Missing (bare empty) cell, and a string that happens
	// to equal the null sentinel is quoted so it is not read back as null.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" || s == nullSentinel || needsCSVQuote(s) {
			return csvQuote(s)
		}
		return s
	}
	return csvQuote(compactJSONValue(raw))
}

// nullSentinel marks a present literal null, distinct from an empty (Missing)
// cell. Chosen to never collide with an ordinary unquoted string (a string
// equal to it is CSV-quoted on render and unquoted on restore).
const nullSentinel = `\N`

// RestoreCrushed reproduces the original JSON array from a CSV+schema block
// produced by CompactJSON. It is the lossless inverse: the returned string is
// a JSON array whose elements equal the original objects (key order is not
// preserved, matching JSON object semantics). Returns ok=false when block is
// not a recognizable CSV+schema render.
func RestoreCrushed(block string) (string, bool) {
	lines := strings.Split(block, "\n")
	if len(lines) == 0 {
		return "", false
	}
	header := lines[0]
	fields, _, ok := parseHeader(header)
	if !ok {
		return "", false
	}

	out := make([]map[string]json.RawMessage, 0)
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		cells := splitCSV(line)
		if len(cells) != len(fields) {
			return "", false
		}
		obj := make(map[string]json.RawMessage, len(fields))
		for ci, f := range fields {
			cell := cells[ci]
			if cell.missing {
				continue // field absent in this row
			}
			raw, err := cellToRaw(cell.value, cell.quoted, f.typeTag)
			if err != nil {
				return "", false
			}
			obj[f.name] = raw
		}
		out = append(out, obj)
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

// parsedCell carries whether a CSV cell was an empty (missing) field or a
// present value (which may have been CSV-quoted).
type parsedCell struct {
	value   string
	missing bool
	quoted  bool
}

func parseHeader(header string) ([]fieldSpec, int, bool) {
	// [N]{col:type,col:type?,...}
	if !strings.HasPrefix(header, "[") {
		return nil, 0, false
	}
	closeIdx := strings.IndexByte(header, ']')
	if closeIdx < 0 {
		return nil, 0, false
	}
	n, err := strconv.Atoi(header[1:closeIdx])
	if err != nil {
		return nil, 0, false
	}
	rest := header[closeIdx+1:]
	if !strings.HasPrefix(rest, "{") {
		return nil, 0, false
	}
	end := strings.IndexByte(rest, '}')
	if end < 0 {
		return nil, 0, false
	}
	decls := rest[1:end]
	var fields []fieldSpec
	if decls != "" {
		for _, d := range strings.Split(decls, ",") {
			colon := strings.LastIndexByte(d, ':')
			if colon < 0 {
				return nil, 0, false
			}
			name := d[:colon]
			tag := d[colon+1:]
			nullable := false
			if strings.HasSuffix(tag, "?") {
				nullable = true
				tag = tag[:len(tag)-1]
			}
			fields = append(fields, fieldSpec{name: name, typeTag: tag, nullable: nullable})
		}
	}
	return fields, n, true
}

func cellToRaw(value string, quoted bool, typeTag string) (json.RawMessage, error) {
	// Only an UNQUOTED null sentinel is a present literal null; a quoted
	// occurrence is a string that happened to equal the sentinel.
	if !quoted && value == nullSentinel {
		return json.RawMessage("null"), nil
	}
	switch typeTag {
	case "int", "float", "bool", "null":
		// Numbers and bools render bare; re-emit verbatim as a JSON literal.
		return json.RawMessage(value), nil
	case "json":
		return json.RawMessage(value), nil
	default: // string
		b, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return b, nil
	}
}

// ─── type inference + CSV escaping ──────────────────────────────────────

func inferTypeTag(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "null"
	}
	switch trimmed[0] {
	case '"':
		return "string"
	case 't', 'f':
		return "bool"
	case 'n':
		return "null"
	case '{', '[':
		return "json"
	default:
		// number
		if strings.ContainsAny(trimmed, ".eE") {
			return "float"
		}
		return "int"
	}
}

// mergeTypeTag widens a column type when rows disagree (e.g. int + float =
// float; any mix with string/json widens to json so the cell stays lossless).
func mergeTypeTag(a, b string) string {
	if a == "" {
		return b
	}
	if a == b {
		return a
	}
	if (a == "int" && b == "float") || (a == "float" && b == "int") {
		return "float"
	}
	// Mixed scalar kinds: fall back to json (renders as a JSON literal,
	// restores verbatim), which is always lossless.
	return "json"
}

func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

func compactJSONValue(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

func needsCSVQuote(s string) bool {
	return strings.ContainsAny(s, ",\"\n\r")
}

func csvQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// splitCSV splits one CSV line honoring double-quote escaping (RFC 4180
// minus embedded newlines, which our render never emits inside a field
// because a quoted field containing a newline would only arise from a
// multi-line string value — handled by the JSON fallback render path).
// It always returns exactly (number of top-level commas + 1) cells: one
// parse iteration per field, each ending by consuming a comma (next field)
// or the end of the line. An unquoted empty field is a Missing cell; a
// quoted empty field ("") is a present empty string.
func splitCSV(line string) []parsedCell {
	var cells []parsedCell
	i := 0
	for {
		if i < len(line) && line[i] == '"' {
			// Quoted field: read to the closing quote, honoring "" escapes.
			i++
			var b strings.Builder
			for i < len(line) {
				if line[i] == '"' {
					if i+1 < len(line) && line[i+1] == '"' {
						b.WriteByte('"')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(line[i])
				i++
			}
			cells = append(cells, parsedCell{value: b.String(), quoted: true})
		} else {
			// Unquoted field up to the next comma.
			start := i
			for i < len(line) && line[i] != ',' {
				i++
			}
			field := line[start:i]
			cells = append(cells, parsedCell{value: field, missing: field == "", quoted: false})
		}
		if i < len(line) && line[i] == ',' {
			i++
			continue // another field follows
		}
		break
	}
	return cells
}
