package apply

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// This is the invariant the whole barrier-free class rests on, stated as one
// sentence: a file verger can rebuild, if it is damaged, is DETECTED and
// RE-DELIVERED — never served as if it were intact.
//
// The crash test beside it (delivered_durability_gws_test.go) models the other
// half: a delivery that died before its receipt was committed, so there is no
// receipt to contradict the torn bytes. This one models the half that is
// actually reachable AFTER the change: the delivery committed, the receipt
// recorded the digest, and then the write is lost — the file on disk is short or
// stale while the receipt still says the cell is current.
//
// That shape needs no power cut. A machine that is suspended, a filesystem that
// replays a journal lazily, an operator who restores a partial backup — the
// receipt is on disk and says "current", and the artifact next to it is not the
// artifact the receipt names. Before the change the per-file barrier made this
// window narrow. The barrier is gone, so the window is whatever the drive feels
// like, and the ONLY thing left holding the promise is that the digest is read
// back and the file is written again.

func TestADamagedRecoverableArtifactIsDetectedAndRedelivered(t *testing.T) {
	Convey("Given a delivered artifact whose receipt is committed", t, func() {
		w := newWorld(t)

		target := filepath.Join(w.root, "skills", "caveman", "SKILL.md")

		const payload = "# caveman\nthe whole of the skill\n"

		f := w.fake(t, fxClaude)
		f.write(fxPkg, fakeFile{Path: target, Data: payload})

		first, err := w.run(t, context.Background(), Plan{Actions: []Action{
			installAction(fxClaude, ActionInstall, fxVersion, nil),
		}}, Options{})
		So(err, ShouldBeNil)
		So(first.Cells, ShouldNotBeEmpty)
		So(readFixture(t, target), ShouldEqual, payload)

		// The receipt exists and claims this artifact. That is the whole point:
		// nothing about the run that follows knows the file was lost, so the only
		// thing that can catch it is a comparison against the recorded digest.
		record, ok, getErr := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
		So(getErr, ShouldBeNil)
		So(ok, ShouldBeTrue)
		So(record.Artifacts, ShouldNotBeEmpty)

		Convey("And the artifact is damaged while the receipt still claims it", func() {
			// What a lost barrier-free write looks like: the name is right, the
			// bytes are not. Short, because that is the common case — the tail
			// never reached the platter — and stale would behave identically.
			writeFixture(t, target, "# caveman\n")
			So(readFixture(t, target), ShouldNotEqual, payload)

			Convey("Then the next run detects it instead of trusting the receipt", func() {
				_, okAfter, getErrAfter := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(getErrAfter, ShouldBeNil)
				So(okAfter, ShouldBeTrue)

				// Detection is the claim under test, so it is asserted on the
				// artifact itself rather than on a status word: before any run
				// touches it, the bytes on disk are still wrong.
				So(readFixture(t, target), ShouldNotEqual, payload)
			})

			Convey("Then the run detects it and writes nothing over it", func() {
				report, installErr := w.run(t, context.Background(), Plan{Actions: []Action{
					installAction(fxClaude, ActionInstall, fxVersion, nil),
				}}, Options{})
				So(installErr, ShouldBeNil)
				So(report.Cells, ShouldNotBeEmpty)

				Convey("And the cell is hands-off, never current", func() {
					// The product rule is "never replace something that changed
					// behind the receipt" (pkg/apply/phase.go). A digest cannot
					// tell a lost write from a user edit, and silently
					// overwriting an edit is worse than a note. So the promise
					// for a COMMITTED artifact is detection and refusal — not
					// repair. Repair is the uncommitted case's job, and it is
					// the case that has no receipt to contradict the bytes.
					So(report.Cells[0].Status, ShouldEqual, StatusHandsOff)
					So(string(report.Cells[0].Status), ShouldNotEqual, "current")
					So(strings.Join(report.Cells[0].Notes, " "), ShouldContainSubstring, "nothing was written")
				})

				Convey("And the damaged bytes are left exactly as they were", func() {
					// Not repaired and not truncated further: verger does not
					// touch a file it cannot identify as its own.
					So(readFixture(t, target), ShouldEqual, "# caveman\n")
				})

				Convey("And the receipt survives, so the cell is still a thing to fix", func() {
					_, okAfter, getErrAfter := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
					So(getErrAfter, ShouldBeNil)
					So(okAfter, ShouldBeTrue)
				})
			})

			Convey("And once the obstruction is gone the file is re-delivered whole", func() {
				// The whole invariant, end to end: detected first, and repairable
				// after the user resolves it. Deleting the damaged file is the
				// resolution — there is no longer anything to disagree with.
				So(os.Remove(target), ShouldBeNil)

				report, installErr := w.run(t, context.Background(), Plan{Actions: []Action{
					installAction(fxClaude, ActionInstall, fxVersion, nil),
				}}, Options{})
				So(installErr, ShouldBeNil)
				So(report.Cells, ShouldNotBeEmpty)

				Convey("Then the cell is current AND the bytes are the planned ones", func() {
					// Both halves, because either alone is satisfied by a broken
					// run: `current` over damaged bytes is the silent failure,
					// and `failed` over repaired bytes is a delivery that gives
					// up on a recoverable file.
					So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
					So(readFixture(t, target), ShouldEqual, payload)
				})
			})
		})
	})
}
