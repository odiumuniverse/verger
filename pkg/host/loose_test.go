package host_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
	"github.com/odiumuniverse/verger/pkg/secret"
	"github.com/odiumuniverse/verger/pkg/store"
)

// openStore returns a payload store rooted in a temp dir.
func openStore(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	return st
}

// ownerMap scripts PathOwner answers: absolute path → package id.
type ownerMap map[string]string

// Owner implements host.PathOwner.
func (o ownerMap) Owner(path string) (string, bool) {
	pkg, ok := o[path]

	return pkg, ok
}

// secretStore seeds a file-backend secret store with the given values.
func secretStore(t *testing.T, values map[string]string) *secret.Store {
	t.Helper()

	st, err := secret.Load(filepath.Join(t.TempDir(), "secrets.json"))
	if err != nil {
		t.Fatalf("load secrets: %v", err)
	}

	for name, value := range values {
		st.Set(name, value)
	}

	return st
}

// snapshotHome records every path below root as dir/file:mode:digest/link.
func snapshotHome(t *testing.T, root string) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}

		if rel == "." {
			return nil
		}

		rel = filepath.ToSlash(rel)

		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			target, linkErr := os.Readlink(path)
			if linkErr != nil {
				return linkErr
			}

			out[rel] = "link:" + target
		case entry.IsDir():
			out[rel] = "dir"
		default:
			data, readErr := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp home
			if readErr != nil {
				return readErr
			}

			out[rel] = "file:" + entry.Type().Perm().String() + ":" + digest.Bytes(data).String()
		}

		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}

	return out
}

// looseScript scripts the two MCP adds of the golden package and the
// ownership probes that prove both names free.
func looseScript(t *testing.T, pkg host.Package) map[string]hostcli.Response {
	t.Helper()

	adds := renderMCPAddKeys(t, pkg, "s3cr3t-token", "web-token")

	script := map[string]hostcli.Response{
		"claude mcp get fs":  mcpAbsent("fs"),
		"claude mcp get web": mcpAbsent("web"),
	}

	for _, key := range adds {
		script[key] = response("")
	}

	return script
}

// renderMCPAddKeys builds the ScriptRunner keys of the golden MCP adds.
func renderMCPAddKeys(t *testing.T, pkg host.Package, fsToken, webToken string) []string {
	t.Helper()

	return []string{
		"claude mcp add --scope user --transport stdio fs node " + pkg.Root + "/server.js --env TOKEN=" + fsToken,
		"claude mcp add --scope user --transport sse web https://example.com/mcp --header Authorization: Bearer " + webToken,
	}
}

// goldenHomeFiles is the exact loose output tree of the golden package.
func goldenHomeFiles() []string {
	return []string{
		".claude/agents/reviewer.md",
		".claude/commands/dev.md",
		".claude/settings.json",
		".claude/skills/alpha/SKILL.md",
		".claude/skills/alpha/scripts/run.sh",
		".claude/skills/beta/SKILL.md",
		".claude/skills/rule-caveman/SKILL.md",
	}
}

func TestLooseGolden(t *testing.T) {
	Convey("Given a Claude package and a temp home", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		writeSettings(t, home, `{"model": "opus"}`)

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token", "WEB_TOKEN": "web-token"})
		pkg := claudePackage(t)

		h, runner := newClaude(t, home, looseScript(t, pkg),
			host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered loose", func() {
			Convey("Then the exact home tree appears with 0700 dirs and 0600 files", func() {
				So(err, ShouldBeNil)
				So(homeFiles(t, home), ShouldResemble, goldenHomeFiles())
				assertHomeModes(t, home)
			})

			Convey("Then agents and commands are rendered bytes with matching digests", func() {
				agent, readErr := os.ReadFile(filepath.Join(home, ".claude", "agents", "reviewer.md")) //nolint:gosec // G304: the test reads its own temp home
				So(readErr, ShouldBeNil)
				So(digestOf(t, res, "agent", "reviewer"), ShouldEqual, digest.Bytes(agent))

				command, readErr := os.ReadFile(filepath.Join(home, ".claude", "commands", "dev.md")) //nolint:gosec // G304: the test reads its own temp home
				So(readErr, ShouldBeNil)
				So(digestOf(t, res, "command", "dev"), ShouldEqual, digest.Bytes(command))
			})

			Convey("Then skills keep their content-hash digest", func() {
				for _, component := range pkg.Components {
					if component.Kind != manifest.KindSkill {
						continue
					}

					target := filepath.Join(home, ".claude", "skills", component.Name)
					sum, sumErr := digest.Tree(target)
					So(sumErr, ShouldBeNil)
					So(digestOf(t, res, "skill", component.Name), ShouldEqual, sum)
				}
			})

			Convey("Then settings.json gains the rewritten hooks and keeps foreign keys", func() {
				settings, readErr := os.ReadFile(filepath.Join(home, ".claude", "settings.json")) //nolint:gosec // G304: the test reads its own temp home
				So(readErr, ShouldBeNil)
				So(string(settings), ShouldEqualJSON, `{
  "model": "opus",
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "node \"`+pkg.Root+`/hooks/guard.js\"", "timeout": 5}]}
    ]
  }
}`)
			})

			Convey("Then MCP names are proven free, then added with resolved secrets only in argv", func() {
				So(callKeys(runner), ShouldResemble, append(
					[]string{"claude mcp get fs", "claude mcp get web"},
					renderMCPAddKeys(t, pkg, "s3cr3t-token", "web-token")...,
				))

				rendered, marshalErr := json.Marshal(res)
				So(marshalErr, ShouldBeNil)
				So(string(rendered), ShouldNotContainSubstring, "s3cr3t-token")
				So(string(rendered), ShouldNotContainSubstring, "web-token")
			})

			Convey("Then the RMA mirrors every artifact in install order", func() {
				So(res.RMA, ShouldHaveLength, len(res.Artifacts))

				ops := map[string]receipt.Op{}
				for _, op := range res.RMA {
					ops[op.Path] = op
				}

				So(ops[filepath.Join(home, ".claude", "skills", "alpha")].Kind, ShouldEqual, receipt.OpCopyTree)
				So(ops[filepath.Join(home, ".claude", "agents", "reviewer.md")].Kind, ShouldEqual, receipt.OpWriteFile)
				So(ops[filepath.Join(home, ".claude", "settings.json")].Kind, ShouldEqual, receipt.OpConfigKey)
				So(ops[filepath.Join(home, ".claude", "settings.json")].KeyPath, ShouldEqual, "hooks")

				hostOps := 0

				for _, op := range res.RMA {
					if op.Kind == receipt.OpHostInstall {
						hostOps++
					}
				}

				So(hostOps, ShouldEqual, 2)
			})
		})
	})
}

// withoutMCP strips the MCP servers for tests that exercise other surfaces.
func withoutMCP(pkg host.Package) host.Package {
	pkg.MCP = nil

	return pkg
}

