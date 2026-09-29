package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	. "github.com/smartystreets/goconvey/convey"
)

// TestForcedRunReportsTheBackup is the test for the promise --force makes in
// its own help text: the previous version is kept, and the report names
// where. A flag that silently destroys a person's file is not a flag; this
// pins the receipt of the destruction.
func TestForcedRunReportsTheBackup(t *testing.T) {
	Convey("Given a forced run that overwrote an edited file", t, func() {
		report := apply.Report{
			Cells: []apply.CellResult{{
				Package: "local:caveman", Host: host.Claude, Scope: "user",
				Status: apply.StatusCurrent, Backup: "/home/u/.verger/state/backups/caveman.md",
			}},
		}

		buf := &bytes.Buffer{}
		a := &app{out: buf}
		So(a.renderReport(report, "/home/u/.verger"), ShouldBeNil)

		Convey("Then the backup path is on screen with the cell", func() {
			So(buf.String(), ShouldContainSubstring, "/home/u/.verger/state/backups/caveman.md")
		})
	})
}

// TestUnforcedRunPrintsNoBackupPath keeps the line from becoming noise: a run
// that changed nothing has no backup to point at.
func TestUnforcedRunPrintsNoBackupPath(t *testing.T) {
	Convey("Given a normal run", t, func() {
		report := apply.Report{
			Cells: []apply.CellResult{{
				Package: "local:caveman", Host: host.Claude, Scope: "user",
				Status: apply.StatusCurrent,
			}},
		}

		buf := &bytes.Buffer{}
		a := &app{out: buf}
		So(a.renderReport(report, "/home/u/.verger"), ShouldBeNil)

		Convey("Then no backup line appears", func() {
			So(buf.String(), ShouldNotContainSubstring, "previous version is kept")
		})
	})
}

// TestRestoredCellIsNotDelivered pins the distinction pR's Restored flag
// exists for: a cell that arrived from the lock had no work done on this
// machine, so calling it "delivered" claims something that never happened.
func TestRestoredCellIsNotDelivered(t *testing.T) {
	Convey("Given a report cell that came from the lock", t, func() {
		report := apply.Report{
			Cells: []apply.CellResult{{
				Package: "local:caveman", Host: host.Claude, Scope: "user",
				Status: apply.StatusCurrent, Restored: true,
			}},
		}

		Convey("When the report is rendered as a table", func() {
			buf := &bytes.Buffer{}
			a := &app{out: buf}
			So(a.renderReport(report, "/home/u/.verger"), ShouldBeNil)

			out := buf.String()

			Convey("Then it reads as restored, not delivered", func() {
				So(out, ShouldContainSubstring, "restored")
				So(out, ShouldNotContainSubstring, "delivered")
			})
		})

		Convey("When the report is rendered as JSON", func() {
			doc := reportDocFrom(report)

			Convey("Then status is restored and the flag is set", func() {
				So(doc.Cells[0].Status, ShouldEqual, wordRestored)
				So(doc.Cells[0].Restored, ShouldBeTrue)
			})
		})
	})
}

// TestReportJSONSpeaksBothVocabularies is the reason the report document was
// routed through the same mapper as the status document: a report cell that
// still said "skew" or "hands-off" would be the one place where --json told
// a user something the table did not.
func TestReportJSONSpeaksBothVocabularies(t *testing.T) {
	Convey("Given a report of every outcome", t, func() {
		report := apply.Report{
			Cells: []apply.CellResult{
				{Package: "a", Host: host.Claude, Status: apply.StatusCurrent},
				{Package: "b", Host: host.Claude, Status: apply.StatusMissing},
				{Package: "c", Host: host.Claude, Status: apply.StatusSkew},
				{Package: "d", Host: host.Claude, Status: apply.StatusForeign},
				{Package: "e", Host: host.Claude, Status: apply.StatusFailed},
			},
		}

		doc := reportDocFrom(report)

		Convey("Then no internal word leaks into status", func() {
			for _, cell := range doc.Cells {
				So(cell.Status, ShouldNotBeIn,
					[]string{"current", "missing", "skew", "foreign", "hands-off", "needs-auth"})
			}
		})

		Convey("Then every cell keeps its exact internal code in detail", func() {
			So(doc.Cells[0].Detail, ShouldEqual, "current")
			So(doc.Cells[1].Detail, ShouldEqual, "missing")
			So(doc.Cells[2].Detail, ShouldEqual, "skew")
			So(doc.Cells[3].Detail, ShouldEqual, "foreign")
		})

		Convey("Then the document round-trips as JSON", func() {
			data, err := marshalJSON(doc)
			So(err, ShouldBeNil)

			var back reportDoc
			So(json.Unmarshal(data, &back), ShouldBeNil)
			So(len(back.Cells), ShouldEqual, 5)
			So(back.Cells[2].Status, ShouldEqual, wordSkipped)
		})
	})
}

// TestForceIsNotPartOfYes pins rule 14 at the flag level: -y accepts
// defaults and never resolves a destructive conflict, so a -y run must not
// carry Force even when the user passed --force nowhere.
func TestForceIsNotPartOfYes(t *testing.T) {
	Convey("Given a plain -y run", t, func() {
		a := &app{yes: true}

		Convey("Then the facade options carry no force", func() {
			So(a.applyOptionsFacade().Force, ShouldBeFalse)
		})
	})

	Convey("Given an explicit --force", t, func() {
		// -y is set only to keep the run off the TTY confirmer, which needs
		// a wired app; the assertion is about Force alone.
		a := &app{yes: true, force: true}

		Convey("Then the facade options carry it", func() {
			So(a.applyOptionsFacade().Force, ShouldBeTrue)
		})
	})
}
