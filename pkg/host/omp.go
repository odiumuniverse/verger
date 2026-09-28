package host

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gofrs/flock"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
)

// omp (oh-my-pi) surface names, verified live against omp/18.4.1
// (docs/reviews/omp-grammar.probe.log, fixtures in testdata/omp/).
const (
	wordOmp = "omp"
	// flagForce is mandatory for a re-delivery: `omp plugin install` of an
	// installed plugin exits 1 ("is already installed. Use force option to
	// reinstall") where Claude and Codex move the plugin to the
	// marketplace's version.
	flagForce = "--force"

	ompDirName       = ".omp"       // base config root below HOME
	ompInstallID     = "install-id" // the config-independent install marker
	ompAgentName     = "agent"      // default agent dir below the base root
	ompSharedDir     = ".agents"    // the shared home omp's `agents` provider reads, ungated
	ompAgentsDir     = "agents"     // subagents, below the agent dir
	ompCommandsDir   = "commands"   // slash commands, below the agent dir
	ompRulesDir      = "rules"      // rulebook documents, below the agent dir
	ompMCPDoc        = "mcp.json"   // MCP servers, below the agent dir
	ompSkillsDirName = "skills"     // the host's own skills dir, which outranks the shared root
	// ompPluginLock is the process-exclusive advisory lock serializing every
	// omp plugin/marketplace mutation: omp's manager is not transactional and
	// has no cross-process locking, so two writers overwrite each other
	// (docs/reviews/omp-grammar.probe.log; OMPDOC
	// plugin-manager-installer-plumbing.md#lock-state-management-details).
	ompPluginLock = ".omp-plugin.verger.lock"
	// ompHooksBlocked is the delivery note of the declarative hook component:
	// omp has no hook document at all, only TS/JS modules under
	// hooks/{pre,post}/, which the payload carries as host modules instead
	// (hookModulesDir).
	ompHooksBlocked = "omp hooks are TS/JS modules under hooks/{pre,post}/ and have no declarative surface"
	ompHooksDir     = "hooks" // hook modules, below the agent dir: hooks/{pre,post}/<name>.{ts,js}
	// ompDefaultProfile is the profile name the host treats as "no profile".
	ompDefaultProfile = "default"
	// ompHookProbePrompt and ompHookProbeTime bound the only load check omp
	// offers: a headless session start. Hook modules are compiled and imported
	// before omp needs a model, so the probe works without credentials, and
	// --max-time keeps a session that has no model from waiting for input.
	ompHookProbePrompt = "omp"
	ompHookProbeTime   = "3"
	// ompHookLoadFailure is the stderr marker of a module that did not load.
	// The exit code proves nothing: a broken module still ends the session
	// normally (the hooks contract, E5/E6).
	ompHookLoadFailure = "Failed to load extension"
)

// omp is the oh-my-pi adapter. Its install grammar is the one of omp 18.4.1,
// verified live (docs/reviews/omp-grammar.probe.log): `plugin marketplace
// add|list|remove`, `plugin install|uninstall <name>@<marketplace>`, `--force`
// required to reinstall, and `--json` honoured on `plugin list` alone.
//
// Two host facts shape the adapter:
//   - omp reads a shared home (`~/.agents`) through its `agents` discovery
//     provider, which is not gated, so skills go there and stay at exactly one
//     copy for Codex, Pi and omp (DESIGN §4.4);
//   - the active agent dir is relocatable: PI_CODING_AGENT_DIR, or a named
//     profile (OMP_PROFILE/PI_PROFILE → <base>/profiles/<name>/agent), which
//     wins over PI_CODING_AGENT_DIR; PI_CONFIG_DIR replaces the `.omp` base
//     root with a home-relative directory. Verger delivers to the agent dir of
//     the active profile only; a multi-profile host stays one adapter instance.
type omp struct {
	base *Base
}

// NewOmp builds the omp adapter; the adapter is immutable after construction
// and safe for concurrent use.
func NewOmp(opts ...Option) Host {
	return &omp{base: newBase(opts)}
}

// ID implements Host.
func (h *omp) ID() ID {
	return Omp
}

// Oracle implements Host.
func (h *omp) Oracle() Oracle {
	return &ompOracle{base: h.base}
}

