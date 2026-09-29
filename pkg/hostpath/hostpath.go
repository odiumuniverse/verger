// Package hostpath answers one question for every supported agent host: given
// a user home and an environment, where does that host actually read and write
// its configuration? It is the single place the two tools agree on, and it is
// pure — no filesystem, no os package, no network, no LLM.
//
// Every constant here carries its evidence in the comment above it. Where the
// two tools disagreed, the host's own observed behaviour won and the
// disagreement is listed in docs/reviews/W2-HOSTPATH-1.md as a migration note.
//
// The package never calls os.Getenv: the caller supplies the environment, so
// the same input always yields the same output and a test can assert the whole
// host × env × GOOS matrix without touching a real machine.
package hostpath

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Canonical host ids (U4). These are the short verger ids and the only ids this
// package accepts; the longer legacy spellings belong to a config migration, not
// to a path resolver.
const (
	Claude   = "claude"
	Codex    = "codex"
	Gemini   = "gemini"
	Agy      = "agy"
	Cursor   = "cursor"
	OpenCode = "opencode"
	Kilo     = "kilo"
	Pi       = "pi"
	DSH      = "dsh"
	Omp      = "omp"
)

// Environment variables, quoted verbatim from the hosts' own source.
const (
	// ClaudeConfigDir relocates the whole ~/.claude base. Live macOS 2.1.283:
	// CLAUDE_CONFIG_DIR=/tmp/x claude mcp list created /tmp/x/backups.
	ClaudeConfigDir = "CLAUDE_CONFIG_DIR"

	// CodexHome relocates ~/.codex. A RELATIVE value is accepted by the host
	// whenever it resolves: live macOS and Linux 0.158.0, CODEX_HOME=relx with
	// ./relx present → `codex mcp add` wrote <cwd>/relx/config.toml, and the
	// host reported `codex_home: AbsolutePathBuf(".../relx")`. The host only
	// fails when the value cannot be resolved: CODEX_HOME=missingdir →
	// "failed to resolve CODEX_HOME", "CODEX_HOME points to \"missingdir\",
	// but that path does not exist". This package has no filesystem and no
	// working directory, so it cannot decide resolvability and refuses
	// instead of guessing — a deliberate deviation, documented in Roots.
	CodexHome = "CODEX_HOME"

	// GeminiCLIHome relocates the *home*, not the .gemini directory: the host
	// appends .gemini itself. Live macOS 0.46.0 and Linux 0.46.0:
	// GEMINI_CLI_HOME=$T/g created $T/g/.gemini/.
	GeminiCLIHome = "GEMINI_CLI_HOME"

	// XDGConfigHome relocates opencode and kilo, and nothing else. Live macOS
	// 2.0.18: XDG_CONFIG_HOME=$T/xdg created $T/xdg/opencode. Live macOS and
	// Linux 7.8.1: XDG_CONFIG_HOME=$T/xk made kilo read $T/xk/kilo/kilo.json.
	XDGConfigHome = "XDG_CONFIG_HOME"

	// OpenCodeConfigDir replaces the OpenCode config root outright and outranks
	// XDG_CONFIG_HOME. Live-probed on macOS 2.0.18 in an isolated HOME with the
	// service started (W1-A §fix F2): `OPENCODE_CONFIG_DIR=$T/cfg opencode
	// debug paths` reported `config  $T/cfg`, it still won when
	// XDG_CONFIG_HOME was also set, and an empty value fell back. The value is
	// used verbatim — a relative one included, exactly as the host reads it.
	OpenCodeConfigDir = "OPENCODE_CONFIG_DIR"

	// KiloConfigDir replaces the kilo config root outright. Live macOS and
	// Linux 7.8.1: KILO_CONFIG_DIR=$T/kd made `kilo mcp list` read
	// $T/kd/kilo.json. Neither tool models this today.
	KiloConfigDir = "KILO_CONFIG_DIR"

	// CursorConfigDir moves cursor's cli-config.json and nothing else. Live
	// macOS and Linux 2026.06.15: CURSOR_CONFIG_DIR=$T/cu created
	// $T/cu/cli-config.json, yet mcp.json was still read only from ~/.cursor.
	CursorConfigDir = "CURSOR_CONFIG_DIR"

	// PiCodingAgentDir is pi's only config override. Live macOS 0.74.2: an
	// absolute value is used verbatim, an empty value falls back. pi's own
	// env list has no PI_CONFIG_DIR and no PI_HOME — those are omp's.
	PiCodingAgentDir = "PI_CODING_AGENT_DIR"

	// DSHHome is taken literally as the DSH root: not trimmed, no tilde
	// expansion, a relative value resolves from the working directory. No
	// dsh binary is installed, so this mirrors beadle pkg/agent/dsh.go:39-46.
	DSHHome = "DSH_HOME"

	// DSHAgentsHome relocates the shared agents root DSH reads at rank 500.
	// beadle pkg/agent/dsh.go:80-86.
	DSHAgentsHome = "DSH_AGENTS_HOME"

	// OMP's four-variable chain, identical in both tools:
	// beadle pkg/agent/omp.go:27-30, 52-95; verger pkg/host/omp.go:984-1021.
	PIConfigDir      = "PI_CONFIG_DIR"
	PICodingAgentDir = "PI_CODING_AGENT_DIR"
	OMPProfile       = "OMP_PROFILE"
	PIProfile        = "PI_PROFILE"
)

// Directory names inside each host's base.
const (
	claudeDirName   = ".claude"
	claudeStateDoc  = ".claude.json"
	codexDirName    = ".codex"
	geminiDirName   = ".gemini"
	cursorDirName   = ".cursor"
	openCodeAppName = "opencode"
	kiloAppName     = "kilo"
	piDirName       = ".pi"
	piAgentDirName  = "agent"
	dshDirName      = ".dsh"
	ompDirName      = ".omp"
	ompAgentDirName = "agent"
	ompConfigFile   = "config.yml"
	ompProfilesDir  = "profiles"
	ompDefaultName  = "default"
	sharedDirName   = ".agents"
	configDirName   = ".config"
	kiloLegacyDir   = ".kilo"
	kiloPluginDir   = "plugin"
	kiloPluginsDir  = "plugins"
)

// JSON pointers for the two MCP dialects these hosts speak.
const (
	// mcpServersPointer is the pointer every JSON-dialect host except the
	// opencode family uses for its server map.
	mcpServersPointer = "/mcpServers"

	// mcpPointer is the opencode/kilo pointer. v2 nests the map one level
	// deeper under /mcp/servers; the file preference is carried separately.
	mcpPointer = "/mcp"
)

// Env is the input to every function here. GOOS selects the managed-policy
// layout; Lookup supplies the variables. A nil Lookup is an empty environment.
type Env struct {
	Home   string
	GOOS   string
	Lookup func(name string) (string, bool)
}

// get returns the value of name and whether it was set at all. The two are
// different questions: omp treats a defined-but-empty OMP_PROFILE as meaningful,
// while every other variable in this package treats empty as unset.
func (e Env) get(name string) (string, bool) {
	if e.Lookup == nil {
		return "", false
	}

	value, ok := e.Lookup(name)

	return value, ok
}

// literal returns a variable's value when it is SET, verbatim. The value is
// never trimmed: the hosts do not trim.
//
// Measured on the real binaries, fresh HOME per case (W1-A-verify-2 F1):
//
//	opencode 2.0.18  OPENCODE_CONFIG_DIR="  /tmp/x  "  → `opencode debug paths`
//	                reports `config /tmp/x  ` — the trailing spaces survive.
//	kilo 7.8.1       KILO_CONFIG_DIR="  /tmp/x  "      → the value is used
//	                verbatim, the resulting directory does not exist, and the
//	                server planted in the untrimmed path is NOT read. Trimming
//	                here would deliver into a directory kilo never reads.
//
// An UNSET variable, and one set to the empty string, are the only two cases
// that fall through to the default: `xdg-basedir` uses `||`, so for
// XDG_CONFIG_HOME empty is indistinguishable from unset, and the omp profile
// rule is a defined-ness check. Everything else is taken as the host reads it.
func (e Env) literal(name string) string {
	value, ok := e.get(name)
	if !ok {
		return ""
	}

	return value
}

