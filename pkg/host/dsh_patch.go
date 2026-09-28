package host

import (
	"bytes"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"

	yaml "go.yaml.in/yaml/v3"
)

// The DSH home patch layer ($DSH_HOME/cordis.patch.yml) is not a key-path
// config document: it is a top-level YAML array of loader patch entries, and an
// MCP server is one record of an `insert` list, identified by the id
// `verger:<server>` and configured under `config` (decision A-44, schema pinned
// live in beadle docs/pending/q16-evidence). The loader mounts only inserted
// records, so verger appends to an insert list and never writes elsewhere; a
// record verger does not own — any id without its prefix — and every comment in
// the file stay byte-for-byte as the user wrote them.
const (
	dshPatchIDPrefix = "verger:"
	// dshMCPPlugin is the plugin each MCP record configures; a record with
	// another `name` is not a server.
	dshMCPPlugin = "@deepseek-ai/dsh-mcp-client"
	dshPatchKey  = "insert"
	dshIDKey     = "id"
	dshNameKey   = "name"
	dshConfigKey = "config"
	// dshPatchHeader seeds the file verger creates: dsh never creates the home
	// layer itself, so the first write explains what the file is.
	dshPatchHeader = "MCP servers for every DSH profile: the home patch layer DSH applies after each profile's own layer.\n" +
		`verger manages only the entries whose id starts with "` + dshPatchIDPrefix + `"; other entries and comments stay untouched.`
	dshTransportStdio = "stdio"
	dshTransportHTTP  = "streamable-http"
	// dshPatchIndent is the width dsh's own `--dump-config` uses.
	dshPatchIndent = 2
	// DSH record config keys, in the order the loader's own dump uses.
	dshTransportKey  = "transport"
	dshServerNameKey = "serverName"
	dshCommandKey    = "command"
	dshArgsKey       = "args"
	dshEnvKey        = "env"
	dshURLKey        = "url"
	dshHeadersKey    = "headers"
)

// dshServerName is the serverName grammar the loader accepts: the value becomes
// part of every mounted tool name (`mcp__<serverName>__<tool>`), so it is
// narrower than verger's portable server name — a dot is not allowed.
var dshServerName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// dshPatchError reports a home patch layer verger cannot read: the loader
// requires a top-level array, and a file that is not one is reported instead of
// being overwritten.
type dshPatchError struct {
	Path   string
	Reason string
}

// Error implements error.
func (e *dshPatchError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, e.Reason)
}

// dshMCPEntries renders one key-path edit per server. The edit value is the
// record's `config` mapping, which is also what the member reader returns, so
// the ownership digest compares the same value on both sides.
func dshMCPEntries(servers []manifest.MCPServer) ([]render.Edit, error) {
	return mcpConfigEntries(servers, dshServerConfig)
}

// dshServerConfig renders one server as the loader reads it: a transport of
// stdio or streamable-http (dsh has no SSE transport), the mounted server name,
// and either command/args/env or url/headers.
func dshServerConfig(server manifest.MCPServer) (map[string]any, error) {
	if !dshServerName.MatchString(server.Name) {
		return nil, fmt.Errorf("serverName %q must match %s", server.Name, dshServerName)
	}

	if server.URL != "" && len(server.Command) > 0 {
		return nil, fmt.Errorf("server %q sets both a command and a url; DSH reads one transport per record", server.Name)
	}

	config := map[string]any{}

	if server.URL != "" {
		if server.Transport == piTransportSSE {
			return nil, fmt.Errorf("transport %q is not supported; DSH reads stdio and streamable-http", server.Transport)
		}

		config[dshTransportKey] = dshTransportHTTP
		config[dshServerNameKey] = server.Name
		config[dshURLKey] = server.URL

		if len(server.Headers) > 0 {
			config[dshHeadersKey] = stringValues(server.Headers)
		}

		return config, nil
	}

	if len(server.Command) == 0 {
		return nil, fmt.Errorf("server %q has neither a command nor a url; DSH reads one transport per record", server.Name)
	}

	config[dshTransportKey] = dshTransportStdio
	config[dshServerNameKey] = server.Name
	config[dshCommandKey] = server.Command[0]

	if len(server.Command) > 1 {
		config[dshArgsKey] = slices.Clone(server.Command[1:])
	}

	if len(server.Env) > 0 {
		config[dshEnvKey] = stringValues(server.Env)
	}

	return config, nil
}

