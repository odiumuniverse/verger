package host_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
	"github.com/odiumuniverse/verger/pkg/store"
)

const codexFixtureRoot = "testdata/codex/acme"

// agentTOMLGolden is the exact Codex rendering of the fixture reviewer agent;
// the leading blank line preserves the source markdown body.
const agentTOMLGolden = `name = "reviewer"
description = "Reviews diffs."
developer_instructions = """

Review the diff.
"""
sandbox_mode = "read-only"
`

// fakeCodex puts an executable `codex` shim at the front of PATH.
func fakeCodex(t *testing.T) {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // G306: a fake host CLI must be executable
		t.Fatalf("write shim: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// newCodex builds the Codex adapter with a scripted runner.
func newCodex(t *testing.T, home string, script map[string]hostcli.Response, opts ...host.Option) (host.Host, *hostcli.ScriptRunner) {
	t.Helper()

	runner := hostcli.NewScriptRunner(script)
	all := append([]host.Option{host.WithHome(home), host.WithRunner(runner)}, opts...)

	return host.NewCodex(all...), runner
}

// useCodexHome pins CODEX_HOME to a profile directory.
func useCodexHome(t *testing.T, dir string) {
	t.Helper()

	t.Setenv("CODEX_HOME", dir)
}

// ownerExisting claims existing paths for one package (tests that re-deliver).
type ownerExisting string

// Owner implements host.PathOwner.
func (o ownerExisting) Owner(path string) (string, bool) {
	if _, err := os.Lstat(path); err != nil {
		return "", false
	}

	return string(o), true
}

// codexProfile returns a fresh temp CODEX_HOME and pins it.
func codexProfile(t *testing.T) string {
	t.Helper()

	return t.TempDir()
}

// codexPackage parses the Codex fixture and adds the rule component the Codex
// manifest format does not carry.
func codexPackage(t *testing.T) host.Package {
	t.Helper()

	return adapterFixturePackage(t, codexFixtureRoot, manifest.FormatCodex)
}

// adapterFixturePackage parses one adapter fixture and adds the rule component
// the manifest format does not carry.
func adapterFixturePackage(t *testing.T, root string, format manifest.Format) host.Package {
	t.Helper()

	parsed, err := manifest.Parse(root, format, manifest.WithID("acme/caveman"))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	ruleDigest := digestFile(t, filepath.Join(root, "rules", "caveman.md"))

	components := slices.Clone(parsed.Components)

	components = append(components, manifest.Component{
		Kind: manifest.KindRule, Name: "caveman", Path: "rules/caveman.md", Digest: ruleDigest,
	})

	return host.Package{
		ID:         parsed.ID,
		Version:    parsed.Version,
		Format:     parsed.Format,
		Root:       parsed.Root,
		Components: components,
		MCP:        parsed.MCP,
		Hooks:      parsed.Hooks,
	}
}

// readTestFile reads one file the test itself wrote.
func readTestFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp home
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(data)
}

// decodeTOML decodes one TOML document.
func decodeTOML(t *testing.T, data string) map[string]any {
	t.Helper()

	doc := map[string]any{}
	if err := toml.Unmarshal([]byte(data), &doc); err != nil {
		t.Fatalf("decode toml: %v", err)
	}

	return doc
}

// mcpEntry digs one mcp_servers entry out of a decoded config document.
func mcpEntry(t *testing.T, doc map[string]any, name string) map[string]any {
	t.Helper()

	servers, ok := doc["mcp_servers"].(map[string]any)
	if !ok {
		t.Fatalf("config has no mcp_servers table")
	}

	entry, ok := servers[name].(map[string]any)
	if !ok {
		t.Fatalf("config has no mcp_servers.%s entry", name)
	}

	return entry
}

// configBytes reads the CODEX_HOME config.toml.
func configBytes(t *testing.T, profile string) string {
	t.Helper()

	return readTestFile(t, filepath.Join(profile, "config.toml"))
}

// assertPathMode checks the permission bits of one path.
func assertPathMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode of %s = %o, want %o", path, got, want)
	}
}

