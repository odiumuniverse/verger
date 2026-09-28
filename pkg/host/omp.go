package host

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

	ompDirName     = ".omp"       // base config root below HOME
	ompInstallID   = "install-id" // the config-independent install marker
	ompAgentName   = "agent"      // default agent dir below the base root
	ompSharedDir   = ".agents"    // the shared home omp's `agents` provider reads, ungated
	ompAgentsDir   = "agents"     // subagents, below the agent dir
	ompCommandsDir = "commands"   // slash commands, below the agent dir
	ompRulesDir    = "rules"      // rulebook documents, below the agent dir
	ompMCPDoc      = "mcp.json"   // MCP servers, below the agent dir
	// ompPluginLock is the process-exclusive advisory lock serializing every
	// omp plugin/marketplace mutation: omp's manager is not transactional and
	// has no cross-process locking, so two writers overwrite each other
	// (docs/reviews/omp-grammar.probe.log; OMPDOC
	// plugin-manager-installer-plumbing.md#lock-state-management-details).
	ompPluginLock = ".omp-plugin.verger.lock"
	// ompHooksBlocked is the delivery note of the hook component: omp has no
	// declarative hook document at all, only TS/JS modules under
	// hooks/{pre,post}/ that no manifest kind can express.
	ompHooksBlocked = "omp hooks are TS/JS modules under hooks/{pre,post}/ and have no declarative surface"
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
		renderAgent:  ompAgent,
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

	return plan.result(d.Strategy, false), err
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
		{Kind: receipt.OpHostInstall, Command: []string{wordPlugin, wordMarketplace, wordRemove, p.marketplace}},
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

		registered, err := h.marketplaces(ctx)
		if err != nil {
			return err
		}

		name, found := h.knownMarketplace(registered, pkg, plan)
		if !found {
			if name, err = h.register(ctx, registered, pkg, plan); err != nil {
				return err
			}
		}

		plan.marketplace = name
		pinned = name

		observed, err = h.verify(ctx, pkg, plan)

		return err
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
// registered: the planned name when the catalog declares it, else the entry
// that appeared. A refusal saying the marketplace already exists is tolerated
// (the manager is not idempotent), any other refusal surfaces.
func (h *omp) register(ctx context.Context, before []registeredMarketplace, pkg Package, plan *ompInstall) (string, error) {
	if _, err := h.base.run(ctx, wordOmp, []string{wordPlugin, wordMarketplace, wordAdd, plan.addRef}); err != nil && !ompMarketplaceExists(err) {
		return "", err
	}

	after, err := h.marketplaces(ctx)
	if err != nil {
		return "", err
	}

	name, found := h.resolveMarketplace(after, before, pkg, plan)
	if !found {
		return "", &DeliveryError{
			Host: string(Omp), Package: pkg.ID, Step: stepPlan,
			Cause: fmt.Errorf("the marketplace %s registered from %s is not reported by omp plugin marketplace list (%s)",
				plan.marketplace, plan.addRef, strings.Join(marketplaceNames(after), ", ")),
		}
	}

	return name, nil
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
			Cause:   fmt.Errorf("%s %s is not listed by the oracle", plan.installID(), pkg.Version),
		}
	}

	return OracleResult{Listed: listed, Verified: true}, nil
}

// knownMarketplace resolves a marketplace this delivery may reuse without
// calling `plugin marketplace add`: the registered one this package owns, or
// one that already serves the ref.
func (h *omp) knownMarketplace(registered []registeredMarketplace, pkg Package, plan *ompInstall) (string, bool) {
	for _, entry := range registered {
		if h.ownsMarketplace(pkg, entry.Name) {
			return entry.Name, true
		}
	}

	if plan.synth != nil {
		// The owner document declares the name, and the registered path is the
		// owner root (the store never moves).
		for _, entry := range registered {
			if entry.Name == plan.marketplace && samePath(entry.Path, plan.addRef) {
				return entry.Name, true
			}
		}

		return "", false
	}

	// A local directory ref registers under the path it was added from.
	for _, entry := range registered {
		if samePath(entry.Path, plan.addRef) {
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
func (h *omp) resolveMarketplace(after, before []registeredMarketplace, pkg Package, plan *ompInstall) (string, bool) {
	for _, entry := range after {
		if entry.Name == plan.marketplace || h.ownsMarketplace(pkg, entry.Name) {
			return entry.Name, true
		}
	}

	known := map[string]bool{}

	for _, entry := range before {
		known[entry.Name] = true
	}

	appeared := make([]string, 0, 1)

	for _, entry := range after {
		if !known[entry.Name] {
			appeared = append(appeared, entry.Name)
		}
	}

	if len(appeared) == 1 {
		return appeared[0], true
	}

	return "", false
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
		layout, name, _, err := ownerMarketplacePlan(ctx, Omp, d.Package, h.marketplaces)
		if err != nil {
			return ompInstall{}, err
		}

		return ompInstall{
			addRef: layout.root, plugin: layout.identity.Name, marketplace: name,
			version: d.Package.Version, synth: &layout,
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
	dir := ompBase(userHome)

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

// ompProfile is the active profile name, if any.
func ompProfile() string {
	for _, name := range []string{"OMP_PROFILE", "PI_PROFILE"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}

	return ""
}

// absolutePath makes a path absolute, keeping it verbatim when that fails.
func absolutePath(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		return absolute
	}

	return path
}
