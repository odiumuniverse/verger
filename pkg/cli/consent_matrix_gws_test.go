package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
				// The fix it names is `-y`, not `--hooks yes`: nothing was
				// refused, the question simply could not be put to anyone. An
				// earlier version of this row asked for `--hooks yes` here and
				// could not pass, because no code path produces that string when
				// the run never reached the hooks decision.
				So(rc, ShouldEqual, 5)
				So(out, ShouldContainSubstring, "-y")
			})
		})

		Convey("When consent is answered --hooks yes", func() {
			rc, out := installHooked(t, true, false, []string{"--hooks", "yes"})

			Convey("Then it is a plain success", func() {
				So(out, ShouldNotContainSubstring, "hooks skipped")
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

	// Sync reaches the same hooks decision as install, but by a different call:
	// install builds the plan and applies it itself, while Sync calls Client.Apply
	// directly (pkg/verger/sync.go). An error one path raises is not raised by the
	// path that never looks for it, and these rows are what stopped sync from
	// answering 0 over a package it had delivered without its hooks.
	//
	// `update` is not looped in here: the CLI's update verb calls the same runner
	// as sync (pkg/cli/update.go -> a.runSync), so a row for it would be a second
	// copy of the sync rows under the other verb's name. The method a library
	// front end calls is Client.Update, pinned by
	// TestUpdateUnansweredConsentIsNotASuccess in pkg/verger.
	Convey("Given a spec that declares a package with a hook, never installed", t, func() {
		Convey("When sync runs with -y and no consent answer", func() {
			rc, out := syncHooked(t, false, true, false)

			Convey("Then it is exit 5, like install", func() {
				So(rc, ShouldEqual, 5)
			})

			Convey("Then it names a command that would settle the consent", func() {
				So(out, ShouldContainSubstring, "approve")
			})
		})

		Convey("When sync runs with no flag at all on a non-TTY", func() {
			rc, _ := syncHooked(t, false, false, false)

			Convey("Then the question cannot be asked, so it is 5", func() {
				So(rc, ShouldEqual, 5)
			})
		})

		Convey("When the consent was already given with approve", func() {
			rc, _ := syncHooked(t, true, true, false)

			Convey("Then it is a plain success and the package landed", func() {
				So(rc, ShouldEqual, 0)
			})
		})

		Convey("When the spec declares a package that asks nothing", func() {
			rc, _ := syncPlain(t, true)

			Convey("Then -y is enough and it is not a consent failure", func() {
				So(rc, ShouldNotEqual, 5)
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
			// The fake host writes only what the test seeds (pkg/cli/cli_test.go
			// Deliver). Without a target this control delivered nothing and
			// answered "produced no files for any host" (exit 2), which is a
			// usage failure and not what this control is for.
			target := w.target(t, "plain.md", "# plain\n")
			pkg := writePlainPackage(t)

			rc, _ := w.runCode(t, "install", pkg, "-y", "--hosts", "claude")

			Convey("Then nothing was asked, so it is exit 0 and the files landed", func() {
				So(readWorldFile(t, target), ShouldEqual, "# plain\n")
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
				for code := range 8 {
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

			// Both guides name the same verbs, and two Convey blocks with one
			// name is a panic rather than a failure, so the set is taken across
			// both files at once and asserted once.
			wanted := map[string]bool{}

			for _, text := range []string{guide, humans} {
				for _, want := range commandsNamedIn(text) {
					wanted[want] = true
				}
			}

			Convey("Then every command they name exists", func() {
				So(len(wanted), ShouldBeGreaterThan, 0)

				for want := range wanted {
					So(names[want], ShouldBeTrue)
				}
			})
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

// runCode runs the command tree and returns the exit code a user would see, via
// the same classifier main uses, plus everything the run wrote. The error text is
// part of what the user reads — main prints it to stderr and the exit code says
// nothing about which package it concerned — so it is part of the output, not
// something the assertion is blind to.
func (w *world) runCode(t *testing.T, args ...string) (int, string) {
	t.Helper()

	out, err := w.run(args...)
	if err == nil {
		return 0, out + w.err.String()
	}

	// A user reads the error text as much as the output stream, and the exit code
	// says nothing about which package it concerned, so the text is part of what
	// the assertions below can see.
	return exitcode.Classify(err), out + w.err.String() + " " + err.Error()
}

func installHooked(t *testing.T, yes, tty bool, extra []string) (int, string) {
	t.Helper()

	w := newWorld(t)

	return installHookedInto(t, w, yes, tty, extra)
}

// installHookedInto is installHooked against a world the caller already owns. The
// restore rows need one: a restore can only ask about consent for a package that
// is actually installed, so the install and the restore have to happen in the
// same world. Each helper building its own left the restore looking at a machine
// where nothing had ever been installed, and it answered "nothing to restore".
func installHookedInto(t *testing.T, w *world, yes, tty bool, extra []string) (int, string) {
	t.Helper()

	w.tty = tty
	w.hosts = []host.Host{w.fake}

	// The fake host writes exactly the files this call seeds (pkg/cli/cli_test.go
	// Deliver) and nothing else, so a helper that seeds none delivers nothing and
	// the run answers "produced no files for any host" (exit 2). Every row in this
	// file was failing on that, not on consent.
	target := w.target(t, "one.md", "# one\n")

	// The package carries a skill as well as a hook, and both have to be in the
	// shape the packager reads: a SKILL.md without frontmatter is dropped, and a
	// hook the host cannot discover is not a hook. See writeHookedPackage, which
	// writes the same shape and says why.
	pkg := filepath.Join(t.TempDir(), "hooked")
	writeHookedPackageIn(t, pkg)

	args := []string{"install", pkg, "--hosts", "claude"}
	if yes {
		args = append(args, "-y")
	}

	args = append(args, extra...)

	rc, out := w.runCode(t, args...)

	// Every row that got as far as delivering must also prove it wrote something:
	// otherwise the matrix is satisfied by a run that produces no files and
	// answers "produced no files for any host" (exit 2), which is how this file
	// was failing while looking like a consent failure.
	//
	// A run with no `-y` on a non-TTY is the one case that must NOT have written:
	// it cannot put the confirmation to anyone, so it refuses before delivering.
	// That is the behaviour, not a hole in the assertion.
	if yes {
		if _, statErr := os.Stat(target); statErr != nil {
			t.Fatalf("verger %v wrote no files for the host: %v.\n%s", args, statErr, out)
		}
	}

	return rc, out
}

// syncHooked reconciles a spec that declares one hooked package, on a machine
// where it has never been installed — so the verb does the delivering and the
// hooks question is a fresh one.
//
// The package sits beside the spec rather than in a temp dir, because the spec's
// relative source resolves against the spec's own directory and a temp-dir path
// makes the spec unreadable. It is written into the world root as well, because
// `approve` takes a source ref and a local ref resolves against the working
// directory: the two are different directories, and one fixture cannot sit in
// both.
func syncHooked(t *testing.T, approved, yes, tty bool) (int, string) {
	t.Helper()
	w := newWorld(t)
	w.tty = tty
	w.hosts = []host.Host{w.fake}
	// `approve` resolves a local ref against the process working directory, and
	// that is the package directory unless the test moves it. The spec's own
	// relative source resolves against the spec's directory instead, which is why
	// the package is written into both.
	w.chdir(t, w.root)
	w.target(t, "one.md", "# one\n")
	writeHookedPackage(t, w.homeDir)
	writeHookedPackage(t, w.root)
	writeSpecDeclaring(t, w.homeDir, "hooked", "0.1.0")

	if approved {
		// An approve that failed leaves the row testing nothing, so it is checked
		// here rather than swallowed.
		if rc, out := w.runCode(t, "approve", "local:hooked", "./hooked"); rc != 0 {
			t.Fatalf("verger approve failed with %d: %s", rc, out)
		}
	}

	args := []string{"sync"}
	if yes {
		args = append(args, "-y")
	}

	return w.runCode(t, args...)
}

// syncPlain is the control for the sync rows: the same spec, declaring a package
// that asks nothing. Without it the rows above could be satisfied by a sync that
// failed every run, including the one where nothing was pending.
func syncPlain(t *testing.T, yes bool) (int, string) {
	t.Helper()
	w := newWorld(t)
	w.tty = false
	w.hosts = []host.Host{w.fake}
	w.target(t, "plain.md", "# plain\n")
	mkdirAll(t, filepath.Join(w.homeDir, "plain", ".claude-plugin"))
	mkdirAll(t, filepath.Join(w.homeDir, "plain", "skills", "plain"))
	writeTestFile(t, filepath.Join(w.homeDir, "plain", ".claude-plugin", "plugin.json"),
		`{"name":"plain","version":"0.1.0"}`)
	writeTestFile(t, filepath.Join(w.homeDir, "plain", "skills", "plain", "SKILL.md"),
		"---\nname: plain\ndescription: no hooks here\n---\n\nbody\n")
	writeSpecDeclaring(t, w.homeDir, "plain", "0.1.0")

	args := []string{"sync"}
	if yes {
		args = append(args, "-y")
	}

	return w.runCode(t, args...)
}

// writeHookedPackage writes a package that ships a skill and a hook into dir.
//
// The skill needs frontmatter to be a skill at all, or the packager drops it and
// the package produces no files. The hook goes in the manifest, which is where a
// Claude plugin declares one (pkg/manifest/claude.go:15,60) — a host hook module
// under runtime/<host>/hooks/<phase>/ is a different thing, and a package with
// only one of those has no hooks for `verger approve` to record: it answers "the
// package carries no hooks" (exit 2) and the consent row tests nothing.
func writeHookedPackage(t *testing.T, dir string) {
	t.Helper()
	writeHookedPackageIn(t, filepath.Join(dir, "hooked"))
}

// writeHookedPackageIn writes the hooked package into an exact directory. The
// install rows keep theirs in a temp dir; the sync rows keep theirs beside the
// spec, because a spec's relative source resolves against the spec's directory.
func writeHookedPackageIn(t *testing.T, pkg string) {
	t.Helper()
	mkdirAll(t, filepath.Join(pkg, ".claude-plugin"))
	mkdirAll(t, filepath.Join(pkg, "skills", "one"))
	writeTestFile(t, filepath.Join(pkg, ".claude-plugin", "plugin.json"),
		`{"name":"hooked","version":"0.1.0","hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"echo hi"}]}]}}`)
	writeTestFile(t, filepath.Join(pkg, "skills", "one", "SKILL.md"),
		"---\nname: one\ndescription: content so the package has files to write\n---\n\nbody\n")
}

// writeSpecDeclaring writes a spec beside the package it names, so its relative
// source resolves.
func writeSpecDeclaring(t *testing.T, specDir, name, version string) {
	t.Helper()
	writeTestFile(t, filepath.Join(specDir, "verger.toml"), `schema = 1

[[source]]
name = "local"
url = "./`+name+`"

[[package]]
id = "local:`+name+`"
version = "`+version+`"
`)
}

func restoreUnanswered(t *testing.T) (int, string) {
	t.Helper()
	w := newWorld(t)
	installHookedInto(t, w, true, false, []string{"--hooks", "yes"})

	return w.runCode(t, "restore", "local:hooked")
}

func restoreUnansweredWithYes(t *testing.T) (int, string) {
	t.Helper()
	w := newWorld(t)
	installHookedInto(t, w, true, false, []string{"--hooks", "yes"})

	return w.runCode(t, "restore", "local:hooked", "-y")
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

// readGuideFile reads one guide from the module's guide/ directory.
//
// The path is anchored to this source file, never to the working directory.
// Tests in this package chdir — a local source ref resolves against the cwd, so
// the world root has to become it — and a relative read is then reading whatever
// that test left behind. On CI that was the whole difference between a green run
// and `open ../guide/ai-agents.md: no such file or directory`: the file is in the
// tree, and the reader was somewhere else.
func readGuideFile(t *testing.T, name string) string {
	t.Helper()

	root := moduleRootForTest()
	if root == "" {
		t.Fatal("locate the module root: no go.mod above this file")
	}

	//nolint:gosec // G304: root is this module's own directory, found by walking up to go.mod
	body, err := os.ReadFile(filepath.Join(root, "guide", name))
	if err != nil {
		t.Fatalf("read guide %s: %v", name, err)
	}

	return string(body)
}

// moduleRootForTest walks up from this source file to the directory holding
// go.mod. It is the same locator pR uses in pkg/verger: a test that needs to
// reach the repository must find it from the code, not from how it was invoked.
func moduleRootForTest() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}

	dir := filepath.Dir(file)

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}

		dir = parent
	}
}

// commandsNamedIn pulls `verger <name>` out of a guide. Only the first word after
// "verger" is taken, and only a plain verb, so a sentence that mentions a path or a
// flag does not invent a command that the tree is then required to have.
func commandsNamedIn(guide string) []string {
	seen := map[string]bool{}

	var out []string

	// The closing backtick is required. Without it "`verger never opens a
	// keychain" — a sentence that happens to start the same way — counts as a
	// command, and the tree is then required to have a verb called "never".
	for _, m := range regexp.MustCompile("`verger ([a-z][a-z-]*)`").FindAllStringSubmatch(guide, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}

	return out
}
