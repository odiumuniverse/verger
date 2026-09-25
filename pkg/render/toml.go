package render

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// tomlBasicString renders a TOML basic string.
func tomlBasicString(value string) string {
	var b strings.Builder

	b.WriteByte('"')

	for _, r := range value {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}

	b.WriteByte('"')

	return b.String()
}

// tomlMultilineValue renders a canonical body as a TOML multi-line basic
// string with exactly one trailing newline.
func tomlMultilineValue(body string) string {
	trimmed := strings.TrimRight(body, "\r\n")

	var b strings.Builder

	b.WriteString(`"""` + "\n")
	b.WriteString(tomlBasicBody(trimmed))

	if trimmed != "" {
		b.WriteByte('\n')
	}

	b.WriteString(`"""`)

	return b.String()
}

// tomlBasicBody escapes text for a multi-line basic string.
func tomlBasicBody(value string) string {
	var b strings.Builder

	for _, r := range value {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteByte('\n')
		case '\t':
			b.WriteByte('\t')
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}

	return b.String()
}

// tomlValue renders one Go value as TOML.
func tomlValue(value any) (string, error) {
	if scalar, ok := tomlScalar(value); ok {
		return scalar, nil
	}

	switch typed := value.(type) {
	case []string:
		return tomlArray(typed), nil
	case []any:
		return tomlArrayAny(typed)
	case map[string]string:
		return tomlInlineStringTable(typed), nil
	case map[string]any:
		return tomlInlineTable(typed)
	default:
		return "", fmt.Errorf("unsupported toml value of type %T", value)
	}
}

// tomlScalar renders a scalar TOML value.
func tomlScalar(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		if strings.Contains(typed, "\n") {
			return tomlMultilineValue(typed), true
		}

		return tomlBasicString(typed), true
	case bool:
		return strconv.FormatBool(typed), true
	case int:
		return strconv.Itoa(typed), true
	case int64:
		return strconv.FormatInt(typed, 10), true
	case uint64:
		return strconv.FormatUint(typed, 10), true
	case float64:
		return tomlFloat(typed), true
	case time.Time:
		return typed.Format(time.RFC3339), true
	case toml.LocalDate:
		return typed.String(), true
	case toml.LocalTime:
		return typed.String(), true
	case toml.LocalDateTime:
		return typed.String(), true
	default:
		return "", false
	}
}

// tomlFloat renders a float; a whole float keeps its ".0" so TOML reads it as
// a float.
func tomlFloat(value float64) string {
	out := strconv.FormatFloat(value, 'g', -1, 64)
	if !strings.ContainsAny(out, ".eEnN") {
		out += ".0"
	}

	return out
}

// tomlArray renders a string array.
func tomlArray(items []string) string {
	values := make([]string, 0, len(items))

	for _, item := range items {
		values = append(values, tomlBasicString(item))
	}

	return "[" + strings.Join(values, ", ") + "]"
}

// tomlArrayAny renders an untyped array.
func tomlArrayAny(items []any) (string, error) {
	values := make([]string, 0, len(items))

	for _, item := range items {
		value, err := tomlValue(item)
		if err != nil {
			return "", err
		}

		values = append(values, value)
	}

	return "[" + strings.Join(values, ", ") + "]", nil
}

// tomlInlineTable renders a nested table inline, keys sorted.
func tomlInlineTable(values map[string]any) (string, error) {
	parts := make([]string, 0, len(values))

	for _, key := range sortedKeys(values) {
		value, err := tomlValue(values[key])
		if err != nil {
			return "", err
		}

		parts = append(parts, tomlKey(key)+" = "+value)
	}

	return "{" + strings.Join(parts, ", ") + "}", nil
}

// tomlInlineStringTable renders a string table inline, keys sorted.
func tomlInlineStringTable(values map[string]string) string {
	parts := make([]string, 0, len(values))

	for _, key := range sortedKeys(values) {
		parts = append(parts, tomlKey(key)+" = "+tomlBasicString(values[key]))
	}

	return "{" + strings.Join(parts, ", ") + "}"
}

