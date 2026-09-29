package host

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/tailscale/hujson"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/hostpath"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
	"github.com/odiumuniverse/verger/pkg/secret"
	"github.com/odiumuniverse/verger/pkg/store"
)

// Payload layout names shared with the renderers.
const (
	skillFile   = "SKILL.md"
	markdownExt = ".md"
	tomlExt     = ".toml"
	artifactMCP = "mcp" // receipt artifact kind of an MCP server
	// artifactPatchDocument is the receipt artifact kind of a config document
	// whose records the adapter owns by id (the DSH home patch layer); it
	// matches pkg/apply's kindPatchDocument.
	artifactPatchDocument = "patch-document"
)

// Plugin variable names of the host dialects.
const (
	varPluginRoot    = "PLUGIN_ROOT"
	varPluginData    = "PLUGIN_DATA"
	varClaudeRoot    = "CLAUDE_PLUGIN_ROOT"
	varClaudeData    = "CLAUDE_PLUGIN_DATA"
	varClaudeProject = "CLAUDE_PROJECT_DIR"
)

// unbracedPluginVar matches every known plugin variable without braces: the
// receiving host would expand a bare $NAME to an empty string.
var unbracedPluginVar = regexp.MustCompile(`\$(PLUGIN_ROOT|PLUGIN_DATA|CLAUDE_[A-Z0-9_]*|CURSOR_PLUGIN_ROOT|extensionPath|workspacePath)`)

// secretRefPattern matches `{secret:NAME}` references, embedded or whole-value.
var secretRefPattern = regexp.MustCompile(`\{secret:[A-Za-z_][A-Za-z0-9_]*\}`)

// rewriteVariables rewrites the known plugin root/data variables of one value
// to absolute paths and refuses every other braced variable: an unresolvable
// host variable must never reach a host as an empty string. extra carries the
// host-specific variables of one dialect (Gemini `${extensionPath}`).
func rewriteVariables(value, root, dataDir string, extra map[string]string) (string, error) {
	var out strings.Builder

	rest := value

	for {
		start := strings.Index(rest, "${")
		if start < 0 {
			out.WriteString(rest)

			break
		}

		end := strings.Index(rest[start:], "}")
		if end < 0 {
			return "", &UnsupportedVariableError{Variable: rest[start:]}
		}

		name := rest[start+2 : start+end]

		replacement, ok := knownVariable(name, root, dataDir)
		if !ok {
			replacement, ok = extra[name]
		}

		if !ok || replacement == "" {
			return "", &UnsupportedVariableError{Variable: name}
		}

		out.WriteString(rest[:start])
		out.WriteString(replacement)

		rest = rest[start+end+1:]
	}

	expanded := out.String()

	if match := unbracedPluginVar.FindString(expanded); match != "" {
		return "", &UnsupportedVariableError{Variable: match}
	}

	return expanded, nil
}

// knownVariable resolves one portable variable name; unknown and unresolvable
// names are refused, including CLAUDE_PROJECT_DIR and workspacePath.
func knownVariable(name, root, dataDir string) (string, bool) {
	switch name {
	case varPluginRoot, varClaudeRoot:
		return root, root != ""
	case varPluginData, varClaudeData:
		return dataDir, dataDir != ""
	default:
		return "", false
	}
}

// looseSpec describes the host-native loose surface of one adapter.
type looseSpec struct {
	host          ID
	binary        string
	home          string // user home for host policy documents ("" skips the hooks policy check)
	skillsDir     string
	agentsDir     string
	commandsDir   string
	settingsPath  string
	mcpAddArgs    func(manifest.MCPServer) ([]string, error)
	mcpRemoveArgs func(string) []string
	mcpGetArgs    func(string) []string // read-only probe of one server name (ownership gate)

	// Optional dialect overrides; the zero values keep the Claude surface.
	agentExt       string                                     // rendered agent file extension, default ".md"
	renderAgent    func(render.Agent, string) ([]byte, error) // default Agent.ClaudeMarkdown
	commandExt     string                                     // rendered command file extension, default ".md"
	renderCommand  func(render.Command) ([]byte, error)       // default Command.Markdown
	rulesDir       string                                     // host-native rulebook dir; empty keeps the D23 skill wrapper
	renderRule     func(string, []byte) ([]byte, error)       // default render.RuleMarkdown
	hooksPath      string                                     // hooks document, default settingsPath
	hooksFormat    manifest.Format                            // hook dialect, default FormatClaude
	hooksTrustNote bool                                       // Codex: hooks need a /hooks review
	// hookRecordPath names the receipt identity of one hook record, so a
	// record verger wrote keeps its own digest and a hand edit inside it is
	// hands-off instead of an overwrite. nil leaves whole-document ownership.
	hookRecordPath func(event, command string) string
	hooksBlocked   string            // non-empty: the host has no declarative hook surface, hooks are skipped with this reason
	hookModulesDir string            // host directory of pre/post hook modules (<dir>/pre/<name>.ts); the payload's runtime hook modules are copied verbatim
	mcpConfig      *mcpConfigSpec    // config-document MCP surface (Codex, Gemini)
	variables      map[string]string // host-specific braced variables (Gemini extensionPath)

	// project is the trusted project root for a project-scope delivery, or
	// "" at user scope. When it is set, projectScope rewrites the surface
	// paths above from hostpath.ProjectSurfaces, so every host delivers into
	// its own project layout without each adapter repeating the mapping.
	project string
}

// mcpConfigSpec describes an MCP surface written into a shared config document
// instead of the host CLI.
type mcpConfigSpec struct {
	path   string
	format manifest.Format
	toml   bool // TOML (Codex config.toml) vs JSONC (Gemini settings.json)
	edit   func([]byte, []render.Edit, render.Owned) ([]byte, []render.Change, error)
	// prefix overrides the container key of the dialect; empty keeps the
	// format's own. OpenCode carries the same values under `mcp.servers.`
	// (v2) and `mcp.` (v1), and the adapter picks the container the config
	// already declares.
	prefix string
	// entries replaces render.MCPEdits when the host's entry shape differs
	// from the dialect format names (pi writes `transport` and no `type`;
	// Antigravity writes `serverUrl`; a patch layer writes whole records).
	entries func([]manifest.MCPServer) ([]render.Edit, error)
	// member reads one record of a document the JSONC/TOML decoder cannot
	// read — a YAML patch layer. Nil reads a key path of the document.
	member func([]byte, string) (any, bool, error)
	// wholeFile marks a document the key-path receipt machinery cannot undo:
	// the adapter edits it as a node tree and a dotted key path means nothing
	// in it, so pkg/apply rolls the document back as one file. The receipt
	// then owns the document — a removal restores the trashed bytes, foreign
	// entries and comments included — instead of one op per record.
	wholeFile bool
	// recordPath is the receipt identity of one record of a whole-file
	// document. The adapter asks the previous receipt for the digest this path
	// carries and hands it to the editor as render.Owned, so a record the user
	// edited by hand is hands-off instead of being overwritten. Nil keeps a
	// document without per-record ownership.
	recordPath func(name string) string
	// recordArtifacts returns one receipt artifact per owned record of a
	// whole-file document, carrying that record's own value digest.
	recordArtifacts func([]render.Edit) []receipt.Artifact
}

// readMember reads one record of the surface document by its key path.
func (s *mcpConfigSpec) readMember(file []byte, keyPath string) (any, bool, error) {
	if s.member != nil {
		return s.member(file, keyPath)
	}

	return configMember(file, keyPath, s.toml)
}

// configEditsUnder returns one key-path edit per server for the surface, under
// the container prefix the surface declares.
func (s *mcpConfigSpec) configEditsUnder(servers []manifest.MCPServer) ([]render.Edit, error) {
	if s.entries != nil {
		return s.entries(servers)
	}

	return render.MCPEditsUnder(s.format, mcpContainerPrefix(s), servers)
}

// agentExtension returns the agent file extension of the surface.
func (s looseSpec) agentExtension() string {
	if s.agentExt != "" {
		return s.agentExt
	}

	return markdownExt
}

// commandExtension returns the command file extension of the surface.
func (s looseSpec) commandExtension() string {
	if s.commandExt != "" {
		return s.commandExt
	}

	return markdownExt
}

// hooksFile returns the hooks document of the surface.
func (s looseSpec) hooksFile() string {
	if s.hooksPath != "" {
		return s.hooksPath
	}

	return s.settingsPath
}

// hooksDialect returns the hook dialect of the surface.
func (s looseSpec) hooksDialect() manifest.Format {
	if s.hooksFormat != "" {
		return s.hooksFormat
	}

	return manifest.FormatClaude
}

// renderedAgent renders one canonical agent in the surface dialect; fallback
// names an agent whose source carries no name.
func (s looseSpec) renderedAgent(agent render.Agent, fallback string) ([]byte, error) {
	if s.renderAgent != nil {
		return s.renderAgent(agent, fallback)
	}

	return agent.ClaudeMarkdown(), nil
}

// renderedCommand renders one canonical command in the surface dialect.
func (s looseSpec) renderedCommand(cmd render.Command) ([]byte, error) {
	if s.renderCommand != nil {
		return s.renderCommand(cmd)
	}

	return cmd.Markdown(), nil
}

// renderedRule renders one rule source as the surface's rulebook document.
func (s looseSpec) renderedRule(name string, rule []byte) ([]byte, error) {
	if s.renderRule != nil {
		return s.renderRule(name, rule)
	}

	return render.RuleMarkdown(name, rule)
}

// projectScope returns the spec aimed at the host's own project layout
// instead of the user one. The paths come from hostpath.ProjectSurfaces, the
// same table beadle resolves, so the two tools write the same project file
// for a host. Surfaces the host has no project form for are left as they
// were: a surface that does not exist at project scope is a gap to report,
// not a path to invent.
func (s looseSpec) projectScope() looseSpec {
	if s.project == "" {
		return s
	}

	surfaces := hostpath.ProjectSurfaces(string(s.host), s.project)

	if surfaces.Skills != "" {
		s.skillsDir = surfaces.Skills
	}

	if surfaces.Agents != "" {
		s.agentsDir = surfaces.Agents
	}

	if surfaces.Commands != "" {
		s.commandsDir = surfaces.Commands
	}

	// ProjectSurfaces is the authority on where a host keeps its hooks in a
	// project, including for a host that uses a document separate from its
	// settings (codex writes .codex/hooks.json beside config.toml). An earlier
	// version only overrode hooksPath when it happened to equal
	// settingsPath, which left codex's hooks document at user scope.
	if surfaces.Hooks != "" {
		s.settingsPath = surfaces.Hooks
		s.hooksPath = surfaces.Hooks
	}

	if surfaces.RulesPerFile != "" {
		s.rulesDir = surfaces.RulesPerFile
	}

	if s.mcpConfig != nil && surfaces.MCPDoc != "" {
		cfg := *s.mcpConfig
		cfg.path = surfaces.MCPDoc
		s.mcpConfig = &cfg
	}

	return s
}

