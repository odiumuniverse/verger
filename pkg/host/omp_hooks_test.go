package host_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
)

const ompHookFixtureRoot = "testdata/omp/hookpkg"

// ompHookProbe is the argv of the load probe: a bounded headless session start,
// the only way omp reports a module that did not load.
var ompHookProbe = []string{"-p", "omp", "--max-time", "3"}

// ompHookProbeKey is the scripted key of the probe for the fake runner.
var ompHookProbeKey = "omp " + strings.Join(ompHookProbe, " ")

// ompHookPackage is the fixture package carrying host hook modules.
func ompHookPackage(t *testing.T) host.Package {
	t.Helper()

	return adapterFixturePackage(t, ompHookFixtureRoot, manifest.FormatClaude)
}

// ompHookPath is the delivered path of one hook module.
func ompHookPath(home, phase, name string) string {
	return filepath.Join(ompHomeAgent(home), "hooks", phase, name)
}

func TestOmpHookModulesDelivered(t *testing.T) {
	Convey("Given a package carrying omp hook modules", t, func() {
		h, cli, home := ompWorld(t)

		cli.fail[ompHookProbeKey] = response("")

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: ompHookPackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered with hook consent", func() {
			Convey("Then only this host's modules land, verbatim, in pre and post", func() {
				So(err, ShouldBeNil)
				So(fileExists(ompHookPath(home, "pre", "guard.ts")), ShouldBeTrue)
				So(fileExists(ompHookPath(home, "post", "audit.js")), ShouldBeTrue)
				So(fileExists(filepath.Join(ompHomeAgent(home), "hooks", "pre", "other.ts")), ShouldBeFalse)

				So(readTestFile(t, ompHookPath(home, "pre", "guard.ts")), ShouldEqual,
					readTestFile(t, filepath.Join(ompHookFixtureRoot, "runtime", "omp", "hooks", "pre", "guard.ts")))
			})

			Convey("Then the module is a file artifact with its own inverse", func() {
				ops := map[string]receipt.Op{}
				for _, op := range res.RMA {
					ops[op.Path] = op
				}

				So(ops[ompHookPath(home, "pre", "guard.ts")].Kind, ShouldEqual, receipt.OpWriteFile)
				So(ops[ompHookPath(home, "post", "audit.js")].Kind, ShouldEqual, receipt.OpWriteFile)
			})

			Convey("Then the delivery warns that the module runs unsandboxed and ungated", func() {
				notes := strings.Join(res.Notes, "\n")
				So(notes, ShouldContainSubstring, "2 hook module(s) are code the host runs unsandboxed")
				So(notes, ShouldContainSubstring, "no trust gate")
				So(notes, ShouldContainSubstring, "restart omp to load them")
			})

			Convey("Then the load probe ran and the module passed it", func() {
				So(cli.Calls(), ShouldContain, ompHookProbeKey)
			})
		})
	})
}

func TestOmpHookModulesNeedConsent(t *testing.T) {
	Convey("Given the same package without hook consent", t, func() {
		h, cli, home := ompWorld(t)

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: ompHookPackage(t), Strategy: host.Loose})

		Convey("When it is delivered", func() {
			Convey("Then no module is written, nothing is probed and the note names consent", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(ompHomeAgent(home), "hooks")), ShouldBeFalse)
				So(cli.Calls(), ShouldBeEmpty)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "2 host hook module(s) skipped: consent is pending")
			})
		})
	})
}

func TestOmpHookModulesReDelivery(t *testing.T) {
	Convey("Given a delivered hook module and a receipt that owns it", t, func() {
		h, cli, home := ompWorld(t, host.WithOwnership(ownerExisting("acme/caveman")))

		cli.fail[ompHookProbeKey] = response("")

		pkg := ompHookPackage(t)

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})
		So(err, ShouldBeNil)

		target := ompHookPath(home, "pre", "guard.ts")

		Convey("When it is delivered again", func() {
			again, againErr := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

			Convey("Then the same file is rewritten idempotently, marked as pre-existing", func() {
				So(againErr, ShouldBeNil)

				ops := map[string]receipt.Op{}
				for _, op := range again.RMA {
					ops[op.Path] = op
				}

				So(ops[target].Kind, ShouldEqual, receipt.OpWriteFile)
				So(ops[target].Existed, ShouldBeTrue)
				So(ops[target].Backup, ShouldNotBeEmpty)
				So(readTestFile(t, target), ShouldContainSubstring, "blocked by the verger fixture")
			})
		})
	})

	Convey("Given a foreign module at the delivery target", t, func() {
		h, _, home := ompWorld(t)

		foreign := ompHookPath(home, "post", "audit.js")
		writeFixtureFile(t, foreign, "// somebody else's module\n", 0o600)

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: ompHookPackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When the package is delivered", func() {
			Convey("Then the foreign file is left alone and the cell is a collision", func() {
				collision, ok := errors.AsType[*host.CollisionError](err)
				So(ok, ShouldBeTrue)
				So(collision.Path, ShouldEqual, foreign)
				So(readTestFile(t, foreign), ShouldEqual, "// somebody else's module\n")
				So(fileExists(ompHookPath(home, "pre", "guard.ts")), ShouldBeFalse)
			})
		})
	})
}

