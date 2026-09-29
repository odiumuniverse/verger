package render

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tailscale/hujson"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/odiumuniverse/verger/pkg/digest"
)

// Edit is one key-path edit of a shared config document.
type Edit struct {
	Path   string // dotted key path, e.g. "hooks", "mcpServers.acme"
	Value  any    // nil + Delete removes the key
	Delete bool
}

// Change is one edit actually applied to a document.
type Change struct {
	Path     string
	Digest   digest.Hash // digest of the value written (or removed)
	Existed  bool        // key existed before
	Previous any         // previous value (for the receipt backup)
}

// Owned maps a key path to the digest of the value verger wrote last time.
//
// A document whose records verger claims one by one (a hook document keyed per
// record) also carries the key itself, but the key's whole-object digest is not
// an ownership signal: a user adding their OWN record changes it without
// touching verger's. PerRecordKey marks such a key, so the check is skipped in
// favour of the per-record guard (DRIFT-2).
type Owned map[string]digest.Hash

// perRecordPrefix marks an Owned entry as "checked record by record, not as a
// whole key".
const perRecordPrefix = "per-record:"

// PerRecordKey is the Owned entry that hands ownership of one key over to its
// records.
func PerRecordKey(keyPath string) string {
	return perRecordPrefix + keyPath
}

// HandsOffError reports a key that changed outside verger: the owned hash does
// not match the current value, so nothing is written. Path is the top-level
// section, KeyPath the full dotted key path.
type HandsOffError struct {
	Path    string
	KeyPath string
	Reason  string
}

// Error implements error.
func (e *HandsOffError) Error() string {
	return fmt.Sprintf("hands-off %s: %s: %s", e.Path, e.KeyPath, e.Reason)
}

// EditJSONC applies key-path edits to a JSONC document, preserving comments and
// foreign keys with hujson. A leading UTF-8 BOM is stripped before parsing
// (decision T0.5 Q3); an untouched document is returned byte-identical. A key
// verger never wrote (or whose owned digest no longer matches) is hands-off:
// nothing is written. Unparsable documents are a *ConfigParseError.
func EditJSONC(file []byte, edits []Edit, owned Owned) ([]byte, []Change, error) {
	source := stripBOM(file)
	if len(bytes.TrimSpace(source)) == 0 {
		source = []byte("{}")
	}

	root, err := hujson.Parse(source)
	if err != nil {
		return nil, nil, &ConfigParseError{Cause: err}
	}

	if _, ok := hujsonObject(&root); !ok {
		return nil, nil, &ConfigParseError{Cause: errors.New("the config root is not an object")}
	}

	standard, err := hujson.Standardize(root.Pack())
	if err != nil {
		return nil, nil, &ConfigParseError{Cause: err}
	}

	doc := map[string]any{}

	if err := json.Unmarshal(standard, &doc); err != nil {
		return nil, nil, &ConfigParseError{Cause: err}
	}

	if len(edits) == 0 {
		return file, nil, nil
	}

	if key, duplicate := duplicateJSONCKey(&root); duplicate {
		return nil, nil, &ConfigParseError{Cause: fmt.Errorf("the config repeats the key %q", key)}
	}

	var (
		changes []Change
		changed bool
	)

	for _, edit := range edits {
		change, err := applyJSONCEdit(&root, doc, edit, owned)
		if err != nil {
			return nil, nil, err
		}

		if change == nil {
			continue
		}

		changes = append(changes, *change)

		changed = true
	}

	if !changed {
		return file, nil, nil
	}

	return root.Pack(), changes, nil
}

// duplicateJSONCKey reports the first object key a document repeats. JSON
// semantics keep the last duplicate, while the hujson edit helpers would edit
// the first, so an ambiguous document is refused instead of half-edited.
func duplicateJSONCKey(root *hujson.Value) (string, bool) {
	switch value := root.Value.(type) {
	case *hujson.Object:
		seen := map[string]bool{}

		for i := range value.Members {
			name, ok := hujsonMemberName(&value.Members[i])
			if !ok {
				continue
			}

			if seen[name] {
				return name, true
			}

			seen[name] = true

			if key, found := duplicateJSONCKey(&value.Members[i].Value); found {
				return key, true
			}
		}
	case *hujson.Array:
		for i := range value.Elements {
			if key, found := duplicateJSONCKey(&value.Elements[i]); found {
				return key, true
			}
		}
	}

	return "", false
}

