package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
)

// Codex surface names (DESIGN §4.2): the shared ~/.agents root.
const (
	wordCodex      = "codex"
	codexAgentsDir = ".agents"
)

// codex is the Codex adapter. Its plugin grammar is the one of codex-cli
// 0.157.1, verified live (docs/reviews/T1.7-T1.8-grammar.probe.log):
// `plugin marketplace add|list|remove`, `plugin add|list|remove`, selectors
// `<plugin>@<marketplace>`, `--json` on every verb.
type codex struct {
	base *Base
}

// NewCodex builds the Codex adapter; the adapter is immutable after
// construction and safe for concurrent use.
func NewCodex(opts ...Option) Host {
	return &codex{base: newBase(opts)}
}

// ID implements Host.
func (h *codex) ID() ID {
	return Codex
}

// Oracle implements Host.
func (h *codex) Oracle() Oracle {
	return &codexOracle{base: h.base}
}

// Detect reports whether the Codex config dir exists or the `codex` binary
// resolves; CODEX_HOME overrides the config home (profiles).
func (h *codex) Detect(home string) bool {
	if isDir(codexConfigDir(h.base.effectiveHome(home))) {
		return true
	}

	_, err := h.base.resolve(wordCodex)

	return err == nil
}

// Deliver implements Host with explicit strategies; the ladder is pkg/plan.
// The shared dispatch checks the source ref (`Package.Marketplace`) before any
// stratum, so a host-forbidden source is never delivered — not even as files.
func (h *codex) Deliver(ctx context.Context, home string, d Delivery) (Result, error) {
	return deliverByStrategy(ctx, h.base, Codex, home, d, h.deliverInstall, h.deliverLoose)
}

// codexSpec is the loose surface of the Codex adapter: skills in the shared
// ~/.agents/skills root, agents/prompts/hooks/config.toml under the Codex home.
func codexSpec(userHome, codexDir string) looseSpec {
	return looseSpec{
		host:           Codex,
		binary:         wordCodex,
		home:           userHome,
		skillsDir:      filepath.Join(userHome, codexAgentsDir, "skills"),
		agentsDir:      filepath.Join(codexDir, "agents"),
		commandsDir:    filepath.Join(codexDir, "prompts"),
		settingsPath:   filepath.Join(codexDir, "hooks.json"),
		hooksPath:      filepath.Join(codexDir, "hooks.json"),
		hooksFormat:    manifest.FormatCodex,
		hooksTrustNote: true,
		agentExt:       tomlExt,
		renderAgent: func(agent render.Agent, fallback string) ([]byte, error) {
			if strings.TrimSpace(agent.Name) == "" {
				agent.Name = fallback
			}

			return agent.CodexTOML()
		},
		renderCommand: func(cmd render.Command) ([]byte, error) { return cmd.CodexPrompt(), nil },
		mcpConfig: &mcpConfigSpec{
			path:   filepath.Join(codexDir, "config.toml"),
			format: manifest.FormatCodex,
			toml:   true,
			edit:   render.EditTOML,
		},
	}
}

