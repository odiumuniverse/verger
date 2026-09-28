package cli

import (
	"slices"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
)

// TestHostMaturityLevels pins the DESIGN §10.3 ladder: the evidence table
// decides the level, and everything the table does not name — including a host
// verger cannot deliver to — is experimental rather than optimistic.
func TestHostMaturityLevels(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want string
	}{
		{"claude is stable on a live full cycle", string(host.Claude), levelStable},
		{"omp is stable on a live full cycle", string(host.Omp), levelStable},
		{"codex is beta on a green e2e run", string(host.Codex), levelBeta},
		{"gemini stays experimental until a green run", string(host.Gemini), levelExperimental},
		{"a host with no adapter is experimental", string(host.Agy), levelExperimental},
		{"a host with no adapter is experimental even with a binary", string(host.Cursor), levelExperimental},
		{"an id verger never heard of is experimental", "nope", levelExperimental},
		{"an empty host id is experimental", "", levelExperimental},
	}

	for _, tc := range cases {
		Convey("Given "+tc.name, t, func() {
			So(hostMaturity(tc.id), ShouldEqual, tc.want)
		})
	}
}

// TestHostMaturityCoversEveryKnownHost keeps the table honest in both
// directions: a level may only be claimed for a declared id, and every id the
// table does not claim must resolve to experimental.
func TestHostMaturityCoversEveryKnownHost(t *testing.T) {
	Convey("Given the declared host ids and the evidence table", t, func() {
		for _, id := range host.All() {
			Convey("The level of "+string(id), func() {
				level := hostMaturity(string(id))

				So(slices.Contains([]string{levelExperimental, levelBeta, levelStable}, level), ShouldBeTrue)
				So(host.Known(id), ShouldBeTrue)
			})
		}

		Convey("Then no id outside the declared set is claimed", func() {
			for id, level := range hostLevels {
				So(host.Known(id), ShouldBeTrue)
				So(hostMaturity(string(id)), ShouldEqual, level)
			}
		})
	})
}

// TestStatusShowsHostLevel is the output contract of DESIGN §10.3 ("уровень
// зрелости виден в status"): the table gains a LEVEL column and the JSON a
// level per cell, and a host verger cannot deliver to reads experimental.
func TestStatusShowsHostLevel(t *testing.T) {
	Convey("Given a package installed for a stable host", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")

		table, err := w.run("status")
		doc, jsonErr := w.run("status", "--json")

		Convey("When status runs", func() {
			Convey("Then the header and the row carry the level", func() {
				So(err, ShouldBeNil)
				So(table, ShouldContainSubstring, "LEVEL")
				So(table, ShouldContainSubstring, levelStable)
			})

			Convey("Then the JSON cell carries the same level", func() {
				So(jsonErr, ShouldBeNil)
				So(doc, ShouldContainSubstring, `"level":"stable"`)
			})
		})
	})

	Convey("Given a package installed for a host with no adapter", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		agy := newFakeHost(host.Agy)
		agy.files = []fakeFile{{Path: w.target(t, "SKILL.md", "# installed\n"), Data: "# installed\n"}}
		w.hosts = []host.Host{agy}

		w.mustRun(t, "install", w.fixture(t), "-y", "--hosts", "agy")

		table, err := w.run("status")
		doc, jsonErr := w.run("status", "--json")

		Convey("When status runs", func() {
			Convey("Then the cell is experimental, never blank or optimistic", func() {
				So(err, ShouldBeNil)
				So(table, ShouldContainSubstring, "agy")
				So(table, ShouldContainSubstring, levelExperimental)

				So(jsonErr, ShouldBeNil)
				So(doc, ShouldContainSubstring, `"host":"agy"`)
				So(doc, ShouldContainSubstring, `"level":"experimental"`)
			})
		})
	})
}

// TestInstallJSONHasNoLevel keeps the level where DESIGN asks for it: status
// reports maturity, and no other command's stable document grows a field.
func TestInstallJSONHasNoLevel(t *testing.T) {
	Convey("Given an install with a JSON report", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")

		stdout, err := w.run("install", w.fixture(t), "-y", "--json")

		Convey("When install runs", func() {
			Convey("Then its cells stay free of a level field", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldNotContainSubstring, `"level"`)
			})
		})
	})
}
