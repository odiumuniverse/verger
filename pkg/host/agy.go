package host

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
)

// Antigravity CLI argv words and surface names. No binary is installed on the
// reference machine, so the argv comes from beadle, which drives the real CLI
// (pkg/engine/bundles.go: `agy plugin install <dir>`, `agy plugin enable
// <name>`; uninstall `agy plugin uninstall <name>`; probe `agy plugin list`),
// and the paths from the same repo's plugin reader and agent surfaces
// (pkg/plugin/source_antigravity.go, pkg/agent/agents.go). DESIGN §4.2 names
// the same three verbs. The adapter stays `experimental` until a live run.
const (
	wordAgy = "agy"
	// `agy plugin` reuses the shared argv words plugin/install/uninstall.
	agyMCPConfigFile = "mcp_config.json"
	agyStateDir      = "antigravity-cli"
	agyPluginsDir    = "plugins"
	agyAgentsDir     = "agents"
	agySharedHome    = ".agents"
	// agyHooksBlocked is the delivery note of the hook component: an
	// Antigravity hooks document is owner-keyed — the top-level keys are plugin
	// names and each owner maps events to handlers, with no `hooks` wrapper
	// (beadle pkg/bundle/render_hooks.go). Writing the nested Claude shape
	// there would be a document the host does not read.
	agyHooksBlocked = "antigravity hooks.json is owner-keyed (top-level plugin names, no hooks wrapper), a dialect verger does not render yet"
)

// agyPluginManifest is the plugin manifest beadle's Antigravity reader parses:
// name, version, description, and nothing else — the schema forbids extra
// properties (pkg/bundle/bundle.go addAntigravityPlugin).
type agyPluginManifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

// agy is the Antigravity CLI adapter. Native and synth share the host's own
// `agy plugin install <dir>` — a plugin is a directory with a plugin.json, so a
// synth package in the store installs by path — and the oracle is the state the
// host keeps itself: the plugin directories of its customization roots, which
// is what `agy plugin list` reports and what its own `ln -s` hint tells a user
// to create (beadle pkg/engine/bundles.go).
type agy struct {
	base *Base
}

// NewAgy builds the Antigravity CLI adapter; the adapter is immutable after
// construction and safe for concurrent use.
func NewAgy(opts ...Option) Host {
	return &agy{base: newBase(opts)}
}

// ID implements Host.
func (h *agy) ID() ID {
	return Agy
}

// Oracle implements Host.
func (h *agy) Oracle() Oracle {
	return &agyOracle{base: h.base}
}

// Detect reports whether the Antigravity CLI is present: its state dir or its
// MCP config exists (beadle pkg/agent/agents.go — the file is shared with the
// IDE and 2.0), or the `agy` binary resolves.
func (h *agy) Detect(home string) bool {
	userHome := h.base.effectiveHome(home)

	if isDir(filepath.Join(agyGeminiDir(userHome), agyStateDir)) || isFile(filepath.Join(agyConfigDir(userHome), agyMCPConfigFile)) {
		return true
	}

	_, err := h.base.resolve(wordAgy)

	return err == nil
}

// Deliver implements Host with explicit strategies; the shared dispatch checks
// the source ref before any stratum, so a host-forbidden source is never
// delivered — not even as files.
func (h *agy) Deliver(ctx context.Context, home string, d Delivery) (Result, error) {
	return deliverByStrategy(ctx, h.base, Agy, home, d, h.deliverInstall, h.deliverLoose)
}

// deliverLoose plans and executes the host-native user files: subagents below
// ~/.gemini/config/agents, skills in the shared Agent Skills directory, the
// rule skill wrapper and the MCP document.
func (h *agy) deliverLoose(ctx context.Context, home string, d Delivery) (Result, error) {
	userHome := h.base.effectiveHome(home)
	spec := agySpec(userHome)

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
	result.Notes = append(result.Notes, agyRuleWrapperNote(d.Package)...)

	return result, err
}

// agySpec is the loose surface of the Antigravity adapter: subagents below the
// config dir, skills in the shared agents home every Agent Skills harness
// reads, and MCP servers in the config's mcp_config.json under the host's own
// `serverUrl` spelling. Antigravity has no slash-command surface and its hooks
// document is owner-keyed, so both are skipped with a reason; a rule component
// keeps the D23 skill wrapper, which the delivery says out loud.
func agySpec(userHome string) looseSpec {
	shared := filepath.Join(userHome, agySharedHome, piSkillsDir)

	return looseSpec{
		host:         Agy,
		binary:       wordAgy,
		home:         userHome,
		skillsDir:    shared,
		agentsDir:    filepath.Join(agyConfigDir(userHome), agyAgentsDir),
		hooksBlocked: agyHooksBlocked,
		mcpConfig: &mcpConfigSpec{
			path:    filepath.Join(agyConfigDir(userHome), agyMCPConfigFile),
			format:  manifest.FormatClaude,
			edit:    render.EditJSONC,
			entries: agyMCPEntries,
		},
	}
}

