package cli

import (
	"encoding/json"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// `remove` of a package that is not installed exits 0 — a script must be able
// to remove a package without first asking whether it is there, and turning
// "already gone" into a failure breaks exactly that script.
//
// What had to change is the reporting. The old text was "no installed cell",
// which reads like a complaint about the arguments, and under --json the branch
// printed NO document at all: a caller could not tell "there was nothing to
// remove" from "the command produced no output". The one that actually shipped
// the silent half of this bug — a removal that removed nothing and said
// nothing — is the second case below.
func TestRemovingAPackageThatIsNotInstalledSaysSo(t *testing.T) {
	Convey("Given a machine with nothing installed", t, func() {
		w := newWorld(t)

		Convey("When the package is removed", func() {
			out, err := w.run("remove", "acme/absent")

			Convey("Then the exit is 0, because remove is idempotent", func() {
				So(err, ShouldBeNil)
			})

			Convey("And the text names the package and the reason", func() {
				So(out, ShouldContainSubstring, "acme/absent")
				So(out, ShouldContainSubstring, "is not installed on any host")
				So(out, ShouldContainSubstring, "nothing to remove")
			})
		})

		Convey("When the same removal asks for JSON", func() {
			out, err := w.run("remove", "acme/absent", "--json")

			Convey("Then it still exits 0", func() {
				So(err, ShouldBeNil)
			})

			Convey("And it emits a document a script can test", func() {
				var doc struct {
					Status string   `json:"status"`
					Cells  []string `json:"cells"`
					Notes  []string `json:"notes"`
				}

				So(json.Unmarshal([]byte(out), &doc), ShouldBeNil)
				So(doc.Status, ShouldEqual, "not-installed")
				So(doc.Cells, ShouldNotBeNil)
				So(strings.Join(doc.Notes, " "), ShouldContainSubstring, "nothing to remove")
			})
		})
	})
}

// The regression that made this worth changing: a package that WAS installed
// produced an empty plan, printed "no installed cell" and exited 0 — the exact
// silent success the remote-archive leg hit. This pins that a package verger
// knows about does NOT take this path, so the "nothing to remove" wording can
// only ever mean what it says.
func TestNotInstalledWordingIsNotUsedForAPackageThatIsInstalled(t *testing.T) {
	Convey("Given a machine where the package IS installed", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")

		out, err := w.run("remove", "caveman", "-y")

		Convey("Then remove does not claim there was nothing to remove", func() {
			So(err, ShouldBeNil)
			So(out, ShouldNotContainSubstring, "nothing to remove")
			So(out, ShouldContainSubstring, "caveman")
		})
	})
}
