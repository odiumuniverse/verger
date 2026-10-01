package apply

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/receipt"
)

// A delivered file is written without a per-file fsync barrier, because the
// receipt carries the digest of every artifact and the next read checks it. That
// is only safe while the check really runs, so this is the test that says it does.
//
// The crash being modelled is the one the barrier used to prevent: the delivery
// put bytes on disk, the run died before it could commit a receipt, and the file
// that survived is SHORT — the name is right, the bytes are not. A barrier-free
// write makes this shape likelier, so the shape has to be detected.

func TestATornDeliveredFileIsNeverReportedCurrent(t *testing.T) {
	Convey("Given an install whose delivery wrote a file and then died before committing", t, func() {
		w := newWorld(t)

		target := filepath.Join(w.root, "skills", "caveman", "SKILL.md")

		const payload = "# caveman\nthe whole of the skill\n"

		f := w.fake(t, fxClaude)
		f.write(fxPkg, fakeFile{Path: target, Data: payload})
		// The crash itself, modelled as what a dead process leaves behind: an
		// intent in the journal that no run ever closed.
		//
		// The first draft drove this through the fake host's failAfterWrite knob
		// instead. That models a HANDLED failure, not a crash: the run reached
		// installFailed, rolled back and closed the intent, so the next run had
		// nothing to recover and the test passed whether or not the digest was
		// ever checked. Both mutations below survived it. Writing the intent
		// directly is the only way to reach recoverInstall at all.
		journalInterruptedInstall(t, w, installAction(fxClaude, ActionInstall, fxVersion, nil), target, payload)

		Convey("And the file that survived the crash is short", func() {
			// A torn write: the file exists and the name is right, but the bytes
			// are not what was planned. This is the artifact the removed barrier
			// now makes reachable.
			writeFixture(t, target, "# caveman\n")
			So(fileExists(target), ShouldBeTrue)
			So(readFixture(t, target), ShouldNotEqual, payload)

			Convey("Then the next run refuses to call it current", func() {
				f.mu.Lock()
				f.failAfterWrite = false
				f.mu.Unlock()

				_, recoverErr := w.run(t, context.Background(), Plan{}, Options{})
				So(recoverErr, ShouldBeNil)

				// The claim, stated so it cannot pass vacuously: recovery
				// compared the torn bytes against the digest the journaled intent
				// recorded, they did not match, and so NO RECEIPT WAS COMMITTED.
				// A run that trusted presence instead would leave one here, and
				// every later run would answer "current" about a file that is not
				// what was planned.
				//
				// The first draft of this test looped over report.Cells asserting
				// none was current. A recovery over an empty plan produces no
				// cells at all — measured — so that loop never ran and the test
				// proved nothing. This is the same trap as the reconcile test in
				// VERIFY-CP-verger-pR-24, and the same guard against it: assert
				// on something that exists.
				_, ok, getErr := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(getErr, ShouldBeNil)
				So(ok, ShouldBeFalse)

				// The torn file itself is left on disk, and that is right: the
				// intent's RMA is keyed by digest, and verger does not delete
				// bytes it does not recognise as its own. What must not happen is
				// the receipt — so the re-delivery below is the assertion that
				// matters, and it is not vacuous because report.Cells is
				// checked to be non-empty first.

				Convey("And a following install re-delivers it whole", func() {
					report, installErr := w.run(t, context.Background(), Plan{Actions: []Action{
						installAction(fxClaude, ActionInstall, fxVersion, nil),
					}}, Options{})
					So(installErr, ShouldBeNil)
					So(report.Cells, ShouldNotBeEmpty)
					So(report.Cells[0].Status, ShouldEqual, StatusCurrent)

					So(readFixture(t, target), ShouldEqual, payload)
				})
			})
		})
	})
}

