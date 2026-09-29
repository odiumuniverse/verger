package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/digest"
)

// Payload layout names of the supported formats.
const (
	skillsDir   = "skills"
	agentsDir   = "agents"
	commandsDir = "commands"
	hooksDir    = "hooks"
	hooksFile   = "hooks.json"
	skillFile   = "SKILL.md"

	markdownExt = ".md"
	tomlExt     = ".toml"

	metaFile         = "plugin.json"
	claudeMetaDir    = ".claude-plugin"
	codexMetaDir     = ".codex-plugin"
	geminiMetaFile   = "gemini-extension.json"
	mcpFile          = ".mcp.json"
	portableMCPFile  = "mcp.json"
	mcpServersKey    = "mcpServers"
	transportStdio   = "stdio"
	transportStream  = "streamable-http"
	transportSSE     = "sse"
	transportHTTPLeg = "http" // the Claude spelling of streamable-http
)

// skipJunk reports whether a tree entry is junk that never joins a digest.
func skipJunk(rel string, _ bool) bool {
	switch path.Base(rel) {
	case ".git", ".DS_Store", "node_modules", "__pycache__", "Thumbs.db":
		return true
	default:
		return false
	}
}

// hasSkillRoot reports whether dir directly holds a regular SKILL.md.
func hasSkillRoot(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	for _, entry := range entries {
		if entry.Name() == skillFile {
			return entry.Type().IsRegular()
		}
	}

	return false
}

// scanSkills lists the immediate skill directories below dir.
func scanSkills(root, dir string, format Format, warnings *[]string) []Component {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		*warnings = append(*warnings, warning(format, relOrRoot(root, dir), "cannot read directory: "+err.Error()))

		return nil
	}

	var components []Component

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillRoot := filepath.Join(dir, entry.Name())
		if !hasSkillRoot(skillRoot) {
			continue
		}

		sum, err := digest.TreeWithSkip(skillRoot, skipJunk)
		if err != nil {
			*warnings = append(*warnings, warning(format, relOrRoot(root, skillRoot), "cannot digest: "+err.Error()))

			continue
		}

		rel, ok := relOrEmpty(root, skillRoot)
		if !ok {
			*warnings = append(*warnings, warning(format, relOrRoot(root, skillRoot), "path escapes the package root; ignored"))

			continue
		}

		components = append(components, Component{Kind: KindSkill, Name: entry.Name(), Path: rel, Digest: sum})
	}

	return components
}

// scanFiles lists the regular files of dir with the extension as components.
func scanFiles(root, dir, ext string, kind Kind, format Format, warnings *[]string) []Component {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		*warnings = append(*warnings, warning(format, relOrRoot(root, dir), "cannot read directory: "+err.Error()))

		return nil
	}

	var components []Component

	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasSuffix(name, ext) {
			continue
		}

		filePath := filepath.Join(dir, name)

		sum, err := digest.File(filePath)
		if err != nil {
			*warnings = append(*warnings, warning(format, relOrRoot(root, filePath), "cannot digest: "+err.Error()))

			continue
		}

		rel, ok := relOrEmpty(root, filePath)
		if !ok {
			*warnings = append(*warnings, warning(format, relOrRoot(root, filePath), "path escapes the package root; ignored"))

			continue
		}

		components = append(components, Component{
			Kind:   kind,
			Name:   strings.TrimSuffix(name, ext),
			Path:   rel,
			Digest: sum,
		})
	}

	return components
}

