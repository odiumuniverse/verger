package host_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
	"github.com/odiumuniverse/verger/pkg/store"
)

const geminiFixtureRoot = "testdata/gemini/acme"

// commandTOMLGolden is the exact Gemini rendering of the fixture command.
const commandTOMLGolden = `description = "Runs the dev loop."
prompt = """
Run the dev loop.
"""
`

// fakeGemini puts an executable `gemini` shim at the front of PATH.
func fakeGemini(t *testing.T) {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "gemini"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // G306: a fake host CLI must be executable
		t.Fatalf("write shim: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// newGemini builds the Gemini adapter with a scripted runner.
func newGemini(t *testing.T, home string, script map[string]hostcli.Response, opts ...host.Option) (host.Host, *hostcli.ScriptRunner) {
	t.Helper()

	runner := hostcli.NewScriptRunner(script)
	all := append([]host.Option{host.WithHome(home), host.WithRunner(runner)}, opts...)

	return host.NewGemini(all...), runner
}

// geminiPackage parses the Gemini fixture and adds the rule component the
// manifest format does not carry.
func geminiPackage(t *testing.T) host.Package {
	t.Helper()

	return adapterFixturePackage(t, geminiFixtureRoot, manifest.FormatGemini)
}

// geminiSettingsPath resolves the settings.json of one test home.
func geminiSettingsPath(home string) string {
	return filepath.Join(home, ".gemini", "settings.json")
}

// geminiPolicyHome writes a Gemini settings.json carrying one policy document.
func geminiPolicyHome(t *testing.T, doc string) string {
	t.Helper()

	home := t.TempDir()
	writeFixtureFile(t, geminiSettingsPath(home), doc, 0o600)

	return home
}

func TestGeminiDetect(t *testing.T) {
	Convey("Given a home without a Gemini config dir", t, func() {
		home := t.TempDir()
		t.Setenv("PATH", t.TempDir())

		h, _ := newGemini(t, home, nil)

		Convey("When no config dir and no binary exist", func() {
			Convey("Then Detect is false", func() {
				So(h.Detect(home), ShouldBeFalse)
			})
		})

		Convey("When the config dir exists", func() {
			if err := os.MkdirAll(filepath.Join(home, ".gemini"), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})

	Convey("Given the gemini binary on PATH", t, func() {
		fakeGemini(t)
		home := t.TempDir()

		h, _ := newGemini(t, home, nil)

		Convey("When only the binary exists", func() {
			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})
}

func TestGeminiLooseGolden(t *testing.T) {
	Convey("Given a Gemini package and a temp home", t, func() {
		fakeGemini(t)
		home := t.TempDir()
		writeFixtureFile(t, geminiSettingsPath(home), `{"model": "opus"}`, 0o600)

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})
		pkg := geminiPackage(t)

		h, runner := newGemini(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered loose", func() {
			Convey("Then the exact home tree appears and GEMINI.md is never touched", func() {
				So(err, ShouldBeNil)
				So(homeFiles(t, home), ShouldResemble, []string{
					".gemini/agents/reviewer.md",
					".gemini/commands/dev.toml",
					".gemini/settings.json",
					".gemini/skills/alpha/SKILL.md",
					".gemini/skills/alpha/scripts/run.sh",
					".gemini/skills/beta/SKILL.md",
					".gemini/skills/rule-caveman/SKILL.md",
				})
				So(fileExists(filepath.Join(home, "GEMINI.md")), ShouldBeFalse)
				So(runner.Calls(), ShouldBeEmpty)
			})

			Convey("Then the command is rendered as Gemini TOML (golden bytes)", func() {
				So(readTestFile(t, filepath.Join(home, ".gemini", "commands", "dev.toml")), ShouldEqual, commandTOMLGolden)
			})

			Convey("Then the agent carries the Gemini tool vocabulary", func() {
				agent := readTestFile(t, filepath.Join(home, ".gemini", "agents", "reviewer.md"))
				So(agent, ShouldContainSubstring, "read_file")
				So(agent, ShouldContainSubstring, "search_file_content")
				So(agent, ShouldContainSubstring, "Review the diff.")
			})

			Convey("Then hooks and mcpServers land in the one settings.json, foreign keys kept", func() {
				settings := readTestFile(t, geminiSettingsPath(home))
				So(settings, ShouldEqualJSON, `{
  "model": "opus",
  "hooks": {
    "BeforeTool": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "node \"`+pkg.Root+`/hooks/guard.js\"", "timeout": 5}]}
    ]
  },
  "mcpServers": {
    "fs": {"command": "node", "args": ["`+pkg.Root+`/server.js"], "env": {"TOKEN": "s3cr3t-token"}},
    "web": {"httpUrl": "https://example.com/mcp"}
  }
}`)
			})

			Convey("Then the secret stays out of the result and tightens the config to 0600", func() {
				rendered, marshalErr := json.Marshal(res)
				So(marshalErr, ShouldBeNil)
				So(string(rendered), ShouldNotContainSubstring, "s3cr3t-token")
				So(strings.Join(res.Notes, "\n"), ShouldNotContainSubstring, "s3cr3t-token")
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "0600")

				assertPathMode(t, geminiSettingsPath(home), 0o600)
				assertPathMode(t, filepath.Join(home, ".gemini", "skills"), 0o700)
			})

			Convey("Then the RMA mirrors every artifact in install order", func() {
				assertArtifactsBackedByOps(t, res)
				So(res.Artifacts, ShouldNotBeEmpty)

				ops := map[string]receipt.Op{}
				for _, op := range res.RMA {
					ops[op.Path] = op
				}

				So(ops[filepath.Join(home, ".gemini", "skills", "alpha")].Kind, ShouldEqual, receipt.OpCopyTree)
				So(ops[filepath.Join(home, ".gemini", "agents", "reviewer.md")].Kind, ShouldEqual, receipt.OpWriteFile)
				So(ops[filepath.Join(home, ".gemini", "commands", "dev.toml")].Kind, ShouldEqual, receipt.OpWriteFile)
				So(ops[geminiSettingsPath(home)].Kind, ShouldEqual, receipt.OpConfigKey)

				keys := map[string]bool{}

				for _, op := range res.RMA {
					if op.Path == geminiSettingsPath(home) {
						keys[op.KeyPath] = true
					}
				}

				So(keys, ShouldResemble, map[string]bool{
					"hooks": true, "mcpServers.fs": true, "mcpServers.web": true,
				})
			})
		})
	})
}

func TestGeminiLooseHooksGating(t *testing.T) {
	Convey("Given AllowHooks=false", t, func() {
		fakeGemini(t)
		home := t.TempDir()

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})
		h, _ := newGemini(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: geminiPackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then hooks are skipped but mcpServers still land", func() {
				So(err, ShouldBeNil)

				settings := readTestFile(t, geminiSettingsPath(home))
				So(settings, ShouldNotContainSubstring, `"hooks"`)
				So(settings, ShouldContainSubstring, `"mcpServers"`)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "consent")

				for _, artifact := range res.Artifacts {
					So(artifact.Kind, ShouldNotEqual, "hook")
				}
			})
		})
	})
}

