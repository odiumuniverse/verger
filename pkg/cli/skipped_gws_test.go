package cli

import (
	"testing"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/exitcode"
	"github.com/odiumuniverse/verger/pkg/host"
	. "github.com/smartystreets/goconvey/convey"
)

// TestSkippedIsMappedNotFailed closes the pR report: the new internal status
// `skipped` (the delivery ran and wrote nothing on purpose) had no case in
// cellState, so it fell through to the default and printed `failed` with
// "we do not recognise the recorded state skipped" — the exact wording the
// default exists to produce for a status this build has never heard of.
func TestSkippedIsMappedNotFailed(t *testing.T) {
	Convey("Given a cell the delivery ran but wrote nothing for", t, func() {
		notes := []string{"nothing to write: the package produced no files for claude"}

		Convey("When it is mapped", func() {
			word, detail := cellState("skipped", notes)

			Convey("Then it reads as skipped and keeps its code", func() {
				So(word, ShouldEqual, wordSkipped)
				So(word, ShouldNotEqual, wordFailed)
				So(detail, ShouldEqual, "skipped")
			})
		})
	})
}

// TestSkippedCarriesItsReason is the other half of the report: a skipped row
// must say why, through the same footnote every other skipped row uses. A
// bare "skipped" would leave the reader to guess whether the package was
// already there or had nothing to offer.
func TestSkippedCarriesItsReason(t *testing.T) {
	Convey("Given a skipped cell with its reason", t, func() {
		word, detail := cellState("skipped", []string{"nothing to write: the package produced no files for claude"})
		st := humanState(word, detail)

		Convey("Then the footnote explains the skip", func() {
			So(st.reason, ShouldContainSubstring, "not delivered on this machine")
		})
	})
}

// TestExitCodeTable is the decision table for the run's verdict. The rule pR
// and the orchestrator settled: a skipped cell is not a failure, so the run
// exits 0 — except a package that produced no file on any host, which exits 2
// because reporting success there would claim work that never happened.
func TestExitCodeTable(t *testing.T) {
	cases := []struct {
		name     string
		cells    []apply.CellResult
		refusals []apply.HostRefusedError
		exit     int
		isFail   bool
	}{
		{
			name: "everything delivered exits ok",
			cells: []apply.CellResult{
				{Package: "a", Host: host.Claude, Status: apply.StatusCurrent},
			},
			exit: exitcode.OK,
		},
		{
			name: "a skipped host alongside a delivered one exits ok",
			cells: []apply.CellResult{
				{Package: "a", Host: host.Claude, Status: apply.StatusCurrent},
				// Skipped on one host only. A package is "empty" when *every*
				// cell is skipped, so a package that delivered somewhere is
				// not empty however many hosts had nothing for it.
				{Package: "b", Host: host.Claude, Status: apply.StatusCurrent},
				{Package: "b", Host: host.Codex, Status: apply.StatusSkipped},
			},
			exit: exitcode.OK,
		},
		{
			name: "every cell skipped exits 2: the package is empty",
			cells: []apply.CellResult{
				{Package: "a", Host: host.Claude, Status: apply.StatusSkipped},
				{Package: "a", Host: host.Codex, Status: apply.StatusSkipped},
			},
			exit:   exitcode.Usage,
			isFail: true,
		},
		{
			name: "one empty package among healthy ones exits 2",
			cells: []apply.CellResult{
				{Package: "a", Host: host.Claude, Status: apply.StatusCurrent},
				{Package: "b", Host: host.Claude, Status: apply.StatusSkipped},
				{Package: "b", Host: host.Codex, Status: apply.StatusSkipped},
			},
			exit:   exitcode.Usage,
			isFail: true,
		},
		{
			// A failed cell is a HOST that did not do what it was asked, which
			// is a thing the user can go and look at. It used to exit 1, the
			// code that means verger itself broke, so a script branching on it
			// could not tell an answer from a crash.
			name: "a failed cell is the host failing, not verger failing",
			cells: []apply.CellResult{
				{Package: "a", Host: host.Claude, Status: apply.StatusFailed},
			},
			isFail: true,
			exit:   exitcode.HostUnavailable,
		},
		{
			name:     "a host that declined the operation names itself, not the cells",
			refusals: []apply.HostRefusedError{{Host: host.Claude, Package: "a", Action: "remove", Output: "a 1.0.0"}},
			isFail:   true,
			exit:     exitcode.HostUnavailable,
		},
		{
			// The refusal outranks the cell list on purpose: "the agent still
			// has the plugin" and "3 cells failed" are different answers, and
			// only one of them can be acted on without reading the report.
			name:     "a refusal wins over the failed cells it came from",
			cells:    []apply.CellResult{{Package: "a", Host: host.Claude, Status: apply.StatusFailed}},
			refusals: []apply.HostRefusedError{{Host: host.Claude, Package: "a", Action: "remove"}},
			isFail:   true,
			exit:     exitcode.HostUnavailable,
		},
	}

	for _, tc := range cases {
		Convey("Given a report: "+tc.name, t, func() {
			err := failedCells(apply.Report{Cells: tc.cells, Refusals: tc.refusals})

			Convey("Then the exit code is the one the table says", func() {
				if tc.isFail {
					So(err, ShouldNotBeNil)
				} else {
					So(err, ShouldBeNil)
				}

				So(exitcode.Classify(err), ShouldEqual, tc.exit)
			})
		})
	}
}

