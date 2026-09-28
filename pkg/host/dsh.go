package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
)

// DeepSeek Harness surface names and argv words. No dsh binary is installed on
// the reference machine, so everything here comes from beadle, which resolved
// the paths and the loader schema against a live dsh 0.1.5-rc.3 (decision A-44,
// evidence docs/pending/q15-evidence and q16-evidence):
//
//	$DSH_HOME        non-empty value taken literally, else ~/.dsh
//	$DSH_AGENTS_HOME non-empty value taken literally, else ~/.agents
//	<dshHome>/skills        skills (rank 400, wins over the shared copy)
//	<agentsHome>/skills     skills, read-only and shadowed by the own copy
//	<dshHome>/cordis.patch.yml  the home MCP patch layer
//	<dshHome>/AGENTS.md     user instructions (one singleton document)
//
// No XDG and no OS branch: beadle resolves the same paths on linux and macOS,
// so verger does too (DESIGN product rule 3).
const (
	wordDSH          = "dsh"
	dshHomeEnv       = "DSH_HOME"
	dshAgentsHomeEnv = "DSH_AGENTS_HOME"
	dshDirName       = ".dsh"
	dshAgentsDirName = ".agents"
	dshSkillsDir     = "skills"
	dshPatchFile     = "cordis.patch.yml"
	// dshA41Note is beadle's documented no-go for DSH subagents, permissions
	// and slash commands (decision A-41,
	// beadle docs/RESEARCH-DEEPSEEK-HARNESS.md §A-41): in DSH these are
	// runtime state and code, not files, so there is nothing a package
	// manager could sync. Revisit on a dsh release above 0.2.
	dshA41Note = "dsh subagents, permissions and slash-commands are runtime state and code, not files; " +
		"documented no-go A-41 (revisit on a dsh release above 0.2)"
	// dshHooksBlocked is the delivery note of the hook component: dsh reaches
	// hooks through the `dsh-hooks-claude-code` plugin's config string, read
	// once at load from a preset's cordis.yml. There is no hook document in
	// the user's home to write, and writing one would be a file the host
	// never opens.
	dshHooksBlocked = "dsh has no hook document: hooks reach it only through the dsh-hooks-claude-code plugin's config string in a profile preset, a dialect verger does not render"
	// dshNoInstaller is the refusal of the CLI-driven strata: `dsh plugin
	// --profile <name> <pnpm args>` is a pnpm passthrough into the profile
	// directory, not a package registry with names verger could claim, and the
	// plugin farm it manages is pnpm's own lock (A-41, decision Q-16).
	dshNoInstaller = "dsh plugin is a pnpm passthrough into the profile directory, not a package " +
		"installer verger can drive; dsh has no native package registry, deliver it loose"
)

// dsh is the DeepSeek Harness adapter. It is loose-only: the harness has no
// package manager, and its only user-editable surfaces are the skills dir, the
// rules singleton and the home MCP patch layer. Subagents, permissions and
// slash commands are a documented no-go (A-41).
type dsh struct {
	base *Base
}

// NewDSH builds the DeepSeek Harness adapter; the adapter is immutable after
// construction and safe for concurrent use.
func NewDSH(opts ...Option) Host {
	return &dsh{base: newBase(opts)}
}

// ID implements Host.
func (h *dsh) ID() ID {
	return DSH
}

// Oracle implements Host.
func (h *dsh) Oracle() Oracle {
	return &dshOracle{base: h.base}
}

// Detect reports whether DSH is present: its home directory exists or the dsh
// binary is on PATH (beadle pkg/agent/dsh.go).
func (h *dsh) Detect(home string) bool {
	if isDir(dshHome(h.base.effectiveHome(home))) {
		return true
	}

	_, err := h.base.resolve(wordDSH)

	return err == nil
}

// Deliver implements Host: the harness has no installer, so native and synth
// are refused with the reason instead of pretending to deliver; loose is the
// only stratum.
func (h *dsh) Deliver(ctx context.Context, home string, d Delivery) (Result, error) {
	return deliverByStrategy(ctx, h.base, DSH, home, d, h.deliverInstall, h.deliverLoose)
}

// deliverInstall refuses the CLI-driven strata.
func (h *dsh) deliverInstall(_ context.Context, _ string, _ Delivery, synth bool) (Result, error) {
	strategy := "native"
	if synth {
		strategy = "synth"
	}

	return Result{}, &NotSupportedError{
		Host:      DSH,
		Operation: strategy + " delivery (" + dshNoInstaller + ")",
	}
}

