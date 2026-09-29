package host

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
	"github.com/odiumuniverse/verger/pkg/store"
)

// Claude CLI argv words.
const (
	wordClaude      = "claude"
	wordPlugin      = "plugin"
	wordScope       = "--scope"
	wordMarketplace = "marketplace"
	wordInstallCLI  = "install"
	wordUninstall   = "uninstall"
	wordUpdate      = "update"
	wordMCP         = "mcp"
	wordRemove      = "remove"
	wordRm          = "rm"
	wordAdd         = "add"
	wordList        = "list"
	wordValidate    = "validate"
	flagJSON        = "--json"
)

// claude is the Claude Code adapter.
type claude struct {
	base *Base
}

// NewClaude builds the Claude Code adapter; the adapter is immutable after
// construction and safe for concurrent use.
func NewClaude(opts ...Option) Host {
	return &claude{base: newBase(opts)}
}

// ID implements Host.
func (h *claude) ID() ID {
	return Claude
}

// Oracle implements Host.
func (h *claude) Oracle() Oracle {
	return &claudeOracle{base: h.base}
}

// Detect reports whether the Claude config dir exists or the `claude` binary
// resolves; CLAUDE_CONFIG_DIR overrides the config home (profiles).
func (h *claude) Detect(home string) bool {
	if isDir(claudeConfigDir(h.base.effectiveHome(home))) {
		return true
	}

	_, err := h.base.resolve(wordClaude)

	return err == nil
}

// Deliver implements Host with explicit strategies; the ladder is pkg/plan.
// The shared dispatch checks the source ref (`Package.Marketplace`) before any
// stratum, so a host-forbidden source is never delivered — not even as files.
func (h *claude) Deliver(ctx context.Context, home string, d Delivery) (Result, error) {
	return deliverByStrategy(ctx, h.base, Claude, home, d, h.deliverInstall, h.deliverLoose)
}

// deliverLoose plans and executes the host-native user files.
func (h *claude) deliverLoose(ctx context.Context, home string, d Delivery) (Result, error) {
	dir := claudeConfigDir(h.base.effectiveHome(home))

	spec := looseSpec{
		host:          Claude,
		binary:        wordClaude,
		home:          h.base.effectiveHome(home),
		skillsDir:     filepath.Join(dir, "skills"),
		agentsDir:     filepath.Join(dir, "agents"),
		commandsDir:   filepath.Join(dir, "commands"),
		settingsPath:  filepath.Join(dir, "settings.json"),
		mcpAddArgs:    render.ClaudeMCPAddArgs,
		mcpRemoveArgs: render.ClaudeMCPRemoveArgs,
		mcpGetArgs:    claudeMCPGetArgs,
	}

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

// claudeMCPGetArgs is the `claude mcp get <name>` argv: the read-only probe of
// one server name across every scope.
func claudeMCPGetArgs(name string) []string {
	return []string{wordMCP, "get", name}
}

// MCPProber is the optional Oracle extension for a host that manages MCP
// servers through its own CLI: the read-only probe of one server's value.
// A host whose MCP surface is a document (gemini, cursor, omp, opencode,
// kilo) does not implement it — the receipt's config-key op is the claim.
type MCPProber interface {
	// MCPGet returns the host's current value of one CLI-managed MCP server,
	// in the manifest shape the receipt digest was computed from, so a drift
	// check can compare the two.
	MCPGet(ctx context.Context, name string) (manifest.MCPServer, error)
}

// ErrServerNotFound reports that the host has no MCP server with the probed
// name: the delivered value is gone, which is drift, not a missing host.
var ErrServerNotFound = errors.New("the host has no such MCP server")

// MCPGet implements MCPProber: the read-only `claude mcp get <name>` probe.
func (o *claudeOracle) MCPGet(ctx context.Context, name string) (manifest.MCPServer, error) {
	out, err := o.base.run(ctx, wordClaude, claudeMCPGetArgs(name))
	if err != nil {
		exit, ok := errors.AsType[*hostcli.ExitError](err)
		if ok && reportsUnknownServer(exit.Stderr+"\n"+string(out)) {
			return manifest.MCPServer{}, ErrServerNotFound
		}

		return manifest.MCPServer{}, err
	}

	return parseClaudeMCPGet(name, out)
}

// parseClaudeMCPGet reads the `claude mcp get <name>` text answer into the
// manifest shape the receipt digest was computed from (loosePlanner's
// rewriteServer output): Name, Transport, Command, Env, URL, Headers. The
// host reports the resolved command, so a server whose manifest carries no
// secrets compares equal; a server with secrets does not, because the
// receipt records the rewritten refs while the host holds the values.
func parseClaudeMCPGet(name string, out []byte) (manifest.MCPServer, error) {
	server := manifest.MCPServer{Name: name}

	var command []string

	var env map[string]string

	inEnv := false

	for line := range strings.Lines(string(out)) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		key, value, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}

		switch key = strings.TrimSpace(key); key {
		case "Type":
			server.Transport = strings.TrimSpace(value)
		case "Command":
			command = append(command, strings.TrimSpace(value))
		case "Args":
			command = append(command, strings.Fields(strings.TrimSpace(value))...)
		case "URL":
			server.URL = strings.TrimSpace(value)
		case "Environment":
			inEnv = true
		default:
			if !inEnv || !strings.Contains(trimmed, "=") {
				continue
			}

			envKey, envValue, _ := strings.Cut(trimmed, "=")

			if env == nil {
				env = map[string]string{}
			}

			env[strings.TrimSpace(envKey)] = strings.TrimSpace(envValue)
		}
	}

	server.Command = command
	server.Env = env

	return server, nil
}