// Detect reports whether omp is present: the active agent dir, the
// config-independent install marker, or the `omp` binary. Both env relocations
// (PI_CODING_AGENT_DIR, the profile vars) are honoured through ompAgentDir, so
// no CLI call is needed here.
func (h *omp) Detect(home string) bool {
	userHome := h.base.effectiveHome(home)

	if isDir(ompAgentDir(userHome)) {
		return true
	}

	if isFile(filepath.Join(ompBase(userHome), ompInstallID)) {
		return true
	}

	_, err := h.base.resolve(wordOmp)

	return err == nil
}

// Deliver implements Host with explicit strategies; the ladder is pkg/plan.
// The shared dispatch checks the source ref before any stratum, so a
// host-forbidden source is never delivered — not even as files.
func (h *omp) Deliver(ctx context.Context, home string, d Delivery) (Result, error) {
	return deliverByStrategy(ctx, h.base, Omp, home, d, h.deliverInstall, h.deliverLoose)
}

// ompSpec is the loose surface of the omp adapter: skills in the shared
// ~/.agents/skills root, subagents, commands, rulebook documents and the MCP
// document under the active agent dir. Hooks have no surface here
// (ompHooksBlocked).
func ompSpec(userHome string) looseSpec {
	agentDir := ompAgentDir(userHome)

	return looseSpec{
		host:         Omp,
		binary:       wordOmp,
		home:         userHome,
		skillsDir:    filepath.Join(userHome, ompSharedDir, "skills"),
		agentsDir:    filepath.Join(agentDir, ompAgentsDir),
		commandsDir:  filepath.Join(agentDir, ompCommandsDir),
		rulesDir:     filepath.Join(agentDir, ompRulesDir),
		settingsPath: filepath.Join(agentDir, ompMCPDoc),
		hooksBlocked: ompHooksBlocked,
		// The declarative hook document has no omp surface (hooksBlocked), but
		// the payload's host modules do: runtime/omp/hooks/{pre,post}/<name>.ts.
		hookModulesDir: filepath.Join(agentDir, ompHooksDir),
		renderAgent:    ompAgent,
		mcpConfig: &mcpConfigSpec{
			path:   filepath.Join(agentDir, ompMCPDoc),
			format: manifest.FormatClaude,
			edit:   render.EditJSONC,
		},
	}
}

// deliverLoose plans and executes the host-native user files.
func (h *omp) deliverLoose(ctx context.Context, home string, d Delivery) (Result, error) {
	userHome := h.base.effectiveHome(home)
	spec := ompSpec(userHome)

	plan, err := planLoose(ctx, h.base, spec, d)
	if err != nil {
		return Result{}, err
	}

	if d.DryRun {
		return plan.result(d.Strategy, true), nil
	}

	// The result is the executed plan even on error (NF-5, Host.Deliver); it
	// is composed after execution, which records the trash buckets.
	err = h.base.executeLoose(ctx, spec, d.Package, plan)
	result := plan.result(d.Strategy, false)

	// The shared root is not the only place omp looks for a skill: its own agent
	// dir wins on a name clash, so a user's copy shadows the delivered one.
	if slices.ContainsFunc(d.Package.Components, func(component manifest.Component) bool {
		return component.Kind == manifest.KindSkill
	}) {
		result.Notes = append(result.Notes, "omp reads skills from "+filepath.Join(ompSharedDir, "skills")+
			" and from its own agent dir ("+filepath.Join(ompAgentDir(userHome), ompSkillsDirName)+
			"), which wins on a name clash (its native provider outranks the shared one): a same-named skill there shadows the delivered one")
	}

	if err == nil && d.AllowHooks {
		if verifyErr := h.verifyHookModules(ctx, userHome, d.Package); verifyErr != nil {
			return result, verifyErr
		}
	}

	return result, err
}

