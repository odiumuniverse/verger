// Package render converts canonical components into host dialects and edits
// shared config documents with key-path ownership. Every function is pure:
// renderers take and return bytes, config editors take and return the document
// bytes; nothing here touches the filesystem.
package render

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// Canonical labels shared by the renderers.
const (
	kindCommand    = "command"
	kindConfig     = "config"
	kindAgent      = "agent"
	kindRule       = "rule"
	kindMCP        = "mcp"
	transportSSE   = "sse"
	transportStdio = "stdio"
	keyCommand     = "command"
	keyType        = "type"
	keyName        = "name"
	keyDescription = "description"
	keyModel       = "model"
)

// RenderError reports malformed canonical input a renderer cannot express.
type RenderError struct {
	Kind  string
	Name  string
	Cause error
}

// Error implements error.
func (e *RenderError) Error() string {
	if e.Name != "" {
		return fmt.Sprintf("render %s %q: %v", e.Kind, e.Name, e.Cause)
	}

	return fmt.Sprintf("render %s: %v", e.Kind, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *RenderError) Unwrap() error {
	return e.Cause
}

// InexpressibleError reports a canonical field a host dialect cannot express.
// A required field missing from the host schema is returned without bytes; the
// optional fields a renderer drops come back joined with the rendered bytes so
// the caller can report them as warnings.
type InexpressibleError struct {
	Kind  string
	Name  string
	Field string
}

// Error implements error.
func (e *InexpressibleError) Error() string {
	if e.Name != "" {
		return fmt.Sprintf("%s %q cannot express %q", e.Kind, e.Name, e.Field)
	}

	return fmt.Sprintf("%s cannot express %q", e.Kind, e.Field)
}

// ConfigParseError reports a shared config document that does not parse. The
// editors are file-less, so Path is empty unless the caller fills it in.
type ConfigParseError struct {
	Path  string
	Cause error
}

// Error implements error.
func (e *ConfigParseError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("parse config %s: %v", e.Path, e.Cause)
	}

	return fmt.Sprintf("parse config: %v", e.Cause)
}

// Unwrap returns the underlying cause.
func (e *ConfigParseError) Unwrap() error {
	return e.Cause
}

// field is one frontmatter key rendered by a codec.
type field struct {
	Key   string
	Value any
}

// splitFrontmatter separates a markdown document into frontmatter content
// (without the fences) and the body. LF and CRLF fences are accepted; the body
// keeps its original line endings. A closing fence must end the line.
func splitFrontmatter(data []byte) ([]byte, string, bool) {
	s := string(data)

	start := 4

	switch {
	case strings.HasPrefix(s, "---\n"):
	case strings.HasPrefix(s, "---\r\n"):
		start = 5
	default:
		return nil, s, false
	}

	rest := s[start:]

	before, after, found := cutFence(rest)
	if !found {
		if tail, empty := leadingFence(rest); empty {
			return []byte{}, tail, true
		}

		return nil, s, false
	}

	front := []byte(strings.TrimSuffix(strings.ReplaceAll(before, "\r\n", "\n"), "\r"))

	switch {
	case strings.HasPrefix(after, "\r\n"):
		return front, after[2:], true
	case strings.HasPrefix(after, "\n"):
		return front, after[1:], true
	default:
		return front, after, true
	}
}

// leadingFence matches a closing fence at offset zero, the empty frontmatter
// block, and returns the body after it.
func leadingFence(rest string) (string, bool) {
	if !strings.HasPrefix(rest, "---") {
		return "", false
	}

	tail := strings.TrimLeft(rest[3:], " \t")

	switch {
	case strings.HasPrefix(tail, "\r\n"):
		return tail[2:], true
	case strings.HasPrefix(tail, "\n"):
		return tail[1:], true
	case tail == "":
		return "", true
	default:
		return "", false
	}
}

// cutFence finds a closing fence that ends its line.
func cutFence(rest string) (before, after string, ok bool) {
	offset := 0

	for {
		idx := strings.Index(rest[offset:], "\n---")
		if idx < 0 {
			return "", "", false
		}

		start := offset + idx
		tail := strings.TrimLeft(rest[start+4:], " \t")

		if tail == "" || tail[0] == '\n' || strings.HasPrefix(tail, "\r\n") {
			return rest[:start], tail, true
		}

		offset = start + 1
	}
}

