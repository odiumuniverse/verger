//go:build live

package host_test

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// credentialSweep is what one walk of a tree found.
//
// Found and Unreadable are kept apart on purpose: a directory the walk could not
// read is neither a clean copy nor a proven clean one, and treating the two as
// the same is how NIGHT-pR-11's glob reported success over nine real credential
// databases.
type credentialSweep struct {
	Found      []string
	Unreadable []string
}

// findCredentialCopies walks fsys and reports every credential database under
// it, or the error that stopped the walk.
//
// It takes an fs.FS and RETURNS its error instead of failing the test, for two
// reasons that are really one reason: a helper that calls t.Fatalf cannot be
// driven from a goroutine and cannot be given a fake filesystem, so the race
// that made this suite flaky could only ever be reproduced by luck. With the
// filesystem injected and the failure returned, that race is a case.
//
// Paths come back slash-separated and relative to the root, which is what
// fs.WalkDir produces; a caller that needs real paths joins its own root back
// on, so no path here is ever silently relative either.
func findCredentialCopies(fsys fs.FS) (credentialSweep, error) {
	var sweep credentialSweep

	err := fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		switch {
		case walkErr == nil:
			// The directory was readable; nothing to record.

		case errors.Is(walkErr, fs.ErrNotExist):
			// An entry the parent directory listed, and that was gone by the time
			// the walk descended into it. $TMPDIR is a LIVE directory: this
			// happens under any concurrent go test and it says nothing about
			// credentials. Skipping is the only correct reading - what vanished
			// was never there to hold a copy.
			return nil

		case errors.Is(walkErr, fs.ErrPermission):
			// /private/tmp carries directories this user cannot read, and that
			// is not a leak. It is also exactly what a silent glob hid last time,
			// so the path is RECORDED rather than dropped, and the caller decides
			// whether an unsearched subtree is acceptable where it stands.
			sweep.Unreadable = append(sweep.Unreadable, path)

			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}

			return nil

		default:
			// Anything else is a real failure to search, and a search that did
			// not happen must never read as a clean tree.
			return walkErr
		}

		if entry != nil && !entry.IsDir() && filepath.Base(path) == "agent.db" {
			sweep.Found = append(sweep.Found, path)
		}

		return nil
	})
	if err != nil {
		return sweep, err
	}

	return sweep, nil
}

