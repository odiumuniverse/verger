package verger

import (
	"errors"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/spec"
)

// `verger search` filters what verger already has. The question a user actually
// asks of a spec with a dozen packages is "which of these do I already have",
// and until this existed the answer was read out of `verger status` by eye.
//
// The rules that shaped it, each from a way this could have lied:
//   - no network. A source that is not local contributes only what is already
//     in the cache, and the row says so; showing a cache entry as a live offer
//     is a claim about the source that the cache cannot support.
//   - a match is a case-insensitive substring over id, name and description,
//     so a user who remembers half the name still finds it.
func TestSearchFiltersSpecLockAndReceipts(t *testing.T) {
	Convey("Given a spec with two packages and one of them installed", t, func() {
		_, client := newFacadeWorld(t)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		doc := spec.New()
		doc.Packages = []spec.Package{{ID: "caveman"}, {ID: "gws-review"}}
		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		Convey("When the user searches for the prefix of one id", func() {
			res, err := client.Search(t.Context(), SearchOptions{
				Paths: paths, Query: "cave",
			})

			Convey("Then only that package comes back", func() {
				So(err, ShouldBeNil)
				So(len(res.Matches), ShouldEqual, 1)
				So(res.Matches[0].ID, ShouldEqual, "caveman")
				So(res.Matches[0].InSpec, ShouldBeTrue)
			})
		})

		Convey("When the query matches nothing", func() {
			res, err := client.Search(t.Context(), SearchOptions{
				Paths: paths, Query: "zzz-not-a-package",
			})

			Convey("Then the result is empty and not an error", func() {
				So(err, ShouldBeNil)
				So(len(res.Matches), ShouldEqual, 0)
			})
		})

		Convey("When the query is the empty string", func() {
			_, err := client.Search(t.Context(), SearchOptions{Paths: paths, Query: ""})

			Convey("Then it is a usage error, not a listing of everything", func() {
				So(err, ShouldNotBeNil)
				var usage *UsageError
				So(errors.As(err, &usage), ShouldBeTrue)
			})
		})
	})
}

// The cache line. A non-local source has no business answering, so if the
// user declared one and we cannot reach it, the row has to carry that fact
// rather than the user discovering it by trusting an empty list.
func TestSearchNeverTouchesTheNetwork(t *testing.T) {
	Convey("Given a spec declaring a non-local source", t, func() {
		_, client := newFacadeWorld(t)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		doc := spec.New()
		doc.Sources = []spec.Source{{Name: "remote", URL: "github:someone/anything"}}
		doc.Packages = []spec.Package{{ID: "caveman"}}
		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		res, err := client.Search(t.Context(), SearchOptions{Paths: paths, Query: "caveman"})

		Convey("Then the search still answers from the spec", func() {
			So(err, ShouldBeNil)
			So(len(res.Matches), ShouldEqual, 1)
		})

		Convey("And the non-local source is reported as cache-only", func() {
			So(err, ShouldBeNil)
			So(res.Skipped, ShouldNotBeEmpty)

			var named bool
			for _, s := range res.Skipped {
				if s.Source == "remote" && s.Reason != "" {
					named = true
				}
			}
			So(named, ShouldBeTrue)
		})
	})
}

// A local source IS searchable, and that is the difference between the two
// rows: local means we may read the directory, non-local means we must not.
func TestSearchReadsDeclaredLocalSources(t *testing.T) {
	Convey("Given a spec declaring a local source with one package", t, func() {
		_, client := newFacadeWorld(t)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		catalog := filepath.Join(t.TempDir(), "catalog")
		writeCatalogPackage(t, catalog, "localpkg", "localpkg")

		doc := spec.New()
		doc.Sources = []spec.Source{{Name: "local", URL: "local:" + catalog}}
		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		res, err := client.Search(t.Context(), SearchOptions{Paths: paths, Query: "localpkg"})

		Convey("Then its offers are in the result and it is not skipped", func() {
			So(err, ShouldBeNil)

			var found bool
			for _, m := range res.Matches {
				if m.ID == "local:localpkg" && m.OfferedBy == "local" {
					found = true
				}
			}
			So(found, ShouldBeTrue)
			So(len(res.Skipped), ShouldEqual, 0)
		})
	})
}
