package spec

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// errCannotPreserve marks a change this splicer will not attempt. It is not a
// failure but a decision: the document is re-encoded the way verger always
// wrote it, so a caller never gets a worse answer than before and never gets a
// partly rewritten file.
var errCannotPreserve = errors.New("the change cannot be spliced into the existing document")

// span is a byte range in a document, [start, end).
type span struct {
	start, end int
}

// tseg is one step of a table path. An array step selects one element of an
// array of tables by its identity, which is what keeps an element addressable
// across an insertion or a removal: `package@acme/caveman` is the same table
// whether it is first or last in the file.
type tseg struct {
	key   string
	array bool
	idx   int
	ident string
}

// tpath is the path of a table from the document root. The root is the empty
// path.
type tpath []tseg

// key renders the path as a map key. The separator cannot occur in a key or an
// id, because an id may contain any of the printable ones.
func (p tpath) key() string {
	parts := make([]string, 0, len(p))

	for _, seg := range p {
		if seg.array {
			parts = append(parts, seg.key+"\x00"+seg.ident)

			continue
		}

		parts = append(parts, seg.key)
	}

	return strings.Join(parts, "\x00")
}

// name renders the path for a human: `package@acme/caveman/propagate`.
func (p tpath) name() string {
	parts := make([]string, 0, len(p))

	for _, seg := range p {
		if seg.array {
			parts = append(parts, seg.key+"@"+seg.ident)

			continue
		}

		parts = append(parts, seg.key)
	}

	return strings.Join(parts, ".")
}

// raw renders the path the way the document spells its headers, which is what
// tells one header from another without consulting the model.
func (p tpath) raw() string {
	parts := make([]string, 0, len(p))
	for _, seg := range p {
		parts = append(parts, seg.key)
	}

	return strings.Join(parts, ".")
}

func (p tpath) leaf() tseg {
	return p[len(p)-1]
}

func (p tpath) hasArray() bool {
	for _, seg := range p {
		if seg.array {
			return true
		}
	}

	return false
}

func (p tpath) child(keys ...string) tpath {
	return append(slices.Clone(p), plainSegs(keys)...)
}

// docKey is one `key = value` line of a table, with the byte range of the whole
// line and of the value inside it.
type docKey struct {
	parts []string
	line  span
	value span
}

func (k docKey) name() string {
	return strings.Join(k.parts, ".")
}

// docTable is one table of the document: its header, the block of lines that
// belong to it, and the keys written in it.
//
// The block ends at the last line the table owns and stops short of the blank
// lines and comments that introduce the next table: a comment standing directly
// above the next header belongs to that next table, so removing a table removes
// its own introduction and nothing above it.
type docTable struct {
	path   tpath
	header span
	block  span
	lead   span
	keys   []docKey
	dotted []docKey
}

// key returns the plain key with the name, or nil.
func (t *docTable) key(name string) *docKey {
	return findKey(t.keys, name)
}

// dottedKey returns the key written as `a.b.c = value`, or nil.
func (t *docTable) dottedKey(name string) *docKey {
	return findKey(t.dotted, name)
}

func findKey(keys []docKey, name string) *docKey {
	for i := range keys {
		if keys[i].name() == name {
			return &keys[i]
		}
	}

	return nil
}

func (t *docTable) addKey(key docKey) {
	if len(key.parts) == 1 {
		t.keys = append(t.keys, key)
	} else {
		t.dotted = append(t.dotted, key)
	}

	if key.line.end > t.block.end {
		t.block.end = key.line.end
	}
}

// docIndex is the byte-level map of one document: where every table and every
// value sits in the bytes the user wrote.
type docIndex struct {
	src     []byte
	nl      string
	root    *docTable
	tables  map[string]*docTable
	order   []*docTable
	headers map[string]bool
	arrays  map[string][]*docTable
}

