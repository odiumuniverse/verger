package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

// assertMode fails the test when path does not carry exactly want.
func assertMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode of %s = %o, want %o", path, got, want)
	}
}

// assertMissing fails the test when path exists.
func assertMissing(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s should not exist (err=%v)", path, err)
	}
}

func TestStoreDefaultRoot(t *testing.T) {
	Convey("Given a machine with an absolute XDG_DATA_HOME", t, func() {
		xdg := filepath.Join(t.TempDir(), "xdg")
		t.Setenv("XDG_DATA_HOME", xdg)

		Convey("When the default store root is resolved", func() {
			root, err := DefaultRoot()

			Convey("Then it lives under XDG_DATA_HOME/verger", func() {
				So(err, ShouldBeNil)
				So(root, ShouldEqual, filepath.Join(xdg, DirName))
			})
		})
	})

	Convey("Given a relative XDG_DATA_HOME", t, func() {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_DATA_HOME", filepath.Join("relative", "data"))

		Convey("When the default store root is resolved", func() {
			root, err := DefaultRoot()

			Convey("Then it falls back to ~/.local/share/verger", func() {
				So(err, ShouldBeNil)
				So(root, ShouldEqual, filepath.Join(home, ".local", "share", DirName))
			})
		})
	})

	Convey("Given an unset XDG_DATA_HOME", t, func() {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_DATA_HOME", "")

		Convey("When the default store root is resolved", func() {
			root, err := DefaultRoot()

			Convey("Then it falls back to ~/.local/share/verger", func() {
				So(err, ShouldBeNil)
				So(root, ShouldEqual, filepath.Join(home, ".local", "share", DirName))
			})
		})
	})

	Convey("Given decoy VERGER_HOME and BEADLE_HOME values", t, func() {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("VERGER_HOME", filepath.Join(t.TempDir(), "decoy-verger"))
		t.Setenv("BEADLE_HOME", filepath.Join(t.TempDir(), "decoy-beadle"))

		Convey("When the default store root is resolved", func() {
			root, err := DefaultRoot()

			Convey("Then the home-shaped variables are never consulted", func() {
				So(err, ShouldBeNil)
				So(root, ShouldEqual, filepath.Join(home, ".local", "share", DirName))
				So(root, ShouldNotContainSubstring, "decoy")
			})
		})
	})
}

func TestStoreOpen(t *testing.T) {
	Convey("Given an explicit store root", t, func() {
		root := filepath.Join(t.TempDir(), "store")

		Convey("When the store is opened", func() {
			s, err := Open(root)

			Convey("Then the root is absolute and nothing is created", func() {
				So(err, ShouldBeNil)
				So(s.Root(), ShouldEqual, root)
				assertMissing(t, root)
			})
		})
	})

	Convey("Given an explicit root with a tilde", t, func() {
		home := t.TempDir()
		t.Setenv("HOME", home)

		Convey("When the store is opened", func() {
			s, err := Open("~/vstore")

			Convey("Then the tilde expands against HOME", func() {
				So(err, ShouldBeNil)
				So(s.Root(), ShouldEqual, filepath.Join(home, "vstore"))
			})
		})
	})

	Convey("Given an empty root and a pinned XDG_DATA_HOME", t, func() {
		xdg := filepath.Join(t.TempDir(), "data")
		t.Setenv("XDG_DATA_HOME", xdg)

		Convey("When the store is opened", func() {
			s, err := Open("")

			Convey("Then the default root is used", func() {
				So(err, ShouldBeNil)
				So(s.Root(), ShouldEqual, filepath.Join(xdg, DirName))
			})
		})
	})

	Convey("Given a relative explicit root", t, func() {
		Convey("When the store is opened", func() {
			s, err := Open(filepath.Join("relative", "store"))

			Convey("Then the root is absolute", func() {
				So(err, ShouldBeNil)
				So(filepath.IsAbs(s.Root()), ShouldBeTrue)
			})
		})
	})

	Convey("Given an unwritable parent for the root", t, func() {
		Convey("When the store is opened", func() {
			_, err := Open(filepath.Join(t.TempDir(), "missing", "store"))

			Convey("Then Open still succeeds because it writes nothing", func() {
				So(err, ShouldBeNil)
			})
		})
	})
}