// TestOmpHookModuleLoadFailure pins the negative leg: omp reports a module that
// does not build as a diagnostic line while the session still exits 0, so the
// probe must read that line and fail the delivery loudly instead of reporting
// success.
func TestOmpHookModuleLoadFailure(t *testing.T) {
	Convey("Given a host that refuses the module it just accepted", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		message := "Failed to load extension ~/.omp/agent/hooks/pre/guard.ts: Failed to load extension: " +
			"Failed to parse extension source for dependency rewriting: 2 errors building\n" +
			"No models available. Use /login or set an API key environment variable.\n"

		h, runner := newOmp(t, home, map[string]hostcli.Response{ompHookProbeKey: response(message)})

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: ompHookPackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When the delivery runs", func() {
			Convey("Then it fails at the verify step and names the module", func() {
				So(callKeys(runner), ShouldContain, ompHookProbeKey)

				failure, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(failure.Step, ShouldEqual, "verify")
				So(failure.Error(), ShouldContainSubstring, "did not load")
				So(failure.Error(), ShouldContainSubstring, "guard.ts")
				So(res.RMA, ShouldNotBeEmpty)
			})
		})
	})
}

// TestOmpHookModuleExtensionRefused pins the discovery rules of the host: a
// module extension omp never auto-discovers is refused with the reason instead
// of being delivered as dead code.
func TestOmpHookModuleExtensionRefused(t *testing.T) {
	Convey("Given a payload with a module extension the host never loads", t, func() {
		h, _, home := ompWorld(t)

		pkg := ompHookPackage(t)
		pkg.Root = copyFixtureTree(t, ompHookFixtureRoot)
		writeFixtureFile(t, filepath.Join(pkg.Root, "runtime", "omp", "hooks", "pre", "dead.mjs"), "export default function () {}\n", 0o600)

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered", func() {
			Convey("Then the cell is refused with the reason and nothing is written", func() {
				unsupported, ok := errors.AsType[*host.NotSupportedError](err)
				So(ok, ShouldBeTrue)
				So(unsupported.Operation, ShouldContainSubstring, ".ts and .js only")
				So(fileExists(filepath.Join(ompHomeAgent(home), "hooks")), ShouldBeFalse)
			})
		})
	})
}

// TestOmpHookModuleApplyRoundTrip pins the exact inverse: the receipt's file op
// removes the module and nothing else.
func TestOmpHookModuleApplyRoundTrip(t *testing.T) {
	Convey("Given an applied hook module delivery", t, func() {
		fakeOmp(t)
		clearOmpEnv(t)

		home := t.TempDir()
		st := openStore(t)

		deps := applyWorld(t, st, ownerMap{})
		owner := receiptsOwner{receipts: deps.Receipts}
		deps.Owned = owner

		cli := newOmpCLI()
		cli.fail[ompHookProbeKey] = response("")

		h := host.NewOmp(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash()), host.WithOwnership(owner))
		deps.Hosts[host.Omp] = h

		pkg := ompHookPackage(t)
		pkg.MCP = nil

		installed := applyCell(t, deps, apply.Action{
			Kind: apply.ActionInstall, Host: host.Omp,
			Delivery: host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true},
		})
		So(installed.Status, ShouldEqual, apply.StatusCurrent)

		rec, found, recErr := deps.Receipts.Get(pkg.ID, string(host.Omp), receipt.ScopeUser)
		So(recErr, ShouldBeNil)
		So(found, ShouldBeTrue)

		Convey("When the package is removed", func() {
			removed := applyCell(t, deps, apply.Action{
				Kind: apply.ActionRemove, Host: host.Omp, Previous: &rec, Cause: "user", Initiator: "omp",
			})

			Convey("Then both modules are gone", func() {
				So(removed.Status, ShouldEqual, apply.StatusCurrent)
				So(fileExists(ompHookPath(home, "pre", "guard.ts")), ShouldBeFalse)
				So(fileExists(ompHookPath(home, "post", "audit.js")), ShouldBeFalse)
			})
		})
	})
}