func TestCodexDetect(t *testing.T) {
	Convey("Given a home without a Codex config dir", t, func() {
		home := t.TempDir()
		t.Setenv("CODEX_HOME", "")
		t.Setenv("PATH", t.TempDir())

		h, _ := newCodex(t, home, nil)

		Convey("When no config dir and no binary exist", func() {
			Convey("Then Detect is false", func() {
				So(h.Detect(home), ShouldBeFalse)
			})
		})

		Convey("When the config dir exists", func() {
			if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})

	Convey("Given the codex binary on PATH", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		t.Setenv("CODEX_HOME", "")

		h, _ := newCodex(t, home, nil)

		Convey("When only the binary exists", func() {
			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})

	Convey("Given a CODEX_HOME profile", t, func() {
		home := t.TempDir()
		t.Setenv("PATH", t.TempDir())

		if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		profile := filepath.Join(t.TempDir(), "profile")
		useCodexHome(t, profile)

		h, _ := newCodex(t, home, nil)

		Convey("When the profile dir is missing but the default exists", func() {
			Convey("Then Detect is false", func() {
				So(h.Detect(home), ShouldBeFalse)
			})
		})

		Convey("When the profile dir exists", func() {
			if err := os.MkdirAll(profile, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})
}

func TestCodexLooseGolden(t *testing.T) {
	Convey("Given a Codex package, a temp CODEX_HOME and ~/.agents", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})
		pkg := codexPackage(t)

		h, runner := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered loose", func() {
			Convey("Then the skills land under the shared ~/.agents/skills root", func() {
				So(err, ShouldBeNil)
				So(homeFiles(t, home), ShouldResemble, []string{
					".agents/skills/alpha/SKILL.md",
					".agents/skills/alpha/scripts/run.sh",
					".agents/skills/beta/SKILL.md",
					".agents/skills/rule-caveman/SKILL.md",
				})
				So(runner.Calls(), ShouldBeEmpty)
			})

			Convey("Then the Codex home carries the TOML agent, prompt, hooks and config", func() {
				So(readTestFile(t, filepath.Join(profile, "agents", "reviewer.toml")), ShouldEqual, agentTOMLGolden)
				So(readTestFile(t, filepath.Join(profile, "prompts", "dev.md")), ShouldEqual, "\nRun the dev loop.\n")
				So(configBytes(t, profile), ShouldContainSubstring, "[mcp_servers.fs]")
			})

			Convey("Then the dropped agent fields are noted", func() {
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "tools")
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "reviewer")
			})

			Convey("Then hooks are rendered in the Codex dialect with the trust note", func() {
				hooks := readTestFile(t, filepath.Join(profile, "hooks.json"))
				So(hooks, ShouldEqualJSON, `{
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "node \"`+pkg.Root+`/hooks/guard.js\"", "timeout": 5}]}
    ]
  }
}`)

				notes := strings.Join(res.Notes, "\n")
				So(notes, ShouldContainSubstring, "review them in /hooks")
				So(notes, ShouldContainSubstring, "trusts hooks by hash")
			})

			Convey("Then config.toml carries both MCP servers with the secret resolved", func() {
				entry := mcpEntry(t, decodeTOML(t, configBytes(t, profile)), "fs")
				So(entry["command"], ShouldEqual, "node")
				So(entry["args"], ShouldResemble, []any{pkg.Root + "/server.js"})

				env, ok := entry["env"].(map[string]any)
				So(ok, ShouldBeTrue)
				So(env["TOKEN"], ShouldEqual, "s3cr3t-token")

				web := mcpEntry(t, decodeTOML(t, configBytes(t, profile)), "web")
				So(web["url"], ShouldEqual, "https://example.com/mcp")
			})

			Convey("Then the secret is only in the config file, never in the result", func() {
				rendered, marshalErr := json.Marshal(res)
				So(marshalErr, ShouldBeNil)
				So(string(rendered), ShouldNotContainSubstring, "s3cr3t-token")
				So(strings.Join(res.Notes, "\n"), ShouldNotContainSubstring, "s3cr3t-token")
			})

			Convey("Then modes are 0700/0600 and the secret-bearing config is 0600", func() {
				assertHomeModes(t, home)
				assertPathMode(t, filepath.Join(profile, "agents"), 0o700)
				assertPathMode(t, filepath.Join(profile, "config.toml"), 0o600)
				assertPathMode(t, filepath.Join(profile, "hooks.json"), 0o600)
			})

			Convey("Then the RMA mirrors every artifact in install order", func() {
				So(res.RMA, ShouldHaveLength, len(res.Artifacts))
				So(res.Artifacts, ShouldNotBeEmpty)

				ops := map[string]receipt.Op{}
				for _, op := range res.RMA {
					ops[op.Path] = op
				}

				So(ops[filepath.Join(home, ".agents", "skills", "alpha")].Kind, ShouldEqual, receipt.OpCopyTree)
				So(ops[filepath.Join(profile, "agents", "reviewer.toml")].Kind, ShouldEqual, receipt.OpWriteFile)
				So(ops[filepath.Join(profile, "hooks.json")].Kind, ShouldEqual, receipt.OpConfigKey)
				So(ops[filepath.Join(profile, "hooks.json")].KeyPath, ShouldEqual, "hooks")

				configPath := filepath.Join(profile, "config.toml")
				configKeys := map[string]bool{}

				for _, op := range res.RMA {
					if op.Path == configPath {
						So(op.Kind, ShouldEqual, receipt.OpConfigKey)

						configKeys[op.KeyPath] = true
					}
				}

				So(configKeys, ShouldResemble, map[string]bool{"mcp_servers.fs": true, "mcp_servers.web": true})
			})
		})
	})
}

func TestCodexLooseHooksGating(t *testing.T) {
	Convey("Given AllowHooks=false", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})
		pkg := codexPackage(t)

		h, _ := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then hooks.json is untouched and no hook artifact, RMA or trust note exists", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(profile, "hooks.json")), ShouldBeFalse)

				for _, artifact := range res.Artifacts {
					So(artifact.Kind, ShouldNotEqual, "hook")
				}

				for _, op := range res.RMA {
					So(op.Path, ShouldNotEqual, filepath.Join(profile, "hooks.json"))
				}

				notes := strings.Join(res.Notes, "\n")
				So(notes, ShouldContainSubstring, "consent")
				So(notes, ShouldNotContainSubstring, "/hooks")
			})
		})
	})

	Convey("Given a commented hooks.json with a foreign key and AllowHooks=true", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)

		hooks := filepath.Join(profile, "hooks.json")
		writeFixtureFile(t, hooks, "{\n  // keep this comment\n  \"other\": true\n}\n", 0o600)

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})

		h, _ := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: codexPackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When the package is delivered", func() {
			Convey("Then hooks land and the comment and the foreign key survive", func() {
				So(err, ShouldBeNil)

				body := readTestFile(t, hooks)
				So(body, ShouldContainSubstring, "// keep this comment")
				So(body, ShouldContainSubstring, `"other": true`)
				So(body, ShouldContainSubstring, `"hooks"`)
			})
		})
	})
}