// HostRoots is where one host keeps its configuration, before any surface is
// named.
type HostRoots struct {
	// ID is the canonical host id.
	ID string

	// ConfigRoot is the directory the host's own files live under. For most
	// hosts this is also the user-level config directory; for omp it is the
	// agent directory and for pi it is already the agent directory.
	ConfigRoot string

	// StateRoot is the directory holding the host's plugin/extension
	// registry. It differs from ConfigRoot only for omp, whose plugin state
	// is a sibling of agent/ rather than a child — the same conclusion
	// beadle reaches in pkg/agent/omp.go:128-135.
	StateRoot string

	// SettingsRoot is where the host's own settings document lives, when
	// that is not ConfigRoot. Only cursor differs: CURSOR_CONFIG_DIR moves
	// cli-config.json but not mcp.json.
	SettingsRoot string

	// Profile is the active profile name, empty for the default profile.
	Profile string

	// XDG reports whether ConfigRoot came from XDG_CONFIG_HOME.
	XDG bool
}

// HostSurfaces is one host's user-scope surface layout: a write target per
// component and, where a host reads more than one place, the ordered read list
// with the write target first.
type HostSurfaces struct {
	ID string

	// Rules is the user-level context/instruction document. Empty when the
	// host has no user-scope document (agy, cursor).
	Rules string

	// RulesPerFile is a directory of per-file rule documents — the surface
	// claude and cursor use for rules that apply conditionally.
	RulesPerFile string

	// RulesReads are the project instruction documents a host reads, with the
	// write target (ProjectSurfaces.Rules) first. Wider than one only for a
	// host that reads several names at one level — today claude, whose
	// project AGENTS.md is the file verger writes and which also reads
	// .claude/CLAUDE.md and CLAUDE.md beside it. A single-entry list is
	// filled from Rules by normalizeReadLists, exactly as the other read
	// lists are.
	RulesReads []string

	// MCPDoc is the single MCP document when the host has one. It is empty
	// for hosts that name their config file by extension search instead.
	MCPDoc string

	// MCPDocCandidates lists the config documents in the order the host
	// prefers them, when the choice depends on which file exists.
	MCPDocCandidates []string

	// MCPPointer is the JSON pointer holding the servers in MCPDoc.
	MCPPointer string

	// MCPTable is the TOML table holding the servers in MCPDoc. It is set
	// for Codex and empty for every JSON host.
	MCPTable string

	// Skills is the write target: the one directory verger writes. It is
	// always a member of SkillsReads.
	Skills string

	// SkillsReads is every directory the host reads skills from, in the
	// host's own precedence order — the order that decides which copy wins
	// a name clash. It is NOT a write order: the write target is Skills, and
	// for most hosts that is also the highest-precedence entry. Gemini is
	// the exception: the host reads the shared ~/.agents/skills above its own
	// ~/.gemini/skills within the same tier, so SkillsReads[0] there is the
	// shared hub and Skills is second. A consumer that wants to write must
	// use Skills, never Reads[0].
	//
	// The gemini order is not a guess. gemini-cli 0.46.0, installed on the
	// machine this was measured on, ships the rule in its own bundle at
	// lib/node_modules/@google/gemini-cli/bundle/docs/cli/skills.md:50-53:
	// "Within the same tier (user or workspace), the `.agents/skills/` alias
	// takes precedence over the `.gemini/skills/` directory." The alias IS
	// the shared hub, so the hub ranks first.
	//
	// Codex is the other host whose read order is NOT measured by a probe —
	// see fillCodex, which explains why and pins what it is.
	SkillsReads []string

	// Agents is the subagents write target, empty when the host has none.
	Agents string

	// AgentsReads is the precedence-ordered read list. Surfaces guarantees it
	// holds at least the write target whenever Agents is set, so it is never
	// nil for a host that has the surface.
	AgentsReads []string

	// Commands is the slash-command write target.
	Commands string

	// CommandsReads is the precedence-ordered read list, never nil when
	// Commands is set.
	CommandsReads []string

	// PluginModules are the directories a host loads plugin *modules* from.
	// This is not the plugin registry a host lists; it is the code channel a
	// runtime shim is dropped into. It is empty for hosts with none, and
	// never nil when PluginModulesWrite is set.
	PluginModules []string

	// PluginModulesWrite is the one directory a shim is written to, and is
	// always a member of PluginModules.
	PluginModulesWrite string

	// ProjectsDir is a per-project memory directory the host owns (today only
	// Claude Code). beadle pkg/agent/memory.go:20-25, agents.go:53-55.
	ProjectsDir string

	// Inbox is the host's append-only inbox file, or "" when the host has
	// none. beadle pkg/agent/inbox_path.go:8-32.
	Inbox string

	// SharedSkillsWrite is the cross-vendor ~/.agents/skills hub as a WRITE
	// target for a host that owns it. beadle pkg/agent/agents.go:585-599
	// registers that hub as its own pseudo-agent, whose skills surface is
	// ModeSync — beadle writes it, and the hosts that read it natively keep
	// their own copy. Without this field a consumer can see the hub in
	// SkillsReads and has nowhere to write it.
	SharedSkillsWrite string

	// StateDir is a host-owned state directory that is neither config nor
	// plugin registry (today Antigravity's `antigravity-cli` state dir).
	// beadle pkg/agent/agents.go:289-292.
	StateDir string

	// AdapterProbes are the paths whose presence tells us a host's optional
	// adapter is installed (today Pi's pi-mcp-adapter). beadle
	// pkg/agent/pi_adapter.go:26-40.
	AdapterProbes []string

	// ProfilesDir is the directory holding a host's profile subdirectories.
	// The resolver names it and never lists it: this package is pure, so the
	// listing is ListProfiles' job. Empty for a host with no profiles.
	ProfilesDir string

	// Markers are the host-identity facts a detect or configured check needs
	// — the config document a host writes on first run and the binary names
	// that identify it. They are identity, not configuration roots, which is
	// why they are a field here rather than something a consumer re-derives
	// from Roots. Empty for a host with no such marker.
	Markers HostMarkers

	// IgnoreRoots are directories under a surface the host itself owns, which
	// a canon walk must skip rather than present as user content. beadle
	// pkg/agent/agents.go:65,156,243,346,429,494,592.
	IgnoreRoots []string

	// ConfigReads are the configuration ROOTS this host reads, in precedence
	// order, with the write target (Roots.ConfigRoot) first. It is wider than
	// one only for a host that unions its roots — today kilo, whose
	// KILO_CONFIG_DIR and effective XDG root are both read. Every other host
	// reads exactly where it writes, so the list is the single root.
	ConfigReads []string

	// Hooks is the hook document. Empty for hosts with no declarative hook
	// surface (omp: modules only; opencode and kilo: plugin modules only).
	Hooks string

	// HooksEmbedded reports that the hooks live inside another document
	// (claude and gemini embed them in settings.json) rather than in a
	// hooks file of their own.
	HooksEmbedded bool

	// HookModules is the directory host hook modules live in, for the one
	// host that has one.
	HookModules string

	// Plugins is the plugin/extension registry directory.
	Plugins string

	// Settings is the host's own settings document, when it is not the
	// hooks document.
	Settings string

	// SharedAgents is the cross-vendor ~/.agents root this host reads.
	SharedAgents string
}

// HostMarkers are a host's identity facts: the config document that proves the
// host has run, and the binary names that identify it. They are deliberately
// NOT part of HostRoots — a marker is not a root, and folding one into the root
// rules would let a root change move a marker that never moved.
type HostMarkers struct {
	// ConfigFile is the document the host writes on first run, so its
	// presence is proof the host is installed and configured.
	ConfigFile string

	// Binaries are the executable names that identify the host on PATH, in
	// preference order. A host with no verified binary name has none; a
	// consumer must not invent one from the host id.
	Binaries []string
}

