package render

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/manifest"
)

// EncodeMCP renders one canonical MCP server in the dialect of a format.
// `{env:NAME}` references are spelled the host way; `{secret:NAME}` references
// stay verbatim.
func EncodeMCP(format manifest.Format, server manifest.MCPServer) (any, error) {
	switch format {
	case manifest.FormatClaude, manifest.FormatCodex, manifest.FormatGemini, manifest.FormatOpenCode:
	default:
		return nil, &RenderError{
			Kind:  kindMCP,
			Name:  server.Name,
			Cause: fmt.Errorf("format %s has no MCP dialect", format),
		}
	}

	if err := validateMCP(server); err != nil {
		return nil, err
	}

	switch format {
	case manifest.FormatClaude:
		return claudeMCPValue(server), nil
	case manifest.FormatCodex:
		return codexMCPValue(server), nil
	case manifest.FormatOpenCode:
		return openCodeMCPValue(server), nil
	default:
		return geminiMCPValue(server), nil
	}
}

// MCPPrefix returns the key path a dialect puts its servers under: the shared
// document key of the format, without the server name.
func MCPPrefix(format manifest.Format) string {
	switch format {
	case manifest.FormatCodex:
		return "mcp_servers."
	case manifest.FormatOpenCode:
		return "mcp."
	case manifest.FormatClaude, manifest.FormatGemini:
		return "mcpServers."
	default:
		return ""
	}
}

// MCPEdits returns one key-path edit per server for the shared config of a
// dialect, under the dialect's own container key.
func MCPEdits(format manifest.Format, servers []manifest.MCPServer) ([]Edit, error) {
	return MCPEditsUnder(format, MCPPrefix(format), servers)
}

// MCPEditsUnder returns one key-path edit per server below an explicit key
// prefix: OpenCode carries the same values under `mcp.servers.` (v2) and
// `mcp.` (v1), and the adapter picks the container the config already
// declares.
func MCPEditsUnder(format manifest.Format, prefix string, servers []manifest.MCPServer) ([]Edit, error) {
	switch format {
	case manifest.FormatClaude, manifest.FormatGemini, manifest.FormatCodex, manifest.FormatOpenCode:
	default:
		return nil, &RenderError{Kind: kindMCP, Cause: fmt.Errorf("format %s has no MCP config dialect", format)}
	}

	if prefix == "" {
		return nil, &RenderError{Kind: kindMCP, Cause: fmt.Errorf("format %s has no MCP container key", format)}
	}

	ordered := slices.Clone(servers)

	slices.SortFunc(ordered, func(a, b manifest.MCPServer) int { return strings.Compare(a.Name, b.Name) })

	seen := map[string]bool{}

	edits := make([]Edit, 0, len(ordered))

	for _, server := range ordered {
		if !validMCPName(server.Name) {
			return nil, &RenderError{Kind: kindMCP, Name: server.Name, Cause: errors.New("the server name must match [A-Za-z0-9._-]+")}
		}

		if seen[server.Name] {
			return nil, &RenderError{Kind: kindMCP, Name: server.Name, Cause: errors.New("duplicate MCP server name")}
		}

		seen[server.Name] = true

		value, err := EncodeMCP(format, server)
		if err != nil {
			return nil, err
		}

		edits = append(edits, Edit{Path: prefix + server.Name, Value: value})
	}

	return edits, nil
}

// ClaudeMCPAddArgs builds the `claude mcp add --scope user` argv of a server.
func ClaudeMCPAddArgs(server manifest.MCPServer) ([]string, error) {
	if strings.TrimSpace(server.Name) == "" {
		return nil, &RenderError{Kind: kindMCP, Name: server.Name, Cause: errors.New("the server name is required")}
	}

	if err := validateMCP(server); err != nil {
		return nil, err
	}

	args := []string{"mcp", "add", "--scope", "user", "--transport", claudeTransport(server)}
	args = append(args, server.Name)

	if server.URL != "" {
		args = append(args, server.URL)
	} else {
		args = append(args, server.Command...)
	}

	for _, key := range sortedKeys(server.Env) {
		args = append(args, "--env", key+"="+hostEnvRefs(server.Env[key]))
	}

	for _, key := range sortedKeys(server.Headers) {
		args = append(args, "--header", key+": "+hostEnvRefs(server.Headers[key]))
	}

	return args, nil
}

// ClaudeMCPRemoveArgs builds the `claude mcp remove --scope user` argv of a
// server name.
func ClaudeMCPRemoveArgs(name string) []string {
	return []string{"mcp", "remove", "--scope", "user", name}
}

// claudeMCPValue renders the Claude .mcp.json entry.
func claudeMCPValue(server manifest.MCPServer) map[string]any {
	if server.URL != "" {
		entry := map[string]any{"type": "http", "url": server.URL}

		if headers := convertedStrings(server.Headers, hostEnvRefs); len(headers) > 0 {
			entry["headers"] = headers
		}

		return entry
	}

	entry := map[string]any{keyType: transportStdio, keyCommand: server.Command[0]}

	if len(server.Command) > 1 {
		entry["args"] = server.Command[1:]
	}

	if env := convertedStrings(server.Env, hostEnvRefs); len(env) > 0 {
		entry["env"] = env
	}

	return entry
}

