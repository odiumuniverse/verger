package host_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
	"github.com/odiumuniverse/verger/pkg/secret"
	"github.com/odiumuniverse/verger/pkg/store"
)

const agyFixtureRoot = "testdata/agy/acme"

// fakeAgy puts an executable `agy` shim at the front of PATH.
func fakeAgy(t *testing.T) {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "agy"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // G306: a fake host CLI must be executable
		t.Fatalf("write shim: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// newAgy builds the Antigravity CLI adapter with a scripted runner.
func newAgy(t *testing.T, home string, script map[string]hostcli.Response, opts ...host.Option) (host.Host, *hostcli.ScriptRunner) {
	t.Helper()

	runner := hostcli.NewScriptRunner(script)
	all := append([]host.Option{host.WithHome(home), host.WithRunner(runner)}, opts...)

	return host.NewAgy(all...), runner
}

// agyPackage parses the Antigravity fixture and adds the rule component the
// manifest format does not carry.
func agyPackage(t *testing.T) host.Package {
	t.Helper()

	return adapterFixturePackage(t, agyFixtureRoot, manifest.FormatClaude)
}

// agyConfigDir is the Antigravity config dir of one test home.
func agyConfigDir(home string) string {
	return filepath.Join(home, ".gemini", "config")
}

// agySecrets is the secret store of the Antigravity fixture: its MCP servers
// reference MCP_TOKEN and WEB_TOKEN.
func agySecrets(t *testing.T) *secret.Store {
	t.Helper()

	return secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token", "WEB_TOKEN": "web-token"})
}

// agySynthPackage lays out one Antigravity plugin in the store: a directory
// with the `plugin.json` beadle's source_antigravity.go reads (name, version,
// description — the schema forbids extra properties).
func agySynthPackage(t *testing.T, st *store.Store, name, version string) host.Package {
	t.Helper()

	dir := t.TempDir()

	writeFixtureFile(t, filepath.Join(dir, "plugin.json"),
		`{"name":"`+name+`","version":"`+version+`","description":"Antigravity toolkit."}`, 0o600)
	writeFixtureFile(t, filepath.Join(dir, "skills", "alpha", "SKILL.md"),
		"---\nname: alpha\ndescription: Alpha.\n---\n\n# Alpha\n", 0o600)

	return host.Package{ID: "acme/" + name, Version: version, SynthDir: dir}
}

// TestAgyDetect pins the beadle detect rule (pkg/agent/agents.go): the
// Antigravity CLI is present when its state dir OR its MCP config exists, or
// when the `agy` binary resolves.
func TestAgyDetect(t *testing.T) {
	Convey("Given a home without Antigravity", t, func() {
		home := t.TempDir()
		t.Setenv("PATH", t.TempDir())

		h, _ := newAgy(t, home, nil)

		Convey("When neither the state dir, the config nor the binary exist", func() {
			Convey("Then Detect is false", func() {
				So(h.Detect(home), ShouldBeFalse)
			})
		})

		Convey("When the ~/.gemini/antigravity-cli state dir exists", func() {
			if err := os.MkdirAll(filepath.Join(home, ".gemini", "antigravity-cli"), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})

		Convey("When only ~/.gemini/config/mcp_config.json exists", func() {
			writeFixtureFile(t, filepath.Join(agyConfigDir(home), "mcp_config.json"), "{}", 0o600)

			Convey("Then Detect is true, the file is shared with the IDE and 2.0", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})

	Convey("Given the agy binary on PATH", t, func() {
		fakeAgy(t)

		home := t.TempDir()
		h, _ := newAgy(t, home, nil)

		Convey("When only the binary exists", func() {
			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})
}

// TestAgyLooseGolden pins the Antigravity user surface beadle verified: agents
// below ~/.gemini/config/agents, MCP servers in ~/.gemini/config/mcp_config.json
// with `serverUrl`, and the shared Agent Skills directory.
func TestAgyLooseGolden(t *testing.T) {
	Convey("Given an Antigravity package and a temp home", t, func() {
		fakeAgy(t)

		home := t.TempDir()
		st := openStore(t)
		pkg := agyPackage(t)

		h, runner := newAgy(t, home, nil,
			host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(agySecrets(t)))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered loose", func() {
			Convey("Then agents, skills, the rule wrapper and mcp_config.json land; nothing else", func() {
				So(err, ShouldBeNil)
				So(homeFiles(t, home), ShouldResemble, []string{
					".agents/skills/alpha/SKILL.md",
					".agents/skills/alpha/scripts/run.sh",
					".agents/skills/beta/SKILL.md",
					".agents/skills/rule-caveman/SKILL.md",
					".gemini/config/agents/reviewer.md",
					".gemini/config/mcp_config.json",
				})
				So(runner.Calls(), ShouldBeEmpty)
			})

			Convey("Then the components Antigravity has no surface for are named", func() {
				notes := strings.Join(res.Notes, "\n")
				So(notes, ShouldContainSubstring, `command "dev" skipped: no commands directory`)
				So(notes, ShouldContainSubstring, "1 hook(s) skipped")
				So(notes, ShouldContainSubstring, "owner-keyed (top-level plugin names")
				So(notes, ShouldContainSubstring, "rule is delivered as a skill wrapper")
			})

			Convey("Then the MCP document uses Antigravity's own dialect: serverUrl, never url or httpUrl", func() {
				So(readTestFile(t, filepath.Join(agyConfigDir(home), "mcp_config.json")), ShouldEqualJSON, `{
  "mcpServers": {
    "fs": {"command": "node", "args": ["`+pkg.Root+`/server.js"], "env": {"TOKEN": "s3cr3t-token"}},
    "web": {"serverUrl": "https://example.com/mcp", "headers": {"Authorization": "Bearer web-token"}}
  }
}`)
			})

			Convey("Then the RMA carries a file op per artifact and no host-install op", func() {
				assertArtifactsBackedByOps(t, res)

				for _, op := range res.RMA {
					So(op.Kind, ShouldNotEqual, receipt.OpHostInstall)
				}
			})
		})
	})
}

// TestAgyMCPForeignKeysPreserved pins that a hand-written key of the shared
// mcp_config.json survives a delivery.
func TestAgyMCPForeignKeysPreserved(t *testing.T) {
	Convey("Given a hand-written mcp_config.json", t, func() {
		fakeAgy(t)

		home := t.TempDir()
		writeFixtureFile(t, filepath.Join(agyConfigDir(home), "mcp_config.json"), `{
  "mcpServers": {"handmade": {"command": "other", "cwd": "/work", "disabled": true}},
  "unrelated": true
}`, 0o600)

		h, _ := newAgy(t, home, nil, host.WithStore(openStore(t)), host.WithSecrets(agySecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: agyPackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then the foreign server and the unrelated key stay exactly as they were", func() {
				So(err, ShouldBeNil)

				doc := readTestFile(t, filepath.Join(agyConfigDir(home), "mcp_config.json"))
				So(doc, ShouldContainSubstring, `"handmade"`)
				So(doc, ShouldContainSubstring, `"cwd": "/work"`)
				So(doc, ShouldContainSubstring, `"disabled": true`)
				So(doc, ShouldContainSubstring, `"unrelated": true`)
			})
		})
	})
}

// TestAgyMCPHandsOff pins the ownership rule: a server key whose value verger
// did not write in this delivery is never overwritten.
func TestAgyMCPHandsOff(t *testing.T) {
	Convey("Given a hand-written entry under a name the package claims", t, func() {
		fakeAgy(t)

		home := t.TempDir()
		writeFixtureFile(t, filepath.Join(agyConfigDir(home), "mcp_config.json"),
			`{"mcpServers": {"web": {"serverUrl": "https://someone-else.test/mcp"}}}`, 0o600)

		h, _ := newAgy(t, home, nil, host.WithStore(openStore(t)), host.WithSecrets(agySecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: agyPackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then the cell is hands-off and the foreign value is untouched", func() {
				hands, ok := errors.AsType[*render.HandsOffError](err)
				So(ok, ShouldBeTrue)
				So(hands.KeyPath, ShouldEqual, "mcpServers.web")
				So(readTestFile(t, filepath.Join(agyConfigDir(home), "mcp_config.json")),
					ShouldEqual, `{"mcpServers": {"web": {"serverUrl": "https://someone-else.test/mcp"}}}`)
			})
		})
	})
}

func TestAgyLooseCollisions(t *testing.T) {
	Convey("Given a home with hand-written Antigravity surfaces", t, func() {
		fakeAgy(t)

		cases := []struct {
			name    string
			target  func(home string) string
			content string
		}{
			{
				name:    "a subagent file",
				target:  func(home string) string { return filepath.Join(agyConfigDir(home), "agents", "reviewer.md") },
				content: "someone else's agent\n",
			},
			{
				name: "a shared skill tree",
				target: func(home string) string {
					return filepath.Join(home, ".agents", "skills", "alpha")
				},
				content: "someone else's skill\n",
			},
		}

		for _, tc := range cases {
			Convey("When "+tc.name+" is in the way", func() {
				home := t.TempDir()
				foreign := tc.target(home)

				if strings.HasSuffix(foreign, ".md") {
					writeFixtureFile(t, foreign, tc.content, 0o600)
				} else {
					if err := os.MkdirAll(foreign, 0o700); err != nil {
						t.Fatalf("mkdir: %v", err)
					}

					writeFixtureFile(t, filepath.Join(foreign, "SKILL.md"), tc.content, 0o600)
				}

				h, _ := newAgy(t, home, nil, host.WithStore(openStore(t)), host.WithSecrets(agySecrets(t)))

				_, err := h.Deliver(t.Context(), home, host.Delivery{Package: agyPackage(t), Strategy: host.Loose})

				Convey("Then the foreign path is untouched and the cell is a collision", func() {
					collision, ok := errors.AsType[*host.CollisionError](err)
					So(ok, ShouldBeTrue)
					So(collision.Path, ShouldEqual, foreign)

					if strings.HasSuffix(foreign, ".md") {
						So(readTestFile(t, foreign), ShouldEqual, tc.content)
					} else {
						So(readTestFile(t, filepath.Join(foreign, "SKILL.md")), ShouldEqual, tc.content)
					}
				})
			})
		}
	})
}

// TestAgyNativeDeliver pins the `agy plugin install <dir>` grammar beadle drives
// (pkg/engine/bundles.go) and the filesystem oracle it verifies against: the
// host links the plugin into one of its customization roots.
func TestAgyNativeDeliver(t *testing.T) {
	Convey("Given an Antigravity plugin in the store", t, func() {
		fakeAgy(t)

		home := t.TempDir()
		st := openStore(t)
		pkg := agySynthPackage(t, st, "caveman", "1.2.3")

		link := filepath.Join(home, ".gemini", "antigravity-cli", "plugins", "caveman")
		linkAgyPlugin(t, link, pkg.SynthDir)

		h, runner := newAgy(t, home, map[string]hostcli.Response{
			"agy plugin install " + pkg.SynthDir: response("Installed " + pkg.SynthDir),
		})

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered natively", func() {
			Convey("Then the host is asked to install the plugin dir and the receipt carries the inverse", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{"agy plugin install " + pkg.SynthDir})
				So(res.RMA, ShouldHaveLength, 1)
				So(res.RMA[0].Kind, ShouldEqual, receipt.OpHostInstall)
				So(res.RMA[0].Command, ShouldResemble, []string{"plugin", "uninstall", "caveman"})
			})

			Convey("Then the delivery is verified through the customization roots", func() {
				So(res.Observed.Verified, ShouldBeTrue)
				So(res.Observed.Listed, ShouldHaveLength, 1)
				So(res.Observed.Listed[0].Name, ShouldEqual, "caveman")
				So(res.Observed.Listed[0].Version, ShouldEqual, "1.2.3")
			})
		})
	})
}

