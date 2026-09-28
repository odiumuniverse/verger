package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/home"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/pack"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/store"
)

// claudeCLI is a stateful fake of the Claude Code 2.1.283 plugin CLI. Its
// output shapes and refusal texts are the ones captured live for F3/F7
// (docs/reviews/F3-F7-fix-claude.md): marketplaces register under the name
// their marketplace.json declares, installs resolve the plugin through that
// document, and an uninstall leaves the plugin cache behind, as the real
// CLI does.
type claudeCLI struct {
	mu           sync.Mutex
	configDir    string
	marketplaces map[string]string // name → dir
	installed    map[string]string // plugin@marketplace → version
	servers      map[string]string // MCP server name → the add argv that configured it
	fail         map[string]hostcli.Response
	calls        []string
}

// newClaudeCLI builds the fake over one Claude config dir.
func newClaudeCLI(configDir string) *claudeCLI {
	return &claudeCLI{
		configDir:    configDir,
		marketplaces: map[string]string{},
		installed:    map[string]string{},
		servers:      map[string]string{},
		fail:         map[string]hostcli.Response{},
	}
}

// Server reports the add argv of a configured MCP server.
func (c *claudeCLI) Server(name string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	argv, ok := c.servers[name]

	return argv, ok
}

// mcp runs one `claude mcp get|add|remove`, refusing like the real CLI: an add
// of a configured name and a get or remove of an unknown one.
func (c *claudeCLI) mcp(args []string) ([]byte, error) {
	name := args[len(args)-1]

	switch args[1] {
	case "get":
		if _, ok := c.servers[name]; !ok {
			return nil, refused(1, "No MCP server found with name: %s", name)
		}

		return []byte(name + ":\n  Scope: User config\n"), nil
	case "add":
		name = args[6]
		if _, ok := c.servers[name]; ok {
			return nil, refused(1, "MCP server %s already exists in user config", name)
		}

		c.servers[name] = strings.Join(args, " ")

		return []byte("Added MCP server " + name), nil
	case "remove":
		if _, ok := c.servers[name]; !ok {
			return nil, refused(1, "No MCP server found with name: %s", name)
		}

		delete(c.servers, name)

		return []byte("Removed MCP server " + name), nil
	default:
		return nil, refused(127, "unknown mcp verb %s", args[1])
	}
}

// Run implements hostcli.Runner.
func (c *claudeCLI) Run(_ context.Context, bin hostcli.Binary, args []string, _ []byte) ([]byte, error) {
	key := strings.Join(args, " ")

	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls = append(c.calls, bin.Name+" "+key)

	if resp, ok := c.fail[key]; ok {
		return resp.Stdout, &hostcli.ExitError{Name: bin.Name, Code: resp.Code, Stderr: resp.Stderr}
	}

	switch {
	case len(args) >= 3 && args[0] == "mcp":
		return c.mcp(args)
	case key == "plugin marketplace list --json":
		return c.marketplaceList()
	case key == "plugin list --json":
		return c.pluginList()
	case len(args) == 4 && args[0] == "plugin" && args[1] == "marketplace":
		return c.marketplace(args[2], args[3])
	case len(args) == 3 && args[0] == "plugin":
		return c.plugin(args[1], args[2])
	default:
		return nil, refused(127, "unscripted: %s", key)
	}
}

// Calls returns the recorded calls as `<name> <joined args>` keys.
func (c *claudeCLI) Calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.calls)
}

// Registered reports the dir of a registered marketplace.
func (c *claudeCLI) Registered(name string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	dir, ok := c.marketplaces[name]

	return dir, ok
}

// Installed reports the version of an installed plugin id.
func (c *claudeCLI) Installed(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	version, ok := c.installed[id]

	return version, ok
}

// refused is a non-zero exit of the fake CLI.
func refused(code int, format string, args ...any) error {
	return &hostcli.ExitError{Name: "claude", Code: code, Stderr: fmt.Sprintf(format, args...)}
}

// marketplaceList is `claude plugin marketplace list --json`.
func (c *claudeCLI) marketplaceList() ([]byte, error) {
	out := []map[string]string{}

	for _, name := range slices.Sorted(maps.Keys(c.marketplaces)) {
		dir := c.marketplaces[name]
		out = append(out, map[string]string{"name": name, "source": "directory", "path": dir, "installLocation": dir})
	}

	return json.Marshal(out)
}

// pluginList is `claude plugin list --json`.
func (c *claudeCLI) pluginList() ([]byte, error) {
	out := []map[string]any{}

	for _, id := range slices.Sorted(maps.Keys(c.installed)) {
		plugin, marketplace := splitTestID(id)
		out = append(out, map[string]any{
			"id": id, "version": c.installed[id], "scope": "user", "enabled": true,
			"installPath": filepath.Join(c.configDir, "plugins", "cache", marketplace, plugin, c.installed[id]),
		})
	}

	return json.Marshal(out)
}

