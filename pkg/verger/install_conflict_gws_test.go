package verger

import (
	"errors"
	"os"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
)

// `sync` learned this: a cell the executor refused is not a quiet success, and
// a run that prints "nothing was written" and exits 0 is the one answer a
// script must never be able to read. `install` ran the very same executor and
// then threw that verdict away.
func TestInstallReportsAHandEditedFileAsAConflict(t *testing.T) {
	Convey("Given a delivered file the user edited, and an install", t, func() {
		carried := newCarriedHome(t)

		So(os.WriteFile(carried.target, []byte("# mine\n"), 0o600), ShouldBeNil)

		paths, err := carried.client.Paths(User, "")
		So(err, ShouldBeNil)

		plan, err := carried.client.Plan(t.Context(), PlanOptions{
			Paths: paths, Refs: []string{carried.ref},
		})
		So(err, ShouldBeNil)

		report, err := carried.client.Install(t.Context(), plan, ApplyOptions{
			Hooks: HooksSkip, Now: fixedClock,
		})

		Convey("Then the run is a conflict, not a success", func() {
			So(err, ShouldNotBeNil)

			_, held := errors.AsType[*HandsOffError](err)
			So(held, ShouldBeTrue)
		})

		Convey("And the report still reaches the caller, so the cell can be printed", func() {
			// The report is what names the cell. Returning the error alone
			// would leave the caller with a sentence and no place to point.
			So(report, ShouldNotBeNil)
			So(report.Cells, ShouldNotBeEmpty)
			So(report.Cells[0].Status, ShouldEqual, apply.StatusHandsOff)
		})

		Convey("And the edit is exactly as the user left it", func() {
			data, readErr := os.ReadFile(carried.target) //nolint:gosec // G304: the test names its own fixture
			So(readErr, ShouldBeNil)
			So(string(data), ShouldEqual, "# mine\n")
		})
	})
}

// A package that delivered cleanly is the other half: the verdict must not
// fire when there was nothing to refuse, or every install would exit 3.
func TestInstallSaysNothingWhenThereWasNothingToRefuse(t *testing.T) {
	Convey("Given a delivered file the user has not touched", t, func() {
		carried := newCarriedHome(t)

		paths, err := carried.client.Paths(User, "")
		So(err, ShouldBeNil)

		plan, err := carried.client.Plan(t.Context(), PlanOptions{
			Paths: paths, Refs: []string{carried.ref},
		})
		So(err, ShouldBeNil)

		_, err = carried.client.Install(t.Context(), plan, ApplyOptions{
			Hooks: HooksSkip, Now: fixedClock,
		})

		Convey("Then the install succeeds", func() {
			So(err, ShouldBeNil)
		})
	})
}