func TestGeminiEventMapping(t *testing.T) {
	Convey("Given hooks with a canon Stop event and a Gemini event", t, func() {
		fakeGemini(t)
		home := t.TempDir()

		st := openStore(t)
		pkg := geminiPackage(t)
		pkg.MCP = nil
		pkg.Hooks = []manifest.Hook{
			{Event: manifest.EventStop, Command: "echo stop"},
			{Event: manifest.EventPreTool, Matcher: "Bash", Command: "echo pre"},
		}

		h, _ := newGemini(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then Stop is skipped with a warning and BeforeTool is rendered", func() {
				So(err, ShouldBeNil)

				settings := readTestFile(t, geminiSettingsPath(home))
				So(settings, ShouldContainSubstring, "BeforeTool")
				So(settings, ShouldContainSubstring, "echo pre")
				So(settings, ShouldNotContainSubstring, "echo stop")
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "stop")
			})
		})
	})
}

func TestGeminiLooseCollisions(t *testing.T) {
	Convey("Given a foreign skill directory", t, func() {
		fakeGemini(t)
		home := t.TempDir()
		foreign := filepath.Join(home, ".gemini", "skills", "alpha")

		writeFixtureFile(t, filepath.Join(foreign, "SKILL.md"), "foreign\n", 0o600)

		st := openStore(t)
		h, _ := newGemini(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: withoutMCP(geminiPackage(t)), Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then a *CollisionError stops the cell before any write", func() {
				typed, ok := errors.AsType[*host.CollisionError](err)
				So(ok, ShouldBeTrue)
				So(typed.Path, ShouldEqual, foreign)
				So(fileExists(filepath.Join(home, ".gemini", "agents")), ShouldBeFalse)
			})
		})
	})

	Convey("Given an agent owned by this package", t, func() {
		fakeGemini(t)
		home := t.TempDir()
		target := filepath.Join(home, ".gemini", "agents", "reviewer.md")

		writeFixtureFile(t, target, "old agent\n", 0o600)

		st := openStore(t)
		h, _ := newGemini(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithOwnership(ownerMap{target: "acme/caveman"}))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: withoutMCP(geminiPackage(t)), Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then it is replaced and the previous bytes wait in trash", func() {
				So(err, ShouldBeNil)
				So(readTestFile(t, target), ShouldNotContainSubstring, "old agent")
				So(trashContains(t, st, "old agent"), ShouldBeTrue)

				op := rmaOp(res, target)
				So(op.Existed, ShouldBeTrue)
				So(op.Backup, ShouldNotBeEmpty)
			})
		})
	})

	Convey("Given a foreign command TOML", t, func() {
		fakeGemini(t)
		home := t.TempDir()
		target := filepath.Join(home, ".gemini", "commands", "dev.toml")

		writeFixtureFile(t, target, "prompt = \"foreign\"\n", 0o600)

		st := openStore(t)
		h, _ := newGemini(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: withoutMCP(geminiPackage(t)), Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then the command is skipped with a note, never overwritten", func() {
				So(err, ShouldBeNil)
				So(readTestFile(t, target), ShouldEqual, "prompt = \"foreign\"\n")
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "dev")

				for _, artifact := range res.Artifacts {
					So(artifact.Name, ShouldNotEqual, "dev")
				}
			})
		})
	})
}