// looseStepKind tags one planned side effect.
type looseStepKind int

const (
	stepCopyTree looseStepKind = iota
	stepWriteFile
	stepConfig
	stepHostCommand
)

// looseStep is one planned side effect of a loose delivery. Planning never
// writes: every read, rewrite and collision check happens first, so a failing
// cell leaves the home untouched.
type looseStep struct {
	kind    looseStepKind
	path    string // absolute target
	src     string // copy-tree source
	data    []byte
	mode    fs.FileMode
	existed bool
	command []string
	digest  digest.Hash
	opIndex int           // index into loosePlan.ops, for the Backup id
	backups []looseBackup // stepConfig: one replaced value per changed key
}

// looseBackup is one replaced value of a shared config document.
type looseBackup struct {
	previous any
	opIndex  int
}

// loosePlan is the result of planning one loose delivery.
type loosePlan struct {
	steps      []looseStep
	artifacts  []receipt.Artifact
	ops        []receipt.Op
	notes      []string
	ensureData bool
}

// result renders the plan as a Delivery Result.
func (p *loosePlan) result(strategy Strategy, dryRun bool) Result {
	notes := slices.Clone(p.notes)

	if dryRun {
		notes = append(notes, "dry-run")
	}

	slices.Sort(notes)

	return Result{
		Strategy:  strategy,
		Artifacts: slices.Clone(p.artifacts),
		RMA:       slices.Clone(p.ops),
		Notes:     notes,
	}
}

// add records one planned artifact, its RMA op and its step.
func (p *loosePlan) add(step looseStep, artifact receipt.Artifact, op receipt.Op) {
	step.opIndex = len(p.ops)

	p.steps = append(p.steps, step)
	p.artifacts = append(p.artifacts, artifact)
	p.ops = append(p.ops, op)
}

// record keeps an artifact and its RMA op with no side effect to run: the host
// already carries exactly what the delivery would write.
func (p *loosePlan) record(artifact receipt.Artifact, op receipt.Op) {
	p.artifacts = append(p.artifacts, artifact)
	p.ops = append(p.ops, op)
}

// addConfig records one shared config-document write backing several keys: a
// single step, one artifact for the document and one RMA op per key the delivery
// owns. replaced carries the previous value of the first len(replaced) ops (the
// keys this write changes); any op after that names a key the delivery owns but
// did not have to change, and records no backup, so an inverse leaves it in
// place instead of restoring a value.
//
// One artifact per document, not per key: an artifact is a path claim and a
// receipt refuses two claims on one path (pkg/receipt Validate, "duplicate
// artifact path"), while pkg/apply already resolves the key op of that path by
// the artifact's path (configOpFor) and verifies the document's first key
// against it. The per-key detail lives in the ops, keyed by path#keyPath.
func (p *loosePlan) addConfig(step looseStep, artifact receipt.Artifact, ops []receipt.Op, previous []any) {
	base := len(p.ops)

	p.artifacts = append(p.artifacts, artifact)

	for i, op := range ops {
		p.ops = append(p.ops, op)

		if op.Existed && i < len(previous) {
			step.backups = append(step.backups, looseBackup{previous: previous[i], opIndex: base + i})
		}
	}

	p.steps = append(p.steps, step)
}

// recordConfig keeps a document this package already owns exactly as the
// delivery would write it, with no side effect to run: the keys stay recorded so
// the receipt keeps owning them (an update whose receipt lost the ownership
// would reconcile the previous receipt's keys away, restoring a backup older
// than the first delivery), and every op is pre-existing, so an inverse leaves
// the values in place.
func (p *loosePlan) recordConfig(artifact receipt.Artifact, ops []receipt.Op) {
	p.artifacts = append(p.artifacts, artifact)
	p.ops = append(p.ops, ops...)
}

// loosePlanner plans one loose delivery; all state lives in plan.
type loosePlanner struct {
	base    *Base
	spec    looseSpec
	d       Delivery
	pkg     Package
	plan    *loosePlan
	dataDir string

	configs []*pendingConfig
	missing []string
}

// planLoose reads the payload, rewrites variables, resolves secrets and checks
// every collision; it never writes.
func planLoose(ctx context.Context, base *Base, spec looseSpec, d Delivery) (*loosePlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// A project-scope delivery lands in the host's own project layout. The
	// scope is resolved before the planner exists, because every path the
	// planner writes comes from the spec.
	if d.Package.Scope != "" && d.Package.Scope != receipt.ScopeUser {
		if d.Package.Scope != receipt.ScopeProject || d.Project == "" {
			return nil, &NotSupportedError{Host: spec.host, Operation: "project scope delivery"}
		}

		spec.project = d.Project
		spec = spec.projectScope()

		// A project-scope delivery must not write outside the project. If the
		// host has no project form for a surface the package needs, the spec
		// still carries the user-scope path — and writing there would put a
		// project's MCP servers into the user's home. Refuse instead.
		if err := spec.refuseOutsideProject(); err != nil {
			return nil, err
		}
	}

	planner := &loosePlanner{
		base: base,
		spec: spec,
		d:    d,
		pkg:  d.Package,
		plan: &loosePlan{},
	}

	planner.dataDir = dataDirPath(base, d.Package, spec.host)

	if err := planner.components(); err != nil {
		return nil, err
	}

	if err := planner.hooks(); err != nil {
		return nil, err
	}

	if err := planner.hookModules(); err != nil {
		return nil, err
	}

	if err := planner.mcp(ctx); err != nil {
		return nil, err
	}

	if len(planner.missing) > 0 {
		slices.Sort(planner.missing)
		planner.missing = slices.Compact(planner.missing)

		return nil, &MissingSecretsError{Names: planner.missing}
	}

	if err := planner.flushConfigs(); err != nil {
		return nil, err
	}

	return planner.plan, nil
}

// dataDirPath resolves the ${PLUGIN_DATA} target of the delivery without
// creating it.
func dataDirPath(base *Base, pkg Package, id ID) string {
	if pkg.DataDir != "" {
		return pkg.DataDir
	}

	if base.store != nil {
		path, err := base.store.PackageDataPath(pkg.ID, string(id))
		if err == nil {
			return path
		}
	}

	return ""
}

// components plans the payload components in canonical order.
func (p *loosePlanner) components() error {
	components := slices.Clone(p.pkg.Components)

	slices.SortFunc(components, func(a, b manifest.Component) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name), cmp.Compare(a.Path, b.Path))
	})

	for _, component := range components {
		plan := p.componentPlan(component.Kind)
		if plan == nil {
			p.note("%s component %q is not expressible in loose %s; skipped", component.Kind, component.Name, p.spec.host)

			continue
		}

		// Every name becomes one path element of a target; a name that is not
		// one would place the file outside its host dir.
		if !validElement(component.Name) {
			return p.deliveryError(stepPlan, fmt.Errorf("%s name %q is not a safe path element", component.Kind, component.Name))
		}

		if err := plan(component); err != nil {
			return err
		}
	}

	return nil
}

// componentPlan returns the planner of one component kind; nil means the
// kind has no loose surface.
func (p *loosePlanner) componentPlan(kind manifest.Kind) func(manifest.Component) error {
	switch kind {
	case manifest.KindSkill:
		return p.skill
	case manifest.KindAgent:
		return p.agent
	case manifest.KindCommand:
		return p.command
	case manifest.KindRule:
		return p.rule
	default:
		return nil
	}
}

// note records one delivery note.
func (p *loosePlanner) note(format string, args ...any) {
	p.plan.notes = append(p.plan.notes, fmt.Sprintf(format, args...))
}

// sourcePath resolves one component path below the payload root.
func (p *loosePlanner) sourcePath(component manifest.Component) (string, error) {
	if component.Path == "" || strings.ContainsRune(component.Path, 0) || !filepath.IsLocal(component.Path) {
		return "", p.deliveryError(stepPlan, fmt.Errorf("component %q escapes the payload root", component.Path))
	}

	return filepath.Join(p.pkg.Root, filepath.FromSlash(component.Path)), nil
}

// sourceDir resolves a component to an existing directory, never a symlink.
func (p *loosePlanner) sourceDir(component manifest.Component) (string, error) {
	source, err := p.sourcePath(component)
	if err != nil {
		return "", err
	}

	info, err := os.Lstat(source)
	if err != nil {
		return "", p.deliveryError(stepPlan, err)
	}

	if !info.IsDir() {
		return "", p.deliveryError(stepPlan, fmt.Errorf("%s is not a directory", component.Path))
	}

	return source, nil
}

// sourceFile reads one regular component file, refusing symlinks.
func (p *loosePlanner) sourceFile(component manifest.Component) ([]byte, error) {
	source, err := p.sourcePath(component)
	if err != nil {
		return nil, err
	}

	info, err := os.Lstat(source)
	if err != nil {
		return nil, p.deliveryError(stepPlan, err)
	}

	if !info.Mode().IsRegular() {
		return nil, p.deliveryError(stepPlan, fmt.Errorf("%s is not a regular file", component.Path))
	}

	data, err := os.ReadFile(source) //nolint:gosec // G304: the path is resolved below the payload root
	if err != nil {
		return nil, p.deliveryError(stepPlan, err)
	}

	return data, nil
}

// deliveryError wraps one planning or install failure.
func (p *loosePlanner) deliveryError(step string, cause error) error {
	return &DeliveryError{Host: string(p.spec.host), Package: p.pkg.ID, Step: step, Cause: cause}
}

// rewrite applies the variable rewrite to one value.
func (p *loosePlanner) rewrite(label, value string) (string, error) {
	out, err := rewriteVariables(value, p.pkg.Root, p.dataDir, p.spec.variables)
	if err != nil {
		if unsupported, ok := errors.AsType[*UnsupportedVariableError](err); ok {
			unsupported.Path = label
		}

		return "", err
	}

	if p.dataDir != "" && strings.Contains(value, varPluginData) {
		p.plan.ensureData = true
	}

	return out, nil
}

// ownerOf returns the package owning a target path, if any.
func (p *loosePlanner) ownerOf(target string) (string, bool) {
	if p.base.ownership == nil {
		return "", false
	}

	return p.base.ownership.Owner(target)
}

