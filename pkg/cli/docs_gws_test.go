package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

// TestGeneratedDocsCoverTheCommandTree is the standing version of "do the man
// pages match the binary". It generates from the same `NewRootCmd` the binary
// uses and asserts that every command and every subcommand produced a page —
// so a command added to the tree cannot ship without documentation, and a
// command removed cannot leave a page behind for something that no longer runs.
//
// The generator itself lives in cmd/gendocs; this drives the same cobra entry
// points it uses, so a change to the header or the writer shows up here too.
func TestGeneratedDocsCoverTheCommandTree(t *testing.T) {
	Convey("Given the command tree", t, func() {
		dir := t.TempDir()
		root := docRoot()
		root.DisableAutoGenTag = true

		header := &doc.GenManHeader{
			Title:   "verger",
			Section: "1",
			Source:  "verger test",
		}

		So(doc.GenManTree(root, header, dir), ShouldBeNil)

		entries, err := os.ReadDir(dir)
		So(err, ShouldBeNil)
		So(len(entries), ShouldBeGreaterThan, 0)

		Convey("Then every command and subcommand has a page", func() {
			for _, name := range expectedPageNames() {
				// cobra's help command carries no page: GenManTree skips a
				// command with no runnable content of its own.
				if name == "help" {
					continue
				}

				Convey("Then verger-"+name+".1 exists", func() {
					_, statErr := os.Stat(filepath.Join(dir, "verger-"+name+".1"))
					So(statErr, ShouldBeNil)
				})
			}
		})

		Convey("Then the watch service is documented", func() {
			// The two surfaces added late are the ones most likely to be
			// missing, because a hand-written list would not include them
			// and a filtered generator would skip them silently.
			for _, name := range []string{"watch", "service", "service-install", "service-status", "service-uninstall"} {
				_, statErr := os.Stat(filepath.Join(dir, "verger-"+name+".1"))
				So(statErr, ShouldBeNil)
			}
		})

		Convey("Then no page exists for a command the tree does not have", func() {
			for _, entry := range entries {
				name := strings.TrimSuffix(entry.Name(), ".1")
				name = strings.TrimPrefix(name, "verger-")

				_, _, findErr := root.Find([]string{name})
				So(findErr, ShouldBeNil)
			}
		})
	})
}

// TestShellCompletionIsDynamic is the completions half of the same question.
// A completion script that hard-codes a command list goes stale the moment a
// verb is added; cobra's generated scripts instead call the binary's hidden
// `__complete` at completion time, so they cannot. This asserts the mechanism
// rather than the contents.
func TestShellCompletionIsDynamic(t *testing.T) {
	Convey("Given the generated completion scripts", t, func() {
		// cobra's own generators, the same ones `verger completion <shell>`
		// calls. They are driven directly so the script lands in a buffer
		// instead of on the test's own stdout.
		generators := map[string]func(*cobra.Command, io.Writer) error{
			"bash":       func(c *cobra.Command, w io.Writer) error { return c.GenBashCompletionV2(w, true) },
			"zsh":        func(c *cobra.Command, w io.Writer) error { return c.GenZshCompletion(w) },
			"fish":       func(c *cobra.Command, w io.Writer) error { return c.GenFishCompletion(w, true) },
			"powershell": func(c *cobra.Command, w io.Writer) error { return c.GenPowerShellCompletion(w) },
		}

		for shell, generate := range generators {
			Convey("When the "+shell+" script is generated", func() {
				var out strings.Builder

				So(generate(docRoot(), &out), ShouldBeNil)

				Convey("Then it asks the binary rather than listing commands", func() {
					// A hard-coded command list is the failure this guards:
					// it would keep offering a verb that no longer exists and
					// would never offer one that was just added.
					So(out.String(), ShouldContainSubstring, "__complete")
				})
			})
		}
	})
}

// TestCompleteReportsEveryCommand checks the same property from the other
// side: the binary itself must answer with the full tree, including the late
// additions, or a correct-looking script still completes nothing.
func TestCompleteReportsEveryCommand(t *testing.T) {
	Convey("Given the hidden completion command", t, func() {
		root := docRoot()

		out := runComplete(root, "")

		for _, name := range []string{"watch", "service", "secret", "install", "sync"} {
			Convey("Then "+name+" is offered", func() {
				So(strings.Contains(out, name), ShouldBeTrue)
			})
		}
	})
}

// TestCompleteOffersServiceSubcommands walks one level down, which is where a
// subcommand added without its parent being re-listed would hide.
func TestCompleteOffersServiceSubcommands(t *testing.T) {
	Convey("Given completion of `verger service`", t, func() {
		out := runComplete(docRoot(), "service", "")

		for _, name := range []string{"install", "uninstall", "status"} {
			Convey("Then "+name+" is offered", func() {
				So(strings.Contains(out, name), ShouldBeTrue)
			})
		}
	})
}

// docRoot builds the command tree the way the binary and cmd/gendocs do,
// including cobra's own commands, which are added during Execute.
func docRoot() *cobra.Command {
	root := NewRootCmd(Options{})
	root.InitDefaultCompletionCmd()
	root.InitDefaultHelpCmd()

	return root
}

// expectedPageNames is every command and subcommand a user can type, as the
// flat page name gendocs derives from the path.
func expectedPageNames() []string {
	names := []string{}

	for _, cmd := range docRoot().Commands() {
		names = append(names, cmd.Name())

		for _, sub := range cmd.Commands() {
			names = append(names, cmd.Name()+"-"+sub.Name())
		}
	}

	return names
}

// runComplete drives the hidden `__complete` command and returns its output.
func runComplete(root *cobra.Command, args ...string) string {
	var out strings.Builder

	root.SetOut(&out)
	root.SetErr(&out)

	// `__complete` takes the current command line, with the last word being
	// the one being completed — so a top-level query is `__complete ""` and a
	// subcommand query is `__complete service ""`. The caller supplies that
	// trailing empty word; adding another here would complete a nonexistent
	// second word and answer with nothing.
	full := make([]string, 0, len(args)+1)
	full = append(full, "__complete")
	full = append(full, args...)
	root.SetArgs(full)

	// A completion error is not a failure here: cobra writes the answer it
	// has to stdout either way, and that is what this test reads.
	_ = root.Execute()

	return out.String()
}
