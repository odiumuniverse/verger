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
	"github.com/odiumuniverse/verger/pkg/digest"
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

		h, runner := newCodex(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))
		pkg := codexPackage(t)
		pkg.SynthDir = synthPackage(t, st, pkg.ID, pkg.Version).SynthDir

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Synth, DryRun: true})

		Convey("When it is delivered", func() {
			Convey("Then no call happens and the marketplace entry is only planned", func() {
				So(err, ShouldBeNil)
				So(runner.Calls(), ShouldBeEmpty)
				So(fileExists(filepath.Join(home, ".agents", "plugins", "marketplace.json")), ShouldBeFalse)
				So(res.RMA, ShouldHaveLength, 3)
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
			"codex plugin marketplace add " + ref:  response(""),
			"codex plugin install caveman@plugins": response(""),
			"codex plugin list --json":             response(matchingList("caveman")),
		}

		h, runner := newCodex(t, home, script)

		pkg := codexPackage(t)
		pkg.Marketplace = ref

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered", func() {
			Convey("Then the exact argv runs and the oracle verifies", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{
					"codex plugin marketplace add " + ref,
					"codex plugin install caveman@plugins",
					"codex plugin list --json",
				})
				So(res.Observed.Verified, ShouldBeTrue)
			})

			Convey("Then the RMA carries the inverse argv", func() {
				So(res.RMA, ShouldResemble, []receipt.Op{
					{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "rm", "plugins"}},
					{Kind: receipt.OpHostInstall, Command: []string{"plugin", "uninstall", "caveman@plugins"}},
				})
			})
		})
	})
}

func TestCodexSynthDeliver(t *testing.T) {
	Convey("Given a scripted synth install", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		t.Setenv("CODEX_HOME", "")

		st := openStore(t)
		pkg := codexPackage(t)
		synthDir := synthPackage(t, st, pkg.ID, pkg.Version).SynthDir
		root := ownerRoot(st, "acme")

		script := map[string]hostcli.Response{
			"codex plugin marketplace add " + root: response(""),
			"codex plugin install caveman@acme":    response(""),
			"codex plugin list --json":             response(matchingList("caveman")),
		}

		h, runner := newCodex(t, home, script)

		pkg.SynthDir = synthDir

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Synth})

		Convey("When it is delivered", func() {
			marketplace := filepath.Join(home, ".agents", "plugins", "marketplace.json")

			Convey("Then the owner marketplace is added and the plugin installed as caveman@acme (F3)", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{
					"codex plugin marketplace add " + root,
					"codex plugin install caveman@acme",
					"codex plugin list --json",
				})
				So(ownerDoc(t, st, "acme"), ShouldEqualJSON, `{"name":"acme","owner":{"name":"acme"},"plugins":[
  {"name":"caveman","source":"./caveman/`+pkg.Version+`"}]}`)
			})

			Convey("Then the personal marketplace entry points at the package dir", func() {
				So(err, ShouldBeNil)
				So(readTestFile(t, marketplace), ShouldEqualJSON, `{
  "plugins": {
    "caveman": {"source": {"source": "local", "path": "`+synthDir+`"}}
  }
}`)
			})

			Convey("Then the RMA removes the entry, the marketplace and the install", func() {
				entryDigest := digest.Bytes([]byte(`{"source":{"path":"` + synthDir + `","source":"local"}}`))

				So(res.RMA, ShouldResemble, []receipt.Op{
					{
						Kind: receipt.OpConfigKey, Path: marketplace, KeyPath: "plugins.caveman",
						Digest: entryDigest, Existed: false,
					},
					{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "rm", "acme"}},
					{Kind: receipt.OpHostInstall, Command: []string{"plugin", "uninstall", "caveman@acme"}},
				})
			})

			Convey("Then the result carries the marketplace entry artifact that landed", func() {
				So(res.Artifacts, ShouldResemble, []receipt.Artifact{{
					Kind: "marketplace", Name: "caveman", Path: marketplace, Digest: res.RMA[0].Digest,
				}})
			})

			Convey("Then a second synth keeps foreign entries and stays idempotent", func() {
				foreign := `{"name": "personal", "plugins": {"other": {"source": {"source": "local", "path": "/tmp/other"}}}}`
				writeFixtureFile(t, marketplace, foreign, 0o600)

				_, secondErr := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Synth})

				So(secondErr, ShouldBeNil)

				after := readTestFile(t, marketplace)
				So(after, ShouldContainSubstring, `"other"`)
				So(after, ShouldContainSubstring, `"personal"`)

				_, thirdErr := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Synth})
				So(thirdErr, ShouldBeNil)
				So(readTestFile(t, marketplace), ShouldEqual, after)
			})
		})
	})
}

