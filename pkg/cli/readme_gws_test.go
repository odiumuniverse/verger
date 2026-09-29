package cli

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	. "github.com/smartystreets/goconvey/convey"
)

// README.md is the one document a user reads before running anything, and it
// drifts silently: a command is renamed, an argument changes shape, a flag is
// dropped, and the README keeps asserting the old thing. These tests read the
// file and check it against the command tree this binary actually builds, so
// the drift is a test failure instead of a user running a command that does
// not exist.

// readmeCommandPattern matches a command in a README table row. The backtick
// is required: without it the opening line of the README ("verger installs
// plugins, skills, …") reads as a command called `installs`.
var readmeCommandPattern = regexp.MustCompile("(?m)^\\| `verger ([a-z][a-z-]*)")

// readmeFencePattern captures a fenced code block; readmeExamplePattern the
// command lines inside one. A table row and a runnable example are the only
// two places the README tells a user to type something.
var readmeFencePattern = regexp.MustCompile("(?s)```[a-z]*\n(.*?)```")

var readmeExamplePattern = regexp.MustCompile("(?m)^verger ([a-z][a-z-]*)")

const readmePath = "../../README.md"

// TestREADMECommandsExist checks every command the README declares against the
// real tree. It reads the table rows and the fenced example lines, not prose:
// a README that tells a user to run a command that does not exist is wrong,
// and one that merely says the word "home" is not.
func TestREADMECommandsExist(t *testing.T) {
	Convey("Given the commands the README declares", t, func() {
		readme := readREADME(t)
		root := NewRootCmd(Options{})
		// cobra adds `completion` and `help` during Execute, not during
		// construction, so a set built here would be missing two commands a
		// user can actually run — and the README documents `completion`.
		root.InitDefaultCompletionCmd()
		root.InitDefaultHelpCmd()

		known := map[string]bool{}
		for _, cmd := range root.Commands() {
			known[cmd.Name()] = true
		}

		declared := map[string]bool{}
		for _, match := range readmeCommandPattern.FindAllStringSubmatch(readme, -1) {
			declared[match[1]] = true
		}

		for _, block := range readmeFencePattern.FindAllStringSubmatch(readme, -1) {
			for _, line := range readmeExamplePattern.FindAllStringSubmatch(block[1], -1) {
				declared[line[1]] = true
			}
		}

		Convey("Then the README declares at least the common ones", func() {
			So(len(declared), ShouldBeGreaterThan, 20)
		})

		for name := range declared {
			Convey("Then the command "+name+" exists", func() {
				So(known[name], ShouldBeTrue)
			})
		}
	})
}

// TestREADMECoversEveryCommand is the other direction: nothing in the tree may
// be missing from the README. A command a user can run but cannot find is the
// failure mode that matters most here.
func TestREADMECoversEveryCommand(t *testing.T) {
	Convey("Given the command tree", t, func() {
		readme := readREADME(t)
		root := NewRootCmd(Options{})
		// cobra adds these during Execute; without this the test would
		// pass while `completion` — which the README does document — was
		// invisible to it.
		root.InitDefaultCompletionCmd()
		root.InitDefaultHelpCmd()

		// `help` is pure cobra chrome; every other command is documented.
		skip := map[string]bool{"help": true}

		for _, cmd := range root.Commands() {
			name := cmd.Name()
			if skip[name] {
				continue
			}

			Convey("Then the README documents "+name, func() {
				So(strings.Contains(readme, "verger "+name), ShouldBeTrue)
			})
		}
	})
}

// TestREADMEFlagsExist checks every `--flag` the README mentions against the
// union of all flags in the tree. A flag that was removed is the one that bites
// hardest, because the user copies the line and the tool refuses the whole
// invocation.
func TestREADMEFlagsExist(t *testing.T) {
	Convey("Given the README's flags", t, func() {
		readme := readREADME(t)
		root := NewRootCmd(Options{})

		known := map[string]bool{}
		for _, cmd := range root.Commands() {
			collectFlags(cmd, known)
		}

		for _, name := range uniqueMatches(readme, regexp.MustCompile("`--([a-z-]+)")) {
			Convey("Then the flag --"+name+" exists somewhere in the tree", func() {
				So(known[name], ShouldBeTrue)
			})
		}
	})
}

// TestREADMEPairsYesWithForce guards the one promise the README makes that a
// reader could otherwise get wrong: `-y` never authorises an overwrite. If a
// future change folds `--force` into `--yes`, this fails before a user does.
func TestREADMEPairsYesWithForce(t *testing.T) {
	Convey("Given the README's safety claim", t, func() {
		readme := readREADME(t)

		Convey("Then it says -y does not imply --force", func() {
			So(readme, ShouldContainSubstring, "never resolves a destructive conflict")
		})

		Convey("And it says the previous version is kept under --force", func() {
			So(readme, ShouldContainSubstring, "previous version is kept")
		})
	})
}

// TestREADMEPairsYesWithForceCode is the same promise checked in code rather
// than prose: if a future change folds `--force` into `--yes`, the flag table
// changes before the README does.
func TestREADMEPairsYesWithForceCode(t *testing.T) {
	Convey("Given the verbs that can overwrite a file", t, func() {
		for _, name := range []string{"install", "remove", "sync", "update", "adopt"} {
			cmd, _, err := NewRootCmd(Options{}).Find([]string{name})
			So(err, ShouldBeNil)

			Convey("When "+name+" is inspected", func() {
				Convey("Then -y and --force are separate flags", func() {
					So(cmd.Flags().ShorthandLookup("y"), ShouldNotBeNil)
					So(cmd.Flags().Lookup("force"), ShouldNotBeNil)
				})
			})
		}
	})
}

// collectFlags walks a command and everything under it, recording every flag
// name it can accept.
func collectFlags(cmd *cobra.Command, into map[string]bool) {
	cmd.Flags().VisitAll(func(f *pflag.Flag) { into[f.Name] = true })
	cmd.InheritedFlags().VisitAll(func(f *pflag.Flag) { into[f.Name] = true })

	for _, sub := range cmd.Commands() {
		collectFlags(sub, into)
	}
}

// uniqueMatches returns the deduplicated capture group 1 of every match.
func uniqueMatches(text string, re *regexp.Regexp) []string {
	seen := map[string]bool{}
	out := []string{}

	for _, m := range re.FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}

	return out
}

// readREADME loads the file, failing the test rather than skipping when it is
// missing: a test that quietly passes without its subject is worse than no test.
func readREADME(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read %s: %v", readmePath, err)
	}

	return string(data)
}