// TestAgyNativeVerifyFailure pins that a host which did not link the plugin
// fails the cell instead of silently stepping down.
func TestAgyNativeVerifyFailure(t *testing.T) {
	Convey("Given a host that links nothing", t, func() {
		fakeAgy(t)

		home := t.TempDir()
		st := openStore(t)
		pkg := agySynthPackage(t, st, "caveman", "1.2.3")

		h, _ := newAgy(t, home, map[string]hostcli.Response{
			"agy plugin install " + pkg.SynthDir: response("Installed " + pkg.SynthDir),
		})

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When the delivery is verified", func() {
			Convey("Then it fails as a delivery error", func() {
				delivery, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(delivery.Host, ShouldEqual, "agy")
				So(delivery.Step, ShouldEqual, "verify")
			})
		})
	})
}

// TestAgyNativeDryRun pins that planning a native delivery runs no host call.
func TestAgyNativeDryRun(t *testing.T) {
	Convey("Given an Antigravity plugin in the store", t, func() {
		fakeAgy(t)

		home := t.TempDir()
		st := openStore(t)
		pkg := agySynthPackage(t, st, "caveman", "1.2.3")

		h, runner := newAgy(t, home, nil)

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native, DryRun: true})

		Convey("When the delivery is a dry run", func() {
			Convey("Then the host is never called and only the inverse is planned", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldBeEmpty)
				So(res.Notes, ShouldResemble, []string{"dry-run"})
				So(res.RMA, ShouldHaveLength, 1)
			})
		})
	})
}

