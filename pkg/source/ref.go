// Package source parses package references and fetches their bytes: the
// §2.1 grammar (github, git, pinned archives, npm, local dirs, mcp/agent refs)
// into the store cache with commit/tree pins and a normalized manifest.
package source

import (
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"github.com/odiumuniverse/verger/pkg/digest"
)

// maxRefLen bounds the parser input; longer strings are rejected outright.
const maxRefLen = 4096

// Kind classifies one package reference.
type Kind string

// Reference kinds of the §2.1 grammar.
const (
	KindGitHub Kind = "github"
	KindGit    Kind = "git"
	KindURL    Kind = "url"
	KindNPM    Kind = "npm"
	KindLocal  Kind = "local"
	KindMCP    Kind = "mcp"
	KindAgent  Kind = "agent"
)

// Ref is one parsed package reference.
type Ref struct {
	Kind    Kind
	Raw     string
	ID      string // canonical owner/name[/subpath] when derivable
	Owner   string
	Repo    string
	Subpath string // //path

	Rev    string      // branch/tag/commit (github/git)
	URL    string      // git URL or archive URL; may carry credentials — render via String()
	SHA256 digest.Hash // required for archives

	NPM  string // full npm spec (scope/name@version)
	Path string // local: absolute directory

	Agent    string // host id (host.ID once T1.6 lands)
	AgentRef string
	MCP      string
}

// Parse parses one reference of the §2.1 grammar.
func Parse(input string) (Ref, error) {
	switch {
	case input == "":
		return Ref{}, &RefError{Input: input, Reason: "empty reference"}
	case len(input) > maxRefLen:
		return Ref{}, &RefError{Input: input, Reason: "reference is too long"}
	case strings.IndexByte(input, 0) >= 0:
		return Ref{}, &RefError{Input: input, Reason: "reference contains a NUL byte"}
	}

	switch {
	case strings.HasPrefix(input, "github:"):
		return parseGitHub(input, strings.TrimPrefix(input, "github:"))
	case strings.HasPrefix(input, "git+"):
		return parseGitURL(input)
	case strings.HasPrefix(input, "npm:"):
		return parseNPM(input)
	case strings.HasPrefix(input, "mcp:"):
		return parseMCP(input)
	case strings.HasPrefix(input, "file://"),
		strings.HasPrefix(input, "http://"),
		strings.HasPrefix(input, "https://"):
		return parseArchiveURL(input)
	case strings.HasPrefix(input, "./"):
		return parseLocal(input)
	}

	if host, rest, ok := strings.Cut(input, ":"); ok {
		if !knownHost(host) || rest == "" {
			return Ref{}, &RefError{Input: input, Reason: "unknown scheme or host"}
		}

		return Ref{Kind: KindAgent, Raw: input, ID: rest, Agent: host, AgentRef: rest}, nil
	}

	return parseRegistryID(input)
}

// ParseAll parses refs in order and stops at the first invalid one.
func ParseAll(inputs []string) ([]Ref, error) {
	refs := make([]Ref, 0, len(inputs))

	for _, input := range inputs {
		ref, err := Parse(input)
		if err != nil {
			return nil, err
		}

		refs = append(refs, ref)
	}

	return refs, nil
}

// String renders the reference without credentials.
func (r Ref) String() string {
	if r.Raw != "" {
		return sanitizeText(r.Raw)
	}

	switch r.Kind {
	case KindGitHub:
		if r.ID != "" {
			return r.ID
		}

		return "github:" + r.Owner + "/" + r.Repo
	case KindGit:
		return "git+" + sanitizeText(r.URL) + revFragment(r.Rev)
	case KindURL:
		return sanitizeText(r.URL) + "#sha256=" + string(r.SHA256)
	case KindNPM:
		return "npm:" + r.NPM
	case KindLocal:
		return r.Path
	case KindMCP:
		return "mcp:" + r.MCP
	case KindAgent:
		return r.Agent + ":" + r.AgentRef
	default:
		return string(r.Kind)
	}
}