// checkOwnership refuses a target that belongs to someone else; ok is false
// when the caller must skip the component.
func (p *loosePlanner) checkOwnership(target string, skip bool) (bool, error) {
	owner, owned := p.ownerOf(target)

	switch {
	case !owned && !fileExists(target):
		return true, nil
	case owned && owner == p.pkg.ID:
		return true, nil
	case skip:
		p.note("%s exists and is not owned by %s; skipped", target, p.pkg.ID)

		return false, nil
	default:
		return false, &CollisionError{Path: target, Owner: owner}
	}
}

// skill plans one skill tree copy.
func (p *loosePlanner) skill(component manifest.Component) error {
	source, err := p.sourceDir(component)
	if err != nil {
		return err
	}

	target := filepath.Join(p.spec.skillsDir, component.Name)

	keep, err := p.checkOwnership(target, false)
	if err != nil || !keep {
		return err
	}

	sum := component.Digest
	if !sum.Valid() {
		if sum, err = digest.Tree(source); err != nil {
			return p.deliveryError(stepPlan, err)
		}
	}

	_, existed := p.ownerOf(target)

	p.plan.add(
		looseStep{kind: stepCopyTree, path: target, src: source, existed: existed, digest: sum, mode: 0o700},
		receipt.Artifact{Kind: "skill", Name: component.Name, Path: target, Digest: sum},
		receipt.Op{Kind: receipt.OpCopyTree, Path: target, Digest: sum, Mode: 0o700, Existed: existed},
	)

	return nil
}

// agent plans one host-side agent file.
func (p *loosePlanner) agent(component manifest.Component) error {
	// A host with no subagent surface (pi, DSH) skips the component: joining
	// an empty dir would plan a path relative to the process, not to the home.
	if p.spec.agentsDir == "" {
		p.note("%s component %q is not expressible in loose %s; skipped", component.Kind, component.Name, p.spec.host)

		return nil
	}

	target := filepath.Join(p.spec.agentsDir, component.Name+p.spec.agentExtension())

	keep, err := p.checkOwnership(target, false)
	if err != nil || !keep {
		return err
	}

	data, err := p.sourceFile(component)
	if err != nil {
		return err
	}

	if !strings.HasSuffix(strings.ToLower(component.Path), markdownExt) {
		p.note("agent %q: source %s is not markdown; skipped", component.Name, component.Path)

		return nil
	}

	agent, err := render.ParseAgentMarkdown(data)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	rendered, renderErr := p.spec.renderedAgent(agent, component.Name)
	if rendered == nil {
		p.note("agent %q is not expressible in %s; skipped", component.Name, p.spec.host)

		return nil
	}

	p.noteDropped("agent", component.Name, renderErr)

	_, existed := p.ownerOf(target)

	return p.writeRendered("agent", component.Name, target, rendered, existed)
}

// noteDropped records the canonical fields a dialect renderer could not carry.
func (p *loosePlanner) noteDropped(kind, name string, err error) {
	if err == nil {
		return
	}

	fields := inexpressibleFields(err)
	if len(fields) == 0 {
		p.note("%s %q: %v", kind, name, err)

		return
	}

	p.note("%s %q: field(s) %s are not expressible in %s; dropped",
		kind, name, strings.Join(fields, ", "), p.spec.host)
}

// inexpressibleFields flattens joined *render.InexpressibleError values into
// their field names.
func inexpressibleFields(err error) []string {
	var fields []string

	var walk func(error)

	walk = func(current error) {
		if current == nil {
			return
		}

		if multi, ok := current.(interface{ Unwrap() []error }); ok {
			for _, inner := range multi.Unwrap() {
				walk(inner)
			}

			return
		}

		if target, ok := errors.AsType[*render.InexpressibleError](current); ok {
			fields = append(fields, target.Field)
		}
	}

	walk(err)

	return fields
}

// command plans one host-side command file; a foreign target is skipped with a
// note instead of failing the cell.
func (p *loosePlanner) command(component manifest.Component) error {
	// A surface without a commands directory skips the component: joining an
	// empty dir would plan a path relative to the process, not to the home.
	if p.spec.commandsDir == "" {
		p.note("%s: command %q skipped: no commands directory is configured on this surface", component.Kind, component.Name)

		return nil
	}

	target := filepath.Join(p.spec.commandsDir, component.Name+p.spec.commandExtension())

	keep, err := p.checkOwnership(target, true)
	if err != nil || !keep {
		return err
	}

	cmd, ok, err := p.commandDocument(component)
	if err != nil || !ok {
		return err
	}

	rendered, renderErr := p.spec.renderedCommand(cmd)
	if rendered == nil {
		p.note("command %q is not expressible in %s; skipped", component.Name, p.spec.host)

		return nil
	}

	p.noteDropped("command", component.Name, renderErr)

	_, existed := p.ownerOf(target)

	return p.writeRendered("command", component.Name, target, rendered, existed)
}

// commandDocument lifts one command source into the canonical markdown.
func (p *loosePlanner) commandDocument(component manifest.Component) (render.Command, bool, error) {
	data, err := p.sourceFile(component)
	if err != nil {
		return render.Command{}, false, err
	}

	switch strings.ToLower(filepath.Ext(component.Path)) {
	case markdownExt:
		cmd, parseErr := render.ParseCommandMarkdown(data)
		if parseErr != nil {
			return render.Command{}, false, p.deliveryError(stepPlan, parseErr)
		}

		cmd.Name = component.Name

		return cmd, true, nil
	case tomlExt:
		lifted, ok, liftErr := render.LiftGeminiCommand(component.Name, data)
		if liftErr != nil {
			return render.Command{}, false, p.deliveryError(stepPlan, liftErr)
		}

		if !ok {
			p.note("command %q: the TOML source carries no prompt; skipped", component.Name)

			return render.Command{}, false, nil
		}

		cmd, parseErr := render.ParseCommandMarkdown(lifted)
		if parseErr != nil {
			return render.Command{}, false, p.deliveryError(stepPlan, parseErr)
		}

		cmd.Name = component.Name

		return cmd, true, nil
	default:
		p.note("%s: command %q is not expressible in loose %s; skipped", component.Kind, component.Name, p.spec.host)

		return render.Command{}, false, nil
	}
}

// rule plans one rule: the host-native rulebook document when the surface
// declares a rules dir, else the D23 skill wrapper.
func (p *loosePlanner) rule(component manifest.Component) error {
	data, err := p.sourceFile(component)
	if err != nil {
		return err
	}

	if p.spec.rulesDir != "" {
		return p.nativeRule(component, data)
	}

	rel, content, err := render.RuleSkill(component.Name, data)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	target := filepath.Join(p.spec.skillsDir, filepath.FromSlash(strings.TrimPrefix(rel, "skills/")))
	file := filepath.Join(target, skillFile)

	// The receipt records the rendered SKILL.md, not its dir: a re-delivery
	// proves ownership through the file, else the dir is checked as a whole.
	guarded := target
	if owner, owned := p.ownerOf(file); owned && owner == p.pkg.ID {
		guarded = file
	}

	keep, err := p.checkOwnership(guarded, false)
	if err != nil || !keep {
		return err
	}

	_, dirOwned := p.ownerOf(target)
	_, fileOwned := p.ownerOf(file)

	return p.writeRendered("rule", component.Name, file, content, dirOwned || fileOwned)
}

// nativeRule plans one host-native rule document (`<rulesDir>/<name>.md`); the
// renderer preserves the source frontmatter and guarantees what the host needs
// to see the rule at all.
func (p *loosePlanner) nativeRule(component manifest.Component, data []byte) error {
	target := filepath.Join(p.spec.rulesDir, component.Name+markdownExt)

	keep, err := p.checkOwnership(target, false)
	if err != nil || !keep {
		return err
	}

	rendered, err := p.spec.renderedRule(component.Name, data)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	_, existed := p.ownerOf(target)

	return p.writeRendered("rule", component.Name, target, rendered, existed)
}

// writeRendered plans one rendered file write.
func (p *loosePlanner) writeRendered(kind, name, target string, data []byte, existed bool) error {
	sum := digest.Bytes(data)

	p.plan.add(
		looseStep{kind: stepWriteFile, path: target, data: data, mode: 0o600, existed: existed, digest: sum},
		receipt.Artifact{Kind: kind, Name: name, Path: target, Digest: sum},
		receipt.Op{Kind: receipt.OpWriteFile, Path: target, Digest: sum, Mode: 0o600, Existed: existed},
	)

	return nil
}

// hooks plans the shared hooks document edit; hooks are only written when the
// delivery allows them, and never for a host whose hooks are code modules
// rather than a declarative document (looseSpec.hooksBlocked).
func (p *loosePlanner) hooks() error {
	if len(p.pkg.Hooks) == 0 {
		return nil
	}

	if p.spec.hooksBlocked != "" {
		p.note("%d hook(s) skipped: %s", len(p.pkg.Hooks), p.spec.hooksBlocked)

		return nil
	}

	if !p.d.AllowHooks {
		p.note("hooks skipped: consent is pending (allow-hooks is false)")

		return nil
	}

	// The policy goes first: hooks it forbids are never delivered, so their
	// commands are neither rewritten nor refused.
	permitted, err := p.hooksPermitted()
	if err != nil {
		return err
	}

	if !permitted {
		p.note("hooks skipped: allowManagedHooksOnly policy forbids plugin hooks")

		return nil
	}

	rewritten, err := p.rewriteHooks()
	if err != nil {
		return err
	}

	file := p.spec.hooksFile()

	existing, err := readOptionalFile(file)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	return p.planHooks(file, existing, rewritten)
}

