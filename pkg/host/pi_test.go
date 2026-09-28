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
	"github.com/odiumuniverse/verger/pkg/secret"
	"github.com/odiumuniverse/verger/pkg/store"
)

const piFixtureRoot = "testdata/pi/acme"

// fakePi puts an executable `pi` shim at the front of PATH.
func fakePi(t *testing.T) {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "pi"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // G306: a fake host CLI must be executable
		t.Fatalf("write shim: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// newPi builds the Pi adapter with a scripted runner.
func newPi(t *testing.T, home string, script map[string]hostcli.Response, opts ...host.Option) (host.Host, *hostcli.ScriptRunner) {
	t.Helper()

	runner := hostcli.NewScriptRunner(script)
	all := append([]host.Option{host.WithHome(home), host.WithRunner(runner)}, opts...)

	return host.NewPi(all...), runner
}

// piPackage parses the Pi fixture and adds the rule component the manifest
// format does not carry.
func piPackage(t *testing.T) host.Package {
	t.Helper()

	return adapterFixturePackage(t, piFixtureRoot, manifest.FormatClaude)
}

// piAgentDir is the Pi agent dir of one test home.
func piAgentDir(home string) string {
	return filepath.Join(home, ".pi", "agent")
}

// piFixture reads one captured pi fixture.
func piFixture(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "pi", name)) //nolint:gosec // G304: a fixture below testdata
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}

	return string(data)
}

// piSecrets is the secret store of the Pi fixture: its MCP servers reference
// MCP_TOKEN and WEB_TOKEN.
func piSecrets(t *testing.T) *secret.Store {
	t.Helper()

	return secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token", "WEB_TOKEN": "web-token"})
}

// piSynthPackage lays out one Pi package in the store: a directory with the
// `pi` manifest pi 0.74.2 documents, pointing at its own skills and prompts.
func piSynthPackage(t *testing.T, st *store.Store, id, version string) host.Package {
	t.Helper()

	dir, err := st.EnsureSynthPath(id, version)
	if err != nil {
		t.Fatalf("synth path: %v", err)
	}

	writeFixtureFile(t, filepath.Join(dir, "skills", "alpha", "SKILL.md"),
		"---\nname: alpha\ndescription: Alpha.\n---\n\n# Alpha\n", 0o600)
	writeFixtureFile(t, filepath.Join(dir, "package.json"),
		`{"name":"`+id+`","version":"`+version+`","keywords":["pi-package"],"pi":{"skills":["./skills"]}}`, 0o600)

	return host.Package{ID: id, Version: version, SynthDir: dir, Marketplace: filepath.Base(dir)}
}

// TestPiVersionFixture pins the live-probed CLI version the adapter's comments
// and the e2e pin refer to.
func TestPiVersionFixture(t *testing.T) {
	Convey("Given the captured pi version", t, func() {
		Convey("Then it is the release the grammar was probed on", func() {
			So(strings.TrimSpace(piFixture(t, "version-0.74.2.txt")), ShouldEqual, "0.74.2")
		})
	})
}

