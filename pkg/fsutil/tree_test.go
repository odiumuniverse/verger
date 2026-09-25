package fsutil

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
)

func mustWrite(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()

	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdir(t *testing.T, path string, perm os.FileMode) {
	t.Helper()

	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustFileDigest(t *testing.T, path string) digest.Hash {
	t.Helper()

	sum, err := digest.File(path)
	if err != nil {
		t.Fatalf("digest file %s: %v", path, err)
	}

	return sum
}

func stubRename(t *testing.T, rename func(oldpath, newpath string) error) {
	t.Helper()

	previous := renameForTest
	renameForTest = rename

	t.Cleanup(func() { renameForTest = previous })
}

// relaxTree restores owner-write on directories before t.TempDir cleanup, so
// fixtures that deliberately preserve read-only modes can still be removed.
func relaxTree(t *testing.T, root string) {
	t.Helper()

	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(current string, entry fs.DirEntry, err error) error {
			if err != nil || !entry.IsDir() {
				return nil //nolint:nilerr // best-effort chmod: cleanup keeps going
			}

			_ = os.Chmod(current, 0o700) //nolint:gosec // G302: restoring fixture permissions for cleanup

			return nil
		})
	})
}

// sourceTree builds a tree with nested dirs, exact modes, unicode names, a
// symlink and a FIFO, and returns its path.
func sourceTree(t *testing.T) string {
	t.Helper()

	from := filepath.Join(t.TempDir(), "from")
	mustMkdir(t, filepath.Join(from, "nested"), 0o750)
	mustWrite(t, filepath.Join(from, "nested", "файл ☃.txt"), []byte("deep\n"), 0o640)
	mustWrite(t, filepath.Join(from, "top.txt"), []byte("top\n"), 0o600)
	mustWrite(t, filepath.Join(from, "empty.bin"), nil, 0o600)
	mustWrite(t, filepath.Join(from, "large.bin"), bytes.Repeat([]byte("x"), 5<<20), 0o600)

	if err := os.Symlink("top.txt", filepath.Join(from, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := syscall.Mkfifo(filepath.Join(from, "pipe"), 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	// Read-only modes are applied after the children exist.
	if err := os.Chmod(filepath.Join(from, "nested"), 0o500); err != nil { //nolint:gosec // G302: the test deliberately preserves read-only modes
		t.Fatalf("chmod nested: %v", err)
	}

	if err := os.Chmod(filepath.Join(from, "nested", "файл ☃.txt"), 0o500); err != nil { //nolint:gosec // G302: the test deliberately preserves read-only modes
		t.Fatalf("chmod deep file: %v", err)
	}

	relaxTree(t, from)

	return from
}

func TestCopyTree(t *testing.T) {
	Convey("Given a source tree with modes, unicode names, a symlink and a FIFO", t, func() {
		from := sourceTree(t)
		to := filepath.Join(t.TempDir(), "to")

		Convey("When the tree is copied", func() {
			err := CopyTree(context.Background(), from, to)
			So(err, ShouldBeNil)

			relaxTree(t, to)

			sourceSum, digestErr := digest.Tree(from)
			So(digestErr, ShouldBeNil)

			copiedSum, digestErr := digest.Tree(to)
			So(digestErr, ShouldBeNil)

			nested, statErr := os.Stat(filepath.Join(to, "nested"))
			So(statErr, ShouldBeNil)

			deep, statErr := os.Stat(filepath.Join(to, "nested", "файл ☃.txt"))
			So(statErr, ShouldBeNil)

			top, statErr := os.Stat(filepath.Join(to, "top.txt"))
			So(statErr, ShouldBeNil)

			link, linkErr := os.Readlink(filepath.Join(to, "link.txt"))
			So(linkErr, ShouldBeNil)

			empty, statErr := os.Stat(filepath.Join(to, "empty.bin"))
			So(statErr, ShouldBeNil)

			large, statErr := os.Stat(filepath.Join(to, "large.bin"))
			So(statErr, ShouldBeNil)

			_, pipeErr := os.Lstat(filepath.Join(to, "pipe"))

			Convey("Then it is complete, mode-identical and special files are skipped", func() {
				So(copiedSum, ShouldEqual, sourceSum)
				So(nested.Mode().Perm(), ShouldEqual, os.FileMode(0o500))
				So(deep.Mode().Perm(), ShouldEqual, os.FileMode(0o500))
				So(top.Mode().Perm(), ShouldEqual, os.FileMode(0o600))
				So(link, ShouldEqual, "top.txt")
				So(empty.Size(), ShouldEqual, 0)
				So(mustFileDigest(t, filepath.Join(to, "empty.bin")), ShouldEqual, digest.Bytes(nil))
				So(large.Size(), ShouldEqual, int64(5<<20))
				So(mustFileDigest(t, filepath.Join(to, "large.bin")),
					ShouldEqual, mustFileDigest(t, filepath.Join(from, "large.bin")))
				So(errors.Is(pipeErr, fs.ErrNotExist), ShouldBeTrue)
			})
		})
	})
}

func TestCopyTreeBoundaries(t *testing.T) {
	Convey("Given a tree with only a 0-byte and a 5 MiB file", t, func() {
		from := filepath.Join(t.TempDir(), "from")
		mustMkdir(t, from, 0o700)
		mustWrite(t, filepath.Join(from, "empty.bin"), nil, 0o600)
		mustWrite(t, filepath.Join(from, "large.bin"), bytes.Repeat([]byte("x"), 5<<20), 0o600)

		to := filepath.Join(t.TempDir(), "to")

		Convey("When the tree is copied", func() {
			So(CopyTree(context.Background(), from, to), ShouldBeNil)

			empty, emptyErr := os.Stat(filepath.Join(to, "empty.bin"))
			large, largeErr := os.Stat(filepath.Join(to, "large.bin"))

			Convey("Then the 0-byte and 5 MiB boundaries are preserved byte-for-byte", func() {
				So(emptyErr, ShouldBeNil)
				So(largeErr, ShouldBeNil)
				So(empty.Size(), ShouldEqual, 0)
				So(mustFileDigest(t, filepath.Join(to, "empty.bin")), ShouldEqual, digest.Bytes(nil))
				So(large.Size(), ShouldEqual, int64(5<<20))
				So(mustFileDigest(t, filepath.Join(to, "large.bin")),
					ShouldEqual, mustFileDigest(t, filepath.Join(from, "large.bin")))
			})
		})
	})
}

func TestCopyTreeHardlinksBecomeIndependentCopies(t *testing.T) {
	Convey("Given a tree where two names share one inode", t, func() {
		from := filepath.Join(t.TempDir(), "from")
		mustMkdir(t, from, 0o700)

		original := filepath.Join(from, "a.txt")
		mustWrite(t, original, []byte("shared\n"), 0o640)

		So(os.Link(original, filepath.Join(from, "b.txt")), ShouldBeNil)

		to := filepath.Join(t.TempDir(), "to")

		Convey("When the tree is copied", func() {
			So(CopyTree(context.Background(), from, to), ShouldBeNil)

			first, firstErr := os.Stat(filepath.Join(to, "a.txt"))
			second, secondErr := os.Stat(filepath.Join(to, "b.txt"))
			source, sourceErr := os.Stat(original)

			Convey("Then every hard-linked name is an independent regular file", func() {
				So(firstErr, ShouldBeNil)
				So(secondErr, ShouldBeNil)
				So(sourceErr, ShouldBeNil)
				So(os.SameFile(first, second), ShouldBeFalse)
				So(os.SameFile(source, first), ShouldBeFalse)
				So(os.SameFile(source, second), ShouldBeFalse)
			})

			Convey("Then mutating one copy leaves the source and the sibling intact", func() {
				So(os.WriteFile(filepath.Join(to, "a.txt"), []byte("changed\n"), 0o600), ShouldBeNil)

				sibling, siblingErr := os.ReadFile(filepath.Join(to, "b.txt")) //nolint:gosec // G304: the test reads its own temp file
				origin, originErr := os.ReadFile(original)                     //nolint:gosec // G304: the test reads its own temp file

				So(siblingErr, ShouldBeNil)
				So(originErr, ShouldBeNil)
				So(string(sibling), ShouldEqual, "shared\n")
				So(string(origin), ShouldEqual, "shared\n")
			})
		})
	})
}

func TestCopyTreeErrors(t *testing.T) {
	Convey("Given a source tree", t, func() {
		from := sourceTree(t)

		Convey("When the destination exists", func() {
			to := filepath.Join(t.TempDir(), "taken")
			So(os.Mkdir(to, 0o700), ShouldBeNil)

			err := CopyTree(context.Background(), from, to)

			detail, ok := errors.AsType[*DestinationExistsError](err)

			Convey("Then a typed destination error is returned", func() {
				So(ok, ShouldBeTrue)

				if ok {
					So(detail.Path, ShouldEqual, to)
				}
			})
		})

		Convey("When the source is missing", func() {
			err := CopyTree(context.Background(), filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "to"))

			detail, ok := errors.AsType[*SourceMissingError](err)

			Convey("Then a typed source error is returned", func() {
				So(ok, ShouldBeTrue)

				if ok {
					So(detail.Path, ShouldContainSubstring, "missing")
				}
			})
		})

		Convey("When the destination parent is missing", func() {
			err := CopyTree(context.Background(), from, filepath.Join(t.TempDir(), "missing", "to"))

			_, ok := errors.AsType[*InvalidMoveError](err)

			Convey("Then an invalid move error is returned", func() {
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When the context is canceled", func() {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			to := filepath.Join(t.TempDir(), "to")

			err := CopyTree(ctx, from, to)

			Convey("Then cancellation surfaces and the destination is removed", func() {
				So(errors.Is(err, context.Canceled), ShouldBeTrue)
				So(Exists(to), ShouldBeFalse)
			})
		})
	})
}

func TestCopyTreeUnwritableDestination(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	Convey("Given an unwritable destination parent", t, func() {
		from := sourceTree(t)

		parent := t.TempDir()
		to := filepath.Join(parent, "to")
		So(os.Chmod(parent, 0o500), ShouldBeNil) //nolint:gosec // G302: the test needs an unwritable directory

		t.Cleanup(func() { _ = os.Chmod(parent, 0o700) }) //nolint:gosec // G302: restoring test fixture permissions

		err := CopyTree(context.Background(), from, to)

		entries, readErr := os.ReadDir(parent)

		Convey("When the tree is copied", func() {
			Convey("Then it fails, removes the partial destination and leaves the source intact", func() {
				So(err, ShouldBeError)
				So(readErr, ShouldBeNil)
				So(entries, ShouldBeEmpty)
				So(Exists(from), ShouldBeTrue)
			})
		})
	})
}

func TestMoveTreeSameFS(t *testing.T) {
	Convey("Given a source tree", t, func() {
		from := sourceTree(t)
		parent := t.TempDir()
		to := filepath.Join(parent, "to")

		expected, err := digest.Tree(from)
		So(err, ShouldBeNil)

		Convey("When it is moved", func() {
			So(MoveTree(context.Background(), from, to), ShouldBeNil)

			relaxTree(t, to)

			copiedSum, err := digest.Tree(to)
			_, sourceErr := digest.Tree(from)

			entries, readErr := os.ReadDir(parent)

			Convey("Then the source is gone and the destination is complete", func() {
				So(err, ShouldBeNil)
				So(copiedSum, ShouldEqual, expected)
				So(sourceErr, ShouldBeError)
				So(Exists(from), ShouldBeFalse)
				So(readErr, ShouldBeNil)
				So(entries, ShouldHaveLength, 1)
				So(entries[0].Name(), ShouldEqual, "to")
			})
		})
	})
}

func TestMoveTreeCrossDevice(t *testing.T) {
	Convey("Given a rename that fails with EXDEV", t, func() {
		from := sourceTree(t)
		parent := t.TempDir()
		to := filepath.Join(parent, "to")

		expected, err := digest.Tree(from)
		So(err, ShouldBeNil)

		stubRename(t, func(oldpath, newpath string) error {
			if oldpath == from {
				return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
			}

			return os.Rename(oldpath, newpath)
		})

		Convey("When the tree is moved", func() {
			So(MoveTree(context.Background(), from, to), ShouldBeNil)

			relaxTree(t, to)

			moved, err := digest.Tree(to)

			entries, readErr := os.ReadDir(parent)

			Convey("Then the copy fallback completes, removes the source and leaves no staging", func() {
				So(err, ShouldBeNil)
				So(moved, ShouldEqual, expected)
				So(Exists(from), ShouldBeFalse)
				So(readErr, ShouldBeNil)
				So(entries, ShouldHaveLength, 1)
				So(entries[0].Name(), ShouldEqual, "to")
			})
		})
	})
}

func TestMoveTreeCrossDeviceCopyFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	Convey("Given EXDEV and an unwritable destination parent", t, func() {
		from := sourceTree(t)

		parent := t.TempDir()
		to := filepath.Join(parent, "to")
		So(os.Chmod(parent, 0o500), ShouldBeNil) //nolint:gosec // G302: the test needs an unwritable directory

		t.Cleanup(func() { _ = os.Chmod(parent, 0o700) }) //nolint:gosec // G302: restoring test fixture permissions

		stubRename(t, func(_, _ string) error {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EXDEV}
		})

		err := MoveTree(context.Background(), from, to)

		entries, readErr := os.ReadDir(parent)

		Convey("When the tree is moved", func() {
			Convey("Then it fails, the source survives and no staging remains", func() {
				So(err, ShouldBeError)
				So(Exists(from), ShouldBeTrue)
				So(readErr, ShouldBeNil)
				So(entries, ShouldBeEmpty)
			})
		})
	})
}

func TestMoveTreeRejectsSamePath(t *testing.T) {
	Convey("Given the same source and destination", t, func() {
		from := sourceTree(t)

		err := MoveTree(context.Background(), from, from)

		_, ok := errors.AsType[*InvalidMoveError](err)

		Convey("When it is moved", func() {
			Convey("Then an invalid move error is returned", func() {
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestMoveTreeLongUnicodeNames(t *testing.T) {
	Convey("Given a deep tree with long unicode names", t, func() {
		from := filepath.Join(t.TempDir(), "from")
		deep := filepath.Join(from, strings.Repeat("d/", 10))
		mustMkdir(t, deep, 0o700)
		mustWrite(t, filepath.Join(deep, strings.Repeat("ж", 100)+".txt"), []byte("x\n"), 0o600)

		to := filepath.Join(t.TempDir(), "to")

		Convey("When it is copied", func() {
			So(CopyTree(context.Background(), from, to), ShouldBeNil)

			source, err := digest.Tree(from)
			So(err, ShouldBeNil)

			copied, err := digest.Tree(to)

			Convey("Then the digests match", func() {
				So(err, ShouldBeNil)
				So(copied, ShouldEqual, source)
			})
		})
	})
}

func TestCopyTreeFileSource(t *testing.T) {
	Convey("Given a single regular file", t, func() {
		dir := t.TempDir()
		from := filepath.Join(dir, "payload.bin")
		mustWrite(t, from, []byte("payload"), 0o640)

		to := filepath.Join(t.TempDir(), "copy.bin")

		Convey("When it is copied", func() {
			err := CopyTree(context.Background(), from, to)
			data, readErr := os.ReadFile(to) //nolint:gosec // G304: the test reads its own temp file
			info, statErr := os.Stat(to)

			Convey("Then content and mode are preserved and the source survives", func() {
				So(err, ShouldBeNil)
				So(readErr, ShouldBeNil)
				So(string(data), ShouldEqual, "payload")
				So(statErr, ShouldBeNil)
				So(info.Mode().Perm(), ShouldEqual, os.FileMode(0o640))
				So(Exists(from), ShouldBeTrue)
			})
		})
	})
}

func TestCopyTreeSymlinkSource(t *testing.T) {
	Convey("Given a symlink source", t, func() {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "target.txt"), []byte("target"), 0o600)

		from := filepath.Join(dir, "link.txt")
		So(os.Symlink("target.txt", from), ShouldBeNil)

		to := filepath.Join(t.TempDir(), "link-copy.txt")

		Convey("When it is copied", func() {
			err := CopyTree(context.Background(), from, to)
			link, linkErr := os.Readlink(to)
			info, statErr := os.Lstat(to)

			Convey("Then the link itself is recreated with the same target text", func() {
				So(err, ShouldBeNil)
				So(linkErr, ShouldBeNil)
				So(link, ShouldEqual, "target.txt")
				So(statErr, ShouldBeNil)
				So(info.Mode()&fs.ModeSymlink, ShouldNotEqual, 0)
			})
		})
	})
}

func TestCopyTreeExpandsHome(t *testing.T) {
	Convey("Given a tilde destination", t, func() {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Chdir(t.TempDir())

		from := sourceTree(t)

		Convey("When it is copied to ~/copied", func() {
			err := CopyTree(context.Background(), from, "~/copied")
			So(err, ShouldBeNil)

			relaxTree(t, filepath.Join(home, "copied"))

			Convey("Then the destination resolves under the home directory", func() {
				So(Exists(filepath.Join(home, "copied")), ShouldBeTrue)
				So(Exists(filepath.Join(home, "copied", "top.txt")), ShouldBeTrue)
			})
		})
	})
}

func TestCopyTreeRejectsRelativePaths(t *testing.T) {
	Convey("Given a relative destination", t, func() {
		t.Chdir(t.TempDir())
		from := sourceTree(t)

		Convey("When the tree is copied", func() {
			err := CopyTree(context.Background(), from, "relative")

			_, ok := errors.AsType[*InvalidMoveError](err)

			Convey("Then it is rejected before anything is written", func() {
				So(ok, ShouldBeTrue)
				So(Exists("relative"), ShouldBeFalse)
			})
		})
	})
}

func TestCopyTreeSymlinkUnsupported(t *testing.T) {
	Convey("Given a filesystem that cannot create symlinks", t, func() {
		previous := symlinkLinker
		symlinkLinker = func(_, _ string) error { return syscall.EPERM }

		t.Cleanup(func() { symlinkLinker = previous })

		from := sourceTree(t)
		to := filepath.Join(t.TempDir(), "to")

		Convey("When the tree is copied", func() {
			err := CopyTree(context.Background(), from, to)

			Convey("Then ErrSymlinkUnsupported wraps the cause and the destination is rolled back", func() {
				So(errors.Is(err, ErrSymlinkUnsupported), ShouldBeTrue)
				So(errors.Is(err, syscall.EPERM), ShouldBeTrue)
				So(Exists(to), ShouldBeFalse)
				So(Exists(from), ShouldBeTrue)
			})
		})
	})
}

func TestMoveTreeStagingRenameFailure(t *testing.T) {
	Convey("Given EXDEV and a failing staging rename", t, func() {
		from := sourceTree(t)
		parent := t.TempDir()
		to := filepath.Join(parent, "to")

		stubRename(t, func(oldpath, newpath string) error {
			if oldpath == from {
				return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
			}

			return errors.New("staging denied")
		})

		err := MoveTree(context.Background(), from, to)

		entries, readErr := os.ReadDir(parent)

		Convey("When the tree is moved", func() {
			Convey("Then it fails, the source survives and no staging remains", func() {
				So(err, ShouldBeError)
				So(Exists(from), ShouldBeTrue)
				So(Exists(to), ShouldBeFalse)
				So(readErr, ShouldBeNil)
				So(entries, ShouldBeEmpty)
			})
		})
	})
}

func TestMoveTreeFileSourceCrossDevice(t *testing.T) {
	Convey("Given a file moved across devices", t, func() {
		dir := t.TempDir()
		from := filepath.Join(dir, "payload.bin")
		mustWrite(t, from, []byte("payload"), 0o600)

		to := filepath.Join(t.TempDir(), "moved.bin")

		stubRename(t, func(oldpath, newpath string) error {
			if oldpath == from {
				return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
			}

			return os.Rename(oldpath, newpath)
		})

		Convey("When it is moved", func() {
			err := MoveTree(context.Background(), from, to)
			data, readErr := os.ReadFile(to) //nolint:gosec // G304: the test reads its own temp file

			Convey("Then the copy fallback preserves content and removes the source", func() {
				So(err, ShouldBeNil)
				So(readErr, ShouldBeNil)
				So(string(data), ShouldEqual, "payload")
				So(Exists(from), ShouldBeFalse)
				So(Exists(to), ShouldBeTrue)
			})
		})
	})
}

func TestMoveTreeCanceledCrossDevice(t *testing.T) {
	Convey("Given EXDEV and an already canceled context", t, func() {
		from := sourceTree(t)
		parent := t.TempDir()
		to := filepath.Join(parent, "to")

		stubRename(t, func(oldpath, newpath string) error {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
		})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := MoveTree(ctx, from, to)

		entries, readErr := os.ReadDir(parent)

		Convey("When the tree is moved", func() {
			Convey("Then cancellation surfaces, the source survives and no staging remains", func() {
				So(errors.Is(err, context.Canceled), ShouldBeTrue)
				So(Exists(from), ShouldBeTrue)
				So(readErr, ShouldBeNil)
				So(entries, ShouldBeEmpty)
			})
		})
	})
}

func TestCopyTreeConcurrent(t *testing.T) {
	Convey("Given concurrent copies to distinct destinations", t, func() {
		from := sourceTree(t)

		expected, err := digest.Tree(from)
		So(err, ShouldBeNil)

		targets := []string{
			filepath.Join(t.TempDir(), "first"),
			filepath.Join(t.TempDir(), "second"),
		}

		errs := make([]error, len(targets))

		var group sync.WaitGroup

		for index := range targets {
			group.Go(func() {
				errs[index] = CopyTree(context.Background(), from, targets[index])
			})
		}

		group.Wait()

		Convey("Then both copies complete with the source digest", func() {
			for index, target := range targets {
				So(errs[index], ShouldBeNil)

				relaxTree(t, target)

				sum, err := digest.Tree(target)
				So(err, ShouldBeNil)
				So(sum, ShouldEqual, expected)
			}
		})
	})
}

func TestSameFSCrossDeviceSeam(t *testing.T) {
	Convey("Given simulated device ids", t, func() {
		first := t.TempDir()
		second := t.TempDir()

		previous := deviceIDForTest

		t.Cleanup(func() { deviceIDForTest = previous })

		Convey("When the two paths report different devices", func() {
			deviceIDForTest = func(path string) (uint64, error) {
				switch path {
				case first:
					return 11, nil
				case second:
					return 22, nil
				default:
					return previous(path)
				}
			}

			same, err := SameFS(first, second)

			Convey("Then SameFS reports a cross-device pair", func() {
				So(err, ShouldBeNil)
				So(same, ShouldBeFalse)
			})
		})

		Convey("When both paths report the same device", func() {
			deviceIDForTest = func(string) (uint64, error) { return 7, nil }

			same, err := SameFS(first, second)

			Convey("Then SameFS reports the same filesystem", func() {
				So(err, ShouldBeNil)
				So(same, ShouldBeTrue)
			})
		})

		Convey("When the device probe fails", func() {
			sentinel := errors.New("probe failed")
			deviceIDForTest = func(string) (uint64, error) { return 0, sentinel }

			_, err := SameFS(first, second)

			Convey("Then the probe error surfaces", func() {
				So(errors.Is(err, sentinel), ShouldBeTrue)
			})
		})
	})
}