// agyGeminiDir is ~/.gemini, shared with the Gemini CLI adapter; Antigravity
// keeps its own subtrees below it.
func agyGeminiDir(userHome string) string {
	return filepath.Join(userHome, geminiDirName)
}

// agyConfigDir is ~/.gemini/config, the dir every Antigravity user surface
// below hangs from.
func agyConfigDir(userHome string) string {
	return filepath.Join(agyGeminiDir(userHome), "config")
}

// agyPluginRoots are the plugin customization roots the host reads, in the
// precedence order beadle's reader uses: ~/.gemini/antigravity-cli/plugins,
// ~/.gemini/config/plugins, ~/.agents/plugins. The first root that provides a
// name wins, later copies are the same plugin.
func agyPluginRoots(userHome string) []string {
	return []string{
		filepath.Join(agyGeminiDir(userHome), agyStateDir, agyPluginsDir),
		filepath.Join(agyConfigDir(userHome), agyPluginsDir),
		filepath.Join(userHome, agySharedHome, agyPluginsDir),
	}
}

// agyPluginPath is the receipt identity of one installed plugin; an artifact at
// this path proves a later delivery that the name is verger's.
func agyPluginPath(name string) string {
	return "agy://plugin/" + name
}

// deliverInstall runs the host installer and verifies through the oracle; a
// verify failure is a *DeliveryError, never a silent step down. The
// command-source policy is checked before any host call.
func (h *agy) deliverInstall(ctx context.Context, home string, d Delivery, _ bool) (Result, error) {
	userHome := h.base.effectiveHome(home)

	if err := checkCommandSources(Agy, userHome, d.Package.Marketplace); err != nil {
		return Result{}, err
	}

	name, err := agyPluginName(d.Package)
	if err != nil {
		return Result{}, err
	}

	dir := d.Package.SynthDir
	if dir == "" || !filepath.IsAbs(dir) {
		return Result{}, &NotSupportedError{
			Host: Agy,
			Operation: "native install (agy plugin install registers a plugin directory on disk; " +
				"verger has no plugin directory for this source, deliver it loose)",
		}
	}

	plan := agyInstall{name: name, dir: dir}
	plan.rma = []receipt.Op{
		{Kind: receipt.OpHostInstall, Command: []string{wordPlugin, wordUninstall, name}},
	}

	if d.DryRun {
		return Result{Strategy: d.Strategy, RMA: slices.Clone(plan.rma), Notes: []string{noteDryRun}}, nil
	}

	artifacts, observed, err := h.install(ctx, d.Package, plan)

	// The result is what execution produced even on error (NF-5).
	return Result{Strategy: d.Strategy, Artifacts: artifacts, RMA: slices.Clone(plan.rma), Observed: observed}, err
}

// agyPluginName is the name the host lists: the plugin name of the package
// identity. A synth plugin carries its own plugin.json, so the manifest name is
// the truth when the dir has one.
func agyPluginName(pkg Package) (string, error) {
	if pkg.SynthDir != "" {
		if manifest, err := readAgyManifest(pkg.SynthDir); err == nil && manifest.Name != "" {
			return manifest.Name, nil
		}
	}

	_, name := splitID(pkg.ID)
	if name == "" {
		return "", &NotSupportedError{Host: Agy, Operation: unsupportedNoName}
	}

	return name, nil
}

// agyInstall is one native or synth plugin install.
type agyInstall struct {
	name string
	dir  string
	rma  []receipt.Op
}

// install runs the host installer and verifies the plugin through the oracle:
// the host links the plugin into one of its customization roots, which is what
// its own `agy plugin list` enumerates.
func (h *agy) install(ctx context.Context, pkg Package, plan agyInstall) ([]receipt.Artifact, OracleResult, error) {
	if _, err := h.base.run(ctx, wordAgy, []string{wordPlugin, wordInstallCLI, plan.dir}); err != nil {
		return nil, OracleResult{}, err
	}

	listed, err := h.Oracle().List(ctx)
	if err != nil {
		return nil, OracleResult{}, err
	}

	if !listedContains(listed, []string{plan.name}) {
		return nil, OracleResult{}, &DeliveryError{
			Host: string(Agy), Package: pkg.ID, Step: stepVerify,
			Cause: fmt.Errorf("plugin %s is not in any Antigravity customization root", plan.name),
		}
	}

	sum, err := digest.Tree(plan.dir)
	if err != nil {
		return nil, OracleResult{Listed: listed, Verified: true},
			&DeliveryError{Host: string(Agy), Package: pkg.ID, Step: stepVerify, Cause: err}
	}

	return []receipt.Artifact{
		{Kind: "plugin", Name: plan.name, Path: agyPluginPath(plan.name), Digest: sum},
	}, OracleResult{Listed: listed, Verified: true}, nil
}

