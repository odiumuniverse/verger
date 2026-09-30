package cli

import (
	"os"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/exitcode"
)

// TestRestoreAsksWithoutYes pins the question a restore has to ask.
//
// `requireConfirmation` only stops a run whose stdin is *detached*: on a
// terminal it returns nil, and nothing else in the restore path asked. So
// `verger restore <id>` on a real terminal put files back from the trash
// without a word to the user, while `remove` and `install` were gated by the
// same call. A write that resurrects a deleted package is exactly the write a
// user wants to be able to interrupt.
func TestRestoreAsksWithoutYes(t *testing.T) {
	Convey("Given a removed package and a terminal", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		target := w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")
		w.mustRun(t, "remove", "caveman", "-y")
		w.tty = true
		w.in = "n\n"

		out, _ := w.run("restore", "caveman")

		Convey("When restore runs without -y", func() {
			Convey("Then it asks, and a no leaves the trash alone", func() {
				So(out, ShouldContainSubstring, "Restore")
				So(out, ShouldContainSubstring, "[y/N]")
				// The file is still gone: answering no is a real no, and the
				// default of the question is no, so an empty answer is too.
				So(worldFileExists(t, target), ShouldBeFalse)
			})
		})
	})

	Convey("Given the same state and a yes", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		target := w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")
		w.mustRun(t, "remove", "caveman", "-y")
		w.tty = true
		w.in = "y\n"

		out, _ := w.run("restore", "caveman")

		Convey("Then the question is asked and the answer is honoured", func() {
			So(out, ShouldContainSubstring, "[y/N]")
			So(readWorldFile(t, target), ShouldEqual, "# installed\n")
		})
	})

	Convey("Given a terminal but -y", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		target := w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")
		w.mustRun(t, "remove", "caveman", "-y")
		w.tty = true

		out, _ := w.run("restore", "caveman", "-y")

		Convey("Then it does not ask, because the answer was given on the line", func() {
			// -y is a script's and a cron job's answer. Asking anyway would
			// make the flag useless and hang anything that pipes in.
			So(out, ShouldNotContainSubstring, "[y/N]")
			So(readWorldFile(t, target), ShouldEqual, "# installed\n")
		})
	})
}

// TestRestoreWithoutYesAndWithoutATerminalRefuses keeps the non-interactive
// half honest: a detached stdin is not a yes, and the run must stop with the
// typed error a script can recognise rather than restoring behind the
// caller's back.
func TestRestoreWithoutYesAndWithoutATerminalRefuses(t *testing.T) {
	Convey("Given a removed package and a detached stdin", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		target := w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")
		w.mustRun(t, "remove", "caveman", "-y")

		_, err := w.run("restore", "caveman")

		Convey("When restore runs with no -y", func() {
			Convey("Then it refuses and writes nothing", func() {
				So(err, ShouldNotBeNil)
				So(err, ShouldWrap, ErrConfirmationRequired)
				So(worldFileExists(t, target), ShouldBeFalse)
			})
		})
	})
}

// TestRestoreWithNoAnswerIsNotANo is the case a script actually hits. The
// terminal check is a character-device test, and /dev/null is a character
// device: a run with stdin redirected from it looks interactive, the question
// is printed, and the read comes back empty. Treating that empty read as "no"
// made the run print "nothing was written" and exit 0 - a refusal reported as
// a success. An unanswered question is a pending question, and it has to leave
// through the typed error so the exit code says so.
func TestRestoreWithNoAnswerIsNotANo(t *testing.T) {
	Convey("Given a removed package and a stdin that answers nothing", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		target := w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")
		w.mustRun(t, "remove", "caveman", "-y")

		w.tty = true
		w.in = "" // /dev/null: the read returns EOF, not an answer

		_, err := w.run("restore", "caveman")

		Convey("Then it refuses with the typed error a script recognises", func() {
			So(err, ShouldWrap, ErrConfirmationRequired)
		})

		Convey("Then nothing was restored", func() {
			So(worldFileExists(t, target), ShouldBeFalse)
		})

		Convey("Then the exit code is consent, not success and not conflict", func() {
			So(exitcode.Classify(err), ShouldEqual, exitcode.Consent)
		})
	})
}

// worldFileExists reports whether the target is there, so a test can say
// "nothing was written" without asserting on output.
func worldFileExists(t *testing.T, path string) bool {
	t.Helper()

	_, err := os.Stat(path)

	return err == nil
}
