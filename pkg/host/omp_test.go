package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
	"github.com/odiumuniverse/verger/pkg/secret"
)

const ompFixtureRoot = "testdata/omp/acme"

// ruleAlwaysGolden is the exact omp rulebook document of the fixture rule with
// a globs/alwaysApply frontmatter: the documented keys keep their order, the
// unknown key survives behind them, and the sequence renders in flow style —
// valid YAML, so omp never falls back to its flat line parser.
const ruleAlwaysGolden = `---
description: Always-on house style.
globs: ['**/*.go']
alwaysApply: true
customField: kept
---
Tabs never reach the tree.
`

// fakeOmp puts an executable `omp` shim at the front of PATH.
func fakeOmp(t *testing.T) {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "omp"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // G306: a fake host CLI must be executable
		t.Fatalf("write shim: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// clearOmpEnv removes the env relocations of the omp agent dir, so a test that
// does not exercise them sees the default home.
func clearOmpEnv(t *testing.T) {
	t.Helper()

	for _, name := range []string{"PI_CODING_AGENT_DIR", "PI_CONFIG_DIR", "OMP_PROFILE", "PI_PROFILE"} {
		t.Setenv(name, "")
	}
}

// newOmp builds the omp adapter with a scripted runner.
func newOmp(t *testing.T, home string, script map[string]hostcli.Response, opts ...host.Option) (host.Host, *hostcli.ScriptRunner) {
	t.Helper()

	runner := hostcli.NewScriptRunner(script)
	all := append([]host.Option{host.WithHome(home), host.WithRunner(runner)}, opts...)

	return host.NewOmp(all...), runner
}

// ompWorld is one omp home over the stateful fake CLI, with the store, trash
// and the secret store the fixture's MCP servers need.
func ompWorld(t *testing.T, opts ...host.Option) (host.Host, *ompCLI, string) {
	t.Helper()

	fakeOmp(t)
	clearOmpEnv(t)

	home := t.TempDir()
	st := openStore(t)
	cli := newOmpCLI()
	all := append([]host.Option{
		host.WithHome(home), host.WithRunner(cli),
		host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)),
	}, opts...)

	return host.NewOmp(all...), cli, home
}

// ompSecrets is the secret store of the omp fixture: its MCP servers reference
// MCP_TOKEN and WEB_TOKEN.
func ompSecrets(t *testing.T) *secret.Store {
	t.Helper()

	return secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token", "WEB_TOKEN": "web-token"})
}

// unsetEnv removes one variable for the duration of a test: `t.Setenv` can only
// set a value, and the host distinguishes "unset" from "set but empty" for
// OMP_PROFILE.
func unsetEnv(t *testing.T, name string) {
	t.Helper()

	previous, had := os.LookupEnv(name)

	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unset %s: %v", name, err)
	}

	t.Cleanup(func() {
		if had {
			//nolint:usetesting // restoring a value the test removed, not a variable the test owns
			_ = os.Setenv(name, previous)
		}
	})
}

// newCursorlessOmp builds the omp adapter with the store and secrets the
// fixture needs, for tests that do not care about the CLI.
func newCursorlessOmp(t *testing.T, home string) (host.Host, *hostcli.ScriptRunner) {
	t.Helper()

	st := openStore(t)

	return newOmp(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)))
}

// ompPackage parses the omp fixture and adds the rule component the manifest
// format does not carry.
func ompPackage(t *testing.T) host.Package {
	t.Helper()

	pkg := adapterFixturePackage(t, ompFixtureRoot, manifest.FormatClaude)

	pkg.Components = append(pkg.Components, manifest.Component{
		Kind: manifest.KindRule, Name: "always", Path: "rules/always.md",
		Digest: digestFile(t, filepath.Join(ompFixtureRoot, "rules", "always.md")),
	})

	return pkg
}

// ompRuleComponent is one rule component over the fixture's caveman.md.
func ompRuleComponent(t *testing.T, name string) manifest.Component {
	t.Helper()

	return manifest.Component{
		Kind: manifest.KindRule, Name: name, Path: "rules/caveman.md",
		Digest: digestFile(t, filepath.Join(ompFixtureRoot, "rules", "caveman.md")),
	}
}

// ompHomeAgent is the agent dir of one test home.
func ompHomeAgent(home string) string {
	return filepath.Join(home, ".omp", "agent")
}

// ompMCPPath is the MCP document of one test home.
func ompMCPPath(home string) string {
	return filepath.Join(ompHomeAgent(home), "mcp.json")
}

// ompSharedSkills is the shared skills root of one test home.
func ompSharedSkills(home string) string {
	return filepath.Join(home, ".agents", "skills")
}

