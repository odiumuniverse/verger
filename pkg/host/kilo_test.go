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
)

const kiloFixtureRoot = "testdata/kilo/acme"

// fakeKilo puts an executable `kilo` shim at the front of PATH.
func fakeKilo(t *testing.T) {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "kilo"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // G306: a fake host CLI must be executable
		t.Fatalf("write shim: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// newKilo builds the Kilo adapter with a scripted runner.
func newKilo(t *testing.T, home string, script map[string]hostcli.Response, opts ...host.Option) (host.Host, *hostcli.ScriptRunner) {
	t.Helper()

	runner := hostcli.NewScriptRunner(script)
	all := append([]host.Option{host.WithHome(home), host.WithRunner(runner)}, opts...)

	return host.NewKilo(all...), runner
}

// kiloPackage parses the Kilo fixture.
func kiloPackage(t *testing.T) host.Package {
	t.Helper()

	return adapterFixturePackage(t, kiloFixtureRoot, manifest.FormatClaude)
}

// kiloConfigDir is the Kilo config root of one test home: <home>/.config/kilo,
// with no XDG relocation (beadle resolves it the same way).
func kiloConfigDir(home string) string {
	return filepath.Join(home, ".config", "kilo")
}

// kiloFixture reads one captured kilo fixture.
func kiloFixture(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "kilo", name)) //nolint:gosec // G304: a fixture below testdata
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}

	return string(data)
}

// kiloTestEnv pins the Kilo root to the home-relative default: the suite sets
// XDG_CONFIG_HOME and a developer's shell may set KILO_CONFIG_DIR, and a test
// that does not exercise the env must not inherit either.
func kiloTestEnv(t *testing.T, home string) string {
	t.Helper()

	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("KILO_CONFIG_DIR", "")

	return filepath.Join(home, ".config", "kilo")
}

// kiloWorld builds the adapter over a temp home with the store and secrets the
// fixture needs.
func kiloWorld(t *testing.T) (host.Host, *hostcli.ScriptRunner, string) {
	t.Helper()

	fakeKilo(t)

	home := t.TempDir()
	st := openStore(t)

	h, runner := newKilo(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(openCodeSecrets(t)))

	return h, runner, home
}