// tableQuote is the quote character the values of one table are written with,
// or zero when the table says nothing about it.
func (idx *docIndex) tableQuote(table *docTable) byte {
	for _, key := range slices.Concat(table.keys, table.dotted) {
		if style := quoteIn(idx.src[key.value.start:key.value.end]); style != 0 {
			return style
		}
	}

	return 0
}

// documentQuote is the quote character the file as a whole is written with,
// which is the dialect a table added to it should be written in.
func (idx *docIndex) documentQuote() byte {
	for _, table := range idx.order {
		if style := idx.tableQuote(table); style != 0 {
			return style
		}
	}

	return 0
}

// buildIndex walks the document once with go-toml's comment-keeping parser and
// records where each table and each value sits. ident names the nth element of
// an array of tables, so the tables it produces are addressed the way the model
// addresses them: by identity rather than by position.
func buildIndex(src []byte, ident func(array string, ordinal int) string) (*docIndex, error) {
	idx := &docIndex{
		src:     src,
		nl:      newlineOf(src),
		tables:  make(map[string]*docTable),
		headers: make(map[string]bool),
		arrays:  make(map[string][]*docTable),
	}
	idx.root = &docTable{block: span{0, 0}}
	idx.tables[idx.root.path.key()] = idx.root
	idx.order = []*docTable{idx.root}

	var parser unstable.Parser

	parser.KeepComments = true
	parser.Reset(src)

	current := idx.root
	pos := 0

	for parser.NextExpression() {
		node := parser.Expression()

		switch node.Kind {
		case unstable.Comment:
			pos = lineEndOf(src, int(node.Raw.Offset))
		case unstable.KeyValue:
			finish := int(node.Raw.Offset) + int(node.Raw.Length)
			current.addKey(docKey{
				parts: keyPartsOf(node),
				line:  span{lineStartOf(src, int(node.Raw.Offset)), lineEndOf(src, finish)},
				value: span{valueStartOf(src, node), finish},
			})

			pos = current.block.end
		case unstable.Table, unstable.ArrayTable:
			table, end, err := idx.addTable(node, current, pos, ident)
			if err != nil {
				return nil, err
			}

			current = table
			pos = end
		default:
			return nil, fmt.Errorf("unexpected %s expression", node.Kind)
		}
	}

	if err := parser.Error(); err != nil {
		return nil, fmt.Errorf("index spec: %w", err)
	}

	return idx, nil
}

// addTable records the header that starts at or after pos and returns the table
// it opens together with the offset the next expression starts at.
func (idx *docIndex) addTable(
	node *unstable.Node,
	current *docTable,
	pos int,
	ident func(string, int) string,
) (*docTable, int, error) {
	offset := skipSpace(idx.src, pos)

	if offset >= len(idx.src) || idx.src[offset] != '[' {
		return nil, 0, fmt.Errorf("spec: expected a table header at byte %d", offset)
	}

	parts := keyPartsOf(node)

	path, array, err := idx.resolve(node.Kind, parts, current, ident)
	if err != nil {
		return nil, 0, err
	}

	if _, seen := idx.tables[path.key()]; seen {
		return nil, 0, fmt.Errorf("spec: table %s is defined twice", path.name())
	}

	start := lineStartOf(idx.src, offset)
	end := lineEndOf(idx.src, offset)
	table := &docTable{path: path, header: span{start, end}, block: span{start, end}}
	table.lead = span{leadStartOf(idx.src, start, current.block.end), start}

	if array != "" {
		idx.arrays[array] = append(idx.arrays[array], table)
	}

	idx.tables[path.key()] = table
	idx.order = append(idx.order, table)
	idx.headers[path.raw()] = true

	return table, end, nil
}

// prefix opens a line before an insertion when the insertion point is not
// already at the start of one, so an inserted line never runs into the line
// above it.
func (idx *docIndex) prefix(at int) string {
	if at == 0 || idx.src[at-1] == '\n' {
		return ""
	}

	return idx.nl
}

