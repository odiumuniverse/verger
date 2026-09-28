package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
)

// Pi surface names and argv words, verified live against pi 0.74.2
// (@earendil-works/pi-coding-agent, docs/reviews/W1-B-impl-1.md §Live probe):
//
//	pi install <source> [-l]   Install a package and add it to settings
//	pi remove  <source> [-l]   Remove a package and its source from settings
//	pi list                    List installed packages from user and project settings
//
// `pi --help` lists exactly these five commands; there is no validate verb,
// and an unknown word is not a usage error but a prompt, so verger only ever
// runs the three words above.
const (
	wordPi = "pi"
	// pi's own argv words are the shared ones: install, remove, list.
	piConfigDir  = ".pi"
	piAgentDir   = "agent"
	piSkillsDir  = "skills"
	piPromptsDir = "prompts"
	// piAgentDirEnv relocates the whole agent dir (pi 0.74.2 dist/cli.js:
	// getAgentDir() reads it and expands a leading `~`, else ~/.pi/agent).
	// oh-my-pi honours the same variable for its own `.omp` tree (DESIGN §3.1),
	// so a relocated pi home is followed here as the host follows it.
	piAgentDirEnv = "PI_CODING_AGENT_DIR"
	// piMCPDoc is the Pi-owned MCP override file. pi itself has no MCP at all
	// ("No MCP. Build CLI tools with READMEs (see Skills), or build an
	// extension that adds MCP support", README §Philosophy); only the
	// third-party pi-mcp-adapter reads this file, so a delivery names it.
	piMCPDoc = "mcp.json"
	// piMCPNote is the gate beadle prints on the same surface
	// (pkg/agent/agents.go: "Pi has no built-in MCP; the servers work only
	// with the third-party pi-mcp-adapter (pi install npm:pi-mcp-adapter)").
	piMCPNote = "pi has no built-in MCP; the servers in " + piMCPDoc + " are read only by the third-party " +
		"pi-mcp-adapter (pi install npm:pi-mcp-adapter)"
	// piHooksBlocked is the delivery note of the hook component: pi 0.74.2 has
	// no hook surface of any kind — its extension API is code, and a
	// declarative document would be a file the host never opens.
	piHooksBlocked = "pi has no hook surface: hooks reach pi only as extension code, a dialect verger does not render"
)

// pi is the Pi coding agent adapter. Native and synth both go through the
// host's own installer — `pi install <path>` adds the package to the agent
// settings by path, without copying it, which is exactly what a synth package
// in the store needs (DESIGN §3.1) — and the oracle is the host's own `pi
// list`. The loose rung covers everything pi reads from user files.
type pi struct {
	base *Base
}

// NewPi builds the Pi adapter; the adapter is immutable after construction and
// safe for concurrent use.
func NewPi(opts ...Option) Host {
	return &pi{base: newBase(opts)}
}

// ID implements Host.
func (h *pi) ID() ID {
	return Pi
}

// Oracle implements Host.
func (h *pi) Oracle() Oracle {
	return &piOracle{base: h.base}
}

// Detect reports whether the Pi agent dir exists or the `pi` binary resolves;
// a relocated agent dir is followed through PI_CODING_AGENT_DIR.
func (h *pi) Detect(home string) bool {
	if isDir(piAgentDirOf(h.base.effectiveHome(home))) {
		return true
	}

	_, err := h.base.resolve(wordPi)

	return err == nil
}

// Deliver implements Host with explicit strategies; the shared dispatch checks
// the source ref before any stratum, so a host-forbidden source is never
// delivered — not even as files.
func (h *pi) Deliver(ctx context.Context, home string, d Delivery) (Result, error) {
	return deliverByStrategy(ctx, h.base, Pi, home, d, h.deliverInstall, h.deliverLoose)
}

// deliverLoose plans and executes the host-native user files: skills, prompt
// templates, the rule skill wrapper and the MCP override document.
func (h *pi) deliverLoose(ctx context.Context, home string, d Delivery) (Result, error) {
	userHome := h.base.effectiveHome(home)
	spec := piSpec(userHome)

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

	if len(d.Package.MCP) > 0 {
		result.Notes = append(result.Notes, piMCPNote)
	}

	result.Notes = append(result.Notes, piRuleWrapperNote(d.Package)...)

	return result, err
}

