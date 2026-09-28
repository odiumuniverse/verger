package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
)

// Gemini CLI argv words and surface names (Gemini CLI 0.61.0, verified live:
// docs/reviews/T1.7-T1.8-grammar.probe.log).
const (
	wordGemini       = "gemini"
	wordExtensions   = "extensions"
	wordLink         = "link"
	flagOutputFormat = "--output-format"
	wordJSON         = "json"
	geminiDirName    = ".gemini"
	// flagConsent acknowledges the install risk without a prompt; without it
	// link/install ask for workspace trust and hang a non-interactive run.
	flagConsent = "--consent"
)

// gemini is the Gemini CLI adapter.
type gemini struct {
	base *Base
}

// NewGemini builds the Gemini CLI adapter; the adapter is immutable after
// construction and safe for concurrent use.
func NewGemini(opts ...Option) Host {
	return &gemini{base: newBase(opts)}
}

// ID implements Host.
func (h *gemini) ID() ID {
	return Gemini
}

// Oracle implements Host.
func (h *gemini) Oracle() Oracle {
	return &geminiOracle{base: h.base}
}

// Detect reports whether the Gemini config dir exists or the `gemini` binary
// resolves; profiles stay separate instances through the home dir.
func (h *gemini) Detect(home string) bool {
	if isDir(geminiConfigDir(h.base.effectiveHome(home))) {
		return true
	}

	_, err := h.base.resolve(wordGemini)

	return err == nil
}

// Deliver implements Host with explicit strategies; the shared dispatch checks
// the source ref before any stratum, so a host-forbidden source is never
// delivered — not even as files.
func (h *gemini) Deliver(ctx context.Context, home string, d Delivery) (Result, error) {
	return deliverByStrategy(ctx, h.base, Gemini, home, d, h.deliverInstall, h.deliverLoose)
}