func TestPiDetect(t *testing.T) {
	Convey("Given a home without a Pi agent dir", t, func() {
		home := t.TempDir()
		t.Setenv("PATH", t.TempDir())
		t.Setenv("PI_CODING_AGENT_DIR", "")

		h, _ := newPi(t, home, nil)

		Convey("When neither the home nor the binary exist", func() {
			Convey("Then Detect is false", func() {
				So(h.Detect(home), ShouldBeFalse)
			})
		})

		Convey("When ~/.pi/agent exists", func() {
			if err := os.MkdirAll(piAgentDir(home), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})

		Convey("When PI_CODING_AGENT_DIR relocates the agent dir", func() {
			elsewhere := filepath.Join(t.TempDir(), "agent")
			t.Setenv("PI_CODING_AGENT_DIR", elsewhere)

			if err := os.MkdirAll(elsewhere, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			Convey("Then Detect follows the relocation", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})

	Convey("Given the pi binary on PATH", t, func() {
		fakePi(t)

		home := t.TempDir()
		t.Setenv("PI_CODING_AGENT_DIR", "")

		h, _ := newPi(t, home, nil)

		Convey("When only the binary exists", func() {
			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})
}

// TestPiLooseGolden pins every pi user surface the host reads (pi 0.74.2,
// README "Place in ~/.pi/agent/skills/, ~/.agents/skills/" and
// ~/.pi/agent/prompts/): skills, prompt templates and the rule wrapper land,
// while the components pi has no surface for say so.
func TestPiLooseGolden(t *testing.T) {
	Convey("Given a Pi package and a temp home", t, func() {
		fakePi(t)

		home := t.TempDir()
		st := openStore(t)
		pkg := piPackage(t)

		h, runner := newPi(t, home, nil,
			host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(piSecrets(t)))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered loose", func() {
			Convey("Then skills, prompts, the rule wrapper and mcp.json land; nothing else", func() {
				So(err, ShouldBeNil)
				So(homeFiles(t, home), ShouldResemble, []string{
					".pi/agent/mcp.json",
					".pi/agent/prompts/dev.md",
					".pi/agent/skills/alpha/SKILL.md",
					".pi/agent/skills/alpha/scripts/run.sh",
					".pi/agent/skills/beta/SKILL.md",
					".pi/agent/skills/rule-caveman/SKILL.md",
				})
				So(runner.Calls(), ShouldBeEmpty)
			})

			Convey("Then the components pi cannot take are named, never written outside the home", func() {
				notes := strings.Join(res.Notes, "\n")
				So(notes, ShouldContainSubstring, `agent component "reviewer" is not expressible in loose pi`)
				So(notes, ShouldContainSubstring, "1 hook(s) skipped")
				So(notes, ShouldContainSubstring, "pi has no hook surface")
				So(notes, ShouldContainSubstring, "rule is delivered as a skill wrapper")
			})

			Convey("Then the MCP document is pi's override shape and the adapter note names the third-party reader", func() {
				So(readTestFile(t, filepath.Join(piAgentDir(home), "mcp.json")), ShouldEqualJSON, `{
  "mcpServers": {
    "fs": {"command": "node", "args": ["`+pkg.Root+`/server.js"], "env": {"TOKEN": "s3cr3t-token"}, "transport": "stdio"},
    "web": {"url": "https://example.com/mcp", "headers": {"Authorization": "Bearer web-token"}, "transport": "sse"}
  }
}`)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "pi has no built-in MCP")
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "pi-mcp-adapter")
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

// TestPiAgentDirEnv pins the live-probed relocation: pi reads every user
// surface from getAgentDir(), which is PI_CODING_AGENT_DIR when set
// (pi 0.74.2 dist/cli.js) and ~/.pi/agent otherwise.
func TestPiAgentDirEnv(t *testing.T) {
	Convey("Given PI_CODING_AGENT_DIR pointing elsewhere", t, func() {
		fakePi(t)

		home := t.TempDir()
		elsewhere := filepath.Join(t.TempDir(), "agent")
		t.Setenv("PI_CODING_AGENT_DIR", elsewhere)

		h, _ := newPi(t, home, nil, host.WithStore(openStore(t)), host.WithSecrets(piSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: piPackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then every surface lands under the relocated agent dir and the default home stays untouched", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(elsewhere, "skills", "alpha", "SKILL.md")), ShouldBeTrue)
				So(fileExists(filepath.Join(elsewhere, "prompts", "dev.md")), ShouldBeTrue)
				So(fileExists(filepath.Join(elsewhere, "mcp.json")), ShouldBeTrue)
				So(fileExists(filepath.Join(home, ".pi")), ShouldBeFalse)
			})
		})
	})
}

func TestPiLooseCollisions(t *testing.T) {
	Convey("Given a foreign skill tree", t, func() {
		fakePi(t)

		home := t.TempDir()

		foreign := filepath.Join(piAgentDir(home), "skills", "alpha")
		if err := os.MkdirAll(foreign, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		writeFixtureFile(t, filepath.Join(foreign, "SKILL.md"), "someone else's skill\n", 0o600)

		h, _ := newPi(t, home, nil, host.WithStore(openStore(t)), host.WithSecrets(piSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: piPackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then the foreign file is untouched and the cell is a collision", func() {
				collision, ok := errors.AsType[*host.CollisionError](err)
				So(ok, ShouldBeTrue)
				So(collision.Path, ShouldEqual, foreign)
				So(readTestFile(t, filepath.Join(foreign, "SKILL.md")), ShouldEqual, "someone else's skill\n")
			})
		})
	})
}

// TestPiNativeDeliver pins the live-verified install grammar: `pi install
// <path>` records the package in the agent settings and the oracle is
// `pi list` (pi 0.74.2).
func TestPiNativeDeliver(t *testing.T) {
	Convey("Given a Pi package in the store", t, func() {
		fakePi(t)

		home := t.TempDir()
		st := openStore(t)
		pkg := piSynthPackage(t, st, "acme/caveman", "1.2.3")

		h, runner := newPi(t, home, map[string]hostcli.Response{
			"pi install " + pkg.SynthDir: response("Installed " + pkg.SynthDir),
			"pi list":                    response("User packages:\n  ../../store/synth/acme/caveman\n    " + pkg.SynthDir + "\n"),
		})

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered natively", func() {
			Convey("Then the host is asked to install the synth dir and the receipt carries the inverse", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{"pi install " + pkg.SynthDir, "pi list"})
				So(res.RMA, ShouldHaveLength, 1)
				So(res.RMA[0].Kind, ShouldEqual, receipt.OpHostInstall)
				So(res.RMA[0].Command, ShouldResemble, []string{"remove", pkg.SynthDir})
				So(res.RMA[0].Existed, ShouldBeFalse)
			})

			Convey("Then the delivery is verified through the host's own listing", func() {
				So(res.Observed.Verified, ShouldBeTrue)
				So(res.Observed.Listed, ShouldHaveLength, 1)
			})

			Convey("Then the artifact proves the path is verger's on the next delivery", func() {
				So(res.Artifacts, ShouldHaveLength, 1)
				So(res.Artifacts[0].Path, ShouldEqual, "pi://package/"+pkg.ID)
			})
		})
	})
}

// TestPiNativeVerifyFailure pins NF-2's other half: a host that does not list
// the package fails the cell instead of silently stepping down.
func TestPiNativeVerifyFailure(t *testing.T) {
	Convey("Given a host whose listing does not mention the package", t, func() {
		fakePi(t)

		home := t.TempDir()
		st := openStore(t)
		pkg := piSynthPackage(t, st, "acme/caveman", "1.2.3")

		h, _ := newPi(t, home, map[string]hostcli.Response{
			"pi install " + pkg.SynthDir: response("Installed " + pkg.SynthDir),
			"pi list":                    response("No packages installed.\n"),
		})

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When the delivery is verified", func() {
			Convey("Then it fails as a delivery error", func() {
				delivery, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(delivery.Host, ShouldEqual, "pi")
				So(delivery.Step, ShouldEqual, "verify")
				So(delivery.Cause, ShouldBeError)
			})
		})
	})
}

// TestPiNativeDryRun pins that planning a native delivery runs no host call.
func TestPiNativeDryRun(t *testing.T) {
	Convey("Given a Pi package in the store", t, func() {
		fakePi(t)

		home := t.TempDir()
		st := openStore(t)
		pkg := piSynthPackage(t, st, "acme/caveman", "1.2.3")

		h, runner := newPi(t, home, nil)

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native, DryRun: true})

		Convey("When the delivery is a dry run", func() {
			Convey("Then the host is never called and only the inverse is planned", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldBeEmpty)
				So(res.Notes, ShouldResemble, []string{"dry-run"})
				So(res.RMA, ShouldHaveLength, 1)
				So(res.RMA[0].Command, ShouldResemble, []string{"remove", pkg.SynthDir})
			})
		})
	})
}