// unmarshalFrontmatter splits a markdown document and decodes its frontmatter
// into dst. ok reports whether a block was present; the body is returned either
// way.
func unmarshalFrontmatter(data []byte, dst any) (string, bool, error) {
	front, body, ok := splitFrontmatter(data)
	if !ok {
		return body, false, nil
	}

	if err := yaml.Unmarshal(front, dst); err != nil {
		return "", false, fmt.Errorf("parse frontmatter: %w", err)
	}

	return body, true, nil
}

// composeFrontmatter builds a markdown document from frontmatter fields and a
// body, with two-space indentation and a trailing newline after every line.
func composeFrontmatter(fields []field, body string) []byte {
	root := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}

	mapping := root.Content[0]

	for _, item := range fields {
		if item.Value == nil {
			continue
		}

		node, err := yamlNode(item.Value)
		if err != nil {
			continue
		}

		mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: item.Key}, node)
	}

	var buf bytes.Buffer

	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)

	if err := encoder.Encode(root); err != nil {
		_ = encoder.Close()

		return nil
	}

	if err := encoder.Close(); err != nil {
		return nil
	}

	return encodeFrontmatter(buf.Bytes(), body)
}

// encodeFrontmatter wraps marshalled frontmatter and a body into a markdown
// document; an empty mapping is not written.
func encodeFrontmatter(front []byte, body string) []byte {
	normalized := normalizeBody(body)

	switch strings.TrimSpace(string(front)) {
	case "", "{}":
		return []byte(normalized)
	}

	var b strings.Builder

	b.WriteString("---\n")
	b.Write(front)
	b.WriteString("---\n")
	b.WriteString(normalized)

	return []byte(b.String())
}

// normalizeBody trims the trailing newlines of a body and keeps exactly one.
func normalizeBody(body string) string {
	trimmed := strings.TrimRight(body, "\r\n")

	if trimmed == "" {
		return ""
	}

	return trimmed + "\n"
}

// yamlNode encodes a Go value as a YAML node; scalar sequences render inline.
func yamlNode(value any) (*yaml.Node, error) {
	data, err := yaml.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode value: %w", err)
	}

	var doc yaml.Node

	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decode value: %w", err)
	}

	if len(doc.Content) == 0 {
		return nil, errors.New("empty value")
	}

	node := doc.Content[0]

	if node.Kind == yaml.SequenceNode && scalarsOnly(node) {
		node.Style = yaml.FlowStyle
	}

	return node, nil
}

// scalarsOnly reports whether a sequence holds scalars only.
func scalarsOnly(node *yaml.Node) bool {
	for _, item := range node.Content {
		if item.Kind != yaml.ScalarNode {
			return false
		}
	}

	return true
}

// stringList decodes a YAML scalar (comma or space separated) or a sequence.
type stringList []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (l *stringList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.SequenceNode:
		out := make([]string, 0, len(node.Content))

		for _, item := range node.Content {
			out = append(out, strings.TrimSpace(item.Value))
		}

		*l = out

		return nil
	case yaml.ScalarNode:
		fields := strings.FieldsFunc(node.Value, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t'
		})

		out := make([]string, 0, len(fields))

		for _, item := range fields {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, item)
			}
		}

		*l = out

		return nil
	default:
		return fmt.Errorf("expected a string or a list, got %s", node.Tag)
	}
}

// validSlug reports whether name is a canonical file-item slug: lowercase
// letters, digits, hyphens and underscores, starting with a letter or digit.
func validSlug(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}

	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}

		if i == 0 && (r == '-' || r == '_') {
			return false
		}
	}

	return true
}

// slugify lowercases a name and replaces every non-alphanumeric run with one
// hyphen.
func slugify(name string) string {
	var b strings.Builder

	dash := false

	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r > 0x7f && r != '\u00ad':
			b.WriteRune(r)

			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')

				dash = true
			}
		}
	}

	return strings.Trim(b.String(), "-")
}

// sortedKeys returns the sorted keys of a string-keyed map.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))

	for key := range m {
		out = append(out, key)
	}

	slices.Sort(out)

	return out
}
