// Package pack renders a package payload as one chimera directory valid for
// every Ф1 host installer: the Agent Plugins, Claude, Codex and Gemini
// manifests coexist in one directory, skills and commands are copied or
// rendered per dialect. Every manifest names the bare plugin; the author is
// the marketplace, so hosts address the package as "name@owner" (decision
// F3). Render and RenderMarketplace are pure; Write is the only disk writer.
package pack

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/render"
	"github.com/odiumuniverse/verger/pkg/store"
)

// Chimera layout names.
const (
	agentPluginsSchemaURL    = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	agentPluginsMCPSchemaURL = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"

	manifestFile    = "plugin.json"
	claudeMetaDir   = ".claude-plugin"
	codexMetaDir    = ".codex-plugin"
	geminiMetaFile  = "gemini-extension.json"
	marketplaceFile = ".claude-plugin/marketplace.json"
	claudeMCPFile   = ".mcp.json"
	portableMCPFile = "mcp.json"
	hookDocument    = "hooks/hooks.json"

	skillsDir   = "skills"
	agentsDir   = "agents"
	commandsDir = "commands"
	skillFile   = "SKILL.md"

	markdownExt = ".md"
	tomlExt     = ".toml"

	transportStdio = "stdio"
	transportSSE   = "sse"
	transportHTTP  = "streamable-http"

	kindCommand = "command"
	keyName     = "name"
)

// Input is one package payload to render as a chimera. The identity comes
// from ID (IdentityOf) when it is set; Name and Owner are read only without an
// ID and must then be plugin-id parts.
type Input struct {
	ID          string // package id, owner/name[//subpath]
	Name        string // plugin name without owner (no ID only)
	Owner       string // author (no ID only); hosts address "<Name>@<Owner>"
	Version     string
	Description string
	License     string
	Keywords    []string
	Format      manifest.Format // identity-defining source format; informational, the chimera renders every format
	Root        string          // payload root on disk
	Components  []manifest.Component
	MCP         []manifest.MCPServer
	Hooks       []manifest.Hook
}

// Artifact is one rendered chimera: the slash-relative files, the digests of
// the logical components they carry, the formats rendered valid and the
// warnings for everything skipped.
//
// Digests maps "kind/name" to the component digest: a skill carries its source
// tree digest (the verbatim copy's digest.Tree), an agent, command or rule the
// digest of the rendered bytes it produced; MCP servers and hooks have no
// digest entry.
type Artifact struct {
	Files    map[string][]byte      // slash-relative
	Digests  map[string]digest.Hash // per logical component (kind/name)
	Formats  []manifest.Format      // formats rendered valid
	Warnings []string

	// symlinks maps a rendered path to its link target: a skill tree is copied
	// verbatim, and a symlink cannot live in a byte map.
	symlinks map[string]string
}

// RenderError reports a source payload that is unreadable or escapes the
// package root.
type RenderError struct {
	Component string
	Cause     error
}

// Error implements error.
func (e *RenderError) Error() string {
	if e.Component != "" {
		return fmt.Sprintf("render %s: %v", e.Component, e.Cause)
	}

	return fmt.Sprintf("render: %v", e.Cause)
}

// Unwrap returns the underlying cause.
func (e *RenderError) Unwrap() error {
	return e.Cause
}

// renderer carries one Render call.
type renderer struct {
	in    Input
	name  string // plugin name, an id part
	owner string // owner marketplace name, an id part
	art   Artifact
	// root caches the resolved payload root (EvalSymlinks) for the component
	// containment checks.
	root string
	// folded maps a case-folded output path to the path that claimed it: a
	// case-insensitive store filesystem would collapse case variants into one
	// file, silently dropping a component.
	folded map[string]string
}