// marketplace runs one `claude plugin marketplace <verb> <arg>`.
func (c *claudeCLI) marketplace(verb, arg string) ([]byte, error) {
	switch verb {
	case "add":
		name, _, err := readTestMarketplace(arg)
		if err != nil {
			return nil, refused(1, "✘ Failed to add marketplace: %v", err)
		}

		if dir, ok := c.marketplaces[name]; ok && dir != arg {
			return nil, refused(1, "✘ Failed to add marketplace: Marketplace '%s' already exists", name)
		}

		c.marketplaces[name] = arg

		return []byte("✔ Successfully added marketplace: " + name), nil
	case "update":
		if _, ok := c.marketplaces[arg]; !ok {
			return nil, refused(1, "✘ Failed to update marketplace: Marketplace '%s' not found", arg)
		}

		return []byte("✔ Successfully updated marketplace: " + arg), nil
	case "rm", "remove":
		if _, ok := c.marketplaces[arg]; !ok {
			return nil, refused(1, "✘ Failed to remove marketplace: Marketplace '%s' not found", arg)
		}

		delete(c.marketplaces, arg)

		return []byte("✔ Successfully removed marketplace: " + arg), nil
	default:
		return nil, refused(127, "unknown marketplace verb %s", verb)
	}
}

// plugin runs one `claude plugin <verb> <id>`.
func (c *claudeCLI) plugin(verb, id string) ([]byte, error) {
	switch verb {
	case "install", "update":
		if verb == "update" {
			if _, ok := c.installed[id]; !ok {
				return nil, refused(1, "✘ Failed to update plugin %q: Plugin %q is not installed", id, id)
			}
		}

		version, err := c.resolve(id)
		if err != nil {
			return nil, err
		}

		c.installed[id] = version

		plugin, marketplace := splitTestID(id)
		cache := filepath.Join(c.configDir, "plugins", "cache", marketplace, plugin, version, ".claude-plugin")

		if err := os.MkdirAll(cache, 0o700); err != nil {
			return nil, err
		}

		return []byte("✔ Successfully installed plugin: " + id), os.WriteFile(filepath.Join(cache, "plugin.json"), []byte(`{"name":"`+plugin+`"}`), 0o600)
	case "uninstall":
		if _, ok := c.installed[id]; !ok {
			return nil, refused(1, "✘ Failed to uninstall plugin %q: Plugin %q not found in installed plugins", id, id)
		}

		delete(c.installed, id)

		return []byte("✔ Successfully uninstalled plugin: " + id), nil
	default:
		return nil, refused(127, "unknown plugin verb %s", verb)
	}
}

// resolve finds the version a registered marketplace serves for a plugin id.
func (c *claudeCLI) resolve(id string) (string, error) {
	plugin, marketplace := splitTestID(id)

	dir, ok := c.marketplaces[marketplace]
	if !ok {
		return "", refused(1, "✘ Failed to install plugin %q: Marketplace %q not found", id, marketplace)
	}

	_, sources, err := readTestMarketplace(dir)
	if err != nil {
		return "", refused(1, "✘ Failed to install plugin %q: %v", id, err)
	}

	source, ok := sources[plugin]
	if !ok {
		return "", refused(1, "✘ Failed to install plugin %q: Plugin %q not found in marketplace %q", id, plugin, marketplace)
	}

	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(source), ".claude-plugin", "plugin.json")) //nolint:gosec // G304: the fake reads its own temp store
	if err != nil {
		return "", refused(1, "✘ Failed to install plugin %q: %v", id, err)
	}

	var manifest struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}

	if err := json.Unmarshal(data, &manifest); err != nil || manifest.Name != plugin {
		return "", refused(1, "✘ Failed to install plugin %q: plugin.json names %q", id, manifest.Name)
	}

	return manifest.Version, nil
}

