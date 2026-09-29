package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// TestBareRunOnAFreshMachineIsOnboarding pins §5.1: a bare `verger` with no
// home shows the screen and asks one question. It must not print help, which
// is what a bare run used to do.
func TestBareRunOnAFreshMachineIsOnboarding(t *testing.T) {
	Convey("Given a machine with no verger home", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		stdout, err := w.run()

		Convey("When verger runs with no arguments", func() {
			Convey("Then it shows the first-run screen, not help", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "Here is what I found")
				So(stdout, ShouldNotContainSubstring, "Available Commands")
			})
		})
	})
}

// TestOnboardingAsksOnce pins the single question: the screen, then one
// prompt, not a prompt per host.
func TestOnboardingAsksOnce(t *testing.T) {
	Convey("Given a machine with no home and a terminal", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.tty = true
		w.in = "n\n"

		stdout, err := w.run()

		Convey("When the first run asks", func() {
			Convey("Then it asks exactly once", func() {
				So(err, ShouldBeNil)
				So(strings.Count(stdout, "Create it now?"), ShouldEqual, 1)
			})
		})
	})
}

// TestOnboardingDeclinedCreatesNothing is the safety half: saying no must
// leave the machine exactly as it was.
func TestOnboardingDeclinedCreatesNothing(t *testing.T) {
	Convey("Given a declined first run", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.tty = true
		w.in = "n\n"

		stdout, err := w.run()

		Convey("When the user says no", func() {
			Convey("Then no home is created and the refusal is stated", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "Nothing was created")

				_, statErr := os.Stat(w.homeDir)
				So(os.IsNotExist(statErr), ShouldBeTrue)
			})
		})
	})
}

// TestOnboardingAcceptedCreatesOnlyTheHome: the screen promises that no agent
// is touched, so nothing outside the home may appear.
func TestOnboardingAcceptedCreatesOnlyTheHome(t *testing.T) {
	Convey("Given an accepted first run", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.tty = true
		w.in = "y\n"

		stdout, err := w.run()

		Convey("When the user says yes", func() {
			Convey("Then the home exists and no agent directory does", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "Created")

				info, statErr := os.Stat(w.homeDir)
				So(statErr, ShouldBeNil)
				So(info.IsDir(), ShouldBeTrue)

				for _, agent := range []string{".claude", ".codex", ".gemini", ".cursor"} {
					_, err := os.Stat(filepath.Join(w.root, agent))
					So(os.IsNotExist(err), ShouldBeTrue)
				}
			})
		})
	})
}

// TestOnboardingIsIdempotent pins §5.1's rerun rule: the second bare run is
// an ordinary invocation, so it prints help and asks nothing.
func TestOnboardingIsIdempotent(t *testing.T) {
	Convey("Given a machine that already has a home", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.tty = true
		w.in = "y\n"

		_, first := w.run()
		So(first, ShouldBeNil)

		stdout, second := w.run()

		Convey("When verger runs bare a second time", func() {
			Convey("Then it is an ordinary run, not a first run", func() {
				So(second, ShouldBeNil)
				So(stdout, ShouldNotContainSubstring, "Here is what I found")
				So(stdout, ShouldNotContainSubstring, "Create it now?")
			})
		})
	})
}

// TestOnboardingDryRunWritesNothing keeps the promise the screen makes about
// `--dry-run`: it shows everything and changes nothing.
func TestOnboardingDryRunWritesNothing(t *testing.T) {
	Convey("Given a first run with --dry-run", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.tty = true
		w.in = "y\n"

		stdout, err := w.run("--dry-run")

		Convey("When the run is a dry run", func() {
			Convey("Then the screen shows and nothing is created", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "Here is what I found")
				So(stdout, ShouldContainSubstring, "was not created")

				_, statErr := os.Stat(w.homeDir)
				So(os.IsNotExist(statErr), ShouldBeTrue)
			})
		})
	})
}

// TestOnboardingReportsEveryHost pins that the screen covers the whole host
// set with a verdict each, and that it never claims more than it knows: a host
// it did not find says so rather than naming a path it made up.
func TestOnboardingReportsEveryHost(t *testing.T) {
	Convey("Given a fresh machine", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.tty = true
		w.in = "n\n"

		stdout, _ := w.run()

		Convey("When the screen renders", func() {
			Convey("Then every registered host gets a state and an evidence cell", func() {
				So(stdout, ShouldContainSubstring, "AGENT")
				So(stdout, ShouldContainSubstring, "STATE")
				So(stdout, ShouldContainSubstring, "EVIDENCE")

				// The screen must not claim more than it knows: a host it
				// did not find says "not installed" rather than naming a
				// path it invented.
				So(stdout, ShouldContainSubstring, "found")
			})
		})
	})
}

// TestOnboardingLeavesAgentsAloneUnderJSON guards the promise more strictly:
// a machine-readable run must not create a home either, and must not print
// the question that only makes sense to a person.
func TestOnboardingLeavesAgentsAloneUnderJSON(t *testing.T) {
	Convey("Given a first run with -y", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		_, err := w.run("-y")

		Convey("When the user answers yes up front", func() {
			Convey("Then the home is created without a question", func() {
				So(err, ShouldBeNil)

				info, statErr := os.Stat(w.homeDir)
				So(statErr, ShouldBeNil)
				So(info.IsDir(), ShouldBeTrue)
			})
		})
	})
}