// Render builds the chimera artifact. It is pure: the payload root is only
// read, and two renders of one input are byte-identical.
func Render(in Input) (Artifact, error) {
	identity, err := resolveIdentity(in)
	if err != nil {
		return Artifact{}, err
	}

	r := &renderer{
		in:    in,
		name:  identity.Name,
		owner: identity.Owner,
		art: Artifact{
			Files:    map[string][]byte{},
			Digests:  map[string]digest.Hash{},
			Formats:  chimeraFormats(),
			symlinks: map[string]string{},
		},
		folded: map[string]string{},
	}

	mcp := r.renderMCP()

	if err := r.manifests(mcp); err != nil {
		return Artifact{}, err
	}

	if err := r.components(); err != nil {
		return Artifact{}, err
	}

	if err := r.hooksDocument(); err != nil {
		return Artifact{}, err
	}

	slices.Sort(r.art.Warnings)

	return r.art, nil
}

// maxIDPart is the longest plugin-id part the hosts accept.
const maxIDPart = 128

// Identity is the host-visible synth identity of a package (decision F3): the
// plugin name and the owner marketplace name, both valid plugin-id parts —
// letters, digits, `.`, `_`, `-`, starting with a letter or digit, at most
// 128 bytes — so `<Name>@<Owner>` is a reference every host parses. `@`
// never appears in either part.
type Identity struct {
	Owner string
	Name  string
}

// IdentityOf projects a package id onto its synth identity: the owner is the
// first id segment and the name is the rest of the id with every `/` turned
// into `-` (the `//` subpath separator becomes `--`); any other byte outside
// the id-part alphabet becomes `-` and a leading non-alphanumeric run is
// dropped. A plain owner/name id keeps both segments verbatim. The projection
// is not injective (`acme/b-c` and `acme/b/c` meet); the store dir is
// (store.SynthElements), and the owner marketplace refuses a second package
// claiming one name instead of overwriting it.
func IdentityOf(id string) (Identity, error) {
	if _, _, err := store.SynthElements(id); err != nil {
		return Identity{}, err
	}

	owner, rest, _ := strings.Cut(id, "/")

	identity := Identity{Owner: projectIDPart(owner), Name: projectIDPart(rest)}

	if err := identity.validate(); err != nil {
		return Identity{}, &RenderError{Component: id, Cause: err}
	}

	return identity, nil
}

// projectIDPart maps every byte outside the id-part alphabet (including `/`)
// to `-` and drops a leading non-alphanumeric run.
func projectIDPart(value string) string {
	out := []byte(value)

	for i, c := range out {
		if !idPartByte(c) {
			out[i] = '-'
		}
	}

	return strings.TrimLeftFunc(string(out), func(r rune) bool { return !isAlphanumeric(r) })
}

// validate reports why the identity is not a pair of plugin-id parts.
func (i Identity) validate() error {
	for _, part := range []struct{ label, value string }{{"owner", i.Owner}, {"name", i.Name}} {
		if err := validIDPart(part.value); err != nil {
			return fmt.Errorf("the %s %w", part.label, err)
		}
	}

	return nil
}

// validIDPart reports why value is not a plugin-id part.
func validIDPart(value string) error {
	switch {
	case value == "":
		return errors.New("is empty")
	case len(value) > maxIDPart:
		return fmt.Errorf("%q is longer than %d bytes", value, maxIDPart)
	case !isAlphanumeric(rune(value[0])):
		return fmt.Errorf("%q must start with a letter or digit", value)
	}

	for i := range len(value) {
		if !idPartByte(value[i]) {
			return fmt.Errorf("%q may use only letters, digits, '.', '_' and '-'", value)
		}
	}

	return nil
}

// idPartByte reports whether c belongs to the plugin-id part alphabet.
func idPartByte(c byte) bool {
	return isAlphanumeric(rune(c)) || c == '.' || c == '_' || c == '-'
}

