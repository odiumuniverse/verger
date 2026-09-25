package render

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// checkTOMLParents verifies that every existing prefix of a dotted path is a
// table, so a create never overwrites a scalar.
func checkTOMLParents(doc map[string]any, path []string) error {
	var current any = doc

	for i, segment := range path[:len(path)-1] {
		object, ok := current.(map[string]any)
		if !ok {
			return fmt.Errorf("cannot create %q below a non-table value", strings.Join(path[:i+1], "."))
		}

		child, ok := object[segment]
		if !ok {
			return nil
		}

		current = child
	}

	if _, ok := current.(map[string]any); !ok {
		return fmt.Errorf("cannot create %q below a non-table value", strings.Join(path[:len(path)-1], "."))
	}

	return nil
}

// tomlRemoval returns the cut that removes the table or assignment defining a
// dotted path.
func tomlRemoval(file []byte, spans tomlSpanScan, tables []tomlTableSpan, path []string) (tomlCut, bool) {
	if table, ok := findTOMLTable(tables, path); ok {
		contentEnd := tomlContentEnd(file, table.bodyStart, table.next)
		end := tomlBlankEnd(file, contentEnd, table.next)

		return tomlCut{start: table.start, end: end}, true
	}

	if span, ok := findTopLevelAssignment(spans, path); ok {
		return tomlCut{start: span.lineStart, end: span.end}, true
	}

	if len(path) >= 2 {
		if table, ok := findTOMLTable(tables, path[:len(path)-1]); ok {
			if span, ok := findTableAssignment(file, table, path[len(path)-1]); ok {
				return tomlCut{start: span.lineStart, end: span.end}, true
			}
		}
	}

	return tomlCut{}, false
}

// tomlPlacement returns the cuts that write a value at a dotted path: a table
// block for map values, an assignment otherwise.
func tomlPlacement(file []byte, spans tomlSpanScan, tables []tomlTableSpan, path []string, value any, eol string) ([]tomlCut, error) {
	table, isMap := value.(map[string]any)

	attempts := []func() ([]tomlCut, bool, error){
		func() ([]tomlCut, bool, error) {
			return placementExactTable(file, tables, path, table, isMap, value, eol)
		},
		func() ([]tomlCut, bool, error) {
			return placementTopAssignment(file, spans, path, table, isMap, value, eol)
		},
		func() ([]tomlCut, bool, error) {
			return placementTableKey(file, tables, path, table, isMap, value, eol)
		},
		func() ([]tomlCut, bool, error) { return placementNew(file, spans, path, table, isMap, value, eol) },
	}

	for _, attempt := range attempts {
		cuts, ok, err := attempt()
		if err != nil {
			return nil, err
		}

		if ok {
			return cuts, nil
		}
	}

	return nil, errors.New("the value has no place in the document")
}

// placementExactTable replaces the table with exactly this path.
func placementExactTable(file []byte, tables []tomlTableSpan, path []string, table map[string]any, isMap bool, value any, eol string) ([]tomlCut, bool, error) {
	span, ok := findTOMLTable(tables, path)
	if !ok {
		return nil, false, nil
	}

	end := tomlContentEnd(file, span.bodyStart, span.next)

	if isMap {
		block, err := renderTOMLTable(path, table, eol)
		if err != nil {
			return nil, false, err
		}

		return []tomlCut{{start: span.start, end: end, data: block}}, true, nil
	}

	line, err := renderTOMLAssignmentPath(path, value, eol)
	if err != nil {
		return nil, false, err
	}

	return []tomlCut{{start: span.start, end: end, data: line}}, true, nil
}

// placementTopAssignment replaces a top-level (dotted) assignment.
func placementTopAssignment(file []byte, spans tomlSpanScan, path []string, table map[string]any, isMap bool, value any, eol string) ([]tomlCut, bool, error) {
	_ = file

	span, ok := findTopLevelAssignment(spans, path)
	if !ok {
		return nil, false, nil
	}

	cuts, err := placementAssignmentReplace(span, path, table, isMap, value, eol)
	if err != nil {
		return nil, false, err
	}

	return cuts, true, nil
}

// placementTableKey replaces or inserts a key inside an existing parent table.
func placementTableKey(file []byte, tables []tomlTableSpan, path []string, table map[string]any, isMap bool, value any, eol string) ([]tomlCut, bool, error) {
	if len(path) < 2 {
		return nil, false, nil
	}

	parent, ok := findTOMLTable(tables, path[:len(path)-1])
	if !ok {
		return nil, false, nil
	}

	if span, ok := findTableAssignment(file, parent, path[len(path)-1]); ok {
		cuts, err := placementAssignmentReplace(span, path, table, isMap, value, eol)
		if err != nil {
			return nil, false, err
		}

		return cuts, true, nil
	}

	inside, err := tomlInsideCut(file, parent, path, value, eol, isMap, table)
	if err != nil {
		return nil, false, err
	}

	return []tomlCut{inside}, true, nil
}