func TestCodexLooseCollisions(t *testing.T) {
	Convey("Given a foreign skill directory in the shared ~/.agents/skills", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)

		foreign := filepath.Join(home, ".agents", "skills", "alpha")

		writeFixtureFile(t, filepath.Join(foreign, "SKILL.md"), "foreign\n", 0o600)

		st := openStore(t)
		h, _ := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: codexPackage(t), Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then a *CollisionError stops the cell before any write", func() {
				typed, ok := errors.AsType[*host.CollisionError](err)
				So(ok, ShouldBeTrue)
				So(typed.Path, ShouldEqual, foreign)
				So(typed.Owner, ShouldBeEmpty)
				So(fileExists(filepath.Join(profile, "agents")), ShouldBeFalse)
			})
		})
	})

	Convey("Given a skill owned by another package (DSH reads the same root)", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)

		target := filepath.Join(home, ".agents", "skills", "alpha")

		writeFixtureFile(t, filepath.Join(target, "SKILL.md"), "other\n", 0o600)

		st := openStore(t)
		h, _ := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithOwnership(ownerMap{target: "other/pkg"}))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: codexPackage(t), Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then the collision names the other owner", func() {
				typed, ok := errors.AsType[*host.CollisionError](err)
				So(ok, ShouldBeTrue)
				So(typed.Owner, ShouldEqual, "other/pkg")
			})
		})
	})

	Convey("Given an agent owned by this package", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		t.Setenv("CODEX_HOME", "")

		target := filepath.Join(home, ".codex", "agents", "reviewer.toml")

		writeFixtureFile(t, target, "old agent\n", 0o600)

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})
		h, _ := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets),
			host.WithOwnership(ownerMap{target: "acme/caveman"}))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: codexPackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then it is replaced and the previous bytes wait in trash", func() {
				So(err, ShouldBeNil)
				So(readTestFile(t, target), ShouldEqual, agentTOMLGolden)
				So(trashContains(t, st, "old agent"), ShouldBeTrue)

				op := rmaOp(res, target)
				So(op.Existed, ShouldBeTrue)
				So(op.Backup, ShouldNotBeEmpty)
			})
		})
	})

	Convey("Given a foreign prompt file", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)
		target := filepath.Join(profile, "prompts", "dev.md")

		writeFixtureFile(t, target, "foreign prompt\n", 0o600)

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})
		h, _ := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: codexPackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then the prompt is skipped with a note, never overwritten", func() {
				So(err, ShouldBeNil)
				So(readTestFile(t, target), ShouldEqual, "foreign prompt\n")
				So(fileExists(filepath.Join(home, ".agents", "skills", "alpha")), ShouldBeTrue)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "dev")
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "skipped")

				for _, artifact := range res.Artifacts {
					So(artifact.Name, ShouldNotEqual, "dev")
				}
			})
		})
	})
}

func TestCodexLooseSecrets(t *testing.T) {
	Convey("Given a package whose secret is missing", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)
		st := openStore(t)

		h, _ := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(secretStore(t, nil)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: codexPackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then it is a *MissingSecretsError and nothing is written", func() {
				typed, ok := errors.AsType[*host.MissingSecretsError](err)
				So(ok, ShouldBeTrue)
				So(typed.Names, ShouldResemble, []string{"MCP_TOKEN"})
				So(fileExists(filepath.Join(home, ".agents")), ShouldBeFalse)
				So(fileExists(filepath.Join(profile, "config.toml")), ShouldBeFalse)
			})
		})
	})

	Convey("Given an existing config.toml that receives a secret", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)

		writeFixtureFile(t, filepath.Join(profile, "config.toml"), "# my codex config\nmodel = \"gpt-5\"\n", 0o644)

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})
		h, _ := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: codexPackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then the config is tightened to 0600 with a note and foreign keys stay", func() {
				So(err, ShouldBeNil)
				So(configBytes(t, profile), ShouldContainSubstring, "# my codex config")
				So(configBytes(t, profile), ShouldContainSubstring, `model = "gpt-5"`)
				assertPathMode(t, filepath.Join(profile, "config.toml"), 0o600)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "0600")
			})
		})
	})
}

func TestCodexLooseVariables(t *testing.T) {
	Convey("Given a server carrying an unknown host variable", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)
		st := openStore(t)
		pkg := codexPackage(t)

		pkg.MCP = []manifest.MCPServer{{
			Name:    "bad",
			Command: []string{"node", "${FOO_BAR}/server.js"},
		}}

		h, _ := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then it refuses with the variable named and writes nothing", func() {
				typed, ok := errors.AsType[*host.UnsupportedVariableError](err)
				So(ok, ShouldBeTrue)
				So(typed.Variable, ShouldEqual, "FOO_BAR")
				So(fileExists(filepath.Join(home, ".agents")), ShouldBeFalse)
				So(fileExists(filepath.Join(profile, "config.toml")), ShouldBeFalse)
			})
		})
	})

	Convey("Given a server referencing the plugin data dir", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)
		st := openStore(t)
		pkg := codexPackage(t)

		pkg.MCP = []manifest.MCPServer{{
			Name:    "data",
			Command: []string{"node", "${PLUGIN_DATA}/server.js"},
		}}

		h, _ := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then the data dir is created and the config points at it", func() {
				So(err, ShouldBeNil)

				dataDir, pathErr := st.PackageDataPath(pkg.ID, "codex")
				So(pathErr, ShouldBeNil)
				So(fileExists(dataDir), ShouldBeTrue)

				entry := mcpEntry(t, decodeTOML(t, configBytes(t, profile)), "data")
				So(entry["args"], ShouldResemble, []any{dataDir + "/server.js"})
			})
		})
	})
}

