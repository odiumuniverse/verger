package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
)

// Codex surface names (DESIGN §4.2): the shared ~/.agents root and the
// personal marketplace document.
const (
	wordCodex           = "codex"
	codexAgentsDir      = ".agents"
	codexPluginsDir     = "plugins"
	codexMarketplaceDoc = "marketplace.json"
)

// codex is the Codex adapter.
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

// codexInstall is one native or synth host install.
type codexInstall struct {
	addRef           string // native: the source ref; synth: the owner marketplace root
	plugin           string
	installID        string // <plugin>@<marketplace>
	marketplace      string
	hostOps          []receipt.Op // inverse host-install argv
	marketplaceEntry *loosePlan   // synth: the personal marketplace entry write
	synth            *synthLayout // synth: the owner marketplace (decision F3)
}

// rma composes the reverse manifest: the personal marketplace entry ops, then
// the host-install ops. Execution records the trash bucket of a replaced entry
// in the entry plan, so the result must be composed after it runs — a copy
// taken earlier loses the Backup id.
func (p codexInstall) rma() []receipt.Op {
	var ops []receipt.Op

	if p.marketplaceEntry != nil {
		ops = slices.Clone(p.marketplaceEntry.ops)
	}

	return append(ops, p.hostOps...)
}

// artifacts reports what the adapter writes itself: the personal marketplace
// entry of a synth install (the host installer owns the rest).
func (p codexInstall) artifacts() []receipt.Artifact {
	if p.marketplaceEntry == nil {
		return nil
	}

	return slices.Clone(p.marketplaceEntry.artifacts)
}

// deliverInstall runs the native or synth host installer and verifies via the
// oracle; verify failure is a *DeliveryError, never a silent step down. The
// command-source policy is checked before anything is planned.
func (h *codex) deliverInstall(ctx context.Context, home string, d Delivery, synth bool) (Result, error) {
	userHome := h.base.effectiveHome(home)

	if err := checkCommandSources(Codex, userHome, d.Package.Marketplace); err != nil {
		return Result{}, err
	}

	plan, err := h.installPlan(userHome, d, synth)
	if err != nil {
		return Result{}, err
	}

	if d.DryRun {
		return Result{Strategy: d.Strategy, Artifacts: plan.artifacts(), RMA: plan.rma(), Notes: []string{"dry-run"}}, nil
	}

	if plan.synth != nil {
		if _, err := plan.synth.writeDoc(ctx, plan.marketplace); err != nil {
			return Result{}, &DeliveryError{Host: string(Codex), Package: d.Package.ID, Step: stepInstall, Cause: err}
		}
	}

	observed, err := h.install(ctx, userHome, d, plan)

	// Once the entry step ran, the result is what execution produced even on
	// error: the entry op carries the bucket of the replaced value, which the
	// dry-run RMA cannot know (NF-5).
	result := Result{Strategy: d.Strategy, Artifacts: plan.artifacts(), RMA: plan.rma(), Observed: observed}

	return result, err
}

// install writes the personal marketplace entry, registers the marketplace,
// installs the plugin and verifies it through the oracle.
func (h *codex) install(ctx context.Context, userHome string, d Delivery, plan codexInstall) (OracleResult, error) {
	if plan.marketplaceEntry != nil {
		spec := codexSpec(userHome, codexConfigDir(userHome))

		if err := h.base.executeLoose(ctx, spec, d.Package, plan.marketplaceEntry); err != nil {
			return OracleResult{}, err
		}
	}

	if _, err := h.base.run(ctx, wordCodex, []string{wordPlugin, wordMarketplace, wordAdd, plan.addRef}); err != nil {
		return OracleResult{}, err
	}

	if _, err := h.base.run(ctx, wordCodex, []string{wordPlugin, wordInstallCLI, plan.installID}); err != nil {
		return OracleResult{}, err
	}

	listed, err := h.Oracle().List(ctx)
	if err != nil {
		return OracleResult{}, err
	}

	if !listedContains(listed, []string{plan.plugin, plan.installID}) {
		return OracleResult{}, &DeliveryError{
			Host:    string(Codex),
			Package: d.Package.ID,
			Step:    stepVerify,
			Cause:   fmt.Errorf("%s is not listed by the oracle", plan.installID),
		}
	}

	return OracleResult{Listed: listed, Verified: true}, nil
}