func TestOmpDetect(t *testing.T) {
	Convey("Given a home without an omp agent dir", t, func() {
		home := t.TempDir()
		t.Setenv("PATH", t.TempDir())
		clearOmpEnv(t)

		h, _ := newOmp(t, home, nil)

		Convey("When neither the agent dir, the install marker nor the binary exist", func() {
			Convey("Then Detect is false", func() {
				So(h.Detect(home), ShouldBeFalse)
			})
		})

		Convey("When the default agent dir exists", func() {
			if err := os.MkdirAll(ompHomeAgent(home), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})

		Convey("When only the config-independent install marker exists", func() {
			writeFixtureFile(t, filepath.Join(home, ".omp", "install-id"), "6f2a", 0o600)

			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})

	Convey("Given PI_CODING_AGENT_DIR relocates the agent dir", t, func() {
		home := t.TempDir()
		t.Setenv("PATH", t.TempDir())
		clearOmpEnv(t)

		profile := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.MkdirAll(profile, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		t.Setenv("PI_CODING_AGENT_DIR", profile)

		st := openStore(t)
		h, _ := newOmp(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)))

		Convey("When the relocated dir exists and the home has no .omp", func() {
			Convey("Then Detect is true and the loose surface follows the relocation", func() {
				So(h.Detect(home), ShouldBeTrue)

				_, err := h.Deliver(t.Context(), home, host.Delivery{Package: ompPackage(t), Strategy: host.Loose})
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(profile, "agents", "reviewer.md")), ShouldBeTrue)
				So(fileExists(filepath.Join(profile, "rules", "caveman.md")), ShouldBeTrue)
				// The relocated agent dir is not created below the home; the
				// shared lock still lives at the plugin state root (its own
				// convention, shared with beadle).
				So(fileExists(filepath.Join(home, ".omp", "agent")), ShouldBeFalse)
			})
		})
	})

	Convey("Given a named profile", t, func() {
		home := t.TempDir()
		t.Setenv("PATH", t.TempDir())
		clearOmpEnv(t)
		t.Setenv("OMP_PROFILE", "work")

		profile := filepath.Join(home, ".omp", "profiles", "work", "agent")

		if err := os.MkdirAll(profile, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		h, _ := newOmp(t, home, nil)

		Convey("When the profile agent dir exists", func() {
			Convey("Then Detect is true and the profile wins over the default dir", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})

	Convey("Given PI_CONFIG_DIR relocates the base root", t, func() {
		home := t.TempDir()
		t.Setenv("PATH", t.TempDir())
		clearOmpEnv(t)
		t.Setenv("PI_CONFIG_DIR", "cfg/omp")

		if err := os.MkdirAll(filepath.Join(home, "cfg", "omp", "agent"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		h, _ := newOmp(t, home, nil)

		Convey("When the relocated base root exists below the home", func() {
			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})

	Convey("Given the omp binary on PATH", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		h, _ := newOmp(t, home, nil)

		Convey("When only the binary exists", func() {
			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})
}

func TestOmpLooseGolden(t *testing.T) {
	Convey("Given an omp package and a temp home", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		st := openStore(t)
		pkg := ompPackage(t)

		h, runner := newOmp(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered loose", func() {
			Convey("Then skills go to the shared ~/.agents root and everything else below the agent dir", func() {
				So(err, ShouldBeNil)
				So(homeFiles(t, home), ShouldResemble, []string{
					".agents/skills/alpha/SKILL.md",
					".agents/skills/alpha/scripts/run.sh",
					".agents/skills/beta/SKILL.md",
					// The shared lock, not a delivered artifact: the MCP document
					// is edited under it (see TestOmpMCPSharedLock).
					".omp/.omp-plugin.verger.lock",
					".omp/agent/agents/reviewer.md",
					".omp/agent/commands/dev.md",
					".omp/agent/mcp.json",
					".omp/agent/rules/always.md",
					".omp/agent/rules/caveman.md",
				})
				So(runner.Calls(), ShouldBeEmpty)
			})

			Convey("Then the rules land unwrapped in the native rulebook dir, not as skills", func() {
				So(readTestFile(t, filepath.Join(ompHomeAgent(home), "rules", "always.md")), ShouldEqual, ruleAlwaysGolden)
				So(fileExists(filepath.Join(ompSharedSkills(home), "rule-caveman")), ShouldBeFalse)
			})

			Convey("Then the MCP servers land in the agent dir mcp.json with the secret resolved", func() {
				So(readTestFile(t, ompMCPPath(home)), ShouldEqualJSON, `{
  "mcpServers": {
    "fs": {"type": "stdio", "command": "node", "args": ["`+pkg.Root+`/server.js"], "env": {"TOKEN": "s3cr3t-token"}},
    "web": {"type": "http", "url": "https://example.com/mcp", "headers": {"Authorization": "Bearer web-token"}}
  }
}`)
				assertPathMode(t, ompMCPPath(home), 0o600)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "0600")
			})

			Convey("Then the shared skills root and its shadowing rule are named", func() {
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "which wins on a name clash")
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, filepath.Join(".omp", "agent", "skills"))
			})

			Convey("Then the result carries no secret value", func() {
				rendered, marshalErr := json.Marshal(res)
				So(marshalErr, ShouldBeNil)
				So(string(rendered), ShouldNotContainSubstring, "s3cr3t-token")
			})

			Convey("Then the RMA mirrors every artifact in install order", func() {
				assertArtifactsBackedByOps(t, res)

				ops := map[string]receipt.Op{}
				for _, op := range res.RMA {
					ops[op.Path] = op
				}

				So(ops[filepath.Join(ompSharedSkills(home), "alpha")].Kind, ShouldEqual, receipt.OpCopyTree)
				So(ops[filepath.Join(ompHomeAgent(home), "agents", "reviewer.md")].Kind, ShouldEqual, receipt.OpWriteFile)
				So(ops[filepath.Join(ompHomeAgent(home), "commands", "dev.md")].Kind, ShouldEqual, receipt.OpWriteFile)
				So(ops[filepath.Join(ompHomeAgent(home), "rules", "always.md")].Kind, ShouldEqual, receipt.OpWriteFile)
				So(ops[ompMCPPath(home)].Kind, ShouldEqual, receipt.OpConfigKey)
			})
		})
	})
}

// TestOmpRulesNative pins the native rulebook decision: the source frontmatter
// survives, a description is guaranteed (omp silently drops a rule with no
// condition, no alwaysApply and no description), and the rule is never wrapped
// as a skill.
func TestOmpRulesNative(t *testing.T) {
	Convey("Given rules with and without frontmatter", t, func() {
		h, _, home := ompWorld(t)
		pkg := ompPackage(t)

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})
		So(err, ShouldBeNil)

		Convey("When they are delivered", func() {
			Convey("Then the bare rule gains a description from its first line", func() {
				So(readTestFile(t, filepath.Join(ompHomeAgent(home), "rules", "caveman.md")), ShouldEqual, `---
description: Speak like caveman.
---
Speak like caveman.
`)
			})

			Convey("Then no rule is delivered as a skill wrapper", func() {
				So(homeFiles(t, home), ShouldNotContain, ".agents/skills/rule-caveman/SKILL.md")
			})
		})
	})

	Convey("Given a rule whose name would be an unsafe path element", t, func() {
		h, _, home := ompWorld(t)

		rule := ompRuleComponent(t, "../escape")
		pkg := ompPackage(t)
		pkg.Components = append(pkg.Components, rule)

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then the plan refuses it and writes nothing", func() {
				So(err, ShouldNotBeNil)
				So(fileExists(filepath.Join(home, ".agents")), ShouldBeFalse)
			})
		})
	})
}

// TestOmpHooksSilenced pins the hook component: omp has no declarative hook
// document at all, so consent does not matter and the cell carries the reason
// as a delivery note.
func TestOmpHooksSilenced(t *testing.T) {
	Convey("Given a package carrying a hook", t, func() {
		h, _, home := ompWorld(t)

		pkg := ompPackage(t)
		pkg.Hooks = []manifest.Hook{{Event: "PreToolUse", Matcher: "Bash", Command: "node guard.js", Timeout: 5}}

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered with hooks allowed", func() {
			Convey("Then no hook document is written and the note names the reason", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(ompHomeAgent(home), "hooks")), ShouldBeFalse)
				So(homeFiles(t, home), ShouldNotContain, ".omp/agent/hooks.json")
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "hooks/{pre,post}/")
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "1 hook(s) skipped")
			})
		})
	})
}

// TestOmpAgentFrontmatterGuard pins the two fields omp requires: a subagent
// whose frontmatter carries neither a name nor a description is skipped
// silently, so the renderer must fill both in.
func TestOmpAgentFrontmatterGuard(t *testing.T) {
	Convey("Given a subagent source with no frontmatter", t, func() {
		h, _, home := ompWorld(t)

		pkg := ompPackage(t)
		pkg.Root = copyFixtureTree(t, ompFixtureRoot)

		body := "Review the diff.\n"
		writeFixtureFile(t, filepath.Join(pkg.Root, "agents", "bare.md"), body, 0o600)

		pkg.Components = append(pkg.Components, manifest.Component{
			Kind: manifest.KindAgent, Name: "bare", Path: "agents/bare.md", Digest: digest.Bytes([]byte(body)),
		})

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then the rendered file carries a name and a description", func() {
				So(err, ShouldBeNil)

				rendered := readTestFile(t, filepath.Join(ompHomeAgent(home), "agents", "bare.md"))
				So(rendered, ShouldContainSubstring, "name: bare")
				So(rendered, ShouldContainSubstring, "description: Review the diff.")
				So(rendered, ShouldContainSubstring, "Review the diff.")
			})
		})
	})
}

func TestOmpLooseCollisions(t *testing.T) {
	Convey("Given an omp home with a foreign rule file", t, func() {
		h, _, home := ompWorld(t)

		foreign := filepath.Join(ompHomeAgent(home), "rules", "caveman.md")
		writeFixtureFile(t, foreign, "someone else's rule\n", 0o600)

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: ompPackage(t), Strategy: host.Loose})

		Convey("When the same rule is delivered", func() {
			Convey("Then the cell is a collision and the foreign file is untouched", func() {
				collision, ok := errors.AsType[*host.CollisionError](err)
				So(ok, ShouldBeTrue)
				So(collision.Path, ShouldEqual, foreign)
				So(readTestFile(t, foreign), ShouldEqual, "someone else's rule\n")
			})
		})
	})

	Convey("Given a rule file this package's receipt records", t, func() {
		home := t.TempDir()
		clearOmpEnv(t)

		cli := newOmpCLI()
		target := filepath.Join(ompHomeAgent(home), "rules", "caveman.md")
		writeFixtureFile(t, target, "old body\n", 0o600)

		st := openStore(t)
		h := host.NewOmp(
			host.WithHome(home), host.WithRunner(cli),
			host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)),
			host.WithOwnership(ownerMap{target: "acme/caveman"}),
		)

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: ompPackage(t), Strategy: host.Loose})

		Convey("When the rule is delivered again", func() {
			Convey("Then the owned file is replaced", func() {
				So(err, ShouldBeNil)
				So(readTestFile(t, target), ShouldEqual, `---
description: Speak like caveman.
---
Speak like caveman.
`)
			})
		})
	})
}