func TestCodexLooseConfigPreservation(t *testing.T) {
	Convey("Given an existing config.toml with comments and foreign keys", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)

		existing := `# my codex config
model = "gpt-5"

# a foreign server
[mcp_servers.foreign]
command = "other"
`
		writeFixtureFile(t, filepath.Join(profile, "config.toml"), existing, 0o600)

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})
		h, _ := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets),
			host.WithOwnership(ownerExisting("acme/caveman")))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: codexPackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			after := configBytes(t, profile)

			Convey("Then comments and foreign keys survive and our tables are added", func() {
				So(err, ShouldBeNil)
				So(after, ShouldContainSubstring, "# my codex config")
				So(after, ShouldContainSubstring, "# a foreign server")
				So(after, ShouldContainSubstring, `model = "gpt-5"`)

				doc := decodeTOML(t, after)
				mcpEntry(t, doc, "foreign")
				mcpEntry(t, doc, "fs")

				So(mcpEntry(t, doc, "foreign")["command"], ShouldEqual, "other")
			})

			Convey("Then a second delivery is idempotent for the config", func() {
				second, secondErr := h.Deliver(t.Context(), home, host.Delivery{Package: codexPackage(t), Strategy: host.Loose, AllowHooks: true})

				So(secondErr, ShouldBeNil)
				So(configBytes(t, profile), ShouldEqual, after)
				So(len(second.RMA), ShouldBeLessThan, len(res.RMA))
			})
		})
	})

	Convey("Given a user-owned mcp_servers entry with the same name", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)

		writeFixtureFile(t, filepath.Join(profile, "config.toml"), "[mcp_servers.fs]\ncommand = \"user-owned\"\n", 0o600)

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})
		h, _ := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		before := configBytes(t, profile)

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: codexPackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then the cell is hands-off and nothing is written", func() {
				_, ok := errors.AsType[*render.HandsOffError](err)
				So(ok, ShouldBeTrue)
				So(configBytes(t, profile), ShouldEqual, before)
				So(fileExists(filepath.Join(home, ".agents")), ShouldBeFalse)
			})
		})
	})

	Convey("Given a read-only config.toml", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)

		writeFixtureFile(t, filepath.Join(profile, "config.toml"), "", 0o444)

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})
		h, _ := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: codexPackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then it reports a DeliveryError and writes nothing else", func() {
				typed, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(typed.Step, ShouldEqual, "plan")
				So(fileExists(filepath.Join(home, ".agents")), ShouldBeFalse)
			})
		})
	})
}

func TestCodexLooseDryRun(t *testing.T) {
	Convey("Given a loose dry run", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)
		st := openStore(t)
		before := snapshotHome(t, home)

		h, runner := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: codexPackage(t), Strategy: host.Loose, AllowHooks: true, DryRun: true})

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

	Convey("Given a synth dry run", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		st := openStore(t)

		h, runner := newCodex(t, home, map[string]hostcli.Response{
			"codex plugin marketplace list --json": response(`{"marketplaces": []}`),
		}, host.WithStore(st), host.WithTrash(st.Trash()))
		pkg := codexPackage(t)
		pkg.SynthDir = synthPackage(t, st, pkg.ID, pkg.Version).SynthDir

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Synth, DryRun: true})

		Convey("When it is delivered", func() {
			Convey("Then only the read-only marketplace listing runs and the inverse is planned", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{"codex plugin marketplace list --json"})
				So(fileExists(filepath.Join(home, ".agents")), ShouldBeFalse)
				So(res.RMA, ShouldHaveLength, 2)
				So(res.Notes, ShouldContain, "dry-run")
			})
		})
	})
}

func TestCodexLooseUnicode(t *testing.T) {
	Convey("Given a unicode skill name", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)

		payload := t.TempDir()
		writeFixtureFile(t, filepath.Join(payload, "skills", "κ-caveman", "SKILL.md"), "# κ\n", 0o600)

		pkg := host.Package{
			ID:   "acme/uni",
			Root: payload,
			Components: []manifest.Component{{
				Kind: manifest.KindSkill, Name: "κ-caveman", Path: "skills/κ-caveman",
			}},
		}

		h, _ := newCodex(t, home, nil)

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then the name survives byte-for-byte", func() {
				So(err, ShouldBeNil)
				So(readTestFile(t, filepath.Join(home, ".agents", "skills", "κ-caveman", "SKILL.md")), ShouldEqual, "# κ\n")
			})
		})
	})
}

func TestCodexConcurrentDryRun(t *testing.T) {
	Convey("Given concurrent dry runs of one adapter", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		st := openStore(t)

		h, _ := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})))

		assertConcurrentDryRun(t, h, home, filepath.Join(home, ".agents"), func() host.Package { return codexPackage(t) })
	})
}