// revFragment renders the optional git fragment.
func revFragment(rev string) string {
	if rev == "" {
		return ""
	}

	return "#" + rev
}

// parseRegistryID parses `name`, `owner/repo` and `owner/repo//subpath`.
func parseRegistryID(input string) (Ref, error) {
	base, sub, hasSub := strings.Cut(input, "//")

	if hasSub {
		if err := validateSubpath(input, sub); err != nil {
			return Ref{}, err
		}
	}

	segments := strings.Split(base, "/")

	switch len(segments) {
	case 1:
		if hasSub {
			return Ref{}, &RefError{Input: input, Reason: "a subpath needs an owner/repo id"}
		}

		if !validSegment(segments[0]) {
			return Ref{}, &RefError{Input: input, Reason: "invalid short name"}
		}

		return Ref{Kind: KindGitHub, Raw: input, ID: segments[0], Repo: segments[0]}, nil
	case 2:
		owner, repo := segments[0], strings.TrimSuffix(segments[1], ".git")
		if !validSegment(owner) || !validSegment(repo) {
			return Ref{}, &RefError{Input: input, Reason: "invalid owner/repo id"}
		}

		id := owner + "/" + repo
		if hasSub {
			id += "//" + sub
		}

		return Ref{
			Kind: KindGitHub, Raw: input, ID: id, Owner: owner, Repo: repo,
			Subpath: sub, URL: githubURL(owner, repo),
		}, nil
	default:
		return Ref{}, &RefError{Input: input, Reason: "expected owner/repo"}
	}
}

// parseGitHub parses `github:owner/repo[//path][@ref]`.
func parseGitHub(input, rest string) (Ref, error) {
	body, rev := rest, ""

	if idx := strings.LastIndex(rest, "@"); idx >= 0 {
		body, rev = rest[:idx], rest[idx+1:]
		if rev == "" {
			return Ref{}, &RefError{Input: input, Reason: "empty ref after @"}
		}
	}

	base, sub, hasSub := strings.Cut(body, "//")

	if hasSub {
		if err := validateSubpath(input, sub); err != nil {
			return Ref{}, err
		}
	}

	segments := strings.Split(base, "/")
	if len(segments) != 2 {
		return Ref{}, &RefError{Input: input, Reason: "github refs need owner/repo"}
	}

	owner, repo := segments[0], strings.TrimSuffix(segments[1], ".git")
	if !validSegment(owner) || !validSegment(repo) {
		return Ref{}, &RefError{Input: input, Reason: "invalid owner/repo id"}
	}

	id := owner + "/" + repo
	if hasSub {
		id += "//" + sub
	}

	return Ref{
		Kind: KindGitHub, Raw: input, ID: id, Owner: owner, Repo: repo,
		Subpath: sub, Rev: rev, URL: githubURL(owner, repo),
	}, nil
}

// parseGitURL parses `git+https://…[#ref]`.
func parseGitURL(input string) (Ref, error) {
	parsed, err := url.Parse(strings.TrimPrefix(input, "git+"))
	if err != nil {
		return Ref{}, &RefError{Input: input, Reason: "malformed git url"}
	}

	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return Ref{}, &RefError{Input: input, Reason: "git refs need a git+https url"}
	}

	rev := parsed.Fragment
	parsed.Fragment = ""

	id := deriveGitID(parsed.Path)
	if id == "" {
		return Ref{}, &RefError{Input: input, Reason: "cannot derive an owner/repo id from the git url"}
	}

	return Ref{
		Kind: KindGit, Raw: input, ID: id,
		URL: parsed.String(), Rev: rev,
	}, nil
}