// planHooks renders the merged hooks object and queues its key edit.
func (p *loosePlanner) planHooks(file string, existing []byte, rewritten []manifest.Hook) error {
	// The hook planner reads plain JSON; the write path below keeps the
	// original JSONC bytes so comments survive the edit (DESIGN §4.3).
	planning, err := jsoncToJSON(existing)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	plan, err := render.PlanHooks(p.spec.hooksDialect(), planning, rewritten, p.hookRecordOwnership(rewritten))
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	p.plan.notes = append(p.plan.notes, plan.Warnings...)

	// A record verger owns, whose bytes the user changed, is a refusal the
	// caller has to see as hands-off. Returning a warning alone would let
	// the cell read "current" over a delivery that wrote nothing, which is
	// the one answer a script must not be able to trust.
	if len(plan.Refused) > 0 {
		return &render.HandsOffError{
			Path:    file,
			KeyPath: plan.Refused[0],
			Reason:  "the record changed outside verger; left in place: " + strings.Join(plan.Refused, ", "),
		}
	}

	if p.spec.hooksTrustNote && plan.Rendered > 0 {
		p.note("%d hook(s) are rendered into %s; %s trusts hooks by hash and skips new or changed ones until you review them in /hooks",
			plan.Rendered, file, p.spec.host)
	}

	if plan.File == nil {
		return nil
	}

	value := any(nil)

	if err := json.Unmarshal(plan.File, &value); err != nil {
		return p.deliveryError(stepPlan, err)
	}

	var ownedDigest digest.Hash

	// A dialect whose records verger claims one by one does not own the whole
	// `hooks` object: the user adds their own records to the same document, and
	// that must not read as verger's value moving (DRIFT-2). In that case the
	// coarse key is marked per-record and recordHookRecords carries the
	// ownership.
	perRecord := p.spec.hookRecordPath != nil

	if current, ok := jsoncMember(existing, "hooks"); ok && !perRecord {
		ownedDigest = canonicalValueDigest(current)
	}

	pe := pendingEdit{
		edit:  render.Edit{Path: "hooks", Value: value},
		kind:  "hook",
		name:  "hooks",
		owned: ownedDigest,
	}

	if perRecord {
		pe.markPerRecord = true
	}

	p.queueConfigEdit(file, false, render.EditJSONC, pe)

	p.recordHookRecords(plan, file)

	return nil
}

// recordHookRecords claims one receipt artifact per record the plan wrote, so a
// later delivery can tell a hand edit from its own bytes. The op is
// receipt.OpRecord: it has no inverse of its own — the document's own write is
// what a removal reverses — and it keeps "every artifact is claimed by an op on
// its path" true, which pkg/receipt validates.
func (p *loosePlanner) recordHookRecords(plan render.HookPlan, hooksFile string) {
	if p.spec.hookRecordPath == nil {
		return
	}

	for _, event := range plan.Events {
		for _, record := range event.Records {
			p.plan.record(
				receipt.Artifact{
					Kind:   cursorHookRecordKind,
					Name:   record.Name,
					Path:   p.spec.hookRecordPath(event.Name, record.Name),
					Digest: record.Digest,
				},
				receipt.Op{
					Kind: receipt.OpRecord,
					Path: p.spec.hookRecordPath(event.Name, record.Name),
					// Note names the DOCUMENT, not the record: it is what
					// tells pkg/apply the document's own records are checked
					// one by one here, so the file-level byte check is skipped
					// for it and a user adding their own record is not a drift
					// on ours.
					Note: hooksFile,
				},
			)
		}
	}
}

// hookModules plans the host-native hook modules of the payload: files below
// <payload>/runtime/<host>/hooks/{pre,post} are copied verbatim into the host's
// hook directory, because such a host discovers code modules, not a declarative
// hook document (omp 18.4.1: a factory directly in hooks/ is silently ignored,
// so only a pre|post directory is delivered). A module is code the host runs on
// every session without a sandbox or a trust gate, so it is written only for a
// delivery that carries hook consent, and the plan says so out loud.
func (p *loosePlanner) hookModules() error {
	if p.spec.hookModulesDir == "" {
		return nil
	}

	modules, err := scanHookModules(p.pkg.Root, string(p.spec.host))
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	if len(modules) == 0 {
		return nil
	}

	if !p.d.AllowHooks {
		p.note("%d host hook module(s) skipped: consent is pending (allow-hooks is false)", len(modules))

		return nil
	}

	written := 0

	for _, module := range modules {
		target := filepath.Join(p.spec.hookModulesDir, module.phase, module.name+module.ext)

		keep, err := p.checkOwnership(target, false)
		if err != nil || !keep {
			return err
		}

		data, err := os.ReadFile(module.path) //nolint:gosec // G304: a module below the payload root
		if err != nil {
			return p.deliveryError(stepPlan, err)
		}

		_, existed := p.ownerOf(target)

		if err := p.writeRendered("hook-module", module.name, target, data, existed); err != nil {
			return err
		}

		written++
	}

	p.note("%d hook module(s) are code the host runs unsandboxed on every session, with no trust gate of its own; restart %s to load them (a live session imports neither the module nor an edit to it, and reloading plugins does not either)",
		written, p.spec.host)

	return nil
}

// hooksPermitted consults the shared policy checker when the surface carries a
// user home; a host without policy documents allows hooks.
func (p *loosePlanner) hooksPermitted() (bool, error) {
	if p.spec.home == "" {
		return true, nil
	}

	return hooksAllowed(p.spec.host, p.spec.home)
}

// pendingConfig is one shared config document receiving key edits from several
// planners (the Gemini settings.json carries both hooks and mcpServers) so it
// is read once and written exactly once.
type pendingConfig struct {
	file    string
	tomlDoc bool
	edit    func([]byte, []render.Edit, render.Owned) ([]byte, []render.Change, error)
	// member reads one record of a document the JSONC/TOML decoder cannot
	// read; nil reads a key path of the document.
	member func([]byte, string) (any, bool, error)
	// wholeFile records the document as one file op rather than one
	// config-key op per record (see mcpConfigSpec.wholeFile).
	wholeFile bool
	// records returns one receipt artifact per owned record of a whole-file
	// document (see mcpConfigSpec.recordArtifacts).
	records func([]render.Edit) []receipt.Artifact
	edits   []pendingEdit
	// recorded are keys this document owns without writing them: their value
	// already matches, and they must still be recorded or the next receipt
	// loses the ownership and an update reconciles them away.
	recorded []pendingEdit
	owned    render.Owned
	secret   bool
}

// readMember reads one record of this document by its key path.
func (cfg *pendingConfig) readMember(file []byte, keyPath string) (any, bool, error) {
	if cfg.member != nil {
		return cfg.member(file, keyPath)
	}

	return configMember(file, keyPath, cfg.tomlDoc)
}

// setMember installs the record reader of a document the JSONC/TOML decoder
// cannot read (the DSH home patch layer).
func (cfg *pendingConfig) setMember(member func([]byte, string) (any, bool, error)) {
	cfg.member = member
}

// setWholeFile marks the document as one file op rather than one config-key op
// per record and installs the per-record receipt identities of the surface.
func (cfg *pendingConfig) setWholeFile(whole bool, records func([]render.Edit) []receipt.Artifact) {
	cfg.wholeFile = whole
	cfg.records = records
}

// pendingEdit is one key edit plus the artifact identity it records.
type pendingEdit struct {
	edit render.Edit
	kind string
	name string
	// markPerRecord hands ownership of this key over to its records, so the
	// whole-object digest is not an ownership signal (DRIFT-2).
	markPerRecord bool
	owned         digest.Hash // current value digest the edit adopts as owned; empty leaves the key unowned (an existing value is hands-off)
}

// queueConfigEdit adds one key edit to the pending document of a file.
func (p *loosePlanner) queueConfigEdit(
	file string, tomlDoc bool,
	edit func([]byte, []render.Edit, render.Owned) ([]byte, []render.Change, error),
	pe pendingEdit,
) {
	cfg := p.configFor(file, tomlDoc, edit)

	if pe.markPerRecord {
		if cfg.owned == nil {
			cfg.owned = render.Owned{}
		}

		cfg.owned[render.PerRecordKey(pe.edit.Path)] = ""
	}

	if pe.owned != "" {
		if cfg.owned == nil {
			cfg.owned = render.Owned{}
		}

		cfg.owned[pe.edit.Path] = pe.owned
	}

	cfg.edits = append(cfg.edits, pe)
}

// configFor finds or creates the pending document of one file.
func (p *loosePlanner) configFor(
	file string, tomlDoc bool,
	edit func([]byte, []render.Edit, render.Owned) ([]byte, []render.Change, error),
) *pendingConfig {
	for _, cfg := range p.configs {
		if cfg.file == file {
			return cfg
		}
	}

	cfg := &pendingConfig{file: file, tomlDoc: tomlDoc, edit: edit}
	p.configs = append(p.configs, cfg)

	return cfg
}

// flushConfigs applies every pending document edit once, in planning order.
func (p *loosePlanner) flushConfigs() error {
	for _, cfg := range p.configs {
		if err := p.flushConfig(cfg); err != nil {
			return err
		}
	}

	return nil
}

// flushConfig applies the accumulated edits of one document, and records the
// keys it owns without writing them.
func (p *loosePlanner) flushConfig(cfg *pendingConfig) error {
	if len(cfg.edits) == 0 && len(cfg.recorded) == 0 {
		return nil
	}

	existing, err := readOptionalFile(cfg.file)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	// Nothing is written for a document whose keys this delivery only records,
	// so a read-only file is not an obstacle to owning them.
	if len(cfg.edits) == 0 {
		p.recordUnchangedOrWholeFile(cfg, cfg.recorded, existing)

		return nil
	}

	if isReadOnlyConfig(cfg.file) {
		return p.deliveryError(stepPlan, fmt.Errorf("%s is read-only", cfg.file))
	}

	out, changes, err := cfg.edit(existing, cfg.keyEdits(), cfg.owned)
	if err != nil {
		return err
	}

	if len(changes) == 0 {
		owned := append(slices.Clone(cfg.edits), cfg.recorded...)

		p.recordUnchangedOrWholeFile(cfg, owned, existing)

		return nil
	}

	if cfg.wholeFile {
		return p.addWholeFile(cfg, existing, out, p.configWriteMode(cfg))
	}

	ops := make([]receipt.Op, 0, len(changes))
	replaced := make([]any, 0, len(changes))

	for _, change := range changes {
		ops = append(ops, receipt.Op{
			Kind: receipt.OpConfigKey, Path: cfg.file, KeyPath: change.Path, Digest: change.Digest, Existed: change.Existed,
		})
		replaced = append(replaced, change.Previous)
	}

	// A key of this document that needed no change is still this package's: its
	// op is recorded (with no backup) beside the changed ones, or the receipt
	// would lose the ownership and an update would reconcile the key away.
	ops = p.appendUnchanged(cfg, existing, changes, ops)

	p.plan.addConfig(
		looseStep{kind: stepConfig, path: cfg.file, data: out, mode: p.configWriteMode(cfg), digest: changes[0].Digest},
		documentArtifact(cfg.file, cfg.editFor(changes[0].Path), changes[0].Digest), ops, replaced,
	)

	p.appendMCPClaims(cfg)

	return nil
}

// keyEdits returns the plain key edits of a document, in planning order.
func (cfg *pendingConfig) keyEdits() []render.Edit {
	edits := make([]render.Edit, 0, len(cfg.edits))

	for _, pe := range cfg.edits {
		edits = append(edits, pe.edit)
	}

	return edits
}