// deliverLoose plans and executes the host-native user files.
func (h *codex) deliverLoose(ctx context.Context, home string, d Delivery) (Result, error) {
	userHome := h.base.effectiveHome(home)
	spec := codexSpec(userHome, codexConfigDir(userHome))

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

// codexInstall is one native or synth plugin install from a marketplace.
type codexInstall struct {
	addRef      string // native: the source ref; synth: the owner marketplace root
	plugin      string
	marketplace string
	synth       *synthLayout // synth: the owner marketplace (decision F3)
}

// installID is the plugin selector `<plugin>@<marketplace>`.
func (p codexInstall) installID() string {
	return p.plugin + "@" + p.marketplace
}

// rma removes the plugin, then — refcounted at removal — its marketplace.
func (p codexInstall) rma() []receipt.Op {
	return []receipt.Op{
		{Kind: receipt.OpHostInstall, Command: []string{wordPlugin, wordMarketplace, wordRemove, p.marketplace}},
		{Kind: receipt.OpHostInstall, Command: []string{wordPlugin, wordRemove, p.installID()}},
	}
}

// deliverInstall registers the marketplace, adds the plugin and verifies it
// through the oracle; a verify failure is a *DeliveryError, never a silent
// step down. The command-source policy is checked before anything is planned.
func (h *codex) deliverInstall(ctx context.Context, home string, d Delivery, synth bool) (Result, error) {
	if err := checkCommandSources(Codex, h.base.effectiveHome(home), d.Package.Marketplace); err != nil {
		return Result{}, err
	}

	plan, err := h.installPlan(ctx, d, synth)
	if err != nil {
		return Result{}, err
	}

	if d.DryRun {
		return Result{Strategy: d.Strategy, RMA: plan.rma(), Notes: []string{"dry-run"}}, nil
	}

	observed, err := h.install(ctx, d.Package, &plan)

	// The result is what execution produced even on error (NF-5): a native
	// add names the marketplace the host actually registered.
	return Result{Strategy: d.Strategy, RMA: plan.rma(), Observed: observed}, err
}

// install writes the owner marketplace document (synth), registers the
// marketplace, adds the plugin and verifies it. `marketplace add` is
// idempotent and `plugin add` of an installed plugin moves it to the
// marketplace's version, so a re-delivery or an update runs the same argv.
func (h *codex) install(ctx context.Context, pkg Package, plan *codexInstall) (OracleResult, error) {
	if plan.synth != nil {
		if _, err := plan.synth.writeDoc(ctx, plan.marketplace); err != nil {
			return OracleResult{}, &DeliveryError{Host: string(Codex), Package: pkg.ID, Step: stepInstall, Cause: err}
		}
	}

	out, err := h.base.run(ctx, wordCodex, []string{wordPlugin, wordMarketplace, wordAdd, plan.addRef, flagJSON})
	if err != nil {
		return OracleResult{}, err
	}

	if name := addedMarketplace(out); name != "" && plan.synth == nil {
		plan.marketplace = name
	}

	if _, err := h.base.run(ctx, wordCodex, []string{wordPlugin, wordAdd, plan.installID()}); err != nil {
		return OracleResult{}, err
	}

	listed, err := h.Oracle().List(ctx)
	if err != nil {
		return OracleResult{}, err
	}

	entry, found := installedAs(listed, plan.plugin, plan.marketplace)

	if found && staleSynthVersion(plan, entry, pkg.Version) {
		found = false
	}

	if !found {
		return OracleResult{}, &DeliveryError{
			Host:    string(Codex),
			Package: pkg.ID,
			Step:    stepVerify,
			Cause:   fmt.Errorf("%s %s is not listed by the oracle", plan.installID(), pkg.Version),
		}
	}

	return OracleResult{Listed: listed, Verified: true}, nil
}

// staleSynthVersion reports whether the oracle lists the plugin at a version
// other than the one this delivery renders. A synth install renders exactly
// its own version, so any other listing is not this install; when either side
// reports no version the plain match decides.
func staleSynthVersion(plan *codexInstall, entry Installed, version string) bool {
	return plan.synth != nil && entry.Version != "" && version != "" && entry.Version != version
}

// addedMarketplace reads the marketplace name `plugin marketplace add --json`
// reports; the name comes from the marketplace's own document.
func addedMarketplace(out []byte) string {
	var added struct {
		MarketplaceName string `json:"marketplaceName"`
	}

	if json.Unmarshal(out, &added) != nil {
		return ""
	}

	return added.MarketplaceName
}

// installPlan resolves the marketplace ref, the plugin and the marketplace of
// one delivery. The native marketplace name is only the planned one: the
// authoritative name is the `marketplaceName` `plugin marketplace add --json`
// reports (the marketplace's own document names it), so install overwrites the
// plan before the RMA is composed. No `plugin marketplace list` lookup is
// needed on this path — the add reports the registered name and is idempotent;
// marketplaces(ctx) serves only the synth owner-marketplace decision.
func (h *codex) installPlan(ctx context.Context, d Delivery, synth bool) (codexInstall, error) {
	if synth {
		layout, name, _, err := ownerMarketplacePlan(ctx, Codex, d.Package, h.marketplaces)
		if err != nil {
			return codexInstall{}, err
		}

		return codexInstall{addRef: layout.root, plugin: layout.identity.Name, marketplace: name, synth: &layout}, nil
	}

	_, name := splitID(d.Package.ID)
	if name == "" {
		return codexInstall{}, &NotSupportedError{Host: Codex, Operation: "install without a package name"}
	}

	if d.Package.Marketplace == "" {
		return codexInstall{}, &NotSupportedError{Host: Codex, Operation: "native install without a marketplace"}
	}

	// The planned name is the ref's last segment; the add reports the real one.
	return codexInstall{addRef: d.Package.Marketplace, plugin: name, marketplace: marketplaceName(d.Package.Marketplace)}, nil
}

// marketplaces lists what `codex plugin marketplace list --json` reports.
func (h *codex) marketplaces(ctx context.Context) ([]registeredMarketplace, error) {
	out, err := h.base.run(ctx, wordCodex, []string{wordPlugin, wordMarketplace, wordList, flagJSON})
	if err != nil {
		return nil, err
	}

	var listed struct {
		Marketplaces []struct {
			Name string `json:"name"`
			Root string `json:"root"`
		} `json:"marketplaces"`
	}

	if err := json.Unmarshal(out, &listed); err != nil {
		return nil, &OracleError{Host: string(Codex), Output: string(out), Cause: err}
	}

	registered := make([]registeredMarketplace, 0, len(listed.Marketplaces))

	for _, entry := range listed.Marketplaces {
		registered = append(registered, registeredMarketplace{Name: entry.Name, Path: entry.Root})
	}

	return registered, nil
}

// Uninstall runs the host-install RMA ops in reverse order; file/tree ops are
// executed by pkg/apply. `plugin remove` is idempotent and removes the plugin
// cache; a marketplace is removed only once no installed plugin comes from it
// (§4.8 refcount), and one the host no longer knows is already removed. An op
// marked Existed names a resource that predates this delivery and is kept
// (Host.Deliver), so a rollback never removes what the user already had.
func (h *codex) Uninstall(ctx context.Context, _ string, r receipt.Receipt) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	notes, err := hostInverses(ctx, r, h.uninstallOp)
	if err != nil {
		return Result{}, err
	}

	return Result{Strategy: Strategy(r.Strategy), Notes: notes}, nil
}