// TestAgyUninstall pins the RMA loop of a native receipt.
func TestAgyUninstall(t *testing.T) {
	Convey("Given a native agy receipt", t, func() {
		fakeAgy(t)

		home := t.TempDir()
		plugin := t.TempDir()
		writeFixtureFile(t, filepath.Join(plugin, "plugin.json"),
			`{"name":"caveman","version":"1.2.3"}`, 0o600)
		linkAgyPlugin(t, filepath.Join(agyConfigDir(home), "plugins", "caveman"), plugin)

		h, runner := newAgy(t, home, map[string]hostcli.Response{
			"agy plugin uninstall caveman": response("Uninstalled caveman"),
		})

		res, err := h.Uninstall(t.Context(), home, receipt.Receipt{
			Strategy: string(host.Native),
			RMA: []receipt.Op{
				{Kind: receipt.OpHostInstall, Command: []string{"plugin", "uninstall", "caveman"}},
			},
		})

		Convey("When it is uninstalled", func() {
			Convey("Then the recorded inverse is what the host is asked", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{"agy plugin uninstall caveman"})
				So(res.Notes, ShouldBeEmpty)
			})
		})
	})
}

// TestAgyOracleList pins the oracle beadle's plugin reader uses: every
// customization root, first root wins, and a directory without plugin.json is
// not a plugin.
func TestAgyOracleList(t *testing.T) {
	Convey("Given plugins in two customization roots and a stray directory", t, func() {
		fakeAgy(t)

		home := t.TempDir()

		writeFixtureFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "plugins", "one", "plugin.json"),
			`{"name":"one","version":"1.0.0","description":"First."}`, 0o600)
		writeFixtureFile(t, filepath.Join(home, ".gemini", "config", "plugins", "two", "plugin.json"),
			`{"name":"two","version":"2.0.0"}`, 0o600)
		writeFixtureFile(t, filepath.Join(home, ".agents", "plugins", "stray", "README.md"), "not a plugin\n", 0o600)

		h, runner := newAgy(t, home, nil)

		listed, err := h.Oracle().List(t.Context())

		Convey("When the oracle is asked", func() {
			Convey("Then every real plugin is reported and the stray directory is ignored", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldBeEmpty)
				So(listed, ShouldHaveLength, 2)

				names := make([]string, 0, len(listed))
				for _, entry := range listed {
					names = append(names, entry.Name)
				}

				So(names, ShouldResemble, []string{"one", "two"})
			})
		})
	})
}