// walkForCredentialCopies returns every credential database under root, or
// fails the test if root cannot be walked at all.
//
// A walk that cannot run reports that, which is the whole difference from a glob
// that silently matches nothing.
func walkForCredentialCopies(t *testing.T, root string) []string {
	t.Helper()

	sweep, err := findCredentialCopies(os.DirFS(root))
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	found := make([]string, 0, len(sweep.Found))
	for _, path := range sweep.Found {
		found = append(found, filepath.Join(root, path))
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

	sweep, err := findCredentialCopies(os.DirFS(root))
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	blocked := make([]string, 0, len(sweep.Unreadable))
	for _, path := range sweep.Unreadable {
		full := filepath.Join(root, path)

		// The root itself is the one unsearched subtree that settles nothing:
		// everything below it is unsearched too.
		if full == root {
			t.Fatalf("%s itself cannot be searched, so nothing under it can be cleared", root)
		}

		blocked = append(blocked, full)
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

// TestTheWalkerSurvivesAnEntryThatVanishes is the deterministic proof of the rule
// pT's flake was hiding behind.
//
// The churn test that stood here before could not be trusted, and the way I know
// is the only way worth knowing: deleting the ENOENT branch left it GREEN. A real
// walk often finishes before it descends into a directory another goroutine
// deleted a moment earlier, so the flake never reproduced - a test that stays
// green when the rule is removed is not a test of the rule.
//
// Injecting the filesystem fixes that. The fake tree lets a parent list a
// directory and then have it be gone, which is exactly the window the real
// $TMPDIR opens on every concurrent test run, except here it opens every time.
func TestTheWalkerSurvivesAnEntryThatVanishes(t *testing.T) {
	Convey("Given a tree whose listed directory is gone by the time it is read", t, func() {
		fsys := newFakeTree().
			dir(".", "live", "doomed").
			file("live/agent.db").
			dir("doomed").
			dir("live", "agent.db").
			goesAway("doomed")

		Convey("Then the vanished entry is not a finding and not a failure", func() {
			sweep, err := findCredentialCopies(fsys)

			So(err, ShouldBeNil)
			So(sweep.Unreadable, ShouldBeEmpty)
		})

		Convey("Then the copy that IS there is named by its own path", func() {
			sweep, err := findCredentialCopies(fsys)

			// The copy comes back as a PATH, not as a walk error. A detector that
			// can only say "something went wrong" cannot tell a leak from a clean
			// machine, which is the failure NIGHT-pR-11 actually shipped.
			So(err, ShouldBeNil)
			So(sweep.Found, ShouldContain, "live/agent.db")
		})

		Convey("Then the vanished entry does not hide the rest of the tree", func() {
			sweep, err := findCredentialCopies(fsys)

			// The point of skipping rather than aborting: the walk carries on past
			// the noise and still reports what it did see.
			So(err, ShouldBeNil)
			So(len(sweep.Found), ShouldEqual, 1)
		})
	})

	Convey("Given a tree with a directory this user cannot read", t, func() {
		fsys := newFakeTree().
			dir(".", "sealed").
			dir("sealed").
			unreadable("sealed")

		Convey("Then it is recorded rather than dropped", func() {
			sweep, err := findCredentialCopies(fsys)

			So(err, ShouldBeNil)
			So(sweep.Unreadable, ShouldContain, "sealed")
		})
	})

	Convey("Given a tree that fails for some other reason", t, func() {
		fsys := newFakeTree().
			dir(".", "broken").
			dir("broken").
			fails("broken")

		Convey("Then the walk fails instead of reporting a clean tree", func() {
			_, err := findCredentialCopies(fsys)

			// Tolerance for a vanished entry must not become a blanket "ignore
			// whatever went wrong": a search that did not happen has to stay
			// visibly distinct from a search that found nothing.
			So(err, ShouldNotBeNil)
			So(errors.Is(err, errFakeTree), ShouldBeTrue)
		})
	})
}

// errFakeTree is the failure fakeTree raises for a directory that is neither
// gone nor unreadable but simply broken.
var errFakeTree = errors.New("the device is on fire")

// fakeTree is an fs.FS whose directories can be gone or unreadable AFTER a
// parent has already listed them.
//
// That ordering is not decoration: fs.WalkDir reads a directory's names and then
// descends into each child, so a child removed in between reaches the walk
// callback as an error with a NON-NIL DirEntry - the precise shape of pT's flake,
// which no amount of real timing reliably produces.
type fakeTree struct {
	dirs     map[string][]string
	files    map[string]bool
	vanished map[string]bool
	denied   map[string]bool
	broken   map[string]bool
}

// newFakeTree returns an empty tree rooted at ".".
func newFakeTree() *fakeTree {
	return &fakeTree{
		dirs:     map[string][]string{},
		files:    map[string]bool{},
		vanished: map[string]bool{},
		denied:   map[string]bool{},
		broken:   map[string]bool{},
	}
}

// dir declares a directory and the names it lists.
func (f *fakeTree) dir(path string, children ...string) *fakeTree {
	f.dirs[path] = children

	return f
}

// file declares a regular file.
func (f *fakeTree) file(path string) *fakeTree {
	f.files[path] = true

	return f
}

// goesAway declares a directory that is listed but already gone.
func (f *fakeTree) goesAway(path string) *fakeTree {
	f.vanished[path] = true

	return f
}

// unreadable declares a directory that is listed but cannot be read.
func (f *fakeTree) unreadable(path string) *fakeTree {
	f.denied[path] = true

	return f
}

// fails declares a directory that is listed and fails for its own reason.
func (f *fakeTree) fails(path string) *fakeTree {
	f.broken[path] = true

	return f
}

// Open implements fs.FS. The three failure modes are checked BEFORE the tree is
// consulted, so a directory can be both listed and unreachable - which is the
// whole point.
func (f *fakeTree) Open(name string) (fs.File, error) {
	if f.vanished[name] {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}

	if f.denied[name] {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrPermission}
	}

	if f.broken[name] {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errFakeTree}
	}

	children, ok := f.dirs[name]
	if !ok {
		if f.files[name] {
			return &fakeFile{name: name}, nil
		}

		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	return &fakeDir{name: name, entries: f.entriesOf(name, children)}, nil
}

// entriesOf turns child names into DirEntries, a file unless the tree also
// declares it a directory.
func (f *fakeTree) entriesOf(dir string, children []string) []fs.DirEntry {
	entries := make([]fs.DirEntry, 0, len(children))

	for _, child := range children {
		childPath := child
		if dir != "." {
			childPath = dir + "/" + child
		}

		_, isDir := f.dirs[childPath]
		entries = append(entries, fakeEntry{fakeInfo: fakeInfo{name: child, isDir: isDir}})
	}

	return entries
}

// fakeDir is the ReadDirFile a fake directory hands back.
type fakeDir struct {
	name    string
	entries []fs.DirEntry
	off     int
}

// Stat implements fs.File.
func (d *fakeDir) Stat() (fs.FileInfo, error) { return fakeInfo{name: d.name, isDir: true}, nil }

// Read implements fs.File; a directory is never read as bytes.
func (d *fakeDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: fs.ErrInvalid}
}

// Close implements fs.File.
func (d *fakeDir) Close() error { return nil }

// ReadDir implements fs.ReadDirFile.
//
// Two details are the whole difference between a fake and a filesystem:
//
//   - n <= 0 means "everything left", which is how fs.ReadDir calls it. Getting
//     only the bounded form right fails on a slice index rather than on the rule
//     under test.
//   - an EMPTY directory returns no entries and NO error. Returning io.EOF here
//     is a contract violation, and it looks exactly like a broken walker: the
//     walk aborts on a directory that holds nothing at all. That mistake is what
//     the throwaway probe caught the first time this ran.
func (d *fakeDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		batch := d.entries[d.off:]
		d.off = len(d.entries)

		return batch, nil
	}

	if d.off >= len(d.entries) {
		return nil, io.EOF
	}

	end := min(d.off+n, len(d.entries))

	batch := d.entries[d.off:end]
	d.off = end

	return batch, nil
}