func TestGeminiLooseSecrets(t *testing.T) {
	Convey("Given a package whose secret is missing", t, func() {
		fakeGemini(t)
		home := t.TempDir()
		st := openStore(t)

		h, _ := newGemini(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(secretStore(t, nil)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: geminiPackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then it is a *MissingSecretsError and nothing is written", func() {
				typed, ok := errors.AsType[*host.MissingSecretsError](err)
				So(ok, ShouldBeTrue)
				So(typed.Names, ShouldResemble, []string{"MCP_TOKEN"})
				So(fileExists(filepath.Join(home, ".gemini")), ShouldBeFalse)
			})
		})
	})
}

func TestGeminiLooseVariables(t *testing.T) {
	Convey("Given a server carrying an unknown host variable", t, func() {
		fakeGemini(t)
		home := t.TempDir()
		st := openStore(t)
		pkg := geminiPackage(t)

		pkg.MCP = []manifest.MCPServer{{
			Name:    "bad",
			Command: []string{"node", "${FOO_BAR}/server.js"},
		}}

		h, _ := newGemini(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then it refuses with the variable named and writes nothing", func() {
				typed, ok := errors.AsType[*host.UnsupportedVariableError](err)
				So(ok, ShouldBeTrue)
				So(typed.Variable, ShouldEqual, "FOO_BAR")
				So(fileExists(filepath.Join(home, ".gemini")), ShouldBeFalse)
			})
		})
	})

	Convey("Given extensionPath in hooks and servers", t, func() {
		fakeGemini(t)
		home := t.TempDir()
		st := openStore(t)
		pkg := geminiPackage(t)

		h, _ := newGemini(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then extensionPath becomes the package root", func() {
				So(err, ShouldBeNil)

				settings := readTestFile(t, geminiSettingsPath(home))
				So(settings, ShouldContainSubstring, pkg.Root+"/hooks/guard.js")
				So(settings, ShouldContainSubstring, pkg.Root+"/server.js")
				So(settings, ShouldNotContainSubstring, "${extensionPath}")
			})
		})
	})
}

func TestGeminiLooseMCPHandsOff(t *testing.T) {
	Convey("Given a user-owned mcpServers entry with the same name", t, func() {
		fakeGemini(t)
		home := t.TempDir()

		writeFixtureFile(t, geminiSettingsPath(home), `{"mcpServers": {"fs": {"command": "user-owned"}}}`, 0o600)

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})
		h, _ := newGemini(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		before := readTestFile(t, geminiSettingsPath(home))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: geminiPackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then the cell is hands-off and nothing is written", func() {
				_, ok := errors.AsType[*render.HandsOffError](err)
				So(ok, ShouldBeTrue)
				So(readTestFile(t, geminiSettingsPath(home)), ShouldEqual, before)
				So(fileExists(filepath.Join(home, ".gemini", "skills")), ShouldBeFalse)
			})
		})
	})
}

func TestGeminiLooseJSONCSettings(t *testing.T) {
	Convey("Given a settings.json with a user comment", t, func() {
		fakeGemini(t)
		home := t.TempDir()
		writeFixtureFile(t, geminiSettingsPath(home), "{\n  // user comment\n  \"model\": \"opus\"\n}\n", 0o600)

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})
		h, _ := newGemini(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: geminiPackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			data := readTestFile(t, geminiSettingsPath(home))

			Convey("Then the comment survives and both key families land", func() {
				So(err, ShouldBeNil)
				So(data, ShouldContainSubstring, "// user comment")
				So(data, ShouldContainSubstring, `"hooks"`)
				So(data, ShouldContainSubstring, `"mcpServers"`)
				So(digestOf(t, res, "hook", geminiSettingsPath(home)), ShouldNotBeEmpty)
				So(digestOf(t, res, "", geminiSettingsPath(home)), ShouldNotBeEmpty)
			})
		})
	})
}

