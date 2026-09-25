package digest_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
)

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create parent of %s: %v", path, err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustTree(t *testing.T, root string) digest.Hash {
	t.Helper()

	sum, err := digest.Tree(root)
	if err != nil {
		t.Fatalf("digest tree %s: %v", root, err)
	}

	return sum
}

func TestTreeAndFiles(t *testing.T) {
	Convey("Given a nested tree written out of order", t, func() {
		root := t.TempDir()
		files := map[string][]byte{
			"zeta.txt":        []byte("zeta\n"),
			"nested/deep.txt": []byte("deep\n"),
			"alpha/b.txt":     []byte("b\n"),
			"empty.bin":       {},
		}

		for rel, data := range files {
			mustWrite(t, filepath.Join(root, filepath.FromSlash(rel)), data)
		}

		Convey("When Tree and Files are computed", func() {
			first, err := digest.Tree(root)
			So(err, ShouldBeNil)

			Convey("Then they agree, are stable and root-independent", func() {
				So(first, ShouldEqual, digest.Files(files))
				So(first, ShouldNotEqual, digest.Bytes(nil))

				for range 5 {
					again, err := digest.Tree(root)
					So(err, ShouldBeNil)
					So(again, ShouldEqual, first)
				}

				copied := t.TempDir()
				for rel, data := range files {
					mustWrite(t, filepath.Join(copied, filepath.FromSlash(rel)), data)
				}

				copiedSum, err := digest.Tree(copied)
				So(err, ShouldBeNil)
				So(copiedSum, ShouldEqual, first)
			})
		})

		Convey("When an empty directory is added and a file changes", func() {
			base := mustTree(t, root)

			So(os.MkdirAll(filepath.Join(root, "void"), 0o750), ShouldBeNil)

			afterVoid := mustTree(t, root)

			mustWrite(t, filepath.Join(root, "zeta.txt"), []byte("ZETA\n"))

			changed := mustTree(t, root)

			Convey("Then empty directories are invisible and content changes are visible", func() {
				So(afterVoid, ShouldEqual, base)
				So(changed, ShouldNotEqual, base)

				rest := digest.Files(map[string][]byte{
					"nested/deep.txt": []byte("deep\n"),
					"alpha/b.txt":     []byte("b\n"),
					"empty.bin":       {},
				})

				So(os.Remove(filepath.Join(root, "zeta.txt")), ShouldBeNil)
				So(os.MkdirAll(filepath.Join(root, "zeta.txt"), 0o750), ShouldBeNil)

				So(mustTree(t, root), ShouldEqual, rest)
			})
		})
	})

	Convey("Given an empty tree", t, func() {
		Convey("When it is digested", func() {
			Convey("Then it equals Bytes(nil)", func() {
				So(mustTree(t, t.TempDir()), ShouldEqual, digest.Bytes(nil))
			})
		})
	})
}

func TestTreeUnicodeAndSpaces(t *testing.T) {
	Convey("Given a tree with unicode and spaced names", t, func() {
		root := t.TempDir()
		files := map[string][]byte{
			"файл-📁.txt":             []byte("данные\n"),
			"dir with space/é.txt":   []byte("accent\n"),
			"nested/файл с пробелом": []byte("x\n"),
		}

		for rel, data := range files {
			mustWrite(t, filepath.Join(root, filepath.FromSlash(rel)), data)
		}

		Convey("When the digest is computed twice", func() {
			first := mustTree(t, root)

			Convey("Then names are significant and stable", func() {
				So(mustTree(t, root), ShouldEqual, first)
				So(first, ShouldEqual, digest.Files(files))
				So(first, ShouldNotEqual, digest.Files(map[string][]byte{"dir with space/e.txt": []byte("accent\n")}))
			})
		})
	})
}

