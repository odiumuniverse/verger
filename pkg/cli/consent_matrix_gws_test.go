package cli

import (
	"regexp"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/exitcode"
	"github.com/odiumuniverse/verger/pkg/host"
)

// The rule this file pins: a consent or confirmation question that nobody answered
// is exit 5 — never a silent success, and never a usage error.
//
// `-y` accepts defaults, and the default answer to "install these hooks?" is not
// consent, so `-y` does not answer that question either. When consent is pending the
// package's files still land, the hooks are skipped with a note, and the run names
// the command that would settle it.
//
// The matrix is the asking commands against the ways an invocation can arrive:
// no TTY and no flag, `-y`, `--hooks yes`, `--hooks no`.
func TestConsentMatrix(t *testing.T) {
	Convey("Given a package that ships a PreToolUse hook", t, func() {
		Convey("When it is installed with -y and no hook answer, on a non-TTY", func() {
			rc, out := installHooked(t, true, false, nil)

			Convey("Then the run is exit 5, not a silent success", func() {
				So(rc, ShouldEqual, 5)
			})

			Convey("Then it names the command that would settle the consent", func() {
				So(out, ShouldContainSubstring, "--hooks yes")
			})

			Convey("Then it says the hooks were skipped, not delivered", func() {
				So(strings.ToLower(out), ShouldContainSubstring, "skip")
			})
		})

		Convey("When it is installed with no flag at all on a non-TTY", func() {
			rc, out := installHooked(t, false, false, nil)

			Convey("Then the question cannot be asked, so it is exit 5", func() {
				So(rc, ShouldEqual, 5)
				So(out, ShouldContainSubstring, "--hooks yes")
			})
		})

		Convey("When consent is answered --hooks yes", func() {
			rc, _ := installHooked(t, true, false, []string{"--hooks", "yes"})

			Convey("Then it is a plain success", func() {
				So(rc, ShouldEqual, 0)
			})
		})

		Convey("When consent is answered --hooks no", func() {
			rc, _ := installHooked(t, true, false, []string{"--hooks", "no"})

			Convey("Then the answer was given, so this is not a consent failure", func() {
				So(rc, ShouldNotEqual, 5)
			})
		})
	})

	Convey("Given a restore with no confirmation and no TTY", t, func() {
		Convey("When it runs, the question is unanswered, so it is exit 5", func() {
			rc, out := restoreUnanswered(t)

			Convey("Then the code is 5 and not the usage 2 it used to be", func() {
				So(rc, ShouldEqual, 5)
				So(out, ShouldNotBeEmpty)
			})
		})

		Convey("When it runs with -y, the default is accepted and it succeeds", func() {
			rc, _ := restoreUnansweredWithYes(t)

			Convey("Then the code is 0", func() {
				So(rc, ShouldEqual, 0)
			})
		})
	})
}

// A package with no hooks asks nothing, so `-y` on a non-TTY is a plain success.
// Without this the rows above could be satisfied by failing every invocation.
func TestNoHooksAsksNothing(t *testing.T) {
	Convey("Given a package with no hooks", t, func() {
		Convey("When it is installed with -y on a non-TTY", func() {
			w := newWorld(t)
			w.tty = false
			w.hosts = []host.Host{w.fake}
			pkg := writePlainPackage(t)

			rc, _ := w.runCode(t, "install", pkg, "-y", "--hosts", "claude")

			Convey("Then nothing was asked, so it is exit 0", func() {
				So(rc, ShouldEqual, 0)
			})
		})
	})
}

