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
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/tailscale/hujson"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/hostcli"
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
	hooksBlocked   string                                     // non-empty: the host has no declarative hook surface, hooks are skipped with this reason
	hookModulesDir string                                     // host directory of pre/post hook modules (<dir>/pre/<name>.ts); the payload's runtime hook modules are copied verbatim
	mcpConfig      *mcpConfigSpec                             // config-document MCP surface (Codex, Gemini)
	variables      map[string]string                          // host-specific braced variables (Gemini extensionPath)
}

// mcpConfigSpec describes an MCP surface written into a shared config document
// instead of the host CLI.
type mcpConfigSpec struct {
	path   string
	format manifest.Format
	toml   bool // TOML (Codex config.toml) vs JSONC (Gemini settings.json)
	edit   func([]byte, []render.Edit, render.Owned) ([]byte, []render.Change, error)
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

// addConfig records one shared config-document write backing several key
// changes: a single step, one artifact and one RMA op per changed key, with
// the previous value of every replaced key kept for the trash backup.
func (p *loosePlan) addConfig(step looseStep, artifacts []receipt.Artifact, ops []receipt.Op, previous []any) {
	base := len(p.ops)

	for i, op := range ops {
		p.ops = append(p.ops, op)
		p.artifacts = append(p.artifacts, artifacts[i])

		if op.Existed {
			var value any
			if i < len(previous) {
				value = previous[i]
			}

			step.backups = append(step.backups, looseBackup{previous: value, opIndex: base + i})
		}
	}

	p.steps = append(p.steps, step)
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

	planner := &loosePlanner{
		base: base,
		spec: spec,
		d:    d,
		pkg:  d.Package,
		plan: &loosePlan{},
	}

	planner.dataDir = dataDirPath(base, d.Package, spec.host)

	if d.Package.Scope != "" && d.Package.Scope != receipt.ScopeUser {
		return nil, &NotSupportedError{Host: spec.host, Operation: "project scope delivery in Ф1"}
	}

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

	plan, err := render.PlanHooks(p.spec.hooksDialect(), planning, rewritten)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	p.plan.notes = append(p.plan.notes, plan.Warnings...)

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

	if current, ok := jsoncMember(existing, "hooks"); ok {
		ownedDigest = canonicalValueDigest(current)
	}

	p.queueConfigEdit(file, false, render.EditJSONC, pendingEdit{
		edit:  render.Edit{Path: "hooks", Value: value},
		kind:  "hook",
		name:  "hooks",
		owned: ownedDigest,
	})

	return nil
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
	edits   []pendingEdit
	owned   render.Owned
	secret  bool
}

// pendingEdit is one key edit plus the artifact identity it records.
type pendingEdit struct {
	edit  render.Edit
	kind  string
	name  string
	owned digest.Hash // current value digest the edit adopts as owned; empty leaves the key unowned (an existing value is hands-off)
}

// queueConfigEdit adds one key edit to the pending document of a file.
func (p *loosePlanner) queueConfigEdit(
	file string, tomlDoc bool,
	edit func([]byte, []render.Edit, render.Owned) ([]byte, []render.Change, error),
	pe pendingEdit,
) {
	cfg := p.configFor(file, tomlDoc, edit)

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

// flushConfig applies the accumulated edits of one document.
func (p *loosePlanner) flushConfig(cfg *pendingConfig) error {
	if len(cfg.edits) == 0 {
		return nil
	}

	existing, err := readOptionalFile(cfg.file)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	if info, statErr := os.Stat(cfg.file); statErr == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o200 == 0 {
		return p.deliveryError(stepPlan, fmt.Errorf("%s is read-only", cfg.file))
	}

	edits := make([]render.Edit, 0, len(cfg.edits))

	for _, pe := range cfg.edits {
		edits = append(edits, pe.edit)
	}

	out, changes, err := cfg.edit(existing, edits, cfg.owned)
	if err != nil {
		return err
	}

	if len(changes) == 0 {
		return nil
	}

	mode := configMode(cfg.file)
	if cfg.secret {
		mode = 0o600

		p.note("%s carries a resolved secret; mode is 0600", cfg.file)
	}

	artifacts := make([]receipt.Artifact, 0, len(changes))
	ops := make([]receipt.Op, 0, len(changes))
	replaced := make([]any, 0, len(changes))

	for _, change := range changes {
		pe := cfg.editFor(change.Path)

		artifacts = append(artifacts, receipt.Artifact{
			Kind: pe.kind, Name: pe.name, Path: cfg.file, Digest: change.Digest,
		})
		ops = append(ops, receipt.Op{
			Kind: receipt.OpConfigKey, Path: cfg.file, KeyPath: change.Path, Digest: change.Digest, Existed: change.Existed,
		})
		replaced = append(replaced, change.Previous)
	}

	p.plan.addConfig(
		looseStep{kind: stepConfig, path: cfg.file, data: out, mode: mode, digest: changes[0].Digest},
		artifacts, ops, replaced,
	)

	return nil
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

	if len(servers) == 0 {
		return nil
	}

	edits, err := render.MCPEdits(cfg.format, servers)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	existing, err := readOptionalFile(cfg.path)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	pending, err := mcpPendingEdits(existing, edits, cfg.toml)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	prefix := mcpConfigPrefix(cfg.format)

	for _, edit := range pending {
		p.queueConfigEdit(cfg.path, cfg.toml, cfg.edit, pendingEdit{
			edit: edit,
			kind: artifactMCP,
			name: strings.TrimPrefix(edit.Path, prefix),
		})
	}

	if secret && len(pending) > 0 {
		p.configFor(cfg.path, cfg.toml, cfg.edit).secret = true
	}

	return nil
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
func mcpPendingEdits(existing []byte, edits []render.Edit, tomlDoc bool) ([]render.Edit, error) {
	pending := make([]render.Edit, 0, len(edits))

	for _, edit := range edits {
		current, exists, err := configMember(existing, edit.Path, tomlDoc)
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

// mcpConfigPrefix returns the config key prefix of one MCP dialect.
func mcpConfigPrefix(format manifest.Format) string {
	switch format {
	case manifest.FormatCodex:
		return "mcp_servers."
	case manifest.FormatClaude, manifest.FormatGemini:
		return "mcpServers."
	default:
		return ""
	}
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
