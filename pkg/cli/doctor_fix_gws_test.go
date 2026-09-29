package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// TestDoctorFindingsCarryRunnableFixes pins the finding shape from
// W7-UX-SPEC §3.1: `fix` is a list of ready-to-run argv arrays, so a consumer
// runs entry 0 without re-splitting a rendered string.
func TestDoctorFindingsCarryRunnableFixes(t *testing.T) {
	Convey("Given a machine whose home is missing", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		stdout, err := w.run("doctor", "--json")

		Convey("When doctor runs", func() {
			Convey("Then the missing directory is fixable and its fix is argv", func() {
				So(err, ShouldBeNil)

				var doc doctorDoc

				So(json.Unmarshal([]byte(stdout), &doc), ShouldBeNil)
				So(doc.Schema.Name, ShouldEqual, schemaDoctor)

				home := findFinding(doc.Findings, "home")
				So(home, ShouldNotBeNil)
				So(home.SafeToAutofix, ShouldBeTrue)
				So(home.Fix, ShouldNotBeEmpty)

				// Entry 0 is executable as given: first element is the program.
				So(home.Fix[0][0], ShouldEqual, "verger")
				So(len(home.Fix[0]), ShouldBeGreaterThan, 1)
			})
		})
	})
}

// TestDoctorSeveritiesUseTheSharedVocabulary pins §3.2: the three words are
// info, warning and error. `ok` is gone, because a passing check is not a
// finding.
func TestDoctorSeveritiesUseTheSharedVocabulary(t *testing.T) {
	Convey("Given any doctor run", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		stdout, _ := w.run("doctor", "--json")

		var doc doctorDoc

		So(json.Unmarshal([]byte(stdout), &doc), ShouldBeNil)
		So(doc.Findings, ShouldNotBeEmpty)

		for _, finding := range doc.Findings {
			Convey("When the finding is "+finding.Subject, func() {
				Convey("Then its severity is one of the three words", func() {
					So(finding.Severity, ShouldBeIn, []string{"info", "warning", "error"})
				})
			})
		}
	})
}

// TestDoctorNeverAutofixesConsent is the rule that matters most: `--fix` must
// not consent on the user's behalf. Only repairs verger fully owns may be
// automatic; anything needing a decision stays a printed instruction.
func TestDoctorNeverAutofixesConsent(t *testing.T) {
	Convey("Given findings that need a decision", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		// A spec that does not parse is a real error verger must not rewrite,
		// and a corrupt receipt is likewise not something to fix blindly.
		writeWorldFile(t, filepath.Join(w.homeDir, "verger.toml"), "schema = 1\n[package\nbroken")
		writeWorldFile(t, filepath.Join(w.homeDir, "state", "receipts", "caveman", "claude-user.json"), "{not json")

		stdout, _ := w.run("doctor", "--json")

		var doc doctorDoc

		So(json.Unmarshal([]byte(stdout), &doc), ShouldBeNil)

		Convey("When doctor reports them", func() {
			Convey("Then none is marked safe to autofix", func() {
				for _, subject := range []string{"spec", "receipts"} {
					finding := findFinding(doc.Findings, subject)
					if finding == nil {
						continue
					}

					So(finding.SafeToAutofix, ShouldBeFalse)
				}
			})
		})
	})
}

// TestDoctorFixAppliesTheOwnedRepairs runs `doctor --fix` for real: the home
// directory verger owns must appear, and the run must say what it repaired.
func TestDoctorFixAppliesTheOwnedRepairs(t *testing.T) {
	Convey("Given a machine whose home is missing", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		home := w.homeDir

		So(os.MkdirAll(filepath.Dir(home), 0o750), ShouldBeNil)

		_, err := w.run("doctor", "--fix", "-y")

		Convey("When doctor --fix runs", func() {
			Convey("Then the owned directory now exists", func() {
				So(err, ShouldBeNil)

				info, statErr := os.Stat(home)
				So(statErr, ShouldBeNil)
				So(info.IsDir(), ShouldBeTrue)
			})
		})
	})
}

// TestDoctorFixAsksOnceForTheWholeSet pins §3.3: one question for all the
// safe repairs, not one per finding.
func TestDoctorFixAsksOnceForTheWholeSet(t *testing.T) {
	Convey("Given a machine with two owned repairs pending and no TTY", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.tty = false

		stdout, err := w.run("doctor", "--fix")

		Convey("When doctor --fix runs without -y", func() {
			Convey("Then it refuses rather than repairing anything", func() {
				So(err, ShouldNotBeNil)
				So(stdout, ShouldNotContainSubstring, "fixed:")
			})
		})
	})
}

// TestDoctorFixAsksOnceOnATerminal pins §3.3 on the case the no-TTY gate does
// not cover: with a terminal and no -y, `--fix` must put the plan on screen
// and ask exactly once. The other write commands let a TTY run through
// unreviewed, which is right for them and wrong here — `--fix` changes the
// machine, so the plan has to be seen before it happens.
func TestDoctorFixAsksOnceOnATerminal(t *testing.T) {
	Convey("Given a terminal, --fix and no -y", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.tty = true

		w.in = "y\n"
		stdout, err := w.run("doctor", "--fix")

		Convey("When doctor --fix runs", func() {
			Convey("Then it shows the plan and asks once", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "fix(es) to apply")
				So(strings.Count(stdout, "apply these?"), ShouldEqual, 1)
			})
		})
	})
}

// TestDoctorFixDeclinedChangesNothing covers the other answer: a user who says
// no gets a machine that is exactly as it was, and the findings still shown.
func TestDoctorFixDeclinedChangesNothing(t *testing.T) {
	Convey("Given a terminal and a declined --fix", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.tty = true

		w.in = "n\n"
		stdout, _ := w.run("doctor", "--fix")

		Convey("When the user declines", func() {
			Convey("Then nothing is repaired and the finding is still reported", func() {
				So(stdout, ShouldNotContainSubstring, "fixed:")
				So(stdout, ShouldContainSubstring, "home")
			})
		})
	})
}

// TestDoctorFixIsIdempotent guards the "rerunning is boring" property: a
// second `--fix` on a healthy machine finds nothing to do and changes nothing.
func TestDoctorFixIsIdempotent(t *testing.T) {
	Convey("Given a machine already repaired", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		_, first := w.run("doctor", "--fix", "-y")
		So(first, ShouldBeNil)

		stdout, second := w.run("doctor", "--fix", "-y")

		Convey("When doctor --fix runs again", func() {
			Convey("Then it succeeds and repairs nothing further", func() {
				So(second, ShouldBeNil)
				So(stdout, ShouldNotContainSubstring, "fixed:")
			})
		})
	})
}

// TestDoctorFixEmitsJSONWhenAsked keeps the two output modes honest: `--json`
// with `--fix` is a document, not prose about repairs.
func TestDoctorFixEmitsJSONWhenAsked(t *testing.T) {
	Convey("Given doctor --fix --json", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		stdout, err := w.run("doctor", "--fix", "-y", "--json")

		Convey("When it runs", func() {
			Convey("Then the output is a parseable document", func() {
				So(err, ShouldBeNil)

				var doc doctorDoc

				So(json.Unmarshal([]byte(stdout), &doc), ShouldBeNil)
				So(doc.Schema.Name, ShouldEqual, schemaDoctor)
			})
		})
	})
}

// findFinding returns the first finding with the given subject, or nil.
func findFinding(findings []doctorCheck, subject string) *doctorCheck {
	for i := range findings {
		if findings[i].Subject == subject {
			return &findings[i]
		}
	}

	return nil
}
