//go:build e2e

package e2e

import (
	"os"
	"strings"
)

// Pinned host CLI versions for the e2e matrix (chosen 2026-09-27 from the
// latest npm releases; OQ-T1.13.1: bumps are deliberate PRs owned by the
// orchestrator). The workflow passes the same pins through E2E_*_VERSION.
const (
	claudeVersionPin = "2.1.283"
	codexVersionPin  = "0.157.1"
	geminiVersionPin = "0.61.0"
	// omp publishes @oh-my-pi/pi-coding-agent on npm; the binary is a Bun
	// program and its launcher needs the bun runtime (npmExtra).
	ompVersionPin = "18.4.1"
	// cursor-agent ships from cursor.com/install (a self-updating binary), not
	// from npm; the pin is what was live-verified locally.
	cursorVersionPin = "2026.06.15-18-00-12-6f5a2cf"
)

// hostSpec describes one host CLI the matrix exercises.
type hostSpec struct {
	id     string // verger host id: claude|codex|gemini|omp
	binary string // host CLI executable name on PATH
	npm    string // npm package the job installs
	// npmExtra lists npm packages the host CLI needs beside its own: the omp
	// launcher starts with `#!/usr/bin/env bun`.
	npmExtra []string
	// installNote is non-empty when the host CLI has no npm distribution: the
	// leg then runs only where the binary is already present, and the note is
	// the reason a runner without it skips instead of failing.
	installNote string
	pin         string // fallback version when no E2E_*_VERSION is set
	// configDirs are host-owned directories below HOME: residue scan and
	// failure listings.
	configDirs []string
	// residueSkip are subtrees below a config dir the residue scan leaves
	// alone: host-owned runtime state that may legitimately mention a plugin
	// (omp's own logs and its prebuilt native module do).
	residueSkip []string
	// listArgs is the host's own JSON list oracle (never an LLM call).
	listArgs []string
	// hooksFile is the host's shared hooks document below HOME; the adopt leg
	// snapshots it to prove no hooks were installed.
	hooksFile string
	// fixtureManifest is the primary manifest path inside the fixture.
	fixtureManifest string
	// adoptReason is non-empty when the adopt leg cannot run: the host has no
	// verified manual-install grammar, or a product limitation makes the
	// adoption fail (the reason names the finding). The leg skips with it.
	adoptReason string
	// noRegisterReason is non-empty when the host has no stratum that
	// registers a package with the host CLI at all (no plugin manager), so the
	// remote-archive scenario — which exists to exercise that rung — cannot
	// apply and is skipped with this reason.
	noRegisterReason string
	// mcpFile is the host's MCP document below HOME — the file the host itself
	// reads, which the canon leg asserts the fixture's server name appears in
	// (the receipt records a synthetic URI for an MCP server, so the document
	// is the fact). Empty for a host with no MCP surface.
	mcpFile string
	// canonKinds are the component kinds a loose delivery of the canon
	// fixture (skills + agents + commands + MCP + hooks) must record for this
	// host: the receipt is the claim, the files are the fact
	// (TestE2ECanonPackageScenario). A kind absent here is a documented host
	// limit, not a silent skip.
	canonKinds []string
	// canonHooksBlocked is the adapter's documented reason the host takes no
	// hook component; non-empty means the leg asserts the skip note instead of
	// a hook edit.
	canonHooksBlocked string
	// adoptByPath says the host's oracle names a package by its install path
	// rather than by a name, so the adopt leg must pass that path as the ref
	// (pi lists resolved directories, pkg/host/pi.go:333-374, and adopt matches
	// a name exactly, pkg/verger/install.go:569-577).
	adoptByPath bool
	// noOracleReason is non-empty when the host has no listing oracle the
	// driver can trust (service-backed, lagging, absent, or absent with a
	// binary that would start a session); every scenario then asserts receipts
	// and files instead of shelling out to the host.
	noOracleReason string
}