// codexReplacedEntry is the older personal marketplace entry a synth update
// replaces, next to a foreign entry.
const codexReplacedEntry = `{"plugins":{` +
	`"caveman":{"source":{"source":"local","path":"/old-store"}},` +
	`"other":{"source":{"source":"local","path":"/tmp/other"}}}}`

// codexReplaceSetup pre-writes the older entry and builds a Codex adapter with
// a store and trash whose runner scripts a full synth install and uninstall.
func codexReplaceSetup(t *testing.T) (host.Host, *hostcli.ScriptRunner, *store.Store, host.Package, string) {
	t.Helper()

	fakeCodex(t)

	home := t.TempDir()
	t.Setenv("CODEX_HOME", "")

	marketplace := filepath.Join(home, ".agents", "plugins", "marketplace.json")

	writeFixtureFile(t, marketplace, codexReplacedEntry, 0o600)

	st := openStore(t)
	pkg := codexPackage(t)
	pkg.SynthDir = synthPackage(t, st, pkg.ID, pkg.Version).SynthDir

	h, runner := newCodex(t, home, map[string]hostcli.Response{
		"codex plugin marketplace add " + ownerRoot(st, "acme"): response(""),
		"codex plugin install caveman@acme":                     response(""),
		"codex plugin list --json":                              response(matchingList("caveman")),
		"codex plugin uninstall caveman@acme":                   response(""),
		"codex plugin marketplace rm acme":                      response(""),
	}, host.WithStore(st), host.WithTrash(st.Trash()))

	return h, runner, st, pkg, marketplace
}

// TestCodexSynthReplaceKeepsBackup pins T1.7-review-1 [B1]/[M1]: a synth
// delivery that replaces an existing personal marketplace entry returns the
// RMA and artifacts execution produced — the entry op carries the trash bucket
// of the replaced value, the artifact is the entry that landed.
func TestCodexSynthReplaceKeepsBackup(t *testing.T) {
	Convey("Given an existing caveman entry pointing at an older synth dir", t, func() {
		h, runner, st, pkg, marketplace := codexReplaceSetup(t)
		entryDigest := digest.Bytes([]byte(`{"source":{"path":"` + pkg.SynthDir + `","source":"local"}}`))

		Convey("When synth delivers the new dir", func() {
			res, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})
			So(err, ShouldBeNil)

			Convey("Then the entry op records the trashed previous value", func() {
				So(res.RMA, ShouldHaveLength, 3)
				So(res.RMA[0].Kind, ShouldEqual, receipt.OpConfigKey)
				So(res.RMA[0].KeyPath, ShouldEqual, "plugins.caveman")
				So(res.RMA[0].Digest, ShouldEqual, entryDigest)
				So(res.RMA[0].Existed, ShouldBeTrue)
				So(res.RMA[0].Backup, ShouldNotBeEmpty)
				So(trashedValue(t, st, res.RMA[0].Backup), ShouldEqualJSON, `{"source":{"source":"local","path":"/old-store"}}`)
			})

			Convey("Then the result carries the marketplace entry artifact that landed", func() {
				So(res.Artifacts, ShouldResemble, []receipt.Artifact{{
					Kind: "marketplace", Name: "caveman", Path: marketplace, Digest: entryDigest,
				}})
			})
		})

		Convey("When the replacement is only planned", func() {
			res, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth, DryRun: true})

			Convey("Then the plan names the entry it would replace and nothing is trashed or run", func() {
				So(err, ShouldBeNil)
				So(res.RMA[0].Existed, ShouldBeTrue)
				So(res.RMA[0].Backup, ShouldBeEmpty)
				So(res.Artifacts, ShouldResemble, []receipt.Artifact{{
					Kind: "marketplace", Name: "caveman", Path: marketplace, Digest: entryDigest,
				}})
				So(runner.Calls(), ShouldBeEmpty)
				So(readTestFile(t, marketplace), ShouldEqual, codexReplacedEntry)
				So(mustTrashEntries(t, st), ShouldBeEmpty)
			})
		})
	})
}

