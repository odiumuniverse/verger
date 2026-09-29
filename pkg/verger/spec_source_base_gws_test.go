package verger

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// A relative [[source]] is a promise that the spec travels with its packages.
// Resolving it against the process working directory breaks that promise the
// moment the two differ, which is the normal case: the spec sits in a vault,
// the process starts wherever the reader happened to be. The failure read as
// "local path … does not exist" and pointed at the wrong directory.
func TestSpecSourceResolvesAgainstTheSpecNotTheWorkingDirectory(t *testing.T) {
	Convey("Given a spec in directory A whose source is a relative path", t, func() {
		specDir := filepath.Join(t.TempDir(), "A")
		elsewhere := filepath.Join(t.TempDir(), "B")
		So(os.MkdirAll(elsewhere, 0o700), ShouldBeNil)

		So(os.MkdirAll(filepath.Join(specDir, "packages", "caveman", ".claude-plugin"), 0o700), ShouldBeNil)
		So(os.WriteFile(filepath.Join(specDir, "packages", "caveman", ".claude-plugin", "plugin.json"),
			[]byte(`{"name":"caveman","version":"1.0.0","description":"A toolkit."}`), 0o600), ShouldBeNil)

		doc := spec.New()
		doc.Sources = []spec.Source{{Name: "vault", URL: "./packages/caveman"}}

		Convey("Then the path resolves next to the spec while the process sits in B", func() {
			t.Chdir(elsewhere)

			ref, err := source.ParseWithBase(doc.Sources[0].URL, specDir)
			So(err, ShouldBeNil)
			So(ref.Path, ShouldEqual, filepath.Join(specDir, "packages", "caveman"))

			Convey("And the same URL resolved against the working directory does not exist", func() {
				_, cwdErr := source.Parse(doc.Sources[0].URL)
				So(cwdErr, ShouldNotBeNil)
			})
		})
	})
}

// The two answers to "is this URL local?" must be one. A spec source that a
// reader would call local but the parser routes elsewhere is a spec that
// fails only for the people who write it in the obvious spelling.
func TestLocalSourceClassificationMatchesTheParser(t *testing.T) {
	Convey("Given the URL forms a spec source may carry", t, func() {
		for _, raw := range []string{"./packages", "../up", "/abs/path", "local:./packages", "file:./packages"} {
			Convey("Then "+raw+" is local for the spec and for the parser alike", func() {
				So(isLocalSourceURL(raw), ShouldBeTrue)
				So(source.IsLocalURL(raw), ShouldBeTrue)
			})
		}

		for _, raw := range []string{"github:o/r", "npm:pkg", "mcp:server", "https://x/y", "~/packages"} {
			Convey("Then "+raw+" is not local for either", func() {
				So(isLocalSourceURL(raw), ShouldBeFalse)
				So(source.IsLocalURL(raw), ShouldBeFalse)
			})
		}
	})
}

// The fetch path itself: a spec in A, the process in B, and the package
// still resolves. This is the shape pS hit in beadle.
func TestFetchSpecPackageFindsThePackageFromAnyDirectory(t *testing.T) {
	Convey("Given a spec in A, the process in B, and a sync that needs the package", t, func() {
		specDir := filepath.Join(t.TempDir(), "A")
		elsewhere := filepath.Join(t.TempDir(), "B")
		So(os.MkdirAll(elsewhere, 0o700), ShouldBeNil)

		pkgDir := filepath.Join(specDir, "packages", "caveman")
		So(os.MkdirAll(filepath.Join(pkgDir, ".claude-plugin"), 0o700), ShouldBeNil)
		So(os.WriteFile(filepath.Join(pkgDir, ".claude-plugin", "plugin.json"),
			[]byte(`{"name":"caveman","version":"1.0.0","description":"A toolkit."}`), 0o600), ShouldBeNil)

		fetcher, err := source.NewFetcher()
		So(err, ShouldBeNil)

		doc := spec.New()
		doc.Sources = []spec.Source{{Name: "vault", URL: "./packages/caveman"}}

		_, _, note := (&Client{}).fetchSpecPackage(
			context.Background(), fetcher, doc, "caveman", specDir,
		)

		Convey("Then the package is found rather than reported missing", func() {
			So(note, ShouldBeEmpty)
		})
	})
}