// configWriteMode is the mode the document is written with: its own, or 0600
// when it carries a resolved secret — which the delivery says out loud.
func (p *loosePlanner) configWriteMode(cfg *pendingConfig) fs.FileMode {
	if !cfg.secret {
		return configMode(cfg.file)
	}

	p.note("%s carries a resolved secret; mode is 0600", cfg.file)

	return 0o600
}

// isReadOnlyConfig reports whether a config document cannot be written because
// the user removed the write bit; a missing document is written, not refused.
func isReadOnlyConfig(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	return info.Mode().IsRegular() && info.Mode().Perm()&0o200 == 0
}

// appendUnchanged adds one pre-existing op per owned key the write did not have
// to change.
func (p *loosePlanner) appendUnchanged(cfg *pendingConfig, existing []byte, changes []render.Change, ops []receipt.Op) []receipt.Op {
	changed := make(map[string]bool, len(changes))

	for _, change := range changes {
		changed[change.Path] = true
	}

	owned := append(slices.Clone(cfg.edits), cfg.recorded...)

	for _, pe := range owned {
		if changed[pe.edit.Path] {
			continue
		}

		current, exists, err := cfg.readMember(existing, pe.edit.Path)
		if err != nil || !exists {
			continue
		}

		ops = append(ops, receipt.Op{
			Kind: receipt.OpConfigKey, Path: cfg.file, KeyPath: pe.edit.Path,
			Digest: canonicalValueDigest(current), Existed: true,
		})
	}

	return ops
}

// recordUnchangedOrWholeFile keeps a document owned when the delivery writes
// nothing. A key-path document keeps one op per key; a whole-file document
// records one file op with no backup, so a removal leaves the document in place
// instead of deleting a file the user also owns. A document this package never
// touched records nothing.
func (p *loosePlanner) recordUnchangedOrWholeFile(cfg *pendingConfig, edits []pendingEdit, existing []byte) {
	if !cfg.wholeFile {
		p.recordUnchanged(cfg, edits, existing)

		return
	}

	if len(edits) == 0 {
		return
	}

	sum := digest.Bytes(existing)
	first := pendingEdit{edit: render.Edit{Path: cfg.file}, kind: cfg.documentKind()}

	p.plan.recordConfig(documentArtifact(cfg.file, first, sum), []receipt.Op{
		{Kind: receipt.OpWriteFile, Path: cfg.file, Digest: sum, Existed: true},
	})

	p.appendRecordArtifacts(cfg, keyEditsOf(edits))
	p.appendMCPClaims(cfg)
}

// addWholeFile records a whole-file document write as one file op, so pkg/apply
// restores the trashed bytes verbatim: a document verger co-owns with the user
// keeps every foreign entry and comment its next removal would otherwise drop.
func (p *loosePlanner) addWholeFile(cfg *pendingConfig, existing, out []byte, mode fs.FileMode) error {
	sum := digest.Bytes(out)
	existed := existing != nil

	step := looseStep{kind: stepConfig, path: cfg.file, data: out, mode: mode, existed: existed, digest: sum}
	first := pendingEdit{edit: render.Edit{Path: cfg.file}, kind: cfg.documentKind()}

	p.plan.addConfig(
		step,
		documentArtifact(cfg.file, first, sum),
		[]receipt.Op{{Kind: receipt.OpWriteFile, Path: cfg.file, Digest: sum, Existed: existed}},
		nil,
	)
	p.appendRecordArtifacts(cfg, cfg.keyEdits())

	return nil
}

// keyEditsOf returns the plain key edits of a list of pending edits.
func keyEditsOf(edits []pendingEdit) []render.Edit {
	out := make([]render.Edit, 0, len(edits))

	for _, pe := range edits {
		out = append(out, pe.edit)
	}

	return out
}

// documentKind is the artifact kind of a whole-file document: the records of
// such a document are owned by id, so only the adapter can verify them and
// pkg/apply must not hold the artifact against a byte digest of a file the user
// may also edit. Every other document is an MCP config document.
func (cfg *pendingConfig) documentKind() string {
	if cfg.wholeFile {
		return artifactPatchDocument
	}

	return artifactMCP
}

// appendMCPClaims records one receipt artifact per MCP server of a shared
// document. Gemini writes `settings.json` for both hooks and MCP servers, so
// the document's own artifact carries whichever component planned first and the
// receipt would claim no `mcp` kind at all (F3). The per-server claim below is
// what restores it — and it is what lets a later delivery prove one server is
// still verger's while another moved.
func (p *loosePlanner) appendMCPClaims(cfg *pendingConfig) {
	if cfg.records == nil {
		return
	}

	claims := make([]render.Edit, 0, len(cfg.edits))

	for _, pe := range cfg.edits {
		if pe.kind == artifactMCP {
			claims = append(claims, pe.edit)
		}
	}

	if len(claims) == 0 {
		return
	}

	artifacts := cfg.records(claims)

	ops := make([]receipt.Op, 0, len(artifacts))

	for _, artifact := range artifacts {
		ops = append(ops, receipt.Op{
			Kind: receipt.OpRecord, Path: artifact.Path, Note: cfg.file, Digest: artifact.Digest,
		})
	}

	p.plan.artifacts = append(p.plan.artifacts, artifacts...)
	p.plan.ops = append(p.plan.ops, ops...)
}

// appendRecordArtifacts records one receipt artifact per owned record of a
// whole-file document, each backed by a record op carrying that record's own
// digest, so the next delivery can prove the record still holds the value verger
// wrote and a hand edit is hands-off instead of a silent overwrite. The op has
// no inverse of its own: the document's file op is what a removal reverses. A
// surface without per-record ownership records none.
func (p *loosePlanner) appendRecordArtifacts(cfg *pendingConfig, edits []render.Edit) {
	if cfg.records == nil {
		return
	}

	artifacts := cfg.records(edits)

	ops := make([]receipt.Op, 0, len(artifacts))

	for _, artifact := range artifacts {
		// The op claims the artifact's own identity, so every artifact is
		// backed by an op on its path; Note names the document the record
		// lives in, which is what a removal restores.
		ops = append(ops, receipt.Op{
			Kind: receipt.OpRecord, Path: artifact.Path, Note: cfg.file, Digest: artifact.Digest,
		})
	}

	p.plan.artifacts = append(p.plan.artifacts, artifacts...)
	p.plan.ops = append(p.plan.ops, ops...)
}

// recordUnchanged keeps a document's keys as owned when the delivery writes
// nothing: the values already match what the package wants, so the receipt must
// still record them, or a later update reconciles the ownership away and an
// inverse restores a value that predates the delivery.
func (p *loosePlanner) recordUnchanged(cfg *pendingConfig, edits []pendingEdit, existing []byte) {
	ops := make([]receipt.Op, 0, len(edits))

	var (
		first digest.Hash
		pe    pendingEdit
	)

	for _, candidate := range edits {
		current, exists, err := cfg.readMember(existing, candidate.edit.Path)
		if err != nil || !exists {
			continue
		}

		sum := canonicalValueDigest(current)
		if first == "" {
			first, pe = sum, candidate
		}

		ops = append(ops, receipt.Op{
			Kind: receipt.OpConfigKey, Path: cfg.file, KeyPath: candidate.edit.Path, Digest: sum, Existed: true,
		})
	}

	if len(ops) == 0 {
		return
	}

	p.plan.recordConfig(documentArtifact(cfg.file, pe, first), ops)
}

// documentArtifact is the one artifact a shared config document records: its
// path is the document and its digest the value of the document's first recorded
// key, which is the key pkg/apply resolves the ops of that path to.
func documentArtifact(file string, first pendingEdit, sum digest.Hash) receipt.Artifact {
	kind := artifactMCP
	if first.kind != "" {
		kind = first.kind
	}

	return receipt.Artifact{Kind: kind, Name: filepath.Base(file), Path: file, Digest: sum}
}

// editFor returns the pending edit of one key path.
func (cfg *pendingConfig) editFor(path string) pendingEdit {
	for _, pe := range cfg.edits {
		if pe.edit.Path == path {
			return pe
		}
	}

	return pendingEdit{}
}

// rewriteHooks applies the variable rewrite to every hook command; a command
// carrying `{secret:NAME}` is refused (decision Q3): the secret would land in a
// plain settings document and in the process list.
func (p *loosePlanner) rewriteHooks() ([]manifest.Hook, error) {
	hooks := slices.Clone(p.pkg.Hooks)

	slices.SortFunc(hooks, func(a, b manifest.Hook) int {
		return cmp.Or(
			cmp.Compare(a.Event, b.Event), cmp.Compare(a.Matcher, b.Matcher),
			cmp.Compare(a.Command, b.Command), cmp.Compare(a.Timeout, b.Timeout),
		)
	})

	for i, hook := range hooks {
		if names := secretNames(hook.Command); len(names) > 0 {
			return nil, &SecretInHookError{Host: p.spec.host, Event: hook.Event, Names: names}
		}

		command, err := p.rewrite("hook "+hook.Event, hook.Command)
		if err != nil {
			return nil, err
		}

		hooks[i].Command = command
	}

	return hooks, nil
}

// mcp plans the MCP surface: a shared config document when the surface
// declares one, else the host CLI calls.
func (p *loosePlanner) mcp(ctx context.Context) error {
	if p.spec.mcpConfig != nil {
		return p.planMCPConfig()
	}

	return p.mcpCLI(ctx)
}

// mcpCLI plans the Claude-style MCP CLI calls: variables rewritten, secrets
// resolved to literals for argv only, and the inverse remove recorded — only
// for a name the ownership gate cleared, so neither a delivery nor a rollback
// from its dry-run plan can remove a server verger did not create.
func (p *loosePlanner) mcpCLI(ctx context.Context) error {
	servers := slices.Clone(p.pkg.MCP)

	slices.SortFunc(servers, func(a, b manifest.MCPServer) int { return cmp.Compare(a.Name, b.Name) })

	for _, server := range servers {
		rewritten, err := p.rewriteServer(server)
		if err != nil {
			return err
		}

		resolved, missing := resolveSecrets(p.base.secrets, rewritten)
		p.missing = append(p.missing, missing...)

		if len(missing) > 0 {
			continue
		}

		args, err := p.spec.mcpAddArgs(resolved)
		if err != nil {
			return p.deliveryError(stepPlan, err)
		}

		sum := digest.Bytes(mustJSON(rewritten))

		claim, err := p.claimMCPName(ctx, server.Name, sum)
		if err != nil {
			return err
		}

		artifact := receipt.Artifact{Kind: artifactMCP, Name: server.Name, Path: p.mcpArtifactPath(server.Name), Digest: sum}
		inverse := receipt.Op{Kind: receipt.OpHostInstall, Command: p.spec.mcpRemoveArgs(server.Name), Existed: claim != mcpFree}

		switch claim {
		case mcpOwnUnchanged:
			p.plan.record(artifact, inverse)
		case mcpOwnChanged:
			// The host refuses `mcp add` on a configured name (NF-2).
			p.plan.steps = append(p.plan.steps, looseStep{kind: stepHostCommand, command: p.spec.mcpRemoveArgs(server.Name)})
			p.plan.add(looseStep{kind: stepHostCommand, command: args, digest: sum}, artifact, inverse)
		default:
			p.plan.add(looseStep{kind: stepHostCommand, command: args, digest: sum}, artifact, inverse)
		}
	}

	return nil
}