func TestGeminiLooseDryRun(t *testing.T) {
	Convey("Given a loose dry run", t, func() {
		fakeGemini(t)
		home := t.TempDir()
		st := openStore(t)
		before := snapshotHome(t, home)

		h, runner := newGemini(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: geminiPackage(t), Strategy: host.Loose, AllowHooks: true, DryRun: true})

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

func TestGeminiLooseUnicode(t *testing.T) {
	Convey("Given a unicode skill name", t, func() {
		fakeGemini(t)
		home := t.TempDir()

		payload := t.TempDir()
		writeFixtureFile(t, filepath.Join(payload, "skills", "κ-caveman", "SKILL.md"), "# κ\n", 0o600)

		pkg := host.Package{
			ID:   "acme/uni",
			Root: payload,
			Components: []manifest.Component{{
				Kind: manifest.KindSkill, Name: "κ-caveman", Path: "skills/κ-caveman",
			}},
		}

		h, _ := newGemini(t, home, nil)

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then the name survives byte-for-byte", func() {
				So(err, ShouldBeNil)
				So(readTestFile(t, filepath.Join(home, ".gemini", "skills", "κ-caveman", "SKILL.md")), ShouldEqual, "# κ\n")
			})
		})
	})
}

func TestGeminiConcurrentDryRun(t *testing.T) {
	Convey("Given concurrent dry runs of one adapter", t, func() {
		fakeGemini(t)
		home := t.TempDir()
		st := openStore(t)

		h, _ := newGemini(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})))

		assertConcurrentDryRun(t, h, home, filepath.Join(home, ".gemini"), func() host.Package { return geminiPackage(t) })
	})
}

func TestGeminiNativeDeliver(t *testing.T) {
	Convey("Given a scripted native install", t, func() {
		fakeGemini(t)
		home := t.TempDir()
		ref := "https://github.com/acme/plugins.git"

		script := map[string]hostcli.Response{
			"gemini extensions install " + ref + " --consent": response(""),
			"gemini extensions list --output-format json":     {Stderr: `[{"name":"caveman","version":"1.2.3","path":"/tmp/ext"}]`},
		}

		h, runner := newGemini(t, home, script)

		pkg := geminiPackage(t)
		pkg.Marketplace = ref

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered", func() {
			Convey("Then the non-interactive argv runs and the oracle verifies from stderr", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{
					"gemini extensions install " + ref + " --consent",
					"gemini extensions list --output-format json",
				})
				So(res.Observed.Verified, ShouldBeTrue)
			})

			Convey("Then the RMA carries the inverse uninstall argv", func() {
				So(res.RMA, ShouldResemble, []receipt.Op{
					{Kind: receipt.OpHostInstall, Command: []string{"extensions", "uninstall", "caveman"}},
				})
			})
		})
	})
}

// geminiSynth lays out acme/caveman in the store with its Gemini manifest.
func geminiSynth(t *testing.T) (*store.Store, host.Package) {
	t.Helper()

	st := openStore(t)
	pkg := geminiPackage(t)
	pkg.SynthDir = synthPackage(t, st, pkg.ID, pkg.Version).SynthDir

	writeFixtureFile(t, filepath.Join(pkg.SynthDir, "gemini-extension.json"),
		`{"name":"caveman","version":"`+pkg.Version+`"}`, 0o600)

	return st, pkg
}

// geminiSynthWorld is one Gemini home over the stateful fake CLI with
// acme/caveman laid out in the store.
func geminiSynthWorld(t *testing.T, opts ...host.Option) (host.Host, *geminiCLI, *store.Store, host.Package, string) {
	t.Helper()

	fakeGemini(t)

	home := t.TempDir()
	st, pkg := geminiSynth(t)
	cli := newGeminiCLI()
	cli.installDir = filepath.Join(home, ".gemini", "extensions")

	all := append([]host.Option{host.WithHome(home), host.WithRunner(cli)}, opts...)

	return host.NewGemini(all...), cli, st, pkg, home
}