// separator is the blank line a table block needs before it: one, unless the
// document already has an empty line at that point.
func (idx *docIndex) separator(at int) string {
	if at == 0 || idx.src[at-1] != '\n' {
		return idx.nl
	}

	if at >= 2 && idx.src[at-2] == '\n' {
		return ""
	}

	return idx.nl
}

// resolve turns a header's key parts into a path. A header that names the array
// of the element it follows continues inside that element, which is what makes
// `package.propagate` under `[[package]]` a table of that package.
func (idx *docIndex) resolve(
	kind unstable.Kind,
	parts []string,
	current *docTable,
	ident func(string, int) string,
) (tpath, string, error) {
	if len(parts) == 0 {
		return nil, "", errors.New("spec: a table header has no key")
	}

	continues := current != idx.root && current.path.hasArray() &&
		parts[0] == current.path.leaf().key &&
		(kind == unstable.Table || len(parts) > 1)
	if continues {
		path := append(slices.Clone(current.path), plainSegs(parts[1:])...)

		return path, arrayOf(kind, parts), nil
	}

	if kind != unstable.ArrayTable {
		return plainSegs(parts), "", nil
	}

	name := parts[0]
	ordinal := len(idx.arrays[name])
	path := tpath{{key: name, array: true, idx: ordinal}}

	if ident != nil {
		path[0].ident = ident(name, ordinal)
	}

	if path[0].ident == "" {
		path[0].ident = "#" + strconv.Itoa(ordinal)
	}

	return append(path, plainSegs(parts[1:])...), name, nil
}

// arrayOf names the array a header element belongs to, so two elements of the
// same array never answer to the same table.
func arrayOf(kind unstable.Kind, parts []string) string {
	if kind != unstable.ArrayTable {
		return ""
	}

	return strings.Join(parts, ".")
}

func plainSegs(parts []string) []tseg {
	segs := make([]tseg, 0, len(parts))
	for _, part := range parts {
		segs = append(segs, tseg{key: part})
	}

	return segs
}

// leadStart walks back over the blank lines and comments that introduce a
// header, stopping at the end of the table before it.
func leadStartOf(src []byte, headerStart, floor int) int {
	start := headerStart

	for start > floor {
		lineStart := lineStartOf(src, start-1)
		if lineStart < floor || !isBlankOrComment(src[lineStart:start-1]) {
			break
		}

		start = lineStart
	}

	return start
}

func isBlankOrComment(line []byte) bool {
	trimmed := bytes.TrimLeft(line, " \t")
	if len(trimmed) == 0 {
		return true
	}

	return trimmed[0] == '#'
}

func lineStartOf(src []byte, offset int) int {
	if i := bytes.LastIndexByte(src[:offset], '\n'); i >= 0 {
		return i + 1
	}

	return 0
}

func lineEndOf(src []byte, offset int) int {
	if i := bytes.IndexByte(src[offset:], '\n'); i >= 0 {
		return offset + i + 1
	}

	return len(src)
}

// skipSpace walks over the whitespace the parser itself skips, so a seek for
// the next header lands exactly where the parser's own scan lands.
func skipSpace(src []byte, offset int) int {
	for offset < len(src) && strings.ContainsRune(" \t\r\n", rune(src[offset])) {
		offset++
	}

	return offset
}

// valueStartOf is where the value of a `key = value` expression begins. The
// parser's own value node cannot be asked: for an array or an inline table its
// range covers only the opening bracket, so the separator is read off the
// expression instead — outside a quoted key, the first `=` is the separator.
func valueStartOf(src []byte, node *unstable.Node) int {
	raw := src[node.Raw.Offset : node.Raw.Offset+node.Raw.Length]

	quote := byte(0)

	for i := 0; i < len(raw); i++ {
		c := raw[i]

		switch {
		case quote == '"' && c == '\\':
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '=':
			return skipSpace(src, int(node.Raw.Offset)+i+1)
		}
	}

	return int(node.Raw.Offset) + int(node.Raw.Length)
}