// tomlKey renders a key bare when possible.
func tomlKey(key string) string {
	if isBareTOMLKey(key) {
		return key
	}

	return tomlBasicString(key)
}

// isBareTOMLKey reports whether a key needs no quoting.
func isBareTOMLKey(key string) bool {
	if key == "" {
		return false
	}

	for _, r := range key {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}

		return false
	}

	return true
}

// tomlKeyPath renders a dotted key path with per-segment quoting.
func tomlKeyPath(path []string) string {
	parts := make([]string, 0, len(path))

	for _, segment := range path {
		parts = append(parts, tomlKey(segment))
	}

	return strings.Join(parts, ".")
}

// tomlBOM is the UTF-8 byte order mark some editors prepend.
func tomlBOM() []byte {
	return []byte{0xEF, 0xBB, 0xBF}
}

// tomlStart returns the offset of the first content byte, skipping a BOM.
func tomlStart(data []byte) int {
	if bytes.HasPrefix(data, tomlBOM()) {
		return len(tomlBOM())
	}

	return 0
}

// tomlKeySpan is one top-level assignment in a TOML document.
type tomlKeySpan struct {
	key        string
	lineStart  int
	valueStart int
	valueEnd   int
	end        int
}

// tomlSpanScan records the top-level assignments of a TOML document and the
// offset where the first table header starts.
type tomlSpanScan struct {
	spans      []tomlKeySpan
	tableStart int
}

// scanTOMLSpans walks a TOML document once and records the value span of every
// top-level key. Scanning stops at the first table header.
func scanTOMLSpans(data []byte) tomlSpanScan {
	scan := tomlSpanScan{tableStart: len(data)}

	for i := tomlStart(data); i < len(data); {
		lineStart := i

		lineEnd := bytes.IndexByte(data[i:], '\n')
		if lineEnd < 0 {
			lineEnd = len(data)
		} else {
			lineEnd = i + lineEnd
		}

		next := lineEnd
		if next < len(data) {
			next++
		}

		line := data[lineStart:lineEnd]
		trimmed := bytes.TrimLeft(line, " \t")

		switch {
		case len(trimmed) == 0 || trimmed[0] == '#':
			i = next

			continue
		case trimmed[0] == '[':
			scan.tableStart = lineStart

			return scan
		}

		key, afterKey, ok := scanTOMLKey(trimmed)
		if !ok {
			i = next

			continue
		}

		eq := bytes.IndexByte(afterKey, '=')
		if eq < 0 {
			i = next

			continue
		}

		valueStart := lineEnd - len(afterKey) + eq + 1
		valueStart += leadingSpace(data[valueStart:])

		valueEnd, end := scanTOMLValue(data, valueStart)

		scan.spans = append(scan.spans, tomlKeySpan{
			key:        key,
			lineStart:  lineStart,
			valueStart: valueStart,
			valueEnd:   valueEnd,
			end:        end,
		})

		i = end
		if i <= lineStart {
			i = next
		}
	}

	return scan
}

// scanTOMLKey reads a bare or quoted key at the start of a stripped line.
func scanTOMLKey(line []byte) (string, []byte, bool) {
	if len(line) == 0 {
		return "", nil, false
	}

	switch line[0] {
	case '"', '\'':
		quote := line[0]

		end := bytes.IndexByte(line[1:], quote)
		if end < 0 {
			return "", nil, false
		}

		return unquoteTOML(string(line[1 : 1+end])), line[2+end:], true
	default:
		end := bytes.IndexAny(line, " \t=")
		if end <= 0 {
			return "", nil, false
		}

		return string(line[:end]), line[end:], true
	}
}