func TestCodexNativeDeliver(t *testing.T) {
	Convey("Given a scripted native install", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		t.Setenv("CODEX_HOME", "")

		ref := "https://github.com/acme/plugins.git"

		script := map[string]hostcli.Response{
			"codex plugin marketplace add " + ref + " --json": response(`{"marketplaceName":"plugins","installedRoot":"/c/plugins","alreadyAdded":false}`),
			"codex plugin add caveman@plugins":                response(""),
			"codex plugin list --json":                        response(codexList("caveman@plugins")),
		}

		h, runner := newCodex(t, home, script)

		pkg := codexPackage(t)
		pkg.Marketplace = ref

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered", func() {
			Convey("Then the codex-cli 0.157.1 argv runs and the oracle verifies", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{
					"codex plugin marketplace add " + ref + " --json",
					"codex plugin add caveman@plugins",
					"codex plugin list --json",
				})
				So(res.Observed.Verified, ShouldBeTrue)
			})

			Convey("Then the RMA carries the inverse argv", func() {
				So(res.RMA, ShouldResemble, []receipt.Op{
					{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "remove", "plugins"}},
					{Kind: receipt.OpHostInstall, Command: []string{"plugin", "remove", "caveman@plugins"}},
				})
			})
		})
	})

	Convey("Given a marketplace whose document declares another name than the ref's last segment", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		t.Setenv("CODEX_HOME", "")

		ref := "https://github.com/acme/plugins.git"

		h, runner := newCodex(t, home, map[string]hostcli.Response{
			"codex plugin marketplace add " + ref + " --json": response(`{"marketplaceName":"acme-tools","installedRoot":"/c/x","alreadyAdded":false}`),
			"codex plugin add caveman@acme-tools":             response(""),
			"codex plugin list --json":                        response(codexList("caveman@acme-tools")),
		})

		pkg := codexPackage(t)
		pkg.Marketplace = ref

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered", func() {
			Convey("Then the name the host registered is the one installed and recorded", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldContain, "codex plugin add caveman@acme-tools")
				So(res.RMA[1].Command, ShouldResemble, []string{"plugin", "remove", "caveman@acme-tools"})
			})
		})
	})
}

// codexSynthWorld is one Codex home over the stateful fake CLI with a store
// for synth packages.
func codexSynthWorld(t *testing.T) (host.Host, *codexCLI, *store.Store, string) {
	t.Helper()

	fakeCodex(t)
	t.Setenv("CODEX_HOME", "")

	home := t.TempDir()
	st := openStore(t)
	cli := newCodexCLI()

	return host.NewCodex(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash())), cli, st, home
}

// TestCodexSynthDeliver pins the codex-cli 0.157.1 synth grammar (T1.7-G):
// the owner root is added as a local marketplace (Codex reads the owner
// document at .claude-plugin/marketplace.json), the plugin is added as
// name@owner, and a new version is the same `plugin add` again.
func TestCodexSynthDeliver(t *testing.T) {
	Convey("Given a synth package acme/caveman in the store", t, func() {
		h, cli, st, home := codexSynthWorld(t)
		pkg := synthPackage(t, st, "acme/caveman", "1.0.0")
		root := ownerRoot(st, "acme")

		Convey("When it is delivered", func() {
			res, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})
			So(err, ShouldBeNil)

			Convey("Then the owner marketplace is added and the plugin installed as caveman@acme", func() {
				So(cli.Calls(), ShouldResemble, []string{
					"codex plugin marketplace list --json",
					"codex plugin marketplace add " + root + " --json",
					"codex plugin add caveman@acme",
					"codex plugin list --json",
				})

				version, installed := cli.Installed("caveman@acme")
				So(installed, ShouldBeTrue)
				So(version, ShouldEqual, "1.0.0")
				So(res.Observed.Verified, ShouldBeTrue)
				So(ownerDoc(t, st, "acme"), ShouldEqualJSON,
					`{"name":"acme","owner":{"name":"acme"},"plugins":[{"name":"caveman","source":"./caveman/1.0.0"}]}`)
			})

			Convey("Then the RMA removes the plugin, then the marketplace, and the personal marketplace is never written", func() {
				So(res.RMA, ShouldResemble, []receipt.Op{
					{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "remove", "acme"}},
					{Kind: receipt.OpHostInstall, Command: []string{"plugin", "remove", "caveman@acme"}},
				})
				So(res.Artifacts, ShouldBeEmpty)
				So(fileExists(filepath.Join(home, ".agents")), ShouldBeFalse)
			})

			Convey("Then a new version is added again and the host moves to it", func() {
				next := synthPackage(t, st, "acme/caveman", "1.1.0")

				_, nextErr := h.Deliver(t.Context(), "", host.Delivery{Package: next, Strategy: host.Synth})
				So(nextErr, ShouldBeNil)

				version, _ := cli.Installed("caveman@acme")
				So(version, ShouldEqual, "1.1.0")
				So(ownerDoc(t, st, "acme"), ShouldContainSubstring, `"./caveman/1.1.0"`)
			})

			Convey("Then a host that keeps the old version fails verify, never a silent success", func() {
				cli.stale = true
				next := synthPackage(t, st, "acme/caveman", "1.1.0")

				nextRes, nextErr := h.Deliver(t.Context(), "", host.Delivery{Package: next, Strategy: host.Synth})

				typed, ok := errors.AsType[*host.DeliveryError](nextErr)
				So(ok, ShouldBeTrue)
				So(typed.Step, ShouldEqual, "verify")
				So(nextRes.RMA, ShouldHaveLength, 2)
			})
		})

		Convey("When it is only planned", func() {
			res, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth, DryRun: true})

			Convey("Then only the read-only listing runs and nothing is written", func() {
				So(err, ShouldBeNil)
				So(cli.Calls(), ShouldResemble, []string{"codex plugin marketplace list --json"})
				So(res.RMA, ShouldHaveLength, 2)
				So(fileExists(filepath.Join(root, ".claude-plugin", "marketplace.json")), ShouldBeFalse)
			})
		})
	})
}