// newlineOf answers which line ending the document uses, so an inserted line is
// indistinguishable from the ones around it.
func newlineOf(src []byte) string {
	if i := bytes.IndexByte(src, '\n'); i > 0 && src[i-1] == '\r' {
		return "\r\n"
	}

	return "\n"
}

func keyPartsOf(node *unstable.Node) []string {
	iter := node.Key()

	var parts []string

	for iter.Next() {
		parts = append(parts, string(iter.Node().Data))
	}

	return parts
}

// inlineAnchor reports whether the document writes a table as an inline value
// on one line. The keys inside an inline table are not lines of their own and
// were never indexed as such, so that one value is the finest unit an edit can
// address.
func (idx *docIndex) inlineAnchor(path tpath) (tpath, span, bool) {
	if len(path) == 0 || path[0].array {
		return nil, span{}, false
	}

	key := idx.root.key(path[0].key)
	if key == nil || !isInlineTable(idx.src[key.value.start:key.value.end]) {
		return nil, span{}, false
	}

	return path[:1], key.value, true
}

// dottedAnchor reports whether the document writes a table as dotted keys
// inside the table above it: `package.propagate.install = "ask"` says as much
// about `package.propagate` as a header would, and a header may not be added
// beside it.
func (idx *docIndex) dottedAnchor(path tpath) (*docTable, bool) {
	want := path.raw()

	for i := 0; i <= len(path); i++ {
		table := idx.tables[path[:i].key()]
		if table == nil {
			continue
		}

		for _, key := range table.dotted {
			if key.name() == want || strings.HasPrefix(key.name(), want+".") {
				return table, true
			}
		}
	}

	return nil, false
}

// isInlineTable reports whether a value is written as `{ … }`, which makes the
// table it names one line instead of a block of its own.
func isInlineTable(value []byte) bool {
	trimmed := bytes.TrimLeft(value, " \t")

	return len(trimmed) > 0 && trimmed[0] == '{'
}

// topLevelEnd is the byte a new table can be appended at without landing inside
// an array element, where every header that follows would belong to it.
func (idx *docIndex) topLevelEnd() int {
	end := idx.root.block.end

	for _, table := range idx.order {
		if table.path.hasArray() {
			continue
		}

		if table.block.end > end {
			end = table.block.end
		}
	}

	return end
}

// arrayEnd is where a new element of an array of tables goes: after the last
// element already there, never inside one.
func (idx *docIndex) arrayEnd(name string) (int, bool) {
	elements := idx.arrays[name]
	if len(elements) == 0 {
		return 0, false
	}

	return elements[len(elements)-1].block.end, true
}

// subtreeEnd is the end of a table's block and of every block nested under it.
func (idx *docIndex) subtreeEnd(table *docTable) int {
	end := table.block.end

	for _, other := range idx.order {
		if !isUnder(other.path, table.path) {
			continue
		}

		if other.block.end > end {
			end = other.block.end
		}
	}

	return end
}

func isUnder(path, prefix tpath) bool {
	if len(path) <= len(prefix) {
		return false
	}

	return slices.Equal(path[:len(prefix)], prefix)
}

// modelKey is one schema key of a table: its name, the canonical bytes of its
// value, and whether the model has a value at all.
type modelKey struct {
	name  string
	value string
	has   bool
}

// modelTable is one addressable table of a spec model with the schema keys it
// owns, in schema order. A table owns few keys, so they are found by name in a
// scan rather than through an index that could fall out of step with the slice.
type modelTable struct {
	path tpath
	keys []modelKey
}

func newModelTable(path tpath, keys ...modelKey) *modelTable {
	return &modelTable{path: path, keys: keys}
}

func (t *modelTable) key(name string) *modelKey {
	for i := range t.keys {
		if t.keys[i].name == name {
			return &t.keys[i]
		}
	}

	return nil
}

