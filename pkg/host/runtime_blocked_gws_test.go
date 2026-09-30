package host_test

import (
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
)

// A shim in a host's plugin directory IS a hook to that host: a module it
// imports and runs. So a host whose hooks are blocked must get no module either,
// and the cell must never read as delivered.
//
// This is the state kilo, pi and omp are in. Their placement is written and
// proven to LOAD — the module is found, the runtime's heartbeat is written — and
// a module that loads is not a hook that runs. VERIFY-CP-verger-pR-2 caught
// exactly that gap: the module loaded on all three and the hook did not execute
// on pi and omp, while the report said `delivered`.
//
// A host leaves this table one green live test at a time: a delivered hook that
// writes a marker, a real host, and the marker read back. Nothing weaker
// unblocks it.
func TestHostsWithUnprovenHookExecutionStayBlocked(t *testing.T) {
	cases := []struct {
		id   host.ID
		fake func(t *testing.T)
		new  func(t *testing.T, home string, opts ...host.Option) host.Host
		// module is where the shim would land, under the config root the
		// adapter resolved for this home.
		module func(t *testing.T, home string) string
	}{
		{
			id:   host.OpenCode,
			fake: fakeOpenCode,
			new: func(t *testing.T, home string, opts ...host.Option) host.Host {
				t.Helper()

				h, _ := newOpenCode(t, home, nil, opts...)

				return h
			},
			module: func(t *testing.T, home string) string {
				t.Helper()

				return filepath.Join(openCodeTestEnv(t, home), "verger-acme-caveman", "index.js")
			},
		},

		{
			id:   host.Kilo,
			fake: fakeKilo,
			new: func(t *testing.T, home string, opts ...host.Option) host.Host {
				t.Helper()

				h, _ := newKilo(t, home, nil, opts...)

				return h
			},
			module: func(t *testing.T, home string) string {
				t.Helper()

				return filepath.Join(kiloTestEnv(t, home), "plugin", "verger-acme-caveman.ts")
			},
		},
		{
			id:   host.Omp,
			fake: fakeOmp,
			new: func(t *testing.T, home string, opts ...host.Option) host.Host {
				t.Helper()

				h, _ := newOmp(t, home, nil, opts...)

				return h
			},
			module: func(_ *testing.T, home string) string {
				return filepath.Join(ompAgentDir(home), "extensions", "verger-acme-caveman.ts")
			},
		},
	}

	for _, tc := range cases {
		Convey("Given "+string(tc.id)+" and a package that ships hooks", t, func() {
			tc.fake(t)

			home := t.TempDir()

			st := openStore(t)
			h := tc.new(t, home, host.WithHome(home), host.WithStore(st),
				host.WithTrash(st.Trash()), host.WithSecrets(openCodeSecrets(t)))

			pkg := blockedHookFixture(t, tc.id)
			So(pkg.Hooks, ShouldNotBeEmpty)

			result, err := h.Deliver(t.Context(), home, host.Delivery{
				Package: pkg, Strategy: host.Loose, AllowHooks: true,
			})

			Convey("Then the delivery succeeds for everything it does claim", func() {
				So(err, ShouldBeNil)
				So(result.Strategy, ShouldEqual, host.Loose)
			})

			Convey("Then no runtime module is placed, because a module is a hook", func() {
				So(fileExists(tc.module(t, home)), ShouldBeFalse)
			})

			Convey("And the note says the hooks were skipped, and why", func() {
				notes := strings.Join(result.Notes, "\n")
				So(notes, ShouldContainSubstring, "hook(s) skipped")

				// A skipped hook always carries a reason: the wording of the reason changes as
				// the evidence does, an absent one does not.
				So(notes, ShouldContainSubstring, "hook(s) skipped: ")
			})
		})
	}
}

// ompAgentDir is the agent dir of one test home; the adapter resolves the same
// path from PI_CODING_AGENT_DIR or ~/.omp/agent.
func ompAgentDir(home string) string {
	return filepath.Join(home, ".omp", "agent")
}

// blockedHookFixture is the parsed fixture of one host in the table; they share
// a payload shape, and the hooks are the part under test.
func blockedHookFixture(t *testing.T, id host.ID) host.Package {
	t.Helper()

	switch id {
	case host.OpenCode:
		return openCodePackage(t)

	case host.Kilo:
		return kiloPackage(t)
	case host.Pi:
		return piPackage(t)
	case host.Omp:
		return ompPackage(t)
	default:
		t.Fatalf("no fixture for %s", id)

		return host.Package{}
	}
}

// A blocked hook is announced once. The declarative surface and the module
// surface see the same block, and both used to say so, so every delivery report
// for a blocked host carried the same line twice — which reads as two separate
// problems when it is one.
func TestABlockedHookIsAnnouncedOnce(t *testing.T) {
	Convey("Given a host whose hooks are blocked", t, func() {
		fakeOmp(t)

		home := t.TempDir()

		st := openStore(t)
		h, _ := newOmp(t, home, nil, host.WithHome(home), host.WithStore(st),
			host.WithTrash(st.Trash()), host.WithSecrets(ompSecrets(t)))

		pkg := ompPackage(t)
		So(pkg.Hooks, ShouldNotBeEmpty)

		result, err := h.Deliver(t.Context(), home, host.Delivery{
			Package: pkg, Strategy: host.Loose, AllowHooks: true,
		})
		So(err, ShouldBeNil)

		Convey("Then the skip is reported exactly once", func() {
			So(strings.Count(strings.Join(result.Notes, "\n"), "hook(s) skipped"), ShouldEqual, 1)
		})
	})
}
