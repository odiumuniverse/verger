package spec

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// edit is one byte range of the document and the bytes that replace it. An
// edit with no text removes the range.
type edit struct {
	span span
	text string
}

// planner turns the difference between two models into the byte edits that
// carry the document from one to the other.
type planner struct {
	idx       *docIndex
	tree      map[string]any
	edits     []edit
	rewrite   map[*docTable]bool
	rewritten map[*docTable]bool
	inline    map[string]bool
}

// planSplice diffs two models against the document and returns the edits that
// carry the document to the second one. A table it cannot reach pointwise is
// re-encoded on its own; a difference it cannot express at all is reported as
// errCannotPreserve, which is a decision to write the canonical document
// rather than a failure.
func planSplice(
	idx *docIndex,
	before, after map[string]*modelTable,
	tree map[string]any,
	model *Spec,
) ([]edit, error) {
	p := &planner{
		idx:       idx,
		tree:      tree,
		rewrite:   make(map[*docTable]bool),
		rewritten: make(map[*docTable]bool),
		inline:    make(map[string]bool),
	}

	if !arrayOrderKept(idx, model) {
		return nil, errCannotPreserve
	}

	for _, key := range modelPaths(before, after) {
		if err := p.emit(key, before, after); err != nil {
			return nil, err
		}
	}

	for _, table := range slices.SortedFunc(maps.Keys(p.rewrite), compareTables(idx)) {
		if err := p.emitRewrite(table); err != nil {
			return nil, err
		}
	}

	return p.edits, nil
}

func compareTables(idx *docIndex) func(a, b *docTable) int {
	return func(a, b *docTable) int {
		return cmp.Compare(idx.tables[a.path.key()].block.start, idx.tables[b.path.key()].block.start)
	}
}

// emit plans one table of the model against the document.
func (p *planner) emit(key string, before, after map[string]*modelTable) error {
	bt, at := before[key], after[key]
	table := p.idx.tables[key]

	if table == nil {
		return p.emitLoose(bt, at, before, after)
	}

	if p.rewritten[table] {
		return nil
	}

	if at == nil {
		p.dropTable(table)

		return nil
	}

	return p.editTable(table, at, bt)
}

// emitLoose handles a table the document does not give a span of its own: it is
// written as an inline value, which the line holding that value rewrites whole,
// or it has been folded into the rewrite of the nearest table around it.
func (p *planner) emitLoose(bt, at *modelTable, before, after map[string]*modelTable) error {
	path := tablePath(bt, at)

	if anchor, _, ok := p.idx.inlineAnchor(path); ok {
		return p.emitInline(anchor, before, after)
	}

	if table, ok := p.idx.dottedAnchor(path); ok {
		// The document writes this table as dotted keys inside the table above
		// it, so a header beside it would be a second definition. The nearest
		// table that does have a span of its own is re-encoded instead.
		return p.markRewrite(table)
	}

	if at == nil || bt != nil {
		return nil
	}

	return p.emitNew(at)
}

// tablePath is the path of whichever of the two models has one.
func tablePath(tables ...*modelTable) tpath {
	for _, table := range tables {
		if table != nil {
			return table.path
		}
	}

	return nil
}

// emitNew places a table the model has gained and the document does not: into
// the table it belongs under, else where no array element can swallow it.
func (p *planner) emitNew(at *modelTable) error {
	near := p.idx.tables[at.path[:len(at.path)-1].key()]
	if near != nil && p.rewritten[near] {
		return nil
	}

	block, err := p.renderInsert(at.path, near)
	if err != nil {
		return err
	}

	pos := p.newTablePos(at.path, near)
	p.push(span{pos, pos}, p.idx.separator(pos)+string(block))

	return nil
}

// newTablePos is where a table with no place of its own goes: after the last
// element of its own array, or inside the table it belongs under, or after the
// last table that is not inside an array element — every header that follows
// one belongs to it.
func (p *planner) newTablePos(path tpath, near *docTable) int {
	if path.leaf().array {
		if pos, ok := p.idx.arrayEnd(path.leaf().key); ok {
			return pos
		}

		return p.idx.topLevelEnd()
	}

	if near != nil {
		return near.block.end
	}

	return p.idx.topLevelEnd()
}