// Uninstall runs the host-install RMA ops in reverse order; file/tree ops are
// executed by pkg/apply. A plugin the host no longer links is already removed.
func (h *agy) Uninstall(ctx context.Context, home string, r receipt.Receipt) (Result, error) {
	userHome := h.base.effectiveHome(home)

	listed, listErr := h.Oracle().List(ctx)

	notes, err := hostInverses(ctx, r, func(ctx context.Context, op receipt.Op) (string, error) {
		name, ok := agyUninstallName(op)
		if !ok {
			return "", &DeliveryError{
				Host: string(Agy), Step: stepUninstall,
				Cause: fmt.Errorf("malformed agy receipt op %v", op.Command),
			}
		}

		// The host lists no state for a plugin, so a link that is gone is the
		// only evidence left that the plugin is not there any more (NF-2).
		if listErr == nil && !listedContains(listed, []string{name}) {
			return "antigravity plugin " + name + " is not linked any more; nothing to remove", nil
		}

		if _, err := h.base.run(ctx, wordAgy, []string{wordPlugin, wordUninstall, name}); err != nil {
			return "", err
		}

		return "", nil
	})

	_ = userHome

	if err != nil {
		return Result{}, err
	}

	return Result{Notes: notes}, nil
}

// agyUninstallName extracts the plugin name from one host-install op.
func agyUninstallName(op receipt.Op) (string, bool) {
	if len(op.Command) < 3 || op.Command[0] != wordPlugin || op.Command[1] != wordUninstall {
		return "", false
	}

	return op.Command[2], true
}

// agyOracle is the Antigravity oracle: the plugin directories of the
// customization roots, which is the state `agy plugin list` reports and the
// host's own documentation tells a user to create by hand. It never starts the
// host and never consults an LLM.
type agyOracle struct {
	base *Base
}

// List implements Oracle: every plugin root is scanned, a directory without a
// plugin.json is not a plugin and is skipped silently, and the first root that
// provides a name wins.
func (o *agyOracle) List(_ context.Context) ([]Installed, error) {
	seen := map[string]bool{}

	var listed []Installed

	for _, root := range agyPluginRoots(o.base.effectiveHome("")) {
		entries, err := readAgyPlugins(root)
		if err != nil {
			return nil, err
		}

		for _, entry := range entries {
			if seen[entry.Name] {
				continue
			}

			seen[entry.Name] = true

			listed = append(listed, entry)
		}
	}

	slices.SortFunc(listed, func(a, b Installed) int { return cmp.Compare(a.Name, b.Name) })

	return listed, nil
}

// Validate implements Oracle: no agy validation verb is verified, so a synth
// directory is validated by parsing its plugin manifest — the offline fallback
// the Gemini adapter uses for the same reason.
func (o *agyOracle) Validate(_ context.Context, dir string) ([]string, error) {
	return validateManifest(dir)
}

// readAgyPlugins lists the plugins of one customization root.
func readAgyPlugins(root string) ([]Installed, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("read antigravity plugin root %s: %w", root, err)
	}

	var listed []Installed

	for _, entry := range entries {
		// The host links a plugin into a root, so a customization entry is a
		// symlink as often as a directory.
		installPath := filepath.Join(root, entry.Name())

		info, statErr := os.Stat(installPath)
		if statErr != nil || !info.IsDir() {
			continue
		}

		manifest, err := readAgyManifest(installPath)
		if err != nil {
			// A directory without a plugin.json is not a plugin (beadle
			// pkg/plugin/source_antigravity.go).
			continue
		}

		listed = append(listed, Installed{
			Name:    manifest.Name,
			Version: manifest.Version,
			Path:    installPath,
			Scope:   "user",
			Enabled: true,
		})
	}

	return listed, nil
}

// readAgyManifest reads one plugin.json; a missing or invalid one is an error
// the caller turns into "not a plugin".
func readAgyManifest(dir string) (agyPluginManifest, error) {
	var manifest agyPluginManifest

	data, err := os.ReadFile(filepath.Join(dir, "plugin.json")) //nolint:gosec // G304: the manifest is read below a host plugin dir
	if err != nil {
		return manifest, err
	}

	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, err
	}

	if manifest.Name == "" {
		return manifest, errors.New("plugin.json carries no name")
	}

	return manifest, nil
}

// agyRuleWrapperNote names the D23 rule wrapper Antigravity gets: its
// instructions are read through the Gemini adapter's `~/.gemini/GEMINI.md`, so
// a rule package lands as a skill instead of being merged into a document
// verger does not own for this host.
func agyRuleWrapperNote(pkg Package) []string {
	if !slices.ContainsFunc(pkg.Components, func(component manifest.Component) bool {
		return component.Kind == manifest.KindRule
	}) {
		return nil
	}

	return []string{"a rule is delivered as a skill wrapper (rule-<name> below " +
		filepath.Join(agySharedHome, piSkillsDir) + "); antigravity reads its instructions through the Gemini adapter's GEMINI.md"}
}
