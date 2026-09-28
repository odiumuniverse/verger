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
	"github.com/odiumuniverse/verger/pkg/receipt"
)

// ompFixture reads one captured omp fixture:
// docs/reviews/omp-grammar.probe.log records the exact command, HOME and omp
// version it came from (omp/18.4.1, live, 2026-09-28).
func ompFixture(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "omp", name)) //nolint:gosec // G304: a fixture below testdata
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}

	return data
}

// TestOmpOracleListFromFixture pins the real `omp plugin list --json` shapes
// (omp/18.4.1, captured live): the empty document and the installed one. The
// installed entry carries its version and install path in a per-scope `entries`
// array, not at the top level (T1.7-T1.8 style oracle pinning).
func TestOmpOracleListFromFixture(t *testing.T) {
	Convey("Given the captured omp plugin list documents", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		list := string(ompFixture(t, "plugin-list-18.4.1.json"))

		Convey("When the host reports an installed plugin", func() {
			h, runner := newOmp(t, t.TempDir(), map[string]hostcli.Response{
				"omp plugin list --json": response(list),
			})

			listed, err := h.Oracle().List(t.Context())

			Convey("Then the entry is keyed name@marketplace with its install record", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{"omp plugin list --json"})
				So(listed, ShouldHaveLength, 1)
				So(listed[0].Name, ShouldEqual, "alpha")
				So(listed[0].Marketplace, ShouldEqual, "acme")
				So(listed[0].Version, ShouldEqual, "1.0.0")
				So(listed[0].Scope, ShouldEqual, "user")
				So(listed[0].Enabled, ShouldBeTrue)
				So(strings.HasSuffix(listed[0].Path, ".omp/plugins/cache/plugins/acme___alpha___1.0.0"), ShouldBeTrue)
			})
		})

		Convey("When the host reports nothing installed", func() {
			h, _ := newOmp(t, t.TempDir(), map[string]hostcli.Response{
				"omp plugin list --json": response(string(ompFixture(t, "plugin-list-empty-18.4.1.json"))),
			})

			listed, err := h.Oracle().List(t.Context())

			Convey("Then the list is empty, not an error", func() {
				So(err, ShouldBeNil)
				So(listed, ShouldBeEmpty)
			})
		})

		Convey("When the host answers a shape the oracle cannot read", func() {
			h, _ := newOmp(t, t.TempDir(), map[string]hostcli.Response{
				"omp plugin list --json": response("Plugin list unavailable\n"),
			})

			_, err := h.Oracle().List(t.Context())

			Convey("Then the oracle reports the output it could not parse", func() {
				oracleErr, ok := errors.AsType[*host.OracleError](err)
				So(ok, ShouldBeTrue)
				So(oracleErr.Output, ShouldContainSubstring, "Plugin list unavailable")
			})
		})
	})
}

// TestOmpOracleValidate pins the unsupported verdict: omp 18.4.1 has no
// `plugin validate` (its `plugin doctor` reports the plugins directory), and a
// caller must treat ErrNotSupported as "cannot validate", never as "valid".
func TestOmpOracleValidate(t *testing.T) {
	Convey("Given the omp oracle", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		h, runner := newOmp(t, t.TempDir(), nil)

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

// TestOmpMarketplaceListParsing pins the text table omp prints for
// `plugin marketplace list`: the verb ignores --json in 18.4.1, the empty form
// is a sentence, and neither form may invent a marketplace.
func TestOmpMarketplaceListParsing(t *testing.T) {
	Convey("Given the captured marketplace list outputs", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		empty := string(ompFixture(t, "marketplace-list-empty-18.4.1.txt"))
		configured := string(ompFixture(t, "marketplace-list-18.4.1.txt"))

		Convey("When a marketplace is configured", func() {
			h, runner := newOmp(t, t.TempDir(), map[string]hostcli.Response{
				"omp plugin marketplace list": response(configured),
				"omp plugin marketplace remove acme": response(strings.TrimSpace(
					string(ompFixture(t, "marketplace-remove-18.4.1.txt")))),
				"omp plugin list --json": response(`{"npm":[],"marketplace":[]}`),
			})

			receipt := ompHostReceipt()

			res, err := h.Uninstall(t.Context(), "", receipt)

			Convey("Then the registered name is parsed and its removal inverse runs", func() {
				So(err, ShouldBeNil)
				So(res.Notes, ShouldBeEmpty)
				So(callKeys(runner), ShouldResemble, []string{
					"omp plugin list --json",
					"omp plugin marketplace remove acme",
				})
			})
		})

		Convey("When nothing is configured", func() {
			h, _ := newOmp(t, t.TempDir(), map[string]hostcli.Response{
				"omp plugin marketplace list": response(empty),
				"omp plugin list --json":      response(`{"npm":[],"marketplace":[]}`),
				"omp plugin marketplace remove acme": {
					Code:   1,
					Stderr: strings.TrimSpace(string(ompFixture(t, "marketplace-remove-missing-18.4.1.err"))),
				},
			})

			res, err := h.Uninstall(t.Context(), "", ompHostReceipt())

			Convey("Then no marketplace is invented: the inverse reports it as never registered", func() {
				So(err, ShouldBeNil)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "was not registered")
			})
		})
	})
}