// TestCodexSynthMarketplaceCollision pins the F3 collision rule on Codex: a
// marketplace named after the owner from another source is foreign (codex
// refuses to re-add a name from a different source), so verger's owner
// marketplace becomes <owner>-verger.
func TestCodexSynthMarketplaceCollision(t *testing.T) {
	Convey("Given a user's own marketplace named acme", t, func() {
		h, cli, st, _ := codexSynthWorld(t)
		cli.marketplaces["acme"] = t.TempDir()

		pkg := synthPackage(t, st, "acme/caveman", "1.0.0")

		Convey("When the synth package is delivered", func() {
			res, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})

			Convey("Then it lands as caveman@acme-verger and the RMA names it", func() {
				So(err, ShouldBeNil)

				_, installed := cli.Installed("caveman@acme-verger")
				So(installed, ShouldBeTrue)
				So(res.RMA[0].Command, ShouldResemble, []string{"plugin", "marketplace", "remove", "acme-verger"})
			})
		})

		Convey("When acme-verger is foreign too", func() {
			cli.marketplaces["acme-verger"] = t.TempDir()

			_, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})

			Convey("Then the cell is hands-off after the read-only listing", func() {
				_, ok := errors.AsType[*render.HandsOffError](err)
				So(ok, ShouldBeTrue)
				So(cli.Calls(), ShouldResemble, []string{"codex plugin marketplace list --json"})
			})
		})
	})
}

// TestCodexSynthRemoveAndRollback pins removal on codex-cli 0.157.1: `plugin
// remove` then — only once no installed plugin comes from it — `marketplace
// remove`; a failed install is rolled back without leaving the marketplace
// registered.
func TestCodexSynthRemoveAndRollback(t *testing.T) {
	Convey("Given two packages of one owner installed", t, func() {
		h, cli, st, _ := codexSynthWorld(t)

		first := synthPackage(t, st, "acme/caveman", "1.0.0")
		second := synthPackage(t, st, "acme/other", "2.0.0")

		firstRes, err := h.Deliver(t.Context(), "", host.Delivery{Package: first, Strategy: host.Synth})
		So(err, ShouldBeNil)

		secondRes, err := h.Deliver(t.Context(), "", host.Delivery{Package: second, Strategy: host.Synth})
		So(err, ShouldBeNil)

		uninstall := func(pkg host.Package, rma []receipt.Op) host.Result {
			res, uninstallErr := h.Uninstall(t.Context(), "", receipt.Receipt{
				Package: pkg.ID, Host: "codex", Scope: receipt.ScopeUser, Strategy: string(host.Synth), RMA: rma,
			})
			So(uninstallErr, ShouldBeNil)

			return res
		}

		Convey("When the first is uninstalled", func() {
			res := uninstall(first, firstRes.RMA)

			Convey("Then the marketplace stays for the other plugin", func() {
				_, registered := cli.Registered("acme")
				So(registered, ShouldBeTrue)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "still serves 1")

				_, installed := cli.Installed("caveman@acme")
				So(installed, ShouldBeFalse)
			})

			Convey("Then the last one takes the marketplace with it", func() {
				uninstall(second, secondRes.RMA)

				_, registered := cli.Registered("acme")
				So(registered, ShouldBeFalse)
			})
		})
	})

	Convey("Given a synth install the host refuses", t, func() {
		h, cli, st, _ := codexSynthWorld(t)
		cli.fail["plugin add caveman@acme"] = hostcli.Response{Code: 1, Stderr: "Error: install failed"}

		deps := applyWorld(t, st, ownerMap{})
		deps.Hosts[host.Codex] = h

		cell := applyCell(t, deps, apply.Action{
			Kind: apply.ActionInstall, Host: host.Codex,
			Delivery: host.Delivery{Package: synthPackage(t, st, "acme/caveman", "1.0.0"), Strategy: host.Synth},
		})

		Convey("When apply installs and rolls back", func() {
			Convey("Then the cell fails and the owner marketplace is not left registered", func() {
				So(cell.Status, ShouldEqual, apply.StatusFailed)

				_, registered := cli.Registered("acme")
				So(registered, ShouldBeFalse)
				So(strings.Join(cell.Notes, "\n"), ShouldNotContainSubstring, "rollback:")
			})
		})
	})
}

// TestCodexSynthLeavesPersonalMarketplace pins that synth never touches
// ~/.agents/plugins/marketplace.json: codex-cli 0.157.1 reads it implicitly,
// and the object-map entry verger once wrote there made `codex plugin
// marketplace list` fail ("invalid type: map, expected a sequence").
func TestCodexSynthLeavesPersonalMarketplace(t *testing.T) {
	Convey("Given a user's personal marketplace document", t, func() {
		h, _, st, home := codexSynthWorld(t)

		personal := filepath.Join(home, ".agents", "plugins", "marketplace.json")

		const document = `{"name":"personal","plugins":[{"name":"mine","source":{"source":"local","path":"./mine"}}]}`
		writeFixtureFile(t, personal, document, 0o600)

		_, err := h.Deliver(t.Context(), "", host.Delivery{Package: synthPackage(t, st, "acme/caveman", "1.0.0"), Strategy: host.Synth})

		Convey("When a synth package is delivered", func() {
			Convey("Then the document is byte-identical", func() {
				So(err, ShouldBeNil)
				So(readTestFile(t, personal), ShouldEqual, document)
			})
		})
	})
}