// parseArchiveURL parses `https://…#sha256=…` and `file://…#sha256=…`.
func parseArchiveURL(input string) (Ref, error) {
	parsed, err := url.Parse(input)
	if err != nil {
		return Ref{}, &RefError{Input: input, Reason: "malformed archive url"}
	}

	switch parsed.Scheme {
	case "https", "http", "file":
	default:
		return Ref{}, &RefError{Input: input, Reason: "archives need an https, http or file url"}
	}

	pin := ""

	if parsed.Fragment != "" {
		values, queryErr := url.ParseQuery(parsed.Fragment)
		if queryErr != nil {
			return Ref{}, &RefError{Input: input, Reason: "malformed archive fragment"}
		}

		pin = values.Get("sha256")
		parsed.Fragment = ""
	}

	if pin == "" {
		return Ref{}, &RefError{Input: input, Reason: "archive refs need a #sha256= pin"}
	}

	hash, err := digest.Parse(pin)
	if err != nil {
		return Ref{}, &RefError{Input: input, Reason: "invalid sha256 pin"}
	}

	return Ref{Kind: KindURL, Raw: input, ID: path.Base(parsed.Path), URL: parsed.String(), SHA256: hash}, nil
}

// parseNPM parses `npm:name@version` (scoped names allowed).
func parseNPM(input string) (Ref, error) {
	spec := strings.TrimPrefix(input, "npm:")

	idx := strings.LastIndex(spec, "@")
	if idx <= 0 || idx == len(spec)-1 {
		return Ref{}, &RefError{Input: input, Reason: "npm refs need <name>@<version>"}
	}

	name := spec[:idx]
	if !validNPMName(name) {
		return Ref{}, &RefError{Input: input, Reason: "invalid npm package name"}
	}

	return Ref{Kind: KindNPM, Raw: input, ID: spec, NPM: spec}, nil
}

// parseMCP parses `mcp:<registry id>`.
func parseMCP(input string) (Ref, error) {
	id := strings.TrimPrefix(input, "mcp:")
	if id == "" || strings.Contains(id, "..") {
		return Ref{}, &RefError{Input: input, Reason: "invalid mcp registry id"}
	}

	return Ref{Kind: KindMCP, Raw: input, ID: id, MCP: id}, nil
}

// parseLocal parses `./relative/path` into an absolute directory; `..` never
// appears in a local ref.
func parseLocal(input string) (Ref, error) {
	for segment := range strings.SplitSeq(input, "/") {
		if segment == ".." {
			return Ref{}, &RefError{Input: input, Reason: "local refs must not contain .."}
		}
	}

	clean := filepath.Clean(input)
	if clean == "." {
		return Ref{}, &RefError{Input: input, Reason: "local refs need a path"}
	}

	abs, err := filepath.Abs(clean)
	if err != nil {
		return Ref{}, &RefError{Input: input, Reason: "cannot resolve the local path"}
	}

	return Ref{Kind: KindLocal, Raw: input, ID: filepath.Base(abs), Path: abs}, nil
}

// githubURL renders the canonical clone URL of a GitHub repo.
func githubURL(owner, repo string) string {
	return "https://github.com/" + owner + "/" + repo + ".git"
}

// deriveGitID derives owner/repo from a git URL path when possible.
func deriveGitID(urlPath string) string {
	trimmed := strings.Trim(path.Clean(urlPath), "/")
	segments := strings.Split(trimmed, "/")

	if len(segments) < 2 {
		return ""
	}

	owner := segments[len(segments)-2]
	repo := strings.TrimSuffix(segments[len(segments)-1], ".git")

	if !validSegment(owner) || !validSegment(repo) {
		return ""
	}

	return owner + "/" + repo
}

// validateSubpath checks a `//path` subpath for traversal and shape.
func validateSubpath(input, sub string) error {
	if sub == "" {
		return &RefError{Input: input, Reason: "empty subpath"}
	}

	for segment := range strings.SplitSeq(sub, "/") {
		if !validSegment(segment) {
			return &RefError{Input: input, Reason: "invalid subpath"}
		}
	}

	return nil
}