// TestGeminiSynthDeliver pins the Gemini CLI 0.61.0 synth grammar (T1.8-G):
// `extensions link <dir> --consent` (without --consent the CLI asks for
// workspace trust and a non-interactive run hangs), the list read from stderr,
// a same-dir re-delivery is a no-op and a new version is uninstalled and
// relinked (the CLI refuses to link over an installed name).
func TestGeminiSynthDeliver(t *testing.T) {
	Convey("Given a synth package in the store", t, func() {
		h, cli, st, pkg, _ := geminiSynthWorld(t)
		synthDir := pkg.SynthDir

		// A non-clean spelling must reach the host as the clean absolute path.
		pkg.SynthDir = filepath.Join(synthDir, "..", filepath.Base(synthDir))

		Convey("When it is delivered", func() {
			res, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})
			So(err, ShouldBeNil)

			Convey("Then the bare name is linked non-interactively from the absolute store path and verified", func() {
				So(cli.Calls(), ShouldResemble, []string{
					"gemini extensions list --output-format json",
					"gemini extensions link " + synthDir + " --consent",
					"gemini extensions list --output-format json",
				})

				linked, ok := cli.Linked("caveman")
				So(ok, ShouldBeTrue)
				So(linked, ShouldEqual, synthDir)
				So(res.Observed.Verified, ShouldBeTrue)
				So(res.RMA, ShouldResemble, []receipt.Op{
					{Kind: receipt.OpHostInstall, Command: []string{"extensions", "uninstall", "caveman"}},
				})
			})

			Convey("Then the extension name is the artifact that proves ownership next time", func() {
				sum, sumErr := digest.Tree(synthDir)
				So(sumErr, ShouldBeNil)
				So(res.Artifacts, ShouldResemble, []receipt.Artifact{{
					Kind: "extension", Name: "caveman", Path: "gemini://extension/caveman", Digest: sum,
				}})
			})

			Convey("Then delivering the same dir again links nothing and marks the inverse pre-existing", func() {
				before := len(cli.Calls())

				again, againErr := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})
				So(againErr, ShouldBeNil)
				So(cli.Calls()[before:], ShouldResemble, []string{
					"gemini extensions list --output-format json",
					"gemini extensions list --output-format json",
				})
				So(again.RMA[0].Existed, ShouldBeTrue)
			})

			Convey("Then a new version is uninstalled and relinked", func() {
				next := synthPackage(t, st, pkg.ID, "9.9.9")
				writeFixtureFile(t, filepath.Join(next.SynthDir, "gemini-extension.json"), `{"name":"caveman","version":"9.9.9"}`, 0o600)

				before := len(cli.Calls())

				updated, updateErr := h.Deliver(t.Context(), "", host.Delivery{Package: next, Strategy: host.Synth})
				So(updateErr, ShouldBeNil)
				So(cli.Calls()[before:], ShouldResemble, []string{
					"gemini extensions list --output-format json",
					"gemini extensions uninstall caveman",
					"gemini extensions link " + next.SynthDir + " --consent",
					"gemini extensions list --output-format json",
				})

				linked, _ := cli.Linked("caveman")
				So(linked, ShouldEqual, next.SynthDir)
				So(updated.RMA[0].Existed, ShouldBeTrue)
			})
		})

		Convey("When it is only planned", func() {
			res, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth, DryRun: true})

			Convey("Then only the read-only list runs", func() {
				So(err, ShouldBeNil)
				So(cli.Calls(), ShouldResemble, []string{"gemini extensions list --output-format json"})
				So(res.RMA, ShouldHaveLength, 1)
			})
		})
	})
}

// TestGeminiSynthNameCollision pins decision F3 for Gemini: the extension is
// named after the plugin; a foreign extension of that name is never touched —
// the package lands as <owner>-<name> from a renamed copy of the synth dir.
func TestGeminiSynthNameCollision(t *testing.T) {
	Convey("Given a user's own extension named caveman", t, func() {
		h, cli, _, pkg, _ := geminiSynthWorld(t)
		cli.installed["caveman"] = "/elsewhere/caveman"
		overlay := pkg.SynthDir + "+gemini"

		Convey("When the synth package is delivered", func() {
			res, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})

			Convey("Then it links a renamed copy as acme-caveman and the store copy keeps its name", func() {
				So(err, ShouldBeNil)
				So(cli.Calls(), ShouldContain, "gemini extensions link "+overlay+" --consent")

				foreign, _ := cli.Linked("caveman")
				So(foreign, ShouldEqual, "/elsewhere/caveman")
				So(manifestNameOf(t, filepath.Join(overlay, "gemini-extension.json")), ShouldEqual, "acme-caveman")
				So(manifestNameOf(t, filepath.Join(pkg.SynthDir, "gemini-extension.json")), ShouldEqual, "caveman")
				So(fileExists(filepath.Join(overlay, ".claude-plugin", "plugin.json")), ShouldBeTrue)
				So(res.RMA, ShouldResemble, []receipt.Op{
					{Kind: receipt.OpHostInstall, Command: []string{"extensions", "uninstall", "acme-caveman"}},
				})
				So(res.Artifacts[0].Name, ShouldEqual, "acme-caveman")
			})
		})
	})

	Convey("Given foreign extensions named caveman and acme-caveman", t, func() {
		h, cli, _, pkg, _ := geminiSynthWorld(t)
		cli.installed["caveman"] = "/elsewhere/a"
		cli.installed["acme-caveman"] = "/elsewhere/b"

		_, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})

		Convey("When the synth package is delivered", func() {
			Convey("Then the cell is hands-off after the read-only list alone", func() {
				_, ok := errors.AsType[*render.HandsOffError](err)
				So(ok, ShouldBeTrue)
				So(cli.Calls(), ShouldResemble, []string{"gemini extensions list --output-format json"})
				So(fileExists(pkg.SynthDir+"+gemini"), ShouldBeFalse)
			})
		})
	})

	Convey("Given a caveman extension this package's receipt records, linked elsewhere", t, func() {
		h, cli, _, pkg, _ := geminiSynthWorld(t, host.WithOwnership(ownerMap{"gemini://extension/caveman": "acme/caveman"}))
		cli.installed["caveman"] = "/old/store/caveman/0.9.0"

		_, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})

		Convey("When it is delivered again", func() {
			Convey("Then the receipt proves the name: it is uninstalled and relinked as caveman", func() {
				So(err, ShouldBeNil)
				So(cli.Calls(), ShouldContain, "gemini extensions uninstall caveman")
				So(cli.Calls(), ShouldContain, "gemini extensions link "+pkg.SynthDir+" --consent")
			})
		})
	})
}