// TestPiNativeWithoutSynthDir pins the refusal: a native install of a
// marketplace ref needs a package dir on disk, and verger has none for pi.
func TestPiNativeWithoutSynthDir(t *testing.T) {
	Convey("Given a native delivery with no synth dir", t, func() {
		fakePi(t)

		home := t.TempDir()
		h, runner := newPi(t, home, nil)

		pkg := piPackage(t)
		pkg.SynthDir = ""

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When the delivery runs", func() {
			Convey("Then it is refused as unsupported and the host is never called", func() {
				unsupported, ok := errors.AsType[*host.NotSupportedError](err)
				So(ok, ShouldBeTrue)
				So(unsupported.Host, ShouldEqual, host.Pi)
				So(callKeys(runner), ShouldBeEmpty)
			})
		})
	})
}

// TestPiUninstall pins the RMA loop of a native receipt: the ops run in
// reverse order and a package the host no longer knows is already removed.
func TestPiUninstall(t *testing.T) {
	Convey("Given a native pi receipt", t, func() {
		fakePi(t)

		home := t.TempDir()
		dir := filepath.Join(t.TempDir(), "pkg")

		h, runner := newPi(t, home, map[string]hostcli.Response{
			"pi list":          response("User packages:\n  x\n    " + dir + "\n"),
			"pi remove " + dir: response("Removed " + dir),
		})

		res, err := h.Uninstall(t.Context(), home, receipt.Receipt{
			Strategy: string(host.Native),
			RMA: []receipt.Op{
				{Kind: receipt.OpHostInstall, Command: []string{"remove", dir}},
			},
		})

		Convey("When it is uninstalled", func() {
			Convey("Then the recorded inverse is what the host is asked", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{"pi list", "pi remove " + dir})
				So(res.Notes, ShouldBeEmpty)
			})
		})
	})
}

