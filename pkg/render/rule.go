package render

import (
	"errors"
	"strings"
)

// RuleSkill wraps a rule document as one skill: rel is the package-relative
// skill directory ("skills/rule-<slug>"), content the SKILL.md bytes. The
// description is the first non-empty rule line, truncated to 120 characters.
func RuleSkill(name string, rule []byte) (string, []byte, error) {
	slug := slugify(name)
	if slug == "" {
		return "", nil, &RenderError{Kind: kindRule, Name: name, Cause: errors.New("the name yields an empty slug")}
	}

	body := normalizeBody(trimLeadingBlankLines(string(rule)))
	if strings.TrimSpace(body) == "" {
		return "", nil, &RenderError{Kind: kindRule, Name: name, Cause: errors.New("the rule is empty")}
	}

	description := firstLine(body)

	return "skills/rule-" + slug, composeFrontmatter([]field{
		{Key: "name", Value: "rule-" + slug},
		{Key: "description", Value: truncateRunes(description, 120)},
	}, body), nil
}

// firstLine returns the first non-empty line of a document, trimmed.
func firstLine(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}

	return ""
}

// trimLeadingBlankLines drops the blank lines a rule document starts with.
func trimLeadingBlankLines(body string) string {
	lines := strings.Split(body, "\n")

	start := 0

	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}

	return strings.Join(lines[start:], "\n")
}

// truncateRunes cuts a string to at most limit runes.
func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}

	return string(runes[:limit])
}