// TestCodexSynthReplaceRemoveHasNoResidue pins the removal half of [B1]: the
// receipt of a replacing synth delivery is an exact reverse, so pkg/apply's
// remove is `current` and leaves the document as it was before the delivery —
// the older entry restored, the delivered synth dir gone, the foreign entry
// kept.
func TestCodexSynthReplaceRemoveHasNoResidue(t *testing.T) {
	Convey("Given a synth delivery that replaced an older caveman entry", t, func() {
		h, runner, st, pkg, marketplace := codexReplaceSetup(t)

		res, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})
		So(err, ShouldBeNil)

		rec := receipt.Receipt{
			Schema: receipt.Schema, Package: pkg.ID, Host: string(host.Codex), Scope: receipt.ScopeUser,
			Strategy: string(host.Synth), Version: pkg.Version, Artifacts: res.Artifacts, RMA: res.RMA,
		}

		deps := applyWorld(t, st, ownerExisting(pkg.ID))
		deps.Hosts[host.Codex] = h

		Convey("When apply removes the cell", func() {
			report, runErr := apply.Run(t.Context(), deps, apply.Plan{Actions: []apply.Action{{
				Kind: apply.ActionRemove, Host: host.Codex, Previous: &rec, Cause: "user", Initiator: "codex",
			}}}, apply.Options{})

			Convey("Then the cell is current and the document is back to its pre-delivery state", func() {
				So(runErr, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, apply.StatusCurrent)

				after := readTestFile(t, marketplace)
				So(after, ShouldEqualJSON, codexReplacedEntry)
				So(after, ShouldNotContainSubstring, pkg.SynthDir)
			})

			Convey("Then the host install is undone in reverse order, the marketplace after its refcount", func() {
				calls := callKeys(runner)
				So(calls[len(calls)-3:], ShouldResemble, []string{
					"codex plugin uninstall caveman@acme",
					"codex plugin list --json",
					"codex plugin marketplace rm acme",
				})
			})
		})
	})
}

// TestCodexSynthFailedInstallRollback pins NF-5: when the host install fails
// after the personal marketplace entry was replaced, the error comes with the
// executed RMA (the entry op carries its trash bucket), so apply's rollback
// restores the user's previous value instead of leaving it overwritten.
func TestCodexSynthFailedInstallRollback(t *testing.T) {
	Convey("Given an existing caveman entry and a host that refuses the install", t, func() {
		h, runner, st, pkg, marketplace := codexReplaceSetup(t)
		runner.Set("codex plugin install caveman@acme", hostcli.Response{Code: 1, Stderr: "install failed"})

		Convey("When the adapter delivers", func() {
			res, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})

			Convey("Then the error carries the executed RMA with the entry's backup", func() {
				So(err, ShouldNotBeNil)
				So(res.RMA, ShouldHaveLength, 3)
				So(res.RMA[0].Existed, ShouldBeTrue)
				So(res.RMA[0].Backup, ShouldNotBeEmpty)
			})
		})

		Convey("When apply installs and rolls the failure back", func() {
			deps := applyWorld(t, st, ownerExisting(pkg.ID))
			deps.Hosts[host.Codex] = h

			cell := applyCell(t, deps, apply.Action{
				Kind: apply.ActionInstall, Host: host.Codex,
				Delivery: host.Delivery{Package: pkg, Strategy: host.Synth},
			})

			Convey("Then the user's previous entry is back and nothing is hands-off", func() {
				So(cell.Status, ShouldEqual, apply.StatusFailed)
				So(readTestFile(t, marketplace), ShouldEqualJSON, codexReplacedEntry)
				So(strings.Join(cell.Notes, "\n"), ShouldNotContainSubstring, "no backup recorded")
			})
		})
	})
}

