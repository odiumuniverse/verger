package verger_test

import (
	"context"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// silencerAdapter is a host with a surface for everything except one thing it
// has no layout for. It implements the OPTIONAL host.Silencer, which is the
// whole point: the ladder must find that by type assertion, so an adapter that
// does not implement the interface is never asked and never lies.
type silencerAdapter struct {
	host.Host
	refuse string // package id this host cannot represent
}

func (s silencerAdapter) Silence(pkg host.Package, _ string) (string, string, bool) {
	if pkg.ID != s.refuse {
		return "", "", false
	}

	return "this host has no layout for a project-scope package",
		"deliver it at user scope, or pick a host that has a project surface", true
}

// plainAdapter is a host with no opinion at all, which is every adapter that
// does not implement Silencer.
type plainAdapter struct {
	host.Host
}

// Given a host that says it cannot represent a package
// When the ladder is asked for a strategy
// does not implement the interface is never asked and never lies.
func TestPickStrategyForReachesTheSilencedRung(t *testing.T) {
	Convey("Given an adapter that silences one package", t, func() {
		adapter := silencerAdapter{Host: host.NewClaude(), refuse: "acme/review-kit"}

		Convey("When the ladder is asked about that package", func() {
			pkg := host.Package{ID: "acme/review-kit"}

			strategy, note := verger.PickStrategyFor(adapter, pkg, source.KindGit)

			Convey("Then the strategy is silenced", func() {
				So(strategy, ShouldEqual, host.Silenced)
			})

			Convey("Then the note carries the reason the host gave", func() {
				So(note, ShouldContainSubstring, "no layout for a project-scope package")
			})

			Convey("Then the note carries what the user would do about it", func() {
				// A reason without a hint is a dead end; a hint without a reason
				// is advice about a problem the user was never shown.
				So(note, ShouldContainSubstring, "user scope")
			})
		})

		Convey("When the ladder is asked about a different package", func() {
			pkg := host.Package{ID: "acme/other"}

			strategy, _ := verger.PickStrategyFor(adapter, pkg, source.KindGit)

			Convey("Then the ordinary ladder answers it, not the silenced rung", func() {
				// Silence is scoped to one package. An adapter that refused
				// everything would be indistinguishable from a broken one.
				So(strategy, ShouldNotEqual, host.Silenced)
			})
		})
	})

	Convey("Given an adapter with no opinion", t, func() {
		adapter := plainAdapter{Host: host.NewClaude()}

		Convey("When the ladder is asked about a package the adapter cannot represent", func() {
			pkg := host.Package{ID: "acme/review-kit"}

			strategy, _ := verger.PickStrategyFor(adapter, pkg, source.KindGit)

			Convey("Then the ladder ends at loose, exactly as it did before", func() {
				// An adapter that does not implement Silencer must be left
				// alone: silence is an assertion, and forcing one out of a host
				// that has no opinion turns "I can deliver that" into a lie.
				So(strategy, ShouldNotEqual, host.Silenced)
			})
		})
	})

	Convey("Given the exported preview entry point", t, func() {
		Convey("When it is called with a package and a host id", func() {
			strategy, _ := verger.PickStrategy(host.Package{ID: "acme/review-kit"}, host.Claude, source.KindGit)

			Convey("Then it still answers from the payload alone", func() {
				// A front end that previews a plan has no adapter, and its
				// answers must not change because the executor grew a rung.
				So(strategy, ShouldNotEqual, host.Silenced)
			})
		})
	})
}

// Given a delivery planned as silenced
// When it is executed
// turned "nothing was written, and here is why" into a failure.
func TestSilencedDeliveryIsAnOutcomeNotAFailure(t *testing.T) {
	Convey("Given a host that would otherwise take a loose delivery", t, func() {
		Convey("When a silenced delivery is executed", func() {
			home := t.TempDir()
			h := host.NewClaude(host.WithHome(home))

			res, err := h.Deliver(context.Background(), home, host.Delivery{
				Package:  host.Package{ID: "acme/review-kit", Version: "1.0.0"},
				Strategy: host.Silenced,
				Note:     "this host has no layout for a project-scope package",
			})

			Convey("Then it does not fail", func() {
				// Silenced is a decision the ladder made. Making it an error
				// turned "nothing was written, and here is why" into a failure,
				// which is the state this rung exists to remove.
				So(err, ShouldBeNil)
			})

			Convey("Then the result says the strategy was silenced", func() {
				So(res.Strategy, ShouldEqual, host.Silenced)
			})

			Convey("Then the note reaches the caller unchanged", func() {
				So(strings.Join(res.Notes, " "), ShouldContainSubstring, "no layout")
			})

			Convey("Then nothing was written to the host", func() {
				So(res.Artifacts, ShouldBeEmpty)
			})
		})
	})
}