// mcpClaim is what the ownership gate learned about one server name.
type mcpClaim int

const (
	mcpFree         mcpClaim = iota // the host has no such server
	mcpOwnUnchanged                 // this package's server, configured as planned
	mcpOwnChanged                   // this package's server, configured otherwise (or unknown)
)

// mcpArtifactPath is the receipt identity of one CLI-managed MCP server; a
// receipt artifact at this path is the proof that verger created the server.
func (p *loosePlanner) mcpArtifactPath(name string) string {
	return string(p.spec.host) + "://mcp/" + name
}

// claimMCPName clears one CLI-managed MCP server name for this package
// (decision Q1, DESIGN §4.3/§4.8): a name another package records, or one the
// host has without a receipt of this package, is hands-off; a host probe
// that cannot tell fails the plan closed. For a name this package's receipt
// proves, the host is still asked (NF-2): a configured server is unchanged
// when the receipt digest equals sum, else it is re-added.
func (p *loosePlanner) claimMCPName(ctx context.Context, name string, sum digest.Hash) (mcpClaim, error) {
	path := p.mcpArtifactPath(name)

	owner, owned := p.ownerOf(path)
	if owned && owner != p.pkg.ID {
		return mcpFree, &render.HandsOffError{Path: path, KeyPath: name, Reason: "the MCP server name is recorded by " + owner}
	}

	exists, err := p.mcpServerExists(ctx, name)
	if err != nil {
		return mcpFree, err
	}

	switch {
	case !exists:
		return mcpFree, nil
	case !owned:
		return mcpFree, &render.HandsOffError{
			Path: path, KeyPath: name,
			Reason: "the host already has an MCP server with this name and no receipt proves verger created it",
		}
	case p.recordedDigest(path) == sum:
		return mcpOwnUnchanged, nil
	default:
		return mcpOwnChanged, nil
	}
}

// recordedDigest is the digest this package's receipt recorded for an
// artifact path, when the ownership source reports one.
func (p *loosePlanner) recordedDigest(path string) digest.Hash {
	digests, ok := p.base.ownership.(ArtifactDigests)
	if !ok {
		return ""
	}

	sum, _ := digests.ArtifactDigest(path)

	return sum
}

// mcpServerExists asks the host CLI whether a server name is configured. A
// clean exit is an existing server and a refusal naming the server unknown is
// a free name; any other refusal cannot prove the name free. A missing binary
// or a cancelled context passes through unchanged. The host grammar is
// unverified (OQ-T1.6.1, pinned by T1.13).
func (p *loosePlanner) mcpServerExists(ctx context.Context, name string) (bool, error) {
	if p.spec.mcpGetArgs == nil {
		return false, p.deliveryError(stepPlan, fmt.Errorf("mcp %s: the host offers no probe to prove the name free", name))
	}

	out, err := p.base.run(ctx, p.spec.binary, p.spec.mcpGetArgs(name))
	if err == nil {
		return true, nil
	}

	exit, ok := errors.AsType[*hostcli.ExitError](err)
	if !ok {
		return false, err
	}

	if reportsUnknownServer(exit.Stderr + "\n" + string(out)) {
		return false, nil
	}

	return false, p.deliveryError(stepPlan, fmt.Errorf("mcp %s: cannot tell whether the host already has the server: %w", name, err))
}

// reportsUnknownServer reports whether host output says the named MCP server
// is not configured; only server-specific messages count, so an unrelated
// "does not exist" never proves a name free.
func reportsUnknownServer(output string) bool {
	text := strings.ToLower(output)

	for _, marker := range []string{"no mcp server found", "no mcp server named", "mcp server not found"} {
		if strings.Contains(text, marker) {
			return true
		}
	}

	return false
}

// planMCPConfig plans the host's config-document MCP surface (Codex
// config.toml `[mcp_servers]`, Gemini settings.json `mcpServers`): variables
// rewritten, secrets resolved into the document (0600), foreign keys and
// comments preserved. An existing key whose value differs is hands-off: the
// cell fails instead of overwriting a value verger did not write in this
// delivery. The edits are queued and flushed once per document, so a file
// shared with the hooks planner is written exactly once.
func (p *loosePlanner) planMCPConfig() error {
	cfg := p.spec.mcpConfig

	servers, secret, err := p.resolvedMCPServers()
	if err != nil {
		return err
	}

	// A resolved secret value would be written verbatim into this document.
	// At user scope that document is the user's own config; at project scope
	// it usually lives in the repository, and a value that reaches a commit
	// is a leak. Refuse with the path and the reason rather than writing it.
	if secret {
		if err := p.refuseSecretIntoGit(cfg.path); err != nil {
			return err
		}
	}

	if len(servers) == 0 {
		return nil
	}

	edits, err := cfg.configEditsUnder(servers)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	existing, err := readOptionalFile(cfg.path)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	pending, err := mcpPendingEdits(existing, edits, cfg)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	prefix := mcpContainerPrefix(cfg)

	document := p.configFor(cfg.path, cfg.toml, cfg.edit)
	document.setMember(cfg.member)
	document.setWholeFile(cfg.wholeFile, cfg.recordArtifacts)

	p.recordMCPEdits(cfg, edits, pending, prefix, document)

	if secret && len(pending) > 0 {
		p.configFor(cfg.path, cfg.toml, cfg.edit).secret = true
	}

	return nil
}

// recordMCPEdits files every server of this package under one config document:
// the ones that need a write are queued, the ones already written are
// recorded without one. It is one loop lifted out of planMCPConfig, which was
// over the complexity budget with it inline.
func (p *loosePlanner) recordMCPEdits(
	cfg *mcpConfigSpec,
	edits []render.Edit,
	pending []render.Edit,
	prefix string,
	document *pendingConfig,
) {
	// Every server is this package's, whether or not this delivery has to write
	// it: the ones already configured exactly as wanted are recorded without a
	// write, the rest are written. Dropping the former would leave them
	// unowned, and an update would reconcile them away one by one.
	pendingPaths := make(map[string]bool, len(pending))
	for _, edit := range pending {
		pendingPaths[edit.Path] = true
	}

	document.setWholeFile(cfg.wholeFile, cfg.recordArtifacts)

	for _, edit := range edits {
		pe := pendingEdit{edit: edit, kind: artifactMCP, name: strings.TrimPrefix(edit.Path, prefix)}

		// A record the previous receipt recorded is verger's: the editor
		// updates it, and a record whose value no longer matches that digest
		// is hands-off rather than silently overwritten.
		if cfg.recordPath != nil {
			pe.owned = p.recordedDigest(cfg.recordPath(pe.name))
		}

		if pendingPaths[edit.Path] {
			p.queueConfigEdit(cfg.path, cfg.toml, cfg.edit, pe)

			continue
		}

		document.recorded = append(document.recorded, pe)
	}
}

// resolvedMCPServers rewrites the variables of every server and resolves its
// secrets; secret reports whether any `{secret:NAME}` reference was resolved.
func (p *loosePlanner) resolvedMCPServers() ([]manifest.MCPServer, bool, error) {
	servers := slices.Clone(p.pkg.MCP)

	slices.SortFunc(servers, func(a, b manifest.MCPServer) int { return cmp.Compare(a.Name, b.Name) })

	resolved := make([]manifest.MCPServer, 0, len(servers))
	secret := false

	for _, server := range servers {
		rewritten, err := p.rewriteServer(server)
		if err != nil {
			return nil, false, err
		}

		if serverHasSecretRef(rewritten) {
			secret = true
		}

		server, missing := resolveSecrets(p.base.secrets, rewritten)
		p.missing = append(p.missing, missing...)

		if len(missing) > 0 {
			continue
		}

		resolved = append(resolved, server)
	}

	return resolved, secret, nil
}

// mcpPendingEdits drops the edits whose value already matches the document and
// keeps the rest; a differing existing key stays unowned, so the editor
// reports hands-off instead of overwriting it.
func mcpPendingEdits(existing []byte, edits []render.Edit, cfg *mcpConfigSpec) ([]render.Edit, error) {
	pending := make([]render.Edit, 0, len(edits))

	for _, edit := range edits {
		current, exists, err := cfg.readMember(existing, edit.Path)
		if err != nil {
			return nil, err
		}

		if exists && canonicalValueDigest(current) == canonicalValueDigest(edit.Value) {
			continue
		}

		pending = append(pending, edit)
	}

	return pending, nil
}

// mcpContainerPrefix returns the config key prefix of one MCP surface: the
// adapter's override when it declares one (the OpenCode v1/v2 containers),
// else the dialect's own.
func mcpContainerPrefix(cfg *mcpConfigSpec) string {
	return cmp.Or(cfg.prefix, render.MCPPrefix(cfg.format))
}

// serverHasSecretRef reports whether a server still carries a `{secret:NAME}`
// reference (checked before resolution).
func serverHasSecretRef(server manifest.MCPServer) bool {
	if secretRefPattern.MatchString(server.URL) || slices.ContainsFunc(server.Command, secretRefPattern.MatchString) {
		return true
	}

	for _, value := range server.Env {
		if secretRefPattern.MatchString(value) {
			return true
		}
	}

	for _, value := range server.Headers {
		if secretRefPattern.MatchString(value) {
			return true
		}
	}

	return false
}

// secretNames returns the sorted distinct names of the `{secret:NAME}`
// references in one value.
func secretNames(value string) []string {
	var names []string

	for _, ref := range secretRefPattern.FindAllString(value, -1) {
		if name, ok := secret.ParseRef(ref); ok {
			names = append(names, name)
		}
	}

	slices.Sort(names)

	return slices.Compact(names)
}