// editTable plans the value-level edits of a table the document and the model
// both have. The keys of both models are walked, not only the new one: a key
// the model has dropped is exactly the key that has to leave the file.
func (p *planner) editTable(table *docTable, at, bt *modelTable) error {
	for _, key := range schemaKeys(at, bt) {
		old := bt.key(key.name)

		switch {
		case !key.has:
			if old != nil && old.has {
				p.dropKey(table, key.name)
			}
		case old == nil || !old.has:
			p.insertKey(table, key)
		case key.value != old.value:
			if err := p.changeValue(table, at.path, key); err != nil {
				return err
			}
		}
	}

	return nil
}

// schemaKeys lists the keys a table owns across two models: those the new one
// has, in schema order, then those only the old one had.
func schemaKeys(at, bt *modelTable) []modelKey {
	keys := slices.Clone(at.keys)

	for _, key := range bt.keys {
		if at.key(key.name) == nil {
			keys = append(keys, modelKey{name: key.name})
		}
	}

	return keys
}

// insertKey adds one key after the last line the table owns, in the quote style
// the file already uses.
func (p *planner) insertKey(table *docTable, key modelKey) {
	at := table.block.end
	p.push(span{at, at}, p.idx.prefix(at)+key.name+" = "+requoteInline(key.value, p.idx.tableQuote(table))+p.idx.nl)
}

// changeValue replaces what stands between the `=` and the end of the value,
// leaving the key, the spacing around it and any trailing comment in place.
func (p *planner) changeValue(table *docTable, path tpath, key modelKey) error {
	value, ok := p.valueSpan(table, key.name)
	if !ok {
		return p.markRewrite(table)
	}

	if inlineValueOf(key) {
		if child := p.idx.tables[path.child(key.name).key()]; child != nil {
			// The document gives this table a section of its own; the value
			// it would replace is not a value at all.
			return p.markRewrite(child)
		}
	}

	p.push(value, requoteInline(key.value, quoteIn(p.idx.src[value.start:value.end])))

	return nil
}

// inlineValueOf reports whether a value is a table written on one line, which
// is how an environment reaches the file.
func inlineValueOf(key modelKey) bool {
	return strings.HasPrefix(key.value, "{")
}

// valueSpan finds the bytes of a key's value, whether it was written as a plain
// key or as the last step of a dotted one.
func (p *planner) valueSpan(table *docTable, name string) (span, bool) {
	if key := table.key(name); key != nil {
		return key.value, true
	}

	if key := table.dottedKey(name); key != nil {
		return key.value, true
	}

	return span{}, false
}

// dropKey removes a key together with its trailing comment. A comment above it
// is not part of the line and stays where the user put it.
func (p *planner) dropKey(table *docTable, name string) {
	for _, keys := range [][]docKey{table.keys, table.dotted} {
		if key := findKey(keys, name); key != nil {
			p.push(key.line, "")

			return
		}
	}
}

// dropTable removes a table with everything it owns: its header, its keys and
// the comments that introduce it. The tables nested under it go with it, or they
// would fall into whatever element follows.
func (p *planner) dropTable(table *docTable) {
	p.push(span{table.lead.start, p.idx.subtreeEnd(table)}, "")
}

// markRewrite re-encodes one table canonically in place of pointwise edits: the
// table keeps its own comments and every other table keeps its bytes. An edit
// already planned inside it is dropped, because the rewrite carries the whole
// subtree.
func (p *planner) markRewrite(table *docTable) error {
	if table == p.idx.root {
		// The root table has no header to re-encode around, and rewriting it
		// would rewrite the file — the one thing never done to a document
		// that already exists.
		return errCannotPreserve
	}

	if p.rewrite[table] {
		return nil
	}

	p.rewrite[table] = true

	low, high := table.block.start, p.idx.subtreeEnd(table)
	p.edits = slices.DeleteFunc(p.edits, func(e edit) bool {
		return e.span.start >= low && e.span.end <= high
	})

	for _, other := range p.idx.tables {
		if other == table || isUnder(other.path, table.path) {
			p.rewritten[other] = true
		}
	}

	return nil
}

