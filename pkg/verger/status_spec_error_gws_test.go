package verger

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/spec"
)

// `status` is the command people run when something is wrong, and it is the
// one that must not answer "no cells" when the reason there are no cells is
// that the spec cannot be read. `sync` already refuses loudly on the same
// spec; a reader who ran the read-only command first got a green answer and
// nothing to act on.
func TestStatusRefusesASpecItCannotResolve(t *testing.T) {
	Convey("Given a spec whose source cannot be resolved", t, func() {
		world, client := newFacadeWorld(t)
		t.Chdir(world.root)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		doc, _, err := LoadSpec(paths.SpecPath)
		So(err, ShouldBeNil)

		doc.Sources = []spec.Source{{Name: "acme", URL: "../nope"}}
		doc.Packages = []spec.Package{{ID: "acme/caveman"}}

		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		Convey("Then status says so, and does not answer 'no cells'", func() {
			_, statusErr := client.Status(t.Context(), StatusOptions{Paths: paths})

			So(statusErr, ShouldNotBeNil)
			So(statusErr.Error(), ShouldContainSubstring, "acme")
			So(statusErr.Error(), ShouldContainSubstring, "nope")

			Convey("And it is a usage error, so a script gets exit 2", func() {
				So(Classify(statusErr), ShouldEqual, RefusalUsage)
			})
		})
	})

	Convey("Given no spec at all", t, func() {
		world, client := newFacadeWorld(t)
		_ = world

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		Convey("Then status answers with an empty matrix, because that is true", func() {
			doc, statusErr := client.Status(t.Context(), StatusOptions{Paths: paths})

			So(statusErr, ShouldBeNil)
			So(doc.Cells, ShouldBeEmpty)
		})
	})

	Convey("Given a spec with a source that resolves", t, func() {
		world, client := newFacadeWorld(t)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		doc, _, err := LoadSpec(paths.SpecPath)
		So(err, ShouldBeNil)

		srcDir := filepath.Join(world.root, "caveman")
		So(os.MkdirAll(srcDir, 0o700), ShouldBeNil)

		doc.Sources = []spec.Source{{Name: "acme", URL: "local:" + srcDir}}

		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		Convey("Then status answers, because the spec is readable", func() {
			_, statusErr := client.Status(t.Context(), StatusOptions{Paths: paths})

			So(statusErr, ShouldBeNil)
		})
	})
}