func TestOmpLooseSecrets(t *testing.T) {
	Convey("Given a package whose MCP server references a stored secret", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		st := openStore(t)

		h, _ := newOmp(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: ompPackage(t), Strategy: host.Loose})

		Convey("When the secret is stored", func() {
			Convey("Then it is resolved into the document and nothing else leaks", func() {
				So(err, ShouldBeNil)
				So(readTestFile(t, ompMCPPath(home)), ShouldContainSubstring, "web-token")
				assertPathMode(t, ompMCPPath(home), 0o600)
			})
		})
	})

	Convey("Given a package whose secret is not stored", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		st := openStore(t)

		h, _ := newOmp(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: ompPackage(t), Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then the plan refuses with the names and writes nothing", func() {
				missing, ok := errors.AsType[*host.MissingSecretsError](err)
				So(ok, ShouldBeTrue)
				So(missing.Names, ShouldResemble, []string{"MCP_TOKEN", "WEB_TOKEN"})
				So(fileExists(filepath.Join(ompHomeAgent(home), "mcp.json")), ShouldBeFalse)
			})
		})
	})
}

func TestOmpLooseMCPHandsOff(t *testing.T) {
	Convey("Given a foreign mcp.json entry of the same name", t, func() {
		h, _, home := ompWorld(t)

		writeFixtureFile(t, ompMCPPath(home), `{"mcpServers": {"fs": {"command": "someone-elses"}}}`, 0o600)

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: ompPackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then the differing key is hands-off and nothing is written", func() {
				handsOff, ok := errors.AsType[*render.HandsOffError](err)
				So(ok, ShouldBeTrue)
				So(handsOff.KeyPath, ShouldEqual, "mcpServers.fs")
				So(readTestFile(t, ompMCPPath(home)), ShouldContainSubstring, "someone-elses")
				So(fileExists(filepath.Join(ompHomeAgent(home), "rules", "caveman.md")), ShouldBeFalse)
			})
		})
	})
}

func TestOmpLooseJSONC(t *testing.T) {
	Convey("Given a foreign mcp.json with comments and unrelated keys", t, func() {
		h, _, home := ompWorld(t)

		writeFixtureFile(t, ompMCPPath(home), `{
  // the user's own comment
  "$schema": "https://omp.sh/mcp.schema.json",
  "disabledServers": ["legacy"],
  "mcpServers": {}
}`, 0o600)
		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: ompPackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then foreign keys and comments survive the edit", func() {
				So(err, ShouldBeNil)

				doc := readTestFile(t, ompMCPPath(home))
				So(doc, ShouldContainSubstring, "the user's own comment")
				So(doc, ShouldContainSubstring, "disabledServers")
				So(doc, ShouldContainSubstring, "mcpServers")
			})
		})
	})
}

func TestOmpLooseDryRun(t *testing.T) {
	Convey("Given an omp package", t, func() {
		h, _, home := ompWorld(t)

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: ompPackage(t), Strategy: host.Loose, DryRun: true})

		Convey("When it is only planned", func() {
			Convey("Then nothing is written and the full RMA is reported", func() {
				So(err, ShouldBeNil)
				So(homeFiles(t, home), ShouldBeEmpty)
				So(res.RMA, ShouldNotBeEmpty)
				assertArtifactsBackedByOps(t, res)
				So(res.Notes, ShouldContain, "dry-run")
			})
		})
	})
}

func TestOmpLooseUnicode(t *testing.T) {
	Convey("Given a skill whose tree carries UTF-8 names and content", t, func() {
		h, _, home := ompWorld(t)

		pkg := ompPackage(t)
		pkg.Root = copyFixtureTree(t, ompFixtureRoot)

		dir := filepath.Join(pkg.Root, "skills", "unicodé")

		writeFixtureFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: unicodé\ndescription: Ünïcode skill.\n---\n\nТело навыка.\n", 0o600)
		writeFixtureFile(t, filepath.Join(dir, "файл.txt"), "данные\n", 0o600)

		sum, sumErr := digest.Tree(dir)
		So(sumErr, ShouldBeNil)

		pkg.Components = append(pkg.Components, manifest.Component{
			Kind: manifest.KindSkill, Name: "unicodé", Path: "skills/unicodé", Digest: sum,
		})

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then the tree lands byte for byte", func() {
				So(err, ShouldBeNil)
				So(readTestFile(t, filepath.Join(ompSharedSkills(home), "unicodé", "файл.txt")), ShouldEqual, "данные\n")
			})
		})
	})
}

// copyFixtureTree copies a fixture package into a temp dir, so a test that
// adds components writes nothing into testdata.
func copyFixtureTree(t *testing.T, root string) string {
	t.Helper()

	dst := filepath.Join(t.TempDir(), filepath.Base(root))

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}

		target := filepath.Join(dst, rel)

		switch {
		case entry.IsDir():
			return os.MkdirAll(target, 0o700)
		case entry.Type().IsRegular():
			data, readErr := os.ReadFile(path) //nolint:gosec // G304: the fixture below testdata
			if readErr != nil {
				return readErr
			}

			//nolint:gosec // G703: the destination is below the test's own temp dir
			return os.WriteFile(target, data, 0o600)
		default:
			return nil
		}
	})
	if err != nil {
		t.Fatalf("copy fixture %s: %v", root, err)
	}

	return dst
}

func TestOmpConcurrentDryRun(t *testing.T) {
	Convey("Given the omp adapter", t, func() {
		h, _, home := ompWorld(t)

		assertConcurrentDryRun(t, h, home, filepath.Join(ompHomeAgent(home), "rules", "caveman.md"), func() host.Package {
			return ompPackage(t)
		})
	})
}

// ompMarketplace writes one local marketplace the fake CLI can register, with
// the catalog name and plugin manifest omp reads.
func ompMarketplace(t *testing.T, name, plugin, version string) string {
	t.Helper()

	root := filepath.Join(t.TempDir(), "marketplace")
	writeFixtureFile(t, filepath.Join(root, ".claude-plugin", "marketplace.json"), `{
  "name": "`+name+`",
  "owner": {"name": "`+name+`"},
  "plugins": [{"name": "`+plugin+`", "source": "./`+plugin+`/`+version+`"}]
}`, 0o600)
	writeFixtureFile(t, filepath.Join(root, plugin, version, ".claude-plugin", "plugin.json"),
		`{"name":"`+plugin+`","version":"`+version+`"}`, 0o600)

	return root
}

func TestOmpNativeDeliver(t *testing.T) {
	Convey("Given a native package whose ref is a local marketplace", t, func() {
		h, cli, home := ompWorld(t)

		// The catalog declares a name the ref's last segment does not carry:
		// omp ignores --json on `marketplace add`, so the name is read back.
		ref := ompMarketplace(t, "acme-tools", "caveman", "1.2.3")

		pkg := ompPackage(t)
		pkg.Marketplace = ref

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered", func() {
			Convey("Then the add, the forced install and the verify run in one locked pass", func() {
				So(err, ShouldBeNil)
				So(cli.Calls(), ShouldResemble, []string{
					"omp plugin marketplace list",
					"omp plugin marketplace add " + ref,
					"omp plugin marketplace list",
					"omp plugin install caveman@acme-tools --force",
					"omp plugin list --json",
				})
				So(res.Observed.Verified, ShouldBeTrue)
			})

			Convey("Then the RMA carries the inverse argv of the name the host registered", func() {
				So(res.RMA, ShouldResemble, []receipt.Op{
					{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "remove", "acme-tools"}},
					{Kind: receipt.OpHostInstall, Command: []string{"plugin", "uninstall", "caveman@acme-tools"}},
				})
			})

			Convey("Then the marketplace artifact proves the name to the next delivery", func() {
				So(res.Artifacts, ShouldResemble, []receipt.Artifact{{
					Kind: "marketplace", Name: "acme-tools",
					Path: "omp://marketplace/acme-tools", Digest: digest.Bytes([]byte(ref)),
				}})
			})
		})
	})
}

