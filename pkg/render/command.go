package render

import (
	"errors"
	"fmt"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// Command is the canonical form of a custom command: a markdown document whose
// frontmatter carries the portable fields and whose body is the prompt
// template. The command name is the file stem and is never rendered into the
// frontmatter.
type Command struct {
	Name                   string
	Description            string
	ArgumentHint           string
	Arguments              []string
	Model                  string
	DisableModelInvocation *bool
	Body                   string
}

// ParseCommandMarkdown decodes a markdown command with optional YAML
// frontmatter. Unknown frontmatter keys are dropped; a missing block yields the
// body only.
func ParseCommandMarkdown(data []byte) (Command, error) {
	var raw struct {
		Name                   string     `yaml:"name"`
		Description            string     `yaml:"description"`
		ArgumentHint           string     `yaml:"argument-hint"`
		Arguments              stringList `yaml:"arguments"`
		Model                  string     `yaml:"model"`
		DisableModelInvocation *bool      `yaml:"disable-model-invocation"`
	}

	body, ok, err := unmarshalFrontmatter(data, &raw)
	if err != nil {
		return Command{}, fmt.Errorf("parse frontmatter: %w", err)
	}

	cmd := Command{Body: normalizeBody(body)}
	if !ok {
		return cmd, nil
	}

	cmd.Name = strings.TrimSpace(raw.Name)
	cmd.Description = raw.Description
	cmd.ArgumentHint = raw.ArgumentHint
	cmd.Arguments = raw.Arguments
	cmd.Model = raw.Model
	cmd.DisableModelInvocation = raw.DisableModelInvocation

	return cmd, nil
}

// Markdown encodes the command as canonical markdown: the non-empty fields in
// canonical order, then the body. The name is not part of the frontmatter.
func (c Command) Markdown() []byte {
	return composeFrontmatter(commandFields(c), c.Body)
}

// CodexPrompt returns the plain markdown prompt of the command.
func (c Command) CodexPrompt() []byte {
	return []byte(normalizeBody(c.Body))
}

// GeminiTOML encodes the command as a Gemini command document: a required
// prompt and an optional description. A body-less command is inexpressible for
// Gemini.
func (c Command) GeminiTOML() ([]byte, error) {
	if strings.TrimSpace(c.Body) == "" {
		return nil, &InexpressibleError{Kind: kindCommand, Name: c.Name, Field: "prompt"}
	}

	var b strings.Builder

	if c.Description != "" {
		b.WriteString("description = ")
		b.WriteString(tomlBasicString(c.Description))
		b.WriteString("\n")
	}

	b.WriteString("prompt = ")
	b.WriteString(tomlMultilineValue(c.Body))
	b.WriteString("\n")

	return []byte(b.String()), nil
}

// LiftGeminiCommand converts a Gemini command document into canonical markdown
// so the rest of verger speaks the canon dialect before rendering to a host.
// ok is false for a name that is not a command slug or a document without a
// prompt.
func LiftGeminiCommand(name string, data []byte) ([]byte, bool, error) {
	if !validSlug(name) {
		return nil, false, nil
	}

	var doc struct {
		Description string `toml:"description"`
		Prompt      string `toml:"prompt"`
	}

	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, false, fmt.Errorf("parse toml: %w", err)
	}

	if strings.TrimSpace(doc.Prompt) == "" {
		return nil, false, errors.New("the prompt key is required")
	}

	cmd := Command{Name: name, Description: doc.Description, Body: normalizeBody(doc.Prompt)}

	return cmd.Markdown(), true, nil
}

// commandFields renders the non-empty canonical frontmatter fields of a
// command in order.
func commandFields(cmd Command) []field {
	candidates := []struct {
		key   string
		value any
		ok    bool
	}{
		{"description", cmd.Description, cmd.Description != ""},
		{"argument-hint", cmd.ArgumentHint, cmd.ArgumentHint != ""},
		{"arguments", cmd.Arguments, len(cmd.Arguments) > 0},
		{"model", cmd.Model, cmd.Model != ""},
		{"disable-model-invocation", boolValue(cmd.DisableModelInvocation), cmd.DisableModelInvocation != nil},
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

// boolValue dereferences an optional bool for rendering.
func boolValue(value *bool) any {
	if value == nil {
		return nil
	}

	return *value
}