// modelTables walks a spec into the tables the schema owns. A table the document
// spells some other way is absent here too, and the splicer reaches it through
// the document's own index.
func modelTables(s *Spec) (map[string]*modelTable, error) {
	out := make(map[string]*modelTable)

	root, err := modelRoot(s)
	if err != nil {
		return nil, err
	}

	out[root.path.key()] = root

	if err := addModelDefaults(out, root, s.Defaults); err != nil {
		return nil, err
	}

	addModelPolicy(out, nil, &s.Propagate)

	if err := addModelHosts(out, s.Hosts); err != nil {
		return nil, err
	}

	return out, addModelEntries(out, s)
}

func modelRoot(s *Spec) (*modelTable, error) {
	value, err := canonicalValue(s.Schema)
	if err != nil {
		return nil, err
	}

	return newModelTable(nil, modelKey{name: "schema", value: value, has: true}), nil
}

func addModelDefaults(out map[string]*modelTable, root *modelTable, defaults Defaults) error {
	if defaults.Hooks == "" && defaults.Cooldown == 0 && len(defaults.raw) == 0 {
		return nil
	}

	path := root.path.child(defaultsTable)
	table := newModelTable(path)

	if defaults.Hooks != "" {
		value, err := canonicalValue(string(defaults.Hooks))
		if err != nil {
			return err
		}

		table.keys = append(table.keys, modelKey{name: hooksKey, value: value, has: true})
	}

	if defaults.Cooldown != 0 {
		value, err := canonicalValue(defaults.Cooldown)
		if err != nil {
			return err
		}

		table.keys = append(table.keys, modelKey{name: cooldownKey, value: value, has: true})
	}

	out[path.key()] = table

	return nil
}

// addModelPolicy adds a propagation policy and, under it, one table per kind and
// per host override. A policy that says nothing owns no table: the model does
// not print an empty section either.
func addModelPolicy(out map[string]*modelTable, prefix tpath, policy *Propagate) {
	if policyEmpty(policy) {
		return
	}

	path := prefix.child("propagate")
	out[path.key()] = policyTable(path, policy)
	addModelLevels(out, path, "kind", policy.Kind)
	addModelLevels(out, path, "host", policy.Host)
}

// addModelLevels adds one table per name of a per-kind or per-host override
// table, and the tables nested under it.
func addModelLevels(out map[string]*modelTable, base tpath, level string, policies map[string]Propagate) {
	for _, name := range slices.Sorted(maps.Keys(policies)) {
		policy := policies[name]
		path := base.child(level, name)
		out[path.key()] = policyTable(path, &policy)
		addModelLevels(out, path, level, policy.Kind)
		addModelLevels(out, path, level, policy.Host)
	}
}

func policyTable(path tpath, policy *Propagate) *modelTable {
	table := newModelTable(path)

	for i, ev := range events {
		mode := policy.Mode(ev)
		if mode == "" {
			continue
		}

		table.keys = append(table.keys, modelKey{name: eventKeys[i], value: quoteValue(string(mode)), has: true})
	}

	return table
}

func addModelHosts(out map[string]*modelTable, hosts map[string]HostSettings) error {
	for _, name := range slices.Sorted(maps.Keys(hosts)) {
		settings := hosts[name]
		path := tpath{{key: "hosts"}, {key: name}}
		table := newModelTable(path)

		for _, key := range []struct {
			name  string
			value *bool
		}{
			{"enabled", settings.Enabled},
			{"runtime", settings.Runtime},
			{hooksKey, settings.Hooks},
		} {
			if key.value == nil {
				continue
			}

			value, err := canonicalValue(*key.value)
			if err != nil {
				return err
			}

			table.keys = append(table.keys, modelKey{name: key.name, value: value, has: true})
		}

		out[path.key()] = table
	}

	return nil
}