func TestStoreEnsure(t *testing.T) {
	Convey("Given an open store", t, func() {
		root := filepath.Join(t.TempDir(), "store")
		s, err := Open(root)
		So(err, ShouldBeNil)

		Convey("When Ensure runs", func() {
			err := s.Ensure()

			Convey("Then the root and every layout dir exist with 0700", func() {
				So(err, ShouldBeNil)

				for _, dir := range []string{s.Root(), s.DataDir(), s.TrashDir(), s.RuntimeDir(), s.BinDir(), s.CacheDir()} {
					assertMode(t, dir, 0o700)
				}
			})

			Convey("Then Ensure is idempotent", func() {
				So(err, ShouldBeNil)
				So(s.Ensure(), ShouldBeNil)
				assertMode(t, s.DataDir(), 0o700)
			})
		})

		Convey("When an existing layout dir is left at 0755", func() {
			So(s.Ensure(), ShouldBeNil)
			So(os.Chmod(s.DataDir(), 0o755), ShouldBeNil) //nolint:gosec // G302: the test deliberately leaves an existing dir at 0755

			err := s.Ensure()

			Convey("Then Ensure never re-chmods an existing directory", func() {
				So(err, ShouldBeNil)
				assertMode(t, s.DataDir(), 0o755)
			})
		})
	})
}

func TestStoreLayoutPaths(t *testing.T) {
	Convey("Given an open store", t, func() {
		root := filepath.Join(t.TempDir(), "store")
		s, err := Open(root)
		So(err, ShouldBeNil)

		Convey("When the layout paths are read", func() {
			Convey("Then every path is exact", func() {
				So(s.DataDir(), ShouldEqual, filepath.Join(root, "data"))
				So(s.TrashDir(), ShouldEqual, filepath.Join(root, "trash"))
				So(s.RuntimeDir(), ShouldEqual, filepath.Join(root, "runtime"))
				So(s.BinDir(), ShouldEqual, filepath.Join(root, "bin"))
				So(s.CacheDir(), ShouldEqual, filepath.Join(root, "cache"))
			})
		})

		Convey("When package data paths are resolved", func() {
			p, err := s.PackageDataPath("owner/name", "claude")

			Convey("Then a slashed id nests directories", func() {
				So(err, ShouldBeNil)
				So(p, ShouldEqual, filepath.Join(root, "data", "owner", "name", "claude"))
				assertMissing(t, p)
			})
		})

		Convey("When a registry-style id is resolved", func() {
			p, err := s.PackageDataPath("mcp:io.github.github/github-mcp-server", "claude")

			Convey("Then the colon form is accepted", func() {
				So(err, ShouldBeNil)
				So(p, ShouldEqual, filepath.Join(root, "data", "mcp:io.github.github", "github-mcp-server", "claude"))
			})
		})

		Convey("When EnsurePackageData runs", func() {
			p, err := s.EnsurePackageData("owner/name", "claude")

			Convey("Then the target directory exists with 0700", func() {
				So(err, ShouldBeNil)
				assertMode(t, p, 0o700)
				assertMode(t, filepath.Dir(p), 0o700)
			})
		})

		Convey("When runtime paths are resolved", func() {
			p, err := s.RuntimePath("claude", "1.2.3")

			Convey("Then the version nests under the host", func() {
				So(err, ShouldBeNil)
				So(p, ShouldEqual, filepath.Join(root, "runtime", "claude", "1.2.3"))
				assertMissing(t, p)
			})
		})

		Convey("When EnsureRuntimePath runs", func() {
			p, err := s.EnsureRuntimePath("claude", "1.2.3")

			Convey("Then the target directory exists with 0700", func() {
				So(err, ShouldBeNil)
				assertMode(t, p, 0o700)
			})
		})

		Convey("When Trash is requested twice", func() {
			Convey("Then the same handle is returned", func() {
				So(s.Trash(), ShouldEqual, s.Trash())
			})
		})
	})
}