// readTestMarketplace reads a dir marketplace: its name and plugin sources.
func readTestMarketplace(dir string) (string, map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "marketplace.json")) //nolint:gosec // G304: the fake reads its own temp store
	if err != nil {
		return "", nil, err
	}

	var doc struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"plugins"`
	}

	if err := json.Unmarshal(data, &doc); err != nil {
		return "", nil, err
	}

	sources := map[string]string{}

	for _, plugin := range doc.Plugins {
		sources[plugin.Name] = plugin.Source
	}

	return doc.Name, sources, nil
}

// splitTestID splits a plugin id at the last @.
func splitTestID(id string) (string, string) {
	index := strings.LastIndexByte(id, '@')
	if index < 0 {
		return id, ""
	}

	return id[:index], id[index+1:]
}

// codexCLI is a stateful fake of the codex-cli 0.157.1 plugin grammar with
// the JSON shapes and refusal texts captured live
// (docs/reviews/T1.7-T1.8-grammar.probe.log): marketplaces register under the
// name their document declares, `plugin add` resolves through it (and moves
// an installed plugin to the marketplace's version), `plugin remove` is
// idempotent.
type codexCLI struct {
	mu           sync.Mutex
	marketplaces map[string]string // name → root
	installed    map[string]string // plugin@marketplace → version
	fail         map[string]hostcli.Response
	calls        []string
	stale        bool // a re-add keeps the installed version (a stale host cache)
}

// newCodexCLI builds an empty fake.
func newCodexCLI() *codexCLI {
	return &codexCLI{marketplaces: map[string]string{}, installed: map[string]string{}, fail: map[string]hostcli.Response{}}
}

// Run implements hostcli.Runner.
//
//nolint:dupl // every stateful fake has the same record-then-dispatch shell; the grammars differ below it
func (c *codexCLI) Run(_ context.Context, bin hostcli.Binary, args []string, _ []byte) ([]byte, error) {
	key := strings.Join(args, " ")

	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls = append(c.calls, bin.Name+" "+key)

	if resp, ok := c.fail[key]; ok {
		return resp.Stdout, &hostcli.ExitError{Name: bin.Name, Code: resp.Code, Stderr: resp.Stderr}
	}

	return c.dispatch(key, args)
}

// dispatch routes one recorded call to the fake verb. --json is dropped first:
// the fake answers the same body for it and does not parse flags.
func (c *codexCLI) dispatch(key string, args []string) ([]byte, error) {
	bare := slices.DeleteFunc(slices.Clone(args), func(arg string) bool { return arg == "--json" })

	switch {
	case len(bare) == 3 && bare[0] == "plugin" && bare[1] == "marketplace" && bare[2] == "list":
		return c.marketplaceList()
	case len(bare) == 4 && bare[0] == "plugin" && bare[1] == "marketplace":
		return c.marketplace(bare[2], bare[3])
	case len(bare) >= 2 && bare[0] == "plugin":
		return c.dispatchPlugin(bare, key)
	default:
		return nil, codexRefused(2, "error: unrecognized subcommand '%s'", key)
	}
}

// dispatchPlugin routes `plugin list` and `plugin <verb> <id>`; the caller has
// already established the `plugin` root and a second argument.
func (c *codexCLI) dispatchPlugin(bare []string, key string) ([]byte, error) {
	switch {
	case len(bare) == 2 && bare[1] == "list":
		return c.pluginList()
	case len(bare) == 3:
		return c.plugin(bare[1], bare[2])
	default:
		return nil, codexRefused(2, "error: unrecognized subcommand '%s'", key)
	}
}

// Calls returns the recorded calls as `<name> <joined args>` keys.
func (c *codexCLI) Calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.calls)
}

// Registered reports the root of a registered marketplace.
func (c *codexCLI) Registered(name string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	root, ok := c.marketplaces[name]

	return root, ok
}

// Installed reports the version of an installed plugin selector.
func (c *codexCLI) Installed(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	version, ok := c.installed[id]

	return version, ok
}

// codexRefused is a non-zero exit of the fake codex CLI.
func codexRefused(code int, format string, args ...any) error {
	return &hostcli.ExitError{Name: "codex", Code: code, Stderr: fmt.Sprintf(format, args...)}
}

// marketplaceList is `codex plugin marketplace list --json`.
func (c *codexCLI) marketplaceList() ([]byte, error) {
	out := []map[string]any{}

	for _, name := range slices.Sorted(maps.Keys(c.marketplaces)) {
		root := c.marketplaces[name]
		out = append(out, map[string]any{
			"name": name, "root": root,
			"marketplaceSource": map[string]string{"sourceType": "local", "source": root},
		})
	}

	return json.Marshal(map[string]any{"marketplaces": out})
}

// pluginList is `codex plugin list --json`.
func (c *codexCLI) pluginList() ([]byte, error) {
	out := []map[string]any{}

	for _, id := range slices.Sorted(maps.Keys(c.installed)) {
		plugin, marketplace := splitTestID(id)
		out = append(out, map[string]any{
			"pluginId": id, "name": plugin, "marketplaceName": marketplace, "version": c.installed[id],
			"installed": true, "enabled": true, "installPolicy": "AVAILABLE", "authPolicy": "ON_INSTALL",
		})
	}

	return json.Marshal(map[string]any{"installed": out, "available": []any{}})
}

// marketplace runs one `codex plugin marketplace add|remove <arg>`.
func (c *codexCLI) marketplace(verb, arg string) ([]byte, error) {
	switch verb {
	case "add":
		name, _, err := readTestMarketplace(arg)
		if err != nil {
			return nil, codexRefused(1, "Error: %v", err)
		}

		root, known := c.marketplaces[name]
		if known && root != arg {
			return nil, codexRefused(1, "Error: marketplace '%s' is already added from a different source; remove it before adding this source", name)
		}

		c.marketplaces[name] = arg

		return json.Marshal(map[string]any{"marketplaceName": name, "installedRoot": arg, "alreadyAdded": known})
	case "remove":
		if _, ok := c.marketplaces[arg]; !ok {
			return nil, codexRefused(1, "Error: marketplace `%s` is not configured or installed", arg)
		}

		delete(c.marketplaces, arg)

		return json.Marshal(map[string]any{"marketplaceName": arg, "installedRoot": nil})
	default:
		return nil, codexRefused(2, "error: unrecognized subcommand '%s'", verb)
	}
}

// plugin runs one `codex plugin add|remove <selector>`.
func (c *codexCLI) plugin(verb, id string) ([]byte, error) {
	switch verb {
	case "add":
		plugin, marketplace := splitTestID(id)

		root, ok := c.marketplaces[marketplace]
		if !ok {
			return nil, codexRefused(1, "Error: marketplace `%s` is not configured or installed", marketplace)
		}

		version, err := testPluginVersion(root, plugin)
		if err != nil {
			return nil, codexRefused(1, "Error: %v", err)
		}

		if _, kept := c.installed[id]; !kept || !c.stale {
			c.installed[id] = version
		}

		return []byte("Added plugin `" + plugin + "` from marketplace `" + marketplace + "`."), nil
	case "remove":
		plugin, marketplace := splitTestID(id)
		delete(c.installed, id)

		return []byte("Removed plugin `" + plugin + "` from marketplace `" + marketplace + "`."), nil
	default:
		return nil, codexRefused(2, "error: unrecognized subcommand '%s'", verb)
	}
}

// testPluginVersion resolves a plugin's version through a dir marketplace.
func testPluginVersion(root, plugin string) (string, error) {
	_, sources, err := readTestMarketplace(root)
	if err != nil {
		return "", err
	}

	source, ok := sources[plugin]
	if !ok {
		return "", fmt.Errorf("plugin %q not found in marketplace %s", plugin, root)
	}

	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(source), ".claude-plugin", "plugin.json")) //nolint:gosec // G304: the fake reads its own temp store
	if err != nil {
		return "", err
	}

	var manifest struct {
		Version string `json:"version"`
	}

	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", err
	}

	return manifest.Version, nil
}

// geminiCLI is a stateful fake of the Gemini CLI 0.61.0 extensions grammar
// captured live: every `extensions` output goes to stderr (list JSON too),
// link/install without --consent ask for workspace trust (here: refused), a
// linked name cannot be linked again, and uninstall of an unknown name fails.
type geminiCLI struct {
	mu        sync.Mutex
	installed map[string]string // extension name → path
	// installDir is ~/.gemini/extensions when the world sets it: the CLI
	// materializes a link there and removes it again on uninstall, while
	// ~/.gemini/extension_integrity.json stays the CLI's alone.
	installDir string
	fail       map[string]hostcli.Response
	calls      []string
}

// newGeminiCLI builds an empty fake.
func newGeminiCLI() *geminiCLI {
	return &geminiCLI{installed: map[string]string{}, fail: map[string]hostcli.Response{}}
}

// Run implements hostcli.Runner: stdout only, like a CLI printing to stderr.
func (g *geminiCLI) Run(ctx context.Context, bin hostcli.Binary, args []string, stdin []byte) ([]byte, error) {
	stdout, _, err := g.RunStreams(ctx, bin, args, stdin)

	return stdout, err
}

// RunStreams implements hostcli.StreamRunner.
func (g *geminiCLI) RunStreams(_ context.Context, bin hostcli.Binary, args []string, _ []byte) ([]byte, []byte, error) {
	key := strings.Join(args, " ")

	g.mu.Lock()
	defer g.mu.Unlock()

	g.calls = append(g.calls, bin.Name+" "+key)

	if resp, ok := g.fail[key]; ok {
		return nil, []byte(resp.Stderr), &hostcli.ExitError{Name: bin.Name, Code: resp.Code, Stderr: resp.Stderr}
	}

	if len(args) < 2 || args[0] != "extensions" {
		return nil, nil, &hostcli.ExitError{Name: bin.Name, Code: 1, Stderr: "Unknown arguments"}
	}

	consented := slices.Contains(args, "--consent")

	switch args[1] {
	case "list":
		return nil, g.list(), nil
	case "link", "install":
		if !consented {
			return nil, nil, &hostcli.ExitError{
				Name: bin.Name, Code: 1,
				Stderr: "The current workspace is not trusted. Do you want to trust this workspace to install extensions?",
			}
		}

		return g.link(bin.Name, args[2])
	case "uninstall":
		name := args[2]
		if _, ok := g.installed[name]; !ok {
			stderr := `Failed to uninstall "` + name + `": Extension not found.`

			return nil, []byte(stderr), &hostcli.ExitError{Name: bin.Name, Code: 1, Stderr: stderr}
		}

		delete(g.installed, name)

		if g.installDir != "" {
			_ = os.Remove(filepath.Join(g.installDir, name))
		}

		return nil, []byte(`Extension "` + name + `" successfully uninstalled.`), nil
	default:
		return nil, nil, &hostcli.ExitError{Name: bin.Name, Code: 1, Stderr: "Unknown arguments"}
	}
}

// Calls returns the recorded calls as `<name> <joined args>` keys.
func (g *geminiCLI) Calls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	return slices.Clone(g.calls)
}

// Linked reports the path of an installed extension.
func (g *geminiCLI) Linked(name string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	path, ok := g.installed[name]

	return path, ok
}

// list renders `extensions list -o json` (printed to stderr).
func (g *geminiCLI) list() []byte {
	out := []map[string]any{}

	for _, name := range slices.Sorted(maps.Keys(g.installed)) {
		path := g.installed[name]
		out = append(out, map[string]any{
			"name": name, "version": "1.0.0", "path": path, "isActive": true,
			"installMetadata": map[string]string{"source": path, "type": "link"},
			"skills":          []map[string]string{{"name": "nested", "extensionName": name}},
		})
	}

	data, _ := json.MarshalIndent(out, "", "  ")

	return data
}

// link registers the extension a dir's gemini-extension.json names.
func (g *geminiCLI) link(bin, dir string) ([]byte, []byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, "gemini-extension.json")) //nolint:gosec // G304: the fake reads its own temp store
	if err != nil {
		return nil, nil, &hostcli.ExitError{Name: bin, Code: 1, Stderr: "Install source not found."}
	}

	var manifest struct {
		Name string `json:"name"`
	}

	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, nil, err
	}

	if _, ok := g.installed[manifest.Name]; ok {
		stderr := `Extension "` + manifest.Name + `" is already installed. Please uninstall it first.`

		return nil, []byte(stderr), &hostcli.ExitError{Name: bin, Code: 1, Stderr: stderr}
	}

	g.installed[manifest.Name] = dir

	if g.installDir != "" {
		if err := os.MkdirAll(g.installDir, 0o700); err != nil {
			return nil, nil, err
		}

		if err := os.Symlink(dir, filepath.Join(g.installDir, manifest.Name)); err != nil {
			return nil, nil, err
		}
	}

	return nil, []byte(`Extension "` + manifest.Name + `" linked successfully and enabled.`), nil
}

// synthPackage lays out one synth package in the store the way pack.Write
// does (<store>/synth/<owner>/<name>/<version>) with a Claude manifest named
// after the plugin identity, and returns the package the CLI would deliver.
func synthPackage(t *testing.T, st *store.Store, id, version string) host.Package {
	t.Helper()

	dir, err := st.EnsureSynthPath(id, version)
	if err != nil {
		t.Fatalf("synth path: %v", err)
	}

	identity, err := pack.IdentityOf(id)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}

	writeFixtureFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"),
		`{"name":"`+identity.Name+`","version":"`+version+`"}`, 0o600)

	return host.Package{ID: id, Version: version, SynthDir: dir, Marketplace: identity.Name + "@" + identity.Owner}
}

// applyWorld builds the pkg/apply dependencies over the adapter's own store,
// so the trash buckets a delivery records are the ones a removal restores.
func applyWorld(t *testing.T, st *store.Store, owner host.PathOwner) apply.Deps {
	t.Helper()

	if err := st.Ensure(); err != nil {
		t.Fatalf("ensure store: %v", err)
	}

	hm, err := home.New(filepath.Join(t.TempDir(), "verger-home"))
	if err != nil {
		t.Fatalf("home: %v", err)
	}

	return apply.Deps{
		Home:       hm,
		Store:      st,
		Receipts:   receipt.NewStore(hm.ReceiptsDir()),
		Journal:    receipt.OpenJournal(hm.JournalPath()),
		Tombstones: receipt.NewTombstoneStore(hm.TombstonesPath()),
		Hosts:      map[host.ID]host.Host{},
		Owned:      owner,
		Lock:       lock.New(),
		LockPath:   hm.LockPath(),
	}
}

// receiptsOwner resolves path ownership from a receipt store, as the CLI
// wiring does.
type receiptsOwner struct {
	receipts *receipt.Store
}

// Owner implements host.PathOwner.
func (o receiptsOwner) Owner(path string) (string, bool) {
	_, pkg, ok := o.artifact(path)

	return pkg, ok
}

// ArtifactDigest implements host.ArtifactDigests.
func (o receiptsOwner) ArtifactDigest(path string) (digest.Hash, bool) {
	artifact, _, ok := o.artifact(path)

	return artifact.Digest, ok
}

// artifact finds the receipt artifact recorded at a path.
func (o receiptsOwner) artifact(path string) (receipt.Artifact, string, bool) {
	list, err := o.receipts.List()
	if err != nil {
		return receipt.Artifact{}, "", false
	}

	for _, record := range list {
		for _, artifact := range record.Artifacts {
			if artifact.Path == path {
				return artifact, record.Package, true
			}
		}
	}

	return receipt.Artifact{}, "", false
}

// refuseConflicts answers every apply confirmation with no.
type refuseConflicts struct{}

// Confirm implements apply.Confirmer.
func (refuseConflicts) Confirm(context.Context, apply.Question) (bool, error) {
	return false, nil
}

// trashedValue reads the payload of one trash bucket.
func trashedValue(t *testing.T, st *store.Store, id string) string {
	t.Helper()

	entry, err := st.Trash().Get(id)
	if err != nil {
		t.Fatalf("trash bucket %s: %v", id, err)
	}

	data, err := os.ReadFile(filepath.Join(st.TrashDir(), id, entry.Stored)) //nolint:gosec // G304: the test reads its own temp store
	if err != nil {
		t.Fatalf("read trash bucket %s: %v", id, err)
	}

	return string(data)
}

// assertConcurrentDryRun runs concurrent dry runs of one adapter and pins that
// every run is clean and nothing was written.
func assertConcurrentDryRun(t *testing.T, h host.Host, home, marker string, pkg func() host.Package) {
	t.Helper()

	const workers = 8

	var (
		wg   sync.WaitGroup
		errs = make([]error, workers)
	)

	for i := range workers {
		wg.Go(func() {
			_, errs[i] = h.Deliver(t.Context(), home, host.Delivery{Package: pkg(), Strategy: host.Loose, DryRun: true})
		})
	}

	wg.Wait()

	Convey("When they finish", func() {
		Convey("Then every run is clean and nothing was written", func() {
			for i := range workers {
				So(errs[i], ShouldBeNil)
			}

			So(fileExists(marker), ShouldBeFalse)
		})
	})
}

// assertArtifactsBackedByOps pins the receipt invariant a loose plan must keep:
// every artifact names a path some RMA op covers (pkg/apply resolves a document's
// key op by the artifact path), and no two artifacts claim one path (a receipt
// refuses that), however many keys a shared document carries.
func assertArtifactsBackedByOps(t *testing.T, res host.Result) {
	t.Helper()

	covered := map[string]int{}
	hostOps := 0

	for _, op := range res.RMA {
		if op.Path != "" {
			covered[op.Path]++
		}

		if op.Kind == receipt.OpHostInstall {
			hostOps++
		}
	}

	seen := map[string]bool{}

	for _, artifact := range res.Artifacts {
		if seen[artifact.Path] {
			t.Fatalf("two artifacts claim %s", artifact.Path)
		}

		seen[artifact.Path] = true

		// A host-managed artifact (an MCP server added through the host CLI) is
		// identified by a host URI and reversed by a host-install op, which
		// carries a command instead of a path.
		hostManaged := strings.Contains(artifact.Path, "://") && hostOps > 0

		if covered[artifact.Path] == 0 && !hostManaged {
			t.Fatalf("artifact %s (%s) has no RMA op", artifact.Path, artifact.Kind)
		}
	}
}

// assertCLIPolicyBlocked pins the `disableCommandPluginSources` verdict on the
// native and synth strata of one adapter.
func assertCLIPolicyBlocked(t *testing.T, home string, pkg host.Package, adapter func(*hostcli.ScriptRunner) host.Host) {
	t.Helper()

	for _, strategy := range []host.Strategy{host.Native, host.Synth} {
		Convey("When "+string(strategy)+" wants a CLI install", func() {
			runner := hostcli.NewScriptRunner(nil)
			h := adapter(runner)

			blocked := pkg
			blocked.SynthDir = t.TempDir()

			_, err := h.Deliver(t.Context(), home, host.Delivery{Package: blocked, Strategy: strategy})

			Convey("Then the CLI install is blocked with the rule named", func() {
				typed, ok := errors.AsType[*host.PolicyError](err)
				So(ok, ShouldBeTrue)
				So(typed.Rule, ShouldEqual, "disableCommandPluginSources")
				So(runner.Calls(), ShouldBeEmpty)
			})
		})
	}
}

// ompCLI is a stateful fake of the omp 18.4.1 plugin grammar with the output
// shapes and refusal texts captured live (docs/reviews/omp-grammar.probe.log,
// fixtures in testdata/omp/): `--json` is honoured on `plugin list` alone,
// marketplaces register under the name their document declares, `marketplace
// add` is not idempotent, `plugin install` needs --force to reinstall, and
// every refusal goes to stderr with exit 1.
type ompCLI struct {
	mu           sync.Mutex
	marketplaces map[string]string // name → registered path
	installed    map[string]string // plugin@marketplace → version
	// shadow models the registry reachable through the same `name@tag`
	// spelling: when the marketplace name is also an npm dist-tag, the real CLI
	// installs the public package and ignores the registered marketplace
	// (live-observed with marketplace `beta` and package `one`). Keys are the
	// full selector, values the registry version it resolved to.
	shadow map[string]string
	// dropInstalls models a host that accepted an install and reports success
	// while listing nothing: the drift the verify step exists for.
	dropInstalls bool
	// registry is what such an install left behind: plugin → version, reported
	// in the npm list, never as a marketplace entry.
	registry map[string]string
	fail     map[string]hostcli.Response
	calls    []string
}

