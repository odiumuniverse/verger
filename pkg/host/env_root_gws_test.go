package host

import (
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/hostpath"
)

// Two packages read DSH_HOME: this one, to know where to deliver, and
// pkg/hostpath, to fill the root tables. When they disagreed, verger computed a
// path from one spelling of a directory and compared it against the other — and
// every containment and drift check built on that comparison was quietly
// answering a question about a string rather than about a directory.
//
// This is the test that pins the agreement, because "they both call a helper" is
// wiring and wiring is not the claim. The claim is that the two answers are the
// same string.
//
// The values are a table, and the table is the point. An earlier version of this
// test carried one messy spelling and one fallback, and it passed while the two
// sides disagreed — because the one value where they actually differ was not in
// it. A test with a case list can only catch the cases somebody thought of, so
// the list has to cover the whole space where the two rules could part company:
// `filepath.Clean` collapses separators and dot segments, and does nothing else.
func TestTheDSHAdapterAndTheHostpathTableAgreeOnTheRoot(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{name: "duplicate separators", value: "/x//y/", want: "/x/y"},
		{name: "trailing slash", value: "/x/y/", want: "/x/y"},
		// The one value where a trim and a clean disagree. `Clean` collapses the
		// path; it does not touch the spaces, because a space is a legal
		// character in a directory name. Trimming here pointed verger at a
		// directory no host looks at — kilo 7.8.1 takes "  /tmp/x  " literally
		// and will not read a server moved to the trimmed path.
		{name: "padded with spaces", value: "  /padded  ", want: "  /padded  "},
		{name: "inner dot segments", value: "/x/./y", want: "/x/y"},
		{name: "parent segment", value: "/x/y/../z", want: "/x/z"},
	}

	for _, tc := range cases {
		Convey("Given DSH_HOME "+tc.name, t, func() {
			t.Setenv("DSH_HOME", tc.value)

			Convey("Then the adapter and the root table name the same directory", func() {
				So(dshHome("/home/u"), ShouldEqual, tc.want)

				roots, err := hostpath.Roots(hostpath.DSH, hostpath.Env{
					Home: "/home/u",
					GOOS: "darwin",
					Lookup: func(name string) (string, bool) {
						if name == hostpath.DSHHome {
							return tc.value, true
						}

						return "", false
					},
				})
				So(err, ShouldBeNil)
				So(roots.ConfigRoot, ShouldEqual, dshHome("/home/u"))
			})
		})
	}

	Convey("Given no DSH_HOME", t, func() {
		t.Setenv("DSH_HOME", "")

		Convey("Then both fall back to the same directory below the home", func() {
			So(dshHome("/home/u"), ShouldEqual, filepath.Join("/home/u", ".dsh"))

			roots, err := hostpath.Roots(hostpath.DSH, hostpath.Env{
				Home:   "/home/u",
				GOOS:   "darwin",
				Lookup: func(string) (string, bool) { return "", false },
			})
			So(err, ShouldBeNil)
			So(roots.ConfigRoot, ShouldEqual, dshHome("/home/u"))
		})
	})
}