// isAlphanumeric reports whether r is an ASCII letter or digit.
func isAlphanumeric(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// chimeraFormats is the fixed format set every render declares.
func chimeraFormats() []manifest.Format {
	return []manifest.Format{
		manifest.FormatClaude,
		manifest.FormatCodex,
		manifest.FormatGemini,
		manifest.FormatAgentPlugins,
	}
}

// resolveIdentity returns the synth identity of one input: the id is
// authoritative when set (the CLI's own Name/Owner split of a subpath id is
// not an identity); without an id, Name and Owner must already be plugin-id
// parts.
func resolveIdentity(in Input) (Identity, error) {
	if in.ID != "" {
		identity, err := IdentityOf(in.ID)
		if err != nil {
			if _, ok := errors.AsType[*RenderError](err); ok {
				return Identity{}, err
			}

			return Identity{}, &RenderError{Component: in.ID, Cause: err}
		}

		return identity, nil
	}

	identity := Identity{Owner: in.Owner, Name: in.Name}

	if err := identity.validate(); err != nil {
		return Identity{}, &RenderError{Component: in.Name, Cause: err}
	}

	return identity, nil
}

// addFile inserts one rendered file, refusing duplicate output paths: an exact
// duplicate keeps the existing "duplicate output path" error, a path that
// differs from an existing one by case alone is refused by the folded-path
// guard.
func (r *renderer) addFile(rel string, data []byte) error {
	if _, ok := r.art.Files[rel]; ok {
		return &RenderError{Component: rel, Cause: errors.New("duplicate output path")}
	}

	if err := r.foldPath(rel); err != nil {
		return err
	}

	files := r.art.Files
	files[rel] = data

	return nil
}

// foldPath records one output path in the case-folded index. Exact duplicates
// keep their kind-specific semantics (files: "duplicate output path"; symlinks:
// last write wins); this guard refuses only a path that differs from an
// already-claimed one by case alone, because a case-insensitive store
// filesystem would overwrite the first with the second. Only case folding is
// covered — Unicode NFC/NFD equivalence would need x/text/unicode/norm, which
// is not an allowed dependency in Ф1.
func (r *renderer) foldPath(rel string) error {
	key := strings.ToLower(rel)

	if prev, ok := r.folded[key]; ok && prev != rel {
		return &RenderError{
			Component: rel,
			Cause:     fmt.Errorf("output path %q differs only by case from %q", rel, prev),
		}
	}

	r.folded[key] = rel

	return nil
}

// warnf records one render warning.
func (r *renderer) warnf(format string, args ...any) {
	r.art.Warnings = append(r.art.Warnings, fmt.Sprintf(format, args...))
}

// encodeJSON marshals a document deterministically: sorted map keys, two-space
// indentation and a trailing newline.
func encodeJSON(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}

	return append(data, '\n'), nil
}

// putNonEmpty sets a string key unless the value is empty.
func putNonEmpty(doc map[string]any, key, value string) {
	if value != "" {
		doc[key] = value
	}
}

// putKeywords sets the keywords key unless the list is empty.
func putKeywords(doc map[string]any, keywords []string) {
	if len(keywords) > 0 {
		doc["keywords"] = slices.Clone(keywords)
	}
}

// mcpRender carries the per-dialect MCP maps of one render.
type mcpRender struct {
	claude   map[string]any
	gemini   map[string]any
	portable map[string]any
	warnings []string
}

// renderMCP encodes every server in the Claude, Gemini and Agent Plugins
// dialects; a server a dialect cannot express is skipped with a warning.
func (r *renderer) renderMCP() mcpRender {
	out := mcpRender{
		claude:   map[string]any{},
		gemini:   map[string]any{},
		portable: map[string]any{},
	}

	seen := map[string]bool{}

	for _, server := range sortServers(r.in.MCP) {
		if !validServerName(server.Name) {
			out.warnings = append(out.warnings,
				fmt.Sprintf("claude: mcp %q: the name must match [A-Za-z0-9._-]+; skipped", server.Name))

			continue
		}

		if seen[server.Name] {
			out.warnings = append(out.warnings, fmt.Sprintf("mcp %q: duplicate server name; skipped", server.Name))

			continue
		}

		seen[server.Name] = true

		out.claude, out.warnings = encodeDialect(out.claude, out.warnings, manifest.FormatClaude, server)
		out.gemini, out.warnings = encodeDialect(out.gemini, out.warnings, manifest.FormatGemini, server)

		entry, warns := agentPluginsMCP(server)

		switch {
		case len(warns) > 0:
			for _, reason := range warns {
				out.warnings = append(out.warnings, fmt.Sprintf("agent-plugins: mcp %q: %s; skipped", server.Name, reason))
			}
		default:
			out.portable[server.Name] = entry
		}
	}

	return out
}

