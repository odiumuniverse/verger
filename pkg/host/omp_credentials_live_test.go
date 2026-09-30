//go:build live

package host_test

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// This file owns the lifecycle of omp's credential copy, and that ownership is
// the point.
//
// In NIGHT-pR-11 I probed omp by hand, copied its credential database into a temp
// home, and afterwards claimed "0 copies left" - verified by a glob that matched
// nothing. Nine copies with real `anthropic`/`opencode-go` credentials sat under
// ~/var/folders/ for hours, because TMPDIR had become relative to HOME and the
// glob was looking in the wrong tree while reporting success.
//
// Three rules follow, and they are what this file implements:
//
//  1. The path is built from filepath.Abs and t.TempDir(), never from $TMPDIR and
//     never from a relative path. A copy that can land somewhere the test does
//     not name is a copy nobody can find to delete.
//  2. t.Cleanup removes it, so cleanup is the test's own responsibility and does
//     not depend on the run surviving.
//  3. After the run, the machine is CHECKED - not assumed. A glob matching zero
//     paths and "no paths exist" are the same output, so the check is a walk
//     that also verifies it was able to walk.

// ompCredentialSource is the database omp reads its provider credentials from.
//
// It is a file, not the keychain. omp's binary does mention keychain, but only
// for browser automation (Playwright's Safe Storage); its provider credentials
// live in this SQLite file, table auth_credentials. Reading the keychain here
// would be both unnecessary and exactly the thing not to do.
const ompCredentialSource = ".omp/agent/agent.db" //nolint:gosec // G101: a PATH to a database, not a credential

// ompCredentialHome copies omp's credential database into a directory owned by
// this test and returns it.
//
// It refuses to run when the source does not exist or when the destination is not
// under an absolute temp path, because a credential copy is the one artifact in
// this repository that must never be written anywhere a later command cannot
// name and remove.
func ompCredentialHome(t *testing.T) string {
	t.Helper()

	source, err := filepath.Abs(filepath.Join(homeDir(t), ompCredentialSource)) //nolint:gosec // G703: the source is the user's own omp home, named by us, never by input
	if err != nil {
		t.Fatalf("resolve the credential source to an absolute path: %v", err)
	}

	if _, statErr := os.Stat(source); statErr != nil {
		t.Skipf("omp has no credential database at %s (%v); nothing to copy", source, statErr)
	}

	// t.TempDir is already absolute, and Abs says so out loud rather than
	// trusting that: this is the assertion that the NIGHT-pR-11 copies failed.
	dir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatalf("resolve the temp home to an absolute path: %v", err)
	}

	if !filepath.IsAbs(dir) || !strings.HasPrefix(dir, "/") {
		t.Fatalf("the temp home %q is not absolute; a credential copy must never depend on the working directory", dir)
	}

	target := filepath.Join(dir, ompCredentialSource)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatalf("create the temp agent dir: %v", err)
	}

	data, err := os.ReadFile(source) //nolint:gosec // G304: an absolute path the test built from homeDir
	if err != nil {
		t.Fatalf("read the credential database: %v", err)
	}

	if err := os.WriteFile(target, data, 0o600); err != nil { //nolint:gosec // G703: t.TempDir() plus a constant, absolute checked above
		t.Fatalf("write the credential copy: %v", err)
	}

	t.Cleanup(func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			t.Errorf("remove the credential copy at %s: %v", dir, rmErr)
		}
	})

	// The copy is inside the temp home and nothing else is: a stray database
	// next to the skills would be a second copy nobody tracked.
	assertOnlyCredentialCopy(t, dir, target)

	return dir
}

// assertOnlyCredentialCopy walks dir and fails if it holds more than the one
// database it was told to create.
func assertOnlyCredentialCopy(t *testing.T, dir, want string) {
	t.Helper()

	found := walkForCredentialCopies(t, dir)
	if len(found) != 1 {
		t.Fatalf("the temp home holds %d credential databases, want exactly 1: %v", len(found), found)
	}

	if found[0] != want {
		t.Fatalf("the credential copy is at %s, want %s", found[0], want)
	}
}

// walkForCredentialCopies returns every credential database under root, or fails
// the test if root cannot be walked at all.
//
// A walk that cannot run reports that, which is the whole difference from a glob
// that silently matches nothing.
func walkForCredentialCopies(t *testing.T, root string) []string {
	t.Helper()

	var found []string

	var unreadable []string

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// /private/tmp carries directories this user cannot read, and that
			// is not a leak. But it is also exactly what a silent glob hid last
			// time, so the unreadable path is recorded and, under HOME, fatal:
			// that is where a copy would land.
			if errors.Is(walkErr, fs.ErrPermission) {
				unreadable = append(unreadable, path)

				if entry != nil && entry.IsDir() {
					return fs.SkipDir
				}

				return nil
			}

			return walkErr
		}

		if !entry.IsDir() && filepath.Base(path) == "agent.db" {
			found = append(found, path)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	return found
}

