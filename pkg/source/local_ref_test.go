package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// These are the behaviours F1 added: an absolute `local:` path, a `~` path, a
// relative path resolved against the manifest's directory rather than the
// process working directory, an existence check, and the `..` refusal.
//
// They are black-box — Parse and ParseWithBase only — because the point is what
// a caller can rely on, not how parseLocal is written. Each case kills one
// mutation; before they existed, deleting the tilde expansion, the existence
// check, the base-dir resolution or the `..` guard all left the suite green.
//
// The fixture directories must EXIST: parseLocal stats the path, so a test
// asserting only the resolved string would pass or fail for the wrong reason.

func TestParseLocalAbsolutePath(t *testing.T) {
	Convey("Given a package at an absolute path", t, func() {
		dir := t.TempDir()
		pkg := filepath.Join(dir, "canon")
		So(os.MkdirAll(pkg, 0o750), ShouldBeNil)

		Convey("Then a local: reference to it resolves to that path", func() {
			ref, err := Parse("local:" + pkg)

			So(err, ShouldBeNil)
			So(ref.Kind, ShouldEqual, KindLocal)
			So(ref.Path, ShouldEqual, pkg)
			So(ref.ID, ShouldEqual, "canon")
		})

		Convey("Then a bare absolute path is the same thing", func() {
			ref, err := Parse(pkg)

			So(err, ShouldBeNil)
			So(ref.Path, ShouldEqual, pkg)
		})
	})
}

func TestParseLocalTildeExpandsToHome(t *testing.T) {
	Convey("Given a package under an isolated home", t, func() {
		home := t.TempDir()
		t.Setenv("HOME", home)

		So(os.MkdirAll(filepath.Join(home, "canon"), 0o750), ShouldBeNil)

		Convey("Then local:~/canon resolves under HOME, not literally", func() {
			ref, err := Parse("local:~/canon")

			So(err, ShouldBeNil)
			So(ref.Path, ShouldEqual, filepath.Join(home, "canon"))

			// The assertion that kills the mutation: the literal string
			// "~/canon" is not an absolute path and is not under home.
			So(ref.Path, ShouldNotContainSubstring, "~")
			So(filepath.IsAbs(ref.Path), ShouldBeTrue)
		})

		Convey("Then a bare ~/canon is NOT a local path — the grammar wants ./, local:, file: or an absolute path", func() {
			// Documenting the boundary matters as much as the case: a bare
			// relative is an owner/repo id, so `~/canon` is a rejected id and
			// not an expanded home path.
			_, err := Parse("~/canon")

			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "invalid owner/repo id")
		})
	})
}

func TestParseLocalRelativeResolvesAgainstBaseDir(t *testing.T) {
	Convey("Given a manifest directory with a package beside it", t, func() {
		manifestDir := t.TempDir()
		So(os.MkdirAll(filepath.Join(manifestDir, "canon"), 0o750), ShouldBeNil)

		// A second directory that is NOT the manifest's, so resolving against
		// the process cwd is a different answer and the mutation is visible.
		elsewhere := t.TempDir()
		t.Chdir(elsewhere)

		Convey("Then a relative local: path resolves against the manifest dir", func() {
			ref, err := ParseWithBase("local:canon", manifestDir)

			So(err, ShouldBeNil)
			So(ref.Path, ShouldEqual, filepath.Join(manifestDir, "canon"))
		})

		Convey("Then a ./relative path resolves against the same dir", func() {
			ref, err := ParseWithBase("./canon", manifestDir)

			So(err, ShouldBeNil)
			So(ref.Path, ShouldEqual, filepath.Join(manifestDir, "canon"))
		})

		Convey("Then a bare relative is a package ID, not a path — it is never baseDir-resolved", func() {
			// The other half of the grammar: only ./, local:, file: and an
			// absolute path are local refs. A bare name is an id, so the
			// manifest directory must not be applied to it.
			ref, err := ParseWithBase("canon", manifestDir)

			So(err, ShouldBeNil)
			So(ref.Kind, ShouldNotEqual, KindLocal)
			So(ref.Path, ShouldBeEmpty)
		})

		Convey("Then a relative path with a subdirectory resolves under it", func() {
			So(os.MkdirAll(filepath.Join(manifestDir, "sub", "pkg"), 0o750), ShouldBeNil)

			ref, err := ParseWithBase("local:sub/pkg", manifestDir)

			So(err, ShouldBeNil)
			So(ref.Path, ShouldEqual, filepath.Join(manifestDir, "sub", "pkg"))
		})
	})
}

func TestParseLocalRejectsAMissingPath(t *testing.T) {
	Convey("Given a path that is not there", t, func() {
		absent := filepath.Join(t.TempDir(), "not-here")

		Convey("Then the reference is refused and says so", func() {
			_, err := Parse("local:" + absent)

			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "does not exist")
			So(err.Error(), ShouldContainSubstring, absent)
		})

		Convey("Then a bare absolute path is refused the same way", func() {
			_, err := Parse(absent)

			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "does not exist")
		})
	})
}

func TestParseLocalRejectsParentSegments(t *testing.T) {
	Convey("Given a reference that climbs out of the manifest", t, func() {
		Convey("Then it is refused, whichever way it is written", func() {
			for _, input := range []string{
				"local:../escape",
				"local:sub/../../escape",
				"local:~/../escape",
				"file:../escape",
			} {
				_, err := ParseWithBase(input, t.TempDir())

				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "must not contain ..")
			}
		})

		Convey("Then a name that merely contains dots is still fine", func() {
			// "a..b" is a legal directory name; the guard is on a whole
			// segment, not on a substring, and this pins the difference.
			dir := t.TempDir()
			So(os.MkdirAll(filepath.Join(dir, "a..b"), 0o750), ShouldBeNil)

			ref, err := ParseWithBase("local:a..b", dir)

			So(err, ShouldBeNil)
			So(ref.Path, ShouldEqual, filepath.Join(dir, "a..b"))
		})
	})
}

func TestParseLocalRejectsAnEmptyPath(t *testing.T) {
	Convey("Given a local reference with nothing after the scheme", t, func() {
		for _, input := range []string{"local:", "local:.", "./"} {
			_, err := ParseWithBase(input, t.TempDir())

			So(err, ShouldNotBeNil)
			So(strings.ToLower(err.Error()), ShouldContainSubstring, "path")
		}

		Convey("Then local:/ is a path — the root exists, so it is not an error", func() {
			// Pinned because "local:/" looks like a missing-path case and is not:
			// the existence check must be about the stat, not about the shape.
			ref, err := Parse("local:/")

			So(err, ShouldBeNil)
			So(ref.Path, ShouldEqual, "/")
		})
	})
}