// encodeDialect adds one server to a dialect map or reports why it was skipped.
func encodeDialect(
	servers map[string]any,
	warnings []string,
	format manifest.Format,
	server manifest.MCPServer,
) (map[string]any, []string) {
	value, err := render.EncodeMCP(format, server)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("%s: %v; skipped", format, err))

		return servers, warnings
	}

	servers[server.Name] = value

	return servers, warnings
}

// sortServers orders servers by name.
func sortServers(servers []manifest.MCPServer) []manifest.MCPServer {
	out := slices.Clone(servers)

	slices.SortFunc(out, func(a, b manifest.MCPServer) int { return cmp.Compare(a.Name, b.Name) })

	return out
}

// validServerName reports whether a server name matches [A-Za-z0-9._-]+.
func validServerName(name string) bool {
	if name == "" {
		return false
	}

	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}

	return true
}

// manifests renders the four format manifests and the Claude marketplace.
func (r *renderer) manifests(mcp mcpRender) error {
	author := map[string]any{keyName: r.owner}

	agentPlugins := map[string]any{
		"$schema": agentPluginsSchemaURL,
		keyName:   r.name,
		"author":  author,
	}
	putNonEmpty(agentPlugins, "version", r.in.Version)
	putNonEmpty(agentPlugins, "description", r.in.Description)
	putNonEmpty(agentPlugins, "license", r.in.License)
	putKeywords(agentPlugins, r.in.Keywords)

	claude := map[string]any{keyName: r.name, "author": author}
	putNonEmpty(claude, "version", r.in.Version)
	putNonEmpty(claude, "description", r.in.Description)
	putNonEmpty(claude, "license", r.in.License)
	putKeywords(claude, r.in.Keywords)

	if len(mcp.claude) > 0 {
		claude["mcpServers"] = mcp.claude
	}

	codex := map[string]any{keyName: r.name}
	putNonEmpty(codex, "version", r.in.Version)
	putNonEmpty(codex, "description", r.in.Description)

	gemini := map[string]any{keyName: r.name}
	putNonEmpty(gemini, "version", r.in.Version)
	putNonEmpty(gemini, "description", r.in.Description)

	if len(mcp.gemini) > 0 {
		gemini["mcpServers"] = mcp.gemini
	}

	// The author's own marketplace (`verger pack`): the package root is the
	// marketplace, named after the owner, so hosts address it as name@owner.
	marketplace := map[string]any{
		keyName: r.owner,
		"owner": author,
		"plugins": []any{
			map[string]any{keyName: r.name, "source": "./"},
		},
	}
	putNonEmpty(marketplace, "description", r.in.Description)

	r.art.Warnings = append(r.art.Warnings, mcp.warnings...)

	documents := []struct {
		path  string
		value any
	}{
		{manifestFile, agentPlugins},
		{path.Join(claudeMetaDir, manifestFile), claude},
		{path.Join(codexMetaDir, manifestFile), codex},
		{geminiMetaFile, gemini},
		{marketplaceFile, marketplace},
	}

	for _, document := range documents {
		data, err := encodeJSON(document.value)
		if err != nil {
			return &RenderError{Component: document.path, Cause: err}
		}

		if err := r.addFile(document.path, data); err != nil {
			return err
		}
	}

	if len(mcp.claude) > 0 {
		data, err := encodeJSON(map[string]any{"mcpServers": mcp.claude})
		if err != nil {
			return &RenderError{Component: claudeMCPFile, Cause: err}
		}

		if err := r.addFile(claudeMCPFile, data); err != nil {
			return err
		}
	}

	if len(mcp.portable) > 0 {
		data, err := encodeJSON(map[string]any{
			"$schema":    agentPluginsMCPSchemaURL,
			"mcpServers": mcp.portable,
		})
		if err != nil {
			return &RenderError{Component: portableMCPFile, Cause: err}
		}

		if err := r.addFile(portableMCPFile, data); err != nil {
			return err
		}
	}

	return nil
}

