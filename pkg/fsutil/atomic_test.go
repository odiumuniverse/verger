package fsutil_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/fsutil"
)

func TestWriteFileAtomic(t *testing.T) {
	Convey("Given an atomic writer", t, func() {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")

		Convey("When a 0600 file is written and then replaced with 0700", func() {
			So(fsutil.WriteFileAtomic(path, []byte("v1"), 0o600), ShouldBeNil)

			info, err := os.Stat(path)
			So(err, ShouldBeNil)

			firstMode := info.Mode().Perm()

			got, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp file
			So(err, ShouldBeNil)

			firstContent := string(got)

			So(fsutil.WriteFileAtomic(path, []byte("v2"), 0o700), ShouldBeNil)

			info, err = os.Stat(path)
			So(err, ShouldBeNil)

			got, err = os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp file
			So(err, ShouldBeNil)

			entries, err := os.ReadDir(dir)

			Convey("Then content and modes are exact and no temp file survives", func() {
				So(firstContent, ShouldEqual, "v1")
				So(firstMode, ShouldEqual, os.FileMode(0o600))
				So(string(got), ShouldEqual, "v2")
				So(info.Mode().Perm(), ShouldEqual, os.FileMode(0o700))
				So(err, ShouldBeNil)
				So(entries, ShouldHaveLength, 1)
				So(entries[0].Name(), ShouldEqual, "config.json")
			})
		})
	})
}

func TestWriteFileAtomicTempName(t *testing.T) {
	Convey("Given a checked writer", t, func() {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")

		var tempName string

		err := fsutil.WriteFileAtomicChecked(path, []byte("v1"), 0o600, func() error {
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				return readErr
			}

			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".config.json.tmp-") {
					tempName = entry.Name()
				}
			}

			return nil
		})

		Convey("When the pre-commit check runs", func() {
			Convey("Then a hidden temp file exists in the target directory", func() {
				So(err, ShouldBeNil)
				So(tempName, ShouldNotBeEmpty)
				So(strings.HasPrefix(tempName, ".config.json.tmp-"), ShouldBeTrue)
			})
		})
	})
}

func TestWriteFileAtomicOverwriteTruncates(t *testing.T) {
	Convey("Given a large file", t, func() {
		path := filepath.Join(t.TempDir(), "data.txt")
		So(fsutil.WriteFileAtomic(path, []byte(strings.Repeat("a", 1<<20)), 0o600), ShouldBeNil)

		Convey("When it is overwritten with one byte and then emptied", func() {
			So(fsutil.WriteFileAtomic(path, []byte("b"), 0o600), ShouldBeNil)

			short, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp file
			So(err, ShouldBeNil)

			So(fsutil.WriteFileAtomic(path, nil, 0o600), ShouldBeNil)

			empty, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp file
			So(err, ShouldBeNil)

			Convey("Then no stale bytes survive", func() {
				So(string(short), ShouldEqual, "b")
				So(empty, ShouldBeEmpty)
			})
		})
	})
}

func TestWriteFileAtomicMissingParent(t *testing.T) {
	Convey("Given a missing parent directory", t, func() {
		path := filepath.Join(t.TempDir(), "missing", "config.json")

		Convey("When the file is written", func() {
			err := fsutil.WriteFileAtomic(path, []byte("v1"), 0o600)

			Convey("Then it fails without creating anything", func() {
				So(err, ShouldBeError)
				So(errors.Is(err, fs.ErrNotExist), ShouldBeTrue)
				So(fsutil.Exists(filepath.Dir(path)), ShouldBeFalse)
			})
		})
	})
}