// TestGeminiUninstallTolerance pins the Gemini removal contract: an extension
// the host no longer knows ("Extension not found.") is already removed, and
// one that existed before the delivery is never removed by its inverse.
func TestGeminiUninstallTolerance(t *testing.T) {
	Convey("Given a Gemini host with a kept extension", t, func() {
		fakeGemini(t)
		home := t.TempDir()
		cli := newGeminiCLI()
		cli.installed["kept"] = "/k"

		h := host.NewGemini(host.WithHome(home), host.WithRunner(cli))

		uninstall := func(op receipt.Op) (host.Result, error) {
			return h.Uninstall(t.Context(), home, receipt.Receipt{
				Package: "acme/caveman", Host: "gemini", Scope: receipt.ScopeUser, Strategy: string(host.Synth), RMA: []receipt.Op{op},
			})
		}

		Convey("When the inverse of an unknown extension runs", func() {
			res, err := uninstall(receipt.Op{Kind: receipt.OpHostInstall, Command: []string{"extensions", "uninstall", "gone"}})

			Convey("Then it is already done, with a note", func() {
				So(err, ShouldBeNil)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "gone")
			})
		})

		Convey("When the inverse of a pre-existing extension runs", func() {
			_, err := uninstall(receipt.Op{Kind: receipt.OpHostInstall, Command: []string{"extensions", "uninstall", "kept"}, Existed: true})

			Convey("Then the extension is kept", func() {
				So(err, ShouldBeNil)

				_, ok := cli.Linked("kept")
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When the inverse op carries no argv", func() {
			_, err := uninstall(receipt.Op{Kind: receipt.OpHostInstall})

			Convey("Then it is reported as a receipt defect, never a panic", func() {
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "no argv")
			})
		})
	})
}

// TestGeminiUninstallLeavesHostOwnedIntegrityStore pins the host-owned file
// policy for gemini-cli 0.61.0: ~/.gemini/extension_integrity.json is an
// HMAC-SHA256-signed store whose key lives in the macOS Keychain, and
// ExtensionIntegrityManager exposes only verify/store — no delete. A surgical
// edit would invalidate the signature and mark every one of the user's
// extensions invalid; deleting the file would lose the records of other tools.
// verger's uninstall therefore runs the CLI alone: the extension (and the link
// the CLI materializes below ~/.gemini/extensions) goes, the integrity store
// stays byte-identical. The two captured states beside the fixture
// (extension-integrity-0.61.0.json before, …-after-uninstall-0.61.0.json as
// the CLI rewrites it — a foreign-tool record appears) show the change only the
// CLI may make.
func TestGeminiUninstallLeavesHostOwnedIntegrityStore(t *testing.T) {
	Convey("Given a Gemini home whose host-owned integrity store precedes the install", t, func() {
		h, cli, _, pkg, home := geminiSynthWorld(t)

		before := readTestFile(t, filepath.Join("testdata", "gemini", "extension-integrity-0.61.0.json"))
		after := readTestFile(t, filepath.Join("testdata", "gemini", "extension-integrity-after-uninstall-0.61.0.json"))
		So(after, ShouldNotEqual, before)

		store := filepath.Join(home, ".gemini", "extension_integrity.json")
		writeFixtureFile(t, store, before, 0o600)

		delivered, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Synth})
		So(err, ShouldBeNil)

		_, err = h.Uninstall(t.Context(), home, receipt.Receipt{
			Package: pkg.ID, Host: "gemini", Scope: receipt.ScopeUser, Strategy: string(host.Synth), RMA: delivered.RMA,
		})

		Convey("When the extension is uninstalled", func() {
			Convey("Then the CLI removed the extension and the link it materialized", func() {
				So(err, ShouldBeNil)

				_, linked := cli.Linked("caveman")
				So(linked, ShouldBeFalse)
				So(fileExists(filepath.Join(home, ".gemini", "extensions", "caveman")), ShouldBeFalse)
			})

			Convey("Then the host-owned integrity store is byte-identical, never rewritten or deleted", func() {
				So(readTestFile(t, store), ShouldEqual, before)
				So(readTestFile(t, store), ShouldNotEqual, after)
			})
		})
	})
}

