package hostpath_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/hostpath"
)

func TestClaudeProjectRulesWriteAGENTSAndReadAllThree(t *testing.T) {
	Convey("Given a claude project", t, func() {
		s := hostpath.ProjectSurfaces(hostpath.Claude, "/work/repo")

		Convey("Then the write target is the project's own AGENTS.md", func() {
			// beadle pkg/agent/agents.go:111 writes this path, and a resolver
			// answering .claude/CLAUDE.md would put the digest block where
			// beadle never reads it.
			So(s.Rules, ShouldEqual, filepath.Join("/work/repo", "AGENTS.md"))
		})

		Convey("Then the read list is the write target first, then the two names beadle only reads", func() {
			So(s.RulesReads, ShouldResemble, []string{
				filepath.Join("/work/repo", "AGENTS.md"),
				filepath.Join("/work/repo", ".claude", "CLAUDE.md"),
				filepath.Join("/work/repo", "CLAUDE.md"),
			})
		})
	})
}

func TestEveryHostThatReadsTheHubSkipsTheClaudePluginTree(t *testing.T) {
	Convey("Given the eight hosts that carry the literal in beadle", t, func() {
		home := "/h"
		want := []string{filepath.Join(home, ".claude", "plugins")}

		Convey("Then every one of them reports the same ignore root", func() {
			for _, id := range []string{
				hostpath.OpenCode, hostpath.Cursor, hostpath.Codex,
				hostpath.Pi, hostpath.Kilo, hostpath.Omp, hostpath.DSH,
			} {
				s, err := hostpath.Surfaces(id, env(home, "linux", nil))
				So(err, ShouldBeNil)
				So(s.IgnoreRoots, ShouldResemble, want)
			}
		})

		Convey("Then the hub itself skips it too", func() {
			So(hostpath.Shared(env(home, "linux", nil)).IgnoreRoots, ShouldResemble, want)
		})

		Convey("Then the ignore root is home-relative even when a config dir is moved", func() {
			// The plugin tree does not travel with CLAUDE_CONFIG_DIR, so a
			// moved config dir must not move the walk-skip with it.
			moved := env(home, "linux", nil)
			moved.Lookup = func(name string) (string, bool) {
				if name == hostpath.ClaudeConfigDir {
					return "/elsewhere", true
				}

				return "", false
			}

			s, err := hostpath.Surfaces(hostpath.Cursor, moved)
			So(err, ShouldBeNil)
			So(s.IgnoreRoots, ShouldResemble, []string{filepath.Join(home, ".claude", "plugins")})
		})
	})
}

func TestClaudeAndGeminiKeepTheirOwnIgnoreRoot(t *testing.T) {
	Convey("Given the two hosts that already declared an ignore root", t, func() {
		claude, err := hostpath.Surfaces(hostpath.Claude, env("/h", "linux", nil))
		So(err, ShouldBeNil)
		So(claude.IgnoreRoots, ShouldResemble, []string{filepath.Join("/h", ".claude", "plugins")})

		gemini, err := hostpath.Surfaces(hostpath.Gemini, env("/h", "linux", nil))
		So(err, ShouldBeNil)
		So(gemini.IgnoreRoots, ShouldResemble, []string{filepath.Join("/h", ".claude", "plugins")})
	})
}

func TestProfilesDirIsNamedButNotListed(t *testing.T) {
	Convey("Given the two hosts with profiles", t, func() {
		omp, err := hostpath.Surfaces(hostpath.Omp, env("/h", "linux", nil))
		So(err, ShouldBeNil)
		So(omp.ProfilesDir, ShouldNotBeEmpty)

		dsh, err := hostpath.Surfaces(hostpath.DSH, env("/h", "linux", nil))
		So(err, ShouldBeNil)
		So(dsh.ProfilesDir, ShouldNotBeEmpty)

		Convey("Then a host without profiles has none", func() {
			claude, err := hostpath.Surfaces(hostpath.Claude, env("/h", "linux", nil))
			So(err, ShouldBeNil)
			So(claude.ProfilesDir, ShouldBeEmpty)
		})
	})
}