// journalInterruptedInstall writes the intent a run journals before it delivers
// and never closes, which is exactly what a process killed between those two
// moments leaves in the journal. The next Run finds it in recover() and has to
// decide whether the artifacts on disk are the ones it planned.
func journalInterruptedInstall(t *testing.T, w *world, action Action, target, payload string) {
	t.Helper()

	sum := digest.Bytes([]byte(payload))

	raw, err := json.Marshal(intentRecord{
		Action: action,
		Artifacts: []receipt.Artifact{
			{Kind: "skill", Name: filepath.Base(target), Path: target, Digest: sum},
		},
		RMA: []receipt.Op{
			{Kind: receipt.OpWriteFile, Path: target, Digest: sum, Mode: 0o600},
		},
	})
	if err != nil {
		t.Fatalf("encode intent: %v", err)
	}

	if err := w.deps.Journal.Append(receipt.Event{
		Kind:    eventKindFor(action.Kind),
		Package: fxPkg, Host: string(fxClaude), Scope: fxUser, Version: fxVersion,
		Extra: map[string]json.RawMessage{intentExtraKey: raw},
	}); err != nil {
		t.Fatalf("journal intent: %v", err)
	}
}

// writeThroughDurable and writeThroughCheap are the two disciplines, named, so
// the comparison below is between two writers rather than between two inlined
// copies of the same call.
func writeThroughDurable(t *testing.T, path string) error {
	t.Helper()

	return fsutil.WriteFileAtomic(path, []byte("body\n"), 0o600)
}

func writeThroughCheap(t *testing.T, path string) error {
	t.Helper()

	return fsutil.WriteFileAtomicCAS(path, []byte("body\n"), 0o600)
}

// The barrier is what makes a file survive a power cut; the digest is what makes
// a file that did not survive detectable. This pins that the two are not confused
// with each other — the durable writer keeps both barriers, and the barrier-free
// one drops both, with no third shape in between.
//
// The counts are the point. This test used to compare only bytes and modes,
// which are identical whichever writer ran, so its claim — "one drops both" —
// was asserted nowhere and a later edit that gave the cheap writer a barrier
// would have left it green. fsutil.CountBarriers is the seam that makes the
// claim observable from this package.
func TestTheTwoWritersDifferOnlyInTheirBarriers(t *testing.T) {
	Convey("Given a directory and two writers", t, func() {
		dir := t.TempDir()

		durable := filepath.Join(dir, "durable")
		cheap := filepath.Join(dir, "cheap")

		// Whatever an earlier test in this package left pending is not this
		// test's business, and draining it is what makes the flush below a
		// count of one rather than a count of however many tests ran first.
		So(fsutil.SyncPendingDirs(), ShouldBeNil)

		durableFileSyncs, durableDirSyncs, err := fsutil.CountBarriers(func() error {
			return writeThroughDurable(t, durable)
		})
		So(err, ShouldBeNil)

		cheapFileSyncs, cheapDirSyncs, err := fsutil.CountBarriers(func() error {
			return writeThroughCheap(t, cheap)
		})
		So(err, ShouldBeNil)

		Convey("Then the durable writer pays two barriers for the one write", func() {
			So(durableFileSyncs, ShouldEqual, 1)
			So(durableDirSyncs, ShouldEqual, 1)
			So(durableFileSyncs+durableDirSyncs, ShouldEqual, 2)
		})

		Convey("And the cheap writer pays none per file", func() {
			So(cheapFileSyncs, ShouldEqual, 0)
			So(cheapDirSyncs, ShouldEqual, 0)
			So(cheapFileSyncs+cheapDirSyncs, ShouldEqual, 0)
		})

		Convey("And it leaves its directory to one batch barrier at the end", func() {
			// The rename still has to become durable, so the cheap write pays
			// later and once: the flush the runner makes for the package.
			flushFileSyncs, flushDirSyncs, err := fsutil.CountBarriers(fsutil.SyncPendingDirs)
			So(err, ShouldBeNil)
			So(flushFileSyncs, ShouldEqual, 0)
			So(flushDirSyncs, ShouldEqual, 1)
		})

		Convey("Then both leave the same bytes and the same mode", func() {
			So(readFixture(t, durable), ShouldEqual, readFixture(t, cheap))

			durableInfo, err := os.Stat(durable)
			So(err, ShouldBeNil)

			cheapInfo, err := os.Stat(cheap)
			So(err, ShouldBeNil)

			So(cheapInfo.Mode().Perm(), ShouldEqual, durableInfo.Mode().Perm())
		})

		Convey("Then neither leaves a temporary file behind", func() {
			entries, err := os.ReadDir(dir)
			So(err, ShouldBeNil)
			So(len(entries), ShouldEqual, 2)
		})
	})
}