// validSegment reports whether one id/path segment is safe.
func validSegment(segment string) bool {
	if segment == "" || segment == "." || segment == ".." {
		return false
	}

	for i := range len(segment) {
		c := segment[i]

		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-', c == '+':
		default:
			return false
		}
	}

	return true
}

// validNPMName reports whether an npm name is well-shaped (scope allowed).
func validNPMName(name string) bool {
	trimmed := strings.TrimPrefix(name, "@")

	for segment := range strings.SplitSeq(trimmed, "/") {
		if !validSegment(segment) {
			return false
		}
	}

	return true
}

// knownHost reports whether prefix names an agent host of the fixed list
// (T1.6 will own the registry).
func knownHost(prefix string) bool {
	switch prefix {
	case "claude", "codex", "gemini", "agy", "cursor", "opencode", "kilo", "pi", "dsh":
		return true
	default:
		return false
	}
}

// skipGitDir is the digest skip for .git directories.
func skipGitDir(rel string, _ bool) bool {
	return rel == ".git" || strings.HasPrefix(rel, ".git/")
}

// sanitizeText redacts credentials from text: URL userinfo is dropped and the
// entire query of an http(s) URL is removed (scheme, host, path and fragment
// remain). Text without "://" is returned unchanged.
func sanitizeText(text string) string {
	if !strings.Contains(text, "://") {
		return text
	}

	var out strings.Builder

	for {
		idx := strings.Index(text, "://")
		if idx < 0 {
			out.WriteString(text)

			return out.String()
		}

		start := schemeStart(text, idx)
		end := urlEnd(text, idx+len("://"))

		out.WriteString(text[:start])
		out.WriteString(redactURL(text[start:end]))

		text = text[end:]
	}
}

// schemeStart walks back from the "://" separator to the first scheme byte.
func schemeStart(text string, sep int) int {
	start := sep

	for start > 0 && isSchemeByte(text[start-1]) {
		start--
	}

	return start
}

// isSchemeByte reports whether c may appear in a URL scheme.
func isSchemeByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '+' || c == '.' || c == '-'
}

// urlEnd returns the first byte after the URL token: whitespace and angle
// brackets terminate it.
func urlEnd(text string, from int) int {
	for i := from; i < len(text); i++ {
		switch text[i] {
		case ' ', '\t', '\n', '\r', '<', '>':
			return i
		}
	}

	return len(text)
}

// redactURL rewrites one scheme:// candidate: userinfo is dropped and an
// http(s) scheme also loses its whole query.
func redactURL(candidate string) string {
	scheme, rest, found := strings.Cut(candidate, "://")
	if !found {
		return candidate
	}

	authority, tail := rest, ""

	if idx := strings.IndexAny(rest, "/?#"); idx >= 0 {
		authority, tail = rest[:idx], rest[idx:]
	}

	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		authority = authority[at+1:]
	}

	if isHTTPScheme(scheme) {
		tail = stripQuery(tail)
	}

	return scheme + "://" + authority + tail
}

// isHTTPScheme reports whether scheme is http, https or their git+ forms.
func isHTTPScheme(scheme string) bool {
	switch strings.ToLower(scheme) {
	case "http", "https", "git+http", "git+https":
		return true
	default:
		return false
	}
}

// stripQuery removes the query part of a path-and-more suffix, keeping any
// fragment.
func stripQuery(tail string) string {
	query := strings.IndexByte(tail, '?')
	if query < 0 {
		return tail
	}

	fragment := strings.IndexByte(tail[query:], '#')
	if fragment < 0 {
		return tail[:query]
	}

	return tail[:query] + tail[query+fragment:]
}

// RefError reports an unparsable or unpinned reference. Input keeps the raw
// text for programmatic use; Error never renders userinfo or a URL query.
type RefError struct {
	Input  string
	Reason string
}

// Error implements error.
func (e *RefError) Error() string {
	return fmt.Sprintf("invalid source ref %q: %s", sanitizeText(e.Input), e.Reason)
}