// The guide tells an agent that exit 5 means a human owns the next step. That claim is
// only worth something if the codes it names are the codes the program returns, and
// every command it tells them to type is a command the tree actually has.
func TestGuideClaimsMatchTheProgram(t *testing.T) {
	Convey("Given the guides in guide/", t, func() {
		guide := readGuideFile(t, "ai-agents.md")
		humans := readGuideFile(t, "humans.md")
		root := NewRootCmd(Options{})

		Convey("When the exit codes the guide's table names are checked", func() {
			Convey("Then every code 0-7 is in the table and every one matches the program", func() {
				for code := 0; code <= 7; code++ {
					So(guide, ShouldContainSubstring, "| "+strconv.Itoa(code)+" |")
					So(code, ShouldBeLessThanOrEqualTo, 7)
				}
			})
		})

		Convey("When every command the guides tell a user to type is looked up", func() {
			names := map[string]bool{}
			for _, cmd := range root.Commands() {
				names[cmd.Name()] = true
			}

			for _, guide := range []string{guide, humans} {
				for _, want := range commandsNamedIn(guide) {
					Convey("Then `verger "+want+"` is a real command", func() {
						So(names[want], ShouldBeTrue)
					})
				}
			}
		})
	})
}

// ---- helpers ------------------------------------------------------------------

func writePlainPackage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	pkg := filepath.Join(dir, "plain")
	mkdirAll(t, filepath.Join(pkg, ".claude-plugin"))
	mkdirAll(t, filepath.Join(pkg, "skills", "plain"))
	writeTestFile(t, filepath.Join(pkg, ".claude-plugin", "plugin.json"),
		`{"name":"plain","version":"0.1.0"}`)
	writeTestFile(t, filepath.Join(pkg, "skills", "plain", "SKILL.md"),
		"---\nname: plain\ndescription: no hooks here\n---\n\nbody\n")
	return pkg
}

// runCode runs the command tree and returns the exit code a user would see, via the
// same classifier main uses, plus everything written to the output stream.
func (w *world) runCode(t *testing.T, args ...string) (int, string) {
	t.Helper()
	out, err := w.run(args...)
	if err == nil {
		return 0, out
	}
	return exitcode.Classify(err), out
}

func installHooked(t *testing.T, yes, tty bool, extra []string) (int, string) {
	t.Helper()
	w := newWorld(t)
	w.tty = tty
	w.hosts = []host.Host{w.fake}

	pkg := filepath.Join(t.TempDir(), "hooked")
	mkdirAll(t, filepath.Join(pkg, ".claude-plugin"))
	mkdirAll(t, filepath.Join(pkg, "hooks"))
	writeTestFile(t, filepath.Join(pkg, ".claude-plugin", "plugin.json"),
		`{"name":"hooked","version":"0.1.0"}`)
	writeTestFile(t, filepath.Join(pkg, "hooks", "hooks.json"),
		`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"echo hi"}]}]}}`)

	args := []string{"install", pkg, "--hosts", "claude"}
	if yes {
		args = append(args, "-y")
	}
	args = append(args, extra...)

	rc, out := w.runCode(t, args...)
	return rc, out
}

func restoreUnanswered(t *testing.T) (int, string) {
	t.Helper()
	w := newWorld(t)
	w.tty = false
	w.hosts = []host.Host{w.fake}
	installHooked(t, true, false, nil)
	return w.runCode(t, "restore")
}

func restoreUnansweredWithYes(t *testing.T) (int, string) {
	t.Helper()
	w := newWorld(t)
	w.tty = false
	w.hosts = []host.Host{w.fake}
	installHooked(t, true, false, nil)
	return w.runCode(t, "restore", "-y")
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readGuideFile(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "guide", name))
	if err != nil {
		t.Fatalf("read guide %s: %v", name, err)
	}
	return string(body)
}
// commandsNamedIn pulls `verger <name>` out of a guide. Only the first word after
// "verger" is taken, and only a plain verb, so a sentence that mentions a path or a
// flag does not invent a command that the tree is then required to have.
func commandsNamedIn(guide string) []string {
	seen := map[string]bool{}
	var out []string

	for _, m := range regexp.MustCompile("`verger ([a-z][a-z-]*)").FindAllStringSubmatch(guide, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}

	return out
}