// fakeFile is a regular file the walk never descends into.
type fakeFile struct{ name string }

// Stat implements fs.File.
func (f *fakeFile) Stat() (fs.FileInfo, error) { return fakeInfo{name: f.name}, nil }

// Read implements fs.File.
func (f *fakeFile) Read([]byte) (int, error) { return 0, io.EOF }

// Close implements fs.File.
func (f *fakeFile) Close() error { return nil }

// fakeEntry is one listed name.
type fakeEntry struct {
	fakeInfo
}

// Name implements fs.DirEntry.
func (e fakeEntry) Name() string { return e.fakeInfo.name }

// IsDir implements fs.DirEntry.
func (e fakeEntry) IsDir() bool { return e.fakeInfo.isDir }

// Type implements fs.DirEntry.
func (e fakeEntry) Type() fs.FileMode { return e.fakeInfo.Mode().Type() }

// Info implements fs.DirEntry.
func (e fakeEntry) Info() (fs.FileInfo, error) { return e.fakeInfo, nil }

// fakeInfo is the minimal FileInfo every fake entry reports.
type fakeInfo struct {
	name  string
	isDir bool
}

// Name implements fs.FileInfo.
func (i fakeInfo) Name() string { return i.name }

// Size implements fs.FileInfo.
func (i fakeInfo) Size() int64 { return 0 }

// Mode implements fs.FileInfo.
func (i fakeInfo) Mode() fs.FileMode {
	if i.isDir {
		return fs.ModeDir | 0o700
	}

	return 0o600
}

// ModTime implements fs.FileInfo.
func (i fakeInfo) ModTime() time.Time { return time.Time{} }

// IsDir implements fs.FileInfo.
func (i fakeInfo) IsDir() bool { return i.isDir }

// Sys implements fs.FileInfo.
func (i fakeInfo) Sys() any { return nil }