// All returns every canonical host id, in the order the READMEs list them.
func All() []string {
	return []string{Claude, Codex, Gemini, Agy, Cursor, OpenCode, Kilo, Pi, DSH, Omp}
}

// Known reports whether id is one of the canonical ids.
func Known(id string) bool {
	return slices.Contains(All(), id)
}

// UnknownIDError reports an id outside the canonical set.
type UnknownIDError struct {
	ID string
}

func (e *UnknownIDError) Error() string {
	ids := slices.Clone(All())
	slices.Sort(ids)

	return fmt.Sprintf("unknown host id %q; known ids: %s", e.ID, strings.Join(ids, ", "))
}

// Roots resolves one host's roots. It is pure: the same Env always produces the
// same HostRoots, and nothing on disk is consulted.
func Roots(id string, env Env) (HostRoots, error) {
	resolve, ok := rootResolvers[id]
	if !ok {
		return HostRoots{}, &UnknownIDError{ID: id}
	}

	if env.Home == "" {
		return HostRoots{}, fmt.Errorf("%s: Env.Home is required", id)
	}

	roots := HostRoots{ID: id, ConfigRoot: env.Home, StateRoot: env.Home, SettingsRoot: env.Home}
	if err := resolve(&roots, env); err != nil {
		return HostRoots{}, err
	}

	if roots.StateRoot == env.Home && id != Omp {
		roots.StateRoot = roots.ConfigRoot
	}

	if roots.SettingsRoot == env.Home {
		roots.SettingsRoot = roots.ConfigRoot
	}

	return roots, nil
}

// rootResolvers is the per-host root table. Each entry is one host's whole
// rule set, so Roots itself stays a lookup and every host's behaviour is
// readable in one place.
var rootResolvers = map[string]func(*HostRoots, Env) error{
	Claude: func(r *HostRoots, env Env) error {
		// CLAUDE_CONFIG_DIR wins wholesale; otherwise ~/.claude.
		r.ConfigRoot = orDefault(env.literal(ClaudeConfigDir), filepath.Join(env.Home, claudeDirName))

		return nil
	},
	Codex: func(r *HostRoots, env Env) error {
		value := env.literal(CodexHome)
		if value != "" && !filepath.IsAbs(value) {
			// The host ACCEPTS a relative CODEX_HOME whenever it resolves
			// against the working directory (probed on macOS and Linux
			// 0.158.0: `codex mcp add` wrote <cwd>/relx/config.toml). It
			// errors only when the value cannot be resolved. This package is
			// pure — no filesystem, no working directory — so it cannot tell
			// a resolvable relative value from a dead one. Refusing is the
			// safe half of that ignorance: a caller that passes a relative
			// value gets an error instead of a root computed against the
			// wrong directory. This is a DELIBERATE DEVIATION from the host,
			// not the host's rule; W2-HOSTPATH-1.md §fix records it.
			return fmt.Errorf("%s: %s must be an absolute path, got %q", Codex, CodexHome, value)
		}

		r.ConfigRoot = orDefault(value, filepath.Join(env.Home, codexDirName))

		return nil
	},
	Gemini: func(r *HostRoots, env Env) error {
		// GEMINI_CLI_HOME replaces the *home*; the host appends .gemini.
		// Live both platforms: GEMINI_CLI_HOME=$T/g created $T/g/.gemini/.
		r.ConfigRoot = filepath.Join(orDefault(env.literal(GeminiCLIHome), env.Home), geminiDirName)

		return nil
	},
	Agy: func(r *HostRoots, env Env) error {
		// Antigravity lives inside the gemini tree. No agy binary is
		// installed, so this mirrors beadle pkg/agent/agents.go:272-274.
		r.ConfigRoot = filepath.Join(env.Home, geminiDirName)

		return nil
	},
	Cursor: func(r *HostRoots, env Env) error {
		// The MCP, skills, agents, commands and hooks documents are all
		// home-relative with no override — verified live on both platforms.
		// Only the settings document moves.
		r.ConfigRoot = filepath.Join(env.Home, cursorDirName)
		r.SettingsRoot = orDefault(env.literal(CursorConfigDir), r.ConfigRoot)

		return nil
	},
	OpenCode: func(r *HostRoots, env Env) error {
		// OPENCODE_CONFIG_DIR replaces the root outright and outranks XDG; it
		// is used verbatim, a relative value included, because that is how
		// the host reads it (W1-A §fix F2, live on 2.0.18). XDG is the
		// fallback, and XDG still reports true/false so a caller can tell the
		// two apart.
		if value := env.literal(OpenCodeConfigDir); value != "" {
			r.ConfigRoot = value

			return nil
		}

		r.ConfigRoot, r.XDG = xdgApp(env, openCodeAppName)

		return nil
	},
	Kilo: func(r *HostRoots, env Env) error {
		// KILO_CONFIG_DIR replaces the XDG root; XDG_CONFIG_HOME is the
		// fallback. Live both platforms: kilo mcp list read the server from
		// both. Neither tool models this today.
		if value := env.literal(KiloConfigDir); value != "" {
			r.ConfigRoot = value

			return nil
		}

		r.ConfigRoot, r.XDG = xdgApp(env, kiloAppName)

		return nil
	},
	Pi: func(r *HostRoots, env Env) error {
		// pi expands only a leading ~ and treats empty as unset.
		r.ConfigRoot = orDefault(expandTilde(env.literal(PiCodingAgentDir), env.Home),
			filepath.Join(env.Home, piDirName, piAgentDirName))

		return nil
	},
	DSH: func(r *HostRoots, env Env) error {
		// DSH takes the value literally: no tilde expansion, and a relative
		// value resolves from the working directory (beadle
		// pkg/agent/dsh.go:39-46, which documents that as the host rule).
		// This package trims only to decide whether the value is set; for
		// every value but a whitespace-only one the result is identical.
		r.ConfigRoot = orDefault(env.literal(DSHHome), filepath.Join(env.Home, dshDirName))

		return nil
	},
	Omp: func(r *HostRoots, env Env) error {
		r.resolveOmp(env)

		return nil
	},
}