func (p *planner) emitRewrite(table *docTable) error {
	block, err := p.renderRewrite(table.path, table)
	if err != nil {
		return err
	}

	p.push(span{table.block.start, p.idx.subtreeEnd(table)}, string(block))

	return nil
}

// emitInline rewrites an inline value whole, which is the finest unit an edit
// can address: the keys inside it are not lines of their own and were never
// indexed as such.
func (p *planner) emitInline(anchor tpath, before, after map[string]*modelTable) error {
	key := anchor.key()
	if p.inline[key] {
		return nil
	}

	p.inline[key] = true

	if subtreeEqual(key, before, after) {
		return nil
	}

	_, value, ok := p.idx.inlineAnchor(anchor)
	if !ok {
		return nil
	}

	rendered, err := inlineAt(p.tree, anchor)
	if err != nil {
		return err
	}

	p.push(value, requoteInline(rendered, quoteIn(p.idx.src[value.start:value.end])))

	return nil
}

// render encodes one table of the model on its own.
func (p *planner) render(path tpath, near *docTable) ([]byte, error) {
	reduced, err := reduceAt(p.tree, path)
	if err != nil {
		return nil, err
	}

	block, err := toml.Marshal(reduced)
	if err != nil {
		return nil, fmt.Errorf("render spec table %s: %w", path.name(), err)
	}

	return []byte(requoteBlock(block, p.quoteStyle(near))), nil
}

// renderInsert encodes a table that is being added beside the ones already in
// the document. Its own header is kept; the headers above it are dropped when
// the document already declares them, because a table may not be defined twice.
func (p *planner) renderInsert(path tpath, near *docTable) ([]byte, error) {
	block, err := p.render(path, near)
	if err != nil {
		return nil, err
	}

	return p.stripKnown(path, block), nil
}

// renderRewrite encodes a table that replaces itself: its own headers are part
// of the bytes being replaced, so none of them is dropped.
func (p *planner) renderRewrite(path tpath, near *docTable) ([]byte, error) {
	return p.render(path, near)
}

func (p *planner) quoteStyle(near *docTable) byte {
	if near != nil {
		if style := p.idx.tableQuote(near); style != 0 {
			return style
		}
	}

	return p.idx.documentQuote()
}

// stripKnown drops the header lines of a block that would re-open a table the
// document already declares. The block's own header is never one of them.
func (p *planner) stripKnown(path tpath, block []byte) []byte {
	want := path.raw()

	lines := strings.SplitAfter(string(block), "\n")
	kept := make([]string, 0, len(lines))

	for _, line := range lines {
		raw, ok := headerRawPath(line)
		if ok && raw != want && strings.HasPrefix(want, raw+".") && p.idx.headers[raw] {
			continue
		}

		kept = append(kept, line)
	}

	return []byte(strings.Join(kept, ""))
}

func (p *planner) push(at span, text string) {
	p.edits = append(p.edits, edit{span: at, text: text})
}

// applyEdits writes the edits into the document in one pass, so an offset never
// has to survive another edit's shift.
func applyEdits(src []byte, edits []edit) ([]byte, error) {
	ordered := slices.Clone(edits)
	slices.SortStableFunc(ordered, func(a, b edit) int { return cmp.Compare(a.span.start, b.span.start) })

	out := make([]byte, 0, len(src)+len(ordered)*8)
	last := 0

	for _, e := range ordered {
		if e.span.start < last || e.span.end < e.span.start || e.span.end > len(src) {
			return nil, errCannotPreserve
		}

		out = append(out, src[last:e.span.start]...)
		out = append(out, e.text...)
		last = e.span.end
	}

	return append(out, src[last:]...), nil
}