// installPlan is one native or synth host install.
type installPlan struct {
	addRef      string // native: the source ref; synth: the owner marketplace root
	plugin      string
	marketplace string
	installID   string       // <plugin>@<marketplace>
	registered  bool         // synth: the owner marketplace is already registered
	synth       *synthLayout // synth only
	rma         []receipt.Op
}

// newInstallPlan builds the plan of one plugin in one marketplace; the RMA
// removes the install, then the marketplace (refcounted at removal).
func newInstallPlan(addRef, plugin, marketplace, scope string) installPlan {
	installID := plugin + "@" + marketplace

	// The scope rides at the tail of every argv, so the inverse removes from
	// the same scope the install wrote to. A project install removed at user
	// scope would leave the project's registration behind.
	return installPlan{
		addRef:      addRef,
		plugin:      plugin,
		marketplace: marketplace,
		installID:   installID,
		rma: []receipt.Op{
			{Kind: receipt.OpHostInstall, Command: concat([]string{wordPlugin, wordMarketplace, wordRm, marketplace}, scopeArgs(scope))},
			{Kind: receipt.OpHostInstall, Command: concat([]string{wordPlugin, wordUninstall, installID}, scopeArgs(scope))},
		},
	}
}

// scopeArgs returns the host CLI flag that selects the installation scope, or
// nothing at user scope so every user-scope command is byte-identical to what
// verger has always run. Claude Code 2.1.284 takes -s/--scope on both
// `plugin marketplace add` and `plugin install`, and "user" is the default.
func scopeArgs(scope string) []string {
	if scope == "" || scope == receipt.ScopeUser {
		return nil
	}

	return []string{wordScope, scope}
}

// concat joins argv pieces into one command.
func concat(parts ...[]string) []string {
	var out []string
	for _, part := range parts {
		out = append(out, part...)
	}

	return out
}

// bareArgv strips a trailing --scope pair, so the command matchers keep
// working whether or not the scope was recorded.
func bareArgv(argv []string) []string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == wordScope {
			return append(append([]string{}, argv[:i]...), argv[i+2:]...)
		}
	}

	return argv
}

// deliverInstall runs the native or synth host installer and verifies via the
// oracle; verify failure is a *DeliveryError, never a silent step down. The
// command-source policy is checked before any host call.
func (h *claude) deliverInstall(ctx context.Context, home string, d Delivery, synth bool) (Result, error) {
	if err := checkCommandSources(Claude, h.base.effectiveHome(home), d.Package.Marketplace); err != nil {
		return Result{}, err
	}

	plan, err := h.installPlan(ctx, d, synth)
	if err != nil {
		return Result{}, err
	}

	if d.DryRun {
		return Result{Strategy: d.Strategy, RMA: slices.Clone(plan.rma), Notes: []string{noteDryRun}}, nil
	}

	verb := wordInstallCLI

	if plan.synth != nil {
		if verb, err = h.registerSynth(ctx, d.Package, plan); err != nil {
			return Result{}, err
		}
	} else if _, err := h.base.run(ctx, wordClaude, concat([]string{wordPlugin, wordMarketplace, wordAdd, plan.addRef}, scopeArgs(d.Package.Scope))); err != nil {
		return Result{}, err
	}

	if _, err := h.base.run(ctx, wordClaude, concat([]string{wordPlugin, verb, plan.installID}, scopeArgs(d.Package.Scope))); err != nil {
		return Result{}, err
	}

	observed, err := h.verify(ctx, d.Package, plan)
	if err != nil {
		return Result{}, err
	}

	return Result{Strategy: d.Strategy, RMA: plan.rma, Observed: observed}, nil
}