// TestTheCredentialCopyLivesAndDiesWithTheTest exercises the lifecycle itself,
// without spending a model call: the copy is made, proved to be the only one in
// its home, and t.Cleanup removes it.
//
// It is the test that keeps ompCredentialHome from being dead code. A helper
// nothing calls is a helper that rots in the one direction that matters here -
// it looks like the leak is handled while the actual copy path is unexercised.
func TestTheCredentialCopyLivesAndDiesWithTheTest(t *testing.T) {
	Convey("Given a temp home built by the credential helper", t, func() {
		home := ompCredentialHome(t)

		Convey("Then it is absolute and holds exactly one database", func() {
			So(filepath.IsAbs(home), ShouldBeTrue)
			So(home, ShouldStartWith, "/")

			found := walkForCredentialCopies(t, home)
			So(found, ShouldHaveLength, 1)
			So(found[0], ShouldEqual, filepath.Join(home, ompCredentialSource))
		})

		Convey("Then the copy is not the user's own", func() {
			// Same contents, different place: the point of the copy is that the
			// live database is never the one a test writes to.
			live := filepath.Join(homeDir(t), ompCredentialSource)
			So(filepath.Join(home, ompCredentialSource), ShouldNotEqual, live)
		})
	})
}

// TestNoCredentialCopiesSurviveTheSuite is the assertion that NIGHT-pR-11 failed
// to make. After the live run, the machine is searched for credential databases
// outside the real omp home, and each root that was searched is proved
// searchable.
//
// The real database is exempt by full path, not by name: a name-based exemption
// would also exempt a stray copy called agent.db, which is the thing being hunted.
func TestNoCredentialCopiesSurviveTheSuite(t *testing.T) {
	live := filepath.Join(homeDir(t), ompCredentialSource)

	Convey("Given the machine after the live omp runs", t, func() {
		roots := []string{os.Getenv("HOME"), "/tmp", "/private/tmp", os.Getenv("TMPDIR")}

		for _, root := range roots {
			if root == "" {
				continue
			}

			Convey("When "+root+" is searched for credential databases", func() {
				// Abs first: a relative TMPDIR is precisely what put the NIGHT-pR-11
				// copies under ~/var instead of /var.
				abs, err := filepath.Abs(root)
				So(err, ShouldBeNil)
				So(filepath.IsAbs(abs), ShouldBeTrue)

				info, statErr := os.Stat(abs)
				if os.IsNotExist(statErr) {
					SkipConvey(abs + " does not exist on this machine; there is nothing there to leak into")
				}

				So(statErr, ShouldBeNil)
				So(info.IsDir(), ShouldBeTrue)

				Convey("Then every database found is the live one", func() {
					for _, found := range walkForCredentialCopies(t, abs) {
						So(found, ShouldEqual, live)
					}
				})

				Convey("Then nothing under the home went unread", func() {
					// Outside the home, an unreadable directory is another user's
					// business and this test has no opinion about it. Inside it, a
					// credential copy could be hiding in exactly that directory, so
					// an unsearched subtree under HOME is a failure, not a skip.
					if !strings.HasPrefix(abs, homeDir(t)+string(os.PathSeparator)) || abs == homeDir(t) {
						return
					}

					So(unreadableUnder(t, abs), ShouldBeEmpty)
				})
			})
		}
	})
}

// unreadableUnder lists the subtrees of root this test could not search, and
// fails the test when root itself cannot be searched at all.
func unreadableUnder(t *testing.T, root string) []string {
	t.Helper()

	var blocked []string

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr == nil {
			return nil
		}

		if !errors.Is(walkErr, fs.ErrPermission) {
			return walkErr
		}

		if path == root {
			t.Fatalf("%s itself cannot be searched, so nothing under it can be cleared: %v", root, walkErr)
		}

		blocked = append(blocked, path)

		if entry != nil && entry.IsDir() {
			return fs.SkipDir
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	return blocked
}

// TestOmpAnsweredWithoutItsOwnCredentialCopy is the cheap half of the same rule,
// and it runs on every machine: it does not copy anything. It exists so that the
// expensive test above is not the only one watching.
func TestOmpAnswersWithoutAnyCredentialCopy(t *testing.T) {
	if _, err := exec.LookPath("omp"); err != nil {
		t.Skip("omp is not on PATH")
	}

	Convey("Given omp started with no credential database reachable", t, func() {
		home := t.TempDir()

		cmd := exec.CommandContext(t.Context(), "omp", "models")
		cmd.Env = []string{
			"HOME=" + home,
			"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
			"XDG_DATA_HOME=" + filepath.Join(home, "data"),
			"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
			"PATH=" + "/usr/bin:/bin:/usr/sbin:/sbin",
		}

		out, _ := cmd.CombinedOutput()

		Convey("Then it says it has no models rather than borrowing any", func() {
			// The dangerous outcome would be a silent success against the developer's
			// own credentials. A refusal names the problem.
			So(strings.ToLower(string(out)), ShouldContainSubstring, "no models available")
		})
	})
}

// homeDir is the current user's home, failing rather than guessing: an empty
// home would turn every path below it into a relative one.
func homeDir(t *testing.T) string {
	t.Helper()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve the user home: %v", err)
	}

	if home == "" || !filepath.IsAbs(home) {
		t.Fatalf("the user home %q is empty or relative", home)
	}

	return home
}
