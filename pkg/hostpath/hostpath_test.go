package hostpath_test

import (
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/hostpath"
)

// env builds an Env from a home, an OS and a fixed variable set.
func env(home, goos string, vars map[string]string) hostpath.Env {
	lookup := func(name string) (string, bool) {
		value, ok := vars[name]

		return value, ok
	}

	return hostpath.Env{Home: home, GOOS: goos, Lookup: lookup}
}

// none is an environment with no variables set at all.
func none(home, goos string) hostpath.Env {
	return env(home, goos, nil)
}

const (
	darwin = "darwin"
	linux  = "linux"
)

func TestRootsDefaults(t *testing.T) {
	Convey("with no variables set, every host lands on its documented default", t, func() {
		table := map[string]string{
			"claude":   "/h/.claude",
			"codex":    "/h/.codex",
			"gemini":   "/h/.gemini",
			"agy":      "/h/.gemini",
			"cursor":   "/h/.cursor",
			"opencode": "/h/.config/opencode",
			"kilo":     "/h/.config/kilo",
			"pi":       "/h/.pi/agent",
			"dsh":      "/h/.dsh",
			"omp":      "/h/.omp/agent",
		}

		for id, want := range table {
			got, err := hostpath.Roots(id, none("/h", darwin))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, want)
		}
	})
}

