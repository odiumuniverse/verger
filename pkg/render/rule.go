package render

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	yaml "go.yaml.in/yaml/v3"
)

// maxSlugBytes caps the generated slug so "rule-"+slug stays inside the
// filesystem's 255-byte name bound.
const maxSlugBytes = 200

// RuleSkill wraps a rule document as one skill: rel is the package-relative
// skill directory ("skills/rule-<slug>"), content the SKILL.md bytes. The
// description is the first non-empty rule line, truncated to 120 characters.
// A slug longer than maxSlugBytes is truncated on a rune boundary and suffixed
// with six hex digits of its digest, so distinct long names keep distinct
// slugs within the filesystem name bound.
func RuleSkill(name string, rule []byte) (string, []byte, error) {
	slug := capSlug(slugify(name))
	if slug == "" {
		return "", nil, &RenderError{Kind: kindRule, Name: name, Cause: errors.New("the name yields an empty slug")}
	}

	body := normalizeBody(trimLeadingBlankLines(string(rule)))
	if strings.TrimSpace(body) == "" {
		return "", nil, &RenderError{Kind: kindRule, Name: name, Cause: errors.New("the rule is empty")}
	}

	description := firstLine(body)

	return "skills/rule-" + slug, composeFrontmatter([]field{
		{Key: keyName, Value: "rule-" + slug},
		{Key: keyDescription, Value: truncateRunes(description, 120)},
	}, body), nil
}

// ompRuleKeys is the frontmatter key order of an omp rulebook document
// (omp://rulebook-matching-pipeline.md#2): the keys omp reads come first in
// their documented order, every other source key keeps its value behind them.
var ompRuleKeys = []string{
	keyName, keyDescription, "globs", "alwaysApply", "condition",
	"ttsr_trigger", "astCondition", "question", "scope", "agents", "interruptMode",
}

// RuleMarkdown encodes a rule as an omp rulebook document
// (`<agentDir>/rules/<name>.md`): the source frontmatter is preserved and a
// non-empty description is guaranteed, because omp silently drops a rule that
// carries no condition, no alwaysApply and no description. The YAML this
// returns is well-formed, so omp never falls back to its flat line parser
// (which would lose nested values). The name stays whatever the source
// declares: omp derives it from the file name when the frontmatter has none.
func RuleMarkdown(name string, rule []byte) ([]byte, error) {
	front, body, ok := splitFrontmatter(rule)

	doc := map[string]any{}

	if ok && strings.TrimSpace(string(front)) != "" {
		if err := yaml.Unmarshal(front, &doc); err != nil {
			return nil, &RenderError{Kind: kindRule, Name: name, Cause: fmt.Errorf("parse frontmatter: %w", err)}
		}
	}

	if strings.TrimSpace(valueString(doc[keyDescription])) == "" {
		description := truncateRunes(firstLine(body), 120)
		if description == "" {
			return nil, &RenderError{Kind: kindRule, Name: name, Cause: errors.New("the rule is empty")}
		}

		doc[keyDescription] = description
	}

	return composeFrontmatter(orderedFields(doc, ompRuleKeys), trimLeadingBlankLines(body)), nil
}

// orderedFields renders a decoded frontmatter mapping: the keys of order first,
// in that order, then every other key sorted.
func orderedFields(doc map[string]any, order []string) []field {
	fields := make([]field, 0, len(doc))

	for _, key := range order {
		if value, ok := doc[key]; ok {
			fields = append(fields, field{Key: key, Value: value})
		}
	}

	rest := make([]string, 0, len(doc))

	for key := range doc {
		if !slices.Contains(order, key) {
			rest = append(rest, key)
		}
	}

	slices.Sort(rest)

	for _, key := range rest {
		fields = append(fields, field{Key: key, Value: doc[key]})
	}

	return fields
}

// valueString renders a decoded YAML value as the text of a scalar; a
// non-scalar has no text.
func valueString(value any) string {
	text, _ := value.(string)

	return text
}

// capSlug bounds one slug: a slug longer than maxSlugBytes is truncated on a
// rune boundary and suffixed with "-" plus six hex digits of the full slug, so
// distinct long slugs stay distinct.
func capSlug(slug string) string {
	if len(slug) <= maxSlugBytes {
		return slug
	}

	sum := sha256.Sum256([]byte(slug))
	suffix := "-" + hex.EncodeToString(sum[:])[:6]

	cut := maxSlugBytes - len(suffix)

	for cut > 0 && !utf8.RuneStart(slug[cut]) {
		cut--
	}

	return strings.TrimRight(slug[:cut], "-") + suffix
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
