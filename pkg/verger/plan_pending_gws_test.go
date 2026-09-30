package verger

import (
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
)

// Apply is public and buildInstallActions is not, so a front end can hold a Plan
// and run it without ever passing through Install. That route used to be a hole:
// the plan carried the answer to the hooks question and Apply dropped it on the
// floor, so a delivery that withheld hooks reported a result that said it had
// withheld nothing.
//
// The invariant this pins is that the result is a function of the plan. Whatever
// the plan decided about consent, the result says the same thing — which is only
// checkable through the public pair, Plan then Apply.
func TestResultRepeatsWhateverItsPlanDecided(t *testing.T) {
	Convey("Given a plan built through the public API for a package with hooks", t, func() {
		world, client := newFacadeWorld(t)

		ref := world.withHooks(t)
		target := filepath.Join(world.root, "host", "one.md")
		world.fake.artifacts = []fakeArtifact{{Path: target, Data: "# one\n"}}

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		plan, err := client.Plan(t.Context(), PlanOptions{
			Paths: paths, Refs: []string{ref}, Hosts: []host.Host{world.fake},
		})
		So(err, ShouldBeNil)

		// Asking is what fills the plan in, and it happens where the consent
		// question is actually put — not in Plan, which has no confirmer to put it
		// to. A confirmer that resolves with the default asks nobody.
		opts := ApplyOptions{Confirm: yesConfirmer{}}
		So(client.buildInstallActions(t.Context(), plan, opts), ShouldBeNil)

		Convey("Then the plan itself says the package is pending", func() {
			So(plan.PendingConsent(), ShouldContain, "local:hooked")
		})

		Convey("When that plan is applied", func() {
			report, applyErr := client.Apply(t.Context(), plan, opts)

			Convey("Then the result says pending, naming the same package", func() {
				// Not "the error is exit 5": the exit code is the CLI's mapping. The
				// claim here is that the result carries the fact, because a caller
				// that never goes through the CLI is owed the same information.
				So(applyErr, ShouldBeNil)
				So(report.PendingConsent, ShouldContain, "local:hooked")
			})

			Convey("Then the files still landed", func() {
				So(fileMissing(target), ShouldBeFalse)
			})
		})
	})
}

// The other half: a plan that asked nothing must not be made to look like it did.
// Otherwise the field is a place to invent a consent failure.
func TestResultInventsNoPendingForAPlanThatAskedNothing(t *testing.T) {
	Convey("Given a plan for a package with no hooks", t, func() {
		world, client := newFacadeWorld(t)

		ref := world.fixture(t, "plain", "0.1.0")
		target := filepath.Join(world.root, "host", "plain.md")
		world.fake.artifacts = []fakeArtifact{{Path: target, Data: "# plain\n"}}

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		plan, err := client.Plan(t.Context(), PlanOptions{
			Paths: paths, Refs: []string{ref}, Hosts: []host.Host{world.fake},
		})
		So(err, ShouldBeNil)

		opts := ApplyOptions{Confirm: yesConfirmer{}}
		So(client.buildInstallActions(t.Context(), plan, opts), ShouldBeNil)

		Convey("Then neither the plan nor the result claims anything is pending", func() {
			So(plan.PendingConsent(), ShouldBeEmpty)

			report, applyErr := client.Apply(t.Context(), plan, opts)
			So(applyErr, ShouldBeNil)
			So(report.PendingConsent, ShouldBeEmpty)
		})
	})
}