// uninstallOp runs one host-install inverse. The adapter records no Existed op
// on the plugin path — `plugin add` is the delivery's own target and is
// idempotent, so removing it is the right inverse, exactly as in the claude
// adapter — but an op that arrives marked Existed (a re-delivered own plugin
// recorded by a caller, Host.Deliver) is kept.
func (h *codex) uninstallOp(ctx context.Context, op receipt.Op) (string, error) {
	argv := codexArgv(op.Command)

	if op.Existed {
		return strings.Join(argv, " ") + " existed before this delivery; kept", nil
	}

	if !isMarketplaceRemove(argv) {
		_, err := h.base.run(ctx, wordCodex, argv)

		return "", err
	}

	marketplace := argv[3]

	if note, keep := h.marketplaceInUse(ctx, marketplace); keep {
		return note, nil
	}

	_, err := h.base.run(ctx, wordCodex, argv)
	if err == nil {
		return "", nil
	}

	if exit, refused := errors.AsType[*hostcli.ExitError](err); refused && strings.Contains(exit.Stderr, "is not configured or installed") {
		return "marketplace " + marketplace + " was not configured", nil
	}

	return "", err
}

// codexArgv maps the inverse argv receipts recorded before codex-cli 0.157.1
// was verified (`plugin uninstall`, `plugin marketplace rm`) to the real verbs.
func codexArgv(argv []string) []string {
	switch {
	case len(argv) == 3 && argv[0] == wordPlugin && argv[1] == wordUninstall:
		return []string{wordPlugin, wordRemove, argv[2]}
	case len(argv) == 4 && argv[0] == wordPlugin && argv[1] == wordMarketplace && argv[2] == wordRm:
		return []string{wordPlugin, wordMarketplace, wordRemove, argv[3]}
	default:
		return argv
	}
}

// marketplaceInUse is the §4.8 refcount of one marketplace: it is kept while
// the oracle lists a plugin from it, and kept when the oracle cannot tell —
// removing a marketplace another plugin needs is worse than leaving it.
func (h *codex) marketplaceInUse(ctx context.Context, marketplace string) (string, bool) {
	listed, err := h.Oracle().List(ctx)
	if err != nil {
		return fmt.Sprintf("codex plugin list failed (%v); marketplace %s kept", err, marketplace), true
	}

	serving := 0

	for _, entry := range listed {
		if entry.Marketplace == marketplace {
			serving++
		}
	}

	if serving > 0 {
		return fmt.Sprintf("marketplace %s still serves %d installed plugin(s); kept", marketplace, serving), true
	}

	return "", false
}

// codexOracle is the Codex CLI oracle: list JSON, never an LLM.
type codexOracle struct {
	base *Base
}

// List implements Oracle: `codex plugin list --json` reports
// {installed: [...], available: [...]}; only installed plugins count.
func (o *codexOracle) List(ctx context.Context) ([]Installed, error) {
	out, err := o.base.run(ctx, wordCodex, []string{wordPlugin, wordList, flagJSON})
	if err != nil {
		return nil, err
	}

	listed, parseErr := parseInstalled(out)
	if parseErr != nil {
		return nil, &OracleError{Host: string(Codex), Output: string(out), Cause: parseErr}
	}

	return listed, nil
}

// Validate implements Oracle: codex-cli 0.157.1 has no plugin validate
// subcommand (the verified grammar is `plugin add|list|remove` and
// `plugin marketplace add|list|remove`), so validation is not supported by
// this host and no CLI call is made. The contract is pinned by
// TestCodexOracleValidate: a caller must treat ErrNotSupported as "this host
// cannot validate", never as "valid".
func (o *codexOracle) Validate(context.Context, string) ([]string, error) {
	return nil, ErrNotSupported
}

// codexConfigDir resolves the Codex config dir: CODEX_HOME (profiles) or
// <home>/.codex.
func codexConfigDir(home string) string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		absolute, err := filepath.Abs(dir)
		if err == nil {
			return absolute
		}

		return dir
	}

	return filepath.Join(home, ".codex")
}
