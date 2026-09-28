package render

import (
	"errors"
	"regexp"
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

// OpenCode/Kilo agent dialect values and keys, verified live on kilo 7.8.1
// (`kilo debug agent`): the host derives the agent id from the file name, and
// it drops the whole document when `mode` is outside its three values, when
// `color` is not a hex color, or when `tools` is not a map — which is exactly
// what a Claude-style `tools` list is.
const (
	openCodeModePrimary  = "primary"
	openCodeModeSubagent = "subagent"
	openCodeModeAll      = "all"

	openCodeKeySteps = "steps"
)

// openCodeModes lists the values the host accepts for `mode`.
func openCodeModes() []string {
	return []string{openCodeModePrimary, openCodeModeSubagent, openCodeModeAll}
}

// openCodeColorRE matches the color values the host keeps; a theme name is not
// verifiable without the host's theme table, so only hex colors are written.
var openCodeColorRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// openCodeModelRE matches the `provider/model[#variant]` form the host
// resolves; a bare alias (a Claude spelling) is dropped instead of written.
var openCodeModelRE = regexp.MustCompile(`^[^/#]+/[^#]+(?:#[^#]+)?$`)

// OpenCode agent permission dialect (ported from beadle's codec, W3-RUNTIME
// answer 3): the v2 schema carries a `permissions` array of
// {action, resource, effect} rules instead of the legacy `tools` map, and a
// v1 key would make the host fall back to its legacy decoder.
const (
	openCodeKeyPermissions = "permissions"
	openCodePermAllow      = "allow"
	openCodePermDeny       = "deny"
	openCodeMCPPrefix      = "mcp__"
)

// openCodeRule is one permission rule of the v2 agent schema.
type openCodeRule struct {
	Action   string `yaml:"action"`
	Resource string `yaml:"resource"`
	Effect   string `yaml:"effect"`
}

// openCodeToolActions maps a canonical tool name to the OpenCode action it
// belongs to; the write class collapses into one action.
func openCodeToolActions() map[string]string {
	return map[string]string{
		toolRead:          "read",
		toolWrite:         "edit",
		toolEdit:          "edit",
		toolNotebookEdit:  "edit",
		"Bash":            "shell",
		"Grep":            "grep",
		"Glob":            "glob",
		"WebFetch":        "webfetch",
		"WebSearch":       "websearch",
		"Task":            "subagent",
		"Agent":           "subagent",
		"Skill":           "skill",
		"AskUserQuestion": "question",
	}
}

// openCodeActionForTool resolves one canonical tool name — including the
// mcp__<server>__<tool> form — to the action the host knows.
func openCodeActionForTool(tool string) (string, bool) {
	if action, ok := openCodeToolActions()[tool]; ok {
		return action, true
	}

	rest, ok := strings.CutPrefix(tool, openCodeMCPPrefix)
	if !ok {
		return "", false
	}

	server, name, ok := strings.Cut(rest, "__")
	if !ok || server == "" || name == "" {
		return "", false
	}

	for _, r := range server {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return "", false
		}
	}

	return server + "_" + name, true
}

// openCodePermissions renders the permissions array: the allowlist envelope
// (`deny *` first when the canonical agent carries one), the allowed actions,
// and the denied ones. A tool the dialect has no action for is reported back
// as dropped instead of being written as a rule that never fires.
func openCodePermissions(agent Agent) ([]openCodeRule, []string) {
	var (
		rules   []openCodeRule
		unknown []string
	)

	allowed := map[string]bool{}

	for _, tool := range agent.Tools {
		action, ok := openCodeActionForTool(tool)
		if !ok {
			unknown = append(unknown, tool)

			continue
		}

		if allowed[action] {
			continue
		}

		allowed[action] = true

		rules = append(rules, openCodeRule{Action: action, Resource: "*", Effect: openCodePermAllow})
	}

	// An allowlist is expressed as "deny everything, then allow the listed
	// actions"; an empty allowlist therefore denies every tool, which is the
	// safe projection of an allowlist the host cannot fully express.
	if agent.Tools != nil {
		rules = append([]openCodeRule{{Action: "*", Resource: "*", Effect: openCodePermDeny}}, rules...)
	}

	denied := map[string]bool{}

	for _, tool := range agent.DisallowedTools {
		action, ok := openCodeActionForTool(tool)
		if !ok {
			unknown = append(unknown, tool)

			continue
		}

		denied[action] = true
	}

	// A read-only agent: an allowlist that carries no write-class tool.
	if agent.Tools != nil && !slices.ContainsFunc(agent.Tools, func(tool string) bool {
		return slices.Contains(writeClass(), tool)
	}) {
		denied["edit"] = true
	}

	for _, action := range sortedMapKeys(denied) {
		if allowed[action] {
			continue
		}

		rules = append(rules, openCodeRule{Action: action, Resource: "*", Effect: openCodePermDeny})
	}

	return rules, unknown
}