// TestOmpNativeRedeivery pins the two non-idempotent verbs: a second delivery
// neither re-adds the marketplace (`marketplace add` refuses a registered
// name) nor installs without --force, and a marketplace this package's receipt
// records is reused even when the ref path moved.
func TestOmpNativeRedeivery(t *testing.T) {
	Convey("Given a marketplace this package already registered", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		cli := newOmpCLI()

		ref := ompMarketplace(t, "acme-tools", "caveman", "1.2.3")
		registered := ompMarketplace(t, "acme-tools", "caveman", "1.2.3")
		cli.marketplaces["acme-tools"] = registered

		h := host.NewOmp(
			host.WithHome(home), host.WithRunner(cli),
			host.WithOwnership(ownerMap{"omp://marketplace/acme-tools": "acme/caveman"}),
		)

		pkg := ompPackage(t)
		pkg.Marketplace = ref

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered again", func() {
			Convey("Then the receipt proves the name, no add is attempted and the install is forced", func() {
				So(err, ShouldBeNil)
				So(cli.Calls(), ShouldResemble, []string{
					"omp plugin marketplace list",
					"omp plugin install caveman@acme-tools --force",
					"omp plugin list --json",
				})
				So(res.Observed.Verified, ShouldBeTrue)
			})
		})
	})

	Convey("Given a marketplace the USER registered from the very ref path", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		cli := newOmpCLI()

		ref := ompMarketplace(t, "acme-tools", "caveman", "1.2.3")
		cli.marketplaces["acme-tools"] = ref

		h := host.NewOmp(host.WithHome(home), host.WithRunner(cli))

		pkg := ompPackage(t)
		pkg.Marketplace = ref

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When the package is delivered", func() {
			Convey("Then the delivery refuses to adopt it, and its inverse may not remove it", func() {
				failure, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(failure.Step, ShouldEqual, "plan")
				So(failure.Error(), ShouldContainSubstring, "not proven verger's")

				_, registered := cli.Registered("acme-tools")
				So(registered, ShouldBeTrue)

				_, installed := cli.Installed("caveman@acme-tools")
				So(installed, ShouldBeFalse)

				So(res.RMA, ShouldNotBeEmpty)
				So(res.RMA[0].Existed, ShouldBeTrue)

				Convey("And an uninstall of that receipt leaves the user's marketplace alone", func() {
					uninstalled, uninstallErr := h.Uninstall(t.Context(), home, receipt.Receipt{
						Strategy: string(host.Native), RMA: res.RMA,
					})

					So(uninstallErr, ShouldBeNil)
					So(strings.Join(uninstalled.Notes, "\n"), ShouldContainSubstring, "existed before this delivery; kept")

					_, stillThere := cli.Registered("acme-tools")
					So(stillThere, ShouldBeTrue)
					So(cli.Calls(), ShouldNotContain, "omp plugin marketplace remove acme-tools")
				})
			})
		})
	})

	Convey("Given a user marketplace whose path carries a space", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		cli := newOmpCLI()

		ref := filepath.Join(t.TempDir(), "my marketplace")
		writeFixtureFile(t, filepath.Join(ref, ".claude-plugin", "marketplace.json"), `{
  "name": "acme-tools",
  "owner": {"name": "acme"},
  "plugins": [{"name": "caveman", "source": "./caveman/1.2.3"}]
}`, 0o600)
		writeFixtureFile(t, filepath.Join(ref, "caveman", "1.2.3", ".claude-plugin", "plugin.json"), `{"name":"caveman","version":"1.2.3"}`, 0o600)

		cli.marketplaces["acme-tools"] = ref

		h := host.NewOmp(host.WithHome(home), host.WithRunner(cli))

		pkg := ompPackage(t)
		pkg.Marketplace = ref

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered", func() {
			Convey("Then a path match is still not an ownership proof", func() {
				_, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(res.RMA[0].Existed, ShouldBeTrue)

				_, registered := cli.Registered("acme-tools")
				So(registered, ShouldBeTrue)
			})
		})
	})
}

func TestOmpSynthDeliver(t *testing.T) {
	Convey("Given a synth package in the store", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		st := openStore(t)
		pkg := ompPackage(t)
		pkg.SynthDir = synthPackage(t, st, pkg.ID, pkg.Version).SynthDir

		cli := newOmpCLI()
		h := host.NewOmp(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash()))

		Convey("When it is delivered", func() {
			res, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})
			So(err, ShouldBeNil)

			Convey("Then the owner document is written and the plugin installed from it", func() {
				// The first list decides the marketplace name (ownerMarketplacePlan),
				// the second is the fresh in-lock baseline of the registration.
				So(cli.Calls(), ShouldResemble, []string{
					"omp plugin marketplace list",
					"omp plugin marketplace list",
					"omp plugin marketplace add " + filepath.Dir(filepath.Dir(pkg.SynthDir)),
					"omp plugin marketplace list",
					"omp plugin install caveman@acme --force",
					"omp plugin list --json",
				})
				So(res.Observed.Verified, ShouldBeTrue)
				So(res.RMA, ShouldResemble, []receipt.Op{
					{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "remove", "acme"}},
					{Kind: receipt.OpHostInstall, Command: []string{"plugin", "uninstall", "caveman@acme"}},
				})
			})

			Convey("Then the owner marketplace document lists the package", func() {
				root := filepath.Dir(filepath.Dir(pkg.SynthDir))
				So(readTestFile(t, filepath.Join(root, ".claude-plugin", "marketplace.json")), ShouldContainSubstring, "./caveman/"+pkg.Version)
			})

			Convey("Then a re-delivery reuses the registered marketplace without adding it again", func() {
				before := len(cli.Calls())

				_, againErr := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})
				So(againErr, ShouldBeNil)
				So(cli.Calls()[before:], ShouldResemble, []string{
					"omp plugin marketplace list",
					"omp plugin install caveman@acme --force",
					"omp plugin list --json",
				})
			})
		})
	})
}

// TestOmpSynthMarketplaceCollision pins decision F3 for omp: a foreign
// marketplace named after the owner is never touched — the package lands in
// <owner>-verger.
func TestOmpSynthMarketplaceCollision(t *testing.T) {
	Convey("Given a foreign marketplace named after the owner", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		st := openStore(t)
		pkg := ompPackage(t)
		pkg.SynthDir = synthPackage(t, st, pkg.ID, pkg.Version).SynthDir

		cli := newOmpCLI()
		cli.marketplaces["acme"] = "/elsewhere/acme"

		h := host.NewOmp(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash()))

		res, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})

		Convey("When the synth package is delivered", func() {
			Convey("Then it registers acme-verger and installs from it", func() {
				So(err, ShouldBeNil)
				So(cli.Calls(), ShouldContain, "omp plugin install caveman@acme-verger --force")
				So(res.RMA[1].Command, ShouldResemble, []string{"plugin", "uninstall", "caveman@acme-verger"})
				So(cli.marketplaces["acme"], ShouldEqual, "/elsewhere/acme")
			})
		})
	})
}

func TestOmpInstallRefusals(t *testing.T) {
	Convey("Given an add refusal that is not an already-registered marketplace", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		ref := filepath.Join(t.TempDir(), "not-a-marketplace")

		h, runner := newOmp(t, home, map[string]hostcli.Response{
			"omp plugin marketplace list": response("No marketplaces configured\n\nAdd one with: omp plugin marketplace add <source>\n"),
			"omp plugin marketplace add " + ref: {
				Code: 1, Stderr: "✘ Failed to add marketplace: Error: No marketplace document found",
			},
		})

		pkg := ompPackage(t)
		pkg.Marketplace = ref

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered", func() {
			Convey("Then the refusal surfaces unchanged and no install runs", func() {
				exit, ok := errors.AsType[*hostcli.ExitError](err)
				So(ok, ShouldBeTrue)
				So(strings.Contains(exit.Stderr, "No marketplace document found"), ShouldBeTrue)
				So(callKeys(runner), ShouldNotContain, "omp plugin install caveman@not-a-marketplace --force")
			})
		})
	})

	Convey("Given an unverified CLI grammar", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		ref := "https://github.com/acme/plugins.git"
		refused := "✘ error: unrecognized command 'plugin marketplace list'"

		h, _ := newOmp(t, home, map[string]hostcli.Response{
			"omp plugin marketplace list": {Code: 2, Stderr: refused},
		})

		pkg := ompPackage(t)
		pkg.Marketplace = ref

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When the read-only list fails", func() {
			Convey("Then the failure surfaces instead of a guessed marketplace name", func() {
				exit, ok := errors.AsType[*hostcli.ExitError](err)
				So(ok, ShouldBeTrue)
				So(exit.Stderr, ShouldEqual, refused)
			})
		})
	})
}