// newOmpCLI builds an empty fake.
func newOmpCLI() *ompCLI {
	return &ompCLI{
		marketplaces: map[string]string{}, installed: map[string]string{},
		shadow: map[string]string{}, registry: map[string]string{},
		fail: map[string]hostcli.Response{},
	}
}

// Run implements hostcli.Runner.
//
//nolint:dupl // every stateful fake has the same record-then-dispatch shell; the grammars differ below it
func (o *ompCLI) Run(_ context.Context, bin hostcli.Binary, args []string, _ []byte) ([]byte, error) {
	key := strings.Join(args, " ")

	o.mu.Lock()
	defer o.mu.Unlock()

	o.calls = append(o.calls, bin.Name+" "+key)

	if resp, ok := o.fail[key]; ok {
		return resp.Stdout, &hostcli.ExitError{Name: bin.Name, Code: resp.Code, Stderr: resp.Stderr}
	}

	return o.dispatch(key, args)
}

// dispatch routes one recorded call; --json is dropped first, exactly as the
// real CLI ignores it outside `plugin list` (the fake answers the same bytes
// either way, so a test can assert the flag was or was not passed).
func (o *ompCLI) dispatch(key string, args []string) ([]byte, error) {
	bare := slices.DeleteFunc(slices.Clone(args), func(arg string) bool { return arg == flagJSONTest })

	switch {
	case len(bare) == 3 && bare[0] == "plugin" && bare[1] == "marketplace" && bare[2] == "list":
		return o.marketplaceList(), nil
	case len(bare) >= 4 && bare[0] == "plugin" && bare[1] == "marketplace":
		return o.marketplace(bare[2], bare[3])
	case len(bare) >= 2 && bare[0] == "plugin":
		return o.dispatchPlugin(bare, key)
	default:
		return nil, ompRefused(1, "✘ error: unrecognized command '%s'", key)
	}
}