// TestCodexSynthFailedAddReturnsRMA pins NF-5 on the rewritten Codex install:
// a refused `plugin add` returns the error together with the inverse RMA.
func TestCodexSynthFailedAddReturnsRMA(t *testing.T) {
	Convey("Given a host that refuses the plugin add", t, func() {
		h, cli, st, _ := codexSynthWorld(t)
		cli.fail["plugin add caveman@acme"] = hostcli.Response{Code: 1, Stderr: "Error: install failed"}

		res, err := h.Deliver(t.Context(), "", host.Delivery{Package: synthPackage(t, st, "acme/caveman", "1.0.0"), Strategy: host.Synth})

		Convey("When the adapter delivers", func() {
			Convey("Then the error carries the executed RMA", func() {
				So(err, ShouldNotBeNil)
				So(res.RMA, ShouldHaveLength, 2)
			})
		})
	})
}

func TestCodexInstallRefusals(t *testing.T) {
	Convey("Given install requests without their inputs", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		t.Setenv("CODEX_HOME", "")

		h, runner := newCodex(t, home, nil)

		Convey("When a native install has no marketplace", func() {
			_, err := h.Deliver(t.Context(), home, host.Delivery{Package: codexPackage(t), Strategy: host.Native})

			Convey("Then it reports *NotSupportedError", func() {
				_, ok := errors.AsType[*host.NotSupportedError](err)
				So(ok, ShouldBeTrue)
				So(runner.Calls(), ShouldBeEmpty)
			})
		})

		Convey("When a synth install has no synth dir", func() {
			_, err := h.Deliver(t.Context(), home, host.Delivery{Package: codexPackage(t), Strategy: host.Synth})

			Convey("Then it reports *NotSupportedError", func() {
				_, ok := errors.AsType[*host.NotSupportedError](err)
				So(ok, ShouldBeTrue)
				So(runner.Calls(), ShouldBeEmpty)
			})
		})

		Convey("When Silenced is requested", func() {
			_, err := h.Deliver(t.Context(), home, host.Delivery{Package: codexPackage(t), Strategy: host.Silenced})

			Convey("Then the adapter refuses the strategy", func() {
				_, ok := errors.AsType[*host.UnsupportedStrategyError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestCodexNativeVerifyFailure(t *testing.T) {
	Convey("Given an oracle that does not list the plugin", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		t.Setenv("CODEX_HOME", "")

		ref := "https://github.com/acme/plugins.git"

		h, runner := newCodex(t, home, map[string]hostcli.Response{
			"codex plugin marketplace add " + ref + " --json": response(`{"marketplaceName":"plugins"}`),
			"codex plugin add caveman@plugins":                response(""),
			"codex plugin list --json":                        response(codexList()),
		})

		pkg := codexPackage(t)
		pkg.Marketplace = ref

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered", func() {
			Convey("Then verify fails with a *DeliveryError, never a silent step down", func() {
				typed, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(typed.Step, ShouldEqual, "verify")
				So(runner.Calls(), ShouldHaveLength, 3)
				So(fileExists(filepath.Join(home, ".agents")), ShouldBeFalse)
			})
		})
	})
}

func TestCodexPolicyEveryStratum(t *testing.T) {
	ref := "https://github.com/acme/plugins.git"

	Convey("Given a Codex config blocking the marketplace", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)

		writeFixtureFile(t, filepath.Join(profile, "config.toml"),
			"# enterprise policy\nblockedMarketplaces = [\""+ref+"\"]\n", 0o600)

		for _, strategy := range []host.Strategy{host.Native, host.Synth, host.Loose} {
			Convey("When the strategy "+string(strategy)+" is delivered", func() {
				h, runner := newCodex(t, home, nil)

				pkg := codexPackage(t)
				pkg.Marketplace = ref
				pkg.SynthDir = t.TempDir()

				_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: strategy})

				Convey("Then the policy blocks it with no call and no write", func() {
					typed, ok := errors.AsType[*host.PolicyError](err)
					So(ok, ShouldBeTrue)
					So(typed.Host, ShouldEqual, host.Codex)
					So(typed.Rule, ShouldEqual, "blockedMarketplaces")
					So(typed.Ref, ShouldEqual, ref)
					So(runner.Calls(), ShouldBeEmpty)
					So(fileExists(filepath.Join(home, ".agents")), ShouldBeFalse)
				})
			})
		}
	})

	Convey("Given strictKnownMarketplaces not listing the ref", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)

		writeFixtureFile(t, filepath.Join(profile, "config.toml"),
			"strictKnownMarketplaces = [\"https://github.com/other/marketplace.git\"]\n", 0o600)

		h, runner := newCodex(t, home, nil)
		pkg := codexPackage(t)
		pkg.Marketplace = ref

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered", func() {
			Convey("Then the strict allowlist blocks it", func() {
				typed, ok := errors.AsType[*host.PolicyError](err)
				So(ok, ShouldBeTrue)
				So(typed.Rule, ShouldEqual, "strictKnownMarketplaces")
				So(runner.Calls(), ShouldBeEmpty)
			})
		})
	})
}

