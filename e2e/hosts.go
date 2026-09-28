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
	// adoptReason is non-empty when the host has no verified manual-install
	// grammar for the adopt leg; the leg is then skipped with this reason.
	adoptReason string
	// noRegisterReason is non-empty when the host has no stratum that
	// registers a package with the host CLI at all (no plugin manager), so the
	// remote-archive scenario — which exists to exercise that rung — cannot
	// apply and is skipped with this reason.
	noRegisterReason string
}

// hostSpecs is the Ф1 e2e matrix (DESIGN §10.3, D21): three real CLIs, plus
// omp (Ф2).
func hostSpecs() []hostSpec {
	return []hostSpec{
		{
			id:              "claude",
			binary:          "claude",
			npm:             "@anthropic-ai/claude-code",
			pin:             claudeVersionPin,
			configDirs:      []string{".claude"},
			listArgs:        []string{"plugin", "list", "--json"},
			hooksFile:       ".claude/settings.json",
			fixtureManifest: ".claude-plugin/plugin.json",
		},
		{
			id:              "codex",
			binary:          "codex",
			npm:             "@openai/codex",
			pin:             codexVersionPin,
			configDirs:      []string{".codex", ".agents"},
			listArgs:        []string{"plugin", "list", "--json"},
			hooksFile:       ".codex/hooks.json",
			fixtureManifest: "plugin.json",
			// The manual local install is the adapter's verified grammar
			// (codex-cli 0.157.1): `plugin marketplace add <dir> --json` then
			// `plugin add <plugin>@<marketplace>`.
		},
		{
			id:              "gemini",
			binary:          "gemini",
			npm:             "@google/gemini-cli",
			pin:             geminiVersionPin,
			configDirs:      []string{".gemini"},
			listArgs:        []string{"extensions", "list", "--output-format", "json"},
			hooksFile:       ".gemini/settings.json",
			fixtureManifest: "gemini-extension.json",
		},
		{
			id:          "cursor",
			binary:      "cursor-agent",
			pin:         cursorVersionPin,
			installNote: "cursor-agent ships from cursor.com/install (a self-updating binary), not from npm",
			configDirs:  []string{".cursor"},
			listArgs:    []string{"mcp", "list"},
			hooksFile:   ".cursor/hooks.json",
			// cursor-agent has no plugin, extension or marketplace command, so
			// the fixture cannot be installed with the host itself.
			adoptReason:      "cursor-agent has no plugin, extension or marketplace command",
			noRegisterReason: "cursor-agent has no plugin, extension or marketplace command, so no stratum registers a package with the host",
			fixtureManifest:  ".claude-plugin/plugin.json",
		},
		{
			id:              "omp",
			binary:          "omp",
			npm:             "@oh-my-pi/pi-coding-agent",
			npmExtra:        []string{"bun"},
			pin:             ompVersionPin,
			configDirs:      []string{".omp", ".agents"},
			residueSkip:     []string{".omp/logs", ".omp/natives"},
			listArgs:        []string{"plugin", "list", "--json"},
			hooksFile:       ".omp/agent/hooks",
			fixtureManifest: ".claude-plugin/plugin.json",
			// The manual local install is the adapter's verified grammar
			// (omp 18.4.1): `plugin marketplace add <dir>` then
			// `plugin install <plugin>@<marketplace> --force`.
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
