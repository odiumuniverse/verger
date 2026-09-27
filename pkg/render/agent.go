package render

import (
	"errors"
	"slices"
	"strings"
)

// Canonical tool names shared by the agent renderers.
const (
	toolRead         = "Read"
	toolWrite        = "Write"
	toolEdit         = "Edit"
	toolNotebookEdit = "NotebookEdit"
)

// Agent is the canonical form of a custom subagent: a markdown document whose
// frontmatter carries the portable fields and whose body becomes the host's
// system prompt.
type Agent struct {
	Name            string
	Description     string
	Mode            string
	Model           string
	Tools           []string
	DisallowedTools []string
	PermissionMode  string
	MaxTurns        int
	Skills          []string
	MCPServers      []any
	Background      *bool
	Isolation       string
	Color           string
	Hidden          *bool
	Body            string
}

// ParseAgentMarkdown decodes a markdown subagent with optional YAML
// frontmatter. Unknown frontmatter keys are dropped; a missing block yields the
// body only.
func ParseAgentMarkdown(data []byte) (Agent, error) {
	var raw struct {
		Name            string     `yaml:"name"`
		Description     string     `yaml:"description"`
		Mode            string     `yaml:"mode"`
		Model           string     `yaml:"model"`
		Tools           stringList `yaml:"tools"`
		DisallowedTools stringList `yaml:"disallowedTools"`
		PermissionMode  string     `yaml:"permissionMode"`
		MaxTurns        int        `yaml:"maxTurns"`
		Skills          stringList `yaml:"skills"`
		MCPServers      []any      `yaml:"mcpServers"`
		Background      *bool      `yaml:"background"`
		Isolation       string     `yaml:"isolation"`
		Color           string     `yaml:"color"`
		Hidden          *bool      `yaml:"hidden"`
	}

	body, ok, err := unmarshalFrontmatter(data, &raw)
	if err != nil {
		return Agent{}, &RenderError{Kind: kindAgent, Cause: err}
	}

	agent := Agent{Body: normalizeBody(body)}
	if !ok {
		return agent, nil
	}

	agent.Name = strings.TrimSpace(raw.Name)
	agent.Description = raw.Description
	agent.Mode = raw.Mode
	agent.Model = raw.Model
	agent.Tools = raw.Tools
	agent.DisallowedTools = raw.DisallowedTools
	agent.PermissionMode = raw.PermissionMode
	agent.MaxTurns = raw.MaxTurns
	agent.Skills = raw.Skills
	agent.MCPServers = raw.MCPServers
	agent.Background = raw.Background
	agent.Isolation = raw.Isolation
	agent.Color = raw.Color
	agent.Hidden = raw.Hidden

	return agent, nil
}

// ClaudeMarkdown encodes the agent as canonical markdown: the non-empty fields
// in canonical order, then the body.
func (a Agent) ClaudeMarkdown() []byte {
	return composeFrontmatter(agentFields(a), a.Body)
}

// GeminiMarkdown encodes the agent as canonical markdown with the Gemini tool
// vocabulary; tools without a Gemini spelling are kept verbatim.
func (a Agent) GeminiMarkdown() []byte {
	out := a
	out.Tools = make([]string, len(a.Tools))

	for i, tool := range a.Tools {
		if name, ok := geminiTool(tool); ok {
			out.Tools[i] = name

			continue
		}

		out.Tools[i] = tool
	}

	return composeFrontmatter(agentFields(out), a.Body)
}

// CodexTOML encodes the agent as a Codex agent document: name, description,
// developer_instructions, model and sandbox_mode. Fields Codex cannot express
// are reported as joined *InexpressibleError values next to the rendered bytes;
// a body-less agent is inexpressible and returns no bytes.
func (a Agent) CodexTOML() ([]byte, error) {
	if strings.TrimSpace(a.Name) == "" {
		return nil, &RenderError{Kind: kindAgent, Name: a.Name, Cause: errors.New("subagent name is required")}
	}

	if strings.TrimSpace(a.Body) == "" {
		return nil, &InexpressibleError{Kind: kindAgent, Name: a.Name, Field: "developer_instructions"}
	}

	var b strings.Builder

	b.WriteString("name = ")
	b.WriteString(tomlBasicString(a.Name))
	b.WriteString("\n")

	if a.Description != "" {
		b.WriteString("description = ")
		b.WriteString(tomlBasicString(a.Description))
		b.WriteString("\n")
	}

	b.WriteString("developer_instructions = ")
	b.WriteString(tomlMultilineValue(a.Body))
	b.WriteString("\n")

	model := codexModel(a.Model)
	if model != "" {
		b.WriteString("model = ")
		b.WriteString(tomlBasicString(model))
		b.WriteString("\n")
	}

	readOnly := agentReadOnly(a)
	if readOnly {
		b.WriteString("sandbox_mode = \"read-only\"\n")
	}

	if dropped := a.droppedCodexFields(readOnly, model); len(dropped) > 0 {
		errs := make([]error, 0, len(dropped))

		for _, key := range dropped {
			errs = append(errs, &InexpressibleError{Kind: kindAgent, Name: a.Name, Field: key})
		}

		return []byte(b.String()), errors.Join(errs...)
	}

	return []byte(b.String()), nil
}