func addModelEntries(out map[string]*modelTable, s *Spec) error {
	for i := range s.Sources {
		source := s.Sources[i]
		path := tpath{{key: sourceTable, array: true, idx: i, ident: source.Name}}
		table := newModelTable(path, modelKey{name: "name", value: quoteValue(source.Name), has: true})

		if source.URL != "" {
			table.keys = append(table.keys, modelKey{name: "url", value: quoteValue(source.URL), has: true})
		}

		out[path.key()] = table
	}

	for i := range s.Packages {
		pkg := s.Packages[i]
		path := tpath{{key: packageTable, array: true, idx: i, ident: pkg.ID}}
		table := newModelTable(path, modelKey{name: "id", value: quoteValue(pkg.ID), has: true})

		for _, key := range []struct{ name, value string }{
			{"channel", pkg.Channel},
			{"version", pkg.Version},
			{"adopted_from", pkg.AdoptedFrom},
		} {
			if key.value == "" {
				continue
			}

			table.keys = append(table.keys, modelKey{name: key.name, value: quoteValue(key.value), has: true})
		}

		if pkg.Disabled {
			table.keys = append(table.keys, modelKey{name: "disabled", value: "true", has: true})
		}

		if len(pkg.Except) > 0 {
			table.keys = append(table.keys, modelKey{name: "except", value: inlineList(pkg.Except), has: true})
		}

		if len(pkg.Env) > 0 {
			table.keys = append(table.keys, modelKey{name: "env", value: inlineTable(pkg.Env), has: true})
		}

		out[path.key()] = table
		addModelPolicy(out, path, pkg.Propagate)
	}

	return nil
}

// quoteValue renders a string the way go-toml renders one: as a literal string
// wherever that is possible.
func quoteValue(value string) string {
	rendered, err := canonicalValue(value)
	if err != nil {
		return "'" + value + "'"
	}

	return rendered
}

// canonicalValue renders one value on its own and returns its bytes without the
// `v = ` the wrapper needs.
func canonicalValue(value any) (string, error) {
	rendered, err := toml.Marshal(map[string]any{"v": value})
	if err != nil {
		return "", fmt.Errorf("render spec value: %w", err)
	}

	text := strings.TrimSuffix(string(rendered), "\n")

	body, ok := strings.CutPrefix(text, "v = ")
	if !ok {
		return "", fmt.Errorf("spec value %v does not render as one line", value)
	}

	return body, nil
}

// inlineList renders a string list as the inline array a `key = [ … ]` line
// carries, so a table written that way is edited as one value.
func inlineList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, quoteValue(value))
	}

	return "[" + strings.Join(quoted, ", ") + "]"
}

// inlineTable renders a package's environment as the inline table it is written
// as when it fits on one line.
func inlineTable(env map[string]string) string {
	keys := slices.Sorted(maps.Keys(env))

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+" = "+quoteValue(env[key]))
	}

	return "{" + strings.Join(parts, ", ") + "}"
}

// policyEmpty reports whether a propagation policy says nothing at all, which is
// what keeps an empty section out of the model — and out of the file.
func policyEmpty(policy *Propagate) bool {
	switch {
	case policy == nil:
		return true
	case policy.hasRaw(), len(policy.Kind) > 0, len(policy.Host) > 0:
		return false
	}

	for _, ev := range events {
		if policy.Mode(ev) != "" {
			return false
		}
	}

	return true
}

// sameKeys reports whether two models of one table hold the same schema values,
// which is the whole question a save has to ask about it.
func sameKeys(before, after *modelTable) bool {
	if len(before.keys) != len(after.keys) {
		return false
	}

	for i := range after.keys {
		if before.keys[i] != after.keys[i] {
			return false
		}
	}

	return true
}

// subtreeEqual reports whether two models hold the same tables with the same
// values under a path, which is how a table written as an inline value is found
// to need no edit at all.
func subtreeEqual(prefix string, before, after map[string]*modelTable) bool {
	for key, base := range before {
		if !underKey(key, prefix) {
			continue
		}

		next, ok := after[key]
		if !ok || !sameKeys(base, next) {
			return false
		}
	}

	for key := range after {
		if underKey(key, prefix) {
			if _, ok := before[key]; !ok {
				return false
			}
		}
	}

	return true
}

func underKey(key, prefix string) bool {
	return key == prefix || strings.HasPrefix(key, prefix+"\x00")
}