// components renders every payload component in canonical order.
func (r *renderer) components() error {
	components := slices.Clone(r.in.Components)

	slices.SortFunc(components, func(a, b manifest.Component) int {
		return cmp.Or(
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Name, b.Name),
			cmp.Compare(a.Path, b.Path),
		)
	})

	seen := map[string]bool{}

	for _, component := range components {
		key := string(component.Kind) + "/" + component.Name
		if seen[key] {
			return &RenderError{Component: key, Cause: errors.New("duplicate component")}
		}

		seen[key] = true

		if err := r.component(component); err != nil {
			return err
		}
	}

	return nil
}

// component dispatches one payload component by kind.
func (r *renderer) component(component manifest.Component) error {
	switch component.Kind {
	case manifest.KindSkill:
		return r.skill(component)
	case manifest.KindAgent:
		return r.agent(component)
	case manifest.KindCommand:
		return r.command(component)
	case manifest.KindRule:
		return r.rule(component)
	default:
		r.warnf("%s: component %q is not expressible in the chimera; skipped", component.Kind, component.Name)

		return nil
	}
}

// skill copies one skill tree verbatim: junk is not filtered at this layer and
// symlinks are recorded for Write instead of followed.
func (r *renderer) skill(component manifest.Component) error {
	if !validComponentName(component.Name) {
		return &RenderError{Component: component.Name, Cause: errors.New("the component name is not a safe path element")}
	}

	source, err := r.sourceDir(component)
	if err != nil {
		return err
	}

	walkErr := filepath.WalkDir(source, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(source, current)
		if relErr != nil {
			return relErr
		}

		if rel == "." {
			return nil
		}

		out := path.Join(skillsDir, component.Name, filepath.ToSlash(rel))

		return r.skillEntry(out, current, entry)
	})
	if walkErr != nil {
		return &RenderError{Component: component.Name, Cause: walkErr}
	}

	if component.Digest.Valid() {
		r.art.Digests["skill/"+component.Name] = component.Digest
	} else if sum, err := digest.Tree(source); err == nil {
		r.art.Digests["skill/"+component.Name] = sum
	}

	return nil
}

// skillEntry copies one walk entry: a symlink is recorded, a regular file is
// read, anything else is skipped with a warning.
func (r *renderer) skillEntry(out, current string, entry fs.DirEntry) error {
	switch {
	case entry.IsDir():
		return nil
	case entry.Type()&fs.ModeSymlink != 0:
		target, err := os.Readlink(current)
		if err != nil {
			return err
		}

		if err := r.foldPath(out); err != nil {
			return err
		}

		r.art.symlinks[out] = target

		return nil
	case entry.Type().IsRegular():
		data, err := os.ReadFile(current) //nolint:gosec // G304: the path is below the caller-provided package root
		if err != nil {
			return err
		}

		return r.addFile(out, data)
	default:
		r.warnf("skill: %q is not a regular file; skipped", out)

		return nil
	}
}

// agent renders one agent markdown into the Claude-side agent file.
func (r *renderer) agent(component manifest.Component) error {
	if !validComponentName(component.Name) {
		return &RenderError{Component: component.Name, Cause: errors.New("the component name is not a safe path element")}
	}

	data, err := r.sourceFile(component)
	if err != nil {
		return err
	}

	if !strings.HasSuffix(strings.ToLower(component.Path), markdownExt) {
		r.warnf("codex: agent %q: source %q is not markdown; skipped", component.Name, component.Path)

		return nil
	}

	agent, err := render.ParseAgentMarkdown(data)
	if err != nil {
		return &RenderError{Component: component.Name, Cause: err}
	}

	rendered := agent.ClaudeMarkdown()

	if err := r.addFile(path.Join(agentsDir, component.Name+markdownExt), rendered); err != nil {
		return err
	}

	r.art.Digests["agent/"+component.Name] = digest.Bytes(rendered)

	return nil
}

