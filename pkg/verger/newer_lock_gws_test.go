package verger

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/lock"
)

// A lock written by a newer verger is the one loss in this program that cannot be
// undone: this build has never understood the document, so anything it writes
// over it destroys fields it could not read. Carrying on quietly is the one
// answer the package must never give.
//
// The guard that stops it lives in two places, and until now only one of them
// was reachable by a test.
//
//	apply.loadLockForRun   reads the lock INSIDE the flock, at run time, and is
//	                       fatal on a newer schema. Coverage measured over the
//	                       whole pkg/verger suite counted that line: 1 statement,
//	                       0 executions.
//	pkg/verger.checkSchemaVersions
//	                       refuses the same document at PLANNING time, which is
//	                       why every user-visible verb already answered 7 and no
//	                       test had to reach the run-time guard to prove it.
//
// So the branch that protects the irreversible loss was the branch no test
// required, and the mutation that neutered it (M3 in VERIFY-CP-verger-pR-19)
// survived the suite. The two tests below close that: the first pins the
// user-visible contract for all four writing verbs, the second drives a newer
// lock into the run-time guard by putting it there AFTER planning succeeded —
// the only way in, and a real race rather than a contrivance.

// writeNewerLock rewrites the scope's lock with a schema no build of this
// program understands, keeping the cells it already had so the document stays a
// plausible one instead of an empty stub no verb would act on.
//
// It edits the JSON as a document rather than through the lock type, and that
// is not a shortcut: (*Lock).Save refuses to write a document this build cannot
// read, and so does (*Lock).Marshal. The protection is real and it is the
// protection under test — but it also means such a file can only ever come from
// another build, so producing one here has to be the one thing this build
// refuses to do.
func writeNewerLock(t *testing.T, path string, found int) {
	t.Helper()

	current, err := lock.ParseFile(path)
	if err != nil {
		t.Fatalf("read the lock this build wrote: %v", err)
	}

	data, err := current.Marshal()
	if err != nil {
		t.Fatalf("marshal the current lock: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode the current lock: %v", err)
	}

	doc["schema"] = found

	newer, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode the newer lock: %v", err)
	}

	if err := os.WriteFile(path, newer, 0o600); err != nil {
		t.Fatalf("write newer lock: %v", err)
	}
}

// lockBytes is the lock as bytes, because "untouched" is a claim about the file
// and not about whatever parsed it.
func lockBytes(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the scope's own lock
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}

	return data
}

// exists is the filesystem's answer, used where the question is "did a refusal
// still deliver half the package".
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestANewerLockIsRefusedByEveryDeliveryVerb pins what a user sees, for every
// verb that writes. The contract is four things at once and each one alone would
// be satisfied by a broken run: the exit code (7 — a script's only channel), the
// message (it has to name the versions or the reader cannot act), the lock
// (byte-for-byte, or the field this guard exists to protect is gone), and the
// absence of any host file (a refusal that already wrote half a delivery is not
// a refusal).
func TestANewerLockIsRefusedByEveryDeliveryVerb(t *testing.T) {
	Convey("Given a package installed on this build's schema", t, func() {
		world, client := newFacadeWorld(t)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		ref := world.fixture(t, "hooked", "1.0.0")

		// The fake writes to a real path, so "nothing was written" is a
		// statement about the disk rather than about a counter: an adapter that
		// was never asked still leaves no file behind, and one that was asked
		// and refused must leave none either.
		target := filepath.Join(world.user, ".claude", "skills", "one", "SKILL.md")
		world.fake.artifacts = []fakeArtifact{{Path: target, Data: "# one\n"}}

		// The refusal may arrive at planning or at run time — both are refusals
		// and the caller cannot tell them apart, which is the point. What must
		// never happen is a stage returning nil while the other one delivers.
		install := func() error {
			plan, planErr := client.Plan(t.Context(), PlanOptions{
				Paths: paths, Refs: []string{ref}, Hosts: []host.Host{world.fake},
			})
			if planErr != nil {
				return planErr
			}

			_, applyErr := client.Install(t.Context(), plan, ApplyOptions{})

			return applyErr
		}

		So(install(), ShouldBeNil)
		So(exists(target), ShouldBeTrue)

		Convey("When a newer verger is then the writer of the lock", func() {
			writeNewerLock(t, paths.LockPath, lock.Schema+1)

			before := lockBytes(t, paths.LockPath)

			assertRefused := func(runErr error) {
				So(runErr, ShouldNotBeNil)

				newer := &lock.SchemaNewerError{}
				So(errors.As(runErr, &newer), ShouldBeTrue)

				// The message is the only thing a user has to act on, so it has
				// to carry both versions and the way out.
				msg := runErr.Error()
				So(msg, ShouldContainSubstring, "newer verger")
				So(msg, ShouldContainSubstring, strconv.Itoa(lock.Schema+1))

				So(lockBytes(t, paths.LockPath), ShouldResemble, before)
			}

			Convey("Then install refuses it", func() {
				assertRefused(install())
			})

			Convey("Then sync refuses it", func() {
				_, _, runErr := client.Sync(t.Context(), SyncOptions{Paths: paths})
				assertRefused(runErr)
			})

			Convey("Then update refuses it", func() {
				_, _, runErr := client.Update(t.Context(), UpdateOptions{Paths: paths})
				assertRefused(runErr)
			})

			Convey("Then remove refuses it", func() {
				plan, planErr := client.PlanRemove(t.Context(), "hooked", RemoveOptions{
					Paths: paths, Hosts: []host.Host{world.fake},
				})
				So(planErr, ShouldBeNil)
				So(len(plan.Actions), ShouldBeGreaterThan, 0)

				_, runErr := client.Remove(t.Context(), plan, ApplyOptions{})
				assertRefused(runErr)
			})

			Convey("Then no host file was written", func() {
				// Remove the file the first install left, so a leftover from
				// before the lock went bad cannot be read as a fresh write.
				So(os.Remove(target), ShouldBeNil)
				So(exists(target), ShouldBeFalse)

				_ = install()

				So(exists(target), ShouldBeFalse)
			})
		})
	})
}