// piSpec is the loose surface of the Pi adapter: skills below the agent dir,
// prompt templates below `prompts/`, and the MCP override document. Pi reads
// no hook document and has no subagents, so both are skipped with a reason; a
// rule component keeps the D23 skill wrapper, which the delivery says out loud.
func piSpec(userHome string) looseSpec {
	agent := piAgentDirOf(userHome)

	return looseSpec{
		host:         Pi,
		binary:       wordPi,
		home:         userHome,
		skillsDir:    filepath.Join(agent, piSkillsDir),
		commandsDir:  filepath.Join(agent, piPromptsDir),
		settingsPath: filepath.Join(agent, piMCPDoc),
		hooksBlocked: piHooksBlocked,
		mcpConfig: &mcpConfigSpec{
			path:    filepath.Join(agent, piMCPDoc),
			format:  manifest.FormatClaude,
			edit:    render.EditJSONC,
			entries: piMCPEntries,
		},
	}
}

// piAgentDirOf resolves the Pi agent dir of one user home: PI_CODING_AGENT_DIR
// when it carries a value (pi 0.74.2 expands a leading `~` in it), else
// ~/.pi/agent. No XDG and no OS branch: pi resolves the same way on linux and
// macOS.
func piAgentDirOf(userHome string) string {
	if dir := strings.TrimSpace(os.Getenv(piAgentDirEnv)); dir != "" {
		return expandPiTilde(dir, userHome)
	}

	return filepath.Join(userHome, piConfigDir, piAgentDir)
}

// expandPiTilde expands a leading `~` the way the host's own expandTildePath
// does; every other value is taken literally, so a relative path stays
// relative and resolves from the process working directory.
func expandPiTilde(dir, userHome string) string {
	if dir == "~" {
		return userHome
	}

	if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		return filepath.Join(userHome, filepath.FromSlash(rest))
	}

	return dir
}

// piPackagePath is the receipt identity of one installed pi package; an
// artifact at this path proves a later delivery that the path is verger's.
func piPackagePath(id string) string {
	return "pi://package/" + id
}

// deliverInstall runs the host installer and verifies through the oracle; a
// verify failure is a *DeliveryError, never a silent step down.
func (h *pi) deliverInstall(ctx context.Context, home string, d Delivery, _ bool) (Result, error) {
	userHome := h.base.effectiveHome(home)

	if err := checkCommandSources(Pi, userHome, d.Package.Marketplace); err != nil {
		return Result{}, err
	}

	dir := d.Package.SynthDir
	if dir == "" {
		return Result{}, &NotSupportedError{
			Host: Pi,
			Operation: "native install (pi install registers a package directory or a source spec; " +
				"verger has no pi package directory for this source, deliver it loose)",
		}
	}

	if !filepath.IsAbs(dir) {
		return Result{}, &NotSupportedError{
			Host:      Pi,
			Operation: "native install (pi resolves a relative source against the settings file, so the package dir must be absolute)",
		}
	}

	plan := piInstall{dir: dir}
	plan.rma = []receipt.Op{
		{Kind: receipt.OpHostInstall, Command: []string{wordRemove, dir}, Existed: plan.existed},
	}

	if d.DryRun {
		return Result{Strategy: d.Strategy, RMA: slices.Clone(plan.rma), Notes: []string{noteDryRun}}, nil
	}

	artifacts, observed, err := h.install(ctx, d.Package, plan)

	// The result is what execution produced even on error (NF-5).
	return Result{Strategy: d.Strategy, Artifacts: artifacts, RMA: slices.Clone(plan.rma), Observed: observed}, err
}

// piInstall is one native install of a package directory.
type piInstall struct {
	dir     string
	existed bool
	rma     []receipt.Op
}

// install runs the host installer and verifies the package through the
// oracle. The install is idempotent in pi (settings records the path), so a
// re-delivery simply rewrites the same entry.
func (h *pi) install(ctx context.Context, pkg Package, plan piInstall) ([]receipt.Artifact, OracleResult, error) {
	if _, err := h.base.run(ctx, wordPi, []string{wordInstallCLI, plan.dir}); err != nil {
		return nil, OracleResult{}, err
	}

	listed, err := h.Oracle().List(ctx)
	if err != nil {
		return nil, OracleResult{}, err
	}

	if !listedPath(listed, plan.dir) {
		return nil, OracleResult{}, &DeliveryError{
			Host: string(Pi), Package: pkg.ID, Step: stepVerify,
			Cause: fmt.Errorf("%s is not listed by `pi list`", plan.dir),
		}
	}

	sum, err := digest.Tree(plan.dir)
	if err != nil {
		return nil, OracleResult{Listed: listed, Verified: true},
			&DeliveryError{Host: string(Pi), Package: pkg.ID, Step: stepVerify, Cause: err}
	}

	return []receipt.Artifact{
		{Kind: "package", Name: pkg.ID, Path: piPackagePath(pkg.ID), Digest: sum},
	}, OracleResult{Listed: listed, Verified: true}, nil
}