// scanTOMLValue returns the end of the value that starts at offset and the
// offset just after the value's last line.
func scanTOMLValue(data []byte, offset int) (valueEnd, end int) {
	if offset >= len(data) {
		return len(data), len(data)
	}

	switch {
	case bytes.HasPrefix(data[offset:], []byte(`"""`)):
		return scanTOMLMultiline(data, offset, []byte(`"""`), true)
	case bytes.HasPrefix(data[offset:], []byte(`'''`)):
		return scanTOMLMultiline(data, offset, []byte(`'''`), false)
	case data[offset] == '[' || data[offset] == '{':
		return scanTOMLNested(data, offset)
	default:
		return scanTOMLScalar(data, offset)
	}
}

// scanTOMLMultiline finds the closing delimiter of a multi-line string.
func scanTOMLMultiline(data []byte, offset int, delimiter []byte, escaped bool) (valueEnd, end int) {
	pos := offset + len(delimiter)

	for pos < len(data) {
		if escaped && data[pos] == '\\' && pos+1 < len(data) {
			pos += 2

			continue
		}

		if bytes.HasPrefix(data[pos:], delimiter) {
			pos += len(delimiter)

			return pos, afterLine(data, pos)
		}

		pos++
	}

	return len(data), len(data)
}

// scanTOMLNested finds the end of an array or inline table.
func scanTOMLNested(data []byte, offset int) (valueEnd, end int) {
	depth := 0
	pos := offset

	for pos < len(data) {
		switch data[pos] {
		case '"', '\'':
			pos = skipTOMLString(data, pos)

			continue
		case '#':
			if line := bytes.IndexByte(data[pos:], '\n'); line >= 0 {
				pos += line

				continue
			}

			return len(data), len(data)
		case '[', '{':
			depth++
		case ']', '}':
			depth--

			if depth == 0 {
				return pos + 1, afterLine(data, pos+1)
			}
		}

		pos++
	}

	return len(data), len(data)
}

// scanTOMLScalar finds the end of a single-line value, dropping trailing
// whitespace and comments.
func scanTOMLScalar(data []byte, offset int) (valueEnd, end int) {
	pos := offset

	if pos < len(data) && (data[pos] == '"' || data[pos] == '\'') {
		pos = skipTOMLString(data, pos)
	}

	for pos < len(data) && data[pos] != '\n' {
		if tomlCommentStart(data, pos, offset) {
			break
		}

		pos++
	}

	valueEnd = trimScalarEnd(data, offset, pos)

	return valueEnd, afterLine(data, valueEnd)
}

// tomlCommentStart reports whether the byte at pos starts a trailing comment
// of the value that starts at offset.
func tomlCommentStart(data []byte, pos, offset int) bool {
	if data[pos] != '#' {
		return false
	}

	return pos == offset || data[pos-1] == ' ' || data[pos-1] == '\t'
}

// trimScalarEnd drops trailing spaces, tabs and carriage returns.
func trimScalarEnd(data []byte, offset, pos int) int {
	for pos > offset && (data[pos-1] == ' ' || data[pos-1] == '\t' || data[pos-1] == '\r') {
		pos--
	}

	return pos
}

// skipTOMLString returns the offset just after a TOML string that starts at
// pos.
func skipTOMLString(data []byte, pos int) int {
	quote := data[pos]

	if bytes.HasPrefix(data[pos:], []byte(`"""`)) || bytes.HasPrefix(data[pos:], []byte(`'''`)) {
		delimiter := []byte{quote, quote, quote}

		end, _ := scanTOMLMultiline(data, pos, delimiter, quote == '"')

		return end
	}

	for i := pos + 1; i < len(data); i++ {
		if quote == '"' && data[i] == '\\' {
			i++

			continue
		}

		if data[i] == quote {
			return i + 1
		}

		if data[i] == '\n' {
			return i
		}
	}

	return len(data)
}

// afterLine returns the offset just after the line that contains offset.
func afterLine(data []byte, offset int) int {
	line := bytes.IndexByte(data[offset:], '\n')
	if line < 0 {
		return len(data)
	}

	return offset + line + 1
}

