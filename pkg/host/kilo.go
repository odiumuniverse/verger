package host

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/odiumuniverse/verger/pkg/hostpath"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
)

// Kilo (Kilo Code, an OpenCode fork) surface names. `<home>/.config/kilo` is
// only the DEFAULT root: kilo relocates, and pkg/hostpath owns the rule —
// KILO_CONFIG_DIR replaces the root outright, XDG_CONFIG_HOME is the
// fallback, and the home default is read exactly when XDG is unset. All three
// were measured live on 7.8.1 (W1-A §fix F2). An earlier version of this
// comment said there was no XDG relocation "there, and none here", which
// stopped being true when hostpath started modelling it.
const (
	wordKilo           = "kilo"
	kiloDirName        = "kilo"
	kiloJSONC          = "kilo.jsonc"
	kiloJSON           = "kilo.json"
	kiloLegacyDirName  = ".kilo"
	kiloSkillsDir      = "skills"
	kiloAgentsDir      = "agents"
	kiloCommandsDir    = "commands"
	kiloMCPPrefix      = "mcp."
	kiloVoiceCheckHint = "kilo mcp list"
)

// kiloHooksBlocked is the reason a hook component is skipped: Kilo's plugin
// system is the v1 OpenCode one (`plugin: [...]` in the legacy opencode.json
// carries hooks and tools only), and the module runtime arrives with T2.3.
const kiloHooksBlocked = "the Kilo runtime adapter arrives with T2.3; a Kilo plugin is a module in the v1 plugin list, not a file"

// kilo is the Kilo Code adapter. Its write surfaces are the host's own user
// files — skills, agents, commands and the MCP container of kilo.jsonc
// (verified live on kilo 7.8.1: `kilo debug paths`, `kilo debug skill` and
// `kilo mcp list` all read them). The plugin rung is not claimed in this wave:
// `kilo plugin <module> --global` writes a v1 `plugin: [...]` entry into the
// legacy opencode.json, exits 0 even when the install fails, and has no
// listing or removal verb (live-verified), so a delivery cannot verify or take
// back what it installed. Everything else is refused with that reason.
type kilo struct {
	base *Base
}

// NewKilo builds the Kilo adapter; the adapter is immutable after construction
// and safe for concurrent use.
func NewKilo(opts ...Option) Host {
	return &kilo{base: newBase(opts)}
}

// ID implements Host.
func (h *kilo) ID() ID {
	return Kilo
}

// Oracle implements Host: kilo's own resolved configuration
// (`kilo debug config`, live-verified on 7.8.1) is the only listing surface —
// it reports the merged config sources, the `plugin` list and the MCP servers
// — so the oracle reads that document instead of an invented command.
func (h *kilo) Oracle() Oracle {
	return &kiloOracle{base: h.base}
}

// Detect reports whether Kilo is present: its config dir, the legacy `~/.kilo`
// dir (beadle detects both), or the `kilo` binary.
func (h *kilo) Detect(home string) bool {
	userHome := h.base.effectiveHome(home)

	if isDir(kiloConfigDir(userHome)) || isDir(filepath.Join(userHome, kiloLegacyDirName)) {
		return true
	}

	_, err := h.base.resolve(wordKilo)

	return err == nil
}

// Deliver implements Host. The adapter has no installer it can verify, so
// native and synth are refused with the reason; loose is the only stratum.
func (h *kilo) Deliver(ctx context.Context, home string, d Delivery) (Result, error) {
	return deliverByStrategy(ctx, h.base, Kilo, home, d, h.deliverInstall, h.deliverLoose)
}

// deliverInstall refuses the CLI-driven strata: `kilo plugin` installs a
// module into `<cache>/packages` and writes the v1 plugin list into
// `<config>/opencode.json` without a listing or removal verb, and it exits 0
// for a failed install (live-verified on 7.8.1), so neither a verify nor a
// rollback can be honest here yet.
func (h *kilo) deliverInstall(_ context.Context, _ string, _ Delivery, synth bool) (Result, error) {
	strategy := "native"
	if synth {
		strategy = "synth"
	}

	return Result{}, &NotSupportedError{
		Host:      Kilo,
		Operation: strategy + " delivery (kilo plugin has no listing or removal verb and exits 0 for a failed install; the v1 plugin runtime arrives with T2.3)",
	}
}

// kiloConfigDir resolves the Kilo config root exactly as the host does,
// live-probed on 7.8.1 (`kilo mcp list` in an isolated HOME): KILO_CONFIG_DIR
// replaces the root outright, XDG_CONFIG_HOME is the fallback and
// <home>/.config/kilo is the default. The rule lives in pkg/hostpath so verger
// and beadle cannot drift; the adapter no longer pins beadle's old hardcoded
// path (W1-A §fix).
func kiloConfigDir(home string) string {
	roots, err := hostpath.Roots(hostpath.Kilo, hostEnv(home))
	if err != nil {
		return filepath.Join(home, ".config", kiloDirName)
	}

	return roots.ConfigRoot
}

// kiloConfigFile resolves the config document: an existing kilo.jsonc wins
// over kilo.json, and a host with neither gets kilo.json (beadle's rule).
func kiloConfigFile(dir string) string {
	for _, name := range []string{kiloJSONC, kiloJSON} {
		if path := filepath.Join(dir, name); isFile(path) {
			return path
		}
	}

	return filepath.Join(dir, kiloJSON)
}