// mustTrashEntries lists the store trash.
func mustTrashEntries(t *testing.T, st *store.Store) []store.Entry {
	t.Helper()

	entries, err := st.Trash().List()
	if err != nil {
		t.Fatalf("trash list: %v", err)
	}

	return entries
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
			"codex plugin marketplace add " + ref:  response(""),
			"codex plugin install caveman@plugins": response(""),
			"codex plugin list --json":             response("[]"),
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
			"codex plugin uninstall caveman@plugins": response(""),
			"codex plugin list --json":               response("[]"),
			"codex plugin marketplace rm plugins":    response(""),
		})

		r := receipt.Receipt{
			Package:  "acme/caveman",
			Host:     "codex",
			Scope:    receipt.ScopeUser,
			Strategy: string(host.Native),
			RMA: []receipt.Op{
				{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "rm", "plugins"}},
				{Kind: receipt.OpHostInstall, Command: []string{"plugin", "uninstall", "caveman@plugins"}},
				{Kind: receipt.OpWriteFile, Path: filepath.Join(home, "file"), Digest: "aa"},
			},
		}

		Convey("When it is uninstalled", func() {
			res, err := h.Uninstall(t.Context(), home, r)

			Convey("Then host ops run in reverse order, the marketplace after an empty refcount", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{
					"codex plugin uninstall caveman@plugins",
					"codex plugin list --json",
					"codex plugin marketplace rm plugins",
				})
				So(res.Strategy, ShouldEqual, host.Native)
			})
		})

		Convey("When another plugin still comes from the marketplace", func() {
			runner.Set("codex plugin list --json", response(`[{"name":"other","marketplace":"plugins"}]`))

			res, err := h.Uninstall(t.Context(), home, r)

			Convey("Then the marketplace is kept with a note (§4.8 refcount)", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldNotContain, "codex plugin marketplace rm plugins")
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "still serves 1")
			})
		})

		Convey("When the oracle cannot list", func() {
			runner.Set("codex plugin list --json", hostcli.Response{Code: 2, Stderr: "boom"})

			res, err := h.Uninstall(t.Context(), home, r)

			Convey("Then the marketplace is kept rather than pulled from under unknown plugins", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldNotContain, "codex plugin marketplace rm plugins")
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "kept")
			})
		})
	})

	Convey("Given a failing codex plugin uninstall", t, func() {
		fakeCodex(t)
		home := t.TempDir()
		t.Setenv("CODEX_HOME", "")

		marketplace := filepath.Join(home, ".agents", "plugins", "marketplace.json")

		writeFixtureFile(t, marketplace, `{"plugins":{"caveman":{"source":{"source":"local","path":"/store/synth"}}}}`, 0o600)

		digest := digest.Bytes([]byte(`{"source":{"path":"/store/synth","source":"local"}}`))

		h, _ := newCodex(t, home, map[string]hostcli.Response{
			"codex plugin uninstall caveman@acme": {Code: 1, Stderr: "no such plugin"},
			"codex plugin list --json":            response("[]"),
			"codex plugin marketplace rm acme":    {Code: 1, Stderr: "no such marketplace"},
		})

		r := receipt.Receipt{
			Package:  "acme/caveman",
			Host:     "codex",
			Scope:    receipt.ScopeUser,
			Strategy: string(host.Synth),
			RMA: []receipt.Op{
				{Kind: receipt.OpConfigKey, Path: marketplace, KeyPath: "plugins.caveman", Digest: digest, Existed: false},
				{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "rm", "acme"}},
				{Kind: receipt.OpHostInstall, Command: []string{"plugin", "uninstall", "caveman@acme"}},
			},
		}

		res, err := h.Uninstall(t.Context(), home, r)

		Convey("When it is uninstalled", func() {
			Convey("Then it falls back to removing the owned marketplace entry", func() {
				So(err, ShouldBeNil)
				So(readTestFile(t, marketplace), ShouldEqualJSON, `{"plugins":{}}`)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "fallback")
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
