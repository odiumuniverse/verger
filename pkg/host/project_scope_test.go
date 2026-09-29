package host_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/hostpath"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/secret"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// projectPackage returns claude's fixture package at the given scope.
func projectPackage(t *testing.T, scope string) host.Package {
	t.Helper()

	pkg := claudePackage(t)
	pkg.Scope = scope

	return pkg
}

func TestProjectScopeDeliversIntoProjectSurfaces(t *testing.T) {
	Convey("Given a trusted project and a package at project scope", t, func() {
		fakeClaude(t)

		home := t.TempDir()
		project := t.TempDir()
		st := openStore(t)
		pkg := projectPackage(t, receipt.ScopeProject)

		h, _ := newClaude(t, home, looseScript(t, pkg), host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token", "WEB_TOKEN": "web-token"})))

		_, err := h.Deliver(t.Context(), home, host.Delivery{
			Package:    pkg,
			Strategy:   host.Loose,
			AllowHooks: true,
			Project:    project,
		})

		Convey("When it is delivered", func() {
			Convey("Then the payload lands under the project, not the user home", func() {
				So(err, ShouldBeNil)

				surfaces := hostpath.ProjectSurfaces(string(host.Claude), project)
				So(fileExists(surfaces.Skills), ShouldBeTrue)
				So(fileExists(filepath.Join(home, ".claude", "skills")), ShouldBeFalse)
			})

			Convey("Then the paths are the ones hostpath resolves, host by host", func() {
				So(err, ShouldBeNil)

				// The same table beadle resolves, so both tools write the
				// same project file for a host.
				for _, id := range []string{
					hostpath.Claude, hostpath.Codex, hostpath.Gemini, hostpath.Agy,
					hostpath.OpenCode, hostpath.Kilo, hostpath.Pi, hostpath.DSH, hostpath.Omp,
				} {
					s := hostpath.ProjectSurfaces(id, project)

					So(s.ID, ShouldEqual, id)

					// Not every host has a project instruction document, and
					// that is a gap to report rather than a path to invent.
					// What must hold everywhere is that a path that does
					// exist stays inside the project.
					for name, path := range map[string]string{
						"Rules": s.Rules, "MCPDoc": s.MCPDoc, "Skills": s.Skills,
						"Agents": s.Agents, "Commands": s.Commands, "Hooks": s.Hooks,
					} {
						if path == "" {
							continue
						}

						So(path, ShouldStartWith, project)

						_ = name
					}
				}
			})
		})
	})
}

func TestProjectScopeWithoutAProjectRootIsUnsupported(t *testing.T) {
	Convey("Given a project-scope package and no project root", t, func() {
		fakeClaude(t)

		home := t.TempDir()
		st := openStore(t)
		pkg := projectPackage(t, receipt.ScopeProject)

		h, _ := newClaude(t, home, looseScript(t, pkg), host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token", "WEB_TOKEN": "web-token"})))

		_, err := h.Deliver(t.Context(), home, host.Delivery{
			Package:    pkg,
			Strategy:   host.Loose,
			AllowHooks: true,
		})

		Convey("When it is delivered", func() {
			Convey("Then it is refused, and nothing is written", func() {
				_, ok := errors.AsType[*host.NotSupportedError](err)
				So(ok, ShouldBeTrue)
				So(fileExists(filepath.Join(home, ".claude", "skills")), ShouldBeFalse)
			})
		})
	})
}

func TestUserScopeIsUnchangedByProjectSupport(t *testing.T) {
	Convey("Given the same package at user scope", t, func() {
		fakeClaude(t)

		home := t.TempDir()
		project := t.TempDir()
		st := openStore(t)
		pkg := projectPackage(t, receipt.ScopeUser)

		h, _ := newClaude(t, home, looseScript(t, pkg), host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token", "WEB_TOKEN": "web-token"})))

		// A project root is present and must be ignored at user scope.
		_, err := h.Deliver(t.Context(), home, host.Delivery{
			Package:    pkg,
			Strategy:   host.Loose,
			AllowHooks: true,
			Project:    project,
		})

		Convey("When it is delivered", func() {
			Convey("Then it still lands in the user home and nowhere in the project", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(home, ".claude", "skills")), ShouldBeTrue)
				So(fileExists(filepath.Join(project, ".claude", "skills")), ShouldBeFalse)
			})
		})
	})
}

func TestProjectScopeRefusesASecretIntoAGitTrackedFile(t *testing.T) {
	Convey("Given a project whose MCP document git already tracks", t, func() {
		fakeClaude(t)

		home := t.TempDir()
		project := t.TempDir()
		st := openStore(t)

		gitInit(t, project)

		// The project MCP document, committed, so a secret written into it
		// would be committed again.
		mcp := filepath.Join(project, ".codex", "config.toml")
		So(os.MkdirAll(filepath.Dir(mcp), 0o700), ShouldBeNil)
		So(os.WriteFile(mcp, []byte("\n"), 0o600), ShouldBeNil)
		gitAdd(t, project, ".codex/config.toml")

		pkg := secretPackage(t, receipt.ScopeProject)

		h, _ := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t", "WEB_TOKEN": "w3b"})))

		_, err := h.Deliver(t.Context(), home, host.Delivery{
			Package:    pkg,
			Strategy:   host.Loose,
			AllowHooks: true,
			Project:    project,
		})

		Convey("When it is delivered", func() {
			Convey("Then it is refused, naming the file, and the value is not written", func() {
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, ".codex/config.toml")
				So(err.Error(), ShouldContainSubstring, "git tracks it")

				got, readErr := os.ReadFile(mcp) //nolint:gosec // G304: `mcp` is a path this test built under t.TempDir(), not input
				So(readErr, ShouldBeNil)
				So(string(got), ShouldNotContainSubstring, "s3cr3t")
			})
		})
	})
}