// TestANewerLockThatArrivesAfterPlanningStopsTheRun reaches the guard the matrix
// above cannot: checkSchemaVersions already refused the newer lock at planning
// time, so loadLockForRun — the read that happens INSIDE the flock, at run time
// — never sees one.
//
// That is a real race, not a contrived one. Planning and the run are two separate
// moments, and anything that can write the lock in between (a second process, an
// upgraded install, a home shared over a synced filesystem) puts a document from
// the future into a run this build had already approved. Without the run-time
// guard that run reads a document it does not understand, merges its own cell
// into it and saves — the overwrite the guard's own comment calls the one loss
// that cannot be undone.
//
// Coverage measured that line at 1 statement, 0 executions across the whole
// suite. This is the test that closes it.
func TestANewerLockThatArrivesAfterPlanningStopsTheRun(t *testing.T) {
	Convey("Given a plan built while the lock was still readable", t, func() {
		world, client := newFacadeWorld(t)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		ref := world.fixture(t, "late", "1.0.0")

		target := filepath.Join(world.user, ".claude", "skills", "one", "SKILL.md")
		world.fake.artifacts = []fakeArtifact{{Path: target, Data: "# one\n"}}

		// A lock this build can read has to exist first, or there is nothing for
		// the newer build to have written over and the test would be measuring a
		// missing file rather than a swapped one.
		So(os.MkdirAll(filepath.Dir(paths.LockPath), 0o700), ShouldBeNil)
		So((&lock.Lock{Schema: lock.Schema}).Save(paths.LockPath), ShouldBeNil)

		plan, err := client.Plan(t.Context(), PlanOptions{
			Paths: paths, Refs: []string{ref}, Hosts: []host.Host{world.fake},
		})
		So(err, ShouldBeNil)

		Convey("When a newer verger writes the lock before the run takes the flock", func() {
			writeNewerLock(t, paths.LockPath, lock.Schema+1)

			before := lockBytes(t, paths.LockPath)

			runErr := func() error {
				_, applyErr := client.Install(t.Context(), plan, ApplyOptions{})
				return applyErr
			}

			Convey("Then the run stops instead of merging into it", func() {
				err := runErr()
				So(err, ShouldNotBeNil)

				newer := &lock.SchemaNewerError{}
				So(errors.As(err, &newer), ShouldBeTrue)
				So(err.Error(), ShouldContainSubstring, strconv.Itoa(lock.Schema+1))
			})

			Convey("And the newer document is left exactly as it was", func() {
				So(runErr(), ShouldNotBeNil)
				So(lockBytes(t, paths.LockPath), ShouldResemble, before)
			})

			Convey("And nothing was delivered", func() {
				So(runErr(), ShouldNotBeNil)
				So(exists(target), ShouldBeFalse)
			})
		})
	})
}

// TestNewerLockMessageNamesBothVersions is the smallest thing a reader needs: a
// message that says only "bad lock" sends them to the file instead of to the
// fix, and the fix is always the same one.
func TestNewerLockMessageNamesBothVersions(t *testing.T) {
	Convey("Given the error the lock reader raises", t, func() {
		err := &lock.SchemaNewerError{Path: "/h/verger.lock", Found: 99, Supported: lock.Schema}

		Convey("Then it names the version found, the version supported and the fix", func() {
			msg := err.Error()
			So(msg, ShouldContainSubstring, "99")
			So(msg, ShouldContainSubstring, strconv.Itoa(lock.Schema))
			So(strings.Contains(msg, "update"), ShouldBeTrue)
		})
	})
}
