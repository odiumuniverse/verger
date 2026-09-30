package verger

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// isolatedHome is the temp home TestMain pins the environment to.
var isolatedHome string

// TestMain pins HOME, VERGER_HOME, BEADLE_HOME, XDG_CONFIG_HOME and
// XDG_DATA_HOME before the suite runs. VERGER_HOME/BEADLE_HOME stay empty so
// the default discovery path (source "default") is exercised, and the suite is
// checked afterwards for having written into its own source directory.
func TestMain(m *testing.M) {
	os.Exit(runIsolatedSuite(m)) // os.Exit skips defers: clean up inside the helper
}

// runIsolatedSuite runs the suite and then fails it if the package directory
// gained anything a delivery writes.
func runIsolatedSuite(m *testing.M) int {
	code := isolateTestHome(m)
	if code != 0 {
		return code
	}

	return assertPackageDirClean()
}

// packageDirJunk is what a delivery leaves beside the package when it is handed
// an empty or relative home. A project-scoped delivery rooted at the working
// directory writes exactly these, which is how `pkg/verger/.claude/skills/one/`
// appeared in the source tree: a test resolving a root from the cwd instead of
// from a temp dir.
//
// verger.toml and verger.lock are here for the same reason - a user-scope
// delivery whose home resolved to "" writes them here too, and a lock file in
// the source tree is the most expensive of the three to notice.
var packageDirJunk = []string{
	".claude", ".gemini", ".codex", ".cursor", ".omp",
	".agents", ".kilo", ".pi", ".dsh", ".verger",
	"verger.toml", "verger.lock",
}

// assertPackageDirClean reports the suite if the directory holding this file
// gained anything a delivery writes.
//
// It runs AFTER m.Run, which is the only moment the answer means anything: a
// check that ran before the suite would pass on exactly the machine that is
// already dirty, which is the machine worth catching.
func assertPackageDirClean() int {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		trace("cannot locate the package dir, so cannot prove the suite left it alone")

		return 1
	}

	dir := filepath.Dir(file)

	var junk []string

	for _, name := range packageDirJunk {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			junk = append(junk, name)
		}
	}

	if len(junk) > 0 {
		trace("the suite wrote into its own source dir " + dir + ": " + strings.Join(junk, ", "))

		return 1
	}

	return 0
}

// isolateTestHome runs the suite from INSIDE a temp directory.
//
// The chdir is the belt to the adapter fix's braces, and it lives here rather
// than in the offending test for two reasons. First, t.Chdir needs a
// *testing.T and the offender was an Example, which has none - there is no
// t.Chdir available where the write happened. Second, a suite-wide chdir is
// strictly stronger than fixing one caller: ANY relative path a future test
// builds lands in the temp dir, so the failure this whole exercise exists to
// prevent cannot recur at all, rather than recurring one test at a time.
func isolateTestHome(m *testing.M) int {
	home, err := os.MkdirTemp("", "verger-facade-test-home") //nolint:usetesting // TestMain has no *testing.T to hang t.TempDir on
	if err != nil {
		trace("create test home: " + err.Error())

		return 1
	}

	defer func() { _ = os.RemoveAll(home) }()

	// The working directory needs its own temp dir, and the deferred chdir back
	// out of it has to happen BEFORE the removal above, or the cleanup runs with
	// the process standing in a directory that no longer exists.
	work, err := os.MkdirTemp("", "verger-facade-test-cwd") //nolint:usetesting // see above
	if err != nil {
		trace("create test cwd: " + err.Error())

		return 1
	}

	defer func() { _ = os.RemoveAll(work) }()

	if err := os.Chdir(work); err != nil {
		trace("enter test cwd: " + err.Error())

		return 1
	}

	defer func() { _ = os.Chdir(os.TempDir()) }()

	for name, value := range map[string]string{
		"HOME":            home,
		"VERGER_HOME":     "",
		"BEADLE_HOME":     "",
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
	} {
		//nolint:usetesting // TestMain cannot use t.Setenv; tests override per test
		if err := os.Setenv(name, value); err != nil {
			trace("set " + name + ": " + err.Error())

			return 1
		}
	}

	isolatedHome = home

	return m.Run()
}

// trace writes a TestMain diagnostic to stderr.
func trace(message string) {
	_, _ = os.Stderr.WriteString(message + "\n")
}

func TestSuiteHomeIsolation(t *testing.T) {
	Convey("Given the isolated test suite", t, func() {
		Convey("When a test reads the environment", func() {
			Convey("Then every home-shaped variable points into the temp isolation dir or is empty", func() {
				So(isolatedHome, ShouldNotBeEmpty)

				home, err := os.UserHomeDir()
				So(err, ShouldBeNil)
				So(strings.HasPrefix(home, isolatedHome), ShouldBeTrue)

				So(os.Getenv("VERGER_HOME"), ShouldBeEmpty)
				So(os.Getenv("BEADLE_HOME"), ShouldBeEmpty)
				So(os.Getenv("XDG_CONFIG_HOME"), ShouldContainSubstring, isolatedHome)
				So(os.Getenv("XDG_DATA_HOME"), ShouldContainSubstring, isolatedHome)
			})
		})
	})
}

// TestSuiteLeavesNoFilesInThePackageDir is the named, runnable half of the
// hermeticity rule; assertPackageDirClean is the half that actually holds the
// line, because it runs after m.Run.
//
// The split is deliberate and it is the whole point. A test cannot observe what
// its neighbours did, so a plain test would sample the package dir at whatever
// moment the scheduler reached it - and the write it is hunting usually happens
// after. So this test pins the rule where it is visible (right now, by name,
// and it fails if someone commits the junk back), and TestMain pins it where it
// is decisive (after everything).
func TestSuiteLeavesNoFilesInThePackageDir(t *testing.T) {
	Convey("Given the directory this suite lives in", t, func() {
		_, file, _, ok := runtime.Caller(0)
		So(ok, ShouldBeTrue)

		dir := filepath.Dir(file)

		Convey("Then nothing a delivery writes is sitting in it", func() {
			var junk []string

			for _, name := range packageDirJunk {
				if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
					junk = append(junk, name)
				}
			}

			SoMsg(dir, junk, ShouldBeEmpty)
		})

		Convey("And the working directory is not the source tree either", func() {
			// isolateTestHome chdir'd the suite into a temp dir, so a relative
			// path cannot land beside the source at all. This is the belt: the
			// named rule above says the tree is clean, this says it cannot get
			// dirty while the suite runs.
			wd, err := os.Getwd()
			So(err, ShouldBeNil)
			So(wd, ShouldNotEqual, dir)
		})
	})
}