// sortedMapKeys returns the sorted keys of one bool map.
func sortedMapKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))

	for key := range values {
		out = append(out, key)
	}

	slices.Sort(out)

	return out
}

// OpenCodeMarkdown encodes the agent in the OpenCode/Kilo agent dialect: the
// keys the host reads (description, mode, model, steps, color, hidden, the
// permissions array), in canonical order. The canonical name is not written —
// the host derives the id from the file name. Every canonical field the
// dialect cannot carry comes back as a joined *InexpressibleError next to the
// rendered bytes, so the delivery reports it instead of writing a value the
// host would drop the whole document over.
func (a Agent) OpenCodeMarkdown() ([]byte, error) {
	fields, dropped := a.openCodeFields()

	doc := composeFrontmatter(fields, a.Body)

	if len(dropped) == 0 {
		return doc, nil
	}

	errs := make([]error, 0, len(dropped))

	for _, name := range dropped {
		errs = append(errs, &InexpressibleError{Kind: kindAgent, Name: a.Name, Field: name})
	}

	return doc, errors.Join(errs...)
}

// openCodeFields renders the dialect fields and names every canonical field it
// had to drop.
func (a Agent) openCodeFields() ([]field, []string) {
	var (
		fields  []field
		dropped []string
	)

	add := func(key string, value any) {
		fields = append(fields, field{Key: key, Value: value})
	}

	if a.Description != "" {
		add(keyDescription, a.Description)
	}

	add("mode", a.openCodeMode(&dropped))

	if a.Model != "" {
		if openCodeModelRE.MatchString(a.Model) {
			add(keyModel, a.Model)
		} else {
			dropped = append(dropped, keyModel)
		}
	}

	if a.MaxTurns > 0 {
		add(openCodeKeySteps, a.MaxTurns)
	}

	if a.Color != "" {
		if openCodeColorRE.MatchString(a.Color) {
			add("color", a.Color)
		} else {
			dropped = append(dropped, "color")
		}
	}

	if a.Hidden != nil {
		add("hidden", *a.Hidden)
	}

	if a.Tools != nil || len(a.DisallowedTools) > 0 {
		rules, unknown := openCodePermissions(a)

		if len(rules) > 0 {
			add(openCodeKeyPermissions, rules)
		}

		dropped = append(dropped, unknown...)
	}

	for _, candidate := range []struct {
		key     string
		dropped bool
	}{
		{"permissionMode", a.PermissionMode != ""},
		{"skills", len(a.Skills) > 0},
		{"mcpServers", len(a.MCPServers) > 0},
		{"background", a.Background != nil},
		{"isolation", a.Isolation != ""},
	} {
		if candidate.dropped {
			dropped = append(dropped, candidate.key)
		}
	}

	return fields, dropped
}

// openCodeMode is the mode the host accepts: its own three values, or the
// safest explicit one when the canonical value is something else — an invalid
// mode makes the host drop the whole document.
func (a Agent) openCodeMode(dropped *[]string) string {
	if slices.Contains(openCodeModes(), a.Mode) {
		return a.Mode
	}

	if a.Mode != "" {
		*dropped = append(*dropped, "mode")
	}

	return openCodeModeSubagent
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
		{keyName, agent.Name, agent.Name != ""},
		{keyDescription, agent.Description, agent.Description != ""},
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
		{keyModel, a.Model != "" && model == ""},
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