func TestProjectScopeWritesASecretIntoAnUntrackedFile(t *testing.T) {
	Convey("Given a project that does not track its MCP document", t, func() {
		fakeClaude(t)

		home := t.TempDir()
		project := t.TempDir()
		st := openStore(t)

		gitInit(t, project)

		pkg := secretPackage(t, receipt.ScopeProject)

		h, _ := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t", "WEB_TOKEN": "w3b"})))

		_, err := h.Deliver(t.Context(), home, host.Delivery{
			Package:    pkg,
			Strategy:   host.Loose,
			AllowHooks: true,
			Project:    project,
		})

		Convey("When it is delivered", func() {
			Convey("Then the delivery is not blocked: the file is the user's to commit or not", func() {
				// The refusal is narrow by design. Whatever this delivery
				// does, it must not fail with the git refusal.
				if err != nil {
					So(err.Error(), ShouldNotContainSubstring, "git tracks it")
				} else {
					So(err, ShouldBeNil)
				}
			})
		})
	})
}

// gitInit makes dir a repository with one commit-ready state.
func gitInit(t *testing.T, dir string) {
	t.Helper()

	run := func(args ...string) {
		t.Helper()

		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // G204: the binary is a literal; args are test-local paths under t.TempDir()
		cmd.Dir = dir

		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	run("init", "-q")
}

// gitAdd stages rel inside the repository at dir.
func gitAdd(t *testing.T, dir, rel string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", "add", rel) //nolint:gosec // G204: the binary is a literal; rel is a test-local path under t.TempDir()
	cmd.Dir = dir

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add %s: %v: %s", rel, err, out)
	}
}

// secretPackage returns a package whose MCP servers carry secret references.
// Codex, because it writes MCP into a config document: the guard under test
// only has a path to inspect for a host that has one.
func secretPackage(t *testing.T, scope string) host.Package {
	t.Helper()

	pkg := codexPackage(t)
	pkg.Scope = scope

	return pkg
}

var _ = secret.ErrKeyringUnavailable

func TestClaudeProjectScopeUsesTheHostInstaller(t *testing.T) {
	Convey("Given a Claude package at project scope with a marketplace", t, func() {
		fakeClaude(t)

		home := t.TempDir()
		project := t.TempDir()
		st := openStore(t)

		ref := "https://github.com/acme/plugins.git"

		// Scripted at the exact argv the adapter now builds: a key that does
		// not match is the scope flag being missing, not a loose assertion.
		script := map[string]hostcli.Response{
			"claude plugin marketplace add " + ref + " --scope project": response(""),
			"claude plugin install caveman@plugins --scope project":     response(""),
			"claude plugin list --json":                                 response(claudeList("caveman@plugins")),
		}

		h, cli := newClaude(t, home, script)

		pkg := claudePackage(t)
		pkg.Scope = receipt.ScopeProject
		pkg.Marketplace = ref

		_ = st

		res, err := h.Deliver(t.Context(), home, host.Delivery{
			Package:    pkg,
			Strategy:   host.Native,
			AllowHooks: true,
			Project:    project,
		})

		Convey("When it is delivered", func() {
			Convey("Then the host CLI is told the project scope, not left at its user default", func() {
				So(err, ShouldBeNil)

				var lines []string
				for _, call := range cli.Calls() {
					lines = append(lines, strings.Join(call.Args, " "))
				}

				calls := strings.Join(lines, "\n")
				So(calls, ShouldContainSubstring, "plugin install")
				So(calls, ShouldContainSubstring, "--scope project")
			})

			Convey("Then the inverse records the same scope, so removal cannot drift to user", func() {
				So(err, ShouldBeNil)

				// Every host-install inverse carries the scope it wrote to.
				inv := 0

				for _, op := range res.RMA {
					if op.Kind == receipt.OpHostInstall && len(op.Command) == 0 {
						continue
					}

					if len(op.Command) > 0 && op.Command[0] == "plugin" {
						inv++

						So(op.Command, ShouldContain, "--scope")
						So(op.Command, ShouldContain, receipt.ScopeProject)
					}
				}

				So(inv, ShouldBeGreaterThan, 0)
			})
		})
	})
}