// verifyHookModules proves the delivered hook modules load, because no omp
// command reports it: the only signal is a `Failed to load extension <path>`
// line on stderr of a session start, and the exit code stays 0 even for a
// module that failed to build. The probe runs headless with a bounded session
// and no model: modules are imported before omp asks for credentials.
func (h *omp) verifyHookModules(ctx context.Context, userHome string, pkg Package) error {
	modules, err := scanHookModules(pkg.Root, string(Omp))
	if err != nil || len(modules) == 0 {
		return err
	}

	stdout, stderr, runErr := h.base.runStreams(ctx, wordOmp, []string{"-p", ompHookProbePrompt, "--max-time", ompHookProbeTime})
	if errors.Is(runErr, hostcli.ErrNotFound) {
		return nil // no CLI to ask; the delivery stands on its own
	}

	// Both streams are read: the marker is a diagnostic line, not a document,
	// and which stream carries it is a host implementation detail.
	output := string(stdout) + "\n" + string(stderr)

	var failed []string

	for line := range strings.SplitSeq(output, "\n") {
		if !strings.Contains(line, ompHookLoadFailure) {
			continue
		}

		for _, module := range modules {
			// omp abbreviates HOME to "~" in the message, so the file is
			// matched by its module path, never by an absolute prefix.
			if strings.Contains(line, filepath.Join(ompHooksDir, module.phase, module.name+module.ext)) {
				failed = append(failed, strings.TrimSpace(line))
			}
		}
	}

	if len(failed) > 0 {
		return &DeliveryError{
			Host: string(Omp), Package: pkg.ID, Step: stepVerify,
			Cause: fmt.Errorf("the delivered hook module(s) did not load: %s", strings.Join(failed, "; ")),
		}
	}

	agentDir := ompAgentDir(userHome)

	for _, module := range modules {
		if !isFile(filepath.Join(agentDir, ompHooksDir, module.phase, module.name+module.ext)) {
			return &DeliveryError{
				Host: string(Omp), Package: pkg.ID, Step: stepVerify,
				Cause: fmt.Errorf("the hook module %s is not on disk after delivery", module.name+module.ext),
			}
		}
	}

	return nil
}

// ompAgent renders one subagent for omp, which silently skips a file whose
// frontmatter carries no name or no description — the two fields the canonical
// renderer may leave empty.
func ompAgent(agent render.Agent, fallback string) ([]byte, error) {
	if strings.TrimSpace(agent.Name) == "" {
		agent.Name = fallback
	}

	if strings.TrimSpace(agent.Description) == "" {
		agent.Description = firstBodyLine(agent.Body, agent.Name)
	}

	return agent.ClaudeMarkdown(), nil
}

// firstBodyLine is the description fallback: omp needs one, and the agent's own
// prompt opens with the closest thing to it. It falls back to the name so the
// value is never empty.
func firstBodyLine(body, fallback string) string {
	for line := range strings.SplitSeq(body, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}

	return fallback
}

// ompInstall is one native or synth plugin install from a marketplace.
type ompInstall struct {
	addRef      string       // native: the source ref; synth: the owner marketplace root
	plugin      string       // the plugin name
	marketplace string       // the registered marketplace name (corrected after the add on the native path)
	version     string       // the delivered version (stale-synth-version check)
	synth       *synthLayout // synth: the owner marketplace
	// existed is true when the marketplace was registered before this delivery:
	// its inverse then keeps it, because a delivery may remove only what it
	// registered itself (Host.Deliver, Op.Existed).
	existed bool
}

// installID is the plugin selector `<plugin>@<marketplace>`.
func (p ompInstall) installID() string {
	return p.plugin + "@" + p.marketplace
}

// marketplacePath is the receipt identity of one registered marketplace; an
// artifact at this path proves a later delivery that the name is verger's (and
// lets a re-delivery resolve the name the host derived from the catalog).
func ompMarketplacePath(name string) string {
	return "omp://marketplace/" + name
}

// rma removes the plugin, then — refcounted at removal — its marketplace.
func (p ompInstall) rma() []receipt.Op {
	return []receipt.Op{
		{Kind: receipt.OpHostInstall, Command: []string{wordPlugin, wordMarketplace, wordRemove, p.marketplace}, Existed: p.existed},
		{Kind: receipt.OpHostInstall, Command: []string{wordPlugin, wordUninstall, p.installID()}},
	}
}

// deliverInstall registers the marketplace, installs the plugin from it and
// verifies it through the oracle; a verify failure is a *DeliveryError, never a
// silent step down. The command-source policy is checked before anything is
// planned.
func (h *omp) deliverInstall(ctx context.Context, home string, d Delivery, synth bool) (Result, error) {
	if err := checkCommandSources(Omp, h.base.effectiveHome(home), d.Package.Marketplace); err != nil {
		return Result{}, err
	}

	plan, err := h.installPlan(ctx, d, synth)
	if err != nil {
		return Result{}, err
	}

	if d.DryRun {
		// A dry run registers nothing, so its inverse may not claim the right to
		// remove a marketplace; the real run corrects this as it learns whether
		// it registered one.
		plan.existed = true

		return Result{Strategy: d.Strategy, RMA: plan.rma(), Notes: []string{noteDryRun}}, nil
	}

	observed, artifacts, err := h.install(ctx, h.base.effectiveHome(home), d.Package, &plan)

	// The result is what execution produced even on error (NF-5):
	// `install` corrects the planned marketplace name before the RMA is read.
	return Result{Strategy: d.Strategy, Artifacts: artifacts, RMA: plan.rma(), Observed: observed}, err
}

