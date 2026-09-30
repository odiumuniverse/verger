package host_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/runtime"
)

// TestRuntimeIsDeliveredThroughTheProductionPath pins the claim the queue called
// N029: the hook runtime is not a library nothing calls - it is what a delivery
// actually puts on disk.
//
// Every other proof of this lives behind the `live` tag, because it needs the real
// host binary to load the module. That is the right place for "the host runs the
// hook" and the wrong place for "verger delivers the runtime at all": a machine
// with no pi, omp, kilo or opencode installed would skip the whole claim and
// report a green gate. So this test drives the PRODUCTION entry point - Deliver -
// never runtime_plugin directly, and needs no binary at all.
//
// What it pins is reachability, which is the part that silently rots: nothing
// about a hook module's runtime import failing looks like an error until a user
// has hooks and a tool that denies every call.
func TestRuntimeIsDeliveredThroughTheProductionPath(t *testing.T) {
	Convey("Given a host whose hooks need the runtime bundle", t, func() {
		home := t.TempDir()
		agent := piAgentDir(home)
		So(os.MkdirAll(filepath.Join(agent, "extensions"), 0o700), ShouldBeNil)

		marker := filepath.Join(t.TempDir(), "marker")
		pkg := piPackage(t)
		pkg.Hooks = []manifest.Hook{{
			Event: "SessionStart", Matcher: "", Command: runtimeMarkerCommand(marker), Timeout: 10,
		}}

		st := openStore(t)
		h, _ := newPi(t, home, map[string]hostcli.Response{}, host.WithStore(st),
			host.WithTrash(st.Trash()), host.WithSecrets(piSecrets(t)))

		result, err := h.Deliver(t.Context(), home, host.Delivery{
			Package: pkg, Strategy: host.Loose, AllowHooks: true,
		})
		So(err, ShouldBeNil)

		Convey("When the delivery has run", func() {
			Convey("Then the runtime bundle is installed in the store", func() {
				// Without the bundle the shim's dynamic import fails and it falls
				// back to denying: every tool call blocked, and nothing anywhere
				// saying the hooks were the cause.
				So(runtime.Installed(st, string(host.Pi), runtime.BundleVersion), ShouldBeTrue)

				dir, pathErr := st.RuntimePath(string(host.Pi), runtime.BundleVersion)
				So(pathErr, ShouldBeNil)
				So(fileExists(runtime.BundlePath(dir)), ShouldBeTrue)
			})

			Convey("Then the rendered shim is placed where the host scans", func() {
				shim := filepath.Join(agent, "extensions", "verger-"+strings.ReplaceAll(pkg.ID, "/", "-")+".ts")
				So(fileExists(shim), ShouldBeTrue)

				Convey("And it points at the very bundle that was just installed", func() {
					dir, pathErr := st.RuntimePath(string(host.Pi), runtime.BundleVersion)
					So(pathErr, ShouldBeNil)

					body, readErr := os.ReadFile(shim) //nolint:gosec // G304: the shim this test just had written, under t.TempDir()
					So(readErr, ShouldBeNil)
					// Not merely "a runtime": the bundle installed a moment ago.
					// A shim pointing at some other version's copy is the failure
					// that looks fine - it loads, and then refuses because the
					// consent hash in that copy's manifest was not approved.
					So(string(body), ShouldContainSubstring, runtime.BundlePath(dir))
				})

				Convey("And it pins the manifest consent is checked against", func() {
					body, readErr := os.ReadFile(shim) //nolint:gosec // G304: the shim this test just had written, under t.TempDir()
					So(readErr, ShouldBeNil)
					// The manifest is what the runtime verifies the approved hooks
					// against; a shim without one runs whatever is on disk.
					So(string(body), ShouldContainSubstring, "manifestPath")
					So(string(body), ShouldContainSubstring, "verger-runtime.log")
				})
			})

			Convey("Then the manifest the runtime verifies against is on disk", func() {
				// It lives with the package's data, not beside the plugin: the
				// plugin directory is replaced on every shim rewrite and the
				// manifest is the record that has to survive it.
				matches, globErr := filepath.Glob(filepath.Join(st.DataDir(), pkg.ID, string(host.Pi), "packages", "*.json"))
				So(globErr, ShouldBeNil)
				So(matches, ShouldNotBeEmpty)
			})

			Convey("Then the delivery reported the hooks rather than skipping them", func() {
				// Not merely placed: the note list is what a user reads. A delivery
				// that placed the module and also said "skipped" would be
				// reporting two contradictory things at once, and only the note
				// survives into a bug report.
				So(strings.Join(result.Notes, "\n"), ShouldNotContainSubstring, "hook(s) skipped")
				So(result.Strategy, ShouldEqual, host.Loose)
			})
		})
	})
}

// runtimeMarkerCommand is a hook command that leaves evidence, so a delivery that
// reported hooks as placed can be told apart from one that merely ran the
// placement. It is defined here rather than shared with the live suite because
// that file is behind the `live` tag and this test must run without any binary.
func runtimeMarkerCommand(marker string) string {
	return "node -e \"require('fs').appendFileSync('" + marker + "','ran\\n')\""
}