// applyJSONCEdit validates and applies one edit; a nil change means no-op.
func applyJSONCEdit(root *hujson.Value, doc map[string]any, edit Edit, owned Owned) (*Change, error) {
	path, err := splitEditPath(edit.Path)
	if err != nil {
		return nil, &RenderError{Kind: kindConfig, Name: edit.Path, Cause: err}
	}

	if !edit.Delete && edit.Value == nil {
		return nil, &RenderError{Kind: kindConfig, Name: edit.Path, Cause: errors.New("nil value without delete")}
	}

	current, exists, err := valueAt(doc, path)
	if err != nil {
		return nil, &RenderError{Kind: kindConfig, Name: edit.Path, Cause: err}
	}

	if exists {
		if handsOff := ownershipError(owned, edit.Path, current); handsOff != nil {
			return nil, handsOff
		}
	}

	if edit.Delete {
		if !exists {
			return nil, nil
		}

		if err := hujsonRemove(root, path); err != nil {
			return nil, err
		}

		deleteAt(doc, path)

		return &Change{Path: edit.Path, Existed: true, Previous: current, Digest: canonicalDigest(current)}, nil
	}

	if exists && canonicalDigest(current) == canonicalDigest(edit.Value) {
		return nil, nil
	}

	if err := hujsonSet(root, path, edit.Value); err != nil {
		return nil, &RenderError{Kind: kindConfig, Name: edit.Path, Cause: err}
	}

	_ = setAt(doc, path, edit.Value)

	return &Change{Path: edit.Path, Existed: exists, Previous: current, Digest: canonicalDigest(edit.Value)}, nil
}

// EditTOML applies key-path edits to a TOML document with the same ownership
// semantics as EditJSONC, on top of a comment-preserving span editor. A leading
// UTF-8 BOM is stripped before parsing (decision T0.5 Q3); an untouched document
// is returned byte-identical. A two-segment map value is written as a
// [section.sub] table block.
func EditTOML(file []byte, edits []Edit, owned Owned) ([]byte, []Change, error) {
	source := stripBOM(file)

	doc := map[string]any{}

	if len(bytes.TrimSpace(source)) > 0 {
		if err := toml.Unmarshal(source, &doc); err != nil {
			return nil, nil, &ConfigParseError{Cause: err}
		}
	}

	spans := scanTOMLSpans(source)
	tables := scanTOMLTables(source)
	arrayOfTables := scanTOMLArrayOfTables(source)
	eol := tomlEOL(source)

	if len(edits) == 0 {
		return file, nil, nil
	}

	var (
		cuts    []tomlCut
		changes []Change
		changed bool
	)

	for _, edit := range edits {
		editCuts, change, err := applyTOMLEdit(source, spans, tables, arrayOfTables, doc, eol, edit, owned)
		if err != nil {
			return nil, nil, err
		}

		if change == nil {
			continue
		}

		cuts = append(cuts, editCuts...)
		changes = append(changes, *change)

		changed = true
	}

	if !changed {
		return file, nil, nil
	}

	return applyTOMLCuts(source, cuts), changes, nil
}

