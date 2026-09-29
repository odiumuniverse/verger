package host_test

import (
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/manifest"
)

// TestBlockedHooksSayWhyAndNameTheTask pins the deferral of the runtime shim.
//
// pkg/runtime is not dead code waiting to be deleted: the bundle and the shim
// renderer are written and proven — live_test.go loads the generated shim
// into real OpenCode, Kilo and Pi. What is missing is the half that PLACES
// it. Until then the adapters skip hooks on those hosts and name the task that
// will place them (T2.3).
//
// That arrangement is only acceptable while it is loud. A skip that said
// nothing, or named no owner, would read exactly like "this package has no
// hooks", and the gap would outlive the schedule that justified it. So the
// reason is pinned here: when T2.3 lands and the shim is written, this test
// fails and points at what to update.
func TestBlockedHooksSayWhyAndNameTheTask(t *testing.T) {
	Convey("Given a package that ships hooks, delivered to a host with no declarative hook surface", t, func() {
		cases := []struct {
			id  host.ID
			new func(t *testing.T, home string, opts ...host.Option) host.Host
		}{
			{host.OpenCode, func(t *testing.T, home string, opts ...host.Option) host.Host {
				t.Helper()

				h, _ := newOpenCode(t, home, nil, opts...)

				return h
			}},
			{host.Kilo, func(t *testing.T, home string, opts ...host.Option) host.Host {
				t.Helper()

				// The suite points XDG_CONFIG_HOME at the shared process
				// home, and Kilo honours it over WithHome. Left alone, this
				// test delivered into every other kilo test's home and the
				// next one read the leftovers as a foreign file.
				kiloTestEnv(t, home)

				h, _ := newKilo(t, home, nil, opts...)

				return h
			}},
		}

		for _, tc := range cases {
			Convey("Then "+string(tc.id)+" names the task instead of dropping them silently", func() {
				home := t.TempDir()
				st := openStore(t)
				// WithHome as well as the delivery argument: the package
				// shares one process HOME, and an adapter that resolves its
				// own base from the environment writes into it whatever
				// home the delivery was addressed to.
				h := tc.new(t, home, host.WithHome(home), host.WithStore(st),
					host.WithTrash(st.Trash()), host.WithSecrets(openCodeSecrets(t)))

				pkg := adapterFixturePackage(t, claudeHookFixtureRoot, manifest.FormatClaude)
				So(pkg.Hooks, ShouldNotBeEmpty)

				result, err := h.Deliver(t.Context(), home, host.Delivery{
					Package: pkg, Strategy: host.Loose, AllowHooks: true,
				})
				So(err, ShouldBeNil)

				notes := strings.Join(result.Notes, "\n")
				So(notes, ShouldContainSubstring, "hook(s) skipped")

				// The wording of the reason is each host's own; what must be
				// common is that the skip is announced and names the task
				// that will place the shim. A host whose note went quiet
				// would read exactly like a package with no hooks.
				So(notes, ShouldContainSubstring, "T2.3")
			})
		}
	})
}

// claudeHookFixtureRoot is a package whose manifest declares hooks.
const claudeHookFixtureRoot = "testdata/claude/acme"