func TestStorePathValidation(t *testing.T) {
	Convey("Given an open store", t, func() {
		root := filepath.Join(t.TempDir(), "store")
		s, err := Open(root)
		So(err, ShouldBeNil)

		invalidPackages := []string{
			"", ".", "..", "/abs", "a//", "//x", "a///b", "a//b//c", "./x", `a\b`, "a/./b", "a/../b", "a\x00b",
			"a b", "a$b", "a/", "/", "~x",
		}

		for _, pkg := range invalidPackages {
			Convey("When PackageDataPath gets an invalid id "+pkg, func() {
				path, err := s.PackageDataPath(pkg, "claude")

				Convey("Then it fails with InvalidIDError and writes nothing", func() {
					So(err, ShouldBeError)
					So(path, ShouldEqual, "")

					target, ok := errors.AsType[*InvalidIDError](err)
					So(ok, ShouldBeTrue)
					So(target.Value, ShouldEqual, pkg)
					So(target.Reason, ShouldNotBeEmpty)
				})
			})
		}

		validPackages := []string{
			"owner/name", "mcp:io.github.github/github-mcp-server", "acme/review-kit",
			"vercel-labs/skills", "owner/name_2.0", "a+b/c@d", "UPPER/Case",
			"a//b", "vercel-labs/skills//find-skills",
		}

		for _, pkg := range validPackages {
			Convey("When PackageDataPath gets the valid id "+pkg, func() {
				_, err := s.PackageDataPath(pkg, "claude")

				Convey("Then it accepts the id", func() {
					So(err, ShouldBeNil)
				})
			})
		}

		Convey("When PackageDataPath gets a canonical subpath id", func() {
			path, err := s.PackageDataPath("vercel-labs/skills//find-skills", "claude")

			Convey("Then the subpath nests as directories", func() {
				So(err, ShouldBeNil)
				So(path, ShouldEqual, filepath.Join(root, "data", "vercel-labs", "skills", "find-skills", "claude"))
			})
		})

		invalidHosts := []string{"", ".", "..", "a/b", "a b", "a:b", `a\b`, "a\x00b", "/a", "a$"}

		for _, host := range invalidHosts {
			Convey("When PackageDataPath gets the invalid host "+host, func() {
				_, err := s.PackageDataPath("owner/name", host)

				Convey("Then it fails with InvalidIDError", func() {
					So(err, ShouldBeError)

					_, ok := errors.AsType[*InvalidIDError](err)
					So(ok, ShouldBeTrue)
				})
			})
		}

		Convey("When RuntimePath gets an invalid version", func() {
			_, err := s.RuntimePath("claude", "1.2.3/beta")

			Convey("Then it fails with InvalidIDError", func() {
				So(err, ShouldBeError)

				_, ok := errors.AsType[*InvalidIDError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestStoreRetentionOption(t *testing.T) {
	Convey("Given the trash retention option", t, func() {
		Convey("When a non-positive value is set", func() {
			s, err := Open(filepath.Join(t.TempDir(), "store"), WithTrashRetention(0))

			Convey("Then the default retention applies", func() {
				So(err, ShouldBeNil)
				So(s.retention, ShouldEqual, DefaultRetention)
				So(DefaultRetention, ShouldEqual, 30*24*time.Hour)
			})
		})

		Convey("When a positive value is set", func() {
			s, err := Open(filepath.Join(t.TempDir(), "store"), WithTrashRetention(2*time.Hour))

			Convey("Then it is stored", func() {
				So(err, ShouldBeNil)
				So(s.retention, ShouldEqual, 2*time.Hour)
				So(s.Trash().retention, ShouldEqual, 2*time.Hour)
			})
		})
	})
}