// command renders one command into the Claude markdown and the Gemini TOML;
// a command Gemini cannot express is skipped with a warning.
func (r *renderer) command(component manifest.Component) error {
	if !validComponentName(component.Name) {
		return &RenderError{Component: component.Name, Cause: errors.New("the component name is not a safe path element")}
	}

	cmd, ok, err := r.commandDocument(component)
	if err != nil {
		return err
	}

	if !ok {
		return nil
	}

	rendered := cmd.Markdown()

	if err := r.addFile(path.Join(commandsDir, component.Name+markdownExt), rendered); err != nil {
		return err
	}

	r.art.Digests["command/"+component.Name] = digest.Bytes(rendered)

	toml, err := cmd.GeminiTOML()
	if err != nil {
		r.warnf("gemini: %v; skipped", err)

		return nil
	}

	return r.addFile(path.Join(commandsDir, component.Name+tomlExt), toml)
}

// commandDocument lifts a command source into the canonical form: markdown is
// parsed, a Gemini TOML document is lifted first. ok is false when the source
// is skipped with a warning.
func (r *renderer) commandDocument(component manifest.Component) (render.Command, bool, error) {
	data, err := r.sourceFile(component)
	if err != nil {
		return render.Command{}, false, err
	}

	switch strings.ToLower(path.Ext(component.Path)) {
	case markdownExt:
		cmd, parseErr := render.ParseCommandMarkdown(data)
		if parseErr != nil {
			return render.Command{}, false, &RenderError{Component: component.Name, Cause: parseErr}
		}

		cmd.Name = component.Name

		return cmd, true, nil
	case tomlExt:
		lifted, ok, liftErr := render.LiftGeminiCommand(component.Name, data)
		if liftErr != nil {
			return render.Command{}, false, &RenderError{Component: component.Name, Cause: liftErr}
		}

		if !ok {
			r.warnf("gemini: command %q: the source carries no prompt; skipped", component.Name)

			return render.Command{}, false, nil
		}

		cmd, parseErr := render.ParseCommandMarkdown(lifted)
		if parseErr != nil {
			return render.Command{}, false, &RenderError{Component: component.Name, Cause: parseErr}
		}

		cmd.Name = component.Name

		return cmd, true, nil
	default:
		r.warnf("%s: command %q is not expressible in the chimera; skipped", component.Kind, component.Name)

		return render.Command{}, false, nil
	}
}

// rule wraps one rule document as a skill (D23).
func (r *renderer) rule(component manifest.Component) error {
	if !validComponentName(component.Name) {
		return &RenderError{Component: component.Name, Cause: errors.New("the component name is not a safe path element")}
	}

	data, err := r.sourceFile(component)
	if err != nil {
		return err
	}

	rel, content, err := render.RuleSkill(component.Name, data)
	if err != nil {
		return &RenderError{Component: component.Name, Cause: err}
	}

	if err := r.addFile(path.Join(rel, skillFile), content); err != nil {
		return err
	}

	r.art.Digests["rule/"+component.Name] = digest.Bytes(content)

	return nil
}

// hooksDocument merges the Claude and Gemini hook renders into one document:
// both dialects read nested matcher groups, and each host ignores the other's
// event names.
func (r *renderer) hooksDocument() error {
	if len(r.in.Hooks) == 0 {
		return nil
	}

	events := map[string]any{}

	for _, format := range []manifest.Format{manifest.FormatClaude, manifest.FormatGemini} {
		plan, err := render.PlanHooks(format, nil, r.in.Hooks)
		if err != nil {
			return &RenderError{Component: hookDocument, Cause: err}
		}

		r.art.Warnings = append(r.art.Warnings, plan.Warnings...)

		if plan.File == nil {
			continue
		}

		var doc map[string]any

		if err := json.Unmarshal(plan.File, &doc); err != nil {
			return &RenderError{Component: hookDocument, Cause: err}
		}

		for event, value := range doc {
			if err := mergeHookEvent(events, event, value); err != nil {
				return &RenderError{Component: hookDocument, Cause: err}
			}
		}
	}

	if len(events) == 0 {
		return nil
	}

	data, err := encodeJSON(map[string]any{"hooks": events})
	if err != nil {
		return &RenderError{Component: hookDocument, Cause: err}
	}

	return r.addFile(hookDocument, data)
}