func TestOmpNativeVerifyFailure(t *testing.T) {
	Convey("Given an install the oracle does not report back", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		cli := newOmpCLI()
		cli.dropInstalls = true

		ref := ompMarketplace(t, "acme-tools", "caveman", "1.2.3")

		h := host.NewOmp(host.WithHome(home), host.WithRunner(cli))

		pkg := ompPackage(t)
		pkg.Marketplace = ref

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When the plugin is missing from the list", func() {
			Convey("Then the delivery fails at the verify step, never silently", func() {
				failure, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(failure.Step, ShouldEqual, "verify")
				So(failure.Error(), ShouldContainSubstring, "is not listed by the oracle")
				So(cli.Calls(), ShouldContain, "omp plugin install caveman@acme-tools --force")
			})
		})
	})
}

// TestOmpPolicyIsolation pins that the policy gate runs for every stratum
// while omp itself has no managed policy document: a policy written for
// another host never blocks an omp delivery.
func TestOmpPolicyIsolation(t *testing.T) {
	Convey("Given a Claude policy that would block every marketplace source", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		writeFixtureFile(t, filepath.Join(home, ".claude", "settings.json"),
			`{"strictKnownMarketplaces": []}`, 0o600)

		st := openStore(t)
		pkg := ompPackage(t)
		pkg.SynthDir = synthPackage(t, st, pkg.ID, pkg.Version).SynthDir
		pkg.Marketplace = ompMarketplace(t, "acme-tools", "caveman", pkg.Version)

		cli := newOmpCLI()
		h := host.NewOmp(
			host.WithHome(home), host.WithRunner(cli),
			host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)),
		)

		Convey("When omp delivers loose, synth and native", func() {
			Convey("Then no stratum is blocked by the foreign document", func() {
				for _, strategy := range []host.Strategy{host.Loose, host.Synth, host.Native} {
					delivery := host.Delivery{Package: pkg, Strategy: strategy}

					_, err := h.Deliver(t.Context(), home, delivery)
					So(err, ShouldBeNil)
				}
			})
		})
	})

	Convey("Given the shared dispatch", t, func() {
		h, _, home := ompWorld(t)

		Convey("When a strategy the adapter never delivers is requested", func() {
			Convey("Then it is refused before anything runs", func() {
				pkg := ompPackage(t)
				pkg.Marketplace = "https://github.com/acme/plugins.git"

				_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Silenced})
				unsupported, ok := errors.AsType[*host.UnsupportedStrategyError](err)
				So(ok, ShouldBeTrue)
				So(unsupported.Strategy, ShouldEqual, host.Silenced)
			})
		})
	})
}

// TestOmpUninstall pins the removal contract: both omp verbs refuse an unknown
// target, so a re-run of the inverse is a note; a resource that existed before
// the delivery is never removed, and a marketplace still serving an installed
// plugin is kept.
func TestOmpUninstall(t *testing.T) {
	Convey("Given an omp receipt with a plugin and its marketplace", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		cli := newOmpCLI()
		cli.marketplaces["acme"] = "/m/acme"
		cli.installed["caveman@acme"] = "1.2.3"

		h := host.NewOmp(host.WithHome(home), host.WithRunner(cli))

		r := receipt.Receipt{Strategy: string(host.Native), RMA: []receipt.Op{
			{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "remove", "acme"}},
			{Kind: receipt.OpHostInstall, Command: []string{"plugin", "uninstall", "caveman@acme"}},
		}}

		res, err := h.Uninstall(t.Context(), home, r)

		Convey("When it is uninstalled", func() {
			Convey("Then the plugin goes first, then the marketplace", func() {
				So(err, ShouldBeNil)
				So(cli.Calls(), ShouldResemble, []string{
					"omp plugin uninstall caveman@acme",
					"omp plugin list --json",
					"omp plugin marketplace remove acme",
				})
				_, installed := cli.Installed("caveman@acme")
				So(installed, ShouldBeFalse)

				_, registered := cli.Registered("acme")

				So(registered, ShouldBeFalse)
				So(res.Strategy, ShouldEqual, host.Native)
			})
		})
	})

	Convey("Given the host no longer knows the plugin or the marketplace", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		cli := newOmpCLI()
		h := host.NewOmp(host.WithHome(home), host.WithRunner(cli))

		r := receipt.Receipt{Strategy: string(host.Native), RMA: []receipt.Op{
			{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "remove", "acme"}},
			{Kind: receipt.OpHostInstall, Command: []string{"plugin", "uninstall", "caveman@acme"}},
		}}

		res, err := h.Uninstall(t.Context(), home, r)

		Convey("When it is uninstalled again", func() {
			Convey("Then both refusals become notes and the cell is clean", func() {
				So(err, ShouldBeNil)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "was not installed")
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "was not registered")
			})
		})
	})

	Convey("Given an op marked Existed and a file op", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		cli := newOmpCLI()
		h := host.NewOmp(host.WithHome(home), host.WithRunner(cli))

		r := receipt.Receipt{Strategy: string(host.Synth), RMA: []receipt.Op{
			{Kind: receipt.OpWriteFile, Path: filepath.Join(home, "x.md")},
			{Kind: receipt.OpHostInstall, Command: []string{"plugin", "uninstall", "caveman@acme"}, Existed: true},
			{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "remove", "acme"}, Existed: true},
		}}

		res, err := h.Uninstall(t.Context(), home, r)

		Convey("When it is uninstalled", func() {
			Convey("Then nothing runs, the host calls are empty and both ops are kept", func() {
				So(err, ShouldBeNil)
				So(cli.Calls(), ShouldBeEmpty)
				So(res.Notes, ShouldHaveLength, 2)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "existed before this delivery; kept")
			})
		})
	})

	Convey("Given a marketplace that still serves another installed plugin", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		cli := newOmpCLI()
		cli.marketplaces["acme"] = "/m/acme"
		cli.installed["other@acme"] = "9.9.9"

		h := host.NewOmp(host.WithHome(home), host.WithRunner(cli))

		r := receipt.Receipt{Strategy: string(host.Native), RMA: []receipt.Op{
			{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "remove", "acme"}},
		}}

		res, err := h.Uninstall(t.Context(), home, r)

		Convey("When the marketplace inverse runs", func() {
			Convey("Then it is kept with a note (the §4.8 refcount)", func() {
				So(err, ShouldBeNil)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "still serves 1 installed plugin(s); kept")

				_, registered := cli.Registered("acme")

				So(registered, ShouldBeTrue)
			})
		})
	})
}

// TestOmpLooseReceiptModes pins the file modes of the loose surface: skill
// trees are 0700 and rendered files 0600.
func TestOmpLooseReceiptModes(t *testing.T) {
	Convey("Given a delivered loose package", t, func() {
		h, _, home := ompWorld(t)

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: ompPackage(t), Strategy: host.Loose})
		So(err, ShouldBeNil)

		Convey("When the tree is inspected", func() {
			Convey("Then the modes are the ones the receipts recorded", func() {
				assertPathMode(t, filepath.Join(ompSharedSkills(home), "alpha"), 0o700)
				assertPathMode(t, filepath.Join(ompSharedSkills(home), "alpha", "SKILL.md"), 0o600)
				assertPathMode(t, filepath.Join(ompHomeAgent(home), "rules", "caveman.md"), 0o600)
				assertPathMode(t, ompMCPPath(home), 0o600)
			})
		})
	})
}