// installPlan resolves the marketplace ref, the install id and the RMA of one
// delivery; synth also plans the personal marketplace entry.
func (h *codex) installPlan(userHome string, d Delivery, synth bool) (codexInstall, error) {
	plan := codexInstall{}

	var err error

	if synth {
		err = h.planSynthInstall(userHome, d.Package, &plan)
	} else {
		err = planNativeInstall(d.Package, &plan)
	}

	if err != nil {
		return codexInstall{}, err
	}

	plan.installID = plan.plugin + "@" + plan.marketplace
	plan.hostOps = []receipt.Op{
		{Kind: receipt.OpHostInstall, Command: []string{wordPlugin, wordMarketplace, wordRm, plan.marketplace}},
		{Kind: receipt.OpHostInstall, Command: []string{wordPlugin, wordUninstall, plan.installID}},
	}

	return plan, nil
}

// planSynthInstall places the synth package in its owner marketplace
// (decision F3): the owner root <store>/synth/<owner> is the marketplace,
// named as the owner document already says (else the owner), and the plugin
// is the bare name; the personal marketplace entry points at the package dir.
// The Codex marketplace listing is unverified (OQ-T1.7.1), so the host is not
// probed for a foreign marketplace of that name.
func (h *codex) planSynthInstall(userHome string, pkg Package, plan *codexInstall) error {
	layout, err := newSynthLayout(Codex, pkg)
	if err != nil {
		return err
	}

	current, entries, ok, err := layout.readDoc()
	if err != nil {
		return &DeliveryError{Host: string(Codex), Package: pkg.ID, Step: stepPlan, Cause: err}
	}

	if err := layout.checkClaim(entries); err != nil {
		return &DeliveryError{Host: string(Codex), Package: pkg.ID, Step: stepPlan, Cause: err}
	}

	plan.addRef = layout.root
	plan.plugin = layout.identity.Name
	plan.marketplace = layout.identity.Owner
	plan.synth = &layout

	if ok {
		plan.marketplace = current
	}

	entry, err := h.marketplaceEntryPlan(userHome, pkg, plan.plugin)
	if err != nil {
		return err
	}

	plan.marketplaceEntry = entry

	return nil
}

// planNativeInstall fills the native marketplace ref.
func planNativeInstall(pkg Package, plan *codexInstall) error {
	_, name := splitID(pkg.ID)
	if name == "" {
		return &NotSupportedError{Host: Codex, Operation: "install without a package name"}
	}

	if pkg.Marketplace == "" {
		return &NotSupportedError{Host: Codex, Operation: "native install without a marketplace"}
	}

	plan.addRef = pkg.Marketplace
	plan.plugin = name
	plan.marketplace = marketplaceName(pkg.Marketplace)

	return nil
}

// marketplaceEntryPlan plans the synth entry in the personal marketplace
// document (`~/.agents/plugins/marketplace.json`): the owned key
// `plugins.<name>` points at the store dir, foreign entries are preserved.
func (h *codex) marketplaceEntryPlan(userHome string, pkg Package, name string) (*loosePlan, error) {
	file := codexMarketplacePath(userHome)

	existing, err := readOptionalFile(file)
	if err != nil {
		return nil, &DeliveryError{Host: string(Codex), Package: pkg.ID, Step: stepPlan, Cause: err}
	}

	keyPath := "plugins." + name
	value := map[string]any{"source": map[string]any{"source": "local", "path": pkg.SynthDir}}

	owned := render.Owned{}

	if current, ok, memberErr := configMember(existing, keyPath, false); memberErr != nil {
		return nil, &DeliveryError{Host: string(Codex), Package: pkg.ID, Step: stepPlan, Cause: memberErr}
	} else if ok {
		owned[keyPath] = canonicalValueDigest(current)
	}

	out, changes, err := render.EditJSONC(existing, []render.Edit{{Path: keyPath, Value: value}}, owned)
	if err != nil {
		return nil, err
	}

	plan := &loosePlan{}

	if len(changes) == 0 {
		return plan, nil
	}

	change := changes[0]

	plan.addConfig(
		looseStep{kind: stepConfig, path: file, data: out, mode: configMode(file), digest: change.Digest},
		[]receipt.Artifact{{Kind: "marketplace", Name: name, Path: file, Digest: change.Digest}},
		[]receipt.Op{{Kind: receipt.OpConfigKey, Path: file, KeyPath: keyPath, Digest: change.Digest, Existed: change.Existed}},
		[]any{change.Previous},
	)

	return plan, nil
}