func TestListProfilesReturnsSortedDirectories(t *testing.T) {
	Convey("Given a profiles directory with entries of both kinds", t, func() {
		dir := t.TempDir()

		for _, name := range []string{"zeta", "alpha", "mid"} {
			So(os.Mkdir(filepath.Join(dir, name), 0o750), ShouldBeNil)
		}

		So(os.WriteFile(filepath.Join(dir, "a-file"), []byte("x"), 0o600), ShouldBeNil)

		Convey("Then the listing is the sorted directory names, files excluded", func() {
			names, err := hostpath.ListProfiles(dir)
			So(err, ShouldBeNil)
			So(names, ShouldResemble, []string{"alpha", "mid", "zeta"})
		})
	})

	Convey("Given a directory that does not exist", t, func() {
		Convey("Then the error names the directory instead of reporting no profiles", func() {
			_, err := hostpath.ListProfiles(filepath.Join(t.TempDir(), "absent"))
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "absent")
		})
	})
}

func TestMarkersAreIdentityFactsPerHost(t *testing.T) {
	Convey("Given the hosts with a verified binary name", t, func() {
		for id, want := range map[string]string{
			hostpath.Codex:    "codex",
			hostpath.Cursor:   "cursor-agent",
			hostpath.OpenCode: "opencode",
			hostpath.Kilo:     "kilo",
			hostpath.Pi:       "pi",
			hostpath.DSH:      "dsh",
			hostpath.Omp:      "omp",
		} {
			s, err := hostpath.Surfaces(id, env("/h", "linux", nil))
			So(err, ShouldBeNil)
			So(s.Markers.Binaries, ShouldResemble, []string{want})
		}
	})

	Convey("Given omp, which also writes a config document on first run", t, func() {
		s, err := hostpath.Surfaces(hostpath.Omp, env("/h", "linux", nil))
		So(err, ShouldBeNil)
		So(s.Markers.ConfigFile, ShouldContainSubstring, filepath.Join("agent", "config.yml"))
	})

	Convey("Given a host with no verified marker", t, func() {
		s, err := hostpath.Surfaces(hostpath.Agy, env("/h", "linux", nil))
		So(err, ShouldBeNil)
		So(s.Markers.ConfigFile, ShouldBeEmpty)
		So(s.Markers.Binaries, ShouldBeEmpty)
	})
}

func TestProjectChainRootWalksUpToTheGitEntry(t *testing.T) {
	Convey("Given a working directory inside a git project", t, func() {
		// ProjectChainRoot probes <dir>/.git, so the map is keyed by that.
		git := map[string]bool{"/w/.git": true}

		Convey("Then the root is the nearest ancestor holding .git", func() {
			root, ok := hostpath.ProjectChainRoot("/w/a/b", func(p string) bool { return git[p] })
			So(ok, ShouldBeTrue)
			So(root, ShouldEqual, "/w")
		})

		Convey("Then a nearer .git wins over a higher one", func() {
			git["/w/a/.git"] = true

			root, ok := hostpath.ProjectChainRoot("/w/a/b", func(p string) bool { return git[p] })
			So(ok, ShouldBeTrue)
			So(root, ShouldEqual, "/w/a")
		})

		Convey("Then no .git anywhere means no chain", func() {
			_, ok := hostpath.ProjectChainRoot("/w/a/b", func(string) bool { return false })
			So(ok, ShouldBeFalse)
		})
	})
}

func TestDSHProjectChainIsRootFirstAndCoversEveryDirectory(t *testing.T) {
	Convey("Given a root and a working directory three levels down", t, func() {
		chain := hostpath.DSHProjectChain("/w", "/w/a/b/c")

		Convey("Then it is every AGENTS.md and CLAUDE.md, root first", func() {
			So(chain, ShouldResemble, []string{
				"/w/AGENTS.md", "/w/CLAUDE.md",
				"/w/a/AGENTS.md", "/w/a/CLAUDE.md",
				"/w/a/b/AGENTS.md", "/w/a/b/CLAUDE.md",
				"/w/a/b/c/AGENTS.md", "/w/a/b/c/CLAUDE.md",
			})
		})

		Convey("Then cwd itself is the project root", func() {
			So(hostpath.DSHProjectChain("/w", "/w"), ShouldResemble, []string{
				"/w/AGENTS.md", "/w/CLAUDE.md",
			})
		})

		Convey("Then a cwd outside the root falls back to the root alone", func() {
			// beadle pkg/agent/dsh_chain.go:139-142: a cwd that cannot be
			// related to the root yields the root and nothing else, rather
			// than a guessed chain.
			So(hostpath.DSHProjectChain("/w", "a"), ShouldResemble, []string{
				"/w/AGENTS.md", "/w/CLAUDE.md",
			})
		})

		Convey("Then no root means no chain", func() {
			So(hostpath.DSHProjectChain("", "/w/a"), ShouldBeEmpty)
		})
	})
}

