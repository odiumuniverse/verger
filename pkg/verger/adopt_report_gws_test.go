package verger

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
)

// An adoption is a delivery: the package is now the spec's, recorded with
// `adopted_from`. Reporting it as a silent spec edit — which is what happened
// whenever the adopt had nowhere else to deliver — returned a report with no
// cells at all, so `adopt --json` printed an empty matrix for a run that had
// genuinely taken a package over.
func TestAdoptIsReportedAsACell(t *testing.T) {
	Convey("Given a package a host already lists", t, func() {
		world, client := newFacadeWorld(t)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		// The host listing the package is what makes it adoptable.
		world.fake.installed = []host.Installed{{
			Name: "caveman", Version: "1.2.3", Path: world.root + "/host/adopted",
		}}

		plan, err := client.Plan(t.Context(), PlanOptions{
			Paths: paths, Refs: []string{"claude:caveman"},
		})
		So(err, ShouldBeNil)
		So(plan.Adopts, ShouldNotBeEmpty)

		report, err := client.Install(t.Context(), plan, ApplyOptions{Now: fixedClock})
		So(err, ShouldBeNil)
		So(report, ShouldNotBeNil)

		Convey("Then the report names the adoption as a cell of its own", func() {
			var found bool

			for _, cell := range report.Cells {
				if cell.Package == plan.Adopts[0].ID && string(cell.Kind) == "adopt" {
					found = true

					break
				}
			}

			So(found, ShouldBeTrue)
		})

		Convey("And that cell is current, because the package is there and owned", func() {
			for _, cell := range report.Cells {
				if string(cell.Kind) != "adopt" {
					continue
				}

				So(cell.Status, ShouldEqual, apply.StatusCurrent)
			}
		})
	})
}