// verifySplice proves that the bytes about to be written say what the model
// says. A splice is a set of byte edits derived from two models; if it ever
// produces a document that parses into something else, the user's file stays
// exactly as it was.
func verifySplice(data []byte, want *Spec) error {
	got, err := parse(data, "")
	if err != nil {
		return fmt.Errorf("written spec does not parse back: %w", err)
	}

	gotBytes, err := got.Marshal()
	if err != nil {
		return fmt.Errorf("written spec does not parse back: %w", err)
	}

	wantBytes, err := want.Marshal()
	if err != nil {
		return fmt.Errorf("written spec does not parse back: %w", err)
	}

	if string(gotBytes) != string(wantBytes) {
		return errors.New("written spec does not parse back into the model it was built from")
	}

	return nil
}

// arrayOrderKept reports whether the document's array elements stand in the
// order the model wants. A model that reordered its entries cannot be expressed
// as an edit of the file, only as a re-encode of it.
func arrayOrderKept(idx *docIndex, model *Spec) bool {
	for _, array := range []struct {
		name string
		in   []string
	}{
		{sourceTable, sourceNames(model)},
		{packageTable, packageIDs(model)},
	} {
		if !elementOrderKept(idx, array.name, array.in) {
			return false
		}
	}

	return true
}

func elementOrderKept(idx *docIndex, name string, inModel []string) bool {
	inFile := make([]string, 0, len(idx.arrays[name]))
	for _, table := range idx.arrays[name] {
		inFile = append(inFile, table.path.leaf().ident)
	}

	return slices.Equal(common(inFile, inModel), common(inModel, inFile))
}

// common keeps the elements both sides have, in the order of the receiver.
func common(order, others []string) []string {
	known := make(map[string]bool, len(others))
	for _, id := range others {
		known[id] = true
	}

	kept := make([]string, 0, len(order))

	for _, id := range order {
		if known[id] {
			kept = append(kept, id)
		}
	}

	return kept
}

func packageIDs(s *Spec) []string {
	ids := make([]string, 0, len(s.Packages))
	for _, pkg := range s.Packages {
		ids = append(ids, pkg.ID)
	}

	return ids
}

func sourceNames(s *Spec) []string {
	names := make([]string, 0, len(s.Sources))
	for _, src := range s.Sources {
		names = append(names, src.Name)
	}

	return names
}

// identityAt names the nth element of an array of tables in the document, so the
// index and the model address the same element whatever happened to the ones
// around it.
func identityAt(s *Spec, array string, ordinal int) string {
	switch array {
	case packageTable:
		if ordinal < len(s.Packages) {
			return s.Packages[ordinal].ID
		}
	case sourceTable:
		if ordinal < len(s.Sources) {
			return s.Sources[ordinal].Name
		}
	}

	return ""
}

func modelPaths(before, after map[string]*modelTable) []string {
	keys := make([]string, 0, len(before)+len(after))
	paths := make(map[string]tpath, len(before)+len(after))

	for key, table := range before {
		keys = append(keys, key)
		paths[key] = table.path
	}

	for key, table := range after {
		if _, ok := before[key]; !ok {
			keys = append(keys, key)
		}

		paths[key] = table.path
	}

	// The order decides bytes where two elements of one array land side by
	// side: they have to come out in the model's order, not in the order their
	// ids happen to sort.
	slices.SortFunc(keys, func(a, b string) int {
		switch {
		case lessPath(paths[a], paths[b]):
			return -1
		case lessPath(paths[b], paths[a]):
			return 1
		default:
			return 0
		}
	})

	return keys
}

// lessPath orders table paths the way the document holds them: by key, and
// within one array of tables by position.
func lessPath(a, b tpath) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i].key != b[i].key {
			return a[i].key < b[i].key
		}

		if a[i].array && b[i].array && a[i].idx != b[i].idx {
			return a[i].idx < b[i].idx
		}
	}

	return len(a) < len(b)
}

// reduceAt cuts the canonical tree down to one table and everything under it,
// so go-toml prints that table — headers and all — and nothing else.
func reduceAt(tree map[string]any, path tpath) (any, error) {
	if len(path) == 0 {
		return tree, nil
	}

	seg := path[0]

	if seg.array {
		list, ok := tree[seg.key].([]any)
		if !ok || seg.idx >= len(list) {
			return nil, errCannotPreserve
		}

		element, _ := list[seg.idx].(map[string]any)

		child, err := reduceAt(element, path[1:])
		if err != nil {
			return nil, err
		}

		return map[string]any{seg.key: []any{child}}, nil
	}

	table, ok := tree[seg.key].(map[string]any)
	if !ok {
		return nil, errCannotPreserve
	}

	child, err := reduceAt(table, path[1:])
	if err != nil {
		return nil, err
	}

	return map[string]any{seg.key: child}, nil
}