// deliverLoose plans and executes the host-native user files: skills, agents,
// commands, rules and the shared settings.json carrying hooks and mcpServers.
func (h *gemini) deliverLoose(ctx context.Context, home string, d Delivery) (Result, error) {
	userHome := h.base.effectiveHome(home)
	dir := geminiConfigDir(userHome)
	settings := filepath.Join(dir, "settings.json")

	spec := looseSpec{
		host:          Gemini,
		binary:        wordGemini,
		home:          userHome,
		skillsDir:     filepath.Join(dir, "skills"),
		agentsDir:     filepath.Join(dir, "agents"),
		commandsDir:   filepath.Join(dir, "commands"),
		settingsPath:  settings,
		hooksPath:     settings,
		hooksFormat:   manifest.FormatGemini,
		commandExt:    tomlExt,
		renderAgent:   func(agent render.Agent, _ string) ([]byte, error) { return agent.GeminiMarkdown(), nil },
		renderCommand: func(cmd render.Command) ([]byte, error) { return cmd.GeminiTOML() },
		variables:     map[string]string{"extensionPath": d.Package.Root},
		mcpConfig: &mcpConfigSpec{
			path:   settings,
			format: manifest.FormatGemini,
			edit:   render.EditJSONC,
		},
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

// geminiInstall is one native or synth extension install.
type geminiInstall struct {
	argv    []string // install or link argv; nil when the host already links the source
	replace bool     // synth: uninstall the version the host links now, first
	existed bool     // the extension existed before this delivery
	name    string   // the extension name the host lists
	source  string   // synth: the dir the host links
	overlay string   // synth: a renamed copy of the synth dir (<owner>-<name>)
	rma     []receipt.Op
}

// geminiExtensionPath is the receipt identity of one synth extension; an
// artifact at this path proves a later delivery that the name is verger's.
func geminiExtensionPath(name string) string {
	return "gemini://extension/" + name
}

// deliverInstall runs the native or synth extension installer and verifies via
// the oracle; verify failure is a *DeliveryError, never a silent step down.
// The command-source policy is checked before any host call.
func (h *gemini) deliverInstall(ctx context.Context, home string, d Delivery, synth bool) (Result, error) {
	userHome := h.base.effectiveHome(home)

	if err := checkCommandSources(Gemini, userHome, d.Package.Marketplace); err != nil {
		return Result{}, err
	}

	plan, err := h.installPlan(ctx, d, synth)
	if err != nil {
		return Result{}, err
	}

	if d.DryRun {
		return Result{Strategy: d.Strategy, RMA: slices.Clone(plan.rma), Notes: []string{noteDryRun}}, nil
	}

	artifacts, observed, err := h.install(ctx, d.Package, plan)

	// The result is what execution produced even on error (NF-5).
	return Result{Strategy: d.Strategy, Artifacts: artifacts, RMA: slices.Clone(plan.rma), Observed: observed}, err
}

// install stages the renamed copy, replaces a previously linked version,
// links or installs, and verifies through the oracle; a synth extension is
// recorded as an artifact that proves its name on the next delivery.
func (h *gemini) install(ctx context.Context, pkg Package, plan geminiInstall) ([]receipt.Artifact, OracleResult, error) {
	if plan.overlay != "" {
		if err := stageRenamedExtension(ctx, pkg.SynthDir, plan.overlay, plan.name); err != nil {
			return nil, OracleResult{}, &DeliveryError{Host: string(Gemini), Package: pkg.ID, Step: stepInstall, Cause: err}
		}
	}

	// gemini refuses to link over an installed name ("already installed").
	if plan.replace {
		if _, err := h.base.run(ctx, wordGemini, []string{wordExtensions, wordUninstall, plan.name}); err != nil {
			return nil, OracleResult{}, err
		}
	}

	if plan.argv != nil {
		if _, err := h.base.run(ctx, wordGemini, plan.argv); err != nil {
			return nil, OracleResult{}, err
		}
	}

	listed, err := h.Oracle().List(ctx)
	if err != nil {
		return nil, OracleResult{}, err
	}

	if !listedContains(listed, []string{plan.name}) {
		return nil, OracleResult{}, &DeliveryError{
			Host:    string(Gemini),
			Package: pkg.ID,
			Step:    stepVerify,
			Cause:   fmt.Errorf("%s is not listed by the oracle", plan.name),
		}
	}

	observed := OracleResult{Listed: listed, Verified: true}

	if plan.source == "" {
		return nil, observed, nil
	}

	sum, err := digest.Tree(plan.source)
	if err != nil {
		return nil, observed, &DeliveryError{Host: string(Gemini), Package: pkg.ID, Step: stepVerify, Cause: err}
	}

	return []receipt.Artifact{{Kind: "extension", Name: plan.name, Path: geminiExtensionPath(plan.name), Digest: sum}}, observed, nil
}

// installPlan resolves the install argv of one delivery; synth links the
// absolute store dir (the store never moves, DESIGN §3.1).
func (h *gemini) installPlan(ctx context.Context, d Delivery, synth bool) (geminiInstall, error) {
	plan := geminiInstall{}

	if synth {
		var err error

		if plan, err = h.synthPlan(ctx, d.Package); err != nil {
			return geminiInstall{}, err
		}
	} else {
		_, name := splitID(d.Package.ID)
		if name == "" {
			return geminiInstall{}, &NotSupportedError{Host: Gemini, Operation: unsupportedNoName}
		}

		if d.Package.Marketplace == "" {
			return geminiInstall{}, &NotSupportedError{Host: Gemini, Operation: unsupportedNoMarketplace}
		}

		plan.name = name
		plan.argv = []string{wordExtensions, wordInstallCLI, d.Package.Marketplace, flagConsent}
	}

	plan.rma = []receipt.Op{
		{Kind: receipt.OpHostInstall, Command: []string{wordExtensions, wordUninstall, plan.name}, Existed: plan.existed},
	}

	return plan, nil
}

// synthPlan names the synth extension (decision F3): the plugin name, unless
// the host already has a foreign extension of that name — then
// <owner>-<name>, linked from a renamed copy of the synth dir, since the store
// dir is shared by every host. A name is verger's when this package's receipt
// records it or the host links it from the owner's store dir. Only the
// read-only list runs.
func (h *gemini) synthPlan(ctx context.Context, pkg Package) (geminiInstall, error) {
	layout, err := newSynthLayout(Gemini, pkg)
	if err != nil {
		return geminiInstall{}, err
	}

	source, err := filepath.Abs(pkg.SynthDir)
	if err != nil {
		return geminiInstall{}, &DeliveryError{Host: string(Gemini), Package: pkg.ID, Step: stepPlan, Cause: err}
	}

	listed, err := h.Oracle().List(ctx)
	if err != nil {
		return geminiInstall{}, err
	}

	plan := geminiInstall{name: layout.identity.Name, source: source}

	if h.foreignExtension(listed, pkg, layout, plan.name) {
		plan.name = layout.identity.Owner + "-" + layout.identity.Name
		plan.overlay = source + "+gemini"
		plan.source = plan.overlay

		if h.foreignExtension(listed, pkg, layout, plan.name) {
			return geminiInstall{}, &render.HandsOffError{
				Path: geminiExtensionPath(layout.identity.Name), KeyPath: layout.identity.Name,
				Reason: "the host already has foreign extensions named " + layout.identity.Name + " and " + plan.name,
			}
		}
	}

	// An extension of this name is verger's here (not foreign): the same dir
	// linked again is a no-op, another version is replaced.
	if index := slices.IndexFunc(listed, func(entry Installed) bool { return entry.Name == plan.name }); index >= 0 {
		plan.existed = true
		plan.replace = !samePath(listed[index].Path, plan.source)
	}

	if !plan.existed || plan.replace {
		plan.argv = []string{wordExtensions, wordLink, plan.source, flagConsent}
	}

	return plan, nil
}

// foreignExtension reports whether the host lists an extension of that name
// verger cannot prove its own.
func (h *gemini) foreignExtension(listed []Installed, pkg Package, layout synthLayout, name string) bool {
	if h.base.ownership != nil {
		if owner, owned := h.base.ownership.Owner(geminiExtensionPath(name)); owned && owner == pkg.ID {
			return false
		}
	}

	root := resolvedPath(layout.root) + string(filepath.Separator)

	for _, entry := range listed {
		if entry.Name != name {
			continue
		}

		return entry.Path == "" || !strings.HasPrefix(resolvedPath(entry.Path)+string(filepath.Separator), root)
	}

	return false
}

// stageRenamedExtension copies the synth dir to overlay and renames the
// extension in its gemini-extension.json; the copy is derived, so it is
// rebuilt on every delivery.
func stageRenamedExtension(ctx context.Context, synthDir, overlay, name string) error {
	if err := os.RemoveAll(overlay); err != nil {
		return err
	}

	if err := writeTree(ctx, synthDir, overlay); err != nil {
		return err
	}

	file := filepath.Join(overlay, "gemini-extension.json")

	data, err := os.ReadFile(file) //nolint:gosec // G304: the manifest of a store synth copy
	if err != nil {
		return err
	}

	doc := map[string]any{}

	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("decode %s: %w", file, err)
	}

	doc["name"] = name

	renamed, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}

	return fsutil.WriteFileAtomic(file, append(renamed, '\n'), 0o600)
}