func TestRootsOverrides(t *testing.T) {
	Convey("Roots applies each host's own relocation rule", t, func() {
		Convey("CLAUDE_CONFIG_DIR replaces the whole ~/.claude base", func() {
			// Live macOS: CLAUDE_CONFIG_DIR=/tmp/x claude mcp list created
			// /tmp/x/backups — the base moved wholesale.
			got, err := hostpath.Roots("claude", env("/h", darwin, map[string]string{
				"CLAUDE_CONFIG_DIR": "/cfg",
			}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/cfg")
		})

		Convey("CODEX_HOME replaces the base and must be absolute", func() {
			// Live both OSes: CODEX_HOME=rel → "failed to resolve CODEX_HOME".
			got, err := hostpath.Roots("codex", env("/h", linux, map[string]string{"CODEX_HOME": "/cx"}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/cx")

			_, err = hostpath.Roots("codex", env("/h", linux, map[string]string{"CODEX_HOME": "rel"}))
			So(err, ShouldBeError, "codex: CODEX_HOME must be an absolute path, got \"rel\"")
		})

		Convey("GEMINI_CLI_HOME is a home, not the .gemini dir itself", func() {
			// Live both OSes: GEMINI_CLI_HOME=$T/g created $T/g/.gemini.
			got, err := hostpath.Roots("gemini", env("/h", darwin, map[string]string{
				"GEMINI_CLI_HOME": "/alt",
			}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/alt/.gemini")
		})

		Convey("XDG_CONFIG_HOME relocates opencode and kilo only", func() {
			for _, id := range []string{"opencode", "kilo"} {
				got, err := hostpath.Roots(id, env("/h", linux, map[string]string{
					"XDG_CONFIG_HOME": "/x",
				}))
				So(err, ShouldBeNil)
				So(got.ConfigRoot, ShouldEqual, "/x/"+id)
				So(got.XDG, ShouldBeTrue)
			}

			// Live macOS + Linux: none of these honour XDG_CONFIG_HOME.
			for _, id := range []string{"claude", "codex", "gemini", "cursor", "pi", "omp", "dsh"} {
				got, err := hostpath.Roots(id, env("/h", linux, map[string]string{
					"XDG_CONFIG_HOME": "/x",
				}))
				So(err, ShouldBeNil)
				So(got.XDG, ShouldBeFalse)
			}
		})

		Convey("an empty or relative XDG_CONFIG_HOME falls back to ~/.config", func() {
			// xdg-basedir uses `||`, so empty is falsy and relative is joined
			// verbatim. Neither is a usable absolute root, so both fall back.
			for _, value := range []string{"", "rel/xdg"} {
				got, err := hostpath.Roots("opencode", env("/h", linux, map[string]string{
					"XDG_CONFIG_HOME": value,
				}))
				So(err, ShouldBeNil)
				So(got.ConfigRoot, ShouldEqual, "/h/.config/opencode")
			}
		})

		Convey("KILO_CONFIG_DIR replaces the kilo config root", func() {
			// Live both OSes: kilo mcp list found a server in $T/kd/kilo.json
			// and in $T/xk/kilo/kilo.json. Neither tool models this today.
			got, err := hostpath.Roots("kilo", env("/h", darwin, map[string]string{
				"KILO_CONFIG_DIR": "/kd",
			}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/kd")
		})

		Convey("CURSOR_CONFIG_DIR moves cli-config.json but not the MCP doc", func() {
			// Live both OSes: cursor wrote $CURSOR_CONFIG_DIR/cli-config.json,
			// yet mcp.json was still only read from ~/.cursor.
			got, err := hostpath.Roots("cursor", env("/h", darwin, map[string]string{
				"CURSOR_CONFIG_DIR": "/cu",
			}))
			So(err, ShouldBeNil)
			So(got.SettingsRoot, ShouldEqual, "/cu")

			surfaces, err := hostpath.Surfaces("cursor", env("/h", darwin, map[string]string{
				"CURSOR_CONFIG_DIR": "/cu",
			}))
			So(err, ShouldBeNil)
			So(surfaces.MCPDoc, ShouldEqual, "/h/.cursor/mcp.json")
			So(surfaces.Settings, ShouldEqual, "/cu/cli-config.json")
		})

		Convey("PI_CODING_AGENT_DIR is pi's only override; PI_CONFIG_DIR is omp's", func() {
			got, err := hostpath.Roots("pi", env("/h", darwin, map[string]string{
				"PI_CODING_AGENT_DIR": "/pi/abs",
			}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/pi/abs")

			// An empty value is falsy for pi: it falls back.
			got, err = hostpath.Roots("pi", env("/h", darwin, map[string]string{
				"PI_CODING_AGENT_DIR": "",
			}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/h/.pi/agent")

			// PI_CONFIG_DIR is documented for omp only; pi ignores it.
			got, err = hostpath.Roots("pi", env("/h", darwin, map[string]string{
				"PI_CONFIG_DIR": "/alt",
			}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/h/.pi/agent")
		})

		Convey("DSH_HOME is a literal root, not a name under HOME", func() {
			// beadle pkg/agent/dsh.go:39-46 takes the value literally.
			got, err := hostpath.Roots("dsh", env("/h", darwin, map[string]string{
				"DSH_HOME": "/opt/dsh",
			}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/opt/dsh")

			// An empty value is ignored by DSH itself.
			got, err = hostpath.Roots("dsh", env("/h", darwin, map[string]string{"DSH_HOME": ""}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/h/.dsh")
		})
	})
}

func TestOpenCodeConfigDir(t *testing.T) {
	Convey("OPENCODE_CONFIG_DIR replaces the opencode root and outranks XDG", t, func() {
		// Live on macOS 2.0.18, isolated HOME, service started
		// (W1-A §fix F2): `OPENCODE_CONFIG_DIR=$T/cfg opencode debug
		// paths` reported `config  $T/cfg`, and it still won when
		// XDG_CONFIG_HOME pointed somewhere else.
		got, err := hostpath.Roots("opencode", env("/h", darwin, map[string]string{
			"OPENCODE_CONFIG_DIR": "/cfg",
		}))
		So(err, ShouldBeNil)
		So(got.ConfigRoot, ShouldEqual, "/cfg")
		// The root did not come from XDG, and the flag says so.
		So(got.XDG, ShouldBeFalse)

		Convey("Then it outranks XDG_CONFIG_HOME", func() {
			got, err := hostpath.Roots("opencode", env("/h", darwin, map[string]string{
				"OPENCODE_CONFIG_DIR": "/cfg", "XDG_CONFIG_HOME": "/x",
			}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/cfg")
		})

		Convey("Then an empty value falls back to the XDG root", func() {
			got, err := hostpath.Roots("opencode", env("/h", darwin, map[string]string{
				"OPENCODE_CONFIG_DIR": "", "XDG_CONFIG_HOME": "/x",
			}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/x/opencode")
			So(got.XDG, ShouldBeTrue)
		})

		Convey("Then a relative value is used verbatim, as the host reads it", func() {
			got, err := hostpath.Roots("opencode", env("/h", darwin, map[string]string{
				"OPENCODE_CONFIG_DIR": "rel/cfg",
			}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "rel/cfg")
		})

		Convey("Then no other host reads it", func() {
			for _, id := range []string{"claude", "codex", "gemini", "agy", "cursor", "kilo", "pi", "dsh", "omp"} {
				got, err := hostpath.Roots(id, env("/h", linux, map[string]string{
					"OPENCODE_CONFIG_DIR": "/cfg",
				}))
				So(err, ShouldBeNil)
				So(got.ConfigRoot, ShouldNotEqual, "/cfg")
			}
		})
	})
}

func TestOmpProfileChain(t *testing.T) {
	Convey("omp's profile chain matches `omp config path` on both platforms", t, func() {
		Convey("the default is <home>/.omp/agent", func() {
			got, err := hostpath.Roots("omp", none("/h", darwin))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/h/.omp/agent")
			So(got.StateRoot, ShouldEqual, "/h/.omp")
			So(got.Profile, ShouldEqual, "")
		})

		Convey("PI_CONFIG_DIR is a name below HOME, never a root of its own", func() {
			// Live macOS: PI_CONFIG_DIR=alt -> $HOME/alt/agent;
			// PI_CONFIG_DIR=/abs -> $HOME/abs/agent.
			for value, want := range map[string]string{
				"alt":  "/h/alt/agent",
				"/abs": "/h/abs/agent",
			} {
				got, err := hostpath.Roots("omp", env("/h", darwin, map[string]string{
					"PI_CONFIG_DIR": value,
				}))
				So(err, ShouldBeNil)
				So(got.ConfigRoot, ShouldEqual, want)
				So(got.StateRoot, ShouldEqual, want[:len(want)-len("/agent")])
			}
		})

		Convey("an empty PI_CONFIG_DIR is ignored", func() {
			// Live macOS: PI_CONFIG_DIR= -> $HOME/.omp/agent.
			got, err := hostpath.Roots("omp", env("/h", darwin, map[string]string{
				"PI_CONFIG_DIR": "",
			}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/h/.omp/agent")
		})

		Convey("a named profile relocates both the agent dir and the state root", func() {
			// Live macOS: OMP_PROFILE=work -> $HOME/.omp/profiles/work/agent.
			got, err := hostpath.Roots("omp", env("/h", darwin, map[string]string{
				"OMP_PROFILE": "work",
			}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/h/.omp/profiles/work/agent")
			So(got.StateRoot, ShouldEqual, "/h/.omp/profiles/work")
			So(got.Profile, ShouldEqual, "work")
		})

		Convey("a profile wins over PI_CODING_AGENT_DIR", func() {
			// A named profile never sees the default profile's agent dir.
			got, err := hostpath.Roots("omp", env("/h", darwin, map[string]string{
				"OMP_PROFILE":         "work",
				"PI_CODING_AGENT_DIR": "/elsewhere",
			}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/h/.omp/profiles/work/agent")
		})

		Convey("PI_CODING_AGENT_DIR moves the default-profile agent dir only", func() {
			// Live both OSes: PI_CODING_AGENT_DIR=relx -> <cwd>/relx,
			// an absolute value is used verbatim.
			got, err := hostpath.Roots("omp", env("/h", darwin, map[string]string{
				"PI_CODING_AGENT_DIR": "/pa",
			}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/pa")
			// The state root is not moved by the agent dir: the plugin
			// registry is a sibling of agent/, not a child.
			So(got.StateRoot, ShouldEqual, "/h/.omp")
		})

		Convey("OMP_PROFILE=default and an empty OMP_PROFILE both mean no profile", func() {
			// Live macOS: OMP_PROFILE=default -> $HOME/.omp/agent;
			// OMP_PROFILE= PI_PROFILE=work -> $HOME/.omp/agent, i.e. a
			// defined-but-empty OMP_PROFILE hides PI_PROFILE entirely.
			for _, vars := range []map[string]string{
				{"OMP_PROFILE": "default"},
				{"OMP_PROFILE": "", "PI_PROFILE": "work"},
			} {
				got, err := hostpath.Roots("omp", env("/h", darwin, vars))
				So(err, ShouldBeNil)
				So(got.ConfigRoot, ShouldEqual, "/h/.omp/agent")
				So(got.Profile, ShouldEqual, "")
			}
		})

		Convey("PI_PROFILE is the legacy fallback, consulted only while OMP_PROFILE is unset", func() {
			got, err := hostpath.Roots("omp", env("/h", darwin, map[string]string{
				"PI_PROFILE": "work",
			}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/h/.omp/profiles/work/agent")
		})
	})
}

func TestSurfacesPerHost(t *testing.T) {
	Convey("Surfaces pins the user-scope write target and read order", t, func() {
		Convey("claude", func() {
			s, err := hostpath.Surfaces("claude", none("/h", darwin))
			So(err, ShouldBeNil)
			So(s.Rules, ShouldEqual, "/h/.claude/CLAUDE.md")
			// The user-scope MCP document is a sibling of ~/.claude, not a
			// child — the trap for every naive resolver.
			So(s.MCPDoc, ShouldEqual, "/h/.claude.json")
			So(s.MCPPointer, ShouldEqual, "/mcpServers")
			So(s.MCPTable, ShouldEqual, "")
			So(s.Skills, ShouldEqual, "/h/.claude/skills")
			So(s.Agents, ShouldEqual, "/h/.claude/agents")
			So(s.Commands, ShouldEqual, "/h/.claude/commands")
			So(s.Hooks, ShouldEqual, "/h/.claude/settings.json")
			So(s.HooksEmbedded, ShouldBeTrue)
			So(s.Plugins, ShouldEqual, "/h/.claude/plugins")
		})

		Convey("codex", func() {
			s, err := hostpath.Surfaces("codex", none("/h", linux))
			So(err, ShouldBeNil)
			So(s.Rules, ShouldEqual, "/h/.codex/AGENTS.md")
			So(s.MCPDoc, ShouldEqual, "/h/.codex/config.toml")
			So(s.MCPTable, ShouldEqual, "mcp_servers")
			So(s.MCPPointer, ShouldEqual, "")
			// The host's own note calls ~/.codex/skills deprecated but it is
			// still read, so it stays in the list — second, below the shared
			// hub that the same note calls the live one.
			So(s.Skills, ShouldEqual, "/h/.agents/skills")
			So(s.SkillsReads, ShouldResemble, []string{"/h/.agents/skills", "/h/.codex/skills"})
			So(s.Agents, ShouldEqual, "/h/.codex/agents")
			So(s.Commands, ShouldEqual, "/h/.codex/prompts")
			So(s.Hooks, ShouldEqual, "/h/.codex/hooks.json")
			So(s.HooksEmbedded, ShouldBeFalse)
		})

		Convey("gemini", func() {
			s, err := hostpath.Surfaces("gemini", none("/h", darwin))
			So(err, ShouldBeNil)
			So(s.Rules, ShouldEqual, "/h/.gemini/GEMINI.md")
			So(s.MCPDoc, ShouldEqual, "/h/.gemini/settings.json")
			So(s.MCPPointer, ShouldEqual, "/mcpServers")
			// docs/cli/skills.md:43-53 — .agents/skills outranks
			// .gemini/skills inside the same tier.
			So(s.Skills, ShouldEqual, "/h/.gemini/skills")
			So(s.SkillsReads, ShouldResemble, []string{"/h/.agents/skills", "/h/.gemini/skills"})
			So(s.Agents, ShouldEqual, "/h/.gemini/agents")
			So(s.Commands, ShouldEqual, "/h/.gemini/commands")
			So(s.Hooks, ShouldEqual, "/h/.gemini/settings.json")
			So(s.HooksEmbedded, ShouldBeTrue)
			So(s.Plugins, ShouldEqual, "/h/.gemini/extensions")
		})

		Convey("agy reads the gemini tree but has no rules, skills or commands", func() {
			s, err := hostpath.Surfaces("agy", none("/h", darwin))
			So(err, ShouldBeNil)
			So(s.Rules, ShouldEqual, "")
			So(s.Skills, ShouldEqual, "")
			So(s.Commands, ShouldEqual, "")
			So(s.MCPDoc, ShouldEqual, "/h/.gemini/config/mcp_config.json")
			So(s.Agents, ShouldEqual, "/h/.gemini/config/agents")
			// The native install rung. Not host-verified: no agy binary on
			// macOS or Linux, so this mirrors beadle's reader.
			So(s.Plugins, ShouldEqual, "/h/.gemini/config/plugins")
		})
		Convey("cursor", func() {
			s, err := hostpath.Surfaces("cursor", none("/h", linux))
			So(err, ShouldBeNil)
			So(s.Rules, ShouldEqual, "")
			So(s.MCPDoc, ShouldEqual, "/h/.cursor/mcp.json")
			So(s.Skills, ShouldEqual, "/h/.cursor/skills")
			So(s.SkillsReads, ShouldResemble, []string{
				"/h/.cursor/skills", "/h/.claude/skills", "/h/.agents/skills",
			})
			So(s.Agents, ShouldEqual, "/h/.cursor/agents")
			So(s.Commands, ShouldEqual, "/h/.cursor/commands")
			So(s.Hooks, ShouldEqual, "/h/.cursor/hooks.json")
			So(s.Settings, ShouldEqual, "/h/.cursor/cli-config.json")
		})
	})
}

func TestSurfacesXDGHosts(t *testing.T) {
	Convey("Surfaces pins the XDG-family layouts", t, func() {
		Convey("opencode and kilo share the opencode dialect", func() {
			s, err := hostpath.Surfaces("opencode", none("/h", linux))
			So(err, ShouldBeNil)
			So(s.Rules, ShouldEqual, "/h/.config/opencode/AGENTS.md")
			So(s.MCPDocCandidates, ShouldResemble, []string{
				"/h/.config/opencode/opencode.jsonc",
				"/h/.config/opencode/opencode.json",
			})
			So(s.MCPPointer, ShouldEqual, "/mcp")
			So(s.Skills, ShouldEqual, "/h/.config/opencode/skills")
			So(s.SkillsReads, ShouldResemble, []string{
				"/h/.config/opencode/skills", "/h/.agents/skills", "/h/.claude/skills",
			})
			So(s.Agents, ShouldEqual, "/h/.config/opencode/agents")
			So(s.AgentsReads, ShouldResemble, []string{
				"/h/.config/opencode/agents", "/h/.config/opencode/agent",
			})
			So(s.Hooks, ShouldEqual, "")
			// OpenCode 2.0.18 declares plugins by path in the config's
			// `plugins` array and scans no plugin directory, so there is
			// nothing to model here — the field stays empty rather than
			// carrying a 1.x path 2.x would never read.
			So(s.PluginModules, ShouldBeEmpty)
			So(s.PluginModulesWrite, ShouldEqual, "")

			s, err = hostpath.Surfaces("kilo", none("/h", linux))
			So(err, ShouldBeNil)
			So(s.Rules, ShouldEqual, "/h/.config/kilo/AGENTS.md")
			So(s.MCPDocCandidates, ShouldResemble, []string{
				"/h/.config/kilo/kilo.jsonc", "/h/.config/kilo/kilo.json",
			})
			So(s.MCPPointer, ShouldEqual, "/mcp")
			// beadle reads four skill roots; the host-code pair wins.
			So(s.SkillsReads, ShouldResemble, []string{
				"/h/.config/kilo/skills", "/h/.config/kilo/skill",
				"/h/.kilo/skills", "/h/.kilo/skill",
			})
			So(s.CommandsReads, ShouldResemble, []string{
				"/h/.config/kilo/commands", "/h/.config/kilo/command",
			})
			// Probed live on kilo 7.8.1 under an isolated HOME: a module in
			// EITHER directory is executed. The singular one is the write
			// target — it is where verger's runtime shim goes.
			So(s.PluginModules, ShouldResemble, []string{
				"/h/.config/kilo/plugin", "/h/.config/kilo/plugins",
			})
			So(s.PluginModulesWrite, ShouldEqual, "/h/.config/kilo/plugin")
		})

		Convey("pi", func() {
			s, err := hostpath.Surfaces("pi", none("/h", darwin))
			So(err, ShouldBeNil)
			So(s.Rules, ShouldEqual, "/h/.pi/agent/AGENTS.md")
			So(s.MCPDoc, ShouldEqual, "/h/.pi/agent/mcp.json")
			So(s.Skills, ShouldEqual, "/h/.pi/agent/skills")
			So(s.Commands, ShouldEqual, "/h/.pi/agent/prompts")
			// pi has no subagents, no hooks document and no plugin registry.
			So(s.Agents, ShouldEqual, "")
			So(s.Hooks, ShouldEqual, "")
			So(s.Plugins, ShouldEqual, "/h/.pi/agent/extensions")
		})

		Convey("dsh", func() {
			s, err := hostpath.Surfaces("dsh", env("/h", darwin, map[string]string{
				"DSH_HOME": "/opt/dsh",
			}))
			So(err, ShouldBeNil)
			So(s.Rules, ShouldEqual, "/opt/dsh/AGENTS.md")
			So(s.MCPDoc, ShouldEqual, "/opt/dsh/cordis.patch.yml")
			So(s.Skills, ShouldEqual, "/opt/dsh/skills")
			So(s.Agents, ShouldEqual, "")
			So(s.Commands, ShouldEqual, "")
			So(s.Hooks, ShouldEqual, "")
			So(s.Plugins, ShouldEqual, "")
			So(s.SharedAgents, ShouldEqual, "/h/.agents")
		})

		Convey("omp", func() {
			s, err := hostpath.Surfaces("omp", env("/h", darwin, map[string]string{
				"OMP_PROFILE": "work",
			}))
			So(err, ShouldBeNil)
			So(s.Rules, ShouldEqual, "/h/.omp/profiles/work/agent/AGENTS.md")
			So(s.MCPDoc, ShouldEqual, "/h/.omp/profiles/work/agent/mcp.json")
			// The write target is the host's own dir: writing to the shared
			// hub would leak the skill to every other host that reads it.
			So(s.Skills, ShouldEqual, "/h/.omp/profiles/work/agent/skills")
			// The native dir outranks the shared hub by name, so it leads the
			// read list; the hub is still read, never written.
			So(s.SkillsReads, ShouldResemble, []string{
				"/h/.omp/profiles/work/agent/skills", "/h/.agents/skills",
			})
			So(s.Agents, ShouldEqual, "/h/.omp/profiles/work/agent/agents")
			So(s.Commands, ShouldEqual, "/h/.omp/profiles/work/agent/commands")
			// omp has no declarative hook document, only modules.
			So(s.Hooks, ShouldEqual, "")
			So(s.HookModules, ShouldEqual, "/h/.omp/profiles/work/agent/hooks")
			So(s.Plugins, ShouldEqual, "/h/.omp/profiles/work/plugins")
		})
	})
}

// The write target must always be a member of the read list, and the read
// list is in the host's precedence order — which is NOT always "write target
// first" (gemini reads the shared hub above its own). A consumer that writes
// Reads[0] would, for gemini, write the cross-agent hub. This test pins the
// invariant that makes that safe to document: Skills is in SkillsReads, and
// for every host except gemini it is the first entry.
func TestWriteTargetsAreReadListMembers(t *testing.T) {
	Convey("Given every host's surfaces", t, func() {
		Convey("Then the write target is always a member of the read list", func() {
			for _, id := range hostpath.All() {
				s, err := hostpath.Surfaces(id, none("/h", darwin))
				So(err, ShouldBeNil)

				if s.Skills == "" {
					So(s.SkillsReads, ShouldBeEmpty)
				} else {
					So(s.SkillsReads, ShouldContain, s.Skills)
				}

				if s.Agents == "" {
					So(s.AgentsReads, ShouldBeEmpty)
				} else {
					So(s.AgentsReads, ShouldContain, s.Agents)
				}

				if s.Commands == "" {
					So(s.CommandsReads, ShouldBeEmpty)
				} else {
					So(s.CommandsReads, ShouldContain, s.Commands)
				}

				if s.PluginModulesWrite == "" {
					So(s.PluginModules, ShouldBeEmpty)
				} else {
					So(s.PluginModules, ShouldContain, s.PluginModulesWrite)
				}
			}
		})

		Convey("Then only gemini's skills write target is not first", func() {
			for _, id := range hostpath.All() {
				if id == "gemini" {
					continue
				}

				s, err := hostpath.Surfaces(id, none("/h", darwin))
				So(err, ShouldBeNil)

				if s.Skills == "" {
					continue
				}

				So(s.SkillsReads[0], ShouldEqual, s.Skills)
			}

			// gemini: the host reads the shared hub above its own directory
			// in the same tier, so the write target is deliberately second.
			gemini, err := hostpath.Surfaces("gemini", none("/h", darwin))
			So(err, ShouldBeNil)
			So(gemini.SkillsReads[0], ShouldEqual, "/h/.agents/skills")
			So(gemini.Skills, ShouldEqual, "/h/.gemini/skills")
		})
	})
}

func TestProjectSurfaces(t *testing.T) {
	Convey("ProjectSurfaces are relative to the project root", t, func() {
		table := map[string]struct {
			rules, mcp, skills, agents, commands string
		}{
			// claude's project rules WRITE target is the project's own AGENTS.md
			// (claude 2.1.280 reads it natively; beadle pkg/agent/agents.go:111
			// writes that path). The other two names the host reads stay in
			// RulesReads — see TestClaudeProjectRulesWriteAGENTSAndReadAllThree.
			"claude":   {"AGENTS.md", ".mcp.json", ".claude/skills", ".claude/agents", ".claude/commands"},
			"codex":    {"AGENTS.md", ".codex/config.toml", ".codex/skills", ".codex/agents", ".codex/prompts"},
			"gemini":   {"GEMINI.md", ".gemini/settings.json", ".gemini/skills", ".gemini/agents", ".gemini/commands"},
			"agy":      {"", ".agents/mcp_config.json", "", "", ""},
			"cursor":   {"", ".cursor/mcp.json", ".cursor/skills", ".cursor/agents", ""},
			"opencode": {"AGENTS.md", "opencode.json", ".opencode/skills", ".opencode/agents", ".opencode/commands"},
			"kilo":     {"AGENTS.md", ".kilo/kilo.json", ".kilo/skills", ".kilo/agents", ".kilo/commands"},
			"pi":       {"AGENTS.md", ".pi/mcp.json", ".pi/skills", "", ".pi/prompts"},
			"dsh":      {"AGENTS.md", "", ".dsh/skills", "", ""},
			"omp":      {".omp/AGENTS.md", ".omp/mcp.json", ".omp/skills", ".omp/agents", ".omp/commands"},
		}

		for id, want := range table {
			got := hostpath.ProjectSurfaces(id, "/repo")
			So(got.Rules, ShouldEqual, join("/repo", want.rules))
			So(got.MCPDoc, ShouldEqual, join("/repo", want.mcp))
			So(got.Skills, ShouldEqual, join("/repo", want.skills))
			So(got.Agents, ShouldEqual, join("/repo", want.agents))
			So(got.Commands, ShouldEqual, join("/repo", want.commands))
		}
	})
}

func TestManagedPolicyPerOS(t *testing.T) {
	Convey("managed policy documents are explicit per GOOS", t, func() {
		Convey("claude", func() {
			mac, err := hostpath.ManagedPolicy("claude", darwin)
			So(err, ShouldBeNil)
			So(mac, ShouldResemble, []string{
				"/Library/Application Support/ClaudeCode/managed-settings.json",
				"/Library/Application Support/ClaudeCode/managed-settings.d",
				"/Library/Application Support/ClaudeCode/managed-mcp.json",
			})

			lin, err := hostpath.ManagedPolicy("claude", linux)
			So(err, ShouldBeNil)
			So(lin, ShouldResemble, []string{
				"/etc/claude-code/managed-settings.json",
				"/etc/claude-code/managed-settings.d",
				"/etc/claude-code/managed-mcp.json",
			})
		})

		Convey("gemini", func() {
			mac, err := hostpath.ManagedPolicy("gemini", darwin)
			So(err, ShouldBeNil)
			So(mac, ShouldResemble, []string{
				"/Library/Application Support/GeminiCli/system-defaults.json",
				"/Library/Application Support/GeminiCli/settings.json",
				"/Library/Application Support/GeminiCli/policies",
			})

			lin, err := hostpath.ManagedPolicy("gemini", linux)
			So(err, ShouldBeNil)
			So(lin, ShouldResemble, []string{
				"/etc/gemini-cli/system-defaults.json",
				"/etc/gemini-cli/settings.json",
				"/etc/gemini-cli/policies",
			})
		})

		Convey("codex is /etc on both platforms", func() {
			for _, goos := range []string{darwin, linux} {
				got, err := hostpath.ManagedPolicy("codex", goos)
				So(err, ShouldBeNil)
				So(got, ShouldResemble, []string{
					"/etc/codex/requirements.toml",
					"/etc/codex/config.toml",
					"/etc/codex/managed_config.toml",
				})
			}
		})

		Convey("cursor's Linux policy is a user file, not an /etc path", func() {
			// Live Linux: /etc/cursor does not exist. The documented policy
			// file is ~/.cursor/policy.json, and macOS has MDM only.
			got, err := hostpath.ManagedPolicy("cursor", linux)
			So(err, ShouldBeNil)
			So(got, ShouldHaveLength, 0)
		})

		Convey("a host with no managed document returns an empty list", func() {
			for _, id := range []string{"agy", "cursor", "opencode", "kilo", "pi", "dsh", "omp"} {
				got, err := hostpath.ManagedPolicy(id, linux)
				So(err, ShouldBeNil)
				So(got, ShouldBeEmpty)
			}
		})

		Convey("an unknown GOOS is an error, never a silent fallback", func() {
			_, err := hostpath.ManagedPolicy("claude", "plan9")
			So(err, ShouldBeError, "claude: unsupported GOOS \"plan9\"")
		})
	})
}

func TestIDsAndErrors(t *testing.T) {
	Convey("ids are the canonical short verger ids (U4)", t, func() {
		So(hostpath.All(), ShouldResemble, []string{
			"claude", "codex", "gemini", "agy", "cursor", "opencode", "kilo", "pi", "dsh", "omp",
		})
		So(hostpath.Known("claude"), ShouldBeTrue)
		So(hostpath.Known("claude-code"), ShouldBeFalse)
		So(hostpath.Known("nope"), ShouldBeFalse)
	})

	Convey("an unknown id is an error on every entry point", t, func() {
		_, err := hostpath.Roots("gemini-cli", none("/h", darwin))
		So(err, ShouldBeError, `unknown host id "gemini-cli"; known ids: agy, claude, codex, cursor, dsh, gemini, kilo, omp, opencode, pi`)

		_, err = hostpath.Surfaces("antigravity-cli", none("/h", darwin))
		So(err, ShouldBeError, `unknown host id "antigravity-cli"; known ids: agy, claude, codex, cursor, dsh, gemini, kilo, omp, opencode, pi`)

		_, err = hostpath.Roots("", none("/h", darwin))
		So(err, ShouldBeError, `unknown host id ""; known ids: agy, claude, codex, cursor, dsh, gemini, kilo, omp, opencode, pi`)
	})

	Convey("an empty home is an error, not a relative path", t, func() {
		_, err := hostpath.Roots("claude", hostpath.Env{Home: "", GOOS: darwin})
		So(err, ShouldBeError, "claude: Env.Home is required")
	})

	Convey("a nil Lookup behaves as an empty environment", t, func() {
		got, err := hostpath.Roots("omp", hostpath.Env{Home: "/h", GOOS: darwin})
		So(err, ShouldBeNil)
		So(got.ConfigRoot, ShouldEqual, "/h/.omp/agent")
	})

	Convey("resolution is pure: the same Env always yields the same result", t, func() {
		first, err := hostpath.Roots("omp", env("/h", linux, map[string]string{
			"OMP_PROFILE": "work", "PI_CONFIG_DIR": "cfg",
		}))
		So(err, ShouldBeNil)

		second, err := hostpath.Roots("omp", env("/h", linux, map[string]string{
			"OMP_PROFILE": "work", "PI_CONFIG_DIR": "cfg",
		}))
		So(err, ShouldBeNil)
		So(first, ShouldResemble, second)
		So(first.Profile, ShouldEqual, "work")
		So(first.StateRoot, ShouldEqual, "/h/cfg/profiles/work")
	})

	Convey("every host resolves on both platforms without error", t, func() {
		for _, goos := range []string{darwin, linux} {
			for _, id := range hostpath.All() {
				roots, err := hostpath.Roots(id, env("/h", goos, map[string]string{
					"XDG_CONFIG_HOME": "/x", "CLAUDE_CONFIG_DIR": "/c",
					"CODEX_HOME": "/cx", "GEMINI_CLI_HOME": "/g",
					"CURSOR_CONFIG_DIR": "/cu", "KILO_CONFIG_DIR": "/k",
					"PI_CODING_AGENT_DIR": "/p", "DSH_HOME": "/d",
					"PI_CONFIG_DIR": "o", "OMP_PROFILE": "work",
				}))
				So(err, ShouldBeNil)
				So(roots.ConfigRoot, ShouldNotBeEmpty)

				surfaces, err := hostpath.Surfaces(id, env("/h", goos, map[string]string{
					"OMP_PROFILE": "work",
				}))
				So(err, ShouldBeNil)
				So(surfaces, ShouldNotBeNil)

				So(hostpath.ProjectSurfaces(id, "/repo"), ShouldNotBeNil)
			}
		}
	})
}

// join is filepath.Join with an empty segment meaning "no path".
func join(root, rel string) string {
	if rel == "" {
		return ""
	}

	return filepath.Join(root, filepath.FromSlash(rel))
}

// TestEnvValuesAreTakenLiterally pins the rule behind W1-A-verify-2 F1: the
// resolver must repeat each host's own reading of a value, and no host here
// trims. The omp profile is the one measured exception and is pinned too.
func TestEnvValuesAreTakenLiterally(t *testing.T) {
	Convey("Given a padded variable value", t, func() {
		Convey("Then each host takes it verbatim, spaces included", func() {
			table := []struct {
				id   string
				env  map[string]string
				want string
			}{
				{"claude", map[string]string{"CLAUDE_CONFIG_DIR": "  /cfg  "}, "  /cfg  "},
				{"gemini", map[string]string{"GEMINI_CLI_HOME": "  /g  "}, "  /g  /.gemini"},
				{"kilo", map[string]string{"KILO_CONFIG_DIR": "  /kd  "}, "  /kd  "},
				{"opencode", map[string]string{"OPENCODE_CONFIG_DIR": "  /oc  "}, "  /oc  "},
				{"pi", map[string]string{"PI_CODING_AGENT_DIR": "  /pi  "}, "  /pi  "},
				{"dsh", map[string]string{"DSH_HOME": "  /dsh  "}, "  /dsh  "},
				{"omp", map[string]string{"PI_CONFIG_DIR": "  o  "}, "/h/  o  /agent"},
			}

			for _, row := range table {
				got, err := hostpath.Roots(row.id, env("/h", darwin, row.env))
				So(err, ShouldBeNil)
				So(got.ConfigRoot, ShouldEqual, row.want)
			}
		})

		Convey("Then a padded XDG_CONFIG_HOME is treated as relative, and the spec says relative is ignored", func() {
			// Not trimming makes a padded value non-absolute, so the XDG rule
			// ("empty/relative is ignored", W2-HOSTPATH) applies. The host
			// itself would use the padded value verbatim; that divergence is
			// inherited from the spec's rule, not chosen here.
			for _, id := range []string{"opencode", "kilo"} {
				got, err := hostpath.Roots(id, env("/h", darwin, map[string]string{
					"XDG_CONFIG_HOME": "  /x  ",
				}))
				So(err, ShouldBeNil)
				So(got.ConfigRoot, ShouldEqual, "/h/.config/"+id)
			}
		})

		Convey("Then cursor's padded value moves the settings document, not the root", func() {
			// CURSOR_CONFIG_DIR relocates cli-config.json only (live on both
			// platforms), so the config root must stay home-relative.
			got, err := hostpath.Roots("cursor", env("/h", darwin, map[string]string{
				"CURSOR_CONFIG_DIR": "  /cu  ",
			}))
			So(err, ShouldBeNil)
			So(got.SettingsRoot, ShouldEqual, "  /cu  ")
			So(got.ConfigRoot, ShouldEqual, "/h/.cursor")
		})

		Convey("Then codex refuses a padded value, because it cannot tell it from a relative one", func() {
			// The host takes CODEX_HOME verbatim, but a value this package
			// does not trim is indistinguishable from a relative path, and it
			// has no working directory to resolve one with. Refusing is the
			// same deliberate deviation a relative value gets.
			_, err := hostpath.Roots("codex", env("/h", darwin, map[string]string{
				"CODEX_HOME": "  /cx  ",
			}))
			So(err, ShouldBeError, `codex: CODEX_HOME must be an absolute path, got "  /cx  "`)
		})

		Convey("Then an omp profile is the exception: omp trims it", func() {
			// omp 18.4.2, live: OMP_PROFILE="  work  " answers
			// $HOME/.omp/profiles/work/agent.
			for _, vars := range []map[string]string{
				{"OMP_PROFILE": "  work  "},
				{"PI_PROFILE": "  work  "},
			} {
				got, err := hostpath.Roots("omp", env("/h", darwin, vars))
				So(err, ShouldBeNil)
				So(got.Profile, ShouldEqual, "work")
				So(got.ConfigRoot, ShouldEqual, "/h/.omp/profiles/work/agent")
			}
		})

		Convey("Then an empty value still falls back, because empty is unset", func() {
			// xdg-basedir uses `||`, so for XDG_CONFIG_HOME empty is
			// indistinguishable from unset.
			got, err := hostpath.Roots("opencode", env("/h", darwin, map[string]string{
				"OPENCODE_CONFIG_DIR": "", "XDG_CONFIG_HOME": "",
			}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "/h/.config/opencode")
		})

		Convey("Then a whitespace-only value is NOT empty, and is used verbatim", func() {
			// The host is handed the same string verger is, so whatever it
			// does with it, both agree.
			got, err := hostpath.Roots("kilo", env("/h", darwin, map[string]string{
				"KILO_CONFIG_DIR": "   ",
			}))
			So(err, ShouldBeNil)
			So(got.ConfigRoot, ShouldEqual, "   ")
		})
	})
}

// TestKiloConfigReadsAreAdditive pins the union kilo was measured to read,
// which a first-non-empty-wins chain cannot express (W1-A-verify-2 F2).
func TestKiloConfigReadsAreAdditive(t *testing.T) {
	Convey("Given kilo's configuration roots", t, func() {
		Convey("Then with nothing set only the home default is read", func() {
			s, err := hostpath.Surfaces("kilo", none("/h", darwin))
			So(err, ShouldBeNil)
			So(s.ConfigReads, ShouldResemble, []string{"/h/.config/kilo"})
		})

		Convey("Then KILO_CONFIG_DIR is written and the XDG root still read", func() {
			// Live 7.8.1, XDG unset: reads KILO_CONFIG_DIR AND ~/.config/kilo.
			s, err := hostpath.Surfaces("kilo", env("/h", darwin, map[string]string{
				"KILO_CONFIG_DIR": "/kd",
			}))
			So(err, ShouldBeNil)
			So(s.MCPDocCandidates[0], ShouldEqual, "/kd/kilo.jsonc")
			So(s.ConfigReads, ShouldResemble, []string{"/kd", "/h/.config/kilo"})
		})

		Convey("Then with both set the XDG root replaces the home default", func() {
			// Live 7.8.1: reads KILO_CONFIG_DIR and $XDG/kilo, not ~/.config/kilo.
			s, err := hostpath.Surfaces("kilo", env("/h", darwin, map[string]string{
				"KILO_CONFIG_DIR": "/kd", "XDG_CONFIG_HOME": "/x",
			}))
			So(err, ShouldBeNil)
			So(s.MCPDocCandidates[0], ShouldEqual, "/kd/kilo.jsonc")
			So(s.ConfigReads, ShouldResemble, []string{"/kd", "/x/kilo"})
		})

		Convey("Then with XDG alone the home default drops out", func() {
			// Live 7.8.1: reads $XDG/kilo alone — no ~/.config/kilo server.
			s, err := hostpath.Surfaces("kilo", env("/h", darwin, map[string]string{
				"XDG_CONFIG_HOME": "/x",
			}))
			So(err, ShouldBeNil)
			So(s.ConfigReads, ShouldResemble, []string{"/x/kilo"})
		})
	})
}

// TestClaudeStateDocFollowsTheOverride pins the rule W3-PREP GAP-13 flagged:
// the state document sits BESIDE the config root by default and INSIDE it
// once CLAUDE_CONFIG_DIR moves the root. Probed live on 2.1.283.
func TestClaudeStateDocFollowsTheOverride(t *testing.T) {
	Convey("Given the claude state document", t, func() {
		Convey("Then with no override it is a sibling of ~/.claude", func() {
			// Live: `claude mcp add` in a fresh HOME wrote $HOME/.claude.json.
			s, err := hostpath.Surfaces("claude", none("/h", darwin))
			So(err, ShouldBeNil)
			So(s.MCPDoc, ShouldEqual, "/h/.claude.json")
		})

		Convey("Then with an override it moves INSIDE the new root", func() {
			// Live: CLAUDE_CONFIG_DIR=O, `claude mcp add` wrote O/.claude.json
			// and $HOME/.claude.json was never created.
			roots, err := hostpath.Roots("claude", env("/h", darwin, map[string]string{
				"CLAUDE_CONFIG_DIR": "/cfg",
			}))
			So(err, ShouldBeNil)
			So(roots.ConfigRoot, ShouldEqual, "/cfg")

			s, err := hostpath.Surfaces("claude", env("/h", darwin, map[string]string{
				"CLAUDE_CONFIG_DIR": "/cfg",
			}))
			So(err, ShouldBeNil)
			So(s.MCPDoc, ShouldEqual, "/cfg/.claude.json")
		})

		Convey("Then an empty override leaves it at the home sibling", func() {
			s, err := hostpath.Surfaces("claude", env("/h", darwin, map[string]string{
				"CLAUDE_CONFIG_DIR": "",
			}))
			So(err, ShouldBeNil)
			So(s.MCPDoc, ShouldEqual, "/h/.claude.json")
		})
	})
}

// TestBeadleFacingSurfaces pins the surfaces W3-PREP GAP-14 listed as missing,
// each at the path beadle's own resolver uses.
func TestBeadleFacingSurfaces(t *testing.T) {
	Convey("Given the surfaces beadle still resolves itself", t, func() {
		Convey("Then claude carries projects memory, an inbox and its ignore root", func() {
			s, err := hostpath.Surfaces("claude", none("/h", darwin))
			So(err, ShouldBeNil)
			So(s.ProjectsDir, ShouldEqual, "/h/.claude/projects")
			So(s.Inbox, ShouldEqual, "/h/.claude/inbox.md")
			So(s.IgnoreRoots, ShouldResemble, []string{"/h/.claude/plugins"})
		})

		Convey("Then each host with an inbox has it", func() {
			for id, want := range map[string]string{
				"gemini": "/h/.gemini/inbox.md",
				"cursor": "/h/.cursor/inbox.md",
				"codex":  "/h/.codex/inbox.md",
				"pi":     "/h/.pi/agent/inbox.md",
				"kilo":   "/h/.config/kilo/inbox.md",
				"omp":    "/h/.omp/agent/inbox.md",
			} {
				s, err := hostpath.Surfaces(id, none("/h", darwin))
				So(err, ShouldBeNil)
				So(s.Inbox, ShouldEqual, want)
			}
		})

		Convey("Then kilo's agents read list carries the legacy {modes,mode} pair", func() {
			s, err := hostpath.Surfaces("kilo", none("/h", darwin))
			So(err, ShouldBeNil)
			So(s.AgentsReads, ShouldResemble, []string{
				"/h/.config/kilo/agents", "/h/.config/kilo/agent",
				"/h/.config/kilo/modes", "/h/.config/kilo/mode",
			})
		})

		Convey("Then agy carries its own state dir", func() {
			s, err := hostpath.Surfaces("agy", none("/h", darwin))
			So(err, ShouldBeNil)
			So(s.StateDir, ShouldEqual, "/h/.gemini/antigravity-cli")
		})

		Convey("Then pi carries the adapter probe paths", func() {
			s, err := hostpath.Surfaces("pi", none("/h", darwin))
			So(err, ShouldBeNil)
			So(s.AdapterProbes, ShouldResemble, []string{
				"/h/.pi/agent/settings.json",
				"/h/.pi/agent/packages/extensions",
				"/h/.pi/agent/node_modules/pi-mcp-adapter",
				"/h/.pi/agent/extensions/pi-mcp-adapter",
			})
		})

		Convey("Then the shared hub is a write target only where it is one", func() {
			// beadle registers the hub as its own ModeSync pseudo-agent
			// (agents.go:585-599); codex is the host whose skills write target
			// is that hub.
			codex, err := hostpath.Surfaces("codex", none("/h", darwin))
			So(err, ShouldBeNil)
			So(codex.SharedSkillsWrite, ShouldEqual, "/h/.agents/skills")

			for _, id := range []string{"gemini", "cursor", "kilo", "omp", "opencode", "pi"} {
				s, err := hostpath.Surfaces(id, none("/h", darwin))
				So(err, ShouldBeNil)
				So(s.SharedSkillsWrite, ShouldEqual, "")
			}
		})
	})
}