// manifestNameOf reads the top-level name of one JSON manifest file.
func manifestNameOf(t *testing.T, path string) string {
	t.Helper()

	var doc struct {
		Name string `json:"name"`
	}

	if err := json.Unmarshal([]byte(readTestFile(t, path)), &doc); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	return doc.Name
}

func TestGeminiInstallRefusals(t *testing.T) {
	Convey("Given install requests without their inputs", t, func() {
		fakeGemini(t)
		home := t.TempDir()

		h, runner := newGemini(t, home, nil)

		Convey("When a native install has no marketplace", func() {
			_, err := h.Deliver(t.Context(), home, host.Delivery{Package: geminiPackage(t), Strategy: host.Native})

			Convey("Then it reports *NotSupportedError", func() {
				_, ok := errors.AsType[*host.NotSupportedError](err)
				So(ok, ShouldBeTrue)
				So(runner.Calls(), ShouldBeEmpty)
			})
		})

		Convey("When a synth install has no synth dir", func() {
			_, err := h.Deliver(t.Context(), home, host.Delivery{Package: geminiPackage(t), Strategy: host.Synth})

			Convey("Then it reports *NotSupportedError", func() {
				_, ok := errors.AsType[*host.NotSupportedError](err)
				So(ok, ShouldBeTrue)
				So(runner.Calls(), ShouldBeEmpty)
			})
		})

		Convey("When Silenced is requested", func() {
			_, err := h.Deliver(t.Context(), home, host.Delivery{Package: geminiPackage(t), Strategy: host.Silenced})

			Convey("Then the adapter refuses the strategy", func() {
				_, ok := errors.AsType[*host.UnsupportedStrategyError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestGeminiNativeVerifyFailure(t *testing.T) {
	Convey("Given an oracle that does not list the extension", t, func() {
		fakeGemini(t)
		home := t.TempDir()
		ref := "https://github.com/acme/plugins.git"

		h, runner := newGemini(t, home, map[string]hostcli.Response{
			"gemini extensions install " + ref + " --consent": response(""),
			"gemini extensions list --output-format json":     {Stderr: "[]"},
		})

		pkg := geminiPackage(t)
		pkg.Marketplace = ref

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered", func() {
			Convey("Then verify fails with a *DeliveryError, never a silent step down", func() {
				typed, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(typed.Step, ShouldEqual, "verify")
				So(runner.Calls(), ShouldHaveLength, 2)
			})
		})
	})
}

func TestGeminiUninstall(t *testing.T) {
	Convey("Given a receipt with host-install RMA ops", t, func() {
		fakeGemini(t)
		home := t.TempDir()

		h, runner := newGemini(t, home, map[string]hostcli.Response{
			"gemini extensions uninstall caveman": response(""),
			"gemini extensions uninstall other":   response(""),
		})

		r := receipt.Receipt{
			Package:  "acme/caveman",
			Host:     "gemini",
			Scope:    receipt.ScopeUser,
			Strategy: string(host.Native),
			RMA: []receipt.Op{
				{Kind: receipt.OpHostInstall, Command: []string{"extensions", "uninstall", "caveman"}},
				{Kind: receipt.OpHostInstall, Command: []string{"extensions", "uninstall", "other"}},
				{Kind: receipt.OpWriteFile, Path: filepath.Join(home, "file"), Digest: "aa"},
			},
		}

		res, err := h.Uninstall(t.Context(), home, r)

		Convey("When it is uninstalled", func() {
			Convey("Then host ops run in reverse order and other ops are left to apply", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{
					"gemini extensions uninstall other",
					"gemini extensions uninstall caveman",
				})
				So(res.Strategy, ShouldEqual, host.Native)
			})
		})
	})
}

// TestGeminiPolicyEveryStratum pins DESIGN §1.3/§4.8 for Gemini: the shared
// checker gates every stratum and the two boolean keys gate their strata.
func TestGeminiPolicyEveryStratum(t *testing.T) {
	ref := "https://github.com/acme/plugins.git"

	Convey("Given blockedMarketplaces listing the source ref", t, func() {
		fakeGemini(t)
		home := geminiPolicyHome(t, `{"blockedMarketplaces": ["`+ref+`"]}`)

		for _, strategy := range []host.Strategy{host.Native, host.Synth, host.Loose} {
			Convey("When "+string(strategy)+" delivers the blocked source", func() {
				runner := hostcli.NewScriptRunner(nil)
				h := host.NewGemini(host.WithHome(home), host.WithRunner(runner))

				pkg := geminiPackage(t)
				pkg.Marketplace = ref
				pkg.SynthDir = t.TempDir()

				_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: strategy, AllowHooks: true})

				Convey("Then the cell is blocked by policy with the rule named", func() {
					typed, ok := errors.AsType[*host.PolicyError](err)
					So(ok, ShouldBeTrue)
					So(typed.Rule, ShouldEqual, "blockedMarketplaces")
					So(typed.Host, ShouldEqual, host.Gemini)
					So(runner.Calls(), ShouldBeEmpty)
					So(fileExists(filepath.Join(home, ".gemini", "skills")), ShouldBeFalse)
				})
			})
		}
	})

	Convey("Given disableCommandPluginSources", t, func() {
		fakeGemini(t)
		home := geminiPolicyHome(t, `{"disableCommandPluginSources": true}`)

		Convey("When the CLI strata want an install", func() {
			pkg := geminiPackage(t)
			pkg.Marketplace = ref

			assertCLIPolicyBlocked(t, home, pkg, func(runner *hostcli.ScriptRunner) host.Host {
				return host.NewGemini(host.WithHome(home), host.WithRunner(runner))
			})
		})
	})

	Convey("Given allowManagedHooksOnly", t, func() {
		fakeGemini(t)
		home := geminiPolicyHome(t, `{"allowManagedHooksOnly": true}`)

		Convey("When loose would deliver plugin hooks", func() {
			st := openStore(t)
			secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})
			h, _ := newGemini(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

			res, err := h.Deliver(t.Context(), home, host.Delivery{Package: geminiPackage(t), Strategy: host.Loose, AllowHooks: true})

			Convey("Then hooks are skipped, mcpServers still land and the rule is named", func() {
				So(err, ShouldBeNil)

				settings := readTestFile(t, geminiSettingsPath(home))
				So(settings, ShouldNotContainSubstring, `"hooks"`)
				So(settings, ShouldContainSubstring, `"mcpServers"`)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "allowManagedHooksOnly")
			})
		})
	})
}