// TestEmptyPackageNamesThePackage keeps the exit-2 message useful: a bare
// exit code with no package name leaves the user to guess which of several
// refs was the empty one.
func TestEmptyPackageNamesThePackage(t *testing.T) {
	Convey("Given two empty packages", t, func() {
		err := failedCells(apply.Report{Cells: []apply.CellResult{
			{Package: "zeta/one", Host: host.Claude, Status: apply.StatusSkipped},
			{Package: "alpha/two", Host: host.Claude, Status: apply.StatusSkipped},
		}})

		Convey("Then the message names both, in a stable order", func() {
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "alpha/two")
			So(err.Error(), ShouldContainSubstring, "zeta/one")
			So(err.Error(), ShouldContainSubstring, "no files for any host")
		})
	})
}

// TestSkippedOnOneHostIsNotAnEmptyPackage is the boundary that makes the
// rule safe: `skipped` also means "this host already had it". A package that
// is merely already-current on one host is not empty, and must not exit 2.
func TestSkippedOnOneHostIsNotAnEmptyPackage(t *testing.T) {
	Convey("Given a package skipped on one host and delivered on another", t, func() {
		err := failedCells(apply.Report{Cells: []apply.CellResult{
			{Package: "a", Host: host.Claude, Status: apply.StatusSkipped},
			{Package: "a", Host: host.Codex, Status: apply.StatusCurrent},
		}})

		Convey("Then the run succeeds", func() {
			So(err, ShouldBeNil)
			So(exitcode.Classify(err), ShouldEqual, exitcode.OK)
		})
	})
}

// TestMixedStatusesInOneReportAreNotCollapsed keeps the verdict honest about
// what it counted: a package with a failed cell and a skipped cell has already
// failed, and the empty-package rule must not relabel that as "the package is
// empty", which would send the user looking in the wrong place.
func TestMixedStatusesInOneReportAreNotCollapsed(t *testing.T) {
	Convey("Given a package that failed rather than produced nothing", t, func() {
		err := failedCells(apply.Report{Cells: []apply.CellResult{
			{Package: "a", Host: host.Claude, Status: apply.StatusFailed},
			{Package: "a", Host: host.Codex, Status: apply.StatusSkipped},
		}})

		Convey("Then the failure is reported as a failure", func() {
			So(err, ShouldNotBeNil)
			So(exitcode.Classify(err), ShouldNotEqual, exitcode.Usage)
		})
	})
}