func TestKiloDetect(t *testing.T) {
	Convey("Given a home without a Kilo config", t, func() {
		home := t.TempDir()
		t.Setenv("PATH", t.TempDir())
		kiloTestEnv(t, home)

		h, _ := newKilo(t, home, nil)

		Convey("When neither the config dir, the legacy ~/.kilo dir nor the binary exist", func() {
			Convey("Then Detect is false", func() {
				So(h.Detect(home), ShouldBeFalse)
			})
		})

		Convey("When the config dir exists", func() {
			if err := os.MkdirAll(filepath.Join(home, ".config", "kilo"), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})

		Convey("When only the relocated XDG config dir exists", func() {
			xdg := filepath.Join(t.TempDir(), "xdg")
			t.Setenv("XDG_CONFIG_HOME", xdg)

			if err := os.MkdirAll(filepath.Join(xdg, "kilo"), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			Convey("Then Detect is true, because that is where the host reads", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})

		Convey("When only the legacy ~/.kilo dir exists", func() {
			if err := os.MkdirAll(filepath.Join(home, ".kilo"), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})

	Convey("Given the kilo binary on PATH", t, func() {
		fakeKilo(t)

		h, _ := newKilo(t, t.TempDir(), nil)

		Convey("When only the binary exists", func() {
			Convey("Then Detect is true", func() {
				So(h.Detect(t.TempDir()), ShouldBeTrue)
			})
		})
	})
}

// TestKiloLooseGolden pins the Kilo loose surface: skills, agents and commands
// below ~/.config/kilo, MCP servers under the v1 `/mcp` container of
// kilo.json, the rule as the D23 skill wrapper, and hooks skipped with the
// runtime reason (a v1 `plugin: [...]` entry carries hooks and tools only).
func TestKiloLooseGolden(t *testing.T) {
	Convey("Given a Kilo package and a temp home", t, func() {
		fakeKilo(t)

		home := t.TempDir()
		kiloTestEnv(t, home)

		st := openStore(t)
		h, runner := newKilo(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(openCodeSecrets(t)))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: kiloPackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered loose", func() {
			Convey("Then the host-native surfaces are written below ~/.config/kilo", func() {
				So(err, ShouldBeNil)
				So(homeFiles(t, home), ShouldResemble, []string{
					".config/kilo/agents/reviewer.md",
					".config/kilo/commands/dev.md",
					".config/kilo/kilo.json",
					".config/kilo/skills/alpha/SKILL.md",
					".config/kilo/skills/alpha/scripts/run.sh",
					".config/kilo/skills/beta/SKILL.md",
					".config/kilo/skills/rule-caveman/SKILL.md",
				})
				So(runner.Calls(), ShouldBeEmpty)
			})

			Convey("Then MCP lands under the legacy mcp container in the Kilo dialect", func() {
				doc := openCodeMCPDoc(t, filepath.Join(kiloConfigDir(home), "kilo.json"))

				servers, ok := doc["mcp"].(map[string]any)
				So(ok, ShouldBeTrue)

				fs, ok := servers["fs"].(map[string]any)
				So(ok, ShouldBeTrue)
				So(fs["type"], ShouldEqual, "local")
				So(fs["environment"], ShouldResemble, map[string]any{"TOKEN": "s3cr3t-token"})

				web, ok := servers["web"].(map[string]any)
				So(ok, ShouldBeTrue)
				So(web["type"], ShouldEqual, "remote")
				So(web["url"], ShouldEqual, "https://example.com/mcp")
			})

			Convey("Then the hooks are skipped with the runtime reason", func() {
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "T2.3")
				So(fileExists(filepath.Join(kiloConfigDir(home), "hooks.json")), ShouldBeFalse)
			})
		})
	})
}

// TestKiloEnvPrecedence pins the config root exactly as kilo 7.8.1 resolves it
// (live-probed with `kilo mcp list` in isolated HOMEs): KILO_CONFIG_DIR
// replaces the root outright, XDG_CONFIG_HOME is the fallback, and the
// home-relative default applies only when neither is set.
func TestKiloEnvPrecedence(t *testing.T) {
	Convey("Given a Kilo package and a temp home", t, func() {
		fakeKilo(t)

		home := t.TempDir()
		st := openStore(t)
		pkg := kiloPackage(t)

		deliver := func(t *testing.T) host.Host {
			t.Helper()

			h, _ := newKilo(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(openCodeSecrets(t)))

			return h
		}

		Convey("When KILO_CONFIG_DIR is set", func() {
			dir := filepath.Join(t.TempDir(), "kd")
			t.Setenv("KILO_CONFIG_DIR", dir)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg"))

			_, err := deliver(t).Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

			Convey("Then the whole surface lands below KILO_CONFIG_DIR", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(dir, "kilo.json")), ShouldBeTrue)
				So(fileExists(filepath.Join(dir, "commands", "dev.md")), ShouldBeTrue)
				So(fileExists(filepath.Join(home, ".config", "kilo")), ShouldBeFalse)
			})
		})

		Convey("When only XDG_CONFIG_HOME is set", func() {
			xdg := filepath.Join(t.TempDir(), "xdg")
			t.Setenv("KILO_CONFIG_DIR", "")
			t.Setenv("XDG_CONFIG_HOME", xdg)

			_, err := deliver(t).Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

			Convey("Then the surface lands below the XDG root, as the host reads it", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(xdg, "kilo", "kilo.json")), ShouldBeTrue)
				So(fileExists(filepath.Join(home, ".config", "kilo")), ShouldBeFalse)
			})
		})

		Convey("When neither is set", func() {
			kiloTestEnv(t, home)

			_, err := deliver(t).Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

			Convey("Then the home-relative default is used", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(home, ".config", "kilo", "kilo.json")), ShouldBeTrue)
			})
		})
	})
}

