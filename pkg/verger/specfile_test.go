package verger

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// TestAddSpecSourceKeepsALocalRefPortable pins FAIL4-A: `verger install
// local:/abs/path` used to write that absolute path into the spec, so a vault
// cloned to another machine pointed at a path that does not exist there.
//
// A path under the spec directory is stored relative to it — which is exactly
// what source.Parse already resolves — and a path outside keeps its absolute
// spelling and is reported by NonPortableSources.
func TestAddSpecSourceKeepsALocalRefPortable(t *testing.T) {
	Convey("Given a spec directory and a local ref under it", t, func() {
		specDir := t.TempDir()
		inside := filepath.Join(specDir, "vault", "caveman")

		So(os.MkdirAll(inside, 0o700), ShouldBeNil)

		Convey("Then the spec stores a path relative to the spec directory", func() {
			doc := spec.New()
			So(AddSpecSourceAt(doc, localRefAt(t, inside, specDir), specDir), ShouldBeTrue)

			So(doc.Sources, ShouldHaveLength, 1)
			So(doc.Sources[0].URL, ShouldEqual, "./vault/caveman")

			Convey("And it carries no absolute path at all", func() {
				So(doc.Sources[0].URL, ShouldNotStartWith, "/")
			})

			Convey("And nothing is reported as non-portable", func() {
				So(NonPortableSources(doc, specDir), ShouldBeEmpty)
			})

			Convey("And a spec round-trip through TOML keeps the relative form", func() {
				// The rewrite is only worth anything if it survives the
				// write/read the vault actually does.
				path := filepath.Join(specDir, "verger.toml")
				So(SaveSpec(path, doc), ShouldBeNil)

				read, _, err := LoadSpec(path)
				So(err, ShouldBeNil)
				So(read.Sources, ShouldHaveLength, 1)
				So(read.Sources[0].URL, ShouldEqual, "./vault/caveman")
			})
		})
	})

	Convey("Given a local ref outside the spec directory", t, func() {
		specDir := filepath.Join(t.TempDir(), "project")
		outside := filepath.Join(t.TempDir(), "elsewhere", "caveman")

		So(os.MkdirAll(outside, 0o700), ShouldBeNil)

		Convey("Then the spec keeps the absolute path", func() {
			// A relative spelling would need "..", which a local ref is not
			// allowed to contain, so there is nothing better to write.
			doc := spec.New()
			So(AddSpecSourceAt(doc, localRefAt(t, outside, specDir), specDir), ShouldBeTrue)

			So(doc.Sources, ShouldHaveLength, 1)
			So(doc.Sources[0].URL, ShouldEqual, outside)
		})

		Convey("Then the caller is told it will not survive a clone", func() {
			doc := spec.New()
			So(AddSpecSourceAt(doc, localRefAt(t, outside, specDir), specDir), ShouldBeTrue)

			So(NonPortableSources(doc, specDir), ShouldResemble, []string{outside})
		})
	})
}

// TestNonPortableSourcesIgnoresOtherKinds pins that the warning is about
// local paths only: a git or npm source is portable by construction and must
// not be reported.
func TestNonPortableSourcesIgnoresOtherKinds(t *testing.T) {
	Convey("Given a spec with no local source", t, func() {
		doc := spec.New()
		doc.Sources = []spec.Source{
			{Name: "acme", URL: "git+ssh://git@github.com/acme/repo"},
			{Name: "pkg", URL: "npm:some-package"},
		}

		Convey("Then nothing is reported", func() {
			So(NonPortableSources(doc, t.TempDir()), ShouldBeEmpty)
		})
	})
}

// localRefAt builds the ref `verger install local:<dir>` produces for a package
// living at dir.
func localRefAt(t *testing.T, dir, _ string) source.Ref {
	t.Helper()

	parsed, err := source.Parse(string(source.KindLocal) + ":" + dir)
	So(err, ShouldBeNil)

	return parsed
}
