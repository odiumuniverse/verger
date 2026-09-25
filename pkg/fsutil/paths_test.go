package fsutil_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/fsutil"
)

func TestExpandHome(t *testing.T) {
	Convey("Given the isolated home directory", t, func() {
		home, err := os.UserHomeDir()
		So(err, ShouldBeNil)

		tests := map[string]string{
			"~":         home,
			"~/":        home,
			"~/x":       filepath.Join(home, "x"),
			"~/a/b/c":   filepath.Join(home, "a", "b", "c"),
			"/abs/path": "/abs/path",
			"relative":  "relative",
			"":          "",
			"~user":     "~user",
			"~x":        "~x",
			"a~b":       "a~b",
		}

		for in, want := range tests {
			Convey("When expanding "+in, func() {
				got, err := fsutil.ExpandHome(in)

				Convey("Then the path matches", func() {
					So(err, ShouldBeNil)
					So(got, ShouldEqual, want)
				})
			})
		}
	})
}

func TestExpandHomeWithoutHome(t *testing.T) {
	Convey("Given HOME unset", t, func() {
		t.Setenv("HOME", "")

		Convey("When tilde and plain paths are expanded", func() {
			_, err := fsutil.ExpandHome("~")
			So(err, ShouldBeError)

			_, err = fsutil.ExpandHome("~/x")
			So(err, ShouldBeError)

			got, err := fsutil.ExpandHome("~user")

			Convey("Then only expanded paths need a home", func() {
				So(err, ShouldBeNil)
				So(got, ShouldEqual, "~user")

				plain, err := fsutil.ExpandHome("/abs/path")
				So(err, ShouldBeNil)
				So(plain, ShouldEqual, "/abs/path")
			})
		})
	})
}

func TestExists(t *testing.T) {
	Convey("Given existing, missing and symlinked paths", t, func() {
		dir := t.TempDir()
		file := filepath.Join(dir, "file.txt")
		So(os.WriteFile(file, []byte("x"), 0o600), ShouldBeNil)

		link := filepath.Join(dir, "link")
		So(os.Symlink(file, link), ShouldBeNil)

		broken := filepath.Join(dir, "broken")
		So(os.Symlink(filepath.Join(dir, "missing"), broken), ShouldBeNil)

		Convey("When existence is checked", func() {
			Convey("Then real paths and resolvable links exist, broken links do not", func() {
				So(fsutil.Exists(dir), ShouldBeTrue)
				So(fsutil.Exists(file), ShouldBeTrue)
				So(fsutil.Exists(link), ShouldBeTrue)
				So(fsutil.Exists(broken), ShouldBeFalse)
				So(fsutil.Exists(filepath.Join(dir, "missing")), ShouldBeFalse)
			})
		})
	})
}

func TestExistsPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	Convey("Given an unreadable parent directory", t, func() {
		parent := t.TempDir()
		child := filepath.Join(parent, "child")
		So(os.Mkdir(child, 0o700), ShouldBeNil)
		So(os.Chmod(parent, 0o000), ShouldBeNil)

		t.Cleanup(func() { _ = os.Chmod(parent, 0o700) }) //nolint:gosec // G302: restoring test fixture permissions

		Convey("When existence is checked", func() {
			Convey("Then a permission error means not existing, without panicking", func() {
				So(fsutil.Exists(filepath.Join(parent, "absent")), ShouldBeFalse)

				_, err := os.Stat(child)
				So(errors.Is(err, os.ErrPermission), ShouldBeTrue)
			})
		})
	})
}