// TestKiloConfigFilePreference pins the config document resolution: an existing
// kilo.jsonc wins over kilo.json, and its comments survive the MCP write.
func TestKiloConfigFilePreference(t *testing.T) {
	Convey("Given a commented kilo.jsonc", t, func() {
		fakeKilo(t)

		home := t.TempDir()
		kiloTestEnv(t, home)

		st := openStore(t)

		path := filepath.Join(kiloConfigDir(home), "kilo.jsonc")
		writeFixtureFile(t, path, `{
  // kilo's own note
  "mcp": {}
}
`, 0o600)

		h, _ := newKilo(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(openCodeSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: kiloPackage(t), Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then the jsonc document is the one written, with its comment", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(kiloConfigDir(home), "kilo.json")), ShouldBeFalse)

				text := readTestFile(t, path)
				So(text, ShouldContainSubstring, "// kilo's own note")
				So(text, ShouldContainSubstring, `"fs"`)
			})
		})
	})
}

// TestKiloNativeRefused pins the strata Kilo does not get in this wave: the
// plugin grammar writes a v1 `plugin: [...]` entry into the legacy
// opencode.json and answers exit 0 even for a failed install, so neither
// native nor synth is claimed.
func TestKiloNativeRefused(t *testing.T) {
	Convey("Given a Kilo package with a source ref", t, func() {
		fakeKilo(t)

		home := t.TempDir()
		st := openStore(t)

		runner := hostcli.NewScriptRunner(nil)
		h := host.NewKilo(host.WithHome(home), host.WithRunner(runner), host.WithStore(st), host.WithTrash(st.Trash()))

		pkg := kiloPackage(t)
		pkg.Marketplace = "npm:@acme/caveman@1.2.3"

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered native", func() {
			Convey("Then it is unsupported and the CLI is never called", func() {
				_, ok := errors.AsType[*host.NotSupportedError](err)
				So(ok, ShouldBeTrue)
				So(callKeys(runner), ShouldBeEmpty)
			})
		})
	})

	Convey("Given a synth delivery", t, func() {
		fakeKilo(t)

		home := t.TempDir()
		st := openStore(t)

		runner := hostcli.NewScriptRunner(nil)
		h := host.NewKilo(host.WithHome(home), host.WithRunner(runner), host.WithStore(st), host.WithTrash(st.Trash()))

		pkg := kiloPackage(t)
		pkg.SynthDir = t.TempDir()

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Synth})

		Convey("When it is delivered synth", func() {
			Convey("Then it is unsupported too", func() {
				_, ok := errors.AsType[*host.NotSupportedError](err)
				So(ok, ShouldBeTrue)
				So(callKeys(runner), ShouldBeEmpty)
			})
		})
	})
}