// agentFields renders the non-empty canonical frontmatter fields of an agent
// in order.
func agentFields(agent Agent) []field {
	candidates := []struct {
		key   string
		value any
		ok    bool
	}{
		{"name", agent.Name, agent.Name != ""},
		{"description", agent.Description, agent.Description != ""},
		{"mode", agent.Mode, agent.Mode != ""},
		{"model", agent.Model, agent.Model != ""},
		{"tools", agent.Tools, len(agent.Tools) > 0},
		{"disallowedTools", agent.DisallowedTools, len(agent.DisallowedTools) > 0},
		{"permissionMode", agent.PermissionMode, agent.PermissionMode != ""},
		{"maxTurns", agent.MaxTurns, agent.MaxTurns > 0},
		{"skills", agent.Skills, len(agent.Skills) > 0},
		{"mcpServers", agent.MCPServers, len(agent.MCPServers) > 0},
		{"background", boolValue(agent.Background), agent.Background != nil},
		{"isolation", agent.Isolation, agent.Isolation != ""},
		{"color", agent.Color, agent.Color != ""},
		{"hidden", boolValue(agent.Hidden), agent.Hidden != nil},
	}

	fields := make([]field, 0, len(candidates))

	for _, candidate := range candidates {
		if !candidate.ok {
			continue
		}

		fields = append(fields, field{Key: candidate.key, Value: candidate.value})
	}

	return fields
}

// droppedCodexFields lists the canonical fields the rendered document does not
// carry, in canonical order.
func (a Agent) droppedCodexFields(readOnly bool, model string) []string {
	candidates := []struct {
		key     string
		dropped bool
	}{
		{"mode", a.Mode != ""},
		{"tools", len(a.Tools) > 0},
		{"disallowedTools", len(a.DisallowedTools) > 0 && !readOnly},
		{"permissionMode", a.PermissionMode != ""},
		{"maxTurns", a.MaxTurns > 0},
		{"skills", len(a.Skills) > 0},
		{"mcpServers", len(a.MCPServers) > 0},
		{"background", a.Background != nil},
		{"isolation", a.Isolation != ""},
		{"color", a.Color != ""},
		{"hidden", a.Hidden != nil},
		{"model", a.Model != "" && model == ""},
	}

	var dropped []string

	for _, candidate := range candidates {
		if candidate.dropped {
			dropped = append(dropped, candidate.key)
		}
	}

	return dropped
}

// writeClass lists the canonical tools that modify files.
func writeClass() []string {
	return []string{toolWrite, toolEdit, toolNotebookEdit}
}

// agentReadOnly reports the derived read-only state: no write-class tool is
// available to the agent.
func agentReadOnly(agent Agent) bool {
	if agent.Tools != nil {
		for _, tool := range agent.Tools {
			if slices.Contains(writeClass(), tool) {
				return false
			}
		}

		return true
	}

	for _, tool := range writeClass() {
		if !slices.Contains(agent.DisallowedTools, tool) {
			return false
		}
	}

	return len(agent.DisallowedTools) > 0
}

// claudeAliases lists the Claude Code model aliases: they are host spellings
// and must not leak into other hosts' model fields.
func claudeAliases() []string {
	return []string{"haiku", "sonnet", "opus"}
}

// codexModel keeps a model Codex can read; omitting the key inherits.
func codexModel(model string) string {
	if model == "" || model == "inherit" || slices.Contains(claudeAliases(), model) {
		return ""
	}

	return model
}

// geminiTool maps a canonical tool name to the Gemini spelling, including the
// mcp_<server>_<tool> form when the server name allows it.
func geminiTool(tool string) (string, bool) {
	switch tool {
	case toolRead:
		return "read_file", true
	case toolWrite:
		return "write_file", true
	case toolEdit:
		return "replace", true
	case "Bash":
		return "run_shell_command", true
	case "Grep":
		return "search_file_content", true
	case "Glob":
		return "glob", true
	case "WebFetch":
		return "web_fetch", true
	case "WebSearch":
		return "google_web_search", true
	case "Skill":
		return "activate_skill", true
	}

	return geminiMCPName(tool)
}

// geminiMCPName maps a canonical mcp__<server>__<tool> name.
func geminiMCPName(tool string) (string, bool) {
	rest, ok := strings.CutPrefix(tool, "mcp__")
	if !ok {
		return "", false
	}

	server, name, ok := strings.Cut(rest, "__")
	if !ok || server == "" || name == "" || strings.Contains(server, "_") {
		return "", false
	}

	return "mcp_" + server + "_" + name, true
}