// TestPiUninstallMissingPackage pins NF-2's inverse rule: a package the host no
// longer lists is already removed, so the inverse says so instead of failing.
func TestPiUninstallMissingPackage(t *testing.T) {
	Convey("Given a native pi receipt for an unknown package", t, func() {
		fakePi(t)

		home := t.TempDir()
		dir := filepath.Join(t.TempDir(), "pkg")

		h, runner := newPi(t, home, map[string]hostcli.Response{
			"pi list": response("No packages installed.\n"),
		})

		res, err := h.Uninstall(t.Context(), home, receipt.Receipt{
			Strategy: string(host.Native),
			RMA: []receipt.Op{
				{Kind: receipt.OpHostInstall, Command: []string{"remove", dir}},
			},
		})

		Convey("When it is uninstalled", func() {
			Convey("Then no remove call is made and the host is asked instead", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{"pi list"})
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "not listed")
			})
		})
	})
}

// TestPiOracleList pins the live `pi list` shape: a section header, the source
// as settings record it and the resolved absolute path on the deeper line.
func TestPiOracleList(t *testing.T) {
	Convey("Given the captured pi listing", t, func() {
		fakePi(t)

		h, runner := newPi(t, t.TempDir(), map[string]hostcli.Response{
			"pi list": response(piFixture(t, "list-0.74.2.txt")),
		})

		listed, err := h.Oracle().List(t.Context())

		Convey("When the host is asked", func() {
			Convey("Then every resolved package path is reported, user and project alike", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{"pi list"})
				So(listed, ShouldHaveLength, 2)

				paths := map[string]host.Installed{}
				for _, entry := range listed {
					paths[entry.Path] = entry
				}

				So(paths, ShouldContainKey, "/Users/u/.verger/store/synth/acme/caveman")
				So(paths, ShouldContainKey, "/tmp/pkg")
			})
		})
	})
}