// rewriteServer rewrites the variables of every server field.
func (p *loosePlanner) rewriteServer(server manifest.MCPServer) (manifest.MCPServer, error) {
	label := "mcp " + server.Name

	out := server

	var err error

	if out.URL != "" {
		if out.URL, err = p.rewrite(label+" url", out.URL); err != nil {
			return manifest.MCPServer{}, err
		}
	}

	out.Command = slices.Clone(server.Command)

	for i, arg := range server.Command {
		if out.Command[i], err = p.rewrite(label+" arg", arg); err != nil {
			return manifest.MCPServer{}, err
		}
	}

	if out.Env, err = p.rewriteMap(label+" env", server.Env); err != nil {
		return manifest.MCPServer{}, err
	}

	if out.Headers, err = p.rewriteMap(label+" header", server.Headers); err != nil {
		return manifest.MCPServer{}, err
	}

	return out, nil
}

// rewriteMap rewrites every value of a string map.
func (p *loosePlanner) rewriteMap(label string, values map[string]string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}

	out := make(map[string]string, len(values))

	for key, value := range values {
		rewritten, err := p.rewrite(label+" "+key, value)
		if err != nil {
			return nil, err
		}

		out[key] = rewritten
	}

	return out, nil
}

// resolveSecrets replaces every `{secret:NAME}` reference, whole-value or
// embedded, with its literal and reports the names with no stored value.
func resolveSecrets(secrets *secret.Store, server manifest.MCPServer) (manifest.MCPServer, []string) {
	var missing []string

	convert := func(value string) string {
		if !strings.Contains(value, "{secret:") {
			return value
		}

		return secretRefPattern.ReplaceAllStringFunc(value, func(ref string) string {
			name, ok := secret.ParseRef(ref)
			if !ok {
				return ref
			}

			if secrets == nil {
				missing = append(missing, name)

				return ref
			}

			literal, found := secrets.Get(name)
			if !found {
				missing = append(missing, name)

				return ref
			}

			return literal
		})
	}

	out := server
	out.URL = convert(server.URL)
	out.Command = slices.Clone(server.Command)

	for i, arg := range server.Command {
		out.Command[i] = convert(arg)
	}

	if len(server.Env) > 0 {
		out.Env = make(map[string]string, len(server.Env))

		for key, value := range server.Env {
			out.Env[key] = convert(value)
		}
	}

	if len(server.Headers) > 0 {
		out.Headers = make(map[string]string, len(server.Headers))

		for key, value := range server.Headers {
			out.Headers[key] = convert(value)
		}
	}

	return out, missing
}

// deliverSurface plans and executes one loose surface: the shared shell of
// every loose-only adapter (plan, dry-run short circuit, execute, result).
// The caller owns the surface — its directories, its MCP container and its
// hook reason — and any lock that must cover the read and the write.
func deliverSurface(ctx context.Context, base *Base, spec looseSpec, d Delivery) (Result, error) {
	plan, err := planLoose(ctx, base, spec, d)
	if err != nil {
		return Result{}, err
	}

	if d.DryRun {
		return plan.result(d.Strategy, true), nil
	}

	err = base.executeLoose(ctx, spec, d.Package, plan)

	return plan.result(d.Strategy, false), err
}

// executeLoose runs a planned delivery; the plan is already collision-free.
func (b *Base) executeLoose(ctx context.Context, spec looseSpec, pkg Package, plan *loosePlan) error {
	if plan.ensureData {
		if b.store == nil {
			return &DeliveryError{Host: string(spec.host), Package: pkg.ID, Step: stepInstall, Cause: errors.New("a plugin data variable requires a store")}
		}

		if _, err := b.store.EnsurePackageData(pkg.ID, string(spec.host)); err != nil {
			return &DeliveryError{Host: string(spec.host), Package: pkg.ID, Step: stepInstall, Cause: err}
		}
	}

	for i := range plan.steps {
		if err := b.executeStep(ctx, spec, pkg, plan, &plan.steps[i]); err != nil {
			return err
		}
	}

	return nil
}

// executeStep runs one planned step; host CLI failures pass through unchanged.
func (b *Base) executeStep(ctx context.Context, spec looseSpec, pkg Package, plan *loosePlan, step *looseStep) error {
	if err := ctx.Err(); err != nil {
		return stepFailure(spec, pkg, err)
	}

	switch step.kind {
	case stepCopyTree:
		return b.executeCopyTree(ctx, spec, pkg, plan, step)
	case stepWriteFile, stepConfig:
		return b.executeWrite(ctx, spec, pkg, plan, step)
	case stepHostCommand:
		return b.executeHost(ctx, spec, step)
	default:
		return stepFailure(spec, pkg, fmt.Errorf("unknown loose step %d", step.kind))
	}
}

// stepFailure wraps one loose step failure.
func stepFailure(spec looseSpec, pkg Package, err error) error {
	return &DeliveryError{Host: string(spec.host), Package: pkg.ID, Step: stepInstall, Cause: err}
}

// executeCopyTree replaces and copies one skill tree, verifying its digest.
func (b *Base) executeCopyTree(ctx context.Context, spec looseSpec, pkg Package, plan *loosePlan, step *looseStep) error {
	if err := b.trashExisting(ctx, spec, pkg, plan, step); err != nil {
		return err
	}

	if err := writeTree(ctx, step.src, step.path); err != nil {
		return stepFailure(spec, pkg, err)
	}

	sum, err := digest.Tree(step.path)
	if err != nil {
		return stepFailure(spec, pkg, err)
	}

	if sum != step.digest {
		return stepFailure(spec, pkg, fmt.Errorf("copied tree %s digests %s, expected %s", step.path, sum, step.digest))
	}

	return nil
}

// executeWrite replaces and writes one file or config document.
func (b *Base) executeWrite(ctx context.Context, spec looseSpec, pkg Package, plan *loosePlan, step *looseStep) error {
	if err := b.trashExisting(ctx, spec, pkg, plan, step); err != nil {
		return err
	}

	if err := fsutil.EnsureDir(filepath.Dir(step.path), 0o700); err != nil {
		return stepFailure(spec, pkg, err)
	}

	if err := fsutil.WriteFileAtomic(step.path, step.data, step.mode); err != nil {
		return stepFailure(spec, pkg, err)
	}

	return nil
}

// executeHost runs one host CLI command.
func (b *Base) executeHost(ctx context.Context, spec looseSpec, step *looseStep) error {
	_, err := b.run(ctx, spec.binary, step.command)

	return err
}

// trashExisting moves a replaced target (or a replaced config key value) into
// the trash and records the bucket id in the RMA.
func (b *Base) trashExisting(ctx context.Context, spec looseSpec, pkg Package, plan *loosePlan, step *looseStep) error {
	if !step.existed && len(step.backups) == 0 {
		return nil
	}

	if b.trash == nil {
		return &DeliveryError{Host: string(spec.host), Package: pkg.ID, Step: stepInstall, Cause: fmt.Errorf("trash is required to replace %s", step.path)}
	}

	if len(step.backups) > 0 {
		for _, backup := range step.backups {
			id, err := b.trashValue(ctx, spec, pkg, backup.previous)
			if err != nil {
				return err
			}

			plan.ops[backup.opIndex].Backup = id
		}

		return nil
	}

	entry, err := b.trash.Put(ctx, step.path, store.PutOptions{Package: pkg.ID, Host: string(spec.host)})
	if err != nil {
		return &DeliveryError{Host: string(spec.host), Package: pkg.ID, Step: stepInstall, Cause: err}
	}

	plan.ops[step.opIndex].Backup = entry.ID

	return nil
}

// trashValue serializes a replaced config value to a temp file and trashes it.
func (b *Base) trashValue(ctx context.Context, spec looseSpec, pkg Package, value any) (string, error) {
	if b.store == nil {
		return "", &DeliveryError{Host: string(spec.host), Package: pkg.ID, Step: stepInstall, Cause: errors.New("a store is required to back up a config key")}
	}

	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", &DeliveryError{Host: string(spec.host), Package: pkg.ID, Step: stepInstall, Cause: err}
	}

	dir := filepath.Join(b.store.CacheDir(), "host-backups")

	if err := fsutil.EnsureDir(dir, 0o700); err != nil {
		return "", &DeliveryError{Host: string(spec.host), Package: pkg.ID, Step: stepInstall, Cause: err}
	}

	file, err := os.CreateTemp(dir, "config-*.json")
	if err != nil {
		return "", &DeliveryError{Host: string(spec.host), Package: pkg.ID, Step: stepInstall, Cause: err}
	}

	name := file.Name()

	writeErr := func() error {
		if _, err := file.Write(append(data, '\n')); err != nil {
			return err
		}

		if err := file.Sync(); err != nil {
			return err
		}

		return file.Close()
	}()
	if writeErr != nil {
		_ = file.Close()
		_ = os.Remove(name)

		return "", &DeliveryError{Host: string(spec.host), Package: pkg.ID, Step: stepInstall, Cause: writeErr}
	}

	entry, err := b.trash.Put(ctx, name, store.PutOptions{Package: pkg.ID, Host: string(spec.host)})
	if err != nil {
		_ = os.Remove(name)

		return "", &DeliveryError{Host: string(spec.host), Package: pkg.ID, Step: stepInstall, Cause: err}
	}

	return entry.ID, nil
}

// writeTree copies a directory tree: dirs 0700, regular files 0600, symlinks as
// symlinks (targets are never read), special files skipped.
func writeTree(ctx context.Context, from, to string) error {
	return filepath.WalkDir(from, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		rel, err := filepath.Rel(from, current)
		if err != nil {
			return err
		}

		target := filepath.Join(to, rel)

		switch {
		case rel == ".":
			return fsutil.EnsureDir(to, 0o700)
		case entry.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(current)
			if err != nil {
				return err
			}

			return os.Symlink(link, target) //nolint:gosec // G122: the link is recreated verbatim, its target is never followed
		case entry.IsDir():
			return fsutil.EnsureDir(target, 0o700)
		case entry.Type().IsRegular():
			return copyRegularFile(current, target)
		default:
			return nil
		}
	})
}

// copyRegularFile writes one regular file 0600.
func copyRegularFile(from, to string) error {
	data, err := os.ReadFile(from) //nolint:gosec // G304: the source is below the payload root
	if err != nil {
		return err
	}

	if err := fsutil.EnsureDir(filepath.Dir(to), 0o700); err != nil {
		return err
	}

	return fsutil.WriteFileAtomic(to, data, 0o600)
}