// dispatchPlugin routes `plugin list` and `plugin <verb> <selector> [--force]`.
func (o *ompCLI) dispatchPlugin(bare []string, key string) ([]byte, error) {
	switch {
	case len(bare) == 2 && bare[1] == "list":
		return o.pluginList()
	case len(bare) == 3:
		return o.plugin(bare[1], bare[2], false)
	case len(bare) == 4 && bare[3] == flagForceTest:
		return o.plugin(bare[1], bare[2], true)
	default:
		return nil, ompRefused(1, "✘ error: unrecognized command '%s'", key)
	}
}

// RunStreams implements hostcli.StreamRunner: the adapter reads stderr, where
// omp reports a module that failed to load while still exiting 0.
func (o *ompCLI) RunStreams(ctx context.Context, bin hostcli.Binary, args []string, stdin []byte) ([]byte, []byte, error) {
	key := strings.Join(args, " ")

	o.mu.Lock()

	o.calls = append(o.calls, bin.Name+" "+key)

	resp, scripted := o.fail[key]

	o.mu.Unlock()

	if scripted {
		return resp.Stdout, []byte(resp.Stderr), &hostcli.ExitError{Name: bin.Name, Code: resp.Code, Stderr: resp.Stderr}
	}

	out, err := o.Run(ctx, bin, args, stdin)

	return out, nil, err
}