// TestPiOracleEmpty pins the captured empty listing: pi answers a sentence,
// not a table, and no package may be invented from it.
func TestPiOracleEmpty(t *testing.T) {
	Convey("Given a host with no installed package", t, func() {
		fakePi(t)

		h, _ := newPi(t, t.TempDir(), map[string]hostcli.Response{
			"pi list": response(piFixture(t, "list-empty-0.74.2.txt")),
		})

		listed, err := h.Oracle().List(t.Context())

		Convey("When the host is asked", func() {
			Convey("Then the list is empty, not an invented package", func() {
				So(err, ShouldBeNil)
				So(listed, ShouldBeEmpty)
			})
		})
	})
}

// TestPiOracleValidate pins the unsupported verdict: pi 0.74.2 has no
// validation subcommand, only install/remove/update/list/config.
func TestPiOracleValidate(t *testing.T) {
	Convey("Given the pi oracle", t, func() {
		fakePi(t)

		h, runner := newPi(t, t.TempDir(), nil)

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

// TestPiReDelivery pins the idempotent re-install: with a receipt that owns the
// paths, the same delivery rewrites them, marks them pre-existing and keeps the
// MCP key owned, so a later update cannot reconcile it away.
func TestPiReDelivery(t *testing.T) {
	Convey("Given a delivered Pi package", t, func() {
		fakePi(t)

		home := t.TempDir()
		st := openStore(t)

		h, _ := newPi(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(piSecrets(t)), host.WithOwnership(ownerExisting("acme/caveman")))

		pkg := piPackage(t)

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})
		So(err, ShouldBeNil)

		Convey("When it is delivered again", func() {
			again, againErr := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

			Convey("Then every artifact is rewritten and marked as pre-existing", func() {
				So(againErr, ShouldBeNil)
				So(again.Artifacts, ShouldNotBeEmpty)

				for _, artifact := range again.Artifacts {
					So(fileExists(artifact.Path), ShouldBeTrue)
				}

				ops := map[string]receipt.Op{}
				keys := map[string]bool{}

				for _, op := range again.RMA {
					ops[op.Path] = op

					if op.Kind == receipt.OpConfigKey {
						keys[op.KeyPath] = true
					}
				}

				prompt := filepath.Join(piAgentDir(home), "prompts", "dev.md")
				So(ops[prompt].Existed, ShouldBeTrue)
				So(ops[prompt].Backup, ShouldNotBeEmpty)
				So(keys, ShouldContainKey, "mcpServers.fs")
				So(keys, ShouldContainKey, "mcpServers.web")
			})
		})
	})
}

// TestPiLooseDryRunWritesNothing pins the dry-run contract every adapter owes
// the CLI: the plan is computed and the RMA returned, but no target is touched —
// otherwise the real pass meets the dry pass's own output as a foreign path.
func TestPiLooseDryRunWritesNothing(t *testing.T) {
	Convey("Given a Pi package and a temp home", t, func() {
		fakePi(t)

		home := t.TempDir()
		h, _ := newPi(t, home, nil, host.WithStore(openStore(t)), host.WithSecrets(piSecrets(t)))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: piPackage(t), Strategy: host.Loose, DryRun: true})

		Convey("When the delivery is a dry run", func() {
			Convey("Then the plan is returned and the home is untouched", func() {
				So(err, ShouldBeNil)
				So(homeFiles(t, home), ShouldBeEmpty)
				So(res.RMA, ShouldNotBeEmpty)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, noteDryRunText)
			})
		})
	})
}

// noteDryRunText is the marker every dry-run Result carries.
const noteDryRunText = "dry-run"