// readOptionalFile reads a config file; a missing path is nil, not an error.
func readOptionalFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is a host config below the caller's home
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	return data, nil
}

// configMember decodes one dotted key path of a JSONC or TOML document.
func configMember(file []byte, keyPath string, tomlDoc bool) (any, bool, error) {
	if len(bytes.TrimSpace(file)) == 0 {
		return nil, false, nil
	}

	doc, err := decodeConfigDoc(file, tomlDoc)
	if err != nil {
		return nil, false, err
	}

	current := any(doc)

	for segment := range strings.SplitSeq(keyPath, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false, nil
		}

		value, exists := object[segment]
		if !exists {
			return nil, false, nil
		}

		current = value
	}

	return current, true, nil
}

// decodeConfigDoc decodes one JSONC or TOML config document into a map.
func decodeConfigDoc(file []byte, tomlDoc bool) (map[string]any, error) {
	doc := map[string]any{}

	if tomlDoc {
		if err := toml.Unmarshal(file, &doc); err != nil {
			return nil, err
		}

		return doc, nil
	}

	root, err := hujson.Parse(file)
	if err != nil {
		return nil, err
	}

	standard, err := hujson.Standardize(root.Pack())
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(standard, &doc); err != nil {
		return nil, err
	}

	return doc, nil
}

// configMode keeps an existing regular config file's mode; new files are 0600.
func configMode(path string) fs.FileMode {
	info, err := os.Stat(path)
	if err == nil && info.Mode().IsRegular() {
		return info.Mode().Perm()
	}

	return 0o600
}

// jsoncMember decodes one top-level JSONC member with the same parser the
// editors use.
func jsoncMember(file []byte, key string) (any, bool) {
	if len(bytes.TrimSpace(file)) == 0 {
		return nil, false
	}

	root, err := hujson.Parse(file)
	if err != nil {
		return nil, false
	}

	standard, err := hujson.Standardize(root.Pack())
	if err != nil {
		return nil, false
	}

	doc := map[string]any{}

	if err := json.Unmarshal(standard, &doc); err != nil {
		return nil, false
	}

	value, ok := doc[key]

	return value, ok
}

// jsoncToJSON standardizes a JSONC document into strict JSON for readers that
// need plain JSON; the write path keeps the original bytes so comments
// survive.
func jsoncToJSON(file []byte) ([]byte, error) {
	if len(bytes.TrimSpace(file)) == 0 {
		return nil, nil
	}

	root, err := hujson.Parse(file)
	if err != nil {
		return nil, err
	}

	return hujson.Standardize(root.Pack())
}

// canonicalValueDigest hashes a config value in its canonical JSON form,
// matching render's key-ownership digests.
func canonicalValueDigest(value any) digest.Hash {
	data, err := json.Marshal(value)
	if err != nil {
		return digest.Bytes(nil)
	}

	return digest.Bytes(data)
}

// mustJSON marshals a JSON-safe value for a digest.
func mustJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}

	return data
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Lstat(path)

	return err == nil
}

// validElement reports whether value is safe as one path element.
func validElement(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}

	if strings.ContainsAny(value, `/\`) || strings.ContainsRune(value, 0) {
		return false
	}

	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}

	return true
}

// Hook module payload layout and phases (omp 18.4.1): a package ships host
// module sources below runtime/<host>/hooks/{pre,post}/, and the host discovers
// only those two directories — a module placed directly in hooks/ is ignored
// silently, and .mjs/.cjs are not auto-discovered at all.
const (
	hookModulePhasePre  = "pre"
	hookModulePhasePost = "post"
	hookModuleRuntime   = "runtime"
	// hookModuleExts are the module extensions the host auto-discovers.
	hookModuleExts = ".ts,.js"
)

// HasHookModules reports whether a payload carries host hook modules for any
// host (<root>/runtime/<host>/hooks/{pre,post}/<name>.{ts,js}). The CLI asks it
// before the hooks consent question: such a module is code the host runs on
// every session, exactly like a declarative hook, and a payload that carries
// only modules declares no declarative hook for the question to key on.
func HasHookModules(root string) bool {
	entries, err := os.ReadDir(filepath.Join(root, hookModuleRuntime))
	if err != nil {
		return false
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		modules, err := scanHookModules(root, entry.Name())
		if err == nil && len(modules) > 0 {
			return true
		}
	}

	return false
}

// hookModule is one file of a package's host module payload.
type hookModule struct {
	phase string // pre|post
	name  string // file stem (the host's module identity)
	ext   string // .ts|.js
	path  string // source file below the payload root
}

// scanHookModules lists the host module files a payload carries for one host:
// <root>/runtime/<host>/hooks/{pre,post}/<name>.{ts,js}, sorted by phase and
// name. Another host's modules are not this host's business, and a module
// extension the host never auto-discovers (.mjs, .cjs, anything else) is
// reported rather than delivered silently dead.
func scanHookModules(root, hostID string) ([]hookModule, error) {
	base := filepath.Join(root, hookModuleRuntime, hostID, "hooks")

	var modules []hookModule

	for _, phase := range []string{hookModulePhasePre, hookModulePhasePost} {
		dir := filepath.Join(base, phase)

		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return nil, err
		}

		for _, entry := range entries {
			if entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 {
				return nil, &NotSupportedError{
					Host:      ID(hostID),
					Operation: "a symlinked hook module in " + filepath.ToSlash(filepath.Join(hookModuleRuntime, hostID, "hooks", phase, entry.Name())),
				}
			}

			ext := filepath.Ext(entry.Name())
			if !slices.Contains(strings.Split(hookModuleExts, ","), ext) {
				return nil, &NotSupportedError{
					Host:      ID(hostID),
					Operation: "a hook module " + entry.Name() + " (the host auto-discovers .ts and .js only)",
				}
			}

			modules = append(modules, hookModule{
				phase: phase, name: strings.TrimSuffix(entry.Name(), ext), ext: ext,
				path: filepath.Join(dir, entry.Name()),
			})
		}
	}

	return modules, nil
}

// hookRecordOwnership collects the digest this package's previous receipt
// recorded for each hook record verger owns, keyed the way the renderer keys
// it. A record with a recorded digest is verger's; without one it is the user's
// and is never rewritten. The whole-document digest the config edit carries
// stays the coarse backstop for a host that declares no per-record identity.
//
// hooks must be the RENDERED list — the same slice planHooks hands the
// renderer. The receipt keys its record artifacts by the rendered command
// (${PLUGIN_ROOT} already substituted), so keying off the package's raw hooks
// asks for a path the receipt never wrote, finds nothing, and silently
// disables the per-record guard for every host that rewrites variables.
func (p *loosePlanner) hookRecordOwnership(hooks []manifest.Hook) render.Owned {
	if p.spec.hookRecordPath == nil {
		return nil
	}

	owned := render.Owned{}

	for _, hook := range hooks {
		event, ok := render.HookEventName(p.spec.hooksDialect(), hook.Event)
		if !ok {
			continue
		}

		if sum := p.recordedDigest(p.spec.hookRecordPath(event, hook.Command)); sum != "" {
			owned["hooks/"+event+"/"+hook.Command] = sum
		}
	}

	if len(owned) == 0 {
		return nil
	}

	return owned
}

// refuseSecretIntoGit refuses a delivery that would write a resolved secret
// value into a file the project's git repository already tracks. The refusal
// names the file and the reason, because "it failed" leaves the user with no
// way to know what to do about it.
//
// The check is deliberately narrow: only project scope, only a path inside
// the project, and only a file git already tracks. A file the repository does
// not track yet is the user's to commit or not, and .verger/ stays out of
// version control by the store's own exclude, so this is the last place a
// leak can be caught before it is written.
func (p *loosePlanner) refuseSecretIntoGit(path string) error {
	if p.spec.project == "" || path == "" {
		return nil
	}

	// Two questions have to be answered before anything is refused: is the
	// file inside the project, and does git already track it. Both have a
	// "cannot tell" answer, and both are answered by not refusing: a path
	// this check cannot place is a path it has no claim about. Written as one
	// condition rather than as two early returns, so failing open is a
	// decision the code states instead of an accident it performs.
	rel, relErr := filepath.Rel(p.spec.project, path)
	inside := relErr == nil && !strings.HasPrefix(rel, "..")

	if !inside || !gitTracks(p.spec.project, rel) {
		return nil
	}

	return fmt.Errorf("refusing to write a resolved secret into %s: git tracks it, so the value would be committed: ignore or untrack the file, or resolve the secret from the environment", path)
}

// gitTracks reports whether the repository at root tracks rel.
//
// It has no error return, and that is the honest shape: a directory that is
// not a repository, a git that is not installed, and a file git does not track
// are all the same answer — nothing to leak into that this check can see — and
// none of them is a failure of the caller. An error return that is only ever
// nil would say "this can fail" and then never say it.
func gitTracks(root, rel string) bool {
	isRepo := false
	if _, statErr := os.Stat(filepath.Join(root, ".git")); statErr == nil {
		isRepo = true
	}

	if !isRepo {
		return false
	}

	// The binary is the literal "git" and the one variable argument is
	// preceded by "--", so a rel that starts with a dash reaches git as a
	// path rather than as an option.
	cmd := exec.CommandContext(context.Background(), "git", "ls-files", "--error-unmatch", "--", rel) //nolint:gosec // G204: the binary is a literal and `--` ends option parsing, so no argument is an option
	cmd.Dir = root

	// `ls-files --error-unmatch` exits non-zero for "not tracked", which is
	// this function's other answer.
	out, err := cmd.Output()
	if err != nil {
		return false
	}

	return strings.TrimSpace(string(out)) != ""
}

// refuseOutsideProject refuses a project-scope spec whose write targets are not
// all inside the project. hostpath.ProjectSurfaces is the table that answers
// "where does this host keep X in a project", and an entry that is empty means
// the host has no project form for it — not that verger should fall back to the
// user scope and quietly write there.
func (s looseSpec) refuseOutsideProject() error {
	if s.project == "" {
		return nil
	}

	targets := map[string]string{
		"skills": s.skillsDir, "agents": s.agentsDir, "commands": s.commandsDir,
		"rules": s.rulesDir, "hooks": s.hooksPath, "settings": s.settingsPath,
	}

	if s.mcpConfig != nil {
		targets["mcp"] = s.mcpConfig.path
	}

	for name, path := range targets {
		if path == "" {
			continue
		}

		rel, err := filepath.Rel(s.project, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return &NotSupportedError{
				Host:      s.host,
				Operation: "project scope delivery of " + name + " (the host has no project surface for it)",
			}
		}
	}

	return nil
}
