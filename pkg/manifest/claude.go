package manifest

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// claudeMeta is the .claude-plugin/plugin.json shape.
type claudeMeta struct {
	Name        string                     `json:"name"`
	Version     string                     `json:"version"`
	Description string                     `json:"description"`
	Hooks       json.RawMessage            `json:"hooks"`
	MCPServers  map[string]json.RawMessage `json:"mcpServers"`
}

// parseClaude reads the Claude plugin layout: identity from
// .claude-plugin/plugin.json, payload skills/, agents/, commands/,
// hooks/hooks.json and .mcp.json.
func parseClaude(root string) (*parsed, error) {
	metaPath := filepath.Join(root, claudeMetaDir, metaFile)

	data, err := requiredEntry(metaPath, FormatClaude)
	if err != nil {
		return nil, err
	}

	meta := claudeMeta{}

	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, &ParseError{Format: FormatClaude, Path: metaPath, Cause: err}
	}

	out := &parsed{
		format:      FormatClaude,
		name:        meta.Name,
		named:       strings.TrimSpace(meta.Name) != "",
		version:     meta.Version,
		description: meta.Description,
	}

	out.components = append(out.components, scanSkills(root, filepath.Join(root, skillsDir), FormatClaude, &out.warnings)...)
	out.components = append(out.components, scanFiles(root, filepath.Join(root, agentsDir), markdownExt, KindAgent, FormatClaude, &out.warnings)...)
	out.components = append(out.components, scanFiles(root, filepath.Join(root, commandsDir), markdownExt, KindCommand, FormatClaude, &out.warnings)...)

	out.hooks = claudeHooks(root, meta, &out.warnings)
	out.mcp = claudeMCP(root, meta.MCPServers, &out.warnings)

	return out, nil
}

// claudeHooks merges hooks/hooks.json with the inline manifest hooks; the file
// wins when both declare hooks.
func claudeHooks(root string, meta claudeMeta, warnings *[]string) []Hook {
	rel := filepath.ToSlash(filepath.Join(hooksDir, hooksFile))

	fileHooks := hooksFromPath(FormatClaude, root, rel, warnings)
	inlineHooks := hooksFromInline(FormatClaude, root, filepath.ToSlash(filepath.Join(claudeMetaDir, metaFile)), meta.Hooks, warnings)

	switch {
	case len(fileHooks) > 0 && len(inlineHooks) > 0:
		*warnings = append(*warnings, warning(FormatClaude, rel, "both hooks/hooks.json and inline manifest hooks are present; using the file"))

		return fileHooks
	case len(fileHooks) > 0:
		return fileHooks
	default:
		return inlineHooks
	}
}

// claudeMCP merges .mcp.json with the inline manifest mcpServers; the file
// wins per server name.
func claudeMCP(root string, inline map[string]json.RawMessage, warnings *[]string) []MCPServer {
	fileServers, found := readMCPServers(FormatClaude, root, mcpFile, warnings)
	inlineServers := mcpServersFromMap(FormatClaude, filepath.ToSlash(filepath.Join(claudeMetaDir, metaFile)), inline, warnings)

	if !found || len(inlineServers) == 0 {
		return append(fileServers, inlineServers...)
	}

	names := map[string]bool{}

	for _, server := range fileServers {
		names[server.Name] = true
	}

	merged := fileServers

	for _, server := range inlineServers {
		if names[server.Name] {
			*warnings = append(*warnings, warning(FormatClaude, mcpFile,
				fmt.Sprintf("mcp server %s is declared inline and in %s; using the file", server.Name, mcpFile)))

			continue
		}

		merged = append(merged, server)
	}

	return merged
}