// kiloSpec is the loose surface of the Kilo adapter: skills, agents and
// commands below the config dir, MCP servers under the v1 `/mcp` container the
// host reads (live-verified: `kilo mcp list` reports a server declared there).
//
//nolint:dupl // kilo and OpenCode share the OpenCode dialect; only the config roots differ
func kiloSpec(userHome string) looseSpec {
	dir := kiloConfigDir(userHome)

	return looseSpec{
		host:          Kilo,
		binary:        wordKilo,
		home:          userHome,
		skillsDir:     filepath.Join(dir, kiloSkillsDir),
		agentsDir:     filepath.Join(dir, kiloAgentsDir),
		commandsDir:   filepath.Join(dir, kiloCommandsDir),
		hooksBlocked:  kiloHooksBlocked,
		renderAgent:   func(agent render.Agent, _ string) ([]byte, error) { return agent.OpenCodeMarkdown() },
		renderCommand: func(cmd render.Command) ([]byte, error) { return cmd.OpenCodeMarkdown() },
		mcpConfig: &mcpConfigSpec{
			path:   kiloConfigFile(dir),
			format: manifest.FormatOpenCode,
			prefix: kiloMCPPrefix,
			edit:   render.EditJSONC,
		},
	}
}

// deliverLoose plans and executes the host-native user files.
func (h *kilo) deliverLoose(ctx context.Context, home string, d Delivery) (Result, error) {
	return deliverSurface(ctx, h.base, kiloSpec(h.base.effectiveHome(home)), d)
}

// Uninstall implements Host: kilo records no host-install op (there is no
// installer it can use), so the file and tree ops of the receipt are all
// there is.
func (h *kilo) Uninstall(_ context.Context, _ string, r receipt.Receipt) (Result, error) {
	return Result{Strategy: Strategy(r.Strategy)}, nil
}

// kiloOracle is the Kilo oracle: the host's own resolved configuration.
type kiloOracle struct {
	base *Base
}

// kiloOracleWait bounds the oracle call: kilo is a Node CLI that boots in
// seconds and talks to no server here, but a bounded wait keeps a wedged
// process from hanging the caller.
const kiloOracleWait = 30 * time.Second

// List implements Oracle: `kilo debug config` prints the resolved
// configuration as one JSON document — the merged `plugin` list with its
// origins and the MCP servers among it (live-verified on 7.8.1). The plugins
// are what the Oracle interface calls packages: MCP servers are not packages,
// are reported by the host's own `kilo mcp list`, and are deliberately left
// out so an adopt lookup can never mistake a server for a package.
func (o *kiloOracle) List(ctx context.Context) ([]Installed, error) {
	ctx, cancel := context.WithTimeout(ctx, kiloOracleWait)
	defer cancel()

	out, err := o.base.run(ctx, wordKilo, []string{"debug", "config"})
	if err != nil {
		if ctx.Err() != nil {
			return nil, &OracleError{
				Host:  string(Kilo),
				Cause: fmt.Errorf("%s debug config did not answer within %s", wordKilo, kiloOracleWait),
			}
		}

		return nil, err
	}

	listed, parseErr := parseKiloConfig(out)
	if parseErr != nil {
		return nil, &OracleError{Host: string(Kilo), Output: string(out), Cause: parseErr}
	}

	return listed, nil
}

// Validate implements Oracle: kilo 7.8.1 has no package validation verb (its
// `debug agent` resolves one agent, `mcp debug` one server), so validation is
// not supported by this host and no CLI call is made.
func (o *kiloOracle) Validate(context.Context, string) ([]string, error) {
	return nil, ErrNotSupported
}

// parseKiloConfig reads the resolved configuration document and returns its
// plugins in the host's own order. `plugin_origins` carries the source and
// scope of the entries it knows; a spec the document lists without an origin
// is still a configured plugin and is listed as a user-scope entry, and an
// origin the flat list lost still counts (the host merges several sources).
func parseKiloConfig(out []byte) ([]Installed, error) {
	var doc struct {
		Plugin  []string `json:"plugin"`
		Origins []struct {
			Spec   string `json:"spec"`
			Source string `json:"source"`
			Scope  string `json:"scope"`
		} `json:"plugin_origins"`
	}

	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, err
	}

	origins := make(map[string]int, len(doc.Origins))

	for index, origin := range doc.Origins {
		origins[origin.Spec] = index
	}

	seen := make(map[string]bool, len(doc.Plugin))
	listed := make([]Installed, 0, len(doc.Plugin)+len(doc.Origins))

	for _, spec := range doc.Plugin {
		entry := Installed{Name: spec, Scope: receipt.ScopeUser, Enabled: true}

		if index, ok := origins[spec]; ok {
			entry.Source = doc.Origins[index].Source
			entry.Scope = doc.Origins[index].Scope
		}

		seen[spec] = true

		listed = append(listed, entry)
	}

	for _, origin := range doc.Origins {
		if seen[origin.Spec] {
			continue
		}

		listed = append(listed, Installed{
			Name: origin.Spec, Source: origin.Source, Scope: origin.Scope, Enabled: true,
		})
	}

	return listed, nil
}
