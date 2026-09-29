package source

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// A ref can parse and then lose its directory: the spec named it a minute
// ago, someone removed it, the vault is on a filesystem that came and went.
// Parsing checked existence once, at parse time; the fetch is a second,
// later moment and has to answer for itself. Without the check the digest
// walk and the manifest read both run against nothing, and the failure that
// reaches the user is whatever those two happened to say.
func TestFetchingALocalPathThatIsGoneIsANamedError(t *testing.T) {
	Convey("Given a local ref whose directory no longer exists", t, func() {
		gone := filepath.Join(t.TempDir(), "removed")

		// The directory is there when the ref is parsed, and gone by the
		// time it is fetched — the shape a remounted volume produces.
		So(os.MkdirAll(gone, 0o700), ShouldBeNil)

		ref, err := ParseWithBase(gone, "")
		So(err, ShouldBeNil)

		So(os.RemoveAll(gone), ShouldBeNil)

		fetcher, err := NewFetcher()
		So(err, ShouldBeNil)

		_, fetchErr := fetcher.Fetch(t.Context(), ref)

		Convey("Then the fetch names the path rather than failing downstream", func() {
			So(fetchErr, ShouldNotBeNil)
			So(fetchErr.Error(), ShouldContainSubstring, gone)
		})
	})

	Convey("Given a local ref that points at a plain file", t, func() {
		dir := t.TempDir()
		file := filepath.Join(dir, "pkg.tar.gz")
		So(os.WriteFile(file, []byte("x"), 0o600), ShouldBeNil)

		// A file cannot be a local ref at all, so the parser refuses it;
		// this pins that the refusal happens before anything is read.
		_, err := ParseWithBase(file, "")

		Convey("Then the parser says it is not a directory", func() {
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "not a directory")
		})
	})
}
