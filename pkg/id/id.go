// Package id owns the single id grammar of verger: canonical package ids and
// single path elements (hosts, versions, trash buckets). It is a leaf package
// and never imports other verger packages.
package id

import (
	"fmt"
	"strings"
)

// InvalidIDError reports a package id or path element that violates the
// grammar.
type InvalidIDError struct {
	Value  string
	Reason string
}

// Error implements error.
func (e *InvalidIDError) Error() string {
	return fmt.Sprintf("invalid id %q: %s", e.Value, e.Reason)
}

// ValidatePackage reports whether pkg is a canonical package id: one or more
// `/`-separated segments followed by at most one `//` separator and a
// non-empty subpath (`owner/repo//skills/foo`, DESIGN §3.4). Segments use
// letters, digits, `.`, `_`, `-`, `+`, `:` and `@`; empty segments, `.`, `..`,
// a leading/trailing `/`, backslashes, NUL bytes and absolute paths are
// rejected.
func ValidatePackage(pkg string) error {
	switch {
	case pkg == "":
		return invalid(pkg, "empty package id")
	case strings.IndexByte(pkg, 0) >= 0:
		return invalid(pkg, "NUL byte")
	case strings.Contains(pkg, `\`):
		return invalid(pkg, "backslash")
	case strings.HasPrefix(pkg, "/"):
		return invalid(pkg, "absolute path")
	case strings.HasSuffix(pkg, "/"):
		return invalid(pkg, "trailing slash")
	}

	base, sub, hasSub := strings.Cut(pkg, "//")

	if err := validateSegments(pkg, base); err != nil {
		return err
	}

	if !hasSub {
		return nil
	}

	if sub == "" {
		return invalid(pkg, "empty subpath")
	}

	return validateSegments(pkg, sub)
}

// ValidateElement reports whether value is safe as one path element (host,
// version, trash bucket): non-empty, not `.`/`..`, and only letters, digits,
// `.`, `_`, `-` and `+`.
func ValidateElement(value string) bool {
	switch value {
	case "", ".", "..":
		return false
	}

	for i := range len(value) {
		if !elementByte(value[i]) {
			return false
		}
	}

	return true
}

// validateSegments rejects the first unsafe segment of one id part.
func validateSegments(pkg, part string) error {
	for segment := range strings.SplitSeq(part, "/") {
		if reason := segmentReason(segment); reason != "" {
			return invalid(pkg, reason)
		}
	}

	return nil
}

// segmentReason explains why segment is not a safe package id segment; an
// empty string means it is safe.
func segmentReason(segment string) string {
	switch segment {
	case "":
		return "empty path segment"
	case ".", "..":
		return "parent directory segment"
	}

	for i := range len(segment) {
		if !packageSegmentByte(segment[i]) {
			return fmt.Sprintf("disallowed character %q", segment[i])
		}
	}

	return ""
}

// packageSegmentByte reports whether c is allowed inside a package id segment.
func packageSegmentByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '.', c == '_', c == '-', c == '+', c == ':', c == '@':
		return true
	default:
		return false
	}
}

// elementByte reports whether c is allowed inside a single path element.
func elementByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '.', c == '_', c == '-', c == '+':
		return true
	default:
		return false
	}
}

// invalid builds one grammar violation.
func invalid(value, reason string) *InvalidIDError {
	return &InvalidIDError{Value: value, Reason: reason}
}
