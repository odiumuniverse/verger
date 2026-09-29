package verger

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// A local [[source]] is a directory. Either the directory is one package, or
// it holds several — the shape a git source has when it points at a
// marketplace. Which one it is, the directory itself decides, and the spec's
// id decides which package is wanted.
func TestLocalSourceOffersRootPackageOrCatalog(t *testing.T) {
	Convey("Given a local source directory", t, func() {
		root := t.TempDir()
		fetcher := catalogFetcher(t)
		doc := newCatalogSpec("acme", root)

		Convey("Then a directory that is a package resolves as that package", func() {
			pkgDir := writeCatalogPackage(t, root, "solo", "caveman")

			fetched, ref, err := (&Client{}).fetchSpecPackage(
				t.Context(), fetcher, doc, "caveman", "",
			)

			So(err, ShouldBeNil)
			So(fetched, ShouldNotBeNil)
			So(ref, ShouldNotBeNil)
			So(fetched.Package.Name, ShouldEqual, "caveman")
			So(fetched.Ref.Path, ShouldEqual, pkgDir)
		})

		Convey("And a directory that is a catalog resolves owner/name from it", func() {
			_ = writeCatalogPackage(t, root, "caveman", "caveman")

			fetched, _, err := (&Client{}).fetchSpecPackage(
				t.Context(), fetcher, doc, "acme/caveman", "",
			)

			So(err, ShouldBeNil)
			So(fetched, ShouldNotBeNil)
			So(fetched.Package.Name, ShouldEqual, "caveman")
		})

		Convey("And the bare directory name resolves too", func() {
			_ = writeCatalogPackage(t, root, "caveman", "caveman")

			fetched, _, err := (&Client{}).fetchSpecPackage(
				t.Context(), fetcher, doc, "caveman", "",
			)

			So(err, ShouldBeNil)
			So(fetched, ShouldNotBeNil)
		})

		Convey("And a catalog entry is found whatever subdirectory holds it", func() {
			_ = writeCatalogPackage(t, root, "tools", "hammer")

			fetched, _, err := (&Client{}).fetchSpecPackage(
				t.Context(), fetcher, doc, "acme/hammer", "",
			)

			So(err, ShouldBeNil)
			So(fetched, ShouldNotBeNil)
			So(fetched.Package.Name, ShouldEqual, "hammer")
		})
	})
}

// Anything the spec names and the machine cannot get is a mistake in the
// spec, and a mistake has to say so. "nothing to do" with exit 0 reads as
// success and leaves the user with a vault that silently installs nothing.
func TestAnUnresolvableSpecPackageIsALoudError(t *testing.T) {
	Convey("Given a spec naming a package no source provides", t, func() {
		root := t.TempDir()
		_ = writeCatalogPackage(t, root, "caveman", "caveman")

		doc := newCatalogSpec("acme", root)

		Convey("Then the run fails, naming the package and the source", func() {
			_, _, err := (&Client{}).fetchSpecPackage(
				t.Context(), catalogFetcher(t), doc, "acme/missing", "",
			)

			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "acme/missing")
			So(err.Error(), ShouldContainSubstring, "acme")
		})

		Convey("And it is a usage error, so a script gets exit 2", func() {
			_, _, err := (&Client{}).fetchSpecPackage(
				t.Context(), catalogFetcher(t), doc, "acme/missing", "",
			)

			So(err, ShouldNotBeNil)
			_, isUsage := errors.AsType[*UsageError](err)

			So(isUsage, ShouldBeTrue)
		})
	})

	Convey("Given a source that names a path that is not there", t, func() {
		doc := newCatalogSpec("acme", filepath.Join(t.TempDir(), "nope"))

		Convey("Then the reason is the missing path, not silence", func() {
			_, _, err := (&Client{}).fetchSpecPackage(
				t.Context(), catalogFetcher(t), doc, "acme/caveman", "",
			)

			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "does not exist")
		})
	})
	Convey("Given a source that names a sibling of the spec directory", t, func() {
		vault := t.TempDir()
		_ = writeCatalogPackage(t, filepath.Join(vault, "pkgs"), "caveman", "caveman")

		doc := newCatalogSpec("acme", "../pkgs")

		Convey("Then it resolves, because the spec author named it", func() {
			fetched, _, err := (&Client{}).fetchSpecPackage(
				t.Context(), catalogFetcher(t), doc, "acme/caveman", filepath.Join(vault, ".verger"),
			)

			So(err, ShouldBeNil)
			So(fetched, ShouldNotBeNil)
			So(fetched.Package.Name, ShouldEqual, "caveman")
		})
	})
}

// catalogFetcher is a fetcher with no cache and the real clock, which is all
// a local source needs: nothing is copied, so there is nothing to key.
func catalogFetcher(t *testing.T) *source.Fetcher {
	t.Helper()

	f, err := source.NewFetcher()
	So(err, ShouldBeNil)

	return f
}

// newCatalogSpec is a one-source spec naming dir.
func newCatalogSpec(name, dir string) *spec.Spec {
	doc := spec.New()
	doc.Sources = []spec.Source{{Name: name, URL: "local:" + dir}}

	return doc
}

// writeCatalogPackage writes one package directory and returns its path.
func writeCatalogPackage(t *testing.T, root, dir, name string) string {
	t.Helper()

	pkgDir := filepath.Join(root, dir)

	So(os.MkdirAll(filepath.Join(pkgDir, ".claude-plugin"), 0o700), ShouldBeNil)
	So(os.WriteFile(filepath.Join(pkgDir, ".claude-plugin", "plugin.json"),
		[]byte(`{"name":"`+name+`","version":"1.0.0","description":"A toolkit."}`), 0o600), ShouldBeNil)

	return pkgDir
}