// verify asks the oracle for the plugin in its marketplace; a synth install
// renders exactly the package version, so the host must list that version.
func (h *claude) verify(ctx context.Context, pkg Package, plan installPlan) (OracleResult, error) {
	listed, err := h.Oracle().List(ctx)
	if err != nil {
		return OracleResult{}, err
	}

	entry, found := installedAs(listed, plan.plugin, plan.marketplace)

	if found && plan.synth != nil && entry.Version != "" && pkg.Version != "" && entry.Version != pkg.Version {
		found = false
	}

	if !found {
		return OracleResult{}, &DeliveryError{
			Host:    string(Claude),
			Package: pkg.ID,
			Step:    stepVerify,
			Cause:   fmt.Errorf("%s %s is not listed by the oracle", plan.installID, pkg.Version),
		}
	}

	return OracleResult{Listed: listed, Verified: true}, nil
}

// installPlan resolves the marketplace, the plugin and the install id of one
// delivery.
func (h *claude) installPlan(ctx context.Context, d Delivery, synth bool) (installPlan, error) {
	if synth {
		return h.synthPlan(ctx, d.Package)
	}

	_, name := splitID(d.Package.ID)
	if name == "" {
		return installPlan{}, &NotSupportedError{Host: Claude, Operation: unsupportedNoName}
	}

	if d.Package.Marketplace == "" {
		return installPlan{}, &NotSupportedError{Host: Claude, Operation: unsupportedNoMarketplace}
	}

	return newInstallPlan(d.Package.Marketplace, name, marketplaceName(d.Package.Marketplace), d.Package.Scope), nil
}

// synthPlan places the synth package in its owner marketplace (decision F3):
// the owner root <store>/synth/<owner> is the marketplace, the plugin is the
// bare name, the reference name@<marketplace>. The marketplace name is the one
// the owner document already carries (else the owner), unless the host has a
// foreign marketplace of that name — then <owner>-verger; a plugin name
// another package of the owner holds is refused. Only read-only host calls.
func (h *claude) synthPlan(ctx context.Context, pkg Package) (installPlan, error) {
	layout, name, isRegistered, err := ownerMarketplacePlan(ctx, Claude, pkg, h.marketplaces)
	if err != nil {
		return installPlan{}, err
	}

	plan := newInstallPlan(layout.root, layout.identity.Name, name, pkg.Scope)
	plan.registered = isRegistered
	plan.synth = &layout

	return plan, nil
}

// registerSynth writes the owner marketplace document, then updates the
// registered marketplace or adds it, and picks install for a new plugin or
// update for one the host already has.
func (h *claude) registerSynth(ctx context.Context, pkg Package, plan installPlan) (string, error) {
	if _, err := plan.synth.writeDoc(ctx, plan.marketplace); err != nil {
		return "", &DeliveryError{Host: string(Claude), Package: pkg.ID, Step: stepInstall, Cause: err}
	}

	register := []string{wordPlugin, wordMarketplace, wordAdd, plan.addRef}
	if plan.registered {
		register = []string{wordPlugin, wordMarketplace, wordUpdate, plan.marketplace}
	}

	if _, err := h.base.run(ctx, wordClaude, register); err != nil {
		return "", err
	}

	listed, err := h.Oracle().List(ctx)
	if err != nil {
		return "", err
	}

	if _, installed := installedAs(listed, plan.plugin, plan.marketplace); installed {
		return wordUpdate, nil
	}

	return wordInstallCLI, nil
}

