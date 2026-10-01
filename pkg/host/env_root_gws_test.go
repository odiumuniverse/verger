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
func TestTheDSHAdapterAndTheHostpathTableAgreeOnTheRoot(t *testing.T) {
	Convey("Given a DSH root the environment spelled messily", t, func() {
		t.Setenv("DSH_HOME", "/x//y/")

		Convey("Then the adapter and the root table name the same directory", func() {
			So(dshHome("/home/u"), ShouldEqual, "/x/y")

			roots, err := hostpath.Roots(hostpath.DSH, hostpath.Env{
				Home: "/home/u",
				GOOS: "darwin",
				Lookup: func(name string) (string, bool) {
					if name == hostpath.DSHHome {
						return "/x//y/", true
					}

					return "", false
				},
			})
			So(err, ShouldBeNil)
			So(roots.ConfigRoot, ShouldEqual, dshHome("/home/u"))
		})
	})

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
