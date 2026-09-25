// Package manifest parses agent plugin manifests into one normalized Package:
// the Claude, Codex, Gemini and Agent Plugins layouts, their components (skill
// trees, agent and command files), MCP servers and hook definitions. Parsers
// are pure over the filesystem: they read, never write, and never follow
// symlinks below the package root.
package manifest

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/odiumuniverse/verger/pkg/digest"
)

// Kind classifies one component of a package payload.
type Kind string

// Component kinds of the normalized model.
const (
	KindSkill       Kind = "skill"
	KindMCP         Kind = "mcp"
	KindAgent       Kind = "agent"
	KindCommand     Kind = "command"
	KindHook        Kind = "hook"
	KindRule        Kind = "rule"
	KindLSP         Kind = "lsp"
	KindStyle       Kind = "style"
	KindTheme       Kind = "theme"
	KindNativeJS    Kind = "native-js"
	KindNativeTS    Kind = "native-ts"
	KindNativeOther Kind = "native-other"
)

// Format identifies one plugin manifest layout.
type Format string

// Manifest formats supported in Ф1.
const (
	FormatClaude       Format = "claude"
	FormatCodex        Format = "codex"
	FormatGemini       Format = "gemini"
	FormatAgentPlugins Format = "agent-plugins"
)

// Component is one payload item: a skill tree, an agent file, a command file.
type Component struct {
	Kind   Kind
	Name   string      // logical name: skill dir, agent/command file stem
	Path   string      // slash-relative to the package root
	Digest digest.Hash // tree digest for skills, file digest otherwise
}

// MCPServer is one MCP server declaration. Env and Headers keep
// `{secret:NAME}` references verbatim; resolution is the caller's job.
type MCPServer struct {
	Name      string
	Transport string // stdio|streamable-http|sse, "" = infer
	Command   []string
	Env       map[string]string
	URL       string
	Headers   map[string]string
}

// Hook is one hook handler in the canonical event vocabulary.
type Hook struct {
	Event   string // canonical event (see the Event* constants)
	Matcher string
	Command string
	Timeout int
	Origin  Format // dialect the hook was read from
}

// Package is the normalized view of one plugin directory.
type Package struct {
	ID          string
	Name        string
	Version     string
	Description string
	Format      Format // identity-defining format
	Root        string
	Components  []Component
	MCP         []MCPServer
	Hooks       []Hook
	Warnings    []string
}

// ErrUnknownFormat reports that no supported manifest exists under a root.
var ErrUnknownFormat = errors.New("unknown plugin format")

// ParseError reports a missing or invalid primary manifest.
type ParseError struct {
	Format Format
	Path   string
	Cause  error
}