// codexMCPValue renders the Codex mcp_servers.<name> table entry. Headers are
// not part of the Codex MCP key set and are not written.
func codexMCPValue(server manifest.MCPServer) map[string]any {
	if server.URL != "" {
		return map[string]any{"url": server.URL}
	}

	entry := map[string]any{keyCommand: server.Command[0]}

	if len(server.Command) > 1 {
		entry["args"] = server.Command[1:]
	}

	if len(server.Env) > 0 {
		entry["env"] = server.Env
	}

	return entry
}

// geminiMCPValue renders the Gemini mcpServers.<name> entry: sse keeps `url`,
// streamable-http uses `httpUrl`.
func geminiMCPValue(server manifest.MCPServer) map[string]any {
	if server.URL != "" {
		entry := map[string]any{}

		if server.Transport == transportSSE {
			entry["url"] = server.URL
		} else {
			entry["httpUrl"] = server.URL
		}

		if headers := convertedStrings(server.Headers, hostEnvRefs); len(headers) > 0 {
			entry["headers"] = headers
		}

		return entry
	}

	entry := map[string]any{keyCommand: server.Command[0]}

	if len(server.Command) > 1 {
		entry["args"] = server.Command[1:]
	}

	if env := convertedStrings(server.Env, hostEnvRefs); len(env) > 0 {
		entry["env"] = env
	}

	return entry
}

// openCodeMCPValue renders the OpenCode/Kilo `mcp.<name>` entry: a `local`
// server with the command as one array and the environment map spelled
// `environment`, or a `remote` server with its url and headers. References
// stay verbatim (the host expands nothing).
func openCodeMCPValue(server manifest.MCPServer) map[string]any {
	if server.URL != "" {
		entry := map[string]any{"type": "remote", "url": server.URL}

		if len(server.Headers) > 0 {
			entry["headers"] = maps.Clone(server.Headers)
		}

		return entry
	}

	entry := map[string]any{"type": "local", "command": slices.Clone(server.Command)}

	if len(server.Env) > 0 {
		entry["environment"] = maps.Clone(server.Env)
	}

	return entry
}

// claudeTransport maps the canonical transport to the Claude CLI spelling.
func claudeTransport(server manifest.MCPServer) string {
	switch {
	case server.URL == "" || server.Transport == transportStdio:
		return transportStdio
	case server.Transport == transportSSE:
		return transportSSE
	default:
		return "http"
	}
}

// validateMCP rejects servers outside the canonical union: an unknown
// transport, neither a command nor a url, both, or an explicit transport that
// contradicts the shape (stdio is command-based; sse and streamable-http are
// url-based). An unset transport is inferred from the command/url shape.
func validateMCP(server manifest.MCPServer) error {
	switch server.Transport {
	case "", transportStdio, "streamable-http", transportSSE:
	default:
		return &RenderError{Kind: kindMCP, Name: server.Name, Cause: fmt.Errorf("unsupported transport %q", server.Transport)}
	}

	switch {
	case len(server.Command) == 0 && server.URL == "":
		return &RenderError{Kind: kindMCP, Name: server.Name, Cause: errors.New("the server has neither a command nor a url")}
	case len(server.Command) > 0 && server.URL != "":
		return &RenderError{Kind: kindMCP, Name: server.Name, Cause: errors.New("the server carries both a command and a url")}
	case server.Transport == transportStdio && server.URL != "":
		return &RenderError{Kind: kindMCP, Name: server.Name, Cause: errors.New("the stdio transport needs a command, not a url")}
	case server.Transport != "" && server.Transport != transportStdio && len(server.Command) > 0:
		return &RenderError{Kind: kindMCP, Name: server.Name, Cause: fmt.Errorf("the %s transport needs a url, not a command", server.Transport)}
	}

	return nil
}

// validMCPName reports whether a server name matches [A-Za-z0-9._-]+.
func validMCPName(name string) bool {
	if name == "" {
		return false
	}

	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}

	return true
}

// convertedStrings maps the values of a string map.
func convertedStrings(values map[string]string, transform func(string) string) map[string]string {
	if len(values) == 0 {
		return nil
	}

	out := make(map[string]string, len(values))

	for key, value := range values {
		out[key] = transform(value)
	}

	return out
}

// hostEnvRefs rewrites whole `{env:NAME}` references into the host `${NAME}`
// spelling; `{secret:NAME}` references stay verbatim.
func hostEnvRefs(value string) string {
	var b strings.Builder

	for i := 0; i < len(value); {
		if name, width, ok := cutEnvRef(value[i:]); ok {
			b.WriteString("${")
			b.WriteString(name)
			b.WriteString("}")

			i += width

			continue
		}

		b.WriteByte(value[i])
		i++
	}

	return b.String()
}

// cutEnvRef matches a leading `{env:NAME}` reference and returns its name and
// byte width.
func cutEnvRef(value string) (string, int, bool) {
	rest, ok := strings.CutPrefix(value, "{env:")
	if !ok {
		return "", 0, false
	}

	end := strings.IndexByte(rest, '}')
	if end <= 0 {
		return "", 0, false
	}

	name := rest[:end]
	if !validEnvName(name) {
		return "", 0, false
	}

	return name, len("{env:") + end + 1, true
}

// validEnvName reports whether a name matches [A-Za-z_][A-Za-z0-9_]*.
func validEnvName(name string) bool {
	if name == "" {
		return false
	}

	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}

	return true
}