func TestTreeSkipsSymlinksAndSpecial(t *testing.T) {
	probe := t.TempDir()

	if err := syscall.Mkfifo(filepath.Join(probe, "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}

	Convey("Given a tree with symlinks and a FIFO", t, func() {
		root := t.TempDir()
		mustWrite(t, filepath.Join(root, "real.txt"), []byte("real\n"))
		mustWrite(t, filepath.Join(root, "sub", "inner.txt"), []byte("inner\n"))

		if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
			t.Fatalf("mkfifo: %v", err)
		}

		So(os.Symlink("real.txt", filepath.Join(root, "link.txt")), ShouldBeNil)
		So(os.Symlink("sub", filepath.Join(root, "linkdir")), ShouldBeNil)

		Convey("When the tree is digested", func() {
			got, err := digest.Tree(root)
			So(err, ShouldBeNil)

			Convey("Then only regular files participate", func() {
				So(got, ShouldEqual, digest.Files(map[string][]byte{
					"real.txt":      []byte("real\n"),
					"sub/inner.txt": []byte("inner\n"),
				}))
			})
		})

		Convey("When a symlink target changes", func() {
			before := mustTree(t, root)

			So(os.Remove(filepath.Join(root, "link.txt")), ShouldBeNil)
			So(os.Symlink("sub/inner.txt", filepath.Join(root, "link.txt")), ShouldBeNil)

			Convey("Then the digest is unchanged", func() {
				So(mustTree(t, root), ShouldEqual, before)
			})
		})
	})
}

func TestTreeWithSkip(t *testing.T) {
	Convey("Given a tree with a file and a subtree to skip", t, func() {
		root := t.TempDir()
		mustWrite(t, filepath.Join(root, "keep.txt"), []byte("keep\n"))
		mustWrite(t, filepath.Join(root, "skip.txt"), []byte("skip\n"))
		mustWrite(t, filepath.Join(root, "skipdir", "inner.txt"), []byte("inner\n"))

		full := mustTree(t, root)

		Convey("When a file is skipped", func() {
			skipped, err := digest.TreeWithSkip(root, func(rel string, _ bool) bool { return rel == "skip.txt" })
			So(err, ShouldBeNil)

			Convey("Then it equals deleting the file", func() {
				So(skipped, ShouldNotEqual, full)
				So(os.Remove(filepath.Join(root, "skip.txt")), ShouldBeNil)
				So(skipped, ShouldEqual, mustTree(t, root))
			})
		})

		Convey("When a directory is skipped", func() {
			skipped, err := digest.TreeWithSkip(root, func(rel string, _ bool) bool { return rel == "skipdir" })
			So(err, ShouldBeNil)

			Convey("Then the whole subtree is pruned", func() {
				So(skipped, ShouldNotEqual, full)
				So(os.RemoveAll(filepath.Join(root, "skipdir")), ShouldBeNil)
				So(skipped, ShouldEqual, mustTree(t, root))
			})
		})

		Convey("When the callback records its calls", func() {
			seen := []string{}

			_, err := digest.TreeWithSkip(root, func(rel string, _ bool) bool {
				seen = append(seen, rel)

				return false
			})
			So(err, ShouldBeNil)

			Convey("Then the root itself is never offered", func() {
				So(seen, ShouldHaveLength, 4)
				So(seen, ShouldNotContain, ".")
				So(seen, ShouldContain, "skipdir")
				So(seen, ShouldContain, "skipdir/inner.txt")
			})
		})
	})
}

func TestTreeGolden(t *testing.T) {
	const golden = "9a3f35dcaf70595ec8705dd09861bdfe3e438ad16e633f774dc879672b8e3b9c"

	Convey("Given a fixed tree of regular files", t, func() {
		root := t.TempDir()
		files := map[string][]byte{
			"a.txt":        []byte("alpha\n"),
			"empty.bin":    {},
			"nested/b.bin": []byte("beta\x00binary"),
			"z.txt":        []byte("z"),
		}

		for rel, data := range files {
			mustWrite(t, filepath.Join(root, filepath.FromSlash(rel)), data)
		}

		// An empty dir and a symlink must not change the digest.
		So(os.MkdirAll(filepath.Join(root, "void"), 0o750), ShouldBeNil)
		So(os.Symlink("a.txt", filepath.Join(root, "link.txt")), ShouldBeNil)

		Convey("When Tree and Files are computed", func() {
			treeSum := mustTree(t, root)
			filesSum := digest.Files(files)

			Convey("Then both match the pinned golden vector", func() {
				So(treeSum.String(), ShouldEqual, golden)
				So(filesSum.String(), ShouldEqual, golden)
				So(treeSum, ShouldEqual, filesSum)
			})

			Convey("Then the canonical bytes are rel NUL hex(payload) LF, sorted", func() {
				canonical := digest.Bytes([]byte(
					"a.txt\x00" + digest.Bytes([]byte("alpha\n")).String() + "\n" +
						"empty.bin\x00" + digest.Bytes(nil).String() + "\n" +
						"nested/b.bin\x00" + digest.Bytes([]byte("beta\x00binary")).String() + "\n" +
						"z.txt\x00" + digest.Bytes([]byte("z")).String() + "\n",
				))

				// Without the NUL separator the same rel/hash pairs concatenate
				// differently: this is the separator-collision pin.
				noSeparator := digest.Bytes([]byte(
					"a.txt" + digest.Bytes([]byte("alpha\n")).String() + "\n" +
						"empty.bin" + digest.Bytes(nil).String() + "\n" +
						"nested/b.bin" + digest.Bytes([]byte("beta\x00binary")).String() + "\n" +
						"z.txt" + digest.Bytes([]byte("z")).String() + "\n",
				))

				So(treeSum, ShouldEqual, canonical)
				So(treeSum, ShouldNotEqual, noSeparator)
			})

			Convey("Then key ordering and boundaries are significant", func() {
				So(digest.Files(map[string][]byte{"ab": []byte("x")}),
					ShouldNotEqual, digest.Files(map[string][]byte{"a": []byte("x")}))
				So(digest.Files(map[string][]byte{"b": []byte("x"), "a": []byte("y")}),
					ShouldEqual, digest.Files(map[string][]byte{"a": []byte("y"), "b": []byte("x")}))
			})
		})
	})
}

func TestTreeRootSymlink(t *testing.T) {
	Convey("Given a symlink to a tree", t, func() {
		realTree := t.TempDir()
		mustWrite(t, filepath.Join(realTree, "a.txt"), []byte("a\n"))
		mustWrite(t, filepath.Join(realTree, "nested", "b.txt"), []byte("b\n"))

		alias := filepath.Join(t.TempDir(), "alias")
		So(os.Symlink(realTree, alias), ShouldBeNil)

		Convey("When the alias is digested", func() {
			Convey("Then it resolves to the real tree digest", func() {
				So(mustTree(t, alias), ShouldEqual, mustTree(t, realTree))
			})
		})
	})
}

func TestTreeWalkOrderSorted(t *testing.T) {
	Convey("Given a tree whose walk order differs from sorted order", t, func() {
		// "a/x.txt" is walked before "a-b.txt" on readdir-order filesystems,
		// but "-" sorts before "/", so the digest must be built from the
		// explicitly sorted rel paths.
		files := map[string][]byte{
			"a/x.txt": []byte("X"),
			"a-b.txt": []byte("AB"),
		}

		first := t.TempDir()
		mustWrite(t, filepath.Join(first, "a", "x.txt"), files["a/x.txt"])
		mustWrite(t, filepath.Join(first, "a-b.txt"), files["a-b.txt"])

		second := t.TempDir()
		mustWrite(t, filepath.Join(second, "a-b.txt"), files["a-b.txt"])
		mustWrite(t, filepath.Join(second, "a", "x.txt"), files["a/x.txt"])

		Convey("When both roots and the in-memory form are digested", func() {
			// Expected is built via digest.Bytes on purpose: this test pins
			// walk-order insensitivity, while the hash algorithm itself is
			// pinned by TestBytes and the literal golden in TestTreeGolden.
			want := digest.Bytes([]byte(
				"a-b.txt\x00" + digest.Bytes(files["a-b.txt"]).String() + "\n" +
					"a/x.txt\x00" + digest.Bytes(files["a/x.txt"]).String() + "\n",
			))

			Convey("Then the walk order cannot leak into the digest", func() {
				So(mustTree(t, first), ShouldEqual, want)
				So(mustTree(t, second), ShouldEqual, want)
				So(digest.Files(files), ShouldEqual, want)
			})
		})
	})
}

func TestTreeErrors(t *testing.T) {
	Convey("Given a missing root and a file root", t, func() {
		dir := t.TempDir()
		file := filepath.Join(dir, "file.txt")
		mustWrite(t, file, []byte("x\n"))

		Convey("When Tree is called", func() {
			missing := filepath.Join(dir, "missing")

			_, missingErr := digest.Tree(missing)
			missingDetail, missingTyped := errors.AsType[*digest.UnreadableError](missingErr)

			_, err := digest.Tree(file)
			detail, typed := errors.AsType[*digest.UnreadableError](err)

			Convey("Then both are typed UnreadableErrors with the OS cause", func() {
				So(missingTyped, ShouldBeTrue)
				So(missingDetail.Path, ShouldEqual, missing)
				So(errors.Is(missingErr, fs.ErrNotExist), ShouldBeTrue)

				So(typed, ShouldBeTrue)
				So(detail.Path, ShouldEqual, file)
			})
		})
	})
}

func TestTreeNonUTF8Names(t *testing.T) {
	root := t.TempDir()

	name := string([]byte{'b', 'a', 'd', 0xff, 0xfe, '.', 't', 'x', 't'})
	if err := os.WriteFile(filepath.Join(root, name), []byte("x\n"), 0o600); err != nil {
		t.Skipf("filesystem rejects non-UTF-8 names: %v", err)
	}

	Convey("Given a file with a non-UTF-8 name", t, func() {
		Convey("When the tree is digested twice", func() {
			first := mustTree(t, root)

			Convey("Then the digest is byte-stable", func() {
				So(mustTree(t, root), ShouldEqual, first)
			})
		})
	})
}

func TestTreeConcurrent(t *testing.T) {
	Convey("Given a tree hashed from several goroutines", t, func() {
		root := t.TempDir()
		mustWrite(t, filepath.Join(root, "a.txt"), []byte("a\n"))
		mustWrite(t, filepath.Join(root, "nested", "b.txt"), []byte("b\n"))

		want := mustTree(t, root)

		Convey("When eight goroutines hash it", func() {
			results := make([]digest.Hash, 8)

			var group sync.WaitGroup

			for index := range results {
				group.Go(func() {
					sum, err := digest.Tree(root)
					if err != nil {
						t.Errorf("tree digest: %v", err)

						return
					}

					results[index] = sum
				})
			}

			group.Wait()

			Convey("Then every digest matches", func() {
				for _, got := range results {
					So(got, ShouldEqual, want)
				}
			})
		})
	})
}
