package verger

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/receipt"
)

// stubHost is one adapter double for the facade options.
type stubHost struct {
	id host.ID
}

func (s stubHost) ID() host.ID         { return s.id }
func (s stubHost) Detect(string) bool  { return true }
func (s stubHost) Oracle() host.Oracle { return stubOracle{} }
func (s stubHost) Deliver(context.Context, string, host.Delivery) (host.Result, error) {
	return host.Result{Strategy: host.Loose}, nil
}

func (s stubHost) Uninstall(context.Context, string, receipt.Receipt) (host.Result, error) {
	return host.Result{}, nil
}

// stubOracle is an empty oracle.
type stubOracle struct{}

func (stubOracle) List(context.Context) ([]host.Installed, error) { return nil, nil }
func (stubOracle) Validate(context.Context, string) ([]string, error) {
	return nil, nil
}

func TestClientHosts(t *testing.T) {
	Convey("Given an explicit host list", t, func() {
		first := stubHost{id: host.Claude}
		second := stubHost{id: host.Codex}

		client, err := Open(t.Context(),
			WithHome(t.TempDir()),
			WithStore(t.TempDir()),
			WithHosts(first, second),
		)

		Convey("When the client opens", func() {
			Convey("Then Hosts returns the adapters in order", func() {
				So(err, ShouldBeNil)
				So(client.Hosts(), ShouldResemble, []host.Host{first, second})
			})

			Convey("Then the returned slice is a copy", func() {
				So(err, ShouldBeNil)

				hosts := client.Hosts()
				hosts[0] = stubHost{id: host.Gemini}

				So(client.Hosts()[0].ID(), ShouldEqual, host.Claude)
			})
		})
	})

	Convey("Given no explicit host list", t, func() {
		client, err := Open(t.Context(), WithHome(t.TempDir()), WithStore(t.TempDir()))

		Convey("When the client opens", func() {
			Convey("Then the ten registered adapters are built (DESIGN §9.1)", func() {
				So(err, ShouldBeNil)

				ids := make([]host.ID, 0, len(client.Hosts()))

				for _, adapter := range client.Hosts() {
					ids = append(ids, adapter.ID())
				}

				// The set is what the spec fixes; the order is the
				// registration order the CLI already used, so a status table
				// rendered from it does not change.
				slices.Sort(ids)

				declared := slices.Clone(host.All())
				slices.Sort(declared)

				So(ids, ShouldResemble, declared)
			})

			Convey("Then building them created no home or store layout", func() {
				So(err, ShouldBeNil)
				// Both roots above are t.TempDir()s and exist by construction;
				// what Open must not do is lay the home or the store out.
				assertMissing(t, client.Home().StateDir())
				assertMissing(t, filepath.Join(client.Store().Root(), "trash"))
			})
		})
	})

	Convey("Given a nil adapter", t, func() {
		Convey("When the client opens", func() {
			_, err := Open(t.Context(), WithHome(t.TempDir()), WithStore(t.TempDir()), WithHosts(nil))

			Convey("Then it is an *OpenError naming hosts", func() {
				typed, ok := errors.AsType[*OpenError](err)
				So(ok, ShouldBeTrue)
				So(typed.Option, ShouldEqual, OptionHosts)
			})
		})
	})
}