// Uninstall runs the host-install RMA ops in reverse order; file/tree ops are
// executed by pkg/apply. A package the host no longer lists is already gone.
func (h *pi) Uninstall(ctx context.Context, _ string, r receipt.Receipt) (Result, error) {
	notes, err := hostInverses(ctx, r, h.uninstallOp)
	if err != nil {
		return Result{}, err
	}

	return Result{Notes: notes}, nil
}

// uninstallOp runs one host-install inverse. A malformed op (no source path) is
// a receipt defect, reported rather than indexed into a panic; a package the
// host no longer lists is already removed, so no call is made.
// uninstallOp runs one host-install inverse of a pi receipt.
func (h *pi) uninstallOp(ctx context.Context, op receipt.Op) (string, error) {
	if len(op.Command) < 2 || op.Command[0] != wordRemove {
		return "", &DeliveryError{Host: string(Pi), Step: stepUninstall, Cause: fmt.Errorf("malformed pi receipt op %v", op.Command)}
	}

	dir := op.Command[1]

	listed, err := h.Oracle().List(ctx)
	if err != nil {
		return "", err
	}

	if !listedPath(listed, dir) {
		return "pi package " + dir + " is not listed by the host; nothing to remove", nil
	}

	if _, err := h.base.run(ctx, wordPi, []string{wordRemove, dir}); err != nil {
		return "", err
	}

	return "", nil
}

// piOracle is the Pi oracle: the host's own `pi list` listing, never an LLM.
type piOracle struct {
	base *Base
}

// List implements Oracle: `pi list` prints one section header per scope and,
// under each, the source as the settings file records it followed by the
// resolved absolute path on a deeper line (live-verified for 0.74.2). A host
// with nothing installed answers one sentence, which carries no path.
func (o *piOracle) List(ctx context.Context) ([]Installed, error) {
	out, err := o.base.run(ctx, wordPi, []string{wordList})
	if err != nil {
		return nil, err
	}

	return parsePiList(out), nil
}

// Validate implements Oracle: pi 0.74.2 has no validate verb (`pi --help`
// lists install, remove, uninstall, update, list and config), so validation is
// not supported by this host and no CLI call is made.
func (o *piOracle) Validate(context.Context, string) ([]string, error) {
	return nil, ErrNotSupported
}

// parsePiList reads the `pi list` listing. A section header carries no colon
// and no path; the source line is indented one level, the resolved path two,
// so only the deeper line names a package the host has.
func parsePiList(out []byte) []Installed {
	var (
		listed []Installed
		source string
	)

	for line := range strings.Lines(string(out)) {
		text := strings.TrimRight(line, "\r\n")

		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			continue
		}

		indent := len(text) - len(strings.TrimLeft(text, " \t"))

		switch {
		case trimmed == "No packages installed.":
			source = ""

			continue
		case indent <= 2:
			source = trimmed

			continue
		case source == "":
			continue
		default:
			listed = append(listed, Installed{Name: trimmed, Path: trimmed, Scope: piListScope(source)})
			source = ""
		}
	}

	return listed
}

// piListScope names the scope section a resolved path was listed under.
func piListScope(section string) string {
	if strings.EqualFold(section, "Project packages:") {
		return "project"
	}

	return "user"
}

// listedPath reports whether the host lists one package directory.
func listedPath(listed []Installed, dir string) bool {
	return slices.ContainsFunc(listed, func(entry Installed) bool {
		return filepath.Clean(entry.Path) == filepath.Clean(dir)
	})
}

// piRuleWrapperNote names the D23 rule wrapper pi gets: its own rules surface is
// the singleton `<agentDir>/AGENTS.md`, which carries no per-rule document, so a
// rule package lands as a skill instead of being merged into an instruction file
// verger would have to own whole.
func piRuleWrapperNote(pkg Package) []string {
	if !slices.ContainsFunc(pkg.Components, func(component manifest.Component) bool {
		return component.Kind == manifest.KindRule
	}) {
		return nil
	}

	return []string{"a rule is delivered as a skill wrapper (rule-<name> below " +
		filepath.Join(piConfigDir, piAgentDir, piSkillsDir) + "); pi's own rules surface is the singleton AGENTS.md"}
}