// Calls returns the recorded calls as `<name> <joined args>` keys.
func (o *ompCLI) Calls() []string {
	o.mu.Lock()
	defer o.mu.Unlock()

	return slices.Clone(o.calls)
}

// Registered reports the path of a registered marketplace.
func (o *ompCLI) Registered(name string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	path, ok := o.marketplaces[name]

	return path, ok
}

// RegistryInstalled reports the version an npm-shadowed selector installed.
func (o *ompCLI) RegistryInstalled(plugin string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	version, ok := o.registry[plugin]

	return version, ok
}

// Installed reports the version of an installed plugin selector.
func (o *ompCLI) Installed(id string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	version, ok := o.installed[id]

	return version, ok
}

// ompRefused is a non-zero exit of the fake omp CLI; omp writes every refusal
// to stderr and leaves stdout empty.
func ompRefused(code int, format string, args ...any) error {
	return &hostcli.ExitError{Name: "omp", Code: code, Stderr: fmt.Sprintf(format, args...)}
}

// marketplaceList is `omp plugin marketplace list` (its text table: --json is
// ignored by omp 18.4.1 on this verb).
func (o *ompCLI) marketplaceList() []byte {
	if len(o.marketplaces) == 0 {
		return []byte("No marketplaces configured\n\nAdd one with: omp plugin marketplace add <source>\n")
	}

	var b strings.Builder

	b.WriteString("Configured Marketplaces:\n\n")

	for _, name := range slices.Sorted(maps.Keys(o.marketplaces)) {
		b.WriteString("  " + name + "  " + o.marketplaces[name] + "\n")
	}

	return []byte(b.String())
}

