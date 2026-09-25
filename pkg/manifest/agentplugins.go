package manifest

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// agentPluginsSchemaURL is the Agent Plugins v1.0.0 plugin schema.
const agentPluginsSchemaURL = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"

// agentPluginsMeta is the closed plugin.json shape of Agent Plugins v1.0.0.
type agentPluginsMeta struct {
	Schema      string `json:"$schema"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

// parseAgentPlugins reads the Agent Plugins layout: identity from the root
// plugin.json carrying the Agent Plugins schema, payload skills/ and mcp.json.
func parseAgentPlugins(root string) (*parsed, error) {
	metaPath := filepath.Join(root, metaFile)

	data, err := requiredEntry(metaPath, FormatAgentPlugins)
	if err != nil {
		return nil, err
	}

	meta := agentPluginsMeta{}

	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, &ParseError{Format: FormatAgentPlugins, Path: metaPath, Cause: err}
	}

	if meta.Schema != agentPluginsSchemaURL {
		return nil, &ParseError{
			Format: FormatAgentPlugins,
			Path:   metaPath,
			Cause:  fmt.Errorf("$schema %q is not the Agent Plugins schema %q", meta.Schema, agentPluginsSchemaURL),
		}
	}

	out := &parsed{
		format:      FormatAgentPlugins,
		name:        meta.Name,
		named:       strings.TrimSpace(meta.Name) != "",
		version:     meta.Version,
		description: meta.Description,
	}

	out.components = append(out.components, scanSkills(root, filepath.Join(root, skillsDir), FormatAgentPlugins, &out.warnings)...)
	out.mcp, _ = readMCPServers(FormatAgentPlugins, root, portableMCPFile, &out.warnings)

	return out, nil
}

// agentPluginsManifest reports whether root/plugin.json declares the Agent
// Plugins schema.
func agentPluginsManifest(root string) bool {
	data, state, err := readEntry(filepath.Join(root, metaFile))
	if err != nil || state != entryFile {
		return false
	}

	var meta struct {
		Schema string `json:"$schema"`
	}

	if json.Unmarshal(data, &meta) != nil {
		return false
	}

	return meta.Schema == agentPluginsSchemaURL
}