// TestGeminiSharedSettingsClaimsMCPServers pins F3: gemini writes hooks and MCP
// servers into one `settings.json`, so the document's own artifact claims
// whichever component planned first — the receipt must still carry the `mcp`
// kind, one claim per server, or `status` and `remove` have no MCP claim at all.
func TestGeminiSharedSettingsClaimsMCPServers(t *testing.T) {
	Convey("Given a Gemini package that ships both hooks and MCP servers", t, func() {
		home := t.TempDir()
		st := openStore(t)
		pkg := geminiPackage(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})

		So(len(pkg.MCP), ShouldBeGreaterThan, 0)

		h, _ := newGemini(t, home, nil,
			host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		res, err := h.Deliver(t.Context(), home, host.Delivery{
			Package: pkg, Strategy: host.Loose, AllowHooks: true,
		})
		So(err, ShouldBeNil)

		Convey("When it is delivered", func() {
			Convey("Then the receipt claims every MCP server beside the document", func() {
				claimed := map[string]bool{}

				for _, artifact := range res.Artifacts {
					if strings.HasSuffix(artifact.Kind, "-record") {
						claimed[artifact.Kind[:len(artifact.Kind)-len("-record")]] = true
					}
				}

				So(claimed, ShouldContainKey, "mcp")

				names := make([]string, 0, len(res.Artifacts))

				for _, artifact := range res.Artifacts {
					if artifact.Kind == "mcp-record" {
						names = append(names, artifact.Name)
						So(artifact.Digest.Valid(), ShouldBeTrue)
					}
				}

				for _, server := range pkg.MCP {
					So(names, ShouldContain, server.Name)
				}
			})

			Convey("Then every claim is backed by an op on its own path", func() {
				ops := make(map[string]bool, len(res.RMA))

				for _, op := range res.RMA {
					ops[op.Path] = true
				}

				for _, artifact := range res.Artifacts {
					So(ops, ShouldContainKey, artifact.Path)
				}
			})
		})
	})
}