// mergeHookEvent unions one event of the two dialect renders.
func mergeHookEvent(events map[string]any, event string, value any) error {
	existing, ok := events[event]
	if !ok {
		events[event] = value

		return nil
	}

	left, leftErr := json.Marshal(existing)
	right, rightErr := json.Marshal(value)

	if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
		return fmt.Errorf("hook event %s is rendered differently by the dialects", event)
	}

	return nil
}

// sourceFile reads one regular component file below the payload root.
func (r *renderer) sourceFile(component manifest.Component) ([]byte, error) {
	source, err := r.sourcePath(component)
	if err != nil {
		return nil, err
	}

	info, err := os.Lstat(source)
	if err != nil {
		return nil, &RenderError{Component: component.Name, Cause: err}
	}

	if !info.Mode().IsRegular() {
		return nil, &RenderError{Component: component.Name, Cause: errors.New("source is not a regular file")}
	}

	data, err := os.ReadFile(source) //nolint:gosec // G304: the path is resolved below the payload root
	if err != nil {
		return nil, &RenderError{Component: component.Name, Cause: err}
	}

	return data, nil
}

// sourceDir resolves a skill component to an existing directory, never a
// symlink.
func (r *renderer) sourceDir(component manifest.Component) (string, error) {
	source, err := r.sourcePath(component)
	if err != nil {
		return "", err
	}

	info, err := os.Lstat(source)
	if err != nil {
		return "", &RenderError{Component: component.Name, Cause: err}
	}

	if !info.IsDir() {
		return "", &RenderError{Component: component.Name, Cause: errors.New("source is not a directory")}
	}

	return source, nil
}

// sourcePath joins a component path to the payload root and confirms the
// resolved target stays inside the resolved root: the lexical guard alone is
// not enough because the OS follows symlinked ancestors. The lexical path is
// returned, so a component whose own final element is a symlink is still
// refused by the Lstat checks in sourceFile and sourceDir.
func (r *renderer) sourcePath(component manifest.Component) (string, error) {
	if component.Path == "" || strings.ContainsRune(component.Path, 0) || !filepath.IsLocal(component.Path) {
		return "", &RenderError{Component: component.Name, Cause: errors.New("source path escapes the package root")}
	}

	joined := filepath.Join(r.in.Root, filepath.FromSlash(component.Path))

	root, err := r.payloadRoot()
	if err != nil {
		return "", &RenderError{Component: component.Name, Cause: err}
	}

	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", &RenderError{Component: component.Name, Cause: err}
	}

	rel, err := filepath.Rel(root, resolved)
	if err != nil || !filepath.IsLocal(rel) {
		return "", &RenderError{Component: component.Name, Cause: errors.New("source path resolves outside the package root")}
	}

	return joined, nil
}

// payloadRoot resolves the payload root once per render; component sources
// must exist, so the root must resolve too.
func (r *renderer) payloadRoot() (string, error) {
	if r.root != "" {
		return r.root, nil
	}

	root, err := filepath.EvalSymlinks(r.in.Root)
	if err != nil {
		return "", err
	}

	r.root = root

	return r.root, nil
}

// validComponentName reports whether a name is safe as one output path
// element; unicode is allowed, separators and control characters are not.
func validComponentName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}

	if strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return false
	}

	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}

	return true
}

