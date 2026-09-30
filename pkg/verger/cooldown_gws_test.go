package verger

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/receipt"
)

// `spec.Defaults.Cooldown` has been in the spec since the first version and had
// no consumer: nothing throttled anything. These tests pin the decision the
// throttle makes, which is the part that can be wrong: which stamp counts, when
// the window is open, and what happens to a package this machine never wrote.
func TestCooldownHoldDecidesFromTheNewestWrite(t *testing.T) {
	Convey("Given a 24h window and receipts from two hosts", t, func() {
		written := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

		stamps := lastUpdated([]receipt.Receipt{
			{Package: "local:caveman", Host: "claude", InstalledAt: written, UpdatedAt: written.Add(time.Hour)},
			{Package: "local:caveman", Host: "cursor", InstalledAt: written, UpdatedAt: written},
			{Package: "local:other", Host: "claude", InstalledAt: written.Add(-72 * time.Hour)},
		})

		Convey("Then one package has one stamp, the newest of its hosts", func() {
			// A throttle measured per host would let a package through as soon
			// as its quietest host aged out, which is not what "do not update
			// this package for a day" means.
			So(stamps["local:caveman"], ShouldEqual, written.Add(time.Hour))
			So(stamps["local:other"], ShouldEqual, written.Add(-72*time.Hour))
		})

		Convey("When the window is open", func() {
			hold := cooldownHold{active: true, window: 24 * time.Hour, now: written.Add(3 * time.Hour)}

			Convey("Then a package written inside it is deferred, with the time it opens", func() {
				until, held := hold.holds("local:caveman", stamps)
				So(held, ShouldBeTrue)
				So(until, ShouldEqual, written.Add(25*time.Hour))
			})
		})

		Convey("When the window has passed", func() {
			hold := cooldownHold{active: true, window: 24 * time.Hour, now: written.Add(48 * time.Hour)}

			Convey("Then nothing is deferred", func() {
				_, held := hold.holds("local:caveman", stamps)
				So(held, ShouldBeFalse)
			})
		})

		Convey("When the hold is inactive - a reconcile, or a forced update", func() {
			hold := cooldownHold{active: false, window: 24 * time.Hour, now: written.Add(time.Minute)}

			Convey("Then nothing is deferred even inside the window", func() {
				// A throttle that reached a reconcile would make `sync` refuse
				// to fix a drifted file for a day, and a throttle that reached
				// --force would make --force a lie.
				_, held := hold.holds("local:caveman", stamps)
				So(held, ShouldBeFalse)
			})
		})

		Convey("When the package was never written on this machine", func() {
			hold := cooldownHold{active: true, window: 24 * time.Hour, now: written}

			Convey("Then it is not deferred", func() {
				// There is nothing to throttle: an install that has not
				// happened yet cannot be too soon.
				_, held := hold.holds("local:never-installed", stamps)
				So(held, ShouldBeFalse)
			})
		})

		Convey("When a receipt carries no UpdatedAt", func() {
			Convey("Then the install time is used instead of a zero stamp", func() {
				// A zero stamp would make every package look infinitely old and
				// the throttle would never fire for a package installed by a
				// build that did not write the field.
				fell := lastUpdated([]receipt.Receipt{{Package: "local:x", InstalledAt: written}})
				So(fell["local:x"], ShouldEqual, written)
			})
		})
	})
}

// TestWithClockSetsTheClientClock pins the option itself: a throttle is the one
// thing in the library that cannot be exercised against a real clock, so the
// clock has to be injectable or the window is untestable by construction.
func TestWithClockSetsTheClientClock(t *testing.T) {
	Convey("Given a fixed instant", t, func() {
		at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

		Convey("When a client is opened with that clock", func() {
			newFacadeWorld(t)

			clocked, err := Open(t.Context(), WithClock(func() time.Time { return at }))
			So(err, ShouldBeNil)

			defer func() { _ = clocked.Close() }()

			Convey("Then the client reads that instant", func() {
				So(clocked.now(), ShouldEqual, at)
			})
		})

		Convey("When a client is opened without one", func() {
			_, client := newFacadeWorld(t)

			Convey("Then it falls back to the real clock", func() {
				// The zero value has to be a working clock, not a nil call: a
				// caller that never heard of WithClock still gets a client.
				So(client.now, ShouldNotBeNil)
				So(client.now().IsZero(), ShouldBeFalse)
			})
		})
	})
}