// applyTOMLEdit validates and places one edit; a nil change means no-op.
func applyTOMLEdit(
	file []byte, spans tomlSpanScan, tables []tomlTableSpan, arrayOfTables [][]string, doc map[string]any, eol string, edit Edit, owned Owned,
) ([]tomlCut, *Change, error) {
	path, err := tomlEditPath(arrayOfTables, edit)
	if err != nil {
		return nil, nil, err
	}

	current, exists := tomlLookup(doc, path)

	if exists {
		if handsOff := ownershipError(owned, edit.Path, current); handsOff != nil {
			return nil, nil, handsOff
		}
	}

	if edit.Delete {
		if !exists {
			return nil, nil, nil
		}

		removal, ok := tomlRemoval(file, spans, tables, path)
		if !ok {
			return nil, nil, &RenderError{
				Kind:  kindConfig,
				Name:  edit.Path,
				Cause: errors.New("the key has no table or assignment this editor can remove"),
			}
		}

		deleteAt(doc, path)

		return []tomlCut{removal}, &Change{Path: edit.Path, Existed: true, Previous: current, Digest: canonicalDigest(current)}, nil
	}

	if exists && canonicalDigest(current) == canonicalDigest(edit.Value) {
		return nil, nil, nil
	}

	if err := checkTOMLParents(doc, path); err != nil {
		return nil, nil, &RenderError{Kind: kindConfig, Name: edit.Path, Cause: err}
	}

	placement, err := tomlPlacement(file, spans, tables, path, edit.Value, eol)
	if err != nil {
		return nil, nil, &RenderError{Kind: kindConfig, Name: edit.Path, Cause: err}
	}

	_ = setAt(doc, path, edit.Value)

	return placement, &Change{Path: edit.Path, Existed: exists, Previous: current, Digest: canonicalDigest(edit.Value)}, nil
}

// tomlEditPath validates one TOML edit and returns its key path: the path must
// split, a set needs a value, and the key must not lie inside an array of
// tables.
func tomlEditPath(arrayOfTables [][]string, edit Edit) ([]string, error) {
	path, err := splitEditPath(edit.Path)
	if err != nil {
		return nil, &RenderError{Kind: kindConfig, Name: edit.Path, Cause: err}
	}

	if !edit.Delete && edit.Value == nil {
		return nil, &RenderError{Kind: kindConfig, Name: edit.Path, Cause: errors.New("nil value without delete")}
	}

	if traversesArrayOfTables(arrayOfTables, path) {
		return nil, &RenderError{
			Kind:  kindConfig,
			Name:  edit.Path,
			Cause: errors.New("the key lies inside an array of tables this editor cannot rewrite"),
		}
	}

	return path, nil
}

// stripBOM removes a leading UTF-8 byte order mark: hand-edited configs from
// Windows editors carry one and it never changes the document semantics
// (decision T0.5 Q3, same policy as pkg/spec).
func stripBOM(data []byte) []byte {
	return bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
}

// canonicalDigest hashes a config value in its canonical JSON form (json.Marshal
// order), so TOML and JSONC documents compare the same value equally.
func canonicalDigest(value any) digest.Hash {
	data, err := json.Marshal(value)
	if err != nil {
		return digest.Bytes(nil)
	}

	return digest.Bytes(data)
}

// ownershipError reports a present key verger does not own.
func ownershipError(owned Owned, keyPath string, current any) error {
	if _, perRecord := owned[PerRecordKey(keyPath)]; perRecord {
		return nil
	}

	digest, ok := owned[keyPath]
	if ok && digest == canonicalDigest(current) {
		return nil
	}

	return &HandsOffError{
		Path:    topSegment(keyPath),
		KeyPath: keyPath,
		Reason:  "the owned hash does not match the current value",
	}
}

// splitEditPath validates and splits a dotted key path.
func splitEditPath(path string) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("empty key path")
	}

	segments := strings.Split(path, ".")

	for _, segment := range segments {
		if strings.TrimSpace(segment) == "" {
			return nil, fmt.Errorf("key path %q has an empty segment", path)
		}
	}

	return segments, nil
}

// topSegment returns the first segment of a dotted path.
func topSegment(path string) string {
	first, _, _ := strings.Cut(path, ".")

	return first
}

// valueAt resolves a dotted path in a decoded JSON document; a non-object
// intermediate value is an error.
func valueAt(doc map[string]any, path []string) (any, bool, error) {
	var current any = doc

	for i, segment := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("cannot look up %q below a non-object value", strings.Join(path[:i+1], "."))
		}

		current, ok = object[segment]
		if !ok {
			return nil, false, nil
		}
	}

	return current, true, nil
}