// dshPatchMember reads one owned record's config mapping by its key path: it is
// the document member reader of a surface the JSONC/TOML decoder cannot read,
// so the pending-edit filter and the receipt ops compare the same value the
// writer would produce.
func dshPatchMember(file []byte, keyPath string) (any, bool, error) {
	name, ok := dshServerNameOf(keyPath)
	if !ok {
		return nil, false, nil
	}

	root, _, err := dshPatchDocument(file)
	if err != nil {
		return nil, false, err
	}

	record := dshFindRecord(root, name)
	if record == nil {
		return nil, false, nil
	}

	return dshNodeValue(dshRecordConfig(record))
}

// dshServerNameOf extracts the server name of one MCP key path.
func dshServerNameOf(keyPath string) (string, bool) {
	name := strings.TrimPrefix(keyPath, hostServerPrefix)

	return name, name != keyPath && name != ""
}

// dshPatchEdit applies the planned records to the home patch layer. Ownership is
// per record: a record whose current value still matches the digest the previous
// receipt recorded for it is verger's and is rewritten in place; a record whose
// value moved is hands-off and nothing is written; a missing record is appended
// to the file's insert list; and a record verger does not own — any id without
// its prefix — is never read for writing and never touched. A document that
// needs no change comes back unchanged, so a re-delivery writes nothing.
func dshPatchEdit(file []byte, edits []render.Edit, owned render.Owned) ([]byte, []render.Change, error) {
	if len(edits) == 0 {
		return file, nil, nil
	}

	root, _, err := dshPatchDocument(file)
	if err != nil {
		return nil, nil, err
	}

	changes := make([]render.Change, 0, len(edits))

	touched := false

	for _, edit := range edits {
		change, wrote, err := dshApplyRecord(root, edit, owned)
		if err != nil {
			return nil, nil, err
		}

		if change != nil {
			changes = append(changes, *change)
		}

		touched = touched || wrote
	}

	if !touched {
		return file, nil, nil
	}

	out, err := dshEncodePatch(root)
	if err != nil {
		return nil, nil, err
	}

	return out, changes, nil
}

// dshApplyRecord applies one planned record to the patch tree. change is nil
// when the record already holds the wanted value, wrote reports whether the tree
// moved, and an error is a hands-off verdict for a record whose value verger no
// longer owns.
func dshApplyRecord(root *yaml.Node, edit render.Edit, owned render.Owned) (*render.Change, bool, error) {
	name, ok := dshServerNameOf(edit.Path)
	if !ok {
		return nil, false, fmt.Errorf("dsh patch key path %q is not an MCP server key", edit.Path)
	}

	value, ok := edit.Value.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("dsh patch record %q is not a mapping", name)
	}

	sum := canonicalValueDigest(value)

	record := dshFindRecord(root, name)
	if record == nil {
		dshAppendRecord(root, dshRecordNode(name, value))

		return &render.Change{Path: edit.Path, Digest: sum}, true, nil
	}

	previous, _, err := dshNodeValue(dshRecordConfig(record))
	if err != nil {
		return nil, false, err
	}

	if canonicalValueDigest(previous) == sum {
		return nil, false, nil
	}

	// The record moved since verger wrote it: the user owns this value now and
	// a delivery must not overwrite it. A record the previous receipt recorded
	// with exactly this digest is the exception — that one is verger's.
	if recorded, isOwned := owned[edit.Path]; !isOwned || recorded != canonicalValueDigest(previous) {
		return nil, false, &render.HandsOffError{
			Path:    hostServerPrefix[:len(hostServerPrefix)-1],
			KeyPath: edit.Path,
			Reason:  "the record changed outside verger; left in place",
		}
	}

	replacement, err := dshConfigNode(value)
	if err != nil {
		return nil, false, err
	}

	dshSetRecordConfig(record, replacement)

	return &render.Change{Path: edit.Path, Digest: sum, Existed: true, Previous: previous}, true, nil
}

