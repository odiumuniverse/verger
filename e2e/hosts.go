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
)

// hostSpec describes one host CLI the matrix exercises.
type hostSpec struct {
	id     string // verger host id: claude|codex|gemini
	binary string // host CLI executable name on PATH
	npm    string // npm package the job installs
	pin    string // fallback version when no E2E_*_VERSION is set
	// configDirs are host-owned directories below HOME: residue scan and
	// failure listings.
	configDirs []string
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
}

// hostSpecs is the Ф1 e2e matrix (DESIGN §10.3, D21): three real CLIs.
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
			// OQ-T1.7.1: the codex plugin subcommands/JSON are unverified;
			// the adopt leg stays skipped until the CI run confirms them.
			adoptReason: "codex manual local install grammar is unverified (OQ-T1.7.1)",
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