// install writes the owner marketplace document (synth), registers the
// marketplace, installs the plugin and verifies it, all under the
// process-exclusive plugin lock. `plugin install` is not idempotent without
// --force and `plugin marketplace add` is not idempotent at all, so a
// re-delivery resolves the registered marketplace instead of adding it twice.
func (h *omp) install(ctx context.Context, userHome string, pkg Package, plan *ompInstall) (OracleResult, []receipt.Artifact, error) {
	var (
		observed OracleResult
		pinned   string
	)

	err := h.withPluginLock(ctx, userHome, func() error {
		if plan.synth != nil {
			if _, err := plan.synth.writeDoc(ctx, plan.marketplace); err != nil {
				return &DeliveryError{Host: string(Omp), Package: pkg.ID, Step: stepInstall, Cause: err}
			}
		}

		if !plan.existed {
			if err := h.resolveAndRegister(ctx, pkg, plan); err != nil {
				return err
			}
		}

		pinned = plan.marketplace

		var verifyErr error

		observed, verifyErr = h.verify(ctx, pkg, plan)

		return verifyErr
	})
	if err != nil {
		return observed, nil, err
	}

	// The marketplace artifact proves the name on the next delivery and gives
	// the uninstall an owner; its digest is the ref this delivery registered.
	artifact := receipt.Artifact{
		Kind: "marketplace", Name: pinned,
		Path: ompMarketplacePath(pinned), Digest: digest.Bytes([]byte(plan.addRef)),
	}

	return observed, []receipt.Artifact{artifact}, nil
}

// register runs the non-idempotent `plugin marketplace add` and names what it
// registered, reporting whether this run registered it. A refusal saying the
// marketplace already exists is tolerated (the manager is not idempotent); any
// other refusal surfaces. When the refusal left no new entry and no receipt
// proves the marketplace, the delivery fails instead of adopting a name it
// would then take the liberty of removing.
func (h *omp) register(ctx context.Context, before []registeredMarketplace, pkg Package, plan *ompInstall) (string, bool, error) {
	_, addErr := h.base.run(ctx, wordOmp, []string{wordPlugin, wordMarketplace, wordAdd, plan.addRef})
	registeredNow := addErr == nil

	if addErr != nil && !ompMarketplaceExists(addErr) {
		return "", false, addErr
	}

	after, err := h.marketplaces(ctx)
	if err != nil {
		return "", false, err
	}

	name, found := h.resolveMarketplace(after, before, pkg, plan, registeredNow)
	if !found {
		return "", false, &DeliveryError{
			Host: string(Omp), Package: pkg.ID, Step: stepPlan,
			Cause: fmt.Errorf("this delivery did not register a marketplace and %s is not proven verger's (%s); remove or adopt it by hand",
				plan.marketplace, strings.Join(marketplaceNames(after), ", ")),
		}
	}

	return name, registeredNow, nil
}

// verify installs the plugin from the resolved marketplace (--force: omp
// refuses a reinstall without it) and asks the oracle whether the host now
// lists it; a miss is a *DeliveryError at the verify step, never a silent step
// down.
func (h *omp) verify(ctx context.Context, pkg Package, plan *ompInstall) (OracleResult, error) {
	if _, err := h.base.run(ctx, wordOmp, []string{wordPlugin, wordInstallCLI, plan.installID(), flagForce}); err != nil {
		return OracleResult{}, err
	}

	listed, err := h.Oracle().List(ctx)
	if err != nil {
		return OracleResult{}, err
	}

	entry, matched := installedFromMarketplace(listed, plan.plugin, plan.marketplace)

	if matched && staleSynthVersion(plan.synth != nil, entry, pkg.Version) {
		matched = false
	}

	if !matched {
		return OracleResult{}, &DeliveryError{
			Host:    string(Omp),
			Package: pkg.ID,
			Step:    stepVerify,
			Cause:   fmt.Errorf("%s %s is not listed by the oracle%s", plan.installID(), pkg.Version, registryNote(listed, plan.plugin)),
		}
	}

	return OracleResult{Listed: listed, Verified: true}, nil
}