func TestPiProjectProbeIsADifferentSetFromTheUserScope(t *testing.T) {
	Convey("Given a pi project", t, func() {
		project := hostpath.ProjectSurfaces(hostpath.Pi, "/w/repo")

		Convey("Then the project probe names the project .pi files", func() {
			So(project.AdapterProbes, ShouldResemble, []string{
				filepath.Join("/w/repo", ".pi", "settings.json"),
				filepath.Join("/w/repo", ".pi", "npm", "node_modules", "pi-mcp-adapter"),
				filepath.Join("/w/repo", ".pi", "extensions", "pi-mcp-adapter"),
			})
		})

		Convey("Then it is not the user-scope set", func() {
			user, err := hostpath.Surfaces(hostpath.Pi, env("/h", "linux", nil))
			So(err, ShouldBeNil)
			So(slices.Contains(user.AdapterProbes, project.AdapterProbes[0]), ShouldBeFalse)
		})
	})
}

func TestSharedHubIsNotAHostId(t *testing.T) {
	Convey("Given the shared hub", t, func() {
		Convey("Then it is a function result, not an eleventh host", func() {
			So(hostpath.Known("shared"), ShouldBeFalse)
			So(hostpath.All(), ShouldNotContain, "shared")
			So(slices.Contains(hostpath.All(), "shared"), ShouldBeFalse)
		})

		Convey("Then it exposes the hub's skills as both read and write", func() {
			s := hostpath.Shared(env("/h", "linux", nil))
			So(s.ID, ShouldEqual, "shared")
			So(s.Skills, ShouldEqual, filepath.Join("/h", ".agents", "skills"))
			So(s.SkillsReads, ShouldResemble, []string{filepath.Join("/h", ".agents", "skills")})
			So(s.SharedAgents, ShouldEqual, filepath.Join("/h", ".agents"))
		})

		Convey("Then it invents no host surface it does not have", func() {
			s := hostpath.Shared(env("/h", "linux", nil))
			So(s.Rules, ShouldBeEmpty)
			So(s.MCPDoc, ShouldBeEmpty)
			So(s.Hooks, ShouldBeEmpty)
			So(s.Agents, ShouldBeEmpty)
			So(s.Commands, ShouldBeEmpty)
		})
	})
}

func TestRootsStaysPure(t *testing.T) {
	Convey("Given the same Env twice", t, func() {
		Convey("Then Roots and Surfaces return identical values and never touch the disk", func() {
			first, err := hostpath.Surfaces(hostpath.Omp, env("/h", "linux", nil))
			So(err, ShouldBeNil)

			second, err := hostpath.Surfaces(hostpath.Omp, env("/h", "linux", nil))
			So(err, ShouldBeNil)

			So(first.ProfilesDir, ShouldEqual, second.ProfilesDir)
			So(first.Markers, ShouldResemble, second.Markers)
			So(first.IgnoreRoots, ShouldResemble, second.IgnoreRoots)
		})
	})
}

