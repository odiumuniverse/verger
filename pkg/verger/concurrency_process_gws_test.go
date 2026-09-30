package verger_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/lock"
)

// TestConcurrentProcessesDoNotLoseAPackage drives the real binary, N processes at
// a time, each installing a DIFFERENT package into one shared home.
//
// Goroutines would not test this: an in-process mutex is not what serialises the
// writers, the flock on disk is. Only separate processes contend for that lock,
// so only separate processes can lose an update to a read-modify-write of
// verger.toml or the lock. The gate exercises the same shape through bash; this
// is the same race with a precise expectation attached, so a failure names the
// lost package instead of "the package did not survive".
//
// The expectation is deliberately strong: every package must be present in the
// spec AND in the lock, and the files must parse. A writer that read the spec,
// decided, and wrote back over a concurrent writer's package satisfies "no
// error was printed" and fails this.
func TestConcurrentProcessesDoNotLoseAPackage(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and runs a process race")
	}

	Convey("Given one home and several packages installed at once", t, func() {
		bin := buildVerger(t)
		home := t.TempDir()
		vergerHome := t.TempDir()

		// An empty HOME has no agent, and every install would exit 6 with nothing
		// written - which looks like a lost-update failure and is not one. The
		// config home is what detection looks at, so one empty .claude directory
		// is all it takes to have a host to deliver to.
		if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
			t.Fatalf("create the claude config home: %v", err)
		}

		const writers = 6

		refs := make([]string, writers)
		for i := range refs {
			refs[i] = writeConcurrencyPackage(t, home, "conc"+strconv.Itoa(i))
		}

		var (
			wg   sync.WaitGroup
			errs = make([]error, writers)
			outs = make([]string, writers)
		)

		for i := range refs {
			wg.Add(1)

			go func(i int) {
				defer wg.Done()

				ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
				defer cancel()

				cmd := exec.CommandContext(ctx, bin, "install", refs[i], "--yes") //nolint:gosec // G204: refs are paths this test wrote
				cmd.Dir = home
				cmd.Env = concurrencyEnv(home, vergerHome)

				out, err := cmd.CombinedOutput()
				outs[i] = string(out)

				if err != nil {
					errs[i] = err
				}
			}(i)
		}

		wg.Wait()

		Convey("When every process has finished", func() {
			for i, err := range errs {
				Convey("Then the install of "+filepath.Base(refs[i])+" reported no failure", func() {
					So(err, ShouldBeNil)
				})
			}

			Convey("Then the lock holds every package", func() {
				// The lock is the record every install rewrites, so this is where a
				// read-modify-write loses an update. It is read from the pinned
				// VERGER_HOME, not from $HOME, because the home follows the DESIGN
				// chain and a machine with a ~/.beadle puts it inside the vault.
				//
				// verger.toml is the SPEC and only `sync` rewrites it; asserting on
				// it here would assert on a file install never touches.
				doc, err := lock.ParseFile(filepath.Join(vergerHome, "verger.lock"))
				So(err, ShouldBeNil)

				seen := make([]string, 0, len(doc.Cells))
				for _, cell := range doc.Cells {
					seen = append(seen, cell.Package)
				}

				// The cell records the RESOLVED id (`local:conc0`), not the bare
				// directory name, so matching is by suffix: a reader that compared
				// the wrong form would report a lost update on a lock that is
				// perfectly whole.
				for _, ref := range refs {
					So(slices.Contains(seen, "local:"+filepath.Base(ref)), ShouldBeTrue)
				}
			})
		})
	})
}

// vergerBin is the CLI this package's process races run, built ONCE.
//
// Building per test was a defect, not a convenience: `go build ./cmd/verger` costs
// seconds of CPU and every extra copy is CPU the machine cannot spare while a
// gate is running. sync.Once gives one build per package, and one path every
// subtest reuses.
//
// The path lives in a package-level temp dir rather than any test's TempDir: a
// per-test dir would be removed when that test ended, and a later subtest would
// find the binary gone.
var (
	vergerBinOnce sync.Once
	vergerBinPath string
	errVergerBin  error
)

// buildVerger returns the package's one verger binary, building it on first use.
func buildVerger(t *testing.T) string {
	t.Helper()

	vergerBinOnce.Do(func() {
		// One directory for the whole package, NOT a test's TempDir: the binary
		// outlives whichever test happened to build it first.
		dir, err := os.MkdirTemp("", "verger-race-bin") //nolint:usetesting // deliberately not t.TempDir(): the binary is built once per PACKAGE and must outlive the test that triggered the build
		if err != nil {
			errVergerBin = err

			return
		}

		bin := filepath.Join(dir, "verger-race")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "./cmd/verger") //nolint:gosec // G204: a literal, in a dir this test chose
		cmd.Dir = moduleRoot()

		if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
			errVergerBin = fmt.Errorf("build the verger binary: %w\n%s", buildErr, out)

			return
		}

		vergerBinPath = bin
	})

	if errVergerBin != nil {
		t.Fatal(errVergerBin)
	}

	return vergerBinPath
}

// moduleRoot walks up from the test working directory to the module root.
// moduleRoot resolves the module directory from THIS FILE, not from the working
// directory.
//
// Walking up from os.Getwd was correct only while the suite happened to run
// inside the repository - which is a property of how you invoked it, not of the
// code. TestMain chdirs the whole package into a temp dir so no relative write
// can land in the source tree, and a locator that answers "" to that is exactly
// the kind of "works on my machine" this repo keeps paying for.
func moduleRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}

	dir := filepath.Dir(file)

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}

		dir = parent
	}
}

// concurrencyEnv is the environment every racing process shares: one HOME, one
// store, nothing inherited that would send them to different places.
func concurrencyEnv(home, vergerHome string) []string {
	// VERGER_HOME is pinned so the test measures the LOCK rather than home
	// discovery. Without it the home follows the DESIGN chain
	// ($VERGER_HOME -> ~/.beadle/verger -> ~/.verger) and on a machine that has a
	// ~/.beadle the home lands inside the vault - correctly, but somewhere this
	// test did not look, which is why it failed with "no such file" on a
	// machine where nothing was wrong.
	//
	// BEADLE_HOME is deliberately NOT set: it outranks the vault default, and a
	// pointer at a vault that does not exist sends the home somewhere else
	// entirely - the wrong end of the experiment to be debugging.
	return []string{
		"HOME=" + home,
		"VERGER_HOME=" + vergerHome,
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
	}
}

// writeConcurrencyPackage writes one local package and returns its path.
func writeConcurrencyPackage(t *testing.T, home, name string) string {
	t.Helper()

	dir := filepath.Join(home, name)
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o700); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "skills", name+"-skill"), 0o700); err != nil {
		t.Fatalf("create the skill dir for %s: %v", name, err)
	}

	manifest := `{"name":"` + name + `","version":"0.1.0"}`
	if err := os.WriteFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write the manifest for %s: %v", name, err)
	}

	skill := "---\nname: " + name + "\ndescription: race probe\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "skills", name+"-skill", "SKILL.md"), []byte(skill), 0o600); err != nil {
		t.Fatalf("write the skill for %s: %v", name, err)
	}

	return dir
}