// resolveAndRegister gives the plan a marketplace of this delivery's own: a
// receipt-proven one (a re-delivery), or one this run registers. Anything else
// fails, because the name may belong to the user: adopting it would hand this
// delivery the right to remove a marketplace it never created. On failure the
// plan is marked as not registered, so the recorded inverse keeps whatever
// holds the name.
func (h *omp) resolveAndRegister(ctx context.Context, pkg Package, plan *ompInstall) error {
	registered, err := h.marketplaces(ctx)
	if err != nil {
		return err
	}

	if name, owned := h.ownedMarketplace(pkg, registered); owned {
		plan.marketplace, plan.existed = name, true

		return nil
	}

	name, registeredNow, err := h.register(ctx, registered, pkg, plan)
	if err != nil {
		// Nothing was registered by this run, so the inverse must not remove
		// whatever holds the name.
		plan.existed = true

		return err
	}

	plan.marketplace, plan.existed = name, !registeredNow

	return nil
}

// ownedMarketplace resolves a marketplace this package's receipts prove it
// registered, which is the only marketplace a delivery may reuse without
// asking the host — and the only one its inverse may remove.
func (h *omp) ownedMarketplace(pkg Package, registered []registeredMarketplace) (string, bool) {
	for _, entry := range registered {
		if h.ownsMarketplace(pkg, entry.Name) {
			return entry.Name, true
		}
	}

	return "", false
}

// ownsMarketplace reports whether this package's receipts recorded the
// marketplace name.
func (h *omp) ownsMarketplace(pkg Package, name string) bool {
	if h.base.ownership == nil {
		return false
	}

	owner, owned := h.base.ownership.Owner(ompMarketplacePath(name))

	return owned && owner == pkg.ID
}

// resolveMarketplace names the marketplace an `add` registered: the planned
// name when it is now registered (the catalog declares it, and for synth the
// document is verger's own), else the single entry that appeared. Anything
// else is ambiguous and fails rather than guessing a name.
func (h *omp) resolveMarketplace(after, before []registeredMarketplace, pkg Package, plan *ompInstall, registeredNow bool) (string, bool) {
	known := map[string]bool{}

	for _, entry := range before {
		known[entry.Name] = true
	}

	// The entry this run's own add created is ours by construction.
	for _, entry := range after {
		if !known[entry.Name] {
			return entry.Name, true
		}
	}

	// A receipt proves an earlier delivery of this package registered it.
	if name, owned := h.ownedMarketplace(pkg, after); owned {
		return name, true
	}

	// The planned name counts only when this run's add succeeded: the catalog
	// then declares it, and on the synth path the document is verger's own.
	// After a refusal that left no new entry the name belongs to someone else.
	if registeredNow {
		for _, entry := range after {
			if entry.Name == plan.marketplace {
				return entry.Name, true
			}
		}
	}

	return "", false
}

// registryNote names a package the host installed from the npm registry under
// the plugin's name instead of from the marketplace: a selector that is also a
// dist-tag resolves there, and the operator has to remove it by hand, because
// the recorded inverse names the marketplace selector, not the registry id.
// A registry entry is the one shape the oracle reports without a marketplace.
func registryNote(listed []Installed, plugin string) string {
	for _, entry := range listed {
		if entry.Name == plugin && entry.Marketplace == "" {
			return fmt.Sprintf("; omp installed %s@%s from the npm registry instead — remove it by hand, a dist-tag selector resolves there",
				plugin, entry.Version)
		}
	}

	return ""
}

// installedFromMarketplace finds the plugin the host lists as installed from
// exactly one marketplace. Unlike the shared installedAs it never accepts an
// entry that reports no marketplace: `omp plugin install <name>@<tag>` falls
// back to the npm registry whenever the selector also names a dist-tag
// (live-observed: a registered marketplace `beta` loses to the public package
// `one@2.0.0-beta.137.1`), and such an install must fail the verify loudly
// instead of passing as a marketplace delivery.
func installedFromMarketplace(listed []Installed, plugin, marketplace string) (Installed, bool) {
	if marketplace == "" {
		return Installed{}, false
	}

	for _, entry := range listed {
		if entry.Name == plugin && entry.Marketplace == marketplace {
			return entry, true
		}
	}

	return Installed{}, false
}

// marketplaceNames lists the registered names for one error message.
func marketplaceNames(registered []registeredMarketplace) []string {
	names := make([]string, 0, len(registered))

	for _, entry := range registered {
		names = append(names, entry.Name)
	}

	if len(names) == 0 {
		return []string{"none"}
	}

	return names
}