// Uninstall runs the host-install RMA ops in reverse order; file/tree ops are
// executed by pkg/apply. An extension that existed before the delivery (a
// re-delivered own extension) is never removed by its inverse, and one the
// host no longer knows is already removed.
func (h *gemini) Uninstall(ctx context.Context, _ string, r receipt.Receipt) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	notes, err := hostInverses(ctx, r, h.uninstallOp)
	if err != nil {
		return Result{}, err
	}

	return Result{Strategy: Strategy(r.Strategy), Notes: notes}, nil
}

// uninstallOp runs one host-install inverse. A malformed op (no argv) is a
// receipt defect, reported rather than indexed into a panic.
func (h *gemini) uninstallOp(ctx context.Context, op receipt.Op) (string, error) {
	if len(op.Command) == 0 {
		return "", fmt.Errorf("gemini: host-install op carries no argv (%+v)", op)
	}

	name := op.Command[len(op.Command)-1]

	if op.Existed {
		return "extension " + name + " existed before this delivery; kept", nil
	}

	_, err := h.base.run(ctx, wordGemini, op.Command)
	if err == nil {
		return "", nil
	}

	if exit, refused := errors.AsType[*hostcli.ExitError](err); refused && strings.Contains(exit.Stderr, "Extension not found") {
		return "extension " + name + " was not installed; nothing to uninstall", nil
	}

	return "", err
}

// geminiOracle is the Gemini CLI oracle: list/validate JSON, never an LLM.
type geminiOracle struct {
	base *Base
}

// List implements Oracle. Gemini CLI 0.61.0 prints `extensions list`, JSON
// included, to stderr and nothing to stdout (live-verified), so stderr is read
// when stdout is empty.
func (o *geminiOracle) List(ctx context.Context) ([]Installed, error) {
	stdout, stderr, err := o.base.runStreams(ctx, wordGemini, []string{wordExtensions, wordList, flagOutputFormat, wordJSON})
	if err != nil {
		return nil, err
	}

	out := streamOutput(stdout, stderr)

	listed, parseErr := parseInstalled(out)
	if parseErr != nil {
		return nil, &OracleError{Host: string(Gemini), Output: string(out), Cause: parseErr}
	}

	return listed, nil
}

// streamOutput is what one host call reported: stdout, or stderr when the CLI
// prints its result there (Gemini CLI 0.61.0 prints every `extensions` result,
// JSON included, to stderr and leaves stdout empty).
func streamOutput(stdout, stderr []byte) []byte {
	if len(bytes.TrimSpace(stdout)) == 0 {
		return stderr
	}

	return stdout
}

// Validate implements Oracle: the CLI subcommand when the host supports it,
// else the manifest parser fallback (the CLI surface is unverified —
// OQ-T1.8.1). Gemini CLI 0.61.0 prints the validate result to stderr, like
// `extensions list`, so stderr is read when stdout is empty.
func (o *geminiOracle) Validate(ctx context.Context, dir string) ([]string, error) {
	stdout, stderr, err := o.base.runStreams(ctx, wordGemini, []string{wordExtensions, wordValidate, dir})
	if err != nil {
		if errors.Is(err, hostcli.ErrNotFound) || isUnknownSubcommand(err) {
			return validateManifest(dir)
		}

		output := strings.TrimSpace(string(streamOutput(stdout, stderr)))

		if exit, ok := errors.AsType[*hostcli.ExitError](err); ok && output == "" {
			output = exit.Stderr
		}

		return nil, &OracleError{Host: string(Gemini), Output: output, Cause: err}
	}

	return warningLines(streamOutput(stdout, stderr)), nil
}

// validateManifest is the offline validate fallback: parse the extension
// manifest and return its warnings.
func validateManifest(dir string) ([]string, error) {
	pkg, err := manifest.ParseAny(dir)
	if err != nil {
		return nil, &OracleError{Host: string(Gemini), Output: dir, Cause: err}
	}

	return slices.Clone(pkg.Warnings), nil
}

// geminiConfigDir resolves the Gemini config dir: <home>/.gemini.
func geminiConfigDir(home string) string {
	return filepath.Join(home, geminiDirName)
}