// TestOmpLooseApplyRoundTrip drives one loose delivery through pkg/apply, so
// the RMA the adapter records is the one a real install/remove replays.
func TestOmpLooseApplyRoundTrip(t *testing.T) {
	Convey("Given an applied loose delivery", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		st := openStore(t)

		deps := applyWorld(t, st, ownerMap{})
		owner := receiptsOwner{receipts: deps.Receipts}
		deps.Owned = owner

		h := host.NewOmp(host.WithHome(home), host.WithRunner(newOmpCLI()),
			host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)), host.WithOwnership(owner))
		deps.Hosts[host.Omp] = h

		// One MCP server, like the existing loose apply round trip: every
		// server of a config document shares the document path, and a receipt
		// carries each artifact path once (reported to the orchestrator).
		pkg := ompPackage(t)
		pkg.MCP = pkg.MCP[:1]

		installed := applyCell(t, deps, apply.Action{
			Kind: apply.ActionInstall, Host: host.Omp,
			Delivery: host.Delivery{Package: pkg, Strategy: host.Loose},
		})
		So(installed.Status, ShouldEqual, apply.StatusCurrent)

		rec, found, recErr := deps.Receipts.Get(pkg.ID, string(host.Omp), receipt.ScopeUser)
		So(recErr, ShouldBeNil)
		So(found, ShouldBeTrue)

		Convey("When it is removed", func() {
			removed := applyCell(t, deps, apply.Action{
				Kind: apply.ActionRemove, Host: host.Omp, Previous: &rec, Cause: "user", Initiator: "omp",
			})

			Convey("Then every host file the plan wrote is gone again", func() {
				So(removed.Status, ShouldEqual, apply.StatusCurrent)
				So(fileExists(filepath.Join(ompHomeAgent(home), "rules", "caveman.md")), ShouldBeFalse)
				So(fileExists(filepath.Join(ompHomeAgent(home), "rules", "always.md")), ShouldBeFalse)
				So(fileExists(filepath.Join(ompSharedSkills(home), "alpha")), ShouldBeFalse)
			})
		})
	})
}

// TestOmpNullableMarketplaceShadow pins the safety net of the strict verify:
// `omp plugin install <name>@<tag>` reaches the npm registry whenever the
// selector also names a dist-tag, and the registry wins over a registered
// marketplace of that name (live-observed: marketplace `beta` vs the public
// package `one@2.0.0-beta.137.1`). Such an install must fail the verify with
// the marketplace named, never be reported as a delivery of this package.
func TestOmpNullableMarketplaceShadow(t *testing.T) {
	Convey("Given a marketplace whose name is also an npm dist-tag", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		cli := newOmpCLI()

		ref := ompMarketplace(t, "beta", "caveman", "1.2.3")
		cli.shadow["caveman@beta"] = "2.0.0-beta.137.1"

		h := host.NewOmp(host.WithHome(home), host.WithRunner(cli))

		pkg := ompPackage(t)
		pkg.Marketplace = ref

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When the registry swallows the install", func() {
			Convey("Then the verify fails on the step, and no marketplace entry was invented", func() {
				failure, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(failure.Step, ShouldEqual, "verify")
				So(cli.Calls(), ShouldContain, "omp plugin install caveman@beta --force")

				registry, registered := cli.RegistryInstalled("caveman")
				So(registry, ShouldEqual, "2.0.0-beta.137.1")
				So(registered, ShouldBeTrue)

				_, fromMarketplace := cli.Installed("caveman@beta")
				So(fromMarketplace, ShouldBeFalse)

				So(failure.Error(), ShouldContainSubstring, "from the npm registry instead")
				So(failure.Error(), ShouldContainSubstring, "caveman@2.0.0-beta.137.1")
			})
		})
	})
}

// TestOmpOracleNPMEntryNeverMatchesAMarketplace pins the same net at the
// parsing level: an npm entry carries no marketplace, whatever its id looks
// like, so it can never satisfy a marketplace selector.
func TestOmpOracleNPMEntryNeverMatchesAMarketplace(t *testing.T) {
	Convey("Given a host listing a registry install beside a marketplace one", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		cli := newOmpCLI()
		cli.registry["caveman"] = "2.0.0-beta.137.1"
		cli.installed["alpha@beta"] = "1.0.0"

		h := host.NewOmp(host.WithHome(t.TempDir()), host.WithRunner(cli))

		listed, err := h.Oracle().List(t.Context())

		Convey("When the list is read", func() {
			Convey("Then the registry entry reports no marketplace and the marketplace one keeps its own", func() {
				So(err, ShouldBeNil)
				So(listed, ShouldHaveLength, 2)

				byName := map[string]string{}
				for _, entry := range listed {
					byName[entry.Name] = entry.Marketplace
				}

				So(byName["caveman"], ShouldEqual, "")
				So(byName["alpha"], ShouldEqual, "beta")
			})
		})
	})
}

// TestOmpOracleUnrecognizedDocument pins the F2 rule: a document whose keys the
// oracle does not recognize is "cannot tell", never "nothing installed" — the
// marketplace refcount keeps a marketplace when the oracle cannot list, so an
// empty answer from a renamed key must not look like an empty host.
func TestOmpOracleUnrecognizedDocument(t *testing.T) {
	Convey("Given a host answering an unfamiliar plugin list document", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		for _, body := range []string{"{}", "null", `{"plugins": {}}`, `{"installed": [], "available": []}`} {
			Convey("When the body is "+body, func() {
				h, _ := newOmp(t, t.TempDir(), map[string]hostcli.Response{
					"omp plugin list --json": response(body),
				})

				listed, err := h.Oracle().List(t.Context())

				Convey("Then the oracle reports that it cannot read the output", func() {
					oracleErr, ok := errors.AsType[*host.OracleError](err)
					So(ok, ShouldBeTrue)
					So(oracleErr.Output, ShouldEqual, body)
					So(listed, ShouldBeEmpty)
				})
			})
		}
	})
}

// TestOmpUninstallKeepsMarketplaceWhenListingUnclear pins the other half of F2:
// when the oracle cannot list, the marketplace refcount keeps the marketplace
// instead of removing one another plugin may need.
func TestOmpUninstallKeepsMarketplaceWhenListingUnclear(t *testing.T) {
	Convey("Given a host whose plugin list cannot be read", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		h, runner := newOmp(t, t.TempDir(), map[string]hostcli.Response{
			"omp plugin list --json": response("{}"),
		})

		r := receipt.Receipt{Strategy: string(host.Native), RMA: []receipt.Op{
			{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "remove", "acme"}},
		}}

		res, err := h.Uninstall(t.Context(), "", r)

		Convey("When the marketplace inverse runs", func() {
			Convey("Then it is kept with a note and the removal never runs", func() {
				So(err, ShouldBeNil)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "marketplace acme kept")
				So(callKeys(runner), ShouldNotContain, "omp plugin marketplace remove acme")
			})
		})
	})
}