// setAt stores a dotted path in a decoded JSON document, creating intermediate
// objects.
func setAt(doc map[string]any, path []string, value any) error {
	object := doc

	for _, segment := range path[:len(path)-1] {
		child, ok := object[segment]
		if !ok {
			child = map[string]any{}
			object[segment] = child
		}

		next, ok := child.(map[string]any)
		if !ok {
			return fmt.Errorf("cannot create %q below a non-object value", segment)
		}

		object = next
	}

	object[path[len(path)-1]] = value

	return nil
}

// deleteAt removes a dotted path from a decoded JSON document.
func deleteAt(doc map[string]any, path []string) {
	object := doc

	for _, segment := range path[:len(path)-1] {
		next, ok := object[segment].(map[string]any)
		if !ok {
			return
		}

		object = next
	}

	delete(object, path[len(path)-1])
}

// hujsonObject returns the object behind a value.
func hujsonObject(value *hujson.Value) (*hujson.Object, bool) {
	object, ok := value.Value.(*hujson.Object)

	return object, ok
}

// hujsonFindMember returns the member with the exact name.
func hujsonFindMember(object *hujson.Object, name string) *hujson.ObjectMember {
	for i := range object.Members {
		if current, ok := hujsonMemberName(&object.Members[i]); ok && current == name {
			return &object.Members[i]
		}
	}

	return nil
}

// hujsonMemberName returns the decoded name of a member.
func hujsonMemberName(member *hujson.ObjectMember) (string, bool) {
	return hujsonLiteralString(member.Name)
}

// hujsonLiteralString decodes a string literal.
func hujsonLiteralString(value hujson.Value) (string, bool) {
	literal, ok := value.Value.(hujson.Literal)
	if !ok {
		return "", false
	}

	var decoded string

	if err := json.Unmarshal(literal, &decoded); err != nil {
		return "", false
	}

	return decoded, true
}

// hujsonSet stores a value at a dotted path, creating intermediate objects.
func hujsonSet(root *hujson.Value, path []string, value any) error {
	replacement, err := hujsonValue(value)
	if err != nil {
		return err
	}

	object, ok := hujsonObject(root)
	if !ok {
		return errors.New("the config root is not an object")
	}

	for _, segment := range path[:len(path)-1] {
		member := hujsonFindMember(object, segment)
		if member == nil {
			child := hujson.Value{Value: &hujson.Object{}}

			object.Members = append(object.Members, hujson.ObjectMember{Name: hujsonName(segment), Value: child})
			member = &object.Members[len(object.Members)-1]
		}

		next, ok := hujsonObject(&member.Value)
		if !ok {
			return fmt.Errorf("cannot create %q below a non-object value", segment)
		}

		object = next
	}

	last := path[len(path)-1]

	if member := hujsonFindMember(object, last); member != nil {
		replacement.BeforeExtra = member.Value.BeforeExtra
		replacement.AfterExtra = member.Value.AfterExtra
		member.Value = replacement

		return nil
	}

	object.Members = append(object.Members, hujson.ObjectMember{Name: hujsonName(last), Value: replacement})

	return nil
}

// hujsonRemove deletes a dotted path; a missing path is a no-op.
func hujsonRemove(root *hujson.Value, path []string) error {
	object, ok := hujsonObject(root)
	if !ok {
		return errors.New("the config root is not an object")
	}

	for _, segment := range path[:len(path)-1] {
		member := hujsonFindMember(object, segment)
		if member == nil {
			return nil
		}

		next, ok := hujsonObject(&member.Value)
		if !ok {
			return nil
		}

		object = next
	}

	last := path[len(path)-1]

	for i := range object.Members {
		if name, ok := hujsonMemberName(&object.Members[i]); ok && name == last {
			object.Members = append(object.Members[:i], object.Members[i+1:]...)

			return nil
		}
	}

	return nil
}

// hujsonValue parses a Go value into a hujson value.
func hujsonValue(value any) (hujson.Value, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return hujson.Value{}, err
	}

	return hujson.Parse(data)
}

// hujsonName renders a member name.
func hujsonName(name string) hujson.Value {
	data, err := json.Marshal(name)
	if err != nil {
		return hujson.Value{}
	}

	parsed, err := hujson.Parse(data)
	if err != nil {
		return hujson.Value{}
	}

	return parsed
}