// pluginList is `omp plugin list --json`.
func (o *ompCLI) pluginList() ([]byte, error) {
	npm := []map[string]any{}

	for _, plugin := range slices.Sorted(maps.Keys(o.registry)) {
		version := o.registry[plugin]
		npm = append(npm, map[string]any{
			"id": plugin + "@" + version, "name": plugin, "version": version,
			"path": filepath.Join(".omp", "plugins", "node_modules", plugin),
		})
	}

	out := []map[string]any{}

	for _, id := range slices.Sorted(maps.Keys(o.installed)) {
		plugin, marketplace := splitTestID(id)
		out = append(out, map[string]any{
			"id": id, "scope": "user",
			"entries": []map[string]any{{
				"scope": "user", "version": o.installed[id],
				"installPath": filepath.Join(".omp", "plugins", "cache", "plugins", marketplace+"___"+plugin+"___"+o.installed[id]),
			}},
		})
	}

	return json.MarshalIndent(map[string]any{"npm": npm, "marketplace": out}, "", "  ")
}

// marketplace runs one `omp plugin marketplace add|remove <arg>`.
func (o *ompCLI) marketplace(verb, arg string) ([]byte, error) {
	switch verb {
	case "add":
		name, _, err := readTestMarketplace(arg)
		if err != nil {
			return nil, ompRefused(1, "✘ Failed to add marketplace: Error: %v", err)
		}

		for registered := range o.marketplaces {
			if strings.EqualFold(registered, name) {
				return nil, ompRefused(1, "✘ Failed to add marketplace: Error: Marketplace %q already exists", registered)
			}
		}

		o.marketplaces[name] = arg

		return []byte("✔ Added marketplace: " + arg + "\n"), nil
	case "remove":
		if _, ok := o.marketplaces[arg]; !ok {
			return nil, ompRefused(1, "✘ Failed to remove marketplace: Error: Marketplace %q not found", arg)
		}

		delete(o.marketplaces, arg)

		return []byte("✔ Removed marketplace: " + arg + "\n"), nil
	default:
		return nil, ompRefused(1, "✘ error: unrecognized marketplace verb %s", verb)
	}
}