// resolveOmp fills in omp's roots. Live matrix from `omp config path` on both
// platforms:
//
//	(none)                            $HOME/.omp/agent
//	PI_CONFIG_DIR=alt                 $HOME/alt/agent
//	PI_CONFIG_DIR=/abs                $HOME/abs/agent
//	PI_CONFIG_DIR=                    $HOME/.omp/agent          (empty ignored)
//	PI_CODING_AGENT_DIR=/pa          /pa
//	OMP_PROFILE=work                  $HOME/.omp/profiles/work/agent
//	OMP_PROFILE=default               $HOME/.omp/agent
//	OMP_PROFILE= PI_PROFILE=work      $HOME/.omp/agent          (defined-empty wins)
//	PI_PROFILE=work                   $HOME/.omp/profiles/work/agent
func (r *HostRoots) resolveOmp(env Env) {
	// PI_CONFIG_DIR names a directory *below* HOME, never a root of its own.
	base := filepath.Join(env.Home, ompDirName)
	if value := env.literal(PIConfigDir); value != "" {
		base = filepath.Join(env.Home, filepath.FromSlash(strings.TrimLeft(value, `/\`)))
	}

	r.Profile = ompProfileName(env)

	if r.Profile != "" {
		r.StateRoot = filepath.Join(base, ompProfilesDir, r.Profile)
		r.ConfigRoot = filepath.Join(r.StateRoot, ompAgentDirName)

		return
	}

	r.StateRoot = base

	// A named profile wins over PI_CODING_AGENT_DIR, so this only applies to
	// the default profile.
	r.ConfigRoot = orDefault(expandTilde(env.literal(PICodingAgentDir), env.Home),
		filepath.Join(base, ompAgentDirName))
}

// ompProfileName applies omp's profile rule: OMP_PROFILE decides whenever it is
// SET — an explicitly empty value means the default profile and hides PI_PROFILE
// — and "default" is not a profile either.
func ompProfileName(env Env) string {
	for _, name := range []string{OMPProfile, PIProfile} {
		value, ok := env.get(name)
		if !ok {
			continue
		}

		// The one variable where trimming IS right: omp 18.4.2, live
		// (fresh HOME, `omp config path`), OMP_PROFILE="  work  " and
		// PI_PROFILE="  work  " both answer `$HOME/.omp/profiles/work/agent`
		// — the padding is dropped, the name is `work`.
		value = strings.TrimSpace(value)
		if value == "" || value == ompDefaultName {
			return ""
		}

		return value
	}

	return ""
}

// xdgApp resolves an XDG-based host. xdg-basedir uses `||`, so an empty value is
// indistinguishable from unset, and a relative value is joined verbatim
// without resolution — both are unusable as a root here, so both fall back.
func xdgApp(env Env, app string) (string, bool) {
	if value := env.literal(XDGConfigHome); value != "" && filepath.IsAbs(value) {
		return filepath.Join(value, app), true
	}

	return filepath.Join(env.Home, configDirName, app), false
}

// Surfaces resolves one host's user-scope surfaces. Roots are the input, so the
// caller resolves once and asks for surfaces per host with the same Env.
func Surfaces(id string, env Env) (HostSurfaces, error) {
	roots, err := Roots(id, env)
	if err != nil {
		return HostSurfaces{}, err
	}

	fill, ok := surfaceFillers[id]
	if !ok {
		return HostSurfaces{}, &UnknownIDError{ID: id}
	}

	s := HostSurfaces{ID: id, SharedAgents: filepath.Join(env.Home, sharedDirName)}
	fill(&s, roots, env)
	s.normalizeReadLists()

	return s, nil
}

// normalizeReadLists makes every read list non-nil whenever its write target
// exists, so a consumer can range over a read list without special-casing the
// hosts that read from exactly one place. A host with no write target for a
// component keeps an empty list, which means "this host has no such surface".
func (s *HostSurfaces) normalizeReadLists() {
	s.SkillsReads = readListOrSingle(s.SkillsReads, s.Skills)
	s.AgentsReads = readListOrSingle(s.AgentsReads, s.Agents)
	s.CommandsReads = readListOrSingle(s.CommandsReads, s.Commands)
	s.PluginModules = readListOrSingle(s.PluginModules, s.PluginModulesWrite)
}

// readListOrSingle returns reads when a host declared them, and the single
// write target otherwise. It never invents a path: an empty write target with
// no declared reads stays empty.
func readListOrSingle(reads []string, write string) []string {
	if len(reads) > 0 {
		return reads
	}

	if write == "" {
		return nil
	}

	return []string{write}
}

// surfaceFillers is the per-host surface table: one function per host, each
// writing that host's whole surface layout in one readable block.
var surfaceFillers = map[string]func(*HostSurfaces, HostRoots, Env){
	Claude:   fillClaude,
	Codex:    fillCodex,
	Gemini:   fillGemini,
	Agy:      fillAgy,
	Cursor:   fillCursor,
	OpenCode: fillOpenCode,
	Kilo:     fillKilo,
	Pi:       fillPi,
	DSH:      fillDSH,
	Omp:      fillOmp,
}

// fillClaude writes the claude surface. Live macOS 2.1.283: settings.json
// carries a "hooks" key and there is no hooks.json; the user-scope MCP
// document is ~/.claude.json, a sibling of ~/.claude rather than a child.
func fillClaude(s *HostSurfaces, roots HostRoots, env Env) {
	s.Rules = filepath.Join(roots.ConfigRoot, "CLAUDE.md")
	// The state document moves with the config root when the root is
	// overridden, and sits beside it when it is not. Probed live on 2.1.283
	// with a fresh HOME each time:
	//
	//	(no override)        claude mcp add → $HOME/.claude.json
	//	CLAUDE_CONFIG_DIR=O  claude mcp add → O/.claude.json, and $HOME/.claude.json
	//	                    is never created
	//
	// so "the parent of the config root" is right only for the default.
	if _, set := env.get(ClaudeConfigDir); set && env.literal(ClaudeConfigDir) != "" {
		s.MCPDoc = filepath.Join(roots.ConfigRoot, claudeStateDoc)
	} else {
		s.MCPDoc = filepath.Join(env.Home, claudeStateDoc)
	}

	s.MCPPointer = mcpServersPointer
	s.Skills = filepath.Join(roots.ConfigRoot, "skills")
	s.SkillsReads = []string{s.Skills}
	s.Agents = filepath.Join(roots.ConfigRoot, "agents")
	s.Commands = filepath.Join(roots.ConfigRoot, "commands")
	s.Hooks = filepath.Join(roots.ConfigRoot, "settings.json")
	s.HooksEmbedded = true
	s.Plugins = filepath.Join(roots.ConfigRoot, "plugins")
	// beadle pkg/agent/memory.go:20-25 (memoryDirName), agents.go:53-55.
	s.ProjectsDir = filepath.Join(roots.ConfigRoot, "projects")
	// beadle pkg/agent/inbox_path.go:18-19.
	s.Inbox = filepath.Join(roots.ConfigRoot, "inbox.md")
	// beadle pkg/agent/agents.go:65 — the host's own plugin tree is never
	// presented as canon skills.
	s.IgnoreRoots = []string{s.Plugins}
}

// fillCodex writes the codex surface. Live macOS 0.158.0: hooks.json is a
// standalone document and MCP lives in config.toml.
func fillCodex(s *HostSurfaces, roots HostRoots, env Env) {
	s.Rules = filepath.Join(roots.ConfigRoot, "AGENTS.md")
	s.MCPDoc = filepath.Join(roots.ConfigRoot, "config.toml")
	s.MCPTable = "mcp_servers"
	// beadle pkg/agent/inbox_path.go:24-25.
	s.Inbox = filepath.Join(roots.ConfigRoot, "inbox.md")
	// Codex reads BOTH skill roots, and the order below is now MEASURED rather
	// than inherited from beadle.
	//
	// Probe, codex-cli 0.158.0, an isolated HOME, the same skill (`probe`) in
	// each directory, `codex debug prompt-input`:
	//
	//	skill roots table:  r0 = <home>/.codex/skills
	//	                    r1 = <home>/.agents/skills
	//	                    r2 = <home>/.codex/skills/.system
	//	available skills:  probe: ORDER PROBE (file: r1/probe/SKILL.md)   <- hub FIRST
	//	                   probe: ORDER PROBE (file: r0/probe/SKILL.md)   <- own dir second
	//
	// Two things follow, and they are different facts. The host ENUMERATES its
	// own directory first (r0), but PRESENTS the available skills hub-first,
	// and that presentation order is what SkillsReads models. And codex does
	// not de-duplicate: both copies of a clashing name are surfaced, so this
	// list is a presentation order, not a name-clash resolution — a consumer
	// must not read it as "the hub wins".
	//
	// The earlier comment here said the order followed beadle because the
	// probe could not rank the two roots. The probe still cannot rank them
	// (both are surfaced), but it does establish the order they are offered in,
	// and the order below matches it. Pinned by TestCodexSkillsReadOrderIsPinned.
	s.Skills = filepath.Join(env.Home, sharedDirName, "skills")
	s.SkillsReads = []string{s.Skills, filepath.Join(roots.ConfigRoot, "skills")}
	// Codex is the one host whose skills write target IS the shared hub, so
	// for codex the hub is a write surface and not only a read one. beadle
	// registers the hub as its own ModeSync pseudo-agent
	// (pkg/agent/agents.go:585-599), which is the same fact from the other
	// side.
	s.SharedSkillsWrite = s.Skills
	s.Agents = filepath.Join(roots.ConfigRoot, "agents")
	s.Commands = filepath.Join(roots.ConfigRoot, "prompts")
	s.Hooks = filepath.Join(roots.ConfigRoot, "hooks.json")
	// Gap 1: the same claude plugin tree is skipped for codex (beadle
	// pkg/agent/agents.go:435).
	s.IgnoreRoots = claudePluginTreeIgnore(env.Home)
	// Gap 3: `codex` is the verified binary name.
	s.Markers = HostMarkers{Binaries: []string{"codex"}}
}

// fillGemini writes the gemini surface. The read ORDER is quoted from the
// doc gemini 0.46.0 ships inside its own installed bundle:
// /opt/homebrew/Cellar/gemini-cli/0.46.0/libexec/lib/node_modules/@google/
// gemini-cli/bundle/docs/cli/skills.md:50-53 — "Within the same tier (user
// or workspace), the `.agents/skills/` alias takes precedence over the
// `.gemini/skills/` directory."
// This is the one host where the write target is not the first read entry.
// Writing to the shared hub would leak into every other host that reads it,
// which is exactly what the orchestrator's omp decision forbids — hence
// Skills, not Reads[0].
func fillGemini(s *HostSurfaces, roots HostRoots, env Env) {
	s.Rules = filepath.Join(roots.ConfigRoot, "GEMINI.md")
	s.MCPDoc = filepath.Join(roots.ConfigRoot, "settings.json")
	s.MCPPointer = mcpServersPointer
	s.Skills = filepath.Join(roots.ConfigRoot, "skills")
	s.SkillsReads = []string{filepath.Join(env.Home, sharedDirName, "skills"), s.Skills}
	s.Agents = filepath.Join(roots.ConfigRoot, "agents")
	s.Commands = filepath.Join(roots.ConfigRoot, "commands")
	s.Hooks = s.MCPDoc
	s.HooksEmbedded = true
	s.Plugins = filepath.Join(roots.ConfigRoot, "extensions")
	// beadle pkg/agent/inbox_path.go:20-21.
	s.Inbox = filepath.Join(roots.ConfigRoot, "inbox.md")
	// beadle pkg/agent/agents.go:243.
	s.IgnoreRoots = []string{filepath.Join(env.Home, claudeDirName, "plugins")}
}

// fillAgy writes the Antigravity surface: MCP and subagents only, both inside
// ~/.gemini/config. No agy binary is installed on either platform, so this
// mirrors beadle pkg/agent/agents.go:272-310 and is NOT host-verified.
func fillAgy(s *HostSurfaces, roots HostRoots, _ Env) {
	s.MCPDoc = filepath.Join(roots.ConfigRoot, "config", "mcp_config.json")
	s.MCPPointer = mcpServersPointer
	s.Agents = filepath.Join(roots.ConfigRoot, "config", "agents")
	// beadle pkg/agent/agents.go:289 — agy's own state dir, which is neither
	// its config nor its plugin registry.
	s.StateDir = filepath.Join(roots.ConfigRoot, "antigravity-cli")
	// The native install rung (DESIGN §4.2; beadle pkg/plugin/
	// source_antigravity.go:17-23 lists this root first). Not host-verified:
	// no agy binary on macOS or Linux.
	s.Plugins = filepath.Join(roots.ConfigRoot, "config", "plugins")
}

// fillCursor writes the cursor surface. Live macOS and Linux 2026.06.15:
// mcp.json is read from ~/.cursor and nowhere else — neither
// XDG_CONFIG_HOME nor CURSOR_CONFIG_DIR moves it, while CURSOR_CONFIG_DIR
// does move cli-config.json.
func fillCursor(s *HostSurfaces, roots HostRoots, env Env) {
	s.MCPDoc = filepath.Join(roots.ConfigRoot, "mcp.json")
	s.MCPPointer = mcpServersPointer
	s.Skills = filepath.Join(roots.ConfigRoot, "skills")
	s.SkillsReads = []string{
		s.Skills,
		filepath.Join(env.Home, claudeDirName, "skills"),
		filepath.Join(env.Home, sharedDirName, "skills"),
	}
	s.Agents = filepath.Join(roots.ConfigRoot, "agents")
	s.Commands = filepath.Join(roots.ConfigRoot, "commands")
	s.Hooks = filepath.Join(roots.ConfigRoot, "hooks.json")
	s.Settings = filepath.Join(roots.SettingsRoot, "cli-config.json")
	// beadle pkg/agent/inbox_path.go:22-23.
	s.Inbox = filepath.Join(roots.ConfigRoot, "inbox.md")
	// Gap 1: beadle pkg/agent/agents.go:346.
	s.IgnoreRoots = claudePluginTreeIgnore(env.Home)
	// Gap 3: the binary the host is invoked as.
	s.Markers = HostMarkers{Binaries: []string{"cursor-agent"}}
}

// fillOpenCode writes the opencode surface. OpenCode 2.0.18 declares plugins
// by path in the config's `plugins` array and does NOT scan a plugin
// directory, so there is no module directory to model for v2; the field is
// left empty rather than invented.
func fillOpenCode(s *HostSurfaces, roots HostRoots, env Env) {
	s.fillXDG(roots, env.Home, "opencode.jsonc", "opencode.json")
	// V2 reads AGENTS.md only.
	s.Rules = filepath.Join(roots.ConfigRoot, "AGENTS.md")
	// Gap 1: beadle pkg/agent/agents.go:156.
	s.IgnoreRoots = claudePluginTreeIgnore(env.Home)
	// Gap 3: `opencode` (2.0.18, probed live).
	s.Markers = HostMarkers{Binaries: []string{"opencode"}}
	// Gap 9: opencode HAS an inbox and beadle writes it — beadle
	// pkg/agent/inbox_path.go:36-43 carried a local line for exactly this,
	// keyed off roots(OpenCode, home).ConfigRoot, which is what this joins.
	// agy and dsh stay without one: beadle models an inbox for every host
	// except agy, opencode and dsh (inbox_path.go:36-39), and two of those
	// three are now covered.
	s.Inbox = filepath.Join(roots.ConfigRoot, "inbox.md")
}

// fillKilo writes the kilo surface, then widens the read lists to the pairs
// beadle pkg/agent/kilo_paths.go:39-65 pins: the host code globs the
// singular/plural pair in the config dir, the docs name the ~/.kilo pair.
// fillKilo writes the kilo surface.
//
// The config ROOT is where kilo WRITES: probed live on 7.8.1 with a fresh
// HOME, `kilo mcp add` created kilo.json under $KILO_CONFIG_DIR when that was
// set and kilo.jsonc under $XDG_CONFIG_HOME/kilo when it was, and nothing in
// ~/.config/kilo either time. So the root is the right write target.
//
// The config READS are wider than the root, which a chain cannot express.
// Measured live on 7.8.1, a server planted in each root:
//
//	KILO_CONFIG_DIR only, XDG unset → reads KILO_CONFIG_DIR and ~/.config/kilo
//	KILO_CONFIG_DIR + XDG_CONFIG_HOME → reads KILO_CONFIG_DIR and $XDG/kilo
//	XDG_CONFIG_HOME only              → reads $XDG/kilo alone
//
// so the read set is the union of the KILO_CONFIG_DIR root and the effective
// XDG root, and the home default is read exactly when XDG is unset — which is
// the effective XDG root. Every probed root stays visible to the host, so this
// is a visibility model, not round 1's invisible delivery.
func fillKilo(s *HostSurfaces, roots HostRoots, env Env) {
	s.fillXDG(roots, env.Home, "kilo.jsonc", "kilo.json")
	s.Rules = filepath.Join(roots.ConfigRoot, "AGENTS.md")
	s.ConfigReads = kiloConfigReads(roots, env)
	s.SkillsReads = []string{
		filepath.Join(roots.ConfigRoot, "skills"),
		filepath.Join(roots.ConfigRoot, "skill"),
		filepath.Join(env.Home, kiloLegacyDir, "skills"),
		filepath.Join(env.Home, kiloLegacyDir, "skill"),
	}
	s.Skills = s.SkillsReads[0]
	// beadle pkg/agent/subagents_kilo.go:15-20 — the host globs the legacy
	// {modes,mode} pair alongside the {agents,agent} one.
	s.AgentsReads = append(s.AgentsReads,
		filepath.Join(roots.ConfigRoot, "modes"),
		filepath.Join(roots.ConfigRoot, "mode"),
	)
	// beadle pkg/agent/inbox_path.go:28-29.
	s.Inbox = filepath.Join(roots.ConfigRoot, "inbox.md")
	// Kilo 7.8.1, probed live on this mac under an isolated HOME: a module
	// placed in EITHER <config>/kilo/plugin/ or <config>/kilo/plugins/ is
	// executed by `kilo mcp list`. The singular directory is the write
	// target (it is the one verger's runtime shim uses); both are read.
	// NOTE for the shim author: the loader takes TypeScript. A `.mjs` module
	// planted in the same directory was NOT loaded by the host, so the
	// directory is right but the extension is not free.
	s.PluginModulesWrite = filepath.Join(roots.ConfigRoot, kiloPluginDir)
	s.PluginModules = []string{s.PluginModulesWrite, filepath.Join(roots.ConfigRoot, kiloPluginsDir)}
	// Gap 1: beadle pkg/agent/kilo_paths.go:93.
	s.IgnoreRoots = claudePluginTreeIgnore(env.Home)
	// Gap 3: `kilo` (7.8.1, probed live).
	s.Markers = HostMarkers{Binaries: []string{"kilo"}}
}

// fillPi writes the pi surface. pi has no subagents and no declarative hooks;
// its mcp.json is read only by the third-party pi-mcp-adapter
// (beadle pkg/agent/agents.go:440-449).
func fillPi(s *HostSurfaces, roots HostRoots, env Env) {
	s.Rules = filepath.Join(roots.ConfigRoot, "AGENTS.md")
	s.MCPDoc = filepath.Join(roots.ConfigRoot, "mcp.json")
	s.MCPPointer = mcpServersPointer
	s.Skills = filepath.Join(roots.ConfigRoot, "skills")
	s.SkillsReads = []string{s.Skills, filepath.Join(env.Home, sharedDirName, "skills")}
	s.Commands = filepath.Join(roots.ConfigRoot, "prompts")
	s.Plugins = filepath.Join(roots.ConfigRoot, "extensions")
	// beadle pkg/agent/inbox_path.go:26-27.
	s.Inbox = filepath.Join(roots.ConfigRoot, "inbox.md")
	// beadle pkg/agent/pi_adapter.go:26-40 — the adapter counts as present
	// when any of these exists, so a consumer can report it without
	// re-deriving the rule.
	s.AdapterProbes = []string{
		filepath.Join(roots.ConfigRoot, "settings.json"),
		filepath.Join(roots.ConfigRoot, "packages", "extensions"),
		filepath.Join(roots.ConfigRoot, "node_modules", "pi-mcp-adapter"),
		filepath.Join(roots.ConfigRoot, "extensions", "pi-mcp-adapter"),
	}
	// Gap 1: beadle pkg/agent/agents.go:500.
	s.IgnoreRoots = claudePluginTreeIgnore(env.Home)
	// Gap 3: `pi` (0.74.2).
	s.Markers = HostMarkers{Binaries: []string{"pi"}}
}

// fillDSH writes the DSH surface. DSH's MCP is a YAML patch layer, not a JSON
// document, and DSH has no subagents, commands or plugin registry.
func fillDSH(s *HostSurfaces, roots HostRoots, env Env) {
	s.Rules = filepath.Join(roots.ConfigRoot, "AGENTS.md")
	s.MCPDoc = filepath.Join(roots.ConfigRoot, "cordis.patch.yml")
	s.Skills = filepath.Join(roots.ConfigRoot, "skills")
	s.SharedAgents = dshSharedAgents(env, env.Home)
	// Gap 1: beadle pkg/agent/dsh.go:146.
	s.IgnoreRoots = claudePluginTreeIgnore(env.Home)
	// Gap 2: the profiles directory; the listing is ListProfiles' job.
	s.ProfilesDir = filepath.Join(roots.ConfigRoot, ompProfilesDir)
	// Gap 3: `dsh`, and the harness home env the host itself reads.
	s.Markers = HostMarkers{Binaries: []string{"dsh"}}
	s.SkillsReads = []string{s.Skills, filepath.Join(s.SharedAgents, "skills")}
}

// fillOmp writes the omp surface. Live macOS 18.4.2: `omp --help` exposes
// hooks only as --hook=<value> file loading, so there is no hook document,
// only a modules directory.
func fillOmp(s *HostSurfaces, roots HostRoots, env Env) {
	s.Rules = filepath.Join(roots.ConfigRoot, "AGENTS.md")
	s.MCPDoc = filepath.Join(roots.ConfigRoot, "mcp.json")
	s.MCPPointer = mcpServersPointer
	// The write target is the host's own skills directory, not the shared
	// ~/.agents hub: a skill written to the hub is visible to every other
	// host that reads it, which is a leak across agents the user did not
	// ask for. The shared hub stays in the read list, below the native dir,
	// because the native provider outranks it by name.
	s.SkillsReads = []string{
		filepath.Join(roots.ConfigRoot, "skills"),
		filepath.Join(env.Home, sharedDirName, "skills"),
	}
	s.Skills = s.SkillsReads[0]
	s.Agents = filepath.Join(roots.ConfigRoot, "agents")
	s.Commands = filepath.Join(roots.ConfigRoot, "commands")
	s.HookModules = filepath.Join(roots.ConfigRoot, "hooks")
	s.Plugins = filepath.Join(roots.StateRoot, "plugins")
	// beadle pkg/agent/inbox_path.go:30-31.
	s.Inbox = filepath.Join(roots.ConfigRoot, "inbox.md")
	// Gap 1: beadle pkg/agent/omp.go:222.
	s.IgnoreRoots = claudePluginTreeIgnore(env.Home)
	// Gap 2: the profiles directory; beadle pkg/agent/omp.go:151 lists it.
	s.ProfilesDir = filepath.Join(roots.StateRoot, ompProfilesDir)
	// Gap 3: the config document omp writes on first run
	// (beadle pkg/agent/omp.go:25,103,120) and the binary name (:26).
	//
	// ConfigRoot ALREADY ends in the agent directory (fillOmp's root rule is
	// <stateRoot>/agent, or <stateRoot>/profiles/<profile>/agent), so joining
	// ompAgentDirName again produced `agent/agent/config.yml`. beadle's own
	// ompMarker is filepath.Join(OmpAgentDir(home), ompConfigFile) — the agent
	// dir and the file, never the dir twice.
	s.Markers = HostMarkers{
		ConfigFile: filepath.Join(roots.ConfigRoot, ompConfigFile),
		Binaries:   []string{"omp"},
	}
}

// fillXDG fills the surfaces the opencode family shares: a config document
// chosen by extension preference, the singular/plural directory pair the host
// globs, and the cross-vendor skills alias.
func (s *HostSurfaces) fillXDG(roots HostRoots, home, jsonc, json string) {
	s.MCPDocCandidates = []string{
		filepath.Join(roots.ConfigRoot, jsonc),
		filepath.Join(roots.ConfigRoot, json),
	}
	// A host with neither file gets the plain one.
	s.MCPDoc = s.MCPDocCandidates[len(s.MCPDocCandidates)-1]
	s.MCPPointer = mcpPointer
	s.Skills = filepath.Join(roots.ConfigRoot, "skills")
	s.SkillsReads = []string{
		s.Skills,
		filepath.Join(home, sharedDirName, "skills"),
		filepath.Join(home, claudeDirName, "skills"),
	}
	s.Agents = filepath.Join(roots.ConfigRoot, "agents")
	s.AgentsReads = []string{s.Agents, filepath.Join(roots.ConfigRoot, "agent")}
	s.Commands = filepath.Join(roots.ConfigRoot, "commands")
	s.CommandsReads = []string{s.Commands, filepath.Join(roots.ConfigRoot, "command")}
}

// dshSharedAgents resolves DSH's rank-500 shared root.
func dshSharedAgents(env Env, home string) string {
	return orDefault(env.literal(DSHAgentsHome), filepath.Join(home, sharedDirName))
}

// ProjectSurfaces are the project-scope paths, relative to a project root.
// Unlike Roots this needs no environment: a project manifest is where the
// project puts it.
func ProjectSurfaces(id, project string) HostSurfaces {
	s := HostSurfaces{ID: id}

	switch id {
	case Claude:
		// From the host's own settings doc (claude-code directory layout):
		// `.mcp.json` carries project MCP servers and `.claude/rules/*.md`
		// are the per-file rules.
		//
		// The project instruction WRITE target is <project>/AGENTS.md, which
		// claude reads natively from v2.1.280 — beadle
		// pkg/agent/agents.go:111 writes exactly that path, and a resolver
		// that answered `.claude/CLAUDE.md` instead would send the digest
		// block somewhere beadle never reads. The other two names stay in
		// the READ list: the host reads all three, but only one of them is
		// ours to write.
		s.Rules = in(project, "AGENTS.md")
		s.RulesReads = []string{
			s.Rules,
			in(project, ".claude/CLAUDE.md"),
			in(project, "CLAUDE.md"),
		}
		s.MCPDoc = in(project, ".mcp.json")
		s.Skills = in(project, ".claude/skills")
		s.Agents = in(project, ".claude/agents")
		s.Commands = in(project, ".claude/commands")
		s.Hooks = in(project, ".claude/settings.json")
		s.HooksEmbedded = true
		s.RulesPerFile = in(project, ".claude/rules")
	case Codex:
		// Codex docs: the project tree mirrors the user one under
		// `<project>/.codex/` plus a root `AGENTS.md`, and it is read only
		// once the project is trusted.
		s.Rules = in(project, "AGENTS.md")
		s.MCPDoc = in(project, ".codex/config.toml")
		s.Skills = in(project, ".codex/skills")
		s.Agents = in(project, ".codex/agents")
		s.Commands = in(project, ".codex/prompts")
		s.Hooks = in(project, ".codex/hooks.json")
	case Gemini:
		// From the gemini 0.46.0 bundle's own docs (custom-commands.md,
		// core/subagents.md, tools/mcp-server.md): the project tree is
		// `<project>/.gemini/…`, and a workspace GEMINI.md is read per
		// directory upward.
		s.Rules = in(project, "GEMINI.md")
		s.MCPDoc = in(project, ".gemini/settings.json")
		s.Skills = in(project, ".gemini/skills")
		s.Agents = in(project, ".gemini/agents")
		s.Commands = in(project, ".gemini/commands")
		s.Hooks = s.MCPDoc
		s.HooksEmbedded = true
	case Agy:
		// No agy binary on either platform; beadle pkg/agent/agents.go:307.
		// There is no agy project RULES surface, which is why only MCP is set.
		s.MCPDoc = in(project, ".agents/mcp_config.json")
	case Cursor:
		// cursor.com/docs/cli + /docs/mcp: the project tree is
		// `<project>/.cursor/`, the project mcp.json wins over the user
		// one, and `.cursor/rules/*.mdc` is the only rules surface the
		// host has — it has no user-level rules document.
		s.MCPDoc = in(project, ".cursor/mcp.json")
		s.Skills = in(project, ".cursor/skills")
		s.Agents = in(project, ".cursor/agents")
		s.Hooks = in(project, ".cursor/hooks.json")
		s.RulesPerFile = in(project, ".cursor/rules")
	case OpenCode:
		// opencode's config assembly (packages/core/src/config.ts) walks
		// upward for `.opencode` directories from the opened directory, so
		// the project tree is a search path rather than one fixed root; this
		// is the checkout-root form of it. Not probe-verified: the live
		// opencode 2.0.18 probes covered the user root.
		s.Rules = in(project, "AGENTS.md")
		s.MCPDoc = in(project, "opencode.json")
		s.Skills = in(project, ".opencode/skills")
		s.Agents = in(project, ".opencode/agents")
		s.Commands = in(project, ".opencode/commands")
	case Kilo:
		// From the host's own source (kilocode packages/core/src/config/
		// paths.ts): the project config is kilo.jsonc, and `.kilo/` wins
		// over a bare kilo.jsonc when both exist. Not probe-verified: the
		// live kilo 7.8.1 probes in this task covered the user root only.
		s.Rules = in(project, "AGENTS.md")
		s.MCPDoc = in(project, ".kilo/kilo.json")
		s.Skills = in(project, ".kilo/skills")
		s.Agents = in(project, ".kilo/agents")
		s.Commands = in(project, ".kilo/commands")
	case Pi:
		// pi 0.74.2 dist/core/package-manager.js: project resources live
		// under <cwd>/.pi/ and are read BEFORE the user agent dir, so the
		// project tree is the higher-precedence one. Not probe-verified:
		// `pi list` is read-only and creates nothing.
		s.Rules = in(project, "AGENTS.md")
		s.MCPDoc = in(project, ".pi/mcp.json")
		s.Skills = in(project, ".pi/skills")
		s.Commands = in(project, ".pi/prompts")
		// Gap 5: the project-scope adapter probe, which is a DIFFERENT set
		// from the user-scope one above — beadle
		// pkg/agent/pi_adapter.go:29-44 checks <cwd>/.pi for the settings
		// file and the npm catalog, where the user scope is
		// <agentDir>/packages/extensions and <agentDir>/node_modules. The
		// resolver names the files; the settings CONTENT scan stays with the
		// consumer, because a path cannot say whether a file mentions the
		// adapter package.
		s.AdapterProbes = []string{
			in(project, ".pi/settings.json"),
			in(project, ".pi/npm/node_modules/pi-mcp-adapter"),
			in(project, ".pi/extensions/pi-mcp-adapter"),
		}
	case DSH:
		// DSH walks every AGENTS.md and CLAUDE.md from the git root down to
		// the working directory, so there is no single project path. No dsh
		// binary is installed on either platform; this mirrors beadle
		// pkg/agent/dsh_chain.go:16,87-113.
		s.Rules = in(project, "AGENTS.md")
		s.Skills = in(project, ".dsh/skills")
	case Omp:
		// omp's native project root is the nearest non-empty .omp/, not the
		// checkout root — beadle pkg/agent/omp.go:35-36, 261-262. Creating
		// it also stops omp looking at farther .omp directories up the tree.
		s.Rules = in(project, ".omp/AGENTS.md")
		s.MCPDoc = in(project, ".omp/mcp.json")
		s.Skills = in(project, ".omp/skills")
		s.Agents = in(project, ".omp/agents")
		s.Commands = in(project, ".omp/commands")
	}

	return s
}

// ManagedPolicy lists the administrator-owned documents that override a user's
// own configuration, in ascending order of precedence. An empty list means the
// host has none on this platform — which is a fact about the host, not an
// omission. An unknown GOOS is an error: a silent fallback would put a macOS
// path in a Linux tree, or worse.
func ManagedPolicy(id, goos string) ([]string, error) {
	if !Known(id) {
		return nil, &UnknownIDError{ID: id}
	}

	switch goos {
	case "darwin":
		return macPolicy(id), nil
	case "linux":
		return linuxPolicy(id), nil
	default:
		return nil, fmt.Errorf("%s: unsupported GOOS %q", id, goos)
	}
}

func macPolicy(id string) []string {
	switch id {
	case Claude:
		return []string{
			"/Library/Application Support/ClaudeCode/managed-settings.json",
			"/Library/Application Support/ClaudeCode/managed-settings.d",
			"/Library/Application Support/ClaudeCode/managed-mcp.json",
		}
	case Gemini:
		return []string{
			"/Library/Application Support/GeminiCli/system-defaults.json",
			"/Library/Application Support/GeminiCli/settings.json",
			"/Library/Application Support/GeminiCli/policies",
		}
	case Codex:
		// Codex is /etc on macOS too — the host treats macOS as Unix.
		return codexPolicy()
	default:
		// Cursor's macOS policy is an MDM profile, not a file. opencode,
		// kilo, pi, dsh and omp have no administrator document. agy returns
		// nil too: it is NOT known to share the Gemini policy documents, and
		// no agy binary is installed to check. Claiming Gemini's list here
		// would make a policy gate look like it covers a document nobody
		// has evidence for.
		return nil
	}
}

func linuxPolicy(id string) []string {
	switch id {
	case Claude:
		return []string{
			"/etc/claude-code/managed-settings.json",
			"/etc/claude-code/managed-settings.d",
			"/etc/claude-code/managed-mcp.json",
		}
	case Gemini:
		return []string{
			"/etc/gemini-cli/system-defaults.json",
			"/etc/gemini-cli/settings.json",
			"/etc/gemini-cli/policies",
		}
	case Codex:
		return codexPolicy()
	default:
		// Cursor's Linux policy lives at ~/.cursor/policy.json, a user
		// directory rather than an /etc path, so it is not a managed
		// document here. Live Linux: /etc/cursor does not exist.
		return nil
	}
}

func codexPolicy() []string {
	return []string{
		"/etc/codex/requirements.toml",
		"/etc/codex/config.toml",
		"/etc/codex/managed_config.toml",
	}
}

// in joins a project-relative slash path, mapping "" to "" so a component the
// host does not have stays absent instead of becoming the project root.
func in(project, rel string) string {
	if rel == "" {
		return ""
	}

	return filepath.Join(project, filepath.FromSlash(rel))
}

// expandTilde expands a leading ~ or ~/ against home, matching pi's own rule
// (pi 0.74.2 dist/config.js:345-351, 362-368: getAgentDir() returns
// expandTildePath(envDir) and a relative value is returned UNCHANGED).
// The two hosts sharing this helper differ on a relative value, and the
// difference is real: pi returns it verbatim (quoted above), while omp
// resolves it against the working directory — live on both platforms,
// PI_CODING_AGENT_DIR=relx → `omp config path`
// answered <cwd>/relx. A pure package has no working directory, so omp's
// resolution is the caller's job; this helper does not invent one.
func expandTilde(value, home string) string {
	switch {
	case value == "~":
		return home
	case strings.HasPrefix(value, "~/"):
		return filepath.Join(home, filepath.FromSlash(value[2:]))
	default:
		return value
	}
}

// orDefault returns value when it is non-empty and fallback otherwise.
func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}

// kiloConfigReads is kilo's ordered configuration-root read list: the write
// target first, then the other root the host also reads. The evidence for the
// union is the live matrix quoted on fillKilo.
func kiloConfigReads(roots HostRoots, env Env) []string {
	reads := []string{roots.ConfigRoot}

	effective, _ := xdgApp(env, kiloAppName)
	if effective != roots.ConfigRoot {
		reads = append(reads, effective)
	}

	return reads
}

// claudePluginsDir is claude's own plugin tree, which lives under the HOME
// directory and NOT under a CLAUDE_CONFIG_DIR that may have been moved
// elsewhere: beadle carries the literal home-relative path in nine places
// (pkg/agent/agents.go:156,243,346,435,500,598, dsh.go:146, kilo_paths.go:93,
// omp.go:222) and the same walk must be skipped for every one of them, so it
// is one helper here rather than nine. Home-relative on purpose: a relocated
// config dir does not relocate the plugin tree the walk must not descend into.
func claudePluginsDir(home string) string {
	return filepath.Join(home, claudeDirName, "plugins")
}

// claudePluginTreeIgnore is the ignore root for the hosts whose own skills
// surface is wide enough to reach into claude's plugin tree — the hosts that
// read the shared hub, plus the shared hub itself. Claude and Gemini set
// IgnoreRoots in their own fillers; these are the rest.
func claudePluginTreeIgnore(home string) []string {
	return []string{claudePluginsDir(home)}
}

// ListProfiles returns the profile directory names under dir, sorted, and nil
// when dir cannot be read. It is the one function in this package that touches
// the filesystem, and it is deliberately not a method on HostSurfaces: Roots
// and Surfaces stay pure, so a consumer resolves roots in tests and lists
// profiles only where it already has a home. A missing directory is not an
// error — a host with no profiles yet has none.
func ListProfiles(dir string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list profiles in %s: %w", dir, err)
	}

	names := make([]string, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}

	slices.Sort(names)

	return names, nil
}

// ProjectChainRoot returns the project root of cwd: the nearest directory at
// or above cwd that holds a .git entry (a directory, or a worktree file). It
// reports false when there is none, and a project with no .git above cwd has
// no chain. exists is injected rather than taken from os.Stat so the walk
// stays testable and this package keeps its purity.
func ProjectChainRoot(cwd string, exists func(string) bool) (string, bool) {
	if cwd == "" {
		return "", false
	}

	for dir := filepath.Clean(cwd); ; {
		if exists(filepath.Join(dir, ".git")) {
			return dir, true
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}

		dir = parent
	}
}

// DSHChainCandidates are the instruction file names DSH reads in EVERY
// directory of a project chain, in the documented order (beadle
// pkg/agent/dsh_chain.go:14-16).
func DSHChainCandidates() []string {
	return []string{"AGENTS.md", "CLAUDE.md"}
}

// DSHProjectChain returns the instruction files DSH reads for a project: every
// AGENTS.md and CLAUDE.md between root and cwd inclusive, root first, each
// directory's candidates in DSHChainCandidates order. It is a function and
// not a HostSurfaces field because it is a WALK, not a path — encoding it as
// a single field would force the caller to re-derive the same walk (beadle
// pkg/agent/dsh_chain.go:87-149).
//
// The result is a candidate list, not a list of existing files: a name with no
// file is inert, so deleting a file prunes its element rather than shifting
// the chain. A caller that needs only the files that exist filters with its
// own existence check; this package does not stat them.
func DSHProjectChain(root, cwd string) []string {
	if root == "" {
		return nil
	}

	root = filepath.Clean(root)
	dirs := []string{root}

	rel, err := filepath.Rel(root, filepath.Clean(cwd))
	if err != nil || rel == "." {
		rel = ""
	}

	dir := root

	for part := range strings.SplitSeq(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." {
			continue
		}

		dir = filepath.Join(dir, part)
		dirs = append(dirs, dir)
	}

	chain := make([]string, 0, len(dirs)*2)

	for _, dir := range dirs {
		for _, name := range DSHChainCandidates() {
			chain = append(chain, filepath.Join(dir, name))
		}
	}

	return chain
}

// Shared is the cross-vendor ~/.agents hub as its own surface, not a host.
// beadle models it as a `shared` pseudo-agent (pkg/agent/agentid.go:25) that
// writes ~/.agents/skills while the hosts that read it natively keep their own
// copy. It is a function rather than an eleventh All() entry because it is
// not a host: it has no binary, no config root, no rules document and no hooks,
// and giving it a host id would invite Roots("shared") to invent all four.
// beadle pkg/agent/agents.go:585-599 is the write side this mirrors.
func Shared(env Env) HostSurfaces {
	root := filepath.Join(env.Home, sharedDirName)

	return HostSurfaces{
		ID:           "shared",
		Skills:       filepath.Join(root, "skills"),
		SkillsReads:  []string{filepath.Join(root, "skills")},
		SharedAgents: root,
		IgnoreRoots:  claudePluginTreeIgnore(env.Home),
	}
}
