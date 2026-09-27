package render

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"
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
		{Key: "name", Value: "rule-" + slug},
		{Key: "description", Value: truncateRunes(description, 120)},
	}, body), nil
}

// capSlug bounds one slug: a slug longer than maxSlugBytes is truncated on a
// rune boundary and suffixed with "-" plus six hex digits of the full slug, so
// distinct long names stay distinct.
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