func TestCodexUninstall(t *testing.T) {
	Convey("Given a receipt with host-install RMA ops", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		t.Setenv("CODEX_HOME", "")

		h, runner := newCodex(t, home, map[string]hostcli.Response{
			"codex plugin remove caveman@plugins":     response(""),
			"codex plugin list --json":                response(codexList()),
			"codex plugin marketplace remove plugins": response(""),
		})

		r := receipt.Receipt{
			Package:  "acme/caveman",
			Host:     "codex",
			Scope:    receipt.ScopeUser,
			Strategy: string(host.Native),
			RMA: []receipt.Op{
				{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "remove", "plugins"}},
				{Kind: receipt.OpHostInstall, Command: []string{"plugin", "remove", "caveman@plugins"}},
				{Kind: receipt.OpWriteFile, Path: filepath.Join(home, "file"), Digest: "aa"},
			},
		}

		Convey("When it is uninstalled", func() {
			res, err := h.Uninstall(t.Context(), home, r)

			Convey("Then host ops run in reverse order, the marketplace after an empty refcount", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{
					"codex plugin remove caveman@plugins",
					"codex plugin list --json",
					"codex plugin marketplace remove plugins",
				})
				So(res.Strategy, ShouldEqual, host.Native)
			})
		})

		Convey("When another plugin still comes from the marketplace", func() {
			runner.Set("codex plugin list --json", response(codexList("other@plugins")))

			res, err := h.Uninstall(t.Context(), home, r)

			Convey("Then the marketplace is kept with a note (§4.8 refcount)", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldNotContain, "codex plugin marketplace remove plugins")
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "still serves 1")
			})
		})

		Convey("When the oracle cannot list", func() {
			runner.Set("codex plugin list --json", hostcli.Response{Code: 2, Stderr: "boom"})

			res, err := h.Uninstall(t.Context(), home, r)

			Convey("Then the marketplace is kept rather than pulled from under unknown plugins", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldNotContain, "codex plugin marketplace remove plugins")
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "kept")
			})
		})

		Convey("When the marketplace is no longer configured", func() {
			runner.Set("codex plugin marketplace remove plugins",
				hostcli.Response{Code: 1, Stderr: "Error: marketplace `plugins` is not configured or installed"})

			res, err := h.Uninstall(t.Context(), home, r)

			Convey("Then it is already removed, with a note", func() {
				So(err, ShouldBeNil)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "was not configured")
			})
		})
	})

	Convey("Given a receipt recorded before the codex-cli 0.157.1 grammar was verified", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		t.Setenv("CODEX_HOME", "")

		h, runner := newCodex(t, home, map[string]hostcli.Response{
			"codex plugin remove caveman@acme":     response(""),
			"codex plugin list --json":             response(codexList()),
			"codex plugin marketplace remove acme": response(""),
		})

		_, err := h.Uninstall(t.Context(), home, receipt.Receipt{
			Package: "acme/caveman", Host: "codex", Scope: receipt.ScopeUser, Strategy: string(host.Synth),
			RMA: []receipt.Op{
				{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "rm", "acme"}},
				{Kind: receipt.OpHostInstall, Command: []string{"plugin", "uninstall", "caveman@acme"}},
			},
		})

		Convey("When it is uninstalled", func() {
			Convey("Then the legacy verbs run as the real ones", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{
					"codex plugin remove caveman@acme",
					"codex plugin list --json",
					"codex plugin marketplace remove acme",
				})
			})
		})
	})
}

// TestCodexUninstallKeepsExistedResources pins the Host.Deliver contract on the
// codex host-install path: an op marked Existed predates the delivery, so its
// inverse is a no-op with a note and no CLI call — the plugin and the
// marketplace stay. (This adapter records Existed=false on the plugin path:
// `plugin add` is the delivery's own target and is idempotent, so `plugin
// remove` is the right inverse, exactly as the claude adapter treats it.)
func TestCodexUninstallKeepsExistedResources(t *testing.T) {
	Convey("Given a receipt whose host-install ops predate the delivery", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		t.Setenv("CODEX_HOME", "")

		h, runner := newCodex(t, home, nil)

		res, err := h.Uninstall(t.Context(), home, receipt.Receipt{
			Package: "acme/caveman", Host: "codex", Scope: receipt.ScopeUser, Strategy: string(host.Native),
			RMA: []receipt.Op{
				{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "remove", "acme"}, Existed: true},
				{Kind: receipt.OpHostInstall, Command: []string{"plugin", "remove", "caveman@acme"}, Existed: true},
			},
		})

		Convey("When it is uninstalled", func() {
			Convey("Then both resources are kept, noted, and no host call runs", func() {
				So(err, ShouldBeNil)
				So(runner.Calls(), ShouldBeEmpty)
				So(res.Notes, ShouldHaveLength, 2)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "existed before this delivery; kept")
			})
		})
	})
}

func TestCodexLooseZeroByteConfig(t *testing.T) {
	Convey("Given zero-byte config and hooks files", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		profile := codexProfile(t)
		useCodexHome(t, profile)

		writeFixtureFile(t, filepath.Join(profile, "config.toml"), "", 0o600)
		writeFixtureFile(t, filepath.Join(profile, "hooks.json"), "", 0o600)

		st := openStore(t)
		secrets := secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token"})
		h, _ := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(secrets))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: codexPackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then both files gain our content", func() {
				So(err, ShouldBeNil)
				So(configBytes(t, profile), ShouldContainSubstring, "[mcp_servers.fs]")
				So(readTestFile(t, filepath.Join(profile, "hooks.json")), ShouldContainSubstring, "PreToolUse")
			})
		})
	})
}