// marketplaces lists what `claude plugin marketplace list --json` reports.
func (h *claude) marketplaces(ctx context.Context) ([]registeredMarketplace, error) {
	out, err := h.base.run(ctx, wordClaude, []string{wordPlugin, wordMarketplace, wordList, flagJSON})
	if err != nil {
		return nil, err
	}

	var listed []struct {
		Name            string `json:"name"`
		Path            string `json:"path"`
		InstallLocation string `json:"installLocation"`
	}

	if err := json.Unmarshal(out, &listed); err != nil {
		return nil, &OracleError{Host: string(Claude), Output: string(out), Cause: err}
	}

	registered := make([]registeredMarketplace, 0, len(listed))

	for _, entry := range listed {
		registered = append(registered, registeredMarketplace{Name: entry.Name, Path: cmp.Or(entry.Path, entry.InstallLocation)})
	}

	return registered, nil
}

// installedAs finds the plugin installed from one marketplace; an entry that
// reports no marketplace matches by name alone.
func installedAs(listed []Installed, plugin, marketplace string) (Installed, bool) {
	for _, entry := range listed {
		if entry.Name == plugin && (entry.Marketplace == "" || entry.Marketplace == marketplace) {
			return entry, true
		}
	}

	return Installed{}, false
}

// listedContains reports whether the oracle lists one of the names.
func listedContains(listed []Installed, names []string) bool {
	for _, entry := range listed {
		if slices.Contains(names, entry.Name) {
			return true
		}
	}

	return false
}

// Uninstall runs the host-install RMA ops in reverse order; file/tree ops are
// executed by pkg/apply. A plugin uninstall the host no longer lists is
// already done (apply rolls failures back with the dry-run RMA); a marketplace
// is removed only once no installed plugin comes from it (§4.8 refcount), so a
// failed delivery never leaves its marketplace registered and a live one is
// never pulled from under another plugin. The plugin cache Claude leaves
// behind goes to the trash.
func (h *claude) Uninstall(ctx context.Context, home string, r receipt.Receipt) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	userHome := h.base.effectiveHome(home)

	var notes []string

	for _, op := range slices.Backward(r.RMA) {
		if op.Kind != receipt.OpHostInstall {
			continue
		}

		note, err := h.uninstallOp(ctx, userHome, r.Package, op)
		if err != nil {
			return Result{}, err
		}

		if note != "" {
			notes = append(notes, note)
		}
	}

	return Result{Strategy: Strategy(r.Strategy), Notes: notes}, nil
}

// uninstallOp runs one host-install inverse.
func (h *claude) uninstallOp(ctx context.Context, userHome, pkg string, op receipt.Op) (string, error) {
	argv := op.Command

	switch {
	case isUninstallCommand(argv):
		return h.uninstallPlugin(ctx, userHome, pkg, argv[2])
	case isMarketplaceRemove(argv):
		return h.removeMarketplace(ctx, userHome, argv[3])
	case isMCPRemove(argv):
		return h.removeMCPServer(ctx, op)
	default:
		_, err := h.base.run(ctx, wordClaude, argv)

		return "", err
	}
}

// removeMCPServer runs one `mcp remove` inverse: a server that existed before
// the delivery (a re-delivered own server, NF-2) is never removed by it, and
// a server the host does not know is already removed (NF-3).
func (h *claude) removeMCPServer(ctx context.Context, op receipt.Op) (string, error) {
	name := op.Command[len(op.Command)-1]

	if op.Existed {
		return "mcp " + name + " existed before this delivery; kept", nil
	}

	out, err := h.base.run(ctx, wordClaude, op.Command)
	if err == nil {
		return "", nil
	}

	if exit, refused := errors.AsType[*hostcli.ExitError](err); refused && reportsUnknownServer(exit.Stderr+"\n"+string(out)) {
		return "mcp " + name + " was not configured; nothing to remove", nil
	}

	return "", err
}

// isMCPRemove reports whether argv is `mcp remove … <name>`.
func isMCPRemove(argv []string) bool {
	return len(argv) >= 3 && argv[0] == wordMCP && argv[1] == wordRemove
}

// isUninstallCommand reports whether argv is the Claude `plugin uninstall <id>` op.
func isUninstallCommand(argv []string) bool {
	argv = bareArgv(argv)

	return len(argv) == 3 && argv[0] == wordPlugin && argv[1] == wordUninstall
}