// agentPluginsMCP renders one server in the closed Agent Plugins v1.0.0 union;
// a non-empty reason list means the server is outside the union and skipped.
// `{secret:NAME}` references stay verbatim.
func agentPluginsMCP(server manifest.MCPServer) (map[string]any, []string) {
	if reason := serverShapeSkip(server); reason != "" {
		return nil, []string{reason}
	}

	if slices.ContainsFunc(append(slices.Clone(server.Command), envValues(server.Env)...), escapesRoot) {
		return nil, []string{"a command or env value climbs above the package root"}
	}

	if server.URL != "" {
		if !validRemoteURL(server.URL) {
			return nil, []string{"the url must be https without user-info or a fragment (http is allowed on loopback only)"}
		}

		entry := map[string]any{"type": transportHTTP, "url": server.URL}
		if server.Transport == transportSSE {
			entry["type"] = transportSSE
		}

		if len(server.Headers) > 0 {
			entry["headers"] = maps.Clone(server.Headers)
		}

		return entry, nil
	}

	if !portableCommand(server.Command[0]) {
		return nil, []string{fmt.Sprintf("the command %q is neither a bare name nor a ./relative path", server.Command[0])}
	}

	entry := map[string]any{"type": transportStdio, kindCommand: server.Command[0]}

	if len(server.Command) > 1 {
		entry["args"] = slices.Clone(server.Command[1:])
	}

	if len(server.Env) > 0 {
		entry["env"] = maps.Clone(server.Env)
	}

	return entry, nil
}

// serverShapeSkip reports whether the transport, url and command combination
// is outside the closed Agent Plugins union.
func serverShapeSkip(server manifest.MCPServer) string {
	hasURL := server.URL != ""
	hasCommand := len(server.Command) > 0

	switch server.Transport {
	case "", transportStdio, transportHTTP, transportSSE:
	default:
		return fmt.Sprintf("transport %q is not part of the Agent Plugins union", server.Transport)
	}

	switch {
	case hasURL && hasCommand:
		return "the server carries both a command and a url"
	case hasURL && server.Transport == transportStdio:
		return "the stdio transport carries a url"
	case !hasURL && !hasCommand:
		return "the server has no command"
	case !hasURL && (server.Transport == transportHTTP || server.Transport == transportSSE):
		return fmt.Sprintf("the %s transport carries no url", server.Transport)
	default:
		return ""
	}
}

// envValues lists the values of an env map.
func envValues(env map[string]string) []string {
	values := make([]string, 0, len(env))

	for _, name := range slices.Sorted(maps.Keys(env)) {
		values = append(values, env[name])
	}

	return values
}

// portableCommand reports whether a command survives the Agent Plugins spec:
// a bare executable name or a ./relative path, without whitespace or control
// characters; command is never interpolated.
func portableCommand(command string) bool {
	switch {
	case command == "", strings.ContainsAny(command, "${}"), strings.HasPrefix(command, "/"):
		return false
	}

	for _, r := range command {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}

	if strings.Contains(command, "/") {
		return strings.HasPrefix(command, "./")
	}

	return true
}

// escapesRoot reports whether a value carries a path that climbs above the
// package root.
func escapesRoot(value string) bool {
	for fragment := range strings.SplitSeq(value, "=") {
		if climbs(fragment) {
			return true
		}
	}

	return false
}

// climbs reports whether a slash-separated fragment walks above its start.
func climbs(fragment string) bool {
	depth := 0

	for part := range strings.SplitSeq(strings.ReplaceAll(fragment, `\`, "/"), "/") {
		switch part {
		case "", ".":
		case "..":
			depth--

			if depth < 0 {
				return true
			}
		default:
			depth++
		}
	}

	return false
}

// validRemoteURL reports whether a remote url satisfies the Agent Plugins
// union: an absolute https url without user-info or a fragment, or http on a
// loopback host.
func validRemoteURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}

	switch parsed.Scheme {
	case "https":
		return true
	case "http":
		host := parsed.Hostname()
		if host == "localhost" {
			return true
		}

		ip := net.ParseIP(host)

		return ip != nil && ip.IsLoopback()
	default:
		return false
	}
}