// TestKiloOracleList pins the oracle over the host's own resolved
// configuration (captured live on kilo 7.8.1): the `plugin_origins` entries
// carry the spec, its config source and its scope.
//
//nolint:dupl // the two oracles share the fixture-then-list shape; fixtures and assertions differ
func TestKiloOracleList(t *testing.T) {
	Convey("Given the captured kilo debug config", t, func() {
		fakeKilo(t)

		h, runner := newKilo(t, t.TempDir(), map[string]hostcli.Response{
			"kilo debug config": response(kiloFixture(t, "debug-config-7.8.1.json")),
		})

		listed, err := h.Oracle().List(t.Context())

		Convey("When the host is asked", func() {
			Convey("Then every configured plugin is listed with its origin", func() {
				So(err, ShouldBeNil)
				So(listed, ShouldHaveLength, 3)
				So(listed[0].Name, ShouldEqual, "opencode-poe-auth")
				So(listed[0].Scope, ShouldEqual, "global")
				So(listed[0].Source, ShouldContainSubstring, ".config/kilo")
				So(listed[0].Enabled, ShouldBeTrue)
				So(callKeys(runner), ShouldResemble, []string{"kilo debug config"})
			})
		})
	})

	Convey("Given a host that reports only the flat plugin list", t, func() {
		fakeKilo(t)

		h, _ := newKilo(t, t.TempDir(), map[string]hostcli.Response{
			"kilo debug config": response(`{"plugin": ["acme-plugin"]}`),
		})

		listed, err := h.Oracle().List(t.Context())

		Convey("When the host is asked", func() {
			Convey("Then the spec is listed without an origin", func() {
				So(err, ShouldBeNil)
				So(listed, ShouldHaveLength, 1)
				So(listed[0].Name, ShouldEqual, "acme-plugin")
				So(listed[0].Scope, ShouldEqual, "user")
			})
		})
	})

	Convey("Given a host with no plugin", t, func() {
		fakeKilo(t)

		h, _ := newKilo(t, t.TempDir(), map[string]hostcli.Response{
			"kilo debug config": response(`{"mcp": {"fs": {"type": "local"}}}`),
		})

		listed, err := h.Oracle().List(t.Context())

		Convey("When the host is asked", func() {
			Convey("Then nothing is invented and no MCP server is a package", func() {
				So(err, ShouldBeNil)
				So(listed, ShouldBeEmpty)
			})
		})
	})

	Convey("Given a host answering something that is not a config", t, func() {
		fakeKilo(t)

		h, _ := newKilo(t, t.TempDir(), map[string]hostcli.Response{
			"kilo debug config": response("not json\n"),
		})

		_, err := h.Oracle().List(t.Context())

		Convey("When the host is asked", func() {
			Convey("Then the unparsable answer is an oracle error", func() {
				_, ok := errors.AsType[*host.OracleError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})
}

// TestKiloOracleValidate pins the unsupported verdict: kilo has no package
// validation verb.
//
//nolint:dupl // the oracle-unsupported shape is the same for every host without one, and the fixture is not
func TestKiloOracleValidate(t *testing.T) {
	Convey("Given the Kilo oracle", t, func() {
		fakeKilo(t)

		h, runner := newKilo(t, t.TempDir(), nil)

		warnings, err := h.Oracle().Validate(t.Context(), t.TempDir())

		Convey("When a directory is validated", func() {
			Convey("Then it is unsupported and no CLI call is made", func() {
				So(errors.Is(err, host.ErrNotSupported), ShouldBeTrue)
				So(warnings, ShouldBeEmpty)
				So(callKeys(runner), ShouldBeEmpty)
			})
		})
	})
}

// TestKiloUninstall pins the honest removal of a loose-only host: a receipt
// that carries a host-install op (recorded by another tool or an older build)
// is not executed — kilo has no verb this adapter may run — and nothing is
// claimed as done.
func TestKiloUninstall(t *testing.T) {
	Convey("Given a receipt carrying a host-install op", t, func() {
		fakeKilo(t)

		h, runner := newKilo(t, t.TempDir(), nil)

		r := receipt.Receipt{Strategy: string(host.Loose), RMA: []receipt.Op{
			{Kind: receipt.OpHostInstall, Command: []string{"plugin", "remove", "acme"}},
			{Kind: receipt.OpWriteFile, Path: filepath.Join(t.TempDir(), "agent.md")},
		}}

		res, err := h.Uninstall(t.Context(), "", r)

		Convey("When it is uninstalled", func() {
			Convey("Then the host is never called and nothing is claimed", func() {
				So(err, ShouldBeNil)
				So(res.Notes, ShouldBeEmpty)
				So(callKeys(runner), ShouldBeEmpty)
			})
		})
	})
}

// TestKiloLooseDryRun pins that planning writes nothing.
//
//nolint:dupl // the dry-run plan shape is the same for every loose-only host, and the fixture is not
func TestKiloLooseDryRun(t *testing.T) {
	Convey("Given a dry-run delivery", t, func() {
		h, _, home := kiloWorld(t)

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: kiloPackage(t), Strategy: host.Loose, DryRun: true})

		Convey("When the plan is rendered", func() {
			Convey("Then it is complete and the home is untouched", func() {
				So(err, ShouldBeNil)
				So(res.Artifacts, ShouldNotBeEmpty)
				So(homeFiles(t, home), ShouldBeEmpty)
			})
		})
	})
}