// plugin runs one `omp plugin install|uninstall <selector>`.
func (o *ompCLI) plugin(verb, id string, force bool) ([]byte, error) {
	plugin, marketplace := splitTestID(id)

	switch verb {
	case "install":
		if o.dropInstalls {
			return []byte("✔ Installed " + plugin + "\n"), nil
		}

		if version, shadowed := o.shadow[id]; shadowed {
			o.registry[plugin] = version

			return []byte("✔ Installed " + plugin + " (" + version + ")\n"), nil
		}

		root, ok := o.marketplaces[marketplace]
		if !ok {
			return nil, ompRefused(1, "✘ Failed to install %s: Error: Marketplace %q not found", id, marketplace)
		}

		version, err := testPluginVersion(root, plugin)
		if err != nil {
			return nil, ompRefused(1, "✘ Failed to install %s: Error: %v", id, err)
		}

		if _, installed := o.installed[id]; installed && !force {
			return nil, ompRefused(1, "✘ Failed to install %s: Error: Plugin %q is already installed. Use force option to reinstall.", id, id)
		}

		o.installed[id] = version

		return []byte("✔ Installed " + plugin + " from " + marketplace + " (" + version + ")\n"), nil
	case "uninstall":
		if _, installed := o.installed[id]; !installed {
			return nil, ompRefused(1, "✘ %s is not installed", id)
		}

		delete(o.installed, id)

		return []byte("✔ Uninstalled " + id + "\n"), nil
	default:
		return nil, ompRefused(1, "✘ error: unrecognized plugin verb %s", verb)
	}
}

// flagJSONTest and flagForceTest are the flags as the fake sees them (the
// adapter's own constants are unexported to the test package).
const (
	flagJSONTest  = "--json"
	flagForceTest = "--force"
)
