package manifest

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
)

// codexMeta is the union of the Codex manifest shapes: the portable root
// plugin.json and the legacy .codex-plugin/plugin.json overlay.
type codexMeta struct {
	Name        string                     `json:"name"`
	Version     string                     `json:"version"`
	Description string                     `json:"description"`
	Skills      stringList                 `json:"skills"`
	Extensions  map[string]json.RawMessage `json:"extensions"`
	Hooks       json.RawMessage            `json:"hooks"`
}

// parseCodex reads the Codex plugin layout: identity from the portable root
// plugin.json falling back to .codex-plugin/plugin.json, payload skills/ (or
// the manifest-declared paths), agents/, commands/, hooks and the portable or
// legacy MCP document.
func parseCodex(root string) (*parsed, error) {
	primary := filepath.Join(root, metaFile)
	legacy := filepath.Join(root, codexMetaDir, metaFile)

	data, state, err := readEntry(primary)
	if err != nil {
		return nil, &ParseError{Format: FormatCodex, Path: primary, Cause: err}
	}

	metaPath, rel := primary, metaFile

	if state != entryFile {
		data, state, err = readEntry(legacy)
		if err != nil {
			return nil, &ParseError{Format: FormatCodex, Path: legacy, Cause: err}
		}

		metaPath, rel = legacy, filepath.ToSlash(filepath.Join(codexMetaDir, metaFile))
	}

	switch state {
	case entryFile:
	case entrySymlink:
		return nil, &ParseError{Format: FormatCodex, Path: metaPath, Cause: errors.New("primary manifest is a symlink; ignored")}
	default:
		return nil, &ParseError{Format: FormatCodex, Path: primary, Cause: errors.New("missing primary manifest")}
	}

	meta := codexMeta{}

	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, &ParseError{Format: FormatCodex, Path: metaPath, Cause: err}
	}

	out := &parsed{
		format:      FormatCodex,
		name:        meta.Name,
		named:       strings.TrimSpace(meta.Name) != "",
		version:     meta.Version,
		description: meta.Description,
	}

	if len(meta.Skills) > 0 {
		out.components = append(out.components, declaredSkills(root, FormatCodex, rel, meta.Skills, &out.warnings)...)
	} else {
		out.components = append(out.components, scanSkills(root, filepath.Join(root, skillsDir), FormatCodex, &out.warnings)...)
	}

	out.components = append(out.components, scanFiles(root, filepath.Join(root, agentsDir), markdownExt, KindAgent, FormatCodex, &out.warnings)...)
	out.components = append(out.components, scanFiles(root, filepath.Join(root, commandsDir), markdownExt, KindCommand, FormatCodex, &out.warnings)...)

	out.hooks = codexHooks(root, rel, meta, &out.warnings)
	out.mcp = codexMCP(root, &out.warnings)

	return out, nil
}

// codexHooks reads the Codex hook locations: the explicit
// extensions.com.openai.hooks declaration wins, then the legacy manifest hooks
// field, then the default hooks/hooks.json.
func codexHooks(root, rel string, meta codexMeta, warnings *[]string) []Hook {
	if raw := openaiHooks(meta.Extensions, rel, warnings); len(raw) > 0 {
		return hookSources(FormatCodex, root, rel, raw, warnings)
	}

	if len(meta.Hooks) > 0 {
		return hookSources(FormatCodex, root, rel, meta.Hooks, warnings)
	}

	return hooksFromPath(FormatCodex, root, filepath.ToSlash(filepath.Join(hooksDir, hooksFile)), warnings)
}

// openaiHooks returns the raw extensions.com.openai.hooks declaration.
func openaiHooks(extensions map[string]json.RawMessage, file string, warnings *[]string) json.RawMessage {
	raw, ok := extensions["com.openai"]
	if !ok {
		return nil
	}

	var openai struct {
		Hooks json.RawMessage `json:"hooks"`
	}

	if err := json.Unmarshal(raw, &openai); err != nil {
		*warnings = append(*warnings, warning(FormatCodex, file, "extensions.com.openai is not an object; ignored"))

		return nil
	}

	return openai.Hooks
}

// codexMCP reads the first existing MCP document: the portable mcp.json wins
// over .mcp.json.
func codexMCP(root string, warnings *[]string) []MCPServer {
	for _, rel := range []string{portableMCPFile, mcpFile} {
		if servers, found := readMCPServers(FormatCodex, root, rel, warnings); found {
			return servers
		}
	}

	return nil
}
