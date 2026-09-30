package verger

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// Recording a fetched bare id as a spec source wrote a source that names no
// place: `name = 'caveman'`, `url = 'acme/caveman'`. The origin of such a
// package is the source that offered it, and that source is already in the
// document — the extra entry was a second, false address for the same package
// and it grew on every install.
func TestABareIDIsNotRecordedAsASpecSource(t *testing.T) {
	Convey("Given a ref that names no place of its own", t, func() {
		ref, err := source.Parse("acme/caveman")
		So(err, ShouldBeNil)

		doc := &spec.Spec{Schema: spec.Schema}

		Convey("Then recording it adds nothing", func() {
			So(AddSpecSource(doc, ref), ShouldBeFalse)
			So(doc.Sources, ShouldBeEmpty)
		})
	})

	Convey("Given a ref that does name a place", t, func() {
		pkgDir := filepath.Join(t.TempDir(), "caveman")
		So(os.MkdirAll(pkgDir, 0o700), ShouldBeNil)

		ref, err := source.Parse(pkgDir)
		So(err, ShouldBeNil)

		doc := &spec.Spec{Schema: spec.Schema}

		Convey("Then it is still recorded, so the origin survives the install", func() {
			So(AddSpecSource(doc, ref), ShouldBeTrue)
			So(doc.Sources, ShouldHaveLength, 1)
		})
	})
}