// hostSpecs is the Ф1 e2e matrix (DESIGN §10.3, D21): three real CLIs, plus
// omp (Ф2).
func hostSpecs() []hostSpec {
	return []hostSpec{
		{
			id:              "claude",
			mcpFile:         ".claude.json",
			binary:          "claude",
			npm:             "@anthropic-ai/claude-code",
			pin:             claudeVersionPin,
			configDirs:      []string{".claude"},
			listArgs:        []string{"plugin", "list", "--json"},
			hooksFile:       ".claude/settings.json",
			fixtureManifest: ".claude-plugin/plugin.json",
			canonKinds:      []string{"skill", "agent", "command", "mcp", "hook"},
		},
		{
			id:              "codex",
			mcpFile:         ".codex/config.toml",
			binary:          "codex",
			npm:             "@openai/codex",
			pin:             codexVersionPin,
			configDirs:      []string{".codex", ".agents"},
			listArgs:        []string{"plugin", "list", "--json"},
			hooksFile:       ".codex/hooks.json",
			fixtureManifest: "plugin.json",
			canonKinds:      []string{"skill", "agent", "command", "mcp", "hook"},
			// The manual local install is the adapter's verified grammar
			// (codex-cli 0.157.1): `plugin marketplace add <dir> --json` then
			// `plugin add <plugin>@<marketplace>`.
		},
		{
			id:              "gemini",
			mcpFile:         ".gemini/settings.json",
			binary:          "gemini",
			npm:             "@google/gemini-cli",
			pin:             geminiVersionPin,
			configDirs:      []string{".gemini"},
			listArgs:        []string{"extensions", "list", "--output-format", "json"},
			hooksFile:       ".gemini/settings.json",
			fixtureManifest: "gemini-extension.json",
			// The MCP server lands in the same document as the hooks, and the
			// receipt records one artifact per document, so the MCP claim is
			// asserted through the document (finding F3 in W3-E2E10-1.md).
			canonKinds: []string{"skill", "agent", "command", "hook"},
		},
		{
			id:      "agy",
			mcpFile: ".gemini/config/mcp_config.json",
			npm:     "",
			pin:     "",
			binary:  "agy",
			installNote: "no agy binary ships on macOS or Linux: Antigravity is a desktop application, " +
				"so this leg skips wherever the CLI is absent",
			configDirs: []string{".gemini"},
			// the adapter's oracle is a filesystem scan and never shells out
			// (pkg/host/agy.go:328-360); with no binary the leg must not run it.
			listArgs:          nil,
			noOracleReason:    "agy has no binary: the adapter's oracle is a filesystem scan",
			hooksFile:         ".gemini/config/hooks.json",
			fixtureManifest:   "plugin.json",
			canonKinds:        []string{"skill", "agent", "mcp"},
			canonHooksBlocked: "antigravity hooks.json is owner-keyed (top-level plugin names, no hooks wrapper), a dialect verger does not render yet",
			adoptReason:       "no agy binary exists to register a package, and the adapter has no verified manual-install grammar",
			noRegisterReason:  "no agy binary exists, so no stratum registers a package with the host CLI",
			// The manual local install is the adapter's verified grammar
			// (omp 18.4.1): `plugin marketplace add <dir>` then
			// `plugin install <plugin>@<marketplace> --force`.
		},
		{
			id:          "cursor",
			mcpFile:     ".cursor/mcp.json",
			binary:      "cursor-agent",
			pin:         cursorVersionPin,
			installNote: "cursor-agent ships from cursor.com/install (a self-updating binary), not from npm",
			configDirs:  []string{".cursor"},
			listArgs:    []string{"mcp", "list"},
			hooksFile:   ".cursor/hooks.json",
			// No cursor-agent subcommand registers a package with the host, and
			// verger does not deliver the host's file-level plugins/local form
			// yet, so the fixture cannot be installed with the host itself.
			adoptReason:      "no cursor-agent subcommand registers a package; verger does not deliver ~/.cursor/plugins/local yet",
			noRegisterReason: "no cursor-agent subcommand registers a package, so no stratum registers one with the host",
			fixtureManifest:  ".claude-plugin/plugin.json",
			canonKinds:       []string{"skill", "agent", "command", "mcp", "hook"},
			// cursor-agent's only listing is `mcp list` (text, MCP servers
			// only), so the driver asserts receipts and files.
			noOracleReason: "cursor-agent has no plugin listing; `mcp list` covers MCP servers only",
		},
		{
			id:      "opencode",
			mcpFile: ".config/opencode/opencode.json",
			npm:     "", // 2.x is not on npm; see installNote
			pin:     "2.0.18",
			installNote: "opencode 2.x ships from the Homebrew tap anomalyco/tap/opencode-v2 " +
				"(brew install anomalyco/tap/opencode-v2); the npm package opencode-ai is the 1.x line " +
				"and installs no opencode binary on PATH",
			binary:     "opencode",
			configDirs: []string{".config/opencode", ".local/share/opencode"},
			// the host's own log legitimately mentions plugin names and module
			// paths (pkg/runtime/live_test.go:228).
			residueSkip: []string{".local/share/opencode/log"},
			// text table, no --json, and it answers from the background service
			// (pkg/host/opencode.go:566-582).
			listArgs:          []string{"plugin", "list"},
			hooksFile:         ".config/opencode/opencode.json",
			fixtureManifest:   ".claude-plugin/plugin.json",
			canonKinds:        []string{"skill", "agent", "command", "mcp"},
			canonHooksBlocked: "the OpenCode runtime adapter arrives with T2.3; OpenCode hooks are plugin modules, not files",
			noOracleReason:    "opencode plugin list is service-backed and lags the config document, so the canon leg asserts receipts and files",
			adoptReason:       "opencode plugin add takes only an npm or Git package specifier; a local directory is refused, so there is no manual-install grammar to adopt",
			noRegisterReason:  "opencode plugin add refuses a local directory (Plugin target must be an npm registry package or Git package specifier), so no stratum registers a local package with the host CLI",
		},
		{
			id:         "kilo",
			mcpFile:    ".config/kilo/kilo.json",
			binary:     "kilo",
			npm:        "@kilocode/cli",
			pin:        "7.8.1",
			configDirs: []string{".config/kilo"},
			residueSkip: []string{
				".local/share/kilo", ".cache/kilo", ".local/state/kilo",
			},
			// the only machine-readable oracle in the whole matrix: real JSON,
			// no service, no network (pkg/host/kilo.go:180-215).
			listArgs:          []string{"debug", "config"},
			hooksFile:         ".config/kilo/kilo.json",
			fixtureManifest:   ".claude-plugin/plugin.json",
			canonKinds:        []string{"skill", "agent", "command", "mcp"},
			canonHooksBlocked: "the Kilo runtime adapter arrives with T2.3; a Kilo plugin is a module in the v1 plugin list, not a file",
			adoptReason:       "kilo plugin takes an npm module name only and a directory module surfaces as a file:// URL, not as a package name, so there is nothing to adopt",
			noRegisterReason:  "kilo plugin <module> has no listing and no removal verb and exits 0 for a failed install, so no stratum registers a local package with the host CLI",
		},
		{
			id:         "pi",
			mcpFile:    ".pi/agent/mcp.json",
			binary:     "pi",
			npm:        "@earendil-works/pi-coding-agent",
			pin:        "0.74.2",
			configDirs: []string{".pi"},
			// `pi list` is text only (no --json) and the driver's oracle
			// matcher is a substring check, so it stays usable; an unknown
			// bare word would start an LLM session instead, so only verbs that
			// exist may be passed (dist/cli/args.js:145-146,193-200).
			listArgs:    []string{"list"},
			adoptByPath: true,
			// `pi list` names a resolved path and carries no version, and
			// adopting a versionless entry fails in the apply phase
			// ("package version is required", pkg/apply/phase.go:1902) —
			// finding F5 in W3-E2E10-1.md, owned by the adopt path.
			adoptReason: "pi list carries no version, and adopt of a versionless entry fails in the apply phase",
			hooksFile:   ".pi/agent/settings.json",
			// pi's own package format (a `pi` key in package.json) is not one of
			// the four manifest formats verger parses, so the fixture uses the
			// portable claude shape every host reads (finding F4 in
			// W3-E2E10-1.md).
			fixtureManifest: ".claude-plugin/plugin.json",
			// pi has no subagents and no hook surface; commands are staged as
			// prompts (pkg/host/pi.go:142-143).
			canonKinds:        []string{"skill", "command", "mcp"},
			canonHooksBlocked: "pi has no hook surface: hooks reach pi only as extension code, a dialect verger does not render",
		},
		{
			id:         "dsh",
			mcpFile:    ".dsh/cordis.patch.yml",
			binary:     "dsh",
			npm:        "@deepseek-ai/dsh",
			pin:        "0.1.7-rc.2",
			configDirs: []string{".dsh"},
			// no listing verb exists: `dsh` with no argv starts a session, and
			// the only subcommand is a pnpm passthrough that initializes the
			// profile as a side effect (pkg/host/dsh.go:50-56).
			listArgs:          nil,
			noOracleReason:    "dsh has no listing verb; its only subcommand is a pnpm passthrough",
			hooksFile:         ".dsh/cordis.patch.yml",
			fixtureManifest:   "plugin.json",
			canonKinds:        []string{"skill", "mcp"},
			canonHooksBlocked: "dsh has no hook document: hooks reach it only through the dsh-hooks-claude-code plugin's config string in a profile preset, a dialect verger does not render",
			adoptReason:       "dsh has no package registry: the only install path is a pnpm passthrough into a profile",
			noRegisterReason:  "dsh has no installer to ride: its plugin verb is a pnpm passthrough into $DSH_HOME/profiles/<name>",
		},
		{
			id:              "omp",
			mcpFile:         ".omp/agent/mcp.json",
			binary:          "omp",
			npm:             "@oh-my-pi/pi-coding-agent",
			npmExtra:        []string{"bun"},
			pin:             ompVersionPin,
			configDirs:      []string{".omp", ".agents"},
			residueSkip:     []string{".omp/logs", ".omp/natives"},
			listArgs:        []string{"plugin", "list", "--json"},
			hooksFile:       ".omp/agent/hooks",
			fixtureManifest: ".claude-plugin/plugin.json",
			canonKinds:      []string{"skill", "agent", "command", "mcp"},
			// omp hooks are TS/JS modules under hooks/{pre,post}/ with no
			// declarative surface (pkg/host/omp.go:53).
			canonHooksBlocked: "omp hooks are TS/JS modules under hooks/{pre,post}/ and have no declarative surface",
		},
	}
}

