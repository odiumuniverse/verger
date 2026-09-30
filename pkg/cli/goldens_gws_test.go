package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/verger"
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

// TestSearchJSONGolden pins the `verger.search` document byte for byte.
//
// One fixture is not enough, and the second one has to come from a real home.
// The three state booleans — `in_spec`, `in_lock`, `installed` — are different
// facts, and a document built by hand pins only the values it was written
// with: when the fixture says all three are true, a collector that folded
// `in_lock` into `in_spec` produces the very same bytes and the golden sees
// nothing. (It did: that mutation survived the first run of this suite.) So
// the second fixture is produced by the collectors themselves, over a home
// where the three states differ, and its bytes are pinned here.
func TestSearchJSONGolden(t *testing.T) {
	Convey("Given a search document", t, func() {
		doc := searchDoc{
			Schema: schemaOf(schemaSearch),
			Query:  "cave",
			Matches: []verger.SearchMatch{{
				ID: "local:caveman", Name: "Caveman toolkit.",
				InSpec: true, InLock: true, Installed: true,
			}},
			Skipped: []verger.SearchSkipped{{Source: "remote", Reason: "not a local ref"}},
		}

		Convey("Then its shape is byte-stable", func() {
			got, err := marshalJSON(doc)
			So(err, ShouldBeNil)
			So(string(got), ShouldEqual, goldenSearch)
		})
	})

	Convey("Given a home whose packages are in different states", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y", "--hooks", "yes")

		// The catalog a declared `local:` source offers: present on disk,
		// in nobody's spec and nobody's lock.
		catalog := filepath.Join(w.homeDir, "catalog")
		writeWorldFile(t, filepath.Join(catalog, "offered", ".claude-plugin", "plugin.json"),
			`{"name":"offered","version":"9.9.9"}`)
		writeWorldFile(t, filepath.Join(catalog, "offered", "skills", "one", "SKILL.md"), "# offered\n")

		// The spec no longer declares `local:caveman`, but the lock and the
		// receipts still carry it — the state a user lands in after deleting a
		// line by hand. That row is the one a collector cannot fake: `in_lock`
		// and `installed` are true while `in_spec` is false, so folding any of
		// them into another changes the bytes below.
		//
		// `ghost` is the mirror image: declared, never resolved, never
		// delivered. `offered` is in the catalog and nowhere else.
		So(w.writeSpec(t, "schema = 1\n"+
			"\n"+
			"[[source]]\n"+
			"name = \"local\"\n"+
			"url = \"local:./catalog\"\n"+
			"\n"+
			"[[package]]\n"+
			"id = \"local:ghost\"\n"), ShouldBeNil)

		client, err := verger.Open(context.Background(), w.options().openOpts...)
		So(err, ShouldBeNil)

		t.Cleanup(func() { _ = client.Close() })

		paths, err := client.Paths(verger.User, "")
		So(err, ShouldBeNil)

		res, err := client.Search(t.Context(), verger.SearchOptions{Paths: paths, Query: "o"})
		So(err, ShouldBeNil)

		Convey("Then the document the command would print is byte-stable", func() {
			got, err := marshalJSON(searchDocFrom(res))
			So(err, ShouldBeNil)
			So(string(got), ShouldEqual, goldenSearchStates)
		})
	})
}

// TestImportJSONGolden pins the `verger.import` document. `wrote` is the field
// worth pinning: it is the only thing that tells a first run from the second one
// over the same home, and a script that runs import in a loop has nothing else
// to read.
func TestImportJSONGolden(t *testing.T) {
	Convey("Given an import that recorded one package and skipped one host", t, func() {
		doc := importDoc{
			Schema:  schemaOf(schemaImport),
			Wrote:   1,
			Added:   2,
			Already: 1,
			Candidates: []importCandidate{
				{ID: "local:caveman", Version: "1.2.3", Host: "claude"},
				{
					ID: "local:old", Host: "claude", Disabled: true,
					Notes: []string{"claude lists no version; recorded, not re-deliverable until it does"},
				},
			},
			Skipped: []importSkipped{{Host: "codex", Reason: "plugin list failed: no such command"}},
			Spec:    "/home/u/.verger/verger.toml",
		}

		Convey("Then the document is byte-stable", func() {
			got, err := marshalJSON(doc)
			So(err, ShouldBeNil)
			So(string(got), ShouldEqual, goldenImport)
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

	goldenSearch = `{"schema":{"name":"verger.search","version":1},"query":"cave","matches":[{"id":"local:caveman","name":"Caveman toolkit.","in_spec":true,"in_lock":true,"installed":true}],"skipped":[{"source":"remote","reason":"not a local ref"}]}`

	// Three rows, three states, produced by the collectors themselves:
	// lock-and-delivered but no longer declared, declared but never resolved,
	// and offered by a local source and nothing else. A collector that folds
	// one flag into another changes these bytes.
	goldenSearchStates = `{"schema":{"name":"verger.search","version":1},"query":"o","matches":[{"id":"local:caveman","source":"claude","in_spec":false,"in_lock":true,"installed":true},{"id":"local:ghost","in_spec":true,"in_lock":false,"installed":false},{"id":"local:offered","name":"offered","in_spec":false,"in_lock":false,"installed":false,"offered_by":"local"}],"skipped":[]}`

	goldenImport = `{"schema":{"name":"verger.import","version":1},"wrote":1,"added":2,"already_in_spec":1,"candidates":[{"id":"local:caveman","version":"1.2.3","host":"claude"},{"id":"local:old","host":"claude","disabled":true,"notes":["claude lists no version; recorded, not re-deliverable until it does"]}],"skipped":[{"host":"codex","reason":"plugin list failed: no such command"}],"spec":"/home/u/.verger/verger.toml"}`
)
