package hostpath_test

import (
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/hostpath"
)

// The DSH root came from the environment verbatim, while every path built from
// it is cleaned by filepath.Join. Those two spellings are one directory to the
// filesystem and different strings to verger, so the root stopped matching its
// own children — and the containment checks that guard a shared file, and the
// drift checks that decide whether verger may replace something, were comparing
// two spellings of one path.
//
// The spelling is not exotic. macOS hands TMPDIR over with a trailing separator
// and a mktemp template built on it doubles the separator, so a temporary HOME
// is routinely "/var/folders/…/T//t-XXXXXX".
func TestTheDSHRootIsCanonicalWhateverTheEnvironmentSpellsIt(t *testing.T) {
	Convey("Given a DSH root spelled with doubled separators and a trailing slash", t, func() {
		const raw = "/x//y/"

		const canonical = "/x/y"

		roots, err := hostpath.Roots(hostpath.DSH, env("/home/u", darwin, map[string]string{
			hostpath.DSHHome: raw,
		}))

		Convey("Then the root is the directory dsh will actually use", func() {
			So(err, ShouldBeNil)
			So(roots.ConfigRoot, ShouldEqual, canonical)
		})

		Convey("And the root contains the paths built from it", func() {
			// The containment test that was failing: Join cleans, so the raw
			// spelling produced the child "/x/y/profiles" while the root still
			// read "/x//y/" and HasPrefix was false.
			child := filepath.Join(roots.ConfigRoot, "profiles", "default")
			So(strings.HasPrefix(child, roots.ConfigRoot), ShouldBeTrue)
			So(child, ShouldEqual, canonical+"/profiles/default")
		})
	})

	Convey("Given a DSH root that is a dot-slash chain", t, func() {
		roots, err := hostpath.Roots(hostpath.DSH, env("/home/u", darwin, map[string]string{
			hostpath.DSHHome: "/x/y/./z/../z/",
		}))

		Convey("Then it is cleaned", func() {
			So(err, ShouldBeNil)
			So(roots.ConfigRoot, ShouldEqual, "/x/y/z")
		})
	})

	Convey("Given DSH_HOME unset", t, func() {
		roots, err := hostpath.Roots(hostpath.DSH, env("/home/u", darwin, map[string]string{
			hostpath.DSHHome: "",
		}))

		Convey("Then the root is the one below the home", func() {
			So(err, ShouldBeNil)
			So(roots.ConfigRoot, ShouldEqual, filepath.Join("/home/u", ".dsh"))
		})
	})

	Convey("Given a DSH root that is only spaces", t, func() {
		roots, err := hostpath.Roots(hostpath.DSH, env("/home/u", darwin, map[string]string{
			hostpath.DSHHome: "   ",
		}))

		Convey("Then it is a literal directory name, like every other host's", func() {
			// NOT a fallback, and this is the case a trimming helper gets wrong.
			// An earlier draft of this test expected the home here — that is the
			// trimming rule, and trimming is exactly what
			// TestEnvValuesAreTakenLiterally forbids, because these hosts read a
			// padded value as a directory whose name is the padding.
			So(err, ShouldBeNil)
			So(roots.ConfigRoot, ShouldEqual, "   ")
		})
	})
}

// CanonicalRoot cleans; it does not otherwise reinterpret. This is what keeps
// the fix from reaching the hosts that read variables verbatim — kilo and
// opencode take "  /tmp/x  " as a literal directory name and will not read a
// server planted in the trimmed path (measured on kilo 7.8.1).
func TestCanonicalRootIsNotAppliedToHostsThatReadVerbatim(t *testing.T) {
	Convey("Given a kilo root padded with spaces", t, func() {
		const padded = "  /tmp/x  "

		roots, err := hostpath.Roots(hostpath.Kilo, env("/home/u", darwin, map[string]string{
			hostpath.KiloConfigDir: padded,
		}))

		Convey("Then the root keeps the spaces the host will look at", func() {
			So(err, ShouldBeNil)
			So(strings.Contains(roots.ConfigRoot, padded), ShouldBeTrue)
		})
	})

	Convey("Given the helper itself", t, func() {
		Convey("Then it cleans a messy absolute path", func() {
			So(hostpath.CanonicalRoot("/x//y/"), ShouldEqual, "/x/y")
			So(hostpath.CanonicalRoot("/x/y/./z/../z/"), ShouldEqual, "/x/y/z")
		})

		Convey("And it leaves everything else alone", func() {
			So(hostpath.CanonicalRoot(""), ShouldEqual, "")
			So(hostpath.CanonicalRoot("  /tmp/x  "), ShouldEqual, "  /tmp/x  ")
		})
	})
}