// deliverLoose plans and executes the host-native user files: skills and the
// rule wrapper in the skills dir, and the MCP servers in the home patch layer.
func (h *dsh) deliverLoose(ctx context.Context, home string, d Delivery) (Result, error) {
	userHome := h.base.effectiveHome(home)
	spec := dshSpec(userHome)

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

	if dshCarriesA41(d.Package) {
		result.Notes = append(result.Notes, dshA41Note)
	}

	result.Notes = append(result.Notes, dshRuleWrapperNote(d.Package)...)

	return result, err
}

// dshCarriesA41 reports whether the package carries a component dsh takes as
// runtime state or code rather than as a file.
func dshCarriesA41(pkg Package) bool {
	return slices.ContainsFunc(pkg.Components, func(component manifest.Component) bool {
		return component.Kind == manifest.KindAgent || component.Kind == manifest.KindCommand
	})
}

// dshSpec is the loose surface of the DSH adapter: skills and the rule wrapper
// below $DSH_HOME/skills, MCP servers in the home patch layer. DSH has no
// subagent dir, no slash-command dir and no hook document, so those components
// are skipped with their reason. The shared agents home is deliberately not a
// surface: dsh reads it at rank 500, shadowed by its own dir, and every other
// harness writes there too — beadle treats it as shared and read-only.
func dshSpec(userHome string) looseSpec {
	dir := dshHome(userHome)

	return looseSpec{
		host:         DSH,
		binary:       wordDSH,
		home:         userHome,
		skillsDir:    filepath.Join(dir, dshSkillsDir),
		hooksBlocked: dshHooksBlocked,
		mcpConfig: &mcpConfigSpec{
			path:            filepath.Join(dir, dshPatchFile),
			format:          manifest.FormatClaude,
			edit:            dshPatchEdit,
			entries:         dshMCPEntries,
			member:          dshPatchMember,
			wholeFile:       true,
			recordPath:      dshRecordPath,
			recordArtifacts: dshRecordArtifacts,
		},
	}
}

// dshHome resolves the harness home: a non-empty $DSH_HOME is taken literally
// (no trim, no `~` expansion, a relative path resolves from the working
// directory — beadle pkg/agent/dsh.go), else ~/.dsh.
func dshHome(userHome string) string {
	if dir := strings.TrimSpace(os.Getenv(dshHomeEnv)); dir != "" {
		return dir
	}

	return filepath.Join(userHome, dshDirName)
}

// Uninstall implements Host: DSH records no host-install op (there is no
// installer), so the file and tree ops of the receipt are all there is.
func (h *dsh) Uninstall(ctx context.Context, _ string, r receipt.Receipt) (Result, error) {
	notes, err := hostInverses(ctx, r, func(_ context.Context, op receipt.Op) (string, error) {
		return "", &DeliveryError{
			Host: string(DSH), Step: stepUninstall,
			Cause: fmt.Errorf("malformed dsh receipt op %v", op.Command),
		}
	})
	if err != nil {
		return Result{}, err
	}

	return Result{Notes: notes}, nil
}

// dshOracle is the DeepSeek Harness oracle. The harness has no listing verb —
// `dsh plugin` is a pnpm passthrough — so both calls report that the host
// cannot answer rather than shelling out to something that is not an installer.
type dshOracle struct {
	base *Base
}

// List implements Oracle.
func (o *dshOracle) List(context.Context) ([]Installed, error) {
	return nil, ErrNotSupported
}

// Validate implements Oracle.
func (o *dshOracle) Validate(context.Context, string) ([]string, error) {
	return nil, ErrNotSupported
}

// dshRuleWrapperNote names the D23 rule wrapper DSH gets: its user instructions
// are the singleton `<dshHome>/AGENTS.md`, which carries no per-rule document.
func dshRuleWrapperNote(pkg Package) []string {
	if !slices.ContainsFunc(pkg.Components, func(component manifest.Component) bool {
		return component.Kind == manifest.KindRule
	}) {
		return nil
	}

	return []string{"a rule is delivered as a skill wrapper (rule-<name> below " +
		filepath.Join(dshDirName, dshSkillsDir) + "); dsh's own rules surface is the singleton AGENTS.md"}
}