// homeFiles lists the relative files and symlinks below home, sorted.
func homeFiles(t *testing.T, home string) []string {
	t.Helper()

	var out []string

	err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		rel, relErr := filepath.Rel(home, path)
		if relErr != nil {
			return relErr
		}

		out = append(out, filepath.ToSlash(rel))

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", home, err)
	}

	slices.Sort(out)

	return out
}

// assertHomeModes checks 0700 dirs and 0600 files below home; the home root
// itself belongs to the test harness.
func assertHomeModes(t *testing.T, home string) {
	t.Helper()

	err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if path == home || entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}

		want := fs.FileMode(0o600)
		if entry.IsDir() {
			want = 0o700
		}

		if got := info.Mode().Perm(); got != want {
			t.Errorf("mode of %s = %o, want %o", path, got, want)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", home, err)
	}
}

// digestOf returns the artifact digest of one kind/name.
func digestOf(t *testing.T, res host.Result, kind, name string) digest.Hash {
	t.Helper()

	for _, artifact := range res.Artifacts {
		if artifact.Kind == kind && artifact.Name == name {
			return artifact.Digest
		}
	}

	t.Fatalf("artifact %s/%s not found", kind, name)

	return ""
}

func TestLooseCollisions(t *testing.T) {
	Convey("Given a foreign skill directory in the target home", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		foreign := filepath.Join(home, ".claude", "skills", "alpha")

		writeFixtureFile(t, filepath.Join(foreign, "SKILL.md"), "foreign\n", 0o600)

		st := openStore(t)
		h, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: claudePackage(t), Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then a *CollisionError stops the cell before any write", func() {
				typed, ok := errors.AsType[*host.CollisionError](err)
				So(ok, ShouldBeTrue)
				So(typed.Path, ShouldEqual, foreign)
				So(typed.Owner, ShouldBeEmpty)
				So(fileExists(filepath.Join(home, ".claude", "agents")), ShouldBeFalse)
			})
		})
	})

	Convey("Given a file owned by another package", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		target := filepath.Join(home, ".claude", "agents", "reviewer.md")

		writeFixtureFile(t, target, "other agent\n", 0o600)

		st := openStore(t)
		h, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithOwnership(ownerMap{target: "other/pkg"}))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: claudePackage(t), Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then the collision names the other owner", func() {
				typed, ok := errors.AsType[*host.CollisionError](err)
				So(ok, ShouldBeTrue)
				So(typed.Owner, ShouldEqual, "other/pkg")
			})
		})
	})

	Convey("Given a file owned by this package", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		target := filepath.Join(home, ".claude", "agents", "reviewer.md")

		writeFixtureFile(t, target, "old agent\n", 0o600)

		st := openStore(t)
		script := looseScript(t, claudePackage(t))
		h, _ := newClaude(t, home, script, host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token", "WEB_TOKEN": "web-token"})),
			host.WithOwnership(ownerMap{target: "acme/caveman"}))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: claudePackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then it is replaced and the previous bytes wait in trash", func() {
				So(err, ShouldBeNil)

				data, readErr := os.ReadFile(target) //nolint:gosec // G304: the test reads its own temp home
				So(readErr, ShouldBeNil)
				So(string(data), ShouldNotContainSubstring, "old agent")

				trashed := trashContains(t, st, "old agent")
				So(trashed, ShouldBeTrue)

				op := rmaOp(res, target)
				So(op.Existed, ShouldBeTrue)
				So(op.Backup, ShouldNotBeEmpty)
			})
		})
	})
}

// writeFixtureFile writes one test file with an explicit mode.
func writeFixtureFile(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Lstat(path)

	return err == nil
}

// trashContains reports whether any trash bucket payload holds the fragment.
func trashContains(t *testing.T, st *store.Store, fragment string) bool {
	t.Helper()

	found := false

	_ = filepath.WalkDir(st.TrashDir(), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		data, readErr := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp trash
		if readErr == nil && bytes.Contains(data, []byte(fragment)) {
			found = true
		}

		return nil
	})

	return found
}

// rmaOp returns the RMA op for one absolute path.
func rmaOp(res host.Result, path string) receipt.Op {
	for _, op := range res.RMA {
		if op.Path == path {
			return op
		}
	}

	return receipt.Op{}
}

func TestLooseCommandForeignCollision(t *testing.T) {
	Convey("Given a foreign command file", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		target := filepath.Join(home, ".claude", "commands", "dev.md")

		writeFixtureFile(t, target, "foreign command\n", 0o600)

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token", "WEB_TOKEN": "web-token"})
		pkg := claudePackage(t)

		h, _ := newClaude(t, home, looseScript(t, pkg), host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then the command is skipped with a note, never prefixed or overwritten", func() {
				So(err, ShouldBeNil)

				data, readErr := os.ReadFile(target) //nolint:gosec // G304: the test reads its own temp home
				So(readErr, ShouldBeNil)
				So(string(data), ShouldEqual, "foreign command\n")
				So(fileExists(filepath.Join(home, ".claude", "skills", "alpha")), ShouldBeTrue)

				var noted bool

				for _, note := range res.Notes {
					if strings.Contains(note, "dev") && strings.Contains(note, "skipped") {
						noted = true
					}
				}

				So(noted, ShouldBeTrue)

				for _, artifact := range res.Artifacts {
					So(artifact.Name, ShouldNotEqual, "dev")
				}
			})
		})
	})
}

func TestLooseHooksGating(t *testing.T) {
	Convey("Given AllowHooks=false", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		writeSettings(t, home, `{"model": "opus"}`)

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token", "WEB_TOKEN": "web-token"})
		pkg := claudePackage(t)

		h, _ := newClaude(t, home, looseScript(t, pkg), host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then settings.json is untouched and no hook artifact or RMA exists", func() {
				So(err, ShouldBeNil)

				settings, readErr := os.ReadFile(filepath.Join(home, ".claude", "settings.json")) //nolint:gosec // G304: the test reads its own temp home
				So(readErr, ShouldBeNil)
				So(string(settings), ShouldEqualJSON, `{"model": "opus"}`)

				for _, artifact := range res.Artifacts {
					So(artifact.Kind, ShouldNotEqual, "hook")
				}

				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "consent")
			})
		})
	})

	Convey("Given AllowHooks=true", t, func() {
		fakeClaude(t)
		home := t.TempDir()

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token", "WEB_TOKEN": "web-token"})
		pkg := claudePackage(t)

		h, _ := newClaude(t, home, looseScript(t, pkg), host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

		Convey("When the package is delivered", func() {
			Convey("Then the hooks artifact appears", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(home, ".claude", "settings.json")), ShouldBeTrue)
				So(digestOf(t, res, "hook", "hooks"), ShouldNotBeEmpty)
			})
		})
	})
}

func TestLooseMissingSecrets(t *testing.T) {
	Convey("Given a package whose secrets are missing", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		st := openStore(t)
		pkg := claudePackage(t)

		h, runner := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(secretStore(t, nil)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then it is a *MissingSecretsError and nothing is written or called", func() {
				typed, ok := errors.AsType[*host.MissingSecretsError](err)
				So(ok, ShouldBeTrue)
				So(typed.Names, ShouldResemble, []string{"MCP_TOKEN", "WEB_TOKEN"})
				So(runner.Calls(), ShouldBeEmpty)
				So(fileExists(filepath.Join(home, ".claude", "skills")), ShouldBeFalse)
			})
		})
	})
}