// uninstallPlugin uninstalls one plugin id; a refusal for a plugin the host
// does not list is an already finished uninstall. Once the host no longer
// lists the plugin, its cache goes to the trash.
func (h *claude) uninstallPlugin(ctx context.Context, userHome, pkg, id string) (string, error) {
	_, runErr := h.base.run(ctx, wordClaude, []string{wordPlugin, wordUninstall, id})
	if _, refused := errors.AsType[*hostcli.ExitError](runErr); runErr != nil && !refused {
		return "", runErr
	}

	listed, err := h.Oracle().List(ctx)
	if err != nil {
		return "", cmp.Or(runErr, err)
	}

	plugin, marketplace := splitPluginID(id)

	if _, still := installedAs(listed, plugin, marketplace); still {
		if runErr != nil {
			return "", runErr
		}

		return id + " is still installed in another scope; its cache is kept", nil
	}

	gone := ""
	if runErr != nil {
		gone = id + " was not installed; nothing to uninstall"
	}

	note, err := h.trashPluginCache(ctx, userHome, pkg, marketplace, plugin)
	if err != nil {
		return "", err
	}

	return strings.Join(slices.DeleteFunc([]string{gone, note}, func(s string) bool { return s == "" }), "; "), nil
}

// removeMarketplace removes one marketplace once no installed plugin comes
// from it; a marketplace the host does not know is already removed.
func (h *claude) removeMarketplace(ctx context.Context, userHome, marketplace string) (string, error) {
	listed, err := h.Oracle().List(ctx)
	if err != nil {
		return "", err
	}

	serving := 0

	for _, entry := range listed {
		if entry.Marketplace == marketplace {
			serving++
		}
	}

	if serving > 0 {
		return fmt.Sprintf("marketplace %s still serves %d installed plugin(s); kept", marketplace, serving), nil
	}

	out, err := h.base.run(ctx, wordClaude, []string{wordPlugin, wordMarketplace, wordRm, marketplace})
	if err != nil {
		exit, refused := errors.AsType[*hostcli.ExitError](err)
		if !refused || !strings.Contains(strings.ToLower(exit.Stderr+string(out)), "not found") {
			return "", err
		}

		return "marketplace " + marketplace + " was not registered", nil
	}

	removeEmptyDir(filepath.Join(claudeConfigDir(userHome), "plugins", "cache", marketplace))

	return "", nil
}

// trashPluginCache moves the cache Claude keeps after an uninstall
// (<config>/plugins/cache/<marketplace>/<plugin>) to the trash.
func (h *claude) trashPluginCache(ctx context.Context, userHome, pkg, marketplace, plugin string) (string, error) {
	if !validElement(marketplace) || !validElement(plugin) {
		return "", nil
	}

	dir := filepath.Join(claudeConfigDir(userHome), "plugins", "cache", marketplace, plugin)
	if !fileExists(dir) {
		return "", nil
	}

	if h.base.trash == nil {
		return "the plugin cache " + dir + " is kept (no trash configured)", nil
	}

	if _, err := h.base.trash.Put(ctx, dir, store.PutOptions{Package: pkg, Host: string(Claude)}); err != nil {
		return "", &DeliveryError{Host: string(Claude), Package: pkg, Step: stepUninstall, Cause: err}
	}

	return "moved the plugin cache " + dir + " to the trash", nil
}

// removeEmptyDir removes a dir that has no entries left.
func removeEmptyDir(dir string) {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		_ = os.Remove(dir)
	}
}

// isMarketplaceRemove reports whether argv is `plugin marketplace rm|remove <name>`.
func isMarketplaceRemove(argv []string) bool {
	argv = bareArgv(argv)

	return len(argv) == 4 && argv[0] == wordPlugin && argv[1] == wordMarketplace && (argv[2] == wordRm || argv[2] == wordRemove)
}

// claudeOracle is the Claude CLI oracle: list/validate JSON, never an LLM.
type claudeOracle struct {
	base *Base
}

// List implements Oracle.
func (o *claudeOracle) List(ctx context.Context) ([]Installed, error) {
	out, err := o.base.run(ctx, wordClaude, []string{wordPlugin, wordList, flagJSON})
	if err != nil {
		return nil, err
	}

	listed, parseErr := parseInstalled(out)
	if parseErr != nil {
		return nil, &OracleError{Host: string(Claude), Output: string(out), Cause: parseErr}
	}

	return listed, nil
}

// Validate implements Oracle.
func (o *claudeOracle) Validate(ctx context.Context, dir string) ([]string, error) {
	out, err := o.base.run(ctx, wordClaude, []string{wordPlugin, wordValidate, dir})
	if err != nil {
		output := strings.TrimSpace(string(out))

		if exit, ok := errors.AsType[*hostcli.ExitError](err); ok && output == "" {
			output = exit.Stderr
		}

		return nil, &OracleError{Host: string(Claude), Output: output, Cause: err}
	}

	return warningLines(out), nil
}

