package source

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// `file://` named one thing — an archive — and the grammar's own sibling for a
// local directory is a plain path. A spec that wrote `file:///srv/pkgs` got
// "archive refs need a #sha256= pin" for a directory that was sitting right
// there, and the whole run said "nothing to do".
func TestFileURLToADirectoryIsALocalRef(t *testing.T) {
	Convey("Given a directory on disk", t, func() {
		dir := t.TempDir()

		Convey("Then file:// pointing at it is a local ref, not an archive", func() {
			ref, err := Parse("file://" + dir)

			So(err, ShouldBeNil)
			So(ref.Kind, ShouldEqual, KindLocal)
			So(ref.Path, ShouldEqual, dir)
		})

		Convey("And a file:// URL that is not a directory is still an archive ref", func() {
			archive := filepath.Join(dir, "pkg.tar.gz")

			So(os.WriteFile(archive, []byte("x"), 0o600), ShouldBeNil)

			_, err := Parse("file://" + archive)

			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "sha256")
		})
	})
}

// A local source is a directory, so a relative one is resolved against the
// spec's own directory. `..` is refused there by the same rule as everywhere
// else — but the refusal must name the reason rather than leaving the caller
// to guess why nothing was delivered.
func TestParentSegmentsInALocalRefNameTheReason(t *testing.T) {
	Convey("Given a relative local ref that climbs out", t, func() {
		Convey("Then it is refused, and the reason says why", func() {
			_, err := ParseWithBase("../pkgs", t.TempDir())

			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "must not contain ..")
		})
	})
}