func TestLooseUnsupportedVariable(t *testing.T) {
	Convey("Given a package carrying an unknown host variable", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		st := openStore(t)
		pkg := claudePackage(t)

		pkg.MCP = []manifest.MCPServer{{
			Name:    "bad",
			Command: []string{"node", "${FOO_BAR}/server.js"},
		}}

		h, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then it refuses with the variable named and writes nothing", func() {
				typed, ok := errors.AsType[*host.UnsupportedVariableError](err)
				So(ok, ShouldBeTrue)
				So(typed.Variable, ShouldEqual, "FOO_BAR")
				So(fileExists(filepath.Join(home, ".claude")), ShouldBeFalse)
			})
		})
	})
}

func TestLooseDataVariable(t *testing.T) {
	Convey("Given a hook referencing the plugin data dir", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		st := openStore(t)
		pkg := withoutMCP(claudePackage(t))

		pkg.Hooks = []manifest.Hook{{
			Event: manifest.EventPostTool, Matcher: "Bash", Command: "node ${CLAUDE_PLUGIN_DATA}/hook.js",
		}}

		h, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then the data dir is created and the hook points at it", func() {
				So(err, ShouldBeNil)

				dataDir, pathErr := st.PackageDataPath(pkg.ID, "claude")
				So(pathErr, ShouldBeNil)
				So(fileExists(dataDir), ShouldBeTrue)

				settings, readErr := os.ReadFile(filepath.Join(home, ".claude", "settings.json")) //nolint:gosec // G304: the test reads its own temp home
				So(readErr, ShouldBeNil)
				So(string(settings), ShouldContainSubstring, dataDir+"/hook.js")
			})
		})
	})
}

func TestLooseDryRun(t *testing.T) {
	Convey("Given a loose dry run", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		st := openStore(t)
		before := snapshotHome(t, home)

		h, runner := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		res, err := h.Deliver(t.Context(), home, host.Delivery{
			Package: withoutMCP(claudePackage(t)), Strategy: host.Loose, AllowHooks: true, DryRun: true,
		})

		Convey("When it is delivered", func() {
			Convey("Then nothing is written or called while the plan is returned", func() {
				So(err, ShouldBeNil)
				So(runner.Calls(), ShouldBeEmpty)
				So(snapshotHome(t, home), ShouldResemble, before)
				So(res.Artifacts, ShouldNotBeEmpty)
				So(res.RMA, ShouldNotBeEmpty)
				So(res.Notes, ShouldContain, "dry-run")
			})
		})
	})
}

func TestLooseSymlinks(t *testing.T) {
	Convey("Given a skill tree holding a symlink", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		root := copyFixture(t)
		outside := filepath.Join(t.TempDir(), "outside.md")

		writeFixtureFile(t, outside, "outside secret\n", 0o600)

		if err := os.Symlink("../../../outside.md", filepath.Join(root, "skills", "alpha", "link.md")); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		pkg := withoutMCP(parsePackage(t, root))
		st := openStore(t)

		h, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then the link is recreated with its target text, never followed", func() {
				So(err, ShouldBeNil)

				link := filepath.Join(home, ".claude", "skills", "alpha", "link.md")

				info, statErr := os.Lstat(link)
				So(statErr, ShouldBeNil)
				So(info.Mode()&fs.ModeSymlink != 0, ShouldBeTrue)

				target, readErr := os.Readlink(link)
				So(readErr, ShouldBeNil)
				So(target, ShouldEqual, "../../../outside.md")
			})
		})
	})

	Convey("Given a component path that is a symlink", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		root := copyFixture(t)
		outside := filepath.Join(t.TempDir(), "outside-agent.md")

		writeFixtureFile(t, outside, "outside agent\n", 0o600)

		if err := os.Symlink(outside, filepath.Join(root, "agents", "linked.md")); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		pkg := withoutMCP(parsePackage(t, root))
		pkg.Components = []manifest.Component{{
			Kind: manifest.KindAgent, Name: "linked", Path: "agents/linked.md",
			Digest: digest.Bytes([]byte("x")),
		}}

		st := openStore(t)
		h, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then the symlinked component is refused without a write", func() {
				typed, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(typed.Step, ShouldEqual, "plan")
				So(fileExists(filepath.Join(home, ".claude", "agents")), ShouldBeFalse)
			})
		})
	})
}

// copyFixture copies the fixture tree into a temp dir.
func copyFixture(t *testing.T) string {
	t.Helper()

	root := filepath.Join(t.TempDir(), "acme")

	if err := copyDir(fixtureRoot, root); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}

	return root
}

// parsePackage parses one Claude fixture root into a host.Package.
func parsePackage(t *testing.T, root string) host.Package {
	t.Helper()

	parsed, err := manifest.Parse(root, manifest.FormatClaude, manifest.WithID("acme/caveman"))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	return host.Package{
		ID: parsed.ID, Version: parsed.Version, Format: parsed.Format, Root: parsed.Root,
		Components: parsed.Components, MCP: parsed.MCP, Hooks: parsed.Hooks,
	}
}

// copyDir copies a directory tree, recreating symlinks.
func copyDir(from, to string) error {
	return filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(from, path)
		if relErr != nil {
			return relErr
		}

		target := filepath.Join(to, rel)

		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			link, linkErr := os.Readlink(path)
			if linkErr != nil {
				return linkErr
			}

			return os.Symlink(link, target) //nolint:gosec // G122: the test fixture copy recreates links verbatim
		case entry.IsDir():
			return os.MkdirAll(target, 0o700)
		default:
			data, readErr := os.ReadFile(path) //nolint:gosec // G304: the test copies its own fixture
			if readErr != nil {
				return readErr
			}

			return os.WriteFile(target, data, 0o600) //nolint:gosec // G703: the target is below the test's temp dir
		}
	})
}

func TestLooseEmptyPackage(t *testing.T) {
	Convey("Given an empty loose package", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		st := openStore(t)
		before := snapshotHome(t, home)

		h, runner := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		res, err := h.Deliver(t.Context(), home, host.Delivery{
			Package: host.Package{ID: "acme/empty", Version: "1.0.0", Root: t.TempDir()}, Strategy: host.Loose,
		})

		Convey("When it is delivered", func() {
			Convey("Then nothing is created", func() {
				So(err, ShouldBeNil)
				So(res.Artifacts, ShouldBeEmpty)
				So(runner.Calls(), ShouldBeEmpty)
				So(snapshotHome(t, home), ShouldResemble, before)
			})
		})
	})
}