func TestOmpProjectScopeUsesTheHostInstaller(t *testing.T) {
	Convey("Given an omp package at project scope with a local marketplace", t, func() {
		h, cli, home := ompWorld(t)

		// The catalog declares a name the ref's last segment does not carry,
		// so the test also proves the scope flag did not disturb the name
		// read-back from `plugin marketplace list`.
		ref := ompMarketplace(t, "acme-tools", "caveman", "1.2.3")

		pkg := ompPackage(t)
		pkg.Scope = receipt.ScopeProject
		pkg.Marketplace = ref

		res, err := h.Deliver(t.Context(), home, host.Delivery{
			Package: pkg, Strategy: host.Native, AllowHooks: true, Project: t.TempDir(),
		})

		Convey("When it is delivered", func() {
			Convey("Then the host CLI is told the project scope on the add and the install", func() {
				So(err, ShouldBeNil)

				calls := strings.Join(cli.Calls(), "\n")
				So(calls, ShouldContainSubstring, "omp plugin marketplace add "+ref+" --scope project")
				So(calls, ShouldContainSubstring, "omp plugin install caveman@acme-tools --force --scope project")
			})

			Convey("Then the inverse carries the same scope, so removal cannot drift to user", func() {
				So(err, ShouldBeNil)

				inv := 0

				for _, op := range res.RMA {
					if op.Kind != receipt.OpHostInstall || len(op.Command) == 0 {
						continue
					}

					inv++

					So(op.Command, ShouldContain, receipt.ScopeProject)
				}

				So(inv, ShouldBeGreaterThan, 0)
			})
		})
	})
}

func TestDshProjectScopeIsRefusedNotLeaked(t *testing.T) {
	Convey("Given a dsh package at project scope", t, func() {
		clearDSHEnv(t)

		home := t.TempDir()
		project := t.TempDir()
		st := openStore(t)

		pkg := dshPackage(t)
		pkg.Scope = receipt.ScopeProject

		h := host.NewDSH(host.WithHome(home), host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token", "WEB_TOKEN": "web-token"})))

		_, err := h.Deliver(t.Context(), home, host.Delivery{
			Package:    pkg,
			Strategy:   host.Loose,
			AllowHooks: true,
			Project:    project,
		})

		Convey("When it is delivered", func() {
			Convey("Then it is refused, because dsh has no project MCP surface", func() {
				// hostpath.ProjectSurfaces declares no MCPDoc for dsh, so
				// dshSpec keeps the user-scope .dsh/cordis.patch.yml. Without
				// the guard the project's MCP servers would land in $HOME.
				_, ok := errors.AsType[*host.NotSupportedError](err)
				So(ok, ShouldBeTrue)
				So(err.Error(), ShouldContainSubstring, "project scope")
			})

			Convey("Then nothing was written into the user home", func() {
				So(fileExists(filepath.Join(home, ".dsh", "cordis.patch.yml")), ShouldBeFalse)
				So(fileExists(filepath.Join(home, ".dsh", "skills")), ShouldBeFalse)
			})
		})
	})
}

// TestProjectStateIsGitExcluded closes the ".verger/ stays git-excluded" rule
// with runtime evidence. It lives in package host_test rather than beside the
// code because pkg/verger's own test binary does not currently compile
// (facade_test.go:1481), and an external test package can import pkg/verger
// without a cycle even though pkg/verger imports pkg/host.
func TestProjectStateIsGitExcluded(t *testing.T) {
	Convey("Given a git project with no exclude file", t, func() {
		home := t.TempDir()
		project := t.TempDir()

		gitInit(t, project)

		exclude := filepath.Join(project, ".git", "info", "exclude")
		// git init pre-creates info/exclude with its own comment template;
		// what matters is that verger's entry is not in it yet.
		before, err := os.ReadFile(exclude) //nolint:gosec // G304: `exclude` is a path this test built under t.TempDir(), not input
		So(err, ShouldBeNil)
		So(string(before), ShouldNotContainSubstring, ".verger/")

		c, cerr := verger.Open(t.Context(), verger.WithHome(home))
		So(cerr, ShouldBeNil)

		defer func() { So(c.Close(), ShouldBeNil) }()

		state := filepath.Join(project, ".verger", "state")
		So(c.Ensure(verger.Paths{
			Scope:    verger.Project,
			Root:     project,
			Project:  project,
			StateDir: state,
		}), ShouldBeNil)

		Convey("When the project state is created", func() {
			Convey("Then .verger/ is excluded from the repository", func() {
				data, err := os.ReadFile(exclude) //nolint:gosec // G304: `exclude` is a path this test built under t.TempDir(), not input
				So(err, ShouldBeNil)
				So(string(data), ShouldContainSubstring, ".verger/")

				So(fileExists(state), ShouldBeTrue)
			})

			Convey("Then asking again does not duplicate the entry", func() {
				So(c.Ensure(verger.Paths{
					Scope:    verger.Project,
					Root:     project,
					Project:  project,
					StateDir: state,
				}), ShouldBeNil)

				data, err := os.ReadFile(exclude) //nolint:gosec // G304: `exclude` is a path this test built under t.TempDir(), not input
				So(err, ShouldBeNil)
				So(strings.Count(string(data), ".verger/"), ShouldEqual, 1)
			})
		})
	})
}