// Uninstall runs the host-install RMA ops in reverse order; file/tree ops are
// executed by pkg/apply. A failing `codex plugin uninstall` falls back to
// removing the owned personal-marketplace entry recorded in the receipt.
func (h *codex) Uninstall(ctx context.Context, home string, r receipt.Receipt) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	userHome := h.base.effectiveHome(home)

	var (
		notes    []string
		fellBack bool
	)

	for _, op := range slices.Backward(r.RMA) {
		if op.Kind != receipt.OpHostInstall {
			continue
		}

		var (
			note string
			err  error
		)

		fellBack, note, err = h.uninstallStep(ctx, userHome, r, op, fellBack)
		if err != nil {
			return Result{}, err
		}

		if note != "" {
			notes = append(notes, note)
		}
	}

	return Result{Strategy: Strategy(r.Strategy), Notes: notes}, nil
}

// uninstallStep runs one host-install op; a failing plugin uninstall falls
// back to removing the owned marketplace entry.
func (h *codex) uninstallStep(
	ctx context.Context, userHome string, r receipt.Receipt, op receipt.Op, fellBack bool,
) (bool, string, error) {
	if isMarketplaceRemove(op.Command) {
		if note, keep := h.marketplaceInUse(ctx, op.Command[3]); keep {
			return fellBack, note, nil
		}
	}

	_, err := h.base.run(ctx, wordCodex, op.Command)
	if err == nil {
		return fellBack, "", nil
	}

	if isUninstallCommand(op.Command) {
		removed, fbErr := h.removeMarketplaceEntry(userHome, r)
		if fbErr != nil {
			return fellBack, "", fbErr
		}

		if !removed {
			return fellBack, "", err
		}

		return true, fmt.Sprintf("codex plugin uninstall failed (%v); removed the owned marketplace entry instead", err), nil
	}

	if fellBack {
		return fellBack, fmt.Sprintf("codex plugin marketplace rm failed during fallback: %v", err), nil
	}

	return fellBack, "", err
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

// isUninstallCommand reports whether argv is the `plugin uninstall <id>` op.
func isUninstallCommand(argv []string) bool {
	return len(argv) == 3 && argv[0] == wordPlugin && argv[1] == wordUninstall
}

// removeMarketplaceEntry removes the owned `plugins.<name>` key recorded in the
// receipt; a hand-edited entry is hands-off and the error surfaces. ok reports
// whether an entry op existed at all.
func (h *codex) removeMarketplaceEntry(userHome string, r receipt.Receipt) (bool, error) {
	file := codexMarketplacePath(userHome)

	for _, op := range r.RMA {
		if op.Kind != receipt.OpConfigKey || op.Path != file || !strings.HasPrefix(op.KeyPath, "plugins.") {
			continue
		}

		existing, err := readOptionalFile(file)
		if err != nil {
			return false, &DeliveryError{Host: string(Codex), Package: r.Package, Step: stepInstall, Cause: err}
		}

		if len(existing) == 0 {
			return true, nil
		}

		owned := render.Owned{op.KeyPath: op.Digest}

		out, changes, err := render.EditJSONC(existing, []render.Edit{{Path: op.KeyPath, Delete: true}}, owned)
		if err != nil {
			return false, err
		}

		if len(changes) == 0 {
			return true, nil
		}

		if err := fsutil.WriteFileAtomic(file, out, configMode(file)); err != nil {
			return false, &DeliveryError{Host: string(Codex), Package: r.Package, Step: stepInstall, Cause: err}
		}

		return true, nil
	}

	return false, nil
}

// codexOracle is the Codex CLI oracle: list/validate JSON, never an LLM.
type codexOracle struct {
	base *Base
}

// List implements Oracle.
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

// Validate implements Oracle. The subcommand grammar is unverified
// (OQ-T1.7.1): a missing binary or an unknown-subcommand failure reports
// ErrNotSupported; a real validation failure is an *OracleError.
func (o *codexOracle) Validate(ctx context.Context, dir string) ([]string, error) {
	out, err := o.base.run(ctx, wordCodex, []string{wordPlugin, wordValidate, dir})
	if err != nil {
		if errors.Is(err, hostcli.ErrNotFound) || isUnknownSubcommand(err) {
			return nil, ErrNotSupported
		}

		output := strings.TrimSpace(string(out))

		if exit, ok := errors.AsType[*hostcli.ExitError](err); ok && output == "" {
			output = exit.Stderr
		}

		return nil, &OracleError{Host: string(Codex), Output: output, Cause: err}
	}

	return warningLines(out), nil
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

// codexMarketplacePath resolves the personal marketplace document.
func codexMarketplacePath(home string) string {
	return filepath.Join(home, codexAgentsDir, codexPluginsDir, codexMarketplaceDoc)
}