func TestLooseUnreadableConfig(t *testing.T) {
	Convey("Given settings.json that is unreadable", t, func() {
		fakeClaude(t)
		home := t.TempDir()

		if err := os.MkdirAll(filepath.Join(home, ".claude", "settings.json"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		st := openStore(t)
		h, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		_, err := h.Deliver(t.Context(), home, host.Delivery{
			Package: claudePackage(t), Strategy: host.Loose, AllowHooks: true,
		})

		Convey("When it is delivered", func() {
			Convey("Then it fails with a *DeliveryError before any artifact is written", func() {
				// settings.json is also Claude's policy document: the gate reads
				// it first for every source ref, the empty one included (R4).
				typed, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(typed.Step, ShouldEqual, "policy")
				So(fileExists(filepath.Join(home, ".claude", "skills")), ShouldBeFalse)
			})
		})
	})
}

func TestLooseReadOnlyConfig(t *testing.T) {
	Convey("Given a read-only settings.json", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		writeSettings(t, home, `{"model": "opus"}`)

		settings := filepath.Join(home, ".claude", "settings.json")
		if err := os.Chmod(settings, 0o400); err != nil {
			t.Fatalf("chmod: %v", err)
		}

		before, readErr := os.ReadFile(settings) //nolint:gosec // G304: the test reads its own temp home
		So(readErr, ShouldBeNil)

		st := openStore(t)
		h, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		_, err := h.Deliver(t.Context(), home, host.Delivery{
			Package: withoutMCP(claudePackage(t)), Strategy: host.Loose, AllowHooks: true,
		})

		Convey("When it is delivered", func() {
			Convey("Then a *DeliveryError refuses and nothing is overwritten", func() {
				typed, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(typed.Step, ShouldEqual, "plan")

				after, readErr := os.ReadFile(settings) //nolint:gosec // G304: the test reads its own temp home
				So(readErr, ShouldBeNil)
				So(after, ShouldResemble, before)
				So(fileExists(filepath.Join(home, ".claude", "skills")), ShouldBeFalse)
			})
		})
	})
}

func TestLooseZeroByteConfig(t *testing.T) {
	Convey("Given a 0-byte settings.json", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		writeSettings(t, home, "")

		st := openStore(t)
		h, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		_, err := h.Deliver(t.Context(), home, host.Delivery{
			Package: withoutMCP(claudePackage(t)), Strategy: host.Loose, AllowHooks: true,
		})

		Convey("When it is delivered", func() {
			Convey("Then the hooks document is created over the empty file", func() {
				So(err, ShouldBeNil)

				settings, readErr := os.ReadFile(filepath.Join(home, ".claude", "settings.json")) //nolint:gosec // G304: the test reads its own temp home
				So(readErr, ShouldBeNil)
				So(string(settings), ShouldContainSubstring, "PreToolUse")
			})
		})
	})
}

func TestLooseUnicodeAndEmptyFiles(t *testing.T) {
	Convey("Given a unicode skill with a 0-byte file", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		root := t.TempDir()

		writeFixtureFile(t, filepath.Join(root, "skills", "умение", "SKILL.md"), "---\nname: умение\n---\n\nBody.\n", 0o600)
		writeFixtureFile(t, filepath.Join(root, "skills", "умение", "empty.bin"), "", 0o600)

		sum, sumErr := digest.Tree(filepath.Join(root, "skills", "умение"))
		So(sumErr, ShouldBeNil)

		pkg := host.Package{
			ID: "acme/умение", Version: "1.0.0", Root: root,
			Components: []manifest.Component{{Kind: manifest.KindSkill, Name: "умение", Path: "skills/умение", Digest: sum}},
		}

		st := openStore(t)
		h, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then unicode names and empty files survive", func() {
				So(err, ShouldBeNil)

				target := filepath.Join(home, ".claude", "skills", "умение")
				So(fileExists(filepath.Join(target, "SKILL.md")), ShouldBeTrue)

				empty, readErr := os.ReadFile(filepath.Join(target, "empty.bin")) //nolint:gosec // G304: the test reads its own temp home
				So(readErr, ShouldBeNil)
				So(empty, ShouldBeEmpty)
			})
		})
	})
}

func TestLooseConfigDirProfile(t *testing.T) {
	Convey("Given a CLAUDE_CONFIG_DIR profile", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		profile := filepath.Join(t.TempDir(), "claude-work")

		t.Setenv("CLAUDE_CONFIG_DIR", profile)

		st := openStore(t)
		h, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: withoutMCP(claudePackage(t)), Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then the profile dir receives everything and the home stays untouched", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(profile, "skills", "alpha", "SKILL.md")), ShouldBeTrue)
				So(fileExists(filepath.Join(home, ".claude")), ShouldBeFalse)
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})
}

func TestLooseConcurrentDryRun(t *testing.T) {
	Convey("Given one loose dry-run delivery from many goroutines", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		st := openStore(t)

		h, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		delivery := host.Delivery{Package: withoutMCP(claudePackage(t)), Strategy: host.Loose, AllowHooks: true, DryRun: true}

		results := make([]host.Result, 8)
		errs := make([]error, 8)

		var wg sync.WaitGroup

		for i := range results {
			wg.Go(func() {
				results[i], errs[i] = h.Deliver(t.Context(), home, delivery)
			})
		}

		wg.Wait()

		Convey("When every delivery finishes", func() {
			Convey("Then all plans are equal", func() {
				for i := range results {
					So(errs[i], ShouldBeNil)
					So(results[i].Artifacts, ShouldResemble, results[0].Artifacts)
				}
			})
		})
	})
}

// The scripted Claude MCP calls of one plain server named github.
const (
	mcpGetGithub    = "claude mcp get github"
	mcpAddGithub    = "claude mcp add --scope user --transport sse github https://example.com/mcp"
	mcpRemoveGithub = "claude mcp remove --scope user github"
	mcpGithubPath   = "claude://mcp/github"
)

// mcpPresent is the host's `claude mcp get` answer for an existing server.
func mcpPresent(name string) hostcli.Response {
	return response(name + ":\n  Scope: User config\n  Type: sse\n  URL: https://example.com/mcp\n")
}

// mcpAbsent is the host's `claude mcp get` answer for an unknown server.
func mcpAbsent(name string) hostcli.Response {
	return hostcli.Response{Code: 1, Stderr: "No MCP server found with name: " + name}
}

// mcpGatePackage is the Claude fixture carrying one plain MCP server, github.
func mcpGatePackage(t *testing.T) host.Package {
	t.Helper()

	pkg := withoutMCP(claudePackage(t))
	pkg.MCP = []manifest.MCPServer{{Name: "github", Transport: "sse", URL: "https://example.com/mcp"}}

	return pkg
}

// hasMCPRemove reports whether an RMA carries a host-install `mcp remove`.
func hasMCPRemove(ops []receipt.Op) bool {
	return slices.ContainsFunc(ops, func(op receipt.Op) bool {
		return op.Kind == receipt.OpHostInstall && len(op.Command) >= 2 && op.Command[0] == "mcp" && op.Command[1] == "remove"
	})
}