// dshPatchDocument parses the home patch layer. A missing or empty file becomes
// a fresh top-level array carrying the header; a document whose root is not an
// array is refused, because records written there would not be mounted.
func dshPatchDocument(file []byte) (*yaml.Node, bool, error) {
	// A missing file is created (dsh never creates the home layer itself); a
	// file that exists but carries nothing is refused, because an empty layer
	// is either a half-finished edit or a mistake, and overwriting it would
	// throw away whatever the user meant to put there.
	if file == nil {
		return newDSHPatchArray(), true, nil
	}

	if len(bytes.TrimSpace(file)) == 0 {
		return nil, false, &dshPatchError{
			Path:   dshPatchFile,
			Reason: "empty patch file; DSH needs a top-level array (write [] or remove the file)",
		}
	}

	var document yaml.Node

	if err := yaml.Unmarshal(file, &document); err != nil {
		return nil, false, &dshPatchError{Path: dshPatchFile, Reason: "parse the patch file: " + err.Error()}
	}

	if len(document.Content) == 0 {
		return newDSHPatchArray(), true, nil
	}

	root := document.Content[0]

	if root.Kind != yaml.SequenceNode {
		return nil, false, &dshPatchError{
			Path:   dshPatchFile,
			Reason: "DSH needs a top-level YAML array of patch entries (write [] or remove the file)",
		}
	}

	return root, false, nil
}

// newDSHPatchArray is an empty top-level patch array carrying the header dsh
// would otherwise have written itself.
func newDSHPatchArray() *yaml.Node {
	return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", HeadComment: dshPatchHeader}
}

// dshFindRecord returns the node of the owned record of one server, or nil.
func dshFindRecord(root *yaml.Node, name string) *yaml.Node {
	want := dshPatchIDPrefix + name

	for _, entry := range root.Content {
		if entry.Kind != yaml.MappingNode {
			continue
		}

		for i := 0; i+1 < len(entry.Content); i += 2 {
			if entry.Content[i].Value != dshPatchKey {
				continue
			}

			list := entry.Content[i+1]
			if list.Kind != yaml.SequenceNode {
				continue
			}

			for _, record := range list.Content {
				if record.Kind != yaml.MappingNode {
					continue
				}

				for j := 0; j+1 < len(record.Content); j += 2 {
					if record.Content[j].Value == dshIDKey && record.Content[j+1].Value == want {
						return record
					}
				}
			}
		}
	}

	return nil
}

// dshRecordPath is the receipt identity of one owned record: an artifact at this
// path carries that record's own value digest, so the next delivery can tell a
// record verger wrote from one the user edited. It is deliberately not a
// filesystem path — pkg/apply skips such an artifact in its verification walk,
// because only this adapter can check it.
func dshRecordPath(name string) string {
	return "dsh://patch/" + name
}

// dshRecordArtifacts returns one receipt artifact per planned record, carrying
// that record's value digest.
func dshRecordArtifacts(edits []render.Edit) []receipt.Artifact {
	artifacts := make([]receipt.Artifact, 0, len(edits))

	for _, edit := range edits {
		name, ok := dshServerNameOf(edit.Path)
		if !ok {
			continue
		}

		value, ok := edit.Value.(map[string]any)
		if !ok {
			continue
		}

		artifacts = append(artifacts, receipt.Artifact{
			Kind:   dshRecordKind,
			Name:   name,
			Path:   dshRecordPath(name),
			Digest: canonicalValueDigest(value),
		})
	}

	return artifacts
}

// dshRecordKind names a receipt artifact of one patch record.
const dshRecordKind = "mcp-record"

// dshRecordConfig returns the `config` mapping node of one record.
func dshRecordConfig(record *yaml.Node) *yaml.Node {
	for i := 0; i+1 < len(record.Content); i += 2 {
		if record.Content[i].Value == dshConfigKey {
			return record.Content[i+1]
		}
	}

	return nil
}

// dshSetRecordConfig replaces a record's `config` mapping, keeping the record's
// id and name untouched.
func dshSetRecordConfig(record, config *yaml.Node) {
	for i := 0; i+1 < len(record.Content); i += 2 {
		if record.Content[i].Value == dshConfigKey {
			record.Content[i+1] = config

			return
		}
	}
}

// dshAppendRecord appends one record to the file's insert list, creating the
// list when the file has none — the loader mounts only inserted records.
func dshAppendRecord(root, record *yaml.Node) {
	for _, entry := range slices.Backward(root.Content) {
		if entry.Kind != yaml.MappingNode {
			continue
		}

		for j := 0; j+1 < len(entry.Content); j += 2 {
			if entry.Content[j].Value != dshPatchKey {
				continue
			}

			list := entry.Content[j+1]
			if list.Kind == yaml.SequenceNode {
				list.Content = append(list.Content, record)

				return
			}
		}
	}

	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	entry.Content = append(entry.Content,
		dshKeyNode(dshPatchKey),
		&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{record}},
	)

	root.Content = append(root.Content, entry)
}

