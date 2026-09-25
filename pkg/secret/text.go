package secret

import (
	"bytes"
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// TextHit describes one secret-like span found in free text.
type TextHit struct {
	Start int
	End   int
	Name  string
	Value string
}

const (
	minTokenLen = 16
	minValueLen = 8
	redacted    = "[redacted]"
)

var (
	refTextRE   = regexp.MustCompile(`\{secret:([A-Za-z_][A-Za-z0-9_]*)\}`)
	hostRE      = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?\.[A-Za-z]{2,}$`)
	numberRE    = regexp.MustCompile(`^[0-9]+$`)
	trimmedRefs = map[string]string{"bearer ": "", "token ": "", "basic ": ""}
)

// textSpan is an accepted hit span used for overlap resolution.
type textSpan struct {
	start int
	end   int
}

// ScanText finds secret-like spans in free text without modifying it.
func ScanText(data []byte) []TextHit {
	protected := protectedSpans(data)

	hits := slices.Concat(pemHits(data), hintHits(data), keyHits(data))

	slices.SortFunc(hits, func(a, b TextHit) int {
		return cmp.Or(cmp.Compare(a.Start, b.Start), cmp.Compare(b.End, a.End))
	})

	accepted := make([]TextHit, 0, len(hits))
	spans := make([]textSpan, 0, len(hits)+len(protected))

	for _, hit := range hits {
		span := textSpan{start: hit.Start, end: hit.End}
		if overlapsAny(span, protected) || overlapsAny(span, spans) {
			continue
		}

		accepted = append(accepted, hit)
		spans = append(spans, span)
	}

	return accepted
}

// ExtractText moves secret-like spans of free text into the store and replaces
// them with references, returning the extracted names.
func ExtractText(data []byte, store *Store) ([]byte, []string, bool, error) {
	hits := ScanText(data)
	if len(hits) == 0 {
		return data, nil, false, nil
	}

	var out bytes.Buffer

	names := make([]string, 0, len(hits))
	prev := 0

	for _, hit := range hits {
		out.Write(data[prev:hit.Start])

		name := store.nameFor(cmp.Or(hit.Name, "SECRET"), hit.Value)
		store.Set(name, hit.Value)

		out.WriteString(Ref(name))

		names = append(names, name)
		prev = hit.End
	}

	out.Write(data[prev:])

	return out.Bytes(), names, true, nil
}

// ResolveText expands secret references in free text, rendering missing names
// as [redacted] and returning them.
func ResolveText(data []byte, store *Store) ([]byte, []string) {
	if !bytes.Contains(data, []byte(refPrefix)) {
		return data, nil
	}

	missing := map[string]struct{}{}

	out := refTextRE.ReplaceAllFunc(data, func(match []byte) []byte {
		name, _ := ParseRef(string(match))

		value, ok := store.Get(name)
		if !ok {
			missing[name] = struct{}{}

			return []byte(redacted)
		}

		return []byte(value)
	})

	return out, slices.Sorted(maps.Keys(missing))
}

// RefsText returns the sorted secret names referenced by free text.
func RefsText(data []byte) []string {
	names := map[string]struct{}{}

	for _, match := range refTextRE.FindAllSubmatch(data, -1) {
		names[string(match[1])] = struct{}{}
	}

	return slices.Sorted(maps.Keys(names))
}

// protectedSpans returns the spans of existing refs, which are never rescanned.
func protectedSpans(data []byte) []textSpan {
	var spans []textSpan

	for _, prefix := range []string{refPrefix, envPrefix} {
		start := 0

		for {
			pos := bytes.Index(data[start:], []byte(prefix))
			if pos < 0 {
				break
			}

			at := start + pos
			end := len(data)

			if closeAt := bytes.IndexByte(data[at:], '}'); closeAt >= 0 {
				end = at + closeAt + 1
			} else if lineEnd := bytes.IndexByte(data[at:], '\n'); lineEnd >= 0 {
				end = at + lineEnd
			}

			spans = append(spans, textSpan{start: at, end: end})

			start = end
		}
	}

	return spans
}

// overlapsAny reports whether span intersects any of spans.
func overlapsAny(span textSpan, spans []textSpan) bool {
	for _, other := range spans {
		if span.start < other.end && other.start < span.end {
			return true
		}
	}

	return false
}

// pemHits finds whole PEM private-key blocks.
func pemHits(data []byte) []TextHit {
	lower := lowerASCII(data)

	var hits []TextHit

	start := 0

	for {
		beginAt := bytes.Index(lower[start:], []byte("-----begin "))
		if beginAt < 0 {
			break
		}

		begin := start + beginAt
		if !privateKeyLine(data, begin) {
			start = begin + 1

			continue
		}

		endAt := bytes.Index(lower[begin:], []byte("-----end "))
		if endAt < 0 {
			break
		}

		endLine := begin + endAt
		if !privateKeyLine(data, endLine) {
			start = endLine + 1

			continue
		}

		end := lineEnd(data, endLine)

		hits = append(hits, TextHit{Start: begin, End: end, Value: string(data[begin:end])})
		start = end
	}

	return hits
}

// privateKeyLine reports whether the line starting at offset at names a
// private key.
func privateKeyLine(data []byte, at int) bool {
	line := data[at:lineEnd(data, at)]

	return bytes.Contains(bytes.ToLower(line), []byte("private key"))
}

// lineEnd returns the index of the newline at or after at, or len(data).
func lineEnd(data []byte, at int) int {
	if idx := bytes.IndexByte(data[at:], '\n'); idx >= 0 {
		return at + idx
	}

	return len(data)
}

// hintHits finds token values introduced by a known prefix (bearer, sk-, …).
func hintHits(data []byte) []TextHit {
	lower := lowerASCII(data)

	var hits []TextHit

	for _, hint := range valueHints {
		if hint == "-----begin " {
			continue
		}

		_, trimmed := trimmedRefs[hint]

		start := 0

		for {
			pos := bytes.Index(lower[start:], []byte(hint))
			if pos < 0 {
				break
			}

			at := start + pos
			valueStart := at

			if trimmed {
				valueStart = at + len(hint)
			}

			valueEnd := valueStart
			for valueEnd < len(data) && isTokenChar(data[valueEnd]) {
				valueEnd++
			}

			if valueEnd-valueStart >= minTokenLen && boundaryBefore(data, at) {
				hits = append(hits, TextHit{Start: valueStart, End: valueEnd, Value: string(data[valueStart:valueEnd])})
			}

			start = cmp.Or(valueEnd, at+1)
		}
	}

	return hits
}

// keyHits finds key=value and key: value pairs whose key looks secret-ish.
func keyHits(data []byte) []TextHit {
	var hits []TextHit

	lineStart := 0

	for lineStart <= len(data) {
		line := lineEnd(data, lineStart) - lineStart
		lineText := data[lineStart : lineStart+line]

		for _, sep := range separators(lineText) {
			key := keyToken(string(lineText[:sep]))
			if !keyHint(key) {
				continue
			}

			value, start, end, ok := keyValue(lineText, sep)
			if !ok {
				continue
			}

			hits = append(hits, TextHit{
				Start: lineStart + start,
				End:   lineStart + end,
				Name:  NormalizeName(key),
				Value: value,
			})

			break
		}

		if lineStart+line >= len(data) {
			break
		}

		lineStart += line + 1
	}

	return hits
}

// separators returns the positions of '=' and ':' in line.
func separators(line []byte) []int {
	var out []int

	for i, b := range line {
		if b == '=' || b == ':' {
			out = append(out, i)
		}
	}

	return out
}

// keyToken returns the trailing key-shaped run of a key prefix.
func keyToken(raw string) string {
	raw = strings.TrimRight(raw, " \t\"'")

	start := len(raw)
	for start > 0 && isKeyChar(raw[start-1]) {
		start--
	}

	return raw[start:]
}

// isKeyChar reports whether b may appear in a key token.
func isKeyChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_' || b == '-' || b == '.':
		return true
	}

	return false
}

// keyValue extracts the value after sep, quoted or bare.
func keyValue(line []byte, sep int) (string, int, int, bool) {
	start := valueStart(line, sep)
	if start >= len(line) {
		return "", 0, 0, false
	}

	if quote := line[start]; quote == '"' || quote == '\'' {
		return quotedValue(line, start, quote)
	}

	return bareValue(line, start)
}

// valueStart skips the separator and following blanks.
func valueStart(line []byte, sep int) int {
	start := sep + 1

	for start < len(line) && (line[start] == ' ' || line[start] == '\t') {
		start++
	}

	return start
}

// quotedValue extracts a quote-delimited value.
func quotedValue(line []byte, start int, quote byte) (string, int, int, bool) {
	end := lineEnd(line, start)

	closeAt := bytes.IndexByte(line[start+1:end], quote)
	if closeAt < 0 {
		return "", 0, 0, false
	}

	value := string(line[start+1 : start+1+closeAt])

	return value, start + 1, start + 1 + closeAt, validTextValue(value)
}

// bareValue extracts an unquoted value up to the end of the line.
func bareValue(line []byte, start int) (string, int, int, bool) {
	end := lineEnd(line, start)

	for end > start && (line[end-1] == ' ' || line[end-1] == '\t' || line[end-1] == '\r') {
		end--
	}

	if end > start && (line[end-1] == '\'' || line[end-1] == '"') {
		end--
	}

	value := string(line[start:end])

	return value, start, end, validTextValue(value)
}

// validTextValue rejects values that are not literals worth extracting.
func validTextValue(value string) bool {
	switch {
	case len(value) < minValueLen, value == redacted:
		return false
	case IsRef(value), strings.HasPrefix(value, envPrefix), strings.HasPrefix(value, "${"):
		return false
	case strings.ContainsRune(value, '{'):
		return false
	case startsWithValueHint(value):
		return false
	case numberRE.MatchString(value):
		return false
	case strings.Contains(value, "://"), hostRE.MatchString(value):
		return false
	}

	return true
}

// startsWithValueHint reports whether value begins with a known token prefix.
func startsWithValueHint(value string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(value))

	return slices.ContainsFunc(valueHints, func(hint string) bool {
		return hint != "-----begin " && strings.HasPrefix(trimmed, hint)
	})
}

// keyHint reports whether a bare key looks secret-ish.
func keyHint(key string) bool {
	return matchKeyHint(key)
}

// boundaryBefore reports whether at starts a token.
func boundaryBefore(data []byte, at int) bool {
	if at == 0 {
		return true
	}

	return !isTokenChar(data[at-1])
}

// isTokenChar reports whether b may appear inside a token value.
func isTokenChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_' || b == '-' || b == '.' || b == '/' || b == '+' || b == '=':
		return true
	}

	return false
}

// lowerASCII lowercases ASCII bytes without touching multibyte runes.
func lowerASCII(data []byte) []byte {
	out := make([]byte, len(data))

	for i, b := range data {
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}

		out[i] = b
	}

	return out
}
