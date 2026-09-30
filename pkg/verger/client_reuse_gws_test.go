package verger

import (
	"errors"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
)

// One client serves many runs. That is not a library convenience: beadle's watch
// loop holds a client and reconciles with it over and over, and the CLI only
// avoids the question by opening a fresh client per invocation.
//
// So the pending-consent record has to belong to the run that produced it. When it
// lived on the Client, one run that nobody answered poisoned every run after it: a
// package with no hooks at all — one that asks nothing, so there is nothing to be
// pending about — came back as a consent failure anyway. A caller that reads only
// the exit code would conclude it has hooks to approve that it never shipped.
func TestOneClientIsNotPoisonedByAnEarlierRun(t *testing.T) {
	Convey("Given one client that has already run a delivery nobody consented to", t, func() {
		world, client := newFacadeWorld(t)

		hooked := world.withHooks(t)
		plain := world.fixture(t, "plain", "0.1.0")

		plainTarget := filepath.Join(world.root, "host", "plain.md")
		world.fake.artifacts = []fakeArtifact{{Path: plainTarget, Data: "# plain\n"}}

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		// The same client for both runs, deliberately: the world hands out one, and
		// this test exists to find out what the second run sees on it.
		install := func(ref string) (*apply.Report, error) {
			plan, planErr := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}, Hosts: []host.Host{world.fake}})
			So(planErr, ShouldBeNil)

			return client.Install(t.Context(), plan, ApplyOptions{Confirm: yesConfirmer{}})
		}

		Convey("When a hooked package is installed with a confirmer that answers by default", func() {
			_, firstErr := install(hooked)

			Convey("Then that run is a consent failure, which is exit 5", func() {
				So(firstErr, ShouldNotBeNil)
				So(errors.Is(firstErr, apply.ErrConfirmationRequired), ShouldBeTrue)
			})

			Convey("And then the same client installs a package that asks nothing", func() {
				_, secondErr := install(plain)

				Convey("Then it is a plain success: nothing was pending in this run", func() {
					// The failure this guards against is not a wrong message, it is a
					// run that cannot succeed at all until the client is replaced.
					So(secondErr, ShouldBeNil)
					So(fileMissing(plainTarget), ShouldBeFalse)
				})
			})
		})
	})
}
