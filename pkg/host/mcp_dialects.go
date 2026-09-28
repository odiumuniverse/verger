package host

import (
	"cmp"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/render"
)

// Host MCP dialects. Three of the hosts read MCP servers from a document the
// renderer carries no entry shape for, so each adapter renders its own value
// and every adapter builds the edit list the same way: one `mcpServers.<name>`
// key edit per server, sorted by name, duplicates refused. The container key is
// the Claude spelling, which is also the one beadle writes for pi and
// Antigravity and the one the DSH patch record id is derived from.

// hostServerName is the portable MCP server-name grammar verger accepts in a
// host document: the same set every dialect takes, so a name that survives one
// host survives the others.
var hostServerName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// hostServerPrefix is the container key of a host MCP document.
const hostServerPrefix = "mcpServers."

// mcpConfigEntries builds the key-path edits of one host MCP document from a
// per-host value renderer.
func mcpConfigEntries(servers []manifest.MCPServer, value func(manifest.MCPServer) (map[string]any, error)) ([]render.Edit, error) {
	ordered := slices.Clone(servers)

	slices.SortFunc(ordered, func(a, b manifest.MCPServer) int { return cmp.Compare(a.Name, b.Name) })

	seen := map[string]bool{}

	edits := make([]render.Edit, 0, len(ordered))

	for _, server := range ordered {
		if seen[server.Name] {
			return nil, errors.New("duplicate MCP server name " + server.Name)
		}

		seen[server.Name] = true

		entry, err := value(server)
		if err != nil {
			return nil, err
		}

		edits = append(edits, render.Edit{Path: hostServerPrefix + server.Name, Value: entry})
	}

	return edits, nil
}

// validateHostServerName refuses a name that is not one safe path element of a
// host document.
func validateHostServerName(server manifest.MCPServer) error {
	if !hostServerName.MatchString(server.Name) {
		return errors.New("the server name must match [A-Za-z0-9._-]+, got " + server.Name)
	}

	if server.URL == "" && len(server.Command) == 0 {
		return errors.New("server " + server.Name + " has neither a command nor a url")
	}

	if server.URL != "" && len(server.Command) > 0 {
		return errors.New("server " + server.Name + " sets both a command and a url; the host reads one transport")
	}

	return nil
}

// stringValues copies one string map: an env value or a header keeps its
// `{env:NAME}` and `{secret:NAME}` spelling for the document editor to resolve.
func stringValues(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))

	for key, value := range values {
		out[strings.TrimSpace(key)] = value
	}

	return out
}

// anyStringMap widens one string map for a renderer that takes `any` values.
func anyStringMap(values map[string]string) map[string]any {
	out := make(map[string]any, len(values))

	for key, value := range values {
		out[key] = value
	}

	return out
}

// pi MCP dialect. Pi has no MCP of its own; the file is the override the
// third-party pi-mcp-adapter reads, and its entries are the adapter's own shape
// (beadle pkg/agent/mcp_codecs.go `piMCP`, whose written bytes are pinned in
// pkg/engine/a21_gws_test.go):
//
//	{"mcpServers": {"demo": {"command": "demo-mcp", "transport": "stdio"}}}
//
// so an entry carries `transport`, never the Claude `type`, and a remote server
// spells its endpoint `url` (the Claude spelling — not Gemini's `httpUrl`, and
// not Antigravity's `serverUrl`).
const (
	piKeyTransport = "transport"
	piKeyURL       = "url"
	piTransportSSE = "sse"
)

// piMCPEntries renders one key-path edit per server in the pi override dialect.
func piMCPEntries(servers []manifest.MCPServer) ([]render.Edit, error) {
	return mcpConfigEntries(servers, piMCPValue)
}

// piMCPValue renders one canonical MCP server the way pi-mcp-adapter reads it:
// a stdio entry carries command/args/env, a remote one url/headers, and both
// carry `transport` instead of a `type` field.
func piMCPValue(server manifest.MCPServer) (map[string]any, error) {
	if err := validateHostServerName(server); err != nil {
		return nil, err
	}

	entry := map[string]any{}

	if server.URL != "" {
		entry[piKeyURL] = server.URL

		if len(server.Headers) > 0 {
			entry["headers"] = stringValues(server.Headers)
		}

		entry[piKeyTransport] = piTransport(server.Transport)

		return entry, nil
	}

	entry["command"] = server.Command[0]

	if len(server.Command) > 1 {
		entry["args"] = slices.Clone(server.Command[1:])
	}

	if len(server.Env) > 0 {
		entry["env"] = stringValues(server.Env)
	}

	entry[piKeyTransport] = "stdio"

	return entry, nil
}

// piTransport spells one transport the pi way: every remote transport is
// streamable-http unless it is sse.
func piTransport(transport string) string {
	if transport == piTransportSSE {
		return piTransportSSE
	}

	return "streamable-http"
}

// agy MCP dialect. Antigravity reads remote servers from `serverUrl` and stdio
// servers from command/args/env, and carries no `type` and no transport field
// (beadle pkg/agent/mcp_codecs.go `antigravityMCP`; its pinned output is
// `{"mcpServers": {"web": {"serverUrl": "https://example.com/mcp"}}}`). The
// legacy `url` and `httpUrl` keys beadle strips on rewrite are never written:
// the endpoint spelling is what the host branches on.
const agyKeyServerURL = "serverUrl"

// agyMCPEntries renders one key-path edit per server in the Antigravity dialect.
func agyMCPEntries(servers []manifest.MCPServer) ([]render.Edit, error) {
	return mcpConfigEntries(servers, agyMCPValue)
}

// agyMCPValue renders one canonical MCP server the way Antigravity reads it:
// a remote server is a `serverUrl` with optional headers, a stdio server is
// command/args/env.
func agyMCPValue(server manifest.MCPServer) (map[string]any, error) {
	if err := validateHostServerName(server); err != nil {
		return nil, err
	}

	entry := map[string]any{}

	if server.URL != "" {
		entry[agyKeyServerURL] = server.URL

		if len(server.Headers) > 0 {
			entry["headers"] = stringValues(server.Headers)
		}

		return entry, nil
	}

	entry["command"] = server.Command[0]

	if len(server.Command) > 1 {
		entry["args"] = slices.Clone(server.Command[1:])
	}

	if len(server.Env) > 0 {
		entry["env"] = stringValues(server.Env)
	}

	return entry, nil
}