// Error implements error.
func (e *ParseError) Error() string {
	return fmt.Sprintf("parse %s manifest %s: %v", e.Format, e.Path, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *ParseError) Unwrap() error {
	return e.Cause
}

// MergeConflictError reports a component or MCP server declared differently by
// two formats of one directory.
type MergeConflictError struct {
	Kind    Kind
	Name    string
	Formats []Format
}

// Error implements error.
func (e *MergeConflictError) Error() string {
	names := make([]string, 0, len(e.Formats))

	for _, format := range e.Formats {
		names = append(names, string(format))
	}

	return fmt.Sprintf("merge packages: %s %q is defined differently by %s", e.Kind, e.Name, strings.Join(names, ", "))
}

// Option configures Parse and ParseAny.
type Option func(*config)

// config carries option values of one Parse call.
type config struct {
	id    string
	idSet bool
}

// WithID overrides the package id (owner/name); the default derives from the
// manifest name.
func WithID(id string) Option {
	return func(c *config) {
		c.id = id
		c.idSet = true
	}
}

// parsed is one format reader result before the identity defaults are applied.
type parsed struct {
	format      Format
	name        string
	named       bool
	version     string
	description string
	components  []Component
	mcp         []MCPServer
	hooks       []Hook
	warnings    []string
}

// Parse reads one plugin directory in the requested format. A missing or
// invalid primary manifest is a *ParseError; broken optional payloads are
// warnings.
func Parse(root string, format Format, opts ...Option) (*Package, error) {
	cfg := config{}

	for _, opt := range opts {
		opt(&cfg)
	}

	result, err := parseFormat(root, format)
	if err != nil {
		return nil, err
	}

	return finish(result, root, cfg), nil
}

// Detect returns the formats whose primary manifest exists under root, in the
// fixed order [claude, codex, gemini, agent-plugins].
func Detect(root string) []Format {
	if !isDir(root) {
		return nil
	}

	var formats []Format

	if regularFile(filepath.Join(root, claudeMetaDir, metaFile)) {
		formats = append(formats, FormatClaude)
	}

	if regularFile(filepath.Join(root, metaFile)) || regularFile(filepath.Join(root, codexMetaDir, metaFile)) {
		formats = append(formats, FormatCodex)
	}

	if regularFile(filepath.Join(root, geminiMetaFile)) {
		formats = append(formats, FormatGemini)
	}

	if agentPluginsManifest(root) {
		formats = append(formats, FormatAgentPlugins)
	}

	return formats
}

// ParseAny parses every detected format and merges the results: identical
// components and MCP servers collapse, hook definitions are deduplicated and
// disagreeing definitions report *MergeConflictError. No detected format is
// ErrUnknownFormat.
func ParseAny(root string, opts ...Option) (*Package, error) {
	cfg := config{}

	for _, opt := range opts {
		opt(&cfg)
	}

	formats := Detect(root)
	if len(formats) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnknownFormat, root)
	}

	results := make([]*parsed, 0, len(formats))

	for _, format := range formats {
		result, err := parseFormat(root, format)
		if err != nil {
			return nil, err
		}

		results = append(results, result)
	}

	merged, err := mergeParsed(results)
	if err != nil {
		return nil, err
	}

	return finish(merged, root, cfg), nil
}

// parseFormat validates the root and dispatches to one format reader.
func parseFormat(root string, format Format) (*parsed, error) {
	if !isDir(root) {
		info, err := os.Stat(root)
		if err != nil {
			return nil, &ParseError{Format: format, Path: root, Cause: err}
		}

		return nil, &ParseError{Format: format, Path: root, Cause: fmt.Errorf("%s is not a directory", info.Name())}
	}

	switch format {
	case FormatClaude:
		return parseClaude(root)
	case FormatCodex:
		return parseCodex(root)
	case FormatGemini:
		return parseGemini(root)
	case FormatAgentPlugins:
		return parseAgentPlugins(root)
	default:
		return nil, &ParseError{Format: format, Path: root, Cause: fmt.Errorf("unknown format %q", format)}
	}
}

// finish applies the identity defaults, sorts the collections and builds the
// public Package.
func finish(result *parsed, root string, cfg config) *Package {
	name := result.name
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(filepath.Clean(root))
	}

	pkg := &Package{
		ID:          defaultID(name),
		Name:        name,
		Version:     result.version,
		Description: result.description,
		Format:      result.format,
		Root:        filepath.Clean(root),
		Components:  sortComponents(result.components),
		MCP:         sortMCP(result.mcp),
		Hooks:       sortHooks(result.hooks),
		Warnings:    result.warnings,
	}

	if cfg.idSet {
		pkg.ID = cfg.id
	}

	return pkg
}

// defaultID renders the id of a manifest name: a single-slash owner/name stays
// verbatim, everything else is local.
func defaultID(name string) string {
	if strings.Count(name, "/") == 1 && !strings.HasPrefix(name, "/") && !strings.HasSuffix(name, "/") {
		return name
	}

	return "local:" + name
}