// TestUpdateCooldownSkipsInsideTheWindowAndRunsAfter is the behaviour a user
// sees, end to end: install, then update inside the window, then the same
// update after it.
//
// The two halves are observed differently on purpose. Inside the window the
// hold is decided before anything is fetched, so the run really does finish,
// with the package skipped and a reason. After the window the run gets past the
// hold and on to the source - which this fixture does not provide, so it fails
// there. Reaching that failure IS the assertion: the cooldown stopped being the
// thing that stopped it. Asserting an install here instead would make the test
// about the resolver, and the resolver has its own tests.
func TestUpdateCooldownSkipsInsideTheWindowAndRunsAfter(t *testing.T) {
	Convey("Given an installed package and a spec with a 24h cooldown", t, func() {
		world, client := newFacadeWorld(t)

		ref := world.fixture(t, "caveman", "1.0.0")
		world.fake.artifacts = []fakeArtifact{{Path: filepath.Join(world.root, "host", "one.md"), Data: "# one\n"}}

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		written := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

		plan, err := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})
		So(err, ShouldBeNil)

		_, err = client.Install(t.Context(), plan, ApplyOptions{Now: func() time.Time { return written }})
		So(err, ShouldBeNil)

		Convey("Then the receipt the install wrote carries a time to measure from", func() {
			// This is the fact the whole cooldown rests on. A receipt with no
			// stamp cannot be throttled, and nothing about the run would say
			// so.
			records, listErr := receipt.NewStore(paths.ReceiptsDir).List()
			So(listErr, ShouldBeNil)
			So(records, ShouldNotBeEmpty)
			So(records[0].UpdatedAt.IsZero(), ShouldBeFalse)
		})

		world.writeSpec(t, paths, "schema = 1\n\n[defaults]\ncooldown = \"24h\"\n\n"+
			"[[package]]\nid = \"local:caveman\"\nversion = \"2.0.0\"\n")

		Convey("When an update runs an hour after the install", func() {
			later := openClocked(t, world, written.Add(time.Hour))

			updatePlan, _, err := later.Update(t.Context(), UpdateOptions{Paths: paths})

			Convey("Then it succeeds and skips the package", func() {
				So(err, ShouldBeNil)
				So(updatePlan.Cells, ShouldBeEmpty)
			})

			Convey("Then the reason names the window and when it opens", func() {
				// A silent skip reads as "nothing to do", and a user who wrote
				// the cooldown has to see it working or they will turn it off.
				So(joinNotes(updatePlan.Notes), ShouldContainSubstring, "cooldown")
			})
		})

		Convey("When the same update runs after the window", func() {
			later := openClocked(t, world, written.Add(48*time.Hour))

			_, _, err := later.Update(t.Context(), UpdateOptions{Paths: paths})

			Convey("Then the cooldown is no longer what stops it", func() {
				So(err, ShouldNotBeNil)
			})
		})

		Convey("When the same update is forced inside the window", func() {
			later := openClocked(t, world, written.Add(time.Hour))

			// --force and an explicit version both mean the user named what
			// they want, so the throttle is for "pull something newer" and not
			// for them. It has to reach the source here too.
			_, _, err := later.Update(t.Context(), UpdateOptions{Paths: paths, Force: true})

			Convey("Then the cooldown does not hold it", func() {
				So(err, ShouldNotBeNil)
			})
		})
	})
}

func openClocked(t *testing.T, world *facadeWorld, at time.Time) *Client {
	t.Helper()

	c, err := Open(t.Context(), WithHosts(world.fake), WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatalf("open clocked: %v", err)
	}

	t.Cleanup(func() { _ = c.Close() })

	return c
}

func joinNotes(notes []string) string {
	var b strings.Builder

	for _, note := range notes {
		b.WriteString(note)
		b.WriteByte('\n')
	}

	return b.String()
}