// installPlan resolves the marketplace ref, the plugin and the marketplace of
// one delivery. The native marketplace name is only the planned one — omp
// ignores --json on `plugin marketplace add`, so the registered name is read
// back from `plugin marketplace list` before the RMA is composed.
func (h *omp) installPlan(ctx context.Context, d Delivery, synth bool) (ompInstall, error) {
	if synth {
		layout, name, registered, err := ownerMarketplacePlan(ctx, Omp, d.Package, h.marketplaces)
		if err != nil {
			return ompInstall{}, err
		}

		return ompInstall{
			addRef: layout.root, plugin: layout.identity.Name, marketplace: name,
			version: d.Package.Version, synth: &layout, existed: registered,
		}, nil
	}

	_, name := splitID(d.Package.ID)
	if name == "" {
		return ompInstall{}, &NotSupportedError{Host: Omp, Operation: unsupportedNoName}
	}

	if d.Package.Marketplace == "" {
		return ompInstall{}, &NotSupportedError{Host: Omp, Operation: unsupportedNoMarketplace}
	}

	return ompInstall{
		addRef: d.Package.Marketplace, plugin: name,
		marketplace: marketplaceName(d.Package.Marketplace), version: d.Package.Version,
	}, nil
}

// marketplaces lists what `omp plugin marketplace list` reports. omp 18.4.1
// ignores `--json` on this verb (it answers `plugin list` and `plugin doctor`
// only), so the two-column table is parsed.
func (h *omp) marketplaces(ctx context.Context) ([]registeredMarketplace, error) {
	out, err := h.base.run(ctx, wordOmp, []string{wordPlugin, wordMarketplace, wordList})
	if err != nil {
		return nil, err
	}

	return parseOmpMarketplaces(out), nil
}