// placementNew writes a value the document does not hold yet.
func placementNew(file []byte, spans tomlSpanScan, path []string, table map[string]any, isMap bool, value any, eol string) ([]tomlCut, bool, error) {
	if isMap {
		block, err := renderTOMLTable(path, table, eol)
		if err != nil {
			return nil, false, err
		}

		return []tomlCut{tomlAppendCut(file, block, eol)}, true, nil
	}

	if len(path) == 1 {
		line, err := renderTOMLAssignment(path[0], value, eol)
		if err != nil {
			return nil, false, err
		}

		offset := tomlInsertOffset(file, spans)

		if offset > 0 && file[offset-1] != '\n' {
			line = append([]byte(eol), line...)
		}

		return []tomlCut{{start: offset, end: offset, data: line}}, true, nil
	}

	line, err := renderTOMLAssignment(path[len(path)-1], value, eol)
	if err != nil {
		return nil, false, err
	}

	block := []byte("[" + tomlKeyPath(path[:len(path)-1]) + "]" + eol)
	block = append(block, line...)

	return []tomlCut{tomlAppendCut(file, block, eol)}, true, nil
}

// placementAssignmentReplace rewrites one assignment: a table block for map
// values, a value-span replacement otherwise.
func placementAssignmentReplace(span tomlKeySpan, path []string, table map[string]any, isMap bool, value any, eol string) ([]tomlCut, error) {
	if isMap {
		block, err := renderTOMLTable(path, table, eol)
		if err != nil {
			return nil, err
		}

		return []tomlCut{{start: span.lineStart, end: span.end, data: block}}, nil
	}

	rendered, err := tomlValue(value)
	if err != nil {
		return nil, err
	}

	return []tomlCut{{start: span.valueStart, end: span.valueEnd, data: []byte(rendered)}}, nil
}

// tomlInsideCut inserts a value inside an existing parent table.
func tomlInsideCut(file []byte, parent tomlTableSpan, path []string, value any, eol string, isMap bool, table map[string]any) (tomlCut, error) {
	offset := tomlContentEnd(file, parent.bodyStart, parent.next)

	var data []byte

	if isMap {
		block, err := renderTOMLTable(path, table, eol)
		if err != nil {
			return tomlCut{}, err
		}

		data = block
	} else {
		line, err := renderTOMLAssignment(path[len(path)-1], value, eol)
		if err != nil {
			return tomlCut{}, err
		}

		data = line
	}

	if offset > 0 && file[offset-1] != '\n' {
		data = append([]byte(eol), data...)
	}

	return tomlCut{start: offset, end: offset, data: data}, nil
}

// findTopLevelAssignment returns the top-level assignment with the dotted key.
func findTopLevelAssignment(spans tomlSpanScan, path []string) (tomlKeySpan, bool) {
	want := tomlKeyPath(path)

	for _, span := range spans.spans {
		if span.key == want {
			return span, true
		}
	}

	return tomlKeySpan{}, false
}

// findTableAssignment returns the assignment of a key inside a table.
func findTableAssignment(file []byte, table tomlTableSpan, key string) (tomlKeySpan, bool) {
	index := newTOMLIndex(file)

	for line := table.line + 1; line <= len(index.starts); line++ {
		offset := index.offset(line)
		if offset >= table.next {
			break
		}

		text := index.line(line)
		trimmed := strings.TrimLeft(text, " \t")

		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "[") {
			continue
		}

		name, afterKey, ok := scanTOMLKey([]byte(trimmed))
		if !ok || name != key {
			continue
		}

		eq := bytes.IndexByte(afterKey, '=')
		if eq < 0 {
			continue
		}

		valueStart := offset + len(trimmed) - len(afterKey) + eq + 1
		valueStart += leadingSpace(file[valueStart:])

		valueEnd, end := scanTOMLValue(file, valueStart)

		return tomlKeySpan{key: name, lineStart: offset, valueStart: valueStart, valueEnd: valueEnd, end: end}, true
	}

	return tomlKeySpan{}, false
}

// renderTOMLAssignmentPath renders a dotted-key assignment line.
func renderTOMLAssignmentPath(path []string, value any, eol string) ([]byte, error) {
	rendered, err := tomlValue(value)
	if err != nil {
		return nil, err
	}

	return []byte(tomlKeyPath(path) + " = " + rendered + eol), nil
}

// tomlInsertOffset returns where a new top-level key belongs.
func tomlInsertOffset(file []byte, spans tomlSpanScan) int {
	if len(spans.spans) > 0 {
		return spans.spans[len(spans.spans)-1].end
	}

	return tomlTopLevelStart(file, spans.tableStart)
}

// tomlTopLevelStart returns the offset of the first meaningful line before the
// first table header, so inserted keys land after leading comments.
func tomlTopLevelStart(data []byte, tableStart int) int {
	pos := tomlStart(data)

	for pos < tableStart {
		lineEnd := bytes.IndexByte(data[pos:], '\n')
		if lineEnd < 0 {
			lineEnd = len(data)
		} else {
			lineEnd += pos
		}

		trimmed := bytes.TrimSpace(data[pos:lineEnd])
		if len(trimmed) > 0 && trimmed[0] != '#' {
			return pos
		}

		pos = lineEnd
		if pos < len(data) {
			pos++
		}
	}

	return tableStart
}

// tomlAppendCut appends a block at the end of the document, one blank line
// apart from the existing content.
func tomlAppendCut(file, block []byte, eol string) tomlCut {
	prefix := []byte{}

	if len(file) > 0 {
		if file[len(file)-1] != '\n' {
			prefix = append(prefix, eol...)
		}

		if !endsWithBlankLine(append(append([]byte{}, file...), prefix...), eol) {
			prefix = append(prefix, eol...)
		}
	}

	return tomlCut{start: len(file), end: len(file), data: append(prefix, block...)}
}
