package manifest

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Reference is one plugin-root path reference found in a document.
type Reference struct {
	Pointer string // JSON pointer
	Path    string // absolute path the reference resolves to
}

// References finds plugin-root paths in any JSON document: tokens starting
// with root or with its home-relative tilde form. The walk never resolves
// symlinks or cleans climbing paths, and the result is sorted by pointer and
// path.
func References(data []byte, root, home string) ([]Reference, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("empty root directory")
	}

	var tree any

	if err := json.Unmarshal(data, &tree); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}

	tilde := tildeRoot(root, home)

	var refs []Reference

	collectRefs(&refs, tree, "", root, tilde, home)

	slices.SortFunc(refs, func(a, b Reference) int {
		return cmp.Or(cmp.Compare(a.Pointer, b.Pointer), cmp.Compare(a.Path, b.Path))
	})

	return slices.Compact(refs), nil
}

// tildeRoot returns the "~/…" form of root when root lives below home.
func tildeRoot(root, home string) string {
	if home == "" {
		return ""
	}

	rel, err := filepath.Rel(home, root)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return ""
	}

	return "~/" + filepath.ToSlash(rel)
}

// collectRefs walks a decoded JSON tree in sorted key order.
func collectRefs(refs *[]Reference, node any, pointer, root, tilde, home string) {
	switch typed := node.(type) {
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(typed)) {
			collectRefs(refs, typed[key], pointer+"/"+escapePointer(key), root, tilde, home)
		}
	case []any:
		for i, item := range typed {
			collectRefs(refs, item, pointer+"/"+strconv.Itoa(i), root, tilde, home)
		}
	case string:
		*refs = append(*refs, extractRefs(typed, pointer, root, tilde, home)...)
	}
}

// escapePointer escapes one JSON pointer token.
func escapePointer(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")

	return strings.ReplaceAll(token, "/", "~1")
}

// extractRefs returns the distinct root-prefixed tokens of one string.
func extractRefs(s, pointer, root, tilde, home string) []Reference {
	var refs []Reference

	seen := map[string]bool{}

	for offset := 0; offset < len(s); {
		start, length := findRoot(s, offset, root, tilde)
		if start < 0 {
			break
		}

		end := tokenEnd(s, start+length)
		token := s[start:end]

		offset = end

		if strings.Contains(token, "${") {
			continue
		}

		target, ok := tokenPath(token, root, tilde, home)
		if !ok {
			continue
		}

		key := pointer + "\x00" + target
		if seen[key] {
			continue
		}

		seen[key] = true

		refs = append(refs, Reference{Pointer: pointer, Path: target})
	}

	return refs
}

// findRoot returns the earliest root prefix in s at or after offset.
func findRoot(s string, offset int, root, tilde string) (int, int) {
	best, length := -1, 0

	prefixes := []string{root}
	if tilde != "" {
		prefixes = append(prefixes, tilde)
	}

	for _, prefix := range prefixes {
		idx := strings.Index(s[offset:], prefix)
		if idx < 0 {
			continue
		}

		if at := offset + idx; best < 0 || at < best {
			best, length = at, len(prefix)
		}
	}

	return best, length
}

// tokenEnd returns the end of the path token starting at start.
func tokenEnd(s string, start int) int {
	for i, r := range s[start:] {
		if refStop(r) {
			return start + i
		}
	}

	return len(s)
}

// refStop reports whether r ends a path token.
func refStop(r rune) bool {
	switch r {
	case '"', '\'', '`', ',', ';', '|', '>', ')', '}':
		return true
	default:
		return unicode.IsSpace(r)
	}
}

// tokenPath maps a token to its absolute path, verbatim for root tokens and
// home-relative for tilde tokens.
func tokenPath(token, root, tilde, home string) (string, bool) {
	if prefixBoundary(token, root) {
		return token, true
	}

	if tilde != "" && prefixBoundary(token, tilde) {
		return filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(token, "~/"))), true
	}

	return "", false
}

// prefixBoundary reports whether token is the prefix itself or continues it
// with a slash.
func prefixBoundary(token, prefix string) bool {
	rest, ok := strings.CutPrefix(token, prefix)
	if !ok {
		return false
	}

	return rest == "" || strings.HasPrefix(rest, "/")
}