// §fix5: beadle W3-A-impl-1 §1.6 flagged two defects in the §fix4 work.
func TestOmpMarkerConfigFileIsNotDoubled(t *testing.T) {
	Convey("Given omp with no profile set", t, func() {
		s, err := hostpath.Surfaces(hostpath.Omp, env("/h", "linux", nil))
		So(err, ShouldBeNil)

		Convey("Then the config document sits directly in the agent dir, once", func() {
			// Roots(omp).ConfigRoot is already <stateRoot>/agent; joining the
			// agent dir again produced agent/agent/config.yml.
			So(s.Markers.ConfigFile, ShouldEqual, filepath.Join("/h/.omp/agent", "config.yml"))
			So(strings.Count(s.Markers.ConfigFile, filepath.Join("agent", "agent")), ShouldEqual, 0)
		})

		Convey("Then it matches the agent dir the rest of the surface uses", func() {
			roots, err := hostpath.Roots(hostpath.Omp, env("/h", "linux", nil))
			So(err, ShouldBeNil)
			So(filepath.Dir(s.Markers.ConfigFile), ShouldEqual, roots.ConfigRoot)
		})
	})

	Convey("Given omp with a profile set", t, func() {
		profiled := env("/h", "linux", nil)
		profiled.Lookup = func(name string) (string, bool) {
			if name == hostpath.OMPProfile {
				return "work", true
			}

			return "", false
		}

		s, err := hostpath.Surfaces(hostpath.Omp, profiled)
		So(err, ShouldBeNil)

		Convey("Then the marker follows the profile, with no doubled segment", func() {
			So(s.Markers.ConfigFile, ShouldEqual, filepath.Join("/h/.omp/profiles/work/agent", "config.yml"))
		})
	})
}

func TestOpenCodeHasAnInbox(t *testing.T) {
	Convey("Given opencode", t, func() {
		s, err := hostpath.Surfaces(hostpath.OpenCode, env("/h", "linux", nil))
		So(err, ShouldBeNil)

		Convey("Then it declares one, under the config root it writes", func() {
			// beadle pkg/agent/inbox_path.go:36-43 carried a local line for
			// this because the resolver did not answer it.
			So(s.Inbox, ShouldNotBeEmpty)

			roots, rerr := hostpath.Roots(hostpath.OpenCode, env("/h", "linux", nil))
			So(rerr, ShouldBeNil)
			So(s.Inbox, ShouldEqual, filepath.Join(roots.ConfigRoot, "inbox.md"))
		})
	})

	Convey("Given the hosts beadle still models without an inbox", t, func() {
		for _, id := range []string{hostpath.Agy, hostpath.DSH} {
			s, err := hostpath.Surfaces(id, env("/h", "linux", nil))
			So(err, ShouldBeNil)
			So(s.Inbox, ShouldBeEmpty)
		}
	})
}

// N027: the gemini read order must carry its evidence, not just its rule.
func TestGeminiSkillsReadOrderIsCited(t *testing.T) {
	Convey("Given the SkillsReads field doc and the gemini surface", t, func() {
		Convey("Then gemini's order is the hub first and its own dir second", func() {
			s, err := hostpath.Surfaces(hostpath.Gemini, env("/h", "linux", nil))
			So(err, ShouldBeNil)
			So(s.SkillsReads, ShouldResemble, []string{
				filepath.Join("/h/.agents", "skills"),
				filepath.Join("/h/.gemini", "skills"),
			})
			// The hub is NOT the write target for gemini, even though it is
			// read first: writing there would leak across every other host.
			So(s.Skills, ShouldEqual, filepath.Join("/h/.gemini", "skills"))
		})
	})
}

// N028: the codex read order was unproven. It is now measured (codex-cli
// 0.158.0) and pinned, so a later "simplification" that swaps the two entries
// has to be a deliberate act rather than a silent one.
func TestCodexSkillsReadOrderIsPinned(t *testing.T) {
	Convey("Given codex", t, func() {
		s, err := hostpath.Surfaces(hostpath.Codex, env("/h", "linux", nil))
		So(err, ShouldBeNil)

		Convey("Then the hub is offered before the host's own skills directory", func() {
			So(s.SkillsReads, ShouldResemble, []string{
				filepath.Join("/h/.agents", "skills"),
				filepath.Join("/h/.codex", "skills"),
			})
		})

		Convey("Then the deprecated directory is still read, and codex writes to the hub", func() {
			So(s.SkillsReads, ShouldContain, filepath.Join("/h/.codex", "skills"))
			So(s.Skills, ShouldEqual, filepath.Join("/h/.agents", "skills"))
			So(s.SharedSkillsWrite, ShouldEqual, s.Skills)
		})
	})
}