// TestLooseMCPOwnershipGate pins decision Q1 (T1.6-review-1 [R2]): before a
// CLI-managed MCP server is planned — dry-run included — the host is asked
// whether the name exists; a name no receipt of this package proves verger
// created is hands-off: never added, and no inverse `mcp remove` is recorded.
func TestLooseMCPOwnershipGate(t *testing.T) {
	Convey("Given a host that already has a user's own github server", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		st := openStore(t)

		h, runner := newClaude(t, home, map[string]hostcli.Response{
			mcpGetGithub:    mcpPresent("github"),
			mcpAddGithub:    response(""),
			mcpRemoveGithub: response(""),
		}, host.WithStore(st), host.WithTrash(st.Trash()))

		for _, dryRun := range []bool{false, true} {
			Convey(fmt.Sprintf("When a package declaring github is delivered (dry-run %v)", dryRun), func() {
				res, err := h.Deliver(t.Context(), home, host.Delivery{Package: mcpGatePackage(t), Strategy: host.Loose, DryRun: dryRun})

				Convey("Then the cell is hands-off naming the server", func() {
					typed, ok := errors.AsType[*render.HandsOffError](err)
					So(ok, ShouldBeTrue)
					So(typed.Path, ShouldEqual, mcpGithubPath)
					So(typed.KeyPath, ShouldEqual, "github")
				})

				Convey("Then only the read-only probe ran and nothing was planned or written", func() {
					So(callKeys(runner), ShouldResemble, []string{mcpGetGithub})
					So(res.RMA, ShouldBeEmpty)
					So(fileExists(filepath.Join(home, ".claude")), ShouldBeFalse)
				})
			})
		}
	})

	Convey("Given a github server a receipt of this package records", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		st := openStore(t)

		h, runner := newClaude(t, home, map[string]hostcli.Response{
			mcpGetGithub:    mcpPresent("github"),
			mcpRemoveGithub: response(""),
			mcpAddGithub:    response(""),
		}, host.WithStore(st), host.WithTrash(st.Trash()), host.WithOwnership(ownerMap{mcpGithubPath: "acme/caveman"}))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: mcpGatePackage(t), Strategy: host.Loose})

		Convey("When the package is delivered again and no receipt digest is known", func() {
			Convey("Then the receipt proves the name: the configured server is removed and re-added (NF-2)", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{mcpGetGithub, mcpRemoveGithub, mcpAddGithub})
				So(hasMCPRemove(res.RMA), ShouldBeTrue)
			})

			Convey("Then its inverse is marked pre-existing, so a rollback never removes the server", func() {
				index := slices.IndexFunc(res.RMA, func(op receipt.Op) bool { return op.Kind == receipt.OpHostInstall })
				So(index, ShouldBeGreaterThanOrEqualTo, 0)
				So(res.RMA[index].Existed, ShouldBeTrue)
			})
		})
	})
}

// TestLooseMCPOwnershipOtherPackage pins the receipt half of [R2]: a name
// another package records is hands-off before the host is even asked.
func TestLooseMCPOwnershipOtherPackage(t *testing.T) {
	Convey("Given a github server a receipt of another package records", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		st := openStore(t)

		h, runner := newClaude(t, home, nil,
			host.WithStore(st), host.WithTrash(st.Trash()), host.WithOwnership(ownerMap{mcpGithubPath: "other/pkg"}))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: mcpGatePackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then it is hands-off naming the owner, with no call", func() {
				typed, ok := errors.AsType[*render.HandsOffError](err)
				So(ok, ShouldBeTrue)
				So(typed.Reason, ShouldContainSubstring, "other/pkg")
				So(runner.Calls(), ShouldBeEmpty)
				So(fileExists(filepath.Join(home, ".claude")), ShouldBeFalse)
			})
		})
	})
}

// TestLooseMCPOwnershipProbe pins the host half of [R2]: a probe proving the
// name free lets the add and its inverse through; a probe that cannot tell
// fails the plan closed.
func TestLooseMCPOwnershipProbe(t *testing.T) {
	Convey("Given a host without a github server", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		st := openStore(t)

		h, runner := newClaude(t, home, map[string]hostcli.Response{
			mcpGetGithub: mcpAbsent("github"),
			mcpAddGithub: response(""),
		}, host.WithStore(st), host.WithTrash(st.Trash()))

		Convey("When the package is delivered", func() {
			res, err := h.Deliver(t.Context(), home, host.Delivery{Package: mcpGatePackage(t), Strategy: host.Loose})

			Convey("Then the probe proves the name free, the add runs and the remove is recorded", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{mcpGetGithub, mcpAddGithub})
				So(hasMCPRemove(res.RMA), ShouldBeTrue)
				So(digestOf(t, res, "mcp", "github"), ShouldNotBeEmpty)
			})
		})

		Convey("When the package is only planned", func() {
			res, err := h.Deliver(t.Context(), home, host.Delivery{Package: mcpGatePackage(t), Strategy: host.Loose, DryRun: true})

			Convey("Then only the probe ran and the plan records the remove", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{mcpGetGithub})
				So(hasMCPRemove(res.RMA), ShouldBeTrue)
			})
		})
	})

	for label, refusal := range map[string]string{
		"a corrupt config":                     "error: ~/.claude.json is corrupt",
		"an unrelated 'does not exist' (NF-3)": "error: /home/u/.claude.json does not exist",
	} {
		Convey("Given a probe that fails for another reason: "+label, t, func() {
			fakeClaude(t)
			home := t.TempDir()
			st := openStore(t)

			h, runner := newClaude(t, home, map[string]hostcli.Response{
				mcpGetGithub: {Code: 2, Stderr: refusal},
				mcpAddGithub: response(""),
			}, host.WithStore(st), host.WithTrash(st.Trash()))

			_, err := h.Deliver(t.Context(), home, host.Delivery{Package: mcpGatePackage(t), Strategy: host.Loose})

			Convey("When the package is delivered", func() {
				Convey("Then ownership is unprovable and the plan fails closed", func() {
					typed, ok := errors.AsType[*host.DeliveryError](err)
					So(ok, ShouldBeTrue)
					So(typed.Step, ShouldEqual, "plan")
					So(callKeys(runner), ShouldResemble, []string{mcpGetGithub})
					So(fileExists(filepath.Join(home, ".claude")), ShouldBeFalse)
				})
			})
		})
	}
}

