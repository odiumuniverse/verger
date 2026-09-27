package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
)

// reportWith builds one report whose cells carry the given statuses on claude.
func reportWith(statuses ...apply.Status) apply.Report {
	var report apply.Report

	for i, status := range statuses {
		report.Cells = append(report.Cells, apply.CellResult{
			Package: "acme/pkg" + string(rune('a'+i)),
			Host:    host.ID("claude"),
			Scope:   "user",
			Status:  status,
		})
	}

	return report
}

func TestPrintReportFailedCells(t *testing.T) {
	Convey("Given an apply report", t, func() {
		var out bytes.Buffer

		a := &app{out: &out}

		Convey("When every cell is current or a policy outcome", func() {
			err := a.printReport(reportWith(apply.StatusCurrent, apply.StatusHandsOff, apply.StatusForeign, apply.StatusNeedsAuth), "")

			Convey("Then the command succeeds", func() {
				So(err, ShouldBeNil)
			})
		})

		Convey("When a cell failed", func() {
			err := a.printReport(reportWith(apply.StatusCurrent, apply.StatusFailed), "")

			Convey("Then the report is printed and a typed error names the cell", func() {
				failed, ok := errors.AsType[*ApplyFailedError](err)
				So(ok, ShouldBeTrue)
				So(failed.Cells, ShouldResemble, []string{"acme/pkgb@claude"})
				So(err.Error(), ShouldContainSubstring, "acme/pkgb@claude")
				So(out.String(), ShouldContainSubstring, "acme/pkgb")
				So(exitCode(err), ShouldEqual, 1)
			})
		})

		Convey("When a cell is left in skew", func() {
			err := a.printReport(reportWith(apply.StatusSkew), "")

			Convey("Then it is a failure too", func() {
				_, ok := errors.AsType[*ApplyFailedError](err)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When --json is set and a cell failed", func() {
			a.jsonOut = true

			err := a.printReport(reportWith(apply.StatusFailed), "/home")

			Convey("Then the JSON document is still printed on stdout before the error", func() {
				var doc reportDoc
				So(json.Unmarshal(out.Bytes(), &doc), ShouldBeNil)
				So(doc.Cells, ShouldHaveLength, 1)
				So(doc.Cells[0].Status, ShouldEqual, "failed")

				_, ok := errors.AsType[*ApplyFailedError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})
}