// parseInstalled extracts plugin entries from any nesting of the host output:
// the Claude Code 2.1.283 shape `{id: "<name>@<marketplace>", version, scope,
// enabled, installPath}` (F7) and, as a fallback, `{name, marketplace,
// version, path}`.
func parseInstalled(output []byte) ([]Installed, error) {
	var root any

	if err := json.Unmarshal(output, &root); err != nil {
		return nil, err
	}

	var listed []Installed

	collectInstalled(root, &listed)

	if len(listed) == 0 && !emptyContainer(root) {
		return nil, errors.New("the output carries no plugin entries")
	}

	return listed, nil
}

// emptyContainer reports whether a parsed JSON value is an empty list, null,
// or an object holding only empty containers (codex-cli 0.157.1 answers
// `{"installed":[],"available":[]}`): a valid "nothing installed".
func emptyContainer(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case []any:
		return len(typed) == 0
	case map[string]any:
		for _, item := range typed {
			if !emptyContainer(item) {
				return false
			}
		}

		return true
	default:
		return false
	}
}

// collectInstalled walks any nesting and collects plugin entries; an entry's
// own fields are not searched further, so a nested object carrying a name is
// not taken for another plugin (T1.6-review-1 [R10]).
func collectInstalled(value any, listed *[]Installed) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			collectInstalled(item, listed)
		}
	case map[string]any:
		if entry, ok := installedEntry(typed); ok {
			*listed = append(*listed, entry)

			return
		}

		for _, key := range slices.Sorted(maps.Keys(typed)) {
			collectInstalled(typed[key], listed)
		}
	}
}

// installedEntry reads one plugin entry of any live shape:
//   - Claude Code 2.1.283: `id` "<name>@<marketplace>", `installPath`;
//   - codex-cli 0.157.1: `pluginId`, `name`, `marketplaceName`, `installed`;
//   - Gemini CLI 0.61.0: `name`, `path`, `isActive` (its `id` is a hash);
//   - the legacy `{name, marketplace, path}`.
//
// An id is split at the last @. An entry reporting `installed: false` (Codex
// --available) is not installed. Anything without a name is not an entry.
func installedEntry(object map[string]any) (Installed, bool) {
	if installed, reported := object["installed"].(bool); reported && !installed {
		return Installed{}, false
	}

	idName, idMarketplace := splitPluginID(cmp.Or(stringField(object, "id"), stringField(object, "pluginId")))

	name := cmp.Or(stringField(object, "name"), idName)
	if name == "" {
		return Installed{}, false
	}

	enabled, reported := object["enabled"].(bool)
	if !reported {
		enabled, reported = object["isActive"].(bool)
	}

	return Installed{
		Name:        name,
		Marketplace: cmp.Or(stringField(object, "marketplace"), stringField(object, "marketplaceName"), idMarketplace),
		Version:     stringField(object, "version"),
		Path:        cmp.Or(stringField(object, "installPath"), stringField(object, "path")),
		Scope:       stringField(object, "scope"),
		Enabled:     enabled || !reported,
	}, true
}

// splitPluginID splits a host plugin id "<name>@<marketplace>" at the last
// @; an id without both parts is not a plugin id.
func splitPluginID(id string) (string, string) {
	index := strings.LastIndexByte(id, '@')
	if index <= 0 || index == len(id)-1 {
		return "", ""
	}

	return id[:index], id[index+1:]
}

// stringField returns a string field or the empty string.
func stringField(object map[string]any, key string) string {
	value, _ := object[key].(string)

	return value
}

// splitID splits one owner/name id; a bare name has no owner.
func splitID(id string) (string, string) {
	if index := strings.LastIndexByte(id, '/'); index >= 0 {
		return id[:index], id[index+1:]
	}

	return "", id
}

// claudeConfigDir resolves the config dir: CLAUDE_CONFIG_DIR (profiles) or
// <home>/.claude.
func claudeConfigDir(home string) string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		absolute, err := filepath.Abs(dir)
		if err == nil {
			return absolute
		}

		return dir
	}

	return filepath.Join(home, ".claude")
}

// isDir reports whether path is an existing directory.
func isDir(path string) bool {
	info, err := os.Stat(path)

	return err == nil && info.IsDir()
}