// mergeParsed unions the per-format results in detection order.
func mergeParsed(results []*parsed) (*parsed, error) {
	out := &parsed{format: results[0].format}

	for _, result := range results {
		if result.named {
			out.format = result.format
			out.name = result.name
			out.version = result.version
			out.description = result.description
			out.named = true

			break
		}
	}

	if err := mergeComponents(out, results); err != nil {
		return nil, err
	}

	if err := mergeServers(out, results); err != nil {
		return nil, err
	}

	out.hooks = mergeHooks(results)

	for _, result := range results {
		out.warnings = append(out.warnings, result.warnings...)
	}

	return out, nil
}

// mergeComponents collapses equal components and reports digest disagreement.
func mergeComponents(out *parsed, results []*parsed) error {
	components := map[string]Component{}
	formats := map[string]Format{}

	for _, result := range results {
		for _, component := range result.components {
			key := componentKey(component)

			existing, ok := components[key]
			if !ok {
				components[key] = component
				formats[key] = result.format

				continue
			}

			if existing.Digest != component.Digest {
				return &MergeConflictError{
					Kind:    component.Kind,
					Name:    component.Name,
					Formats: []Format{formats[key], result.format},
				}
			}
		}
	}

	for _, component := range components {
		out.components = append(out.components, component)
	}

	return nil
}

// mergeServers collapses equal MCP servers and reports definition
// disagreement.
func mergeServers(out *parsed, results []*parsed) error {
	servers := map[string]MCPServer{}
	formats := map[string]Format{}

	for _, result := range results {
		for _, server := range result.mcp {
			existing, ok := servers[server.Name]
			if !ok {
				servers[server.Name] = server
				formats[server.Name] = result.format

				continue
			}

			if !reflect.DeepEqual(existing, server) {
				return &MergeConflictError{
					Kind:    KindMCP,
					Name:    server.Name,
					Formats: []Format{formats[server.Name], result.format},
				}
			}
		}
	}

	for _, server := range servers {
		out.mcp = append(out.mcp, server)
	}

	return nil
}

// mergeHooks deduplicates hook definitions, keeping the first dialect.
func mergeHooks(results []*parsed) []Hook {
	seen := map[string]bool{}

	var hooks []Hook

	for _, result := range results {
		for _, hook := range result.hooks {
			key := hookKey(hook)
			if seen[key] {
				continue
			}

			seen[key] = true

			hooks = append(hooks, hook)
		}
	}

	return hooks
}

// componentKey is the component identity used for deduplication.
func componentKey(component Component) string {
	return string(component.Kind) + "\x00" + component.Path
}

// hookKey is the hook identity used for deduplication.
func hookKey(hook Hook) string {
	return strings.Join([]string{hook.Event, hook.Matcher, hook.Command, strconv.Itoa(hook.Timeout)}, "\x00")
}

// sortComponents orders components by (Kind, Name, Path).
func sortComponents(components []Component) []Component {
	out := slices.Clone(components)

	slices.SortFunc(out, func(a, b Component) int {
		return cmp.Or(
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Name, b.Name),
			cmp.Compare(a.Path, b.Path),
		)
	})

	return out
}

// sortMCP orders MCP servers by name.
func sortMCP(servers []MCPServer) []MCPServer {
	out := slices.Clone(servers)

	slices.SortFunc(out, func(a, b MCPServer) int { return cmp.Compare(a.Name, b.Name) })

	return out
}

// sortHooks orders hooks by (Event, Matcher, Command, Timeout).
func sortHooks(hooks []Hook) []Hook {
	out := slices.Clone(hooks)

	slices.SortFunc(out, func(a, b Hook) int {
		return cmp.Or(
			cmp.Compare(a.Event, b.Event),
			cmp.Compare(a.Matcher, b.Matcher),
			cmp.Compare(a.Command, b.Command),
			cmp.Compare(a.Timeout, b.Timeout),
		)
	})

	return out
}

// isDir reports whether path is an existing directory.
func isDir(path string) bool {
	info, err := os.Stat(path)

	return err == nil && info.IsDir()
}

// regularFile reports whether path is an existing regular file; a symlink to
// one counts for detection, the readers still refuse to follow it.
func regularFile(path string) bool {
	info, err := os.Lstat(path)

	return err == nil && info.Mode().IsRegular()
}