// declaredSkills resolves manifest-declared skill paths: a skill root becomes
// one component, a container directory is scanned for child skills.
func declaredSkills(root string, format Format, file string, rels []string, warnings *[]string) []Component {
	var components []Component

	for _, declared := range rels {
		dir, ok := localPath(root, declared)
		if !ok {
			*warnings = append(*warnings, warning(format, file, fmt.Sprintf("skill path %q escapes the package root; ignored", declared)))

			continue
		}

		if hasSkillRoot(dir) {
			sum, err := digest.TreeWithSkip(dir, skipJunk)
			if err != nil {
				*warnings = append(*warnings, warning(format, file, fmt.Sprintf("skill path %q cannot be digested: %v", declared, err)))

				continue
			}

			rel, ok := relOrEmpty(root, dir)
			if !ok {
				*warnings = append(*warnings, warning(format, file, fmt.Sprintf("skill path %q escapes the package root; ignored", declared)))

				continue
			}

			components = append(components, Component{Kind: KindSkill, Name: filepath.Base(dir), Path: rel, Digest: sum})

			continue
		}

		components = append(components, scanSkills(root, dir, format, warnings)...)
	}

	return components
}

// localPath resolves one plugin-relative path; ok is false when the path
// escapes the package root, including through a symlink.
func localPath(root, rel string) (string, bool) {
	if !filepath.IsLocal(rel) {
		return "", false
	}

	joined := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(rel, "./")))

	// A symlink inside the package can point outside it. Resolve both the
	// candidate and the root (on macOS, TempDir is a symlink to /private/var)
	// before comparing, so a path under a symlinked root is not wrongly
	// rejected.
	if resolved, err := filepath.EvalSymlinks(joined); err == nil {
		if resolvedRoot, err := filepath.EvalSymlinks(root); err == nil {
			if !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) && resolved != resolvedRoot {
				return "", false
			}
		} else if !strings.HasPrefix(resolved, root+string(filepath.Separator)) && resolved != root {
			return "", false
		}
	}

	return joined, true
}

// relOrEmpty renders a slash-relative path below root; ok is false when the
// path escapes it.
func relOrEmpty(root, target string) (string, bool) {
	rel, err := filepath.Rel(root, target)
	if err != nil || !filepath.IsLocal(rel) {
		return "", false
	}

	return filepath.ToSlash(rel), true
}

// relOrRoot renders target relative to root, falling back to the base name.
func relOrRoot(root, target string) string {
	rel, ok := relOrEmpty(root, target)
	if !ok {
		return filepath.Base(target)
	}

	return rel
}

// stringList decodes a JSON field that is a string or a list of strings.
type stringList []string

// UnmarshalJSON implements json.Unmarshaler.
func (l *stringList) UnmarshalJSON(data []byte) error {
	var one string

	if err := json.Unmarshal(data, &one); err == nil {
		if one == "" {
			*l = nil
		} else {
			*l = stringList{one}
		}

		return nil
	}

	var many []string

	if err := json.Unmarshal(data, &many); err != nil {
		return errors.New("expected a string or a list of strings")
	}

	filtered := make([]string, 0, len(many))

	for _, item := range many {
		if item != "" {
			filtered = append(filtered, item)
		}
	}

	*l = stringList(filtered)

	return nil
}