// TestOmpLockFollowsTheStateRoot pins G-PROFILE: the lock and the mutations it
// serializes resolve through the same root, and that root follows a named
// profile and PI_CONFIG_DIR (live-verified: with OMP_PROFILE=work the host reads
// ~/.omp/profiles/work/plugins/ and reports nothing installed from the default
// root; PI_CONFIG_DIR moves the same state below <home>/<dir>).
func TestOmpLockFollowsTheStateRoot(t *testing.T) {
	cases := []struct {
		name  string
		env   map[string]string
		unset []string
		want  string
	}{
		{"default root", nil, nil, filepath.Join(".omp", ".omp-plugin.verger.lock")},
		{"named profile", map[string]string{"OMP_PROFILE": "work"}, nil, filepath.Join(".omp", "profiles", "work", ".omp-plugin.verger.lock")},
		{"pi config dir", map[string]string{"PI_CONFIG_DIR": "cfg"}, nil, filepath.Join("cfg", ".omp-plugin.verger.lock")},
		// The host's own profile rules, live-measured with `omp config path` on
		// 18.4.1: `default` is no profile, an explicitly empty OMP_PROFILE hides
		// PI_PROFILE, and PI_PROFILE only counts while OMP_PROFILE is unset.
		{"OMP_PROFILE=default is the default root", map[string]string{"OMP_PROFILE": "default"}, nil, filepath.Join(".omp", ".omp-plugin.verger.lock")},
		{"an empty OMP_PROFILE hides PI_PROFILE", map[string]string{"OMP_PROFILE": "", "PI_PROFILE": "work"}, nil, filepath.Join(".omp", ".omp-plugin.verger.lock")},
		{"OMP_PROFILE=default hides PI_PROFILE", map[string]string{"OMP_PROFILE": "default", "PI_PROFILE": "work"}, nil, filepath.Join(".omp", ".omp-plugin.verger.lock")},
		{"PI_PROFILE alone names the profile", map[string]string{"PI_PROFILE": "work"}, []string{"OMP_PROFILE"}, filepath.Join(".omp", "profiles", "work", ".omp-plugin.verger.lock")},
		{"PI_PROFILE=default is the default root", map[string]string{"PI_PROFILE": "default"}, []string{"OMP_PROFILE"}, filepath.Join(".omp", ".omp-plugin.verger.lock")},
		{"OMP_PROFILE wins over PI_PROFILE", map[string]string{"OMP_PROFILE": "work", "PI_PROFILE": "other"}, nil, filepath.Join(".omp", "profiles", "work", ".omp-plugin.verger.lock")},
	}

	for _, item := range cases {
		Convey("Given the "+item.name, t, func() {
			h, cli, home := ompWorld(t)

			for name, value := range item.env {
				t.Setenv(name, value)
			}

			for _, name := range item.unset {
				unsetEnv(t, name)
			}

			ref := ompMarketplace(t, "acme-tools", "caveman", "1.2.3")

			pkg := ompPackage(t)
			pkg.Marketplace = ref

			_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

			Convey("When a delivery mutates the plugin state", func() {
				Convey("Then the lock lives in the root the host reads, not the default one", func() {
					So(err, ShouldBeNil)
					So(cli.Calls(), ShouldNotBeEmpty)

					So(fileExists(filepath.Join(home, item.want)), ShouldBeTrue)
					So(fileExists(filepath.Join(home, ".omp", ".omp-plugin.verger.lock")), ShouldEqual, item.want == filepath.Join(".omp", ".omp-plugin.verger.lock"))
				})
			})
		})
	}
}

// TestOmpProfileMatchesTheHost pins the profile matrix against what the host
// itself reports: the same table was measured with `omp config path` on
// omp 18.4.1 in an isolated HOME, and the loose agent dir must follow it too.
func TestOmpProfileMatchesTheHost(t *testing.T) {
	cases := []struct {
		name     string
		env      map[string]string
		unset    []string // variables that must be ABSENT, which t.Setenv cannot express
		profiles string   // "" for the default root
	}{
		{"no variables", nil, nil, ""},
		{"OMP_PROFILE=default", map[string]string{"OMP_PROFILE": "default"}, nil, ""},
		{"empty OMP_PROFILE with PI_PROFILE=work", map[string]string{"OMP_PROFILE": "", "PI_PROFILE": "work"}, nil, ""},
		{"OMP_PROFILE=work", map[string]string{"OMP_PROFILE": "work"}, nil, "work"},
		{"PI_PROFILE=work with OMP_PROFILE unset", map[string]string{"PI_PROFILE": "work"}, []string{"OMP_PROFILE"}, "work"},
		{"PI_PROFILE=default with OMP_PROFILE unset", map[string]string{"PI_PROFILE": "default"}, []string{"OMP_PROFILE"}, ""},
		{"OMP_PROFILE=work over PI_PROFILE=other", map[string]string{"OMP_PROFILE": "work", "PI_PROFILE": "other"}, nil, "work"},
		{"OMP_PROFILE=default over PI_PROFILE=work", map[string]string{"OMP_PROFILE": "default", "PI_PROFILE": "work"}, nil, ""},
	}

	for _, item := range cases {
		Convey("Given "+item.name, t, func() {
			fakeOmp(t)
			clearOmpEnv(t)

			for name, value := range item.env {
				t.Setenv(name, value)
			}

			// "Not set" is a case of its own: the host ignores PI_PROFILE while
			// OMP_PROFILE is set, an empty value included.
			for _, name := range item.unset {
				unsetEnv(t, name)
			}

			home := t.TempDir()

			h, _ := newCursorlessOmp(t, home)

			_, err := h.Deliver(t.Context(), home, host.Delivery{Package: ompPackage(t), Strategy: host.Loose})

			Convey("Then the agent dir lands where the host reads it", func() {
				So(err, ShouldBeNil)

				want := filepath.Join(home, ".omp", "agent")
				if item.profiles != "" {
					want = filepath.Join(home, ".omp", "profiles", item.profiles, "agent")
				}

				So(fileExists(filepath.Join(want, "agents", "reviewer.md")), ShouldBeTrue)
				So(fileExists(filepath.Join(home, ".omp", "agent", "agents", "reviewer.md")), ShouldEqual, item.profiles == "")
			})
		})
	}
}

// TestOmpMCPSharedDocumentMerge pins the read-modify-write itself: two
// deliveries in a row, under the shared lock, keep the user's own key and
// comment beside both writers' servers.
func TestOmpMCPSharedDocumentMerge(t *testing.T) {
	Convey("Given a foreign key and comment in the shared document", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		st := openStore(t)

		writeFixtureFile(t, filepath.Join(home, ".omp", "agent", "mcp.json"), `{
  // the user's own comment
  "mcpServers": {},
  "disabledServers": ["legacy"]
}`, 0o600)

		h, _ := newOmp(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)))

		Convey("When two packages are delivered one after another", func() {
			first, firstErr := h.Deliver(t.Context(), home, host.Delivery{Package: ompPackage(t), Strategy: host.Loose})
			So(firstErr, ShouldBeNil)
			So(first.Artifacts, ShouldNotBeEmpty)

			// Only the shared document is under test here: the second package
			// carries no component of its own, so both deliveries touch
			// mcp.json alone and no ownership map is involved.
			second := ompPackage(t)
			second.ID = "acme/other"
			second.Components = nil
			second.MCP = []manifest.MCPServer{{Name: "other", Command: []string{"node", "other.js"}}}

			h2, _ := newOmp(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)))

			_, secondErr := h2.Deliver(t.Context(), home, host.Delivery{Package: second, Strategy: host.Loose})

			Convey("Then both writers' keys and the user's own survive", func() {
				So(secondErr, ShouldBeNil)

				doc := readTestFile(t, filepath.Join(home, ".omp", "agent", "mcp.json"))
				So(doc, ShouldContainSubstring, "the user's own comment")
				So(doc, ShouldContainSubstring, "disabledServers")
				So(doc, ShouldContainSubstring, `"fs"`)
				So(doc, ShouldContainSubstring, `"web"`)
				So(doc, ShouldContainSubstring, `"other"`)
			})
		})
	})

	_ = lockPathOf
}

// lockPathOf is the shared lock of one test home.
func lockPathOf(home string) string {
	return filepath.Join(home, ".omp", ".omp-plugin.verger.lock")
}