// TestAgyOracleValidate pins the offline validate fallback: the CLI surface
// is unverified, so a synth directory is validated by parsing its manifest.
func TestAgyOracleValidate(t *testing.T) {
	Convey("Given an agy plugin directory", t, func() {
		fakeAgy(t)

		home := t.TempDir()
		st := openStore(t)
		pkg := agySynthPackage(t, st, "caveman", "1.2.3")

		h, runner := newAgy(t, home, nil)

		warnings, err := h.Oracle().Validate(t.Context(), pkg.SynthDir)

		Convey("When the directory is validated", func() {
			Convey("Then the manifest parser answers and no CLI call is made", func() {
				So(err, ShouldBeNil)
				So(warnings, ShouldBeEmpty)
				So(callKeys(runner), ShouldBeEmpty)
			})
		})
	})
}

// linkAgyPlugin links a plugin dir into a customization root the way
// `agy plugin install` does.
func linkAgyPlugin(t *testing.T, link, dir string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.Symlink(dir, link); err != nil {
		t.Fatalf("link: %v", err)
	}
}

// TestAgyLooseDryRunWritesNothing pins the dry-run contract for a document that
// has to be created: the plan is returned, and neither the skills tree, the
// subagent file nor the MCP document the delivery would write exists.
func TestAgyLooseDryRunWritesNothing(t *testing.T) {
	Convey("Given an Antigravity package and a temp home", t, func() {
		fakeAgy(t)

		home := t.TempDir()
		h, _ := newAgy(t, home, nil, host.WithStore(openStore(t)), host.WithSecrets(agySecrets(t)))

		res, err := h.Deliver(t.Context(), home, host.Delivery{
			Package:  agyPackage(t),
			Strategy: host.Loose,
			DryRun:   true,
		})

		Convey("When the delivery is a dry run", func() {
			Convey("Then the plan is returned and every planned surface stays absent", func() {
				So(err, ShouldBeNil)
				So(homeFiles(t, home), ShouldBeEmpty)
				So(fileExists(filepath.Join(agyConfigDir(home), "mcp_config.json")), ShouldBeFalse)
				So(fileExists(filepath.Join(home, ".agents", "skills", "alpha")), ShouldBeFalse)
				So(res.RMA, ShouldNotBeEmpty)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, noteDryRunText)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, `command "dev" skipped`)
			})
		})
	})
}