// TestLooseMCPCollisionThroughApply pins [R2] end to end: a user's own server
// sharing a package's MCP name survives an install, a failing install with
// its rollback, and a later removal of the package.
func TestLooseMCPCollisionThroughApply(t *testing.T) {
	Convey("Given a user's own github server and an apply world", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		st := openStore(t)

		deps := applyWorld(t, st, ownerMap{})
		owner := receiptsOwner{receipts: deps.Receipts}
		deps.Owned = owner

		h, runner := newClaude(t, home, map[string]hostcli.Response{
			mcpGetGithub:    mcpPresent("github"),
			mcpAddGithub:    {Code: 1, Stderr: "MCP server github already exists in user config"},
			mcpRemoveGithub: response(""),
		}, host.WithStore(st), host.WithTrash(st.Trash()), host.WithOwnership(owner))
		deps.Hosts[host.Claude] = h

		pkg := mcpGatePackage(t)
		pkg.Version = "2.0.0"

		run := func(action apply.Action) apply.CellResult {
			report, err := apply.Run(t.Context(), deps, apply.Plan{Actions: []apply.Action{action}}, apply.Options{Confirm: refuseConflicts{}})
			So(err, ShouldBeNil)

			return report.Cells[0]
		}

		Convey("When apply installs a package declaring the same name, whose add the host would refuse", func() {
			cell := run(apply.Action{Kind: apply.ActionInstall, Host: host.Claude, Delivery: host.Delivery{Package: pkg, Strategy: host.Loose}})

			_, found, getErr := deps.Receipts.Get(pkg.ID, string(host.Claude), receipt.ScopeUser)

			Convey("Then the cell is hands-off and neither the add nor a rollback remove runs", func() {
				So(cell.Status, ShouldEqual, apply.StatusHandsOff)
				So(callKeys(runner), ShouldNotContain, mcpAddGithub)
				So(callKeys(runner), ShouldNotContain, mcpRemoveGithub)
				So(getErr, ShouldBeNil)
				So(found, ShouldBeFalse)
				So(fileExists(filepath.Join(home, ".claude")), ShouldBeFalse)
			})
		})

		Convey("When an installed package gains the same name in an update and is then removed", func() {
			v1 := withoutMCP(pkg)
			v1.Version = "1.0.0"

			installed := run(apply.Action{Kind: apply.ActionInstall, Host: host.Claude, Delivery: host.Delivery{Package: v1, Strategy: host.Loose}})

			rec, found, getErr := deps.Receipts.Get(pkg.ID, string(host.Claude), receipt.ScopeUser)
			So(getErr, ShouldBeNil)
			So(found, ShouldBeTrue)

			updated := run(apply.Action{Kind: apply.ActionUpdate, Host: host.Claude, Previous: &rec, Delivery: host.Delivery{Package: pkg, Strategy: host.Loose}})
			removed := run(apply.Action{Kind: apply.ActionRemove, Host: host.Claude, Previous: &rec, Cause: "user", Initiator: "claude"})

			Convey("Then the update is hands-off, the removal is clean and the user's server is never touched", func() {
				So(installed.Status, ShouldEqual, apply.StatusCurrent)
				So(updated.Status, ShouldEqual, apply.StatusHandsOff)
				So(removed.Status, ShouldEqual, apply.StatusCurrent)
				So(fileExists(filepath.Join(home, ".claude", "skills", "alpha")), ShouldBeFalse)
				So(callKeys(runner), ShouldNotContain, mcpAddGithub)
				So(callKeys(runner), ShouldNotContain, mcpRemoveGithub)
			})
		})
	})
}

// mcpWorld is one Claude home over the stateful fake CLI with a receipt-backed
// owner, and an apply world sharing the adapter's store.
func mcpWorld(t *testing.T) (apply.Deps, *claudeCLI, string) {
	t.Helper()

	fakeClaude(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	home := t.TempDir()
	st := openStore(t)

	deps := applyWorld(t, st, ownerMap{})
	owner := receiptsOwner{receipts: deps.Receipts}
	deps.Owned = owner

	cli := newClaudeCLI(filepath.Join(home, ".claude"))
	deps.Hosts[host.Claude] = host.NewClaude(host.WithHome(home), host.WithRunner(cli),
		host.WithStore(st), host.WithTrash(st.Trash()), host.WithOwnership(owner))

	return deps, cli, home
}

// mcpVersion is the Claude fixture at a version with plain sse servers
// (name → url).
func mcpVersion(t *testing.T, version string, servers map[string]string) host.Package {
	t.Helper()

	pkg := withoutMCP(claudePackage(t))
	pkg.Version = version

	for _, name := range slices.Sorted(maps.Keys(servers)) {
		pkg.MCP = append(pkg.MCP, manifest.MCPServer{Name: name, Transport: "sse", URL: servers[name]})
	}

	return pkg
}

// applyCell runs one apply action and returns its cell.
func applyCell(t *testing.T, deps apply.Deps, action apply.Action) apply.CellResult {
	t.Helper()

	report, err := apply.Run(t.Context(), deps, apply.Plan{Actions: []apply.Action{action}}, apply.Options{Confirm: refuseConflicts{}})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	return report.Cells[0]
}

// currentReceipt returns the stored receipt of the Claude cell.
func currentReceipt(t *testing.T, deps apply.Deps, pkg string) receipt.Receipt {
	t.Helper()

	rec, found, err := deps.Receipts.Get(pkg, string(host.Claude), receipt.ScopeUser)
	if err != nil || !found {
		t.Fatalf("receipt %s: found=%v err=%v", pkg, found, err)
	}

	return rec
}

// TestLooseMCPOwnUpdate pins NF-2: re-delivering a server this package's
// receipt owns never runs `mcp add` on a configured name — an unchanged
// server is skipped, a changed one is removed and re-added — and a rolled-back
// update leaves the configured server in place.
func TestLooseMCPOwnUpdate(t *testing.T) {
	Convey("Given v1 of a package that configured the github server", t, func() {
		deps, cli, _ := mcpWorld(t)

		v1 := mcpVersion(t, "1.0.0", map[string]string{"github": "https://example.com/v1"})
		So(applyCell(t, deps, apply.Action{
			Kind: apply.ActionInstall, Host: host.Claude,
			Delivery: host.Delivery{Package: v1, Strategy: host.Loose},
		}).Status, ShouldEqual, apply.StatusCurrent)

		rec := currentReceipt(t, deps, v1.ID)
		before := len(cli.Calls())

		update := func(pkg host.Package) apply.CellResult {
			return applyCell(t, deps, apply.Action{
				Kind: apply.ActionUpdate, Host: host.Claude, Previous: &rec,
				Delivery: host.Delivery{Package: pkg, Strategy: host.Loose},
			})
		}

		Convey("When v2 carries the same server", func() {
			cell := update(mcpVersion(t, "2.0.0", map[string]string{"github": "https://example.com/v1"}))

			Convey("Then the update is current, the server untouched and the receipt on v2", func() {
				So(cell.Status, ShouldEqual, apply.StatusCurrent)

				argv, configured := cli.Server("github")
				So(configured, ShouldBeTrue)
				So(argv, ShouldEndWith, "https://example.com/v1")
				So(cli.Calls()[before:], ShouldNotContain, "claude "+strings.Join(render.ClaudeMCPRemoveArgs("github"), " "))
				So(currentReceipt(t, deps, v1.ID).Version, ShouldEqual, "2.0.0")
			})

			Convey("Then a later removal still removes the server verger created", func() {
				removed := applyCell(t, deps, apply.Action{
					Kind: apply.ActionRemove, Host: host.Claude,
					Previous: new(currentReceipt(t, deps, v1.ID)), Cause: "user", Initiator: "claude",
				})

				So(removed.Status, ShouldEqual, apply.StatusCurrent)

				_, configured := cli.Server("github")
				So(configured, ShouldBeFalse)
			})
		})

		Convey("When v2 changes the server", func() {
			cell := update(mcpVersion(t, "2.0.0", map[string]string{"github": "https://example.com/v2"}))

			Convey("Then it is removed and re-added with the new config, and the update is current", func() {
				So(cell.Status, ShouldEqual, apply.StatusCurrent)

				argv, configured := cli.Server("github")
				So(configured, ShouldBeTrue)
				So(argv, ShouldEndWith, "https://example.com/v2")
				So(currentReceipt(t, deps, v1.ID).Version, ShouldEqual, "2.0.0")
			})
		})

		Convey("When v2 changes the server and adds one the host refuses", func() {
			v2 := mcpVersion(t, "2.0.0", map[string]string{"github": "https://example.com/v2", "zeta": "https://example.com/z"})
			cli.fail["mcp add --scope user --transport sse zeta https://example.com/z"] = hostcli.Response{Code: 1, Stderr: "add failed"}

			cell := update(v2)

			Convey("Then the update fails but the rollback keeps the configured github server and v1's receipt", func() {
				So(cell.Status, ShouldEqual, apply.StatusFailed)

				_, configured := cli.Server("github")
				So(configured, ShouldBeTrue)

				_, zeta := cli.Server("zeta")
				So(zeta, ShouldBeFalse)
				So(currentReceipt(t, deps, v1.ID).Version, ShouldEqual, "1.0.0")
				So(strings.Join(cell.Notes, "\n"), ShouldNotContainSubstring, "rollback:")
			})
		})
	})
}

// TestLooseFailureReturnsExecutedRMA pins the NF-5 class on the loose
// surface: a step failing after an owned file was replaced returns the error
// together with the executed RMA, whose op carries the replaced file's bucket.
func TestLooseFailureReturnsExecutedRMA(t *testing.T) {
	Convey("Given an owned command file and a server whose add the host refuses", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		st := openStore(t)

		target := filepath.Join(home, ".claude", "commands", "dev.md")
		writeFixtureFile(t, target, "previous command\n", 0o600)

		pkg := withoutMCP(claudePackage(t))
		pkg.MCP = []manifest.MCPServer{{Name: "github", Transport: "sse", URL: "https://example.com/mcp"}}

		h, _ := newClaude(t, home, map[string]hostcli.Response{
			mcpGetGithub: mcpAbsent("github"),
			mcpAddGithub: {Code: 1, Stderr: "add failed"},
		}, host.WithStore(st), host.WithTrash(st.Trash()), host.WithOwnership(ownerMap{target: pkg.ID}))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

		Convey("When the delivery fails at the MCP add", func() {
			Convey("Then the error comes with the RMA execution produced", func() {
				So(err, ShouldNotBeNil)

				op := rmaOp(res, target)
				So(op.Existed, ShouldBeTrue)
				So(op.Backup, ShouldNotBeEmpty)
				So(trashedValue(t, st, op.Backup), ShouldEqual, "previous command\n")
			})
		})
	})
}