// TestOmpMCPSharedLock pins the contract with the other writer of
// <agentDir>/mcp.json (beadle's omp surface, and the host itself): the whole
// read-modify-write of that document runs under the shared
// <state root>/.omp-plugin.verger.lock, a held lock makes the delivery refuse
// instead of writing, and a released lock lets the next writer in.
func TestOmpMCPSharedLock(t *testing.T) {
	lockPath := func(home string) string {
		return filepath.Join(home, ".omp", ".omp-plugin.verger.lock")
	}

	Convey("Given a lock held by another writer", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		st := openStore(t)

		if err := os.MkdirAll(filepath.Dir(lockPath(home)), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		held := flock.New(lockPath(home))
		if _, err := held.TryLock(); err != nil {
			t.Fatalf("hold the lock: %v", err)
		}

		h, _ := newOmp(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)))

		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()

		_, err := h.Deliver(ctx, home, host.Delivery{Package: ompPackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then nothing is written and the refusal names the other writer", func() {
				failure, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(failure.Error(), ShouldContainSubstring, "another writer holds")
				So(failure.Error(), ShouldContainSubstring, ".omp-plugin.verger.lock")
				So(fileExists(filepath.Join(home, ".omp", "agent", "mcp.json")), ShouldBeFalse)
				So(fileExists(filepath.Join(home, ".agents")), ShouldBeFalse)
			})
		})

		Convey("And once the lock is released the same delivery writes", func() {
			if err := held.Unlock(); err != nil {
				t.Fatalf("release the lock: %v", err)
			}

			fresh := t.TempDir()

			releasedStore := openStore(t)

			released, _ := newOmp(t, fresh, nil,
				host.WithStore(releasedStore), host.WithTrash(releasedStore.Trash()), host.WithSecrets(ompSecrets(t)))

			_, deliverErr := released.Deliver(t.Context(), fresh, host.Delivery{Package: ompPackage(t), Strategy: host.Loose})

			So(deliverErr, ShouldBeNil)
			So(fileExists(filepath.Join(fresh, ".omp", "agent", "mcp.json")), ShouldBeTrue)
		})
	})

	Convey("Given a package without MCP servers", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		st := openStore(t)

		h, _ := newOmp(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)))

		pkg := ompPackage(t)
		pkg.MCP = nil

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})
		So(err, ShouldBeNil)

		Convey("When it is delivered", func() {
			Convey("Then no lock is taken for a write that cannot happen", func() {
				So(fileExists(lockPath(home)), ShouldBeFalse)
			})
		})
	})

	Convey("Given a dry run of a package with MCP servers", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		st := openStore(t)

		h, _ := newOmp(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)))

		_, dryErr := h.Deliver(t.Context(), home, host.Delivery{Package: ompPackage(t), Strategy: host.Loose, DryRun: true})

		Convey("When it is planned", func() {
			Convey("Then nothing is written and no lock is taken", func() {
				So(dryErr, ShouldBeNil)
				So(fileExists(lockPath(home)), ShouldBeFalse)
				So(fileExists(filepath.Join(home, ".omp", "agent", "mcp.json")), ShouldBeFalse)
			})
		})
	})
}

// TestOmpLockWaitIsBounded pins the wait limit: a writer that cannot take the
// shared lock gives up with a clear error (DefaultLockWait, the same bound a
// sibling tool uses on this file), instead of waiting for the user to
// intervene. The test shortens the limit through the adapter option.
func TestOmpLockWaitIsBounded(t *testing.T) {
	Convey("Given a lock held for longer than the wait limit", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		waited := 200 * time.Millisecond

		home := t.TempDir()
		st := openStore(t)
		lockFile := filepath.Join(home, ".omp", ".omp-plugin.verger.lock")

		if err := os.MkdirAll(filepath.Dir(lockFile), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		held := flock.New(lockFile)
		if _, err := held.TryLock(); err != nil {
			t.Fatalf("hold the lock: %v", err)
		}

		defer func() { _ = held.Unlock() }()

		h, _ := newOmp(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)),
			host.WithLockWait(waited))

		started := time.Now()

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: ompPackage(t), Strategy: host.Loose})

		Convey("When the delivery runs", func() {
			Convey("Then it gives up within the limit, names it, and writes nothing", func() {
				So(err, ShouldNotBeNil)
				So(time.Since(started), ShouldBeLessThan, 5*time.Second)

				failure, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(failure.Error(), ShouldContainSubstring, "backing off")
				So(failure.Error(), ShouldContainSubstring, "more than "+waited.String())
				So(fileExists(filepath.Join(home, ".omp", "agent", "mcp.json")), ShouldBeFalse)
			})
		})
	})
}

// TestOmpRepeatedDeliveryKeepsOwnMCPKeys pins the package's own MCP key across
// repeated deliveries through pkg/apply (the path the CLI takes): the same
// package installed, updated three times and then removed keeps the user's own
// key and comment throughout, and the removal takes only this package's key.
//
// The document records one artifact however many keys it carries, and a
// delivery that changes nothing still records its keys, so an update cannot
// reconcile them away.
func TestOmpRepeatedDeliveryKeepsOwnMCPKeys(t *testing.T) {
	Convey("Given an applied omp delivery with a foreign key in mcp.json", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		st := openStore(t)

		deps := applyWorld(t, st, ownerMap{})
		owner := receiptsOwner{receipts: deps.Receipts}
		deps.Owned = owner

		writeFixtureFile(t, filepath.Join(home, ".omp", "agent", "mcp.json"), `{
  // the user's own comment
  "mcpServers": {"mine": {"command": "mine"}},
  "disabledServers": ["legacy"]
}`, 0o600)

		h := host.NewOmp(host.WithHome(home), host.WithRunner(newOmpCLI()),
			host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)), host.WithOwnership(owner))
		deps.Hosts[host.Omp] = h

		pkg := ompPackage(t)

		install := applyCell(t, deps, apply.Action{
			Kind: apply.ActionInstall, Host: host.Omp,
			Delivery: host.Delivery{Package: pkg, Strategy: host.Loose},
		})
		So(install.Status, ShouldEqual, apply.StatusCurrent)

		rec, found, recErr := deps.Receipts.Get(pkg.ID, string(host.Omp), receipt.ScopeUser)
		So(recErr, ShouldBeNil)
		So(found, ShouldBeTrue)

		assertKeys := func(stage string) {
			doc := readTestFile(t, filepath.Join(home, ".omp", "agent", "mcp.json"))

			for _, key := range []string{`"fs"`, `"mine"`, "disabledServers", "the user's own comment"} {
				So(doc+" at "+stage, ShouldContainSubstring, key)
			}
		}

		assertKeys("after install")

		Convey("When it is delivered again and again", func() {
			previous := rec

			for i := range 3 {
				updated := applyCell(t, deps, apply.Action{
					Kind: apply.ActionUpdate, Host: host.Omp,
					Previous: &previous, Delivery: host.Delivery{Package: pkg, Strategy: host.Loose},
				})
				So(updated.Status, ShouldEqual, apply.StatusCurrent)

				assertKeys("after update")

				next, nextFound, nextErr := deps.Receipts.Get(pkg.ID, string(host.Omp), receipt.ScopeUser)
				So(nextErr, ShouldBeNil)
				So(nextFound, ShouldBeTrue)

				previous = next

				t.Logf("update %d kept every key", i+1)
			}

			Convey("Then removing the package takes only its own keys", func() {
				removed := applyCell(t, deps, apply.Action{
					Kind: apply.ActionRemove, Host: host.Omp, Previous: &previous, Cause: "user", Initiator: "omp",
				})
				So(removed.Status, ShouldEqual, apply.StatusCurrent)

				doc := readTestFile(t, filepath.Join(home, ".omp", "agent", "mcp.json"))
				So(doc, ShouldNotContainSubstring, `"fs"`)
				So(doc, ShouldContainSubstring, `"mine"`)
				So(doc, ShouldContainSubstring, "disabledServers")
				So(doc, ShouldContainSubstring, "the user's own comment")
			})
		})
	})
}

// TestOmpMCPOneArtifactPerDocument pins the artifact model of a shared config
// document: two MCP servers land as two config-key ops and ONE artifact, so the
// receipt is accepted (two artifacts claiming one path are refused) and
// pkg/apply can still resolve the document's keys by the artifact path.
func TestOmpMCPOneArtifactPerDocument(t *testing.T) {
	Convey("Given a package with two MCP servers and a user's own server", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		st := openStore(t)

		writeFixtureFile(t, filepath.Join(home, ".omp", "agent", "mcp.json"), `{
  "mcpServers": {"mine": {"command": "mine"}}
}`, 0o600)

		h, _ := newOmp(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: ompPackage(t), Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then one artifact covers the document and every key has its own op", func() {
				So(err, ShouldBeNil)

				document := filepath.Join(home, ".omp", "agent", "mcp.json")

				artifacts, keys := 0, 0

				for _, artifact := range res.Artifacts {
					if artifact.Path == document {
						artifacts++
					}
				}

				for _, op := range res.RMA {
					if op.Kind == receipt.OpConfigKey && op.Path == document {
						keys++
					}
				}

				So(artifacts, ShouldEqual, 1)
				So(keys, ShouldEqual, 2)
				assertArtifactsBackedByOps(t, res)
			})
		})
	})
}