func TestWriteFileAtomicChecked(t *testing.T) {
	Convey("Given a nil check", t, func() {
		path := filepath.Join(t.TempDir(), "config.json")

		Convey("When the file is written", func() {
			So(fsutil.WriteFileAtomicChecked(path, []byte("v1"), 0o600, nil), ShouldBeNil)

			got, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp file

			Convey("Then it behaves like the plain writer", func() {
				So(err, ShouldBeNil)
				So(string(got), ShouldEqual, "v1")
			})
		})
	})

	Convey("Given a passing check", t, func() {
		path := filepath.Join(t.TempDir(), "config.json")
		So(os.WriteFile(path, []byte("original"), 0o600), ShouldBeNil)

		Convey("When the file is written", func() {
			err := fsutil.WriteFileAtomicChecked(path, []byte("replacement"), 0o600, func() error { return nil })
			So(err, ShouldBeNil)

			got, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp file

			Convey("Then it is replaced", func() {
				So(err, ShouldBeNil)
				So(string(got), ShouldEqual, "replacement")
			})
		})
	})

	Convey("Given a failing check on an existing file", t, func() {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		So(os.WriteFile(path, []byte("original"), 0o600), ShouldBeNil)

		sentinel := errors.New("changed concurrently")

		Convey("When the file is written", func() {
			err := fsutil.WriteFileAtomicChecked(path, []byte("replacement"), 0o600, func() error { return sentinel })
			So(errors.Is(err, sentinel), ShouldBeTrue)

			got, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp file
			So(err, ShouldBeNil)

			entries, err := os.ReadDir(dir)

			Convey("Then the old file is intact and no temp files remain", func() {
				So(err, ShouldBeNil)
				So(string(got), ShouldEqual, "original")
				So(entries, ShouldHaveLength, 1)
			})
		})
	})

	Convey("Given a failing check on a new file", t, func() {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")

		sentinel := errors.New("appeared concurrently")

		Convey("When the file is written", func() {
			err := fsutil.WriteFileAtomicChecked(path, []byte("v1"), 0o600, func() error { return sentinel })
			So(errors.Is(err, sentinel), ShouldBeTrue)

			_, err = os.Stat(path)
			So(errors.Is(err, fs.ErrNotExist), ShouldBeTrue)

			entries, err := os.ReadDir(dir)

			Convey("Then nothing is created and no temp files remain", func() {
				So(err, ShouldBeNil)
				So(entries, ShouldBeEmpty)
			})
		})
	})
}

func TestWriteFileAtomicConcurrent(t *testing.T) {
	Convey("Given two goroutines writing the same path", t, func() {
		path := filepath.Join(t.TempDir(), "config.json")
		bodies := []string{strings.Repeat("a", 4096), strings.Repeat("b", 4096)}

		var group sync.WaitGroup

		for _, body := range bodies {
			group.Go(func() {
				if err := fsutil.WriteFileAtomic(path, []byte(body), 0o600); err != nil {
					t.Errorf("atomic write: %v", err)
				}
			})
		}

		group.Wait()

		got, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp file

		Convey("When both complete", func() {
			Convey("Then the file equals one body, never a mix", func() {
				So(err, ShouldBeNil)
				So(string(got) == bodies[0] || string(got) == bodies[1], ShouldBeTrue)
			})
		})
	})
}

func TestEnsureDir(t *testing.T) {
	Convey("Given a fresh nested path", t, func() {
		leaf := filepath.Join(t.TempDir(), "a", "b", "c")

		Convey("When it is ensured with 0700", func() {
			So(fsutil.EnsureDir(leaf, 0o700), ShouldBeNil)

			info, err := os.Stat(leaf)

			Convey("Then the leaf is a directory with exactly 0700", func() {
				So(err, ShouldBeNil)
				So(info.IsDir(), ShouldBeTrue)
				So(info.Mode().Perm(), ShouldEqual, os.FileMode(0o700))
			})
		})
	})

	Convey("Given an existing directory with a custom mode", t, func() {
		existing := filepath.Join(t.TempDir(), "keep")
		So(os.Mkdir(existing, 0o750), ShouldBeNil)

		Convey("When it is ensured with another mode", func() {
			So(fsutil.EnsureDir(existing, 0o700), ShouldBeNil)

			info, err := os.Stat(existing)

			Convey("Then the existing mode is untouched", func() {
				So(err, ShouldBeNil)
				So(info.Mode().Perm(), ShouldEqual, os.FileMode(0o750))
			})
		})
	})

	Convey("Given a file where a directory belongs", t, func() {
		path := filepath.Join(t.TempDir(), "file.txt")
		So(os.WriteFile(path, []byte("x"), 0o600), ShouldBeNil)

		Convey("When it is ensured", func() {
			err := fsutil.EnsureDir(path, 0o700)

			Convey("Then it errors instead of touching the file", func() {
				So(err, ShouldBeError)
			})
		})
	})
}

func TestEnsureDirExactModeUnderUmask(t *testing.T) {
	Convey("Given a restrictive umask", t, func() {
		previous := syscall.Umask(0o077)

		t.Cleanup(func() { syscall.Umask(previous) })

		leaf := filepath.Join(t.TempDir(), "fresh", "leaf")

		Convey("When a 0755 directory is ensured", func() {
			So(fsutil.EnsureDir(leaf, 0o755), ShouldBeNil)

			info, err := os.Stat(leaf)

			Convey("Then the leaf carries exactly 0755 regardless of umask", func() {
				So(err, ShouldBeNil)
				So(info.Mode().Perm(), ShouldEqual, os.FileMode(0o755))
			})
		})
	})
}