// parseOmpMarketplaces reads the `omp plugin marketplace list` table
// (live-verified):
//
//	Configured Marketplaces:
//
//	  acme  /tmp/probe/mkt
//
// and the empty form ("No marketplaces configured" plus a hint line). A row
// counts only when it is indented and its last field is a path, so a rewording
// of the header or the hint cannot invent a marketplace.
func parseOmpMarketplaces(out []byte) []registeredMarketplace {
	var listed []registeredMarketplace

	for line := range strings.SplitSeq(string(out), "\n") {
		if !strings.HasPrefix(line, " ") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		path := fields[len(fields)-1]
		if !strings.ContainsAny(path, `/\`) {
			continue
		}

		listed = append(listed, registeredMarketplace{Name: fields[0], Path: path})
	}

	return listed
}

// withPluginLock runs one omp plugin/marketplace mutation under a
// process-exclusive advisory lock on the base root: omp's manager has no
// cross-process locking of its own, so two verger runs (hosts are delivered in
// parallel, pkg/apply) must not interleave their plugin writes.
func (h *omp) withPluginLock(ctx context.Context, userHome string, run func() error) error {
	dir := ompStateRoot(userHome)

	if err := fsutil.EnsureDir(dir, 0o700); err != nil {
		return &DeliveryError{Host: string(Omp), Step: stepInstall, Cause: err}
	}

	fileLock := flock.New(filepath.Join(dir, ompPluginLock))

	if _, err := fileLock.TryLockContext(ctx, lockRetryDelay); err != nil {
		return &DeliveryError{Host: string(Omp), Step: stepInstall, Cause: fmt.Errorf("lock %s: %w", fileLock.Path(), err)}
	}

	defer func() { _ = fileLock.Unlock() }()

	return run()
}

// ompMarketplaceExists reports whether an add refusal means the marketplace is
// already registered (the manager is not idempotent); any other refusal — an
// unreadable catalog, a name conflict with a different source — must surface.
func ompMarketplaceExists(err error) bool {
	exit, ok := errors.AsType[*hostcli.ExitError](err)
	if !ok {
		return false
	}

	text := strings.ToLower(exit.Stderr)

	return strings.Contains(text, "already exists") || strings.Contains(text, "conflicts with existing marketplace")
}

// Uninstall runs the host-install RMA ops in reverse order; file/tree ops are
// executed by pkg/apply. Both omp verbs refuse an unknown target (unlike the
// Claude and Codex ones), so an already-removed plugin or marketplace is a
// note, not an error; a marketplace is removed only once no installed plugin
// comes from it (§4.8 refcount). An op marked Existed names a resource that
// predates this delivery and is kept (Host.Deliver).
func (h *omp) Uninstall(ctx context.Context, home string, r receipt.Receipt) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	result := Result{Strategy: Strategy(r.Strategy)}

	var notes []string

	run := func() error {
		var err error

		notes, err = hostInverses(ctx, r, h.uninstallOp)

		return err
	}

	// Every op predating this delivery runs nothing, so no lock is needed and
	// no host directory is created for a no-op.
	if len(runnableHostInstalls(r)) == 0 {
		if err := run(); err != nil {
			return Result{}, err
		}
	} else if err := h.withPluginLock(ctx, h.base.effectiveHome(home), run); err != nil {
		return Result{}, err
	}

	result.Notes = notes

	return result, nil
}

// runnableHostInstalls lists the host-install ops an uninstall will actually
// run (an Existed op is kept, never run).
func runnableHostInstalls(r receipt.Receipt) []receipt.Op {
	var ops []receipt.Op

	for _, op := range r.RMA {
		if op.Kind == receipt.OpHostInstall && !op.Existed {
			ops = append(ops, op)
		}
	}

	return ops
}

// uninstallOp runs one host-install inverse. A malformed op (no argv) is a
// receipt defect, reported rather than indexed into a panic.
func (h *omp) uninstallOp(ctx context.Context, op receipt.Op) (string, error) {
	if len(op.Command) == 0 {
		return "", fmt.Errorf("omp: host-install op carries no argv (%+v)", op)
	}

	argv := op.Command

	if op.Existed {
		return strings.Join(argv, " ") + " existed before this delivery; kept", nil
	}

	if !isMarketplaceRemove(argv) {
		_, err := h.base.run(ctx, wordOmp, argv)
		if err == nil {
			return "", nil
		}

		if exit, refused := errors.AsType[*hostcli.ExitError](err); refused && strings.Contains(exit.Stderr, "is not installed") {
			return argv[len(argv)-1] + " was not installed; nothing to uninstall", nil
		}

		return "", err
	}

	marketplace := argv[len(argv)-1]

	if note, keep := marketplaceRefcount(ctx, Omp, h.Oracle().List, marketplace); keep {
		return note, nil
	}

	_, err := h.base.run(ctx, wordOmp, argv)
	if err == nil {
		return "", nil
	}

	if exit, refused := errors.AsType[*hostcli.ExitError](err); refused && strings.Contains(exit.Stderr, "not found") {
		return "marketplace " + marketplace + " was not registered", nil
	}

	return "", err
}

// ompOracle is the omp CLI oracle: list JSON, never an LLM.
type ompOracle struct {
	base *Base
}

// List implements Oracle: `omp plugin list --json` reports
// {npm: [...], marketplace: [{id, scope, entries: [...]}]}; only what is
// installed counts. omp prints it on stdout and honours --json here.
func (o *ompOracle) List(ctx context.Context) ([]Installed, error) {
	out, err := o.base.run(ctx, wordOmp, []string{wordPlugin, wordList, flagJSON})
	if err != nil {
		return nil, err
	}

	listed, parseErr := parseOmpInstalled(out)
	if parseErr != nil {
		return nil, &OracleError{Host: string(Omp), Output: string(out), Cause: parseErr}
	}

	return listed, nil
}

// Validate implements Oracle: omp 18.4.1 has no `plugin validate` subcommand.
// `plugin doctor` reports the plugins directory's own health (its JSON is a
// list of {name,status,message} about the plugins dir), never a package's, so
// validation is not supported by this host and no CLI call is made. The
// contract is pinned by TestOmpOracleValidate: a caller must treat
// ErrNotSupported as "this host cannot validate", never as "valid".
func (o *ompOracle) Validate(context.Context, string) ([]string, error) {
	return nil, ErrNotSupported
}

// ompPluginList is the omp 18.4.1 `plugin list --json` document: npm packages
// and marketplace plugins, the latter keyed by `<name>@<marketplace>` with one
// install record per scope.
type ompPluginList struct {
	NPM         []any `json:"npm"`
	Marketplace []struct {
		ID      string `json:"id"`
		Scope   string `json:"scope"`
		Entries []struct {
			Scope       string `json:"scope"`
			InstallPath string `json:"installPath"`
			Version     string `json:"version"`
		} `json:"entries"`
	} `json:"marketplace"`
}

// parseOmpInstalled reads one `omp plugin list --json` document. The
// marketplace entries are the verification target; the npm array is decoded
// with the shared tolerant walker, since no omp host here has one installed.
func parseOmpInstalled(out []byte) ([]Installed, error) {
	var keys map[string]json.RawMessage

	if err := json.Unmarshal(out, &keys); err != nil {
		return nil, err
	}

	// An unrecognized document means "cannot tell", never "nothing installed":
	// the marketplace refcount keeps a marketplace when the oracle cannot list,
	// and an empty answer from a renamed key would remove one another plugin
	// needs.
	if _, npm := keys["npm"]; !npm {
		if _, marketplaces := keys["marketplace"]; !marketplaces {
			return nil, errors.New("the output carries neither an npm nor a marketplace list")
		}
	}

	var doc ompPluginList

	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, err
	}

	var listed []Installed

	// An npm plugin has no marketplace, whatever its id looks like: the field
	// is cleared so a registry package can never satisfy a marketplace
	// selector (the registry is reachable through the same `name@tag` spelling).
	for _, item := range doc.NPM {
		var registry []Installed

		collectInstalled(item, &registry)

		for _, entry := range registry {
			entry.Marketplace = ""
			listed = append(listed, entry)
		}
	}

	for _, item := range doc.Marketplace {
		name, marketplace := splitPluginID(item.ID)
		if name == "" {
			continue
		}

		entry := Installed{Name: name, Marketplace: marketplace, Scope: item.Scope, Enabled: true}

		if len(item.Entries) > 0 {
			last := item.Entries[len(item.Entries)-1]
			entry.Version = last.Version
			entry.Path = last.InstallPath
			entry.Scope = cmp.Or(last.Scope, item.Scope)
		}

		listed = append(listed, entry)
	}

	return listed, nil
}

// ompStateRoot resolves the root omp keeps its plugin and marketplace state in:
// the base root, or `<base>/profiles/<profile>` for a named profile. The lock
// and every mutation of that state resolve through it, so verger never locks one
// tree and writes another. Live-verified: with OMP_PROFILE=work the host reads
// `~/.omp/profiles/work/plugins/` and reports nothing installed from the default
// root, and PI_CONFIG_DIR moves the same state to `<home>/<dir>/plugins`. Plugin
// state is a sibling of the agent dir, so PI_CODING_AGENT_DIR does not move it.
func ompStateRoot(home string) string {
	if profile := ompProfile(); profile != "" {
		return filepath.Join(ompBase(home), "profiles", profile)
	}

	return ompBase(home)
}

// ompBase resolves omp's base config root below HOME: PI_CONFIG_DIR replaces
// the `.omp` directory with a path below HOME (live-verified: a value of
// `/abs/cfg` resolves to `<home>/abs/cfg`), else `~/.omp`.
func ompBase(home string) string {
	if dir := strings.TrimSpace(os.Getenv("PI_CONFIG_DIR")); dir != "" {
		return filepath.Join(home, filepath.FromSlash(strings.TrimLeft(dir, `/\`)))
	}

	return filepath.Join(home, ompDirName)
}

// ompAgentDir resolves the agent dir of the active profile the way
// `omp config path` reports it: a named profile (OMP_PROFILE, then
// PI_PROFILE) wins over PI_CODING_AGENT_DIR, and both win over the default
// `<base>/agent` (live-verified).
func ompAgentDir(home string) string {
	if profile := ompProfile(); profile != "" {
		return filepath.Join(ompBase(home), "profiles", profile, ompAgentName)
	}

	if dir := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")); dir != "" {
		return absolutePath(dir)
	}

	return filepath.Join(ompBase(home), ompAgentName)
}

// ompProfile is the active profile name, if any. The rules are the host's own,
// measured against omp 18.4.1 with `omp config path` in an isolated HOME:
// OMP_PROFILE decides whenever it is SET — an explicitly empty value means the
// default profile and hides PI_PROFILE — the name `default` means the default
// profile too, and PI_PROFILE is consulted only while OMP_PROFILE is unset.
// Getting this wrong puts the lock, and every path derived from it, in a tree
// the host does not read.
func ompProfile() string {
	if value, set := os.LookupEnv("OMP_PROFILE"); set {
		return namedProfile(value)
	}

	return namedProfile(os.Getenv("PI_PROFILE"))
}

// namedProfile maps one profile variable onto a profile directory name; an empty
// value or `default` selects the default profile, which has no directory.
func namedProfile(value string) string {
	name := strings.TrimSpace(value)
	if name == "" || name == ompDefaultProfile {
		return ""
	}

	return name
}

// absolutePath makes a path absolute, keeping it verbatim when that fails.
func absolutePath(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		return absolute
	}

	return path
}