// leadingSpace returns the number of leading spaces and tabs in data.
func leadingSpace(data []byte) int {
	pos := 0

	for pos < len(data) && (data[pos] == ' ' || data[pos] == '\t') {
		pos++
	}

	return pos
}

// unquoteTOML strips the escapes of a quoted TOML key.
func unquoteTOML(key string) string {
	key = strings.ReplaceAll(key, `\"`, `"`)
	key = strings.ReplaceAll(key, `\'`, `'`)

	return strings.ReplaceAll(key, `\\`, `\`)
}

// tomlCut is one byte-range replacement.
type tomlCut struct {
	start int
	end   int
	data  []byte
}

// applyTOMLCuts applies sorted, non-overlapping cuts to data.
func applyTOMLCuts(data []byte, cuts []tomlCut) []byte {
	slices.SortStableFunc(cuts, func(a, b tomlCut) int { return a.start - b.start })

	out := make([]byte, 0, len(data))
	cursor := 0

	for _, cut := range cuts {
		if cut.start < cursor {
			continue
		}

		out = append(out, data[cursor:cut.start]...)
		out = append(out, cut.data...)

		cursor = cut.end
	}

	return append(out, data[cursor:]...)
}

// tomlLineIndex maps 1-based line numbers to byte offsets.
type tomlLineIndex struct {
	data   []byte
	starts []int
}

// newTOMLIndex builds a line index.
func newTOMLIndex(data []byte) tomlLineIndex {
	starts := []int{0}

	for i, b := range data {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}

	return tomlLineIndex{data: data, starts: starts}
}

// line returns the text of a 1-based line.
func (ix tomlLineIndex) line(n int) string {
	start := ix.offset(n)
	end := len(ix.data)

	if n < len(ix.starts) {
		end = ix.starts[n]
	}

	return string(ix.data[start:end])
}

// offset returns the offset of a 1-based line.
func (ix tomlLineIndex) offset(line int) int {
	if line <= 1 {
		return 0
	}

	if line-1 >= len(ix.starts) {
		return len(ix.data)
	}

	return ix.starts[line-1]
}

// tomlTableSpan is one table header and the range it owns.
type tomlTableSpan struct {
	path      []string
	line      int
	start     int // offset of the header line start
	bodyStart int // offset just after the header line
	next      int // offset of the next table header, or len(data)
}

// scanTOMLTables records every single-bracket table header of a document.
// Array-of-tables headers are skipped: the editor refuses to rewrite them.
func scanTOMLTables(data []byte) []tomlTableSpan {
	index := newTOMLIndex(data)

	var tables []tomlTableSpan

	for line := 1; line <= len(index.starts); line++ {
		text := strings.TrimLeft(index.line(line), " \t")

		if !strings.HasPrefix(text, "[") || strings.HasPrefix(text, "[[") {
			continue
		}

		path, ok := parseTOMLHeader(text)
		if !ok {
			continue
		}

		tables = append(tables, tomlTableSpan{path: path, line: line, start: index.offset(line)})
	}

	for i := range tables {
		tables[i].bodyStart = afterLine(data, tables[i].start)
		tables[i].next = len(data)

		if i+1 < len(tables) {
			tables[i].next = tables[i+1].start
		}
	}

	return tables
}

// parseTOMLHeader parses "[a.b]" into its key segments.
func parseTOMLHeader(text string) ([]string, bool) {
	body, ok := strings.CutPrefix(text, "[")
	if !ok {
		return nil, false
	}

	content, _, ok := strings.Cut(body, "]")
	if !ok {
		return nil, false
	}

	content = strings.TrimSpace(content)
	if content == "" || strings.HasPrefix(content, "[") {
		return nil, false
	}

	return splitTOMLPath(content)
}

// splitTOMLPath splits a dotted key path, honoring quoted segments.
func splitTOMLPath(content string) ([]string, bool) {
	var (
		segments []string
		current  strings.Builder
		quote    byte
	)

	for i := range len(content) {
		c := content[i]

		switch {
		case quote != 0:
			if c == quote {
				quote = 0

				continue
			}

			current.WriteByte(c)
		case c == '"' || c == '\'':
			quote = c
		case c == '.':
			segment := strings.TrimSpace(current.String())
			if segment == "" {
				return nil, false
			}

			segments = append(segments, segment)

			current.Reset()
		default:
			current.WriteByte(c)
		}
	}

	if quote != 0 {
		return nil, false
	}

	segment := strings.TrimSpace(current.String())
	if segment == "" {
		return nil, false
	}

	return append(segments, segment), true
}

// findTOMLTable returns the table with exactly the path.
func findTOMLTable(tables []tomlTableSpan, path []string) (tomlTableSpan, bool) {
	for _, table := range tables {
		if slices.Equal(table.path, path) {
			return table, true
		}
	}

	return tomlTableSpan{}, false
}

// tomlContentEnd trims trailing blank and comment lines of a range.
func tomlContentEnd(data []byte, from, to int) int {
	end := to

	for end > from {
		start := previousLineStart(data, end)

		text := strings.TrimSpace(string(data[start:end]))
		if text != "" && !strings.HasPrefix(text, "#") {
			break
		}

		end = start
	}

	return end
}

// tomlBlankEnd extends end over the blank lines that follow it up to next.
func tomlBlankEnd(data []byte, end, next int) int {
	for end < next {
		lineEnd := bytes.IndexByte(data[end:], '\n')
		if lineEnd < 0 {
			lineEnd = len(data)
		} else {
			lineEnd += end
		}

		if strings.TrimSpace(string(data[end:lineEnd])) != "" {
			break
		}

		end = lineEnd
		if end < len(data) {
			end++
		}
	}

	return min(end, next)
}

// previousLineStart returns the offset of the line that contains end-1.
func previousLineStart(data []byte, end int) int {
	if end <= 0 {
		return 0
	}

	idx := bytes.LastIndexByte(data[:end-1], '\n')
	if idx < 0 {
		return 0
	}

	return idx + 1
}

// tomlEOL returns the line ending of a document.
func tomlEOL(data []byte) string {
	if bytes.Contains(data, []byte("\r\n")) {
		return "\r\n"
	}

	return "\n"
}

// endsWithBlankLine reports whether the buffer ends with an empty line.
func endsWithBlankLine(data []byte, eol string) bool {
	for _, blank := range []string{"\n\n", "\n\r\n", "\r\n\n", eol + eol} {
		if bytes.HasSuffix(data, []byte(blank)) {
			return true
		}
	}

	return false
}

// renderTOMLTable renders a table block for a key path: the header, the scalar
// keys sorted, and nested tables inline.
func renderTOMLTable(path []string, values map[string]any, eol string) ([]byte, error) {
	var b strings.Builder

	b.WriteString("[")
	b.WriteString(tomlKeyPath(path))
	b.WriteString("]")
	b.WriteString(eol)

	for _, key := range sortedKeys(values) {
		value, err := tomlValue(values[key])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}

		b.WriteString(tomlKey(key))
		b.WriteString(" = ")
		b.WriteString(value)
		b.WriteString(eol)
	}

	return []byte(b.String()), nil
}

// renderTOMLAssignment renders one `key = value` line.
func renderTOMLAssignment(key string, value any, eol string) ([]byte, error) {
	rendered, err := tomlValue(value)
	if err != nil {
		return nil, err
	}

	return []byte(tomlKey(key) + " = " + rendered + eol), nil
}

// tomlLookup resolves a dotted path in a decoded TOML document.
func tomlLookup(doc map[string]any, path []string) (any, bool) {
	var current any = doc

	for _, segment := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}

		current, ok = object[segment]
		if !ok {
			return nil, false
		}
	}

	return current, true
}
