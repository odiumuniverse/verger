package apply

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/lock"
)

// The guard this file tests is the only one of the three that reads the lock
// AFTER the flock is held.
//
//	checkSchemaVersions  runs at planning time, before the flock.
//	Client.applyDeps     reads the lock once, before Run is called.
//	loadLockForRun       re-reads it here, with the flock held.
//
// The first two are defeated by a lock that changes between planning and the
// run — a second process, an upgraded install, a home shared over a synced
// filesystem. Only the third one is looking at the document at the moment the
// document can no longer be replaced, which is why it is the one that decides
// whether this run overwrites a file it has never understood.
//
// Coverage over the whole suite counted that line at 1 statement, 0 executions,
// and the mutation that neutered it (M3 in VERIFY-CP-verger-pR-19) survived:
// nothing in the repository required the one branch that protects the one loss
// that cannot be undone. This is the test that requires it.

// writeNewerLockAt puts a document on disk that no build of this program can
// read, keeping whatever cells the current lock had so the result is a plausible
// lock rather than an empty file.
//
// It edits the JSON rather than going through the lock type, and that is not a
// shortcut: (*Lock).Save and (*Lock).Marshal both refuse to write a schema this
// build cannot read. That refusal is the protection, and it also means such a
// file can only ever come from another build — so the test has to do the one
// thing this build will not.
func writeNewerLockAt(t *testing.T, path string, found int) {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the world's own lock
	if err != nil {
		t.Fatalf("read current lock: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode current lock: %v", err)
	}

	doc["schema"] = found

	newer, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode newer lock: %v", err)
	}

	if err := os.WriteFile(path, newer, 0o600); err != nil {
		t.Fatalf("write newer lock: %v", err)
	}
}

// TestANewerLockUnderTheFlockStopsTheRun is the line no test reached.
func TestANewerLockUnderTheFlockStopsTheRun(t *testing.T) {
	Convey("Given a lock this build understands, and a run about to take the flock", t, func() {
		w := newWorld(t)
		w.fake(t, fxClaude)

		So(os.MkdirAll(filepath.Dir(w.deps.LockPath), 0o700), ShouldBeNil)

		// The world starts with no lock on disk — that is a home that has never
		// synced. Seed the one this build would have written, so what is swapped
		// in afterwards is a document that replaced a live one rather than one
		// that appeared in an empty directory.
		So(w.deps.Lock.Save(w.deps.LockPath), ShouldBeNil)

		before, err := os.ReadFile(w.deps.LockPath) //nolint:gosec // G304: the world's own lock
		So(err, ShouldBeNil)

		Convey("When a newer verger's document is on disk at the moment of the read", func() {
			writeNewerLockAt(t, w.deps.LockPath, lock.Schema+1)

			newer, readErr := os.ReadFile(w.deps.LockPath) //nolint:gosec // G304: the world's own lock
			So(readErr, ShouldBeNil)

			report, runErr := Run(context.Background(), w.deps, Plan{}, Options{})

			Convey("Then the run stops instead of merging its cell into it", func() {
				So(runErr, ShouldNotBeNil)

				fatal, isFatal := errors.AsType[*lock.SchemaNewerError](runErr)
				So(isFatal, ShouldBeTrue)
				So(fatal.Found, ShouldEqual, lock.Schema+1)
				So(fatal.Supported, ShouldEqual, lock.Schema)
			})

			Convey("And the message names both versions, so the reader can act", func() {
				So(runErr, ShouldNotBeNil)
				So(runErr.Error(), ShouldContainSubstring, strconv.Itoa(lock.Schema+1))
				So(runErr.Error(), ShouldContainSubstring, strconv.Itoa(lock.Schema))
			})

			Convey("And the newer document is left byte-for-byte", func() {
				after, readErr := os.ReadFile(w.deps.LockPath) //nolint:gosec // G304: the world's own lock
				So(readErr, ShouldBeNil)
				So(after, ShouldResemble, newer)
				So(after, ShouldNotResemble, before)
			})

			Convey("And no cell was written, because a refusal that delivered is not one", func() {
				So(runErr, ShouldNotBeNil)
				So(report.Cells, ShouldBeEmpty)
			})
		})
	})
}