// inlineAt renders one table as the inline value a `key = { … }` line carries.
func inlineAt(tree map[string]any, path tpath) (string, error) {
	var node any = tree

	for _, seg := range path {
		table, ok := node.(map[string]any)
		if !ok {
			return "", errCannotPreserve
		}

		if seg.array {
			list, ok := table[seg.key].([]any)
			if !ok || seg.idx >= len(list) {
				return "", errCannotPreserve
			}

			node = list[seg.idx]

			continue
		}

		node, ok = table[seg.key]
		if !ok {
			return "", errCannotPreserve
		}
	}

	return inlineSubtree(node)
}

func inlineSubtree(value any) (string, error) {
	table, ok := value.(map[string]any)
	if !ok {
		return canonicalValue(value)
	}

	parts := make([]string, 0, len(table))

	for _, key := range slices.Sorted(maps.Keys(table)) {
		rendered, err := inlineSubtree(table[key])
		if err != nil {
			return "", err
		}

		parts = append(parts, key+" = "+rendered)
	}

	return "{" + strings.Join(parts, ", ") + "}", nil
}

// requoteBlock restates every literal string of a freshly encoded block, so a
// table written out of hand comes back in the file's own quotes.
func requoteBlock(block []byte, style byte) string {
	const opening = " = '"

	if style == 0 || style == '\'' {
		return string(block)
	}

	lines := strings.SplitAfter(string(block), "\n")

	for i, line := range lines {
		prefix, _, ok := strings.Cut(line, opening)
		if !ok {
			continue
		}

		rest := strings.TrimSuffix(line, "\n")
		if !strings.HasSuffix(rest, "'") {
			continue
		}

		body := rest[len(prefix)+len(opening) : len(rest)-1]
		if strings.Contains(body, "'") || strings.Contains(body, `\`) {
			continue
		}

		lines[i] = prefix + opening[:len(opening)-1] + `"` + body + "\"\n"
	}

	return strings.Join(lines, "")
}

// requoteInline restates every literal string inside an inline value, which is
// how `key = {a = 'x'}` comes back in the quotes the file already uses. A string
// that cannot be written that way — one holding the quote itself — keeps the
// spelling it came with.
func requoteInline(value string, style byte) string {
	if style == 0 || style == '\'' || !strings.ContainsRune(value, '\'') {
		return value
	}

	var out strings.Builder

	for i := 0; i < len(value); {
		quote := value[i]
		if quote != '\'' && quote != '"' {
			out.WriteByte(quote)

			i++

			continue
		}

		end := strings.IndexByte(value[i+1:], quote)
		if end < 0 {
			out.WriteString(value[i:])

			break
		}

		body := value[i+1 : i+1+end]
		if quote == '"' || strings.Contains(body, "'") || strings.Contains(body, `\`) {
			out.WriteString(value[i : i+2+end])
		} else {
			out.WriteByte('"')
			out.WriteString(body)
			out.WriteByte('"')
		}

		i += 2 + end
	}

	return out.String()
}

// quoteIn reports the quote character a raw value is written with.
func quoteIn(value []byte) byte {
	if i := bytes.IndexAny(value, "'\""); i >= 0 {
		return value[i]
	}

	return 0
}

// headerRawPath reads the key path of a header line, so a block rendered
// beside the document can be compared with the headers already in it.
func headerRawPath(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)

	switch {
	case strings.HasPrefix(trimmed, "[[") && strings.HasSuffix(trimmed, "]]"):
		trimmed = trimmed[2 : len(trimmed)-2]
	case strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]"):
		trimmed = trimmed[1 : len(trimmed)-1]
	default:
		return "", false
	}

	if strings.ContainsAny(trimmed, `"'`) {
		return "", false
	}

	return strings.TrimSpace(trimmed), true
}