// mcpEntry is one raw MCP server of a document.
type mcpEntry struct {
	Type    string            `json:"type"`
	Command stringList        `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	HTTPURL string            `json:"httpUrl"`
	Headers map[string]string `json:"headers"`
}

// mcpServersFromDocument decodes a `{"mcpServers": {...}}` document.
func mcpServersFromDocument(format Format, file string, data []byte, warnings *[]string) []MCPServer {
	if !isJSONObject(data) {
		*warnings = append(*warnings, warning(format, file, "mcp document is not an object; ignored"))

		return nil
	}

	var raw map[string]json.RawMessage

	if err := json.Unmarshal(data, &raw); err != nil {
		*warnings = append(*warnings, warning(format, file, "parse mcp document: "+err.Error()))

		return nil
	}

	value, ok := raw[mcpServersKey]
	if !ok {
		return nil
	}

	if !isJSONObject(value) {
		*warnings = append(*warnings, warning(format, file, "mcpServers is not an object; ignored"))

		return nil
	}

	servers := map[string]json.RawMessage{}

	if err := json.Unmarshal(value, &servers); err != nil {
		*warnings = append(*warnings, warning(format, file, "mcpServers is not an object; ignored"))

		return nil
	}

	return mcpServersFromMap(format, file, servers, warnings)
}

// mcpServersFromMap normalizes a name-keyed map of raw server declarations.
func mcpServersFromMap(format Format, file string, raw map[string]json.RawMessage, warnings *[]string) []MCPServer {
	var servers []MCPServer

	for _, name := range slices.Sorted(maps.Keys(raw)) {
		var entry mcpEntry

		if err := json.Unmarshal(raw[name], &entry); err != nil {
			*warnings = append(*warnings, warning(format, file, fmt.Sprintf("mcp server %s is not an object; skipped", name)))

			continue
		}

		server, ok := mcpServerFromEntry(format, file, name, entry, warnings)
		if ok {
			servers = append(servers, server)
		}
	}

	return servers
}

// mcpServerFromEntry infers the transport and reports servers with no command
// and no url, or an unsupported transport.
func mcpServerFromEntry(format Format, file, name string, entry mcpEntry, warnings *[]string) (MCPServer, bool) {
	command := entry.Command
	if len(entry.Args) > 0 {
		command = append(slices.Clone(entry.Command), entry.Args...)
	}

	url := entry.URL
	if url == "" {
		url = entry.HTTPURL
	}

	if len(command) == 0 && url == "" {
		*warnings = append(*warnings, warning(format, file, fmt.Sprintf("mcp server %s has neither command nor url; skipped", name)))

		return MCPServer{}, false
	}

	switch entry.Type {
	case "", transportStdio, transportStream, transportSSE, transportHTTPLeg:
	default:
		*warnings = append(*warnings, warning(format, file, fmt.Sprintf("mcp server %s has unsupported transport %q; skipped", name, entry.Type)))

		return MCPServer{}, false
	}

	transport := ""

	switch {
	case entry.Type == transportSSE:
		transport = transportSSE
	case entry.Type == transportStream || entry.Type == transportHTTPLeg:
		transport = transportStream
	case entry.Type == transportStdio:
		transport = transportStdio
	case url != "":
		transport = transportStream
	default:
		transport = transportStdio
	}

	return MCPServer{
		Name:      name,
		Transport: transport,
		Command:   command,
		Env:       entry.Env,
		URL:       url,
		Headers:   entry.Headers,
	}, true
}

// readMCPServers reads an optional MCP document; found reports whether the file
// exists at all, so callers can implement a candidate order.
func readMCPServers(format Format, root, rel string, warnings *[]string) ([]MCPServer, bool) {
	data, state, err := readEntry(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		*warnings = append(*warnings, warning(format, rel, "cannot read: "+err.Error()))

		return nil, false
	}

	switch state {
	case entryFile:
	case entryMissing:
		return nil, false
	case entrySymlink:
		*warnings = append(*warnings, warning(format, rel, "symlink is never followed; ignored"))

		return nil, false
	default:
		*warnings = append(*warnings, warning(format, rel, "not a regular file; ignored"))

		return nil, false
	}

	return mcpServersFromDocument(format, rel, data, warnings), true
}

// requiredEntry reads a primary manifest; any failure is a *ParseError.
func requiredEntry(path string, format Format) ([]byte, error) {
	data, state, err := readEntry(path)
	if err != nil {
		return nil, &ParseError{Format: format, Path: path, Cause: err}
	}

	switch state {
	case entryFile:
		return data, nil
	case entryMissing:
		return nil, &ParseError{Format: format, Path: path, Cause: errors.New("missing primary manifest")}
	case entrySymlink:
		return nil, &ParseError{Format: format, Path: path, Cause: errors.New("primary manifest is a symlink; ignored")}
	default:
		return nil, &ParseError{Format: format, Path: path, Cause: errors.New("primary manifest is not a regular file")}
	}
}

// isJSONObject reports whether data is a JSON object literal.
func isJSONObject(data []byte) bool {
	trimmed := bytes.TrimSpace(data)

	return len(trimmed) > 0 && trimmed[0] == '{'
}