// TestLooseMCPFailedAddRollback pins the host half of NF-3: the rollback of a
// failed first `mcp add` meets `mcp remove` of a server that was never added;
// the adapter treats the unknown server as already removed, so the rollback
// cleans every other artifact of the delivery.
func TestLooseMCPFailedAddRollback(t *testing.T) {
	Convey("Given a free server whose add the host refuses", t, func() {
		deps, cli, home := mcpWorld(t)

		pkg := mcpVersion(t, "1.0.0", map[string]string{"fresh": "https://example.com/fresh"})
		cli.fail["mcp add --scope user --transport sse fresh https://example.com/fresh"] = hostcli.Response{Code: 1, Stderr: "add failed"}

		cell := applyCell(t, deps, apply.Action{
			Kind: apply.ActionInstall, Host: host.Claude,
			Delivery: host.Delivery{Package: pkg, Strategy: host.Loose},
		})

		Convey("When apply installs it", func() {
			Convey("Then the cell fails and the rollback leaves no file of the delivery", func() {
				So(cell.Status, ShouldEqual, apply.StatusFailed)
				So(homeFiles(t, home), ShouldBeEmpty)
				So(cli.Calls(), ShouldContain, "claude mcp remove --scope user fresh")
				So(strings.Join(cell.Notes, "\n"), ShouldNotContainSubstring, "rollback:")
			})
		})
	})
}

// TestClaudeUninstallUnknownMCP pins NF-3 at the adapter: `mcp remove` of a
// server the host does not know is done, not an error; a server that existed
// before the delivery is never removed by its inverse.
func TestClaudeUninstallUnknownMCP(t *testing.T) {
	Convey("Given a Claude host without a fresh server", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		cli := newClaudeCLI(filepath.Join(home, ".claude"))
		cli.servers["kept"] = "claude mcp add kept"

		h := host.NewClaude(host.WithHome(home), host.WithRunner(cli))

		uninstall := func(op receipt.Op) (host.Result, error) {
			return h.Uninstall(t.Context(), home, receipt.Receipt{
				Package: "acme/caveman", Host: "claude", Scope: receipt.ScopeUser, Strategy: string(host.Loose), RMA: []receipt.Op{op},
			})
		}

		Convey("When the inverse removes fresh", func() {
			res, err := uninstall(receipt.Op{Kind: receipt.OpHostInstall, Command: render.ClaudeMCPRemoveArgs("fresh")})

			Convey("Then it is already done, with a note", func() {
				So(err, ShouldBeNil)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "fresh")
			})
		})

		Convey("When the inverse of a server that existed before the delivery runs", func() {
			_, err := uninstall(receipt.Op{Kind: receipt.OpHostInstall, Command: render.ClaudeMCPRemoveArgs("kept"), Existed: true})

			Convey("Then the server is kept", func() {
				So(err, ShouldBeNil)

				_, configured := cli.Server("kept")
				So(configured, ShouldBeTrue)
			})
		})
	})
}