// hostSpecByID finds one matrix entry; an unknown id is a driver bug.
func hostSpecByID(id string) (hostSpec, bool) {
	for _, spec := range hostSpecs() {
		if spec.id == id {
			return spec, true
		}
	}

	return hostSpec{}, false
}

// version resolves the pinned host CLI version: E2E_<ID>_VERSION, then the
// generic E2E_VERSION (the matrix passes one), then the code pin.
func (h hostSpec) version() string {
	if v := strings.TrimSpace(os.Getenv("E2E_" + strings.ToUpper(h.id) + "_VERSION")); v != "" {
		return v
	}

	if v := strings.TrimSpace(os.Getenv("E2E_VERSION")); v != "" {
		return v
	}

	return h.pin
}

// moduleRef is the npm install ref, e.g. @openai/codex@0.157.1.
func (h hostSpec) moduleRef() string {
	return h.npm + "@" + h.version()
}

// installHint is what the skip message tells a reader to do: install the npm
// package, or the reason this host has none.
func (h hostSpec) installHint() string {
	if h.npm == "" {
		return h.installNote
	}

	return "npm install -g " + strings.Join(h.installRefs(), " ")
}

// installRefs is the npm install argument list of one host: its own package at
// the pinned version plus the runtime packages it needs (omp's launcher). A host
// without an npm distribution has none, and installNote says why.
func (h hostSpec) installRefs() []string {
	if h.npm == "" {
		return nil
	}

	return append([]string{h.moduleRef()}, h.npmExtra...)
}
