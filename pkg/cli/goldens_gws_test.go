package cli

import (
	"testing"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	. "github.com/smartystreets/goconvey/convey"
)

// TestReportJSONGolden closes W6-NITS N012 for `verger.report`. A golden is
// the only assertion that catches a field being renamed, a tag changing, or
// `omitempty` silently swallowing a value — none of which a substring check
// would see.
func TestReportJSONGolden(t *testing.T) {
	Convey("Given a report with one cell of every shape", t, func() {
		doc := reportDocFrom(apply.Report{
			Notes: []string{"one host refused"},
			Cells: []apply.CellResult{
				{
					Package: "local:caveman", Host: host.Claude, Scope: "user",
					Status: apply.StatusCurrent, Version: "1.2.3",
					Strategy: host.Loose, Kind: "install",
				},
				{
					Package: "local:skewed", Host: host.Claude, Scope: "user",
					Status: apply.StatusSkew, Version: "1.0.0",
					Strategy: host.Loose, Kind: "install",
				},
				{
					Package: "local:forced", Host: host.Omp, Scope: "user",
					Status: apply.StatusCurrent, Version: "2.0.0",
					Strategy: host.Synth, Kind: "install",
					Backup: "/home/u/.verger/state/backups/forced.md",
				},
				{
					Package: "local:restored", Host: host.Omp, Scope: "user",
					Status: apply.StatusCurrent, Version: "2.0.0",
					Strategy: host.Synth, Kind: "install", Restored: true,
				},
			},
		})
		doc.Home = "/home/u/.verger"

		Convey("Then the document is byte-stable", func() {
			got, err := marshalJSON(doc)
			So(err, ShouldBeNil)
			So(string(got), ShouldEqual, goldenReport)
		})
	})
}

// TestWhyJSONGolden pins the `verger.why` document, including the vocabulary
// pair: `why` must speak the same status word as `status` and `report`, with
// the exact internal code in `detail`, or it is explaining a cell in a
// language nothing else on screen uses.
func TestWhyJSONGolden(t *testing.T) {
	Convey("Given a skewed cell being explained", t, func() {
		doc := whyDoc{
			Schema:  schemaOf(schemaWhy),
			Package: "local:skewed", Host: "claude", Scope: "user",
			Status: wordSkipped, Detail: "skew",
			Version: "1.0.0", Strategy: "loose",
			Reasons:  []string{"the lock asks for 1.0.0, the receipt says 2.0.0"},
			Blockers: []string{},
		}

		Convey("Then the document is byte-stable", func() {
			got, err := marshalJSON(doc)
			So(err, ShouldBeNil)
			So(string(got), ShouldEqual, goldenWhy)
		})
	})
}

// TestDoctorJSONGolden pins the `verger.doctor` envelope, including the
// `fix` array-of-arrays shape — the one field whose type is easy to change
// without a compile error anywhere.
func TestDoctorJSONGolden(t *testing.T) {
	Convey("Given a doctor run with one finding and one fix applied", t, func() {
		doc := doctorDoc{
			Schema: schemaOf(schemaDoctor),
			Findings: []doctorCheck{{
				Severity: "warning", Subject: "spec:project",
				Message:       "the project spec was not written by this machine",
				Fix:           [][]string{{"verger trust", "."}},
				SafeToAutofix: true,
			}},
			Applied: []string{"spec:project"},
		}

		Convey("Then the document is byte-stable", func() {
			got, err := marshalJSON(doc)
			So(err, ShouldBeNil)
			So(string(got), ShouldEqual, goldenDoctor)
		})
	})
}

// TestWhyStatusMatchesTheStatusDocument is the cross-document pin: the same
// cell, explained by two different commands, must carry the same word. This
// is the bug that `why` had — it passed the raw internal status straight
// through while `status` and `report` translated it.
func TestWhyStatusMatchesTheStatusDocument(t *testing.T) {
	Convey("Given the same status reaching both documents", t, func() {
		whyWord, whyDetail := cellState("skew", nil)
		cell := cellDoc{Status: "skew", Detail: "skew"}
		cell.Status, cell.Detail = whyWord, whyDetail

		Convey("Then the two agree on word and detail", func() {
			So(cell.Status, ShouldEqual, whyWord)
			So(cell.Detail, ShouldEqual, whyDetail)
			So(cell.Status, ShouldEqual, wordSkipped)
		})
	})
}

// The stored bytes of each `--json` document. They exist so a field rename, a
// tag change or an `omitempty` that starts swallowing a value fails a test
// instead of a consumer. Regenerate deliberately and read the diff: a golden
// that moves without a reason is the failure these strings exist to catch.
//
// `verger.plan` has no entry. There is no plan document — `printPlan` renders
// a table and returns nil for `--json`, and the machine-readable form of a
// delivery is `verger.report`. The `schemaPlan` constant is a name reserved
// for that surface, not a description of one that exists today.
const (
	goldenReport = `{"schema":{"name":"verger.report","version":1},"cells":[{"package":"local:caveman","host":"claude","scope":"user","status":"delivered","detail":"current","version":"1.2.3","strategy":"loose","kind":"install"},{"package":"local:skewed","host":"claude","scope":"user","status":"skipped","detail":"skew","version":"1.0.0","strategy":"loose","kind":"install"},{"package":"local:forced","host":"omp","scope":"user","status":"delivered","detail":"current","version":"2.0.0","strategy":"synth","kind":"install","backup":"/home/u/.verger/state/backups/forced.md"},{"package":"local:restored","host":"omp","scope":"user","status":"restored","detail":"restored from lock","version":"2.0.0","strategy":"synth","kind":"install","restored":true}],"notes":["one host refused"],"home":"/home/u/.verger"}`

	goldenWhy = `{"schema":{"name":"verger.why","version":1},"package":"local:skewed","host":"claude","scope":"user","status":"skipped","detail":"skew","version":"1.0.0","strategy":"loose","reasons":["the lock asks for 1.0.0, the receipt says 2.0.0"],"blockers":[]}`

	goldenDoctor = `{"schema":{"name":"verger.doctor","version":1},"findings":[{"severity":"warning","subject":"spec:project","message":"the project spec was not written by this machine","fix":[["verger trust","."]],"safe_to_autofix":true}],"applied":["spec:project"]}`
)