// TestLooseHookSecretRefused pins decision Q3 (T1.6-review-1 [R7]): a hook
// command carrying `{secret:NAME}` is refused with a typed error (cell
// silenced(secret-in-hook)) whenever the hooks would be delivered; nothing is
// written and no value leaks.
func TestLooseHookSecretRefused(t *testing.T) {
	Convey("Given a hook command carrying a secret reference", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		writeSettings(t, home, `{"model": "opus"}`)

		settings := filepath.Join(home, ".claude", "settings.json")
		st := openStore(t)
		secrets := secretStore(t, map[string]string{"HOOK_TOKEN": "hook-s3cr3t"})

		h, runner := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		pkg := withoutMCP(claudePackage(t))
		pkg.Hooks = []manifest.Hook{{Event: manifest.EventPreTool, Matcher: "Bash", Command: "guard --token {secret:HOOK_TOKEN}"}}

		for _, dryRun := range []bool{false, true} {
			Convey(fmt.Sprintf("When hooks are allowed (dry-run %v)", dryRun), func() {
				res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true, DryRun: dryRun})

				Convey("Then the delivery is refused as secret-in-hook without the value", func() {
					typed, ok := errors.AsType[*host.SecretInHookError](err)
					So(ok, ShouldBeTrue)
					So(typed.Host, ShouldEqual, host.Claude)
					So(typed.Names, ShouldResemble, []string{"HOOK_TOKEN"})
					So(typed.Reason(), ShouldEqual, "secret-in-hook")
					So(errors.Is(err, host.ErrNotSupported), ShouldBeTrue)
					So(err.Error(), ShouldNotContainSubstring, "hook-s3cr3t")
				})

				Convey("Then nothing is run or written", func() {
					So(res.RMA, ShouldBeEmpty)
					So(runner.Calls(), ShouldBeEmpty)
					So(readTestFile(t, settings), ShouldEqual, `{"model": "opus"}`)
					So(fileExists(filepath.Join(home, ".claude", "skills")), ShouldBeFalse)
				})
			})
		}

		Convey("When the hooks wait for consent", func() {
			res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

			Convey("Then the rest is delivered and the hook is skipped with the consent note", func() {
				So(err, ShouldBeNil)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "consent")
				So(readTestFile(t, settings), ShouldEqual, `{"model": "opus"}`)
			})
		})
	})

	Convey("Given the same hook under allowManagedHooksOnly", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		writeSettings(t, home, `{"allowManagedHooksOnly": true}`)

		st := openStore(t)
		h, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		pkg := withoutMCP(claudePackage(t))
		pkg.Hooks = []manifest.Hook{{Event: manifest.EventPreTool, Matcher: "Bash", Command: "guard --token {secret:HOOK_TOKEN}"}}

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

		Convey("When hooks are allowed by consent but not by the policy", func() {
			Convey("Then the hook is never delivered, so nothing is refused", func() {
				So(err, ShouldBeNil)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "allowManagedHooksOnly")
			})
		})
	})
}

// TestLooseForeignHooksKeyAdopted pins the Ф1 contract of decision Q2
// (T1.6-review-1 [R1]): a pre-existing hooks key verger never wrote is adopted,
// not hands-off — the user's handlers survive the merge, the previous value is
// trashed, and a removal restores it. Receipt-digest hands-off for this key is
// T2.1 (NEW-4).
func TestLooseForeignHooksKeyAdopted(t *testing.T) {
	Convey("Given settings.json with a user's own PreToolUse handler", t, func() {
		fakeClaude(t)
		home := t.TempDir()

		const foreignHooks = `{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"user-guard.sh"}]}]}`

		writeSettings(t, home, `{"model":"opus","hooks":`+foreignHooks+`}`)

		settings := filepath.Join(home, ".claude", "settings.json")
		st := openStore(t)

		h, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))
		pkg := withoutMCP(claudePackage(t))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})
		So(err, ShouldBeNil)

		Convey("When the package delivers its hooks", func() {
			op := rmaOp(res, settings)

			Convey("Then the key is adopted and both handlers are present", func() {
				body := readTestFile(t, settings)
				So(body, ShouldContainSubstring, "user-guard.sh")
				So(body, ShouldContainSubstring, pkg.Root+"/hooks/guard.js")
			})

			Convey("Then the hooks op records the replaced value in the trash", func() {
				So(op.KeyPath, ShouldEqual, "hooks")
				So(op.Existed, ShouldBeTrue)
				So(op.Backup, ShouldNotBeEmpty)
				So(trashedValue(t, st, op.Backup), ShouldEqualJSON, foreignHooks)
			})
		})

		Convey("When apply later removes the cell", func() {
			rec := receipt.Receipt{
				Schema: receipt.Schema, Package: pkg.ID, Host: string(host.Claude), Scope: receipt.ScopeUser,
				Strategy: string(host.Loose), Version: pkg.Version, Artifacts: res.Artifacts, RMA: res.RMA,
			}

			deps := applyWorld(t, st, ownerMap{})
			deps.Hosts[host.Claude] = h

			report, runErr := apply.Run(t.Context(), deps, apply.Plan{Actions: []apply.Action{{
				Kind: apply.ActionRemove, Host: host.Claude, Previous: &rec, Cause: "user", Initiator: "claude",
			}}}, apply.Options{})

			Convey("Then the user's hooks value is restored exactly", func() {
				So(runErr, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, apply.StatusCurrent)
				So(readTestFile(t, settings), ShouldEqualJSON, `{"model":"opus","hooks":`+foreignHooks+`}`)
			})
		})
	})
}

// TestLooseRuleRedelivery pins the rule-skill ownership proof: the receipt
// records the rendered SKILL.md, so a re-delivery whose owner resolves that
// file is not a collision with its own dir, while a foreign dir still is.
func TestLooseRuleRedelivery(t *testing.T) {
	Convey("Given a delivered rule skill", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		st := openStore(t)

		pkg := withoutMCP(claudePackage(t))
		file := filepath.Join(home, ".claude", "skills", "rule-caveman", "SKILL.md")

		first, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))
		_, err := first.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})
		So(err, ShouldBeNil)

		owned := ownerMap{}
		for _, path := range homeFiles(t, home) {
			owned[filepath.Join(home, filepath.FromSlash(path))] = pkg.ID
		}

		for _, dir := range []string{"alpha", "beta"} {
			owned[filepath.Join(home, ".claude", "skills", dir)] = pkg.ID
		}

		Convey("When the receipt records only the rendered SKILL.md and it is delivered again", func() {
			So(owned[file], ShouldEqual, pkg.ID)

			again, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithOwnership(owned))
			_, againErr := again.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

			Convey("Then the re-delivery is not a collision", func() {
				So(againErr, ShouldBeNil)
			})
		})

		Convey("When another package's receipt records the SKILL.md", func() {
			owned[file] = "other/pkg"

			again, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithOwnership(owned))
			_, againErr := again.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

			Convey("Then the rule dir is still a collision", func() {
				_, ok := errors.AsType[*host.CollisionError](againErr)
				So(ok, ShouldBeTrue)
			})
		})
	})
}

// TestLooseComponentNamesValidated pins T1.6-review-1 [R6]: every component
// name is a single safe path element before any target path is built.
func TestLooseComponentNamesValidated(t *testing.T) {
	for _, component := range []manifest.Component{
		{Kind: manifest.KindAgent, Name: "../../escape", Path: "agents/reviewer.md"},
		{Kind: manifest.KindCommand, Name: "../../escape", Path: "commands/dev.md"},
	} {
		Convey("Given a "+string(component.Kind)+" named "+component.Name, t, func() {
			fakeClaude(t)
			home := t.TempDir()

			pkg := withoutMCP(claudePackage(t))
			pkg.Components = []manifest.Component{component}

			h, _ := newClaude(t, home, nil)

			_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

			Convey("When it is delivered", func() {
				Convey("Then the plan refuses it and nothing lands outside the host dir", func() {
					typed, ok := errors.AsType[*host.DeliveryError](err)
					So(ok, ShouldBeTrue)
					So(typed.Step, ShouldEqual, "plan")
					So(fileExists(filepath.Join(home, "escape.md")), ShouldBeFalse)
					So(fileExists(filepath.Join(home, ".claude")), ShouldBeFalse)
				})
			})
		})
	}
}