// TestOmpVersionFixture pins the observed version format, the only probe a
// doctor may compare a pin against.
func TestOmpVersionFixture(t *testing.T) {
	Convey("Given the captured omp --version output", t, func() {
		version := strings.TrimSpace(string(ompFixture(t, "version-18.4.1.txt")))

		Convey("Then it is omp/<semver>", func() {
			So(version, ShouldEqual, "omp/18.4.1")
		})
	})
}

// TestOmpConfigPathFixture pins the agent-dir probe the adapter mirrors: the
// path is absolute and ends in `.omp/agent` (the relocation cases are pinned in
// TestOmpDetect).
func TestOmpConfigPathFixture(t *testing.T) {
	Convey("Given the captured omp config path output", t, func() {
		agentDir := strings.TrimSpace(string(ompFixture(t, "config-path-18.4.1.txt")))

		Convey("Then it is the absolute agent dir below the profile home", func() {
			So(filepath.IsAbs(agentDir), ShouldBeTrue)
			So(filepath.Base(agentDir), ShouldEqual, "agent")
			So(filepath.Base(filepath.Dir(agentDir)), ShouldEqual, ".omp")
		})
	})
}

// TestOmpFakeMatchesCapturedRefusals pins that the fake CLI speaks the refusal
// texts the real one does: a fake that drifts would let the adapter's
// tolerance rules pass while the host answers something else.
func TestOmpFakeMatchesCapturedRefusals(t *testing.T) {
	Convey("Given the refusals captured live from omp 18.4.1", t, func() {
		Convey("When the fake refuses an already-registered marketplace", func() {
			cli := newOmpCLI()
			cli.marketplaces["acme"] = "/tmp/probe/mkt"

			_, err := cli.marketplace("add", ompMarketplace(t, "acme", "alpha", "1.0.0"))

			Convey("Then it answers the captured text", func() {
				exit, ok := errors.AsType[*hostcli.ExitError](err)
				So(ok, ShouldBeTrue)
				So(exit.Code, ShouldEqual, 1)
				So(exit.Stderr, ShouldEqual, refusalOf(t, "marketplace-add-exists-18.4.1.err", "acme"))
			})
		})

		Convey("When the fake refuses an install of an installed plugin", func() {
			cli := newOmpCLI()
			cli.marketplaces["acme"] = ompMarketplace(t, "acme", "alpha", "1.0.0")
			cli.installed["alpha@acme"] = "1.0.0"

			_, err := cli.plugin("install", "alpha@acme", false)

			Convey("Then it answers the captured text", func() {
				exit, ok := errors.AsType[*hostcli.ExitError](err)
				So(ok, ShouldBeTrue)
				So(exit.Stderr, ShouldEqual, refusalOf(t, "install-already-18.4.1.err", "alpha@acme"))
			})
		})

		Convey("When the fake refuses an uninstall of an absent plugin", func() {
			cli := newOmpCLI()

			_, err := cli.plugin("uninstall", "alpha@acme", false)

			Convey("Then it answers the captured text", func() {
				exit, ok := errors.AsType[*hostcli.ExitError](err)
				So(ok, ShouldBeTrue)
				So(exit.Stderr, ShouldEqual, refusalOf(t, "uninstall-missing-18.4.1.err", "alpha@acme"))
			})
		})

		Convey("When the fake refuses a removal of an unknown marketplace", func() {
			cli := newOmpCLI()

			_, err := cli.marketplace("remove", "acme")

			Convey("Then it answers the captured text", func() {
				exit, ok := errors.AsType[*hostcli.ExitError](err)
				So(ok, ShouldBeTrue)
				So(exit.Stderr, ShouldEqual, refusalOf(t, "marketplace-remove-missing-18.4.1.err", "acme"))
			})
		})
	})
}

// refusalOf reads one captured refusal and renames the plugin the capture used,
// so a fake answering another selector is still compared byte for byte.
func refusalOf(t *testing.T, fixture, name string) string {
	t.Helper()

	text := strings.TrimSpace(string(ompFixture(t, fixture)))

	switch fixture {
	case "install-already-18.4.1.err":
		return strings.ReplaceAll(text, "alpha@acme", name)
	case "uninstall-missing-18.4.1.err":
		return strings.ReplaceAll(text, "alpha@acme", name)
	default:
		return text
	}
}

// ompHostReceipt is one receipt whose only op removes a marketplace.
func ompHostReceipt() receipt.Receipt {
	return receipt.Receipt{Strategy: string(host.Native), RMA: []receipt.Op{
		{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "remove", "acme"}},
	}}
}