// dshRecordNode builds one whole record: the owned id, the MCP plugin name and
// the server's config mapping.
func dshRecordNode(name string, config map[string]any) *yaml.Node {
	configNode, err := dshConfigNode(config)
	if err != nil {
		// The config was rendered by dshServerConfig, which already validated
		// it; an error here would be a programming error, not a user one.
		panic(err)
	}

	record := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	record.Content = append(record.Content,
		dshKeyNode(dshIDKey), dshStringNode(dshPatchIDPrefix+name),
		dshKeyNode(dshNameKey), dshStringNode(dshMCPPlugin),
		dshKeyNode(dshConfigKey), configNode,
	)

	return record
}

// dshKeyNode builds one mapping key. A key is emitted plain: it is a fixed
// word of the loader schema and never needs quoting.
func dshKeyNode(key string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
}

// dshStringNode builds one explicitly typed string scalar, quoting it only when
// the plain form would be read back as something else: the loader re-reads
// this file, and a bare `true` or `123` would arrive as a boolean or a number.
func dshStringNode(value string) *yaml.Node {
	node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}

	var decoded any

	if err := yaml.Unmarshal([]byte(value), &decoded); err != nil {
		node.Style = yaml.SingleQuotedStyle

		return node
	}

	if _, isString := decoded.(string); !isString {
		node.Style = yaml.SingleQuotedStyle
	}

	return node
}

// dshConfigNode renders one server's config mapping with the key order the
// loader's own dump uses and sorted env/header maps, so a file verger writes is
// stable across runs.
func dshConfigNode(config map[string]any) (*yaml.Node, error) {
	node, err := dshMapNode(config)
	if err != nil {
		return nil, err
	}

	order := []string{dshTransportKey, dshServerNameKey, dshCommandKey, dshURLKey, dshArgsKey, dshEnvKey, dshHeadersKey}
	content := make([]*yaml.Node, 0, len(node.Content))

	for _, key := range order {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				content = append(content, node.Content[i], node.Content[i+1])

				break
			}
		}
	}

	node.Content = content

	return node, nil
}

// dshMapNode converts one map into a YAML mapping node with sorted keys.
func dshMapNode(values map[string]any) (*yaml.Node, error) {
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}

	for _, key := range slices.Sorted(maps.Keys(values)) {
		child, err := dshValueNode(values[key])
		if err != nil {
			return nil, err
		}

		node.Content = append(node.Content, dshKeyNode(key), child)
	}

	return node, nil
}

// dshValueNode converts one value into a YAML node; strings stay explicitly
// typed, so `"true"` and `"123"` are not re-read as a boolean or a number.
func dshValueNode(value any) (*yaml.Node, error) {
	switch typed := value.(type) {
	case string:
		return dshStringNode(typed), nil
	case []string:
		node := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}

		for _, item := range typed {
			child, err := dshValueNode(item)
			if err != nil {
				return nil, err
			}

			node.Content = append(node.Content, child)
		}

		return node, nil
	case map[string]string:
		return dshMapNode(anyStringMap(typed))
	default:
		return nil, fmt.Errorf("dsh patch value of type %T is not renderable", value)
	}
}

// dshNodeValue decodes one YAML node back into the Go value a digest is taken
// over, so a document verger reads and one verger writes hash identically.
func dshNodeValue(node *yaml.Node) (any, bool, error) {
	if node == nil {
		return nil, false, nil
	}

	value := any(map[string]any{})

	if err := node.Decode(&value); err != nil {
		return nil, false, fmt.Errorf("decode a dsh patch record: %w", err)
	}

	return value, true, nil
}

// dshEncodePatch re-encodes the node tree once, so every comment, entry order
// and scalar style of the file survives the write.
func dshEncodePatch(root *yaml.Node) ([]byte, error) {
	var buffer bytes.Buffer

	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(dshPatchIndent)

	if err := encoder.Encode(root); err != nil {
		return nil, fmt.Errorf("encode the dsh patch file: %w", err)
	}

	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("encode the dsh patch file: %w", err)
	}

	return buffer.Bytes(), nil
}
