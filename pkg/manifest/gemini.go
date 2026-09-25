package manifest

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// geminiMeta is the gemini-extension.json shape.
type geminiMeta struct {
	Name        string                     `json:"name"`
	Version     string                     `json:"version"`
	Description string                     `json:"description"`
	MCPServers  map[string]json.RawMessage `json:"mcpServers"`
}

// parseGemini reads the Gemini extension layout: identity from
// gemini-extension.json, payload skills/, agents/, commands/*.toml,
// hooks/hooks.json and the inline mcpServers.
func parseGemini(root string) (*parsed, error) {
	metaPath := filepath.Join(root, geminiMetaFile)

	data, err := requiredEntry(metaPath, FormatGemini)
	if err != nil {
		return nil, err
	}

	meta := geminiMeta{}

	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, &ParseError{Format: FormatGemini, Path: metaPath, Cause: err}
	}

	out := &parsed{
		format:      FormatGemini,
		name:        meta.Name,
		named:       strings.TrimSpace(meta.Name) != "",
		version:     meta.Version,
		description: meta.Description,
	}

	out.components = append(out.components, scanSkills(root, filepath.Join(root, skillsDir), FormatGemini, &out.warnings)...)
	out.components = append(out.components, scanFiles(root, filepath.Join(root, agentsDir), markdownExt, KindAgent, FormatGemini, &out.warnings)...)
	out.components = append(out.components, scanFiles(root, filepath.Join(root, commandsDir), tomlExt, KindCommand, FormatGemini, &out.warnings)...)

	out.hooks = hooksFromPath(FormatGemini, root, filepath.ToSlash(filepath.Join(hooksDir, hooksFile)), &out.warnings)
	out.mcp = mcpServersFromMap(FormatGemini, geminiMetaFile, meta.MCPServers, &out.warnings)

	return out, nil
}
