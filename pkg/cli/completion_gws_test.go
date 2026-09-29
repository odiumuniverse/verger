package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

// These tests drive cobra's own `__complete` command, which is exactly what a
// shell invokes. Going through it means the assertions cover the wiring, not
// just a helper that happens to return the right list.

// shellComplete asks the command tree for candidates, the way a shell does,
// and returns them with the directive line removed.
func shellComplete(t *testing.T, w *world, args ...string) []string {
	t.Helper()

	root := NewRootCmd(w.options())
	root.SetOut(&w.out)
	root.SetErr(&w.err)
	root.SetArgs(append([]string{"__complete"}, args...))

	if err := root.Execute(); err != nil {
		t.Fatalf("__complete %s: %v\nstderr: %s", strings.Join(args, " "), err, w.err.String())
	}

	var out []string

	for line := range strings.SplitSeq(w.out.String(), "\n") {
		line = strings.TrimSpace(line)
		// The last line is the directive ("ShellCompDirectiveNoFileComp" or
		// ":4"); everything before it is a candidate.
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "ShellCompDirective") {
			continue
		}

		out = append(out, line)
	}

	return out
}

// TestHostFlagCompletionOffersEveryHost pins §4: `--hosts` and `--except`
// complete to the canonical host list, and the list does not depend on which
// hosts happen to be installed — a user naming a host they have not
// installed yet must still get the name.
func TestHostFlagCompletionOffersEveryHost(t *testing.T) {
	Convey("Given a command carrying --hosts and --except", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		Convey("When the shell completes --hosts", func() {
			Convey("Then every canonical host is offered", func() {
				values := shellComplete(t, w, "install", "--hosts", "")

				So(values, ShouldContain, "claude")
				So(values, ShouldContain, "opencode")
				So(values, ShouldContain, "cursor")
			})
		})

		Convey("When the shell completes --except", func() {
			Convey("Then the same hosts are offered", func() {
				values := shellComplete(t, w, "install", "--except", "")

				So(values, ShouldContain, "claude")
			})
		})
	})
}

// TestFlagCompletionFiltersByPrefix keeps a long host list usable: typing
// "op" narrows instead of dumping everything.
func TestFlagCompletionFiltersByPrefix(t *testing.T) {
	Convey("Given a partially typed host", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		Convey("When the shell completes the prefix", func() {
			Convey("Then only the matching hosts come back", func() {
				values := shellComplete(t, w, "install", "--hosts", "op")

				So(values, ShouldContain, "opencode")
				So(values, ShouldNotContain, "claude")
			})
		})
	})
}

// TestPackageCompletionComesFromTheLocalSpec pins the local rule: the ids
// the current spec declares, and nothing fetched.
func TestPackageCompletionComesFromTheLocalSpec(t *testing.T) {
	Convey("Given a machine with a package installed", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		// The fake host needs a target, or the delivery has nothing to
		// write: every cell is then `skipped` and `install` correctly exits
		// 2, "produced no files for any host". A package that is on the spec
		// is not a package that is installed, so the test has to make the
		// install real before it can ask what completion offers.
		w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")

		Convey("When the shell completes a package argument", func() {
			Convey("Then the local id is offered", func() {
				values := shellComplete(t, w, "remove", "")

				So(values, ShouldContain, "local:caveman")
			})
		})
	})
}

// TestPackageCompletionIsEmptyOnAFreshMachine is the "fast on an empty home"
// requirement: nothing declared completes to nothing, quickly, and does not
// fail.
func TestPackageCompletionIsEmptyOnAFreshMachine(t *testing.T) {
	Convey("Given a machine with no spec at all", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		Convey("When the shell completes a package argument", func() {
			Convey("Then nothing is offered and it returns at once", func() {
				start := time.Now()
				values := shellComplete(t, w, "remove", "")
				elapsed := time.Since(start)

				So(values, ShouldBeEmpty)
				So(elapsed, ShouldBeLessThan, 2*time.Second)
			})
		})
	})
}

// TestKindCompletionOffersTheModelsKinds pins the kind list on the command
// that filters by kind.
func TestKindCompletionOffersTheModelsKinds(t *testing.T) {
	Convey("Given propagate set, which takes --kind", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		Convey("When the shell completes --kind", func() {
			Convey("Then the model's kinds are offered", func() {
				values := shellComplete(t, w, "propagate", "set", "--kind", "")

				So(values, ShouldContain, "skill")
				So(values, ShouldContain, "mcp")
				So(values, ShouldContain, "hook")
			})
		})
	})
}

// TestWhyCompletesHostAsSecondArgument covers the two-argument command: the
// first argument is a package, the second is a host, and the two must not be
// mixed up.
func TestWhyCompletesHostAsSecondArgument(t *testing.T) {
	Convey("Given why <id> <host>", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		Convey("When the shell has already typed the package", func() {
			Convey("Then the second argument completes hosts", func() {
				values := shellComplete(t, w, "why", "local:caveman", "")

				So(values, ShouldContain, "claude")
			})
		})
	})
}

// TestCompletionNeverWritesToDisk guards the no-side-effects property: a
// shell asking for candidates must not create the home it is asking about.
func TestCompletionNeverWritesToDisk(t *testing.T) {
	Convey("Given a machine with no home", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		Convey("When completions are asked for", func() {
			Convey("Then nothing is created on disk", func() {
				shellComplete(t, w, "install", "")
				shellComplete(t, w, "install", "--hosts", "")

				_, err := os.Stat(w.homeDir)
				So(os.IsNotExist(err), ShouldBeTrue)
			})
		})
	})
}

// TestFilterPrefixCoversTheEmptyPrefix documents the small rule the shell
// depends on: an empty prefix means every value.
func TestFilterPrefixCoversTheEmptyPrefix(t *testing.T) {
	Convey("Given a list of values", t, func() {
		Convey("When nothing has been typed", func() {
			Convey("Then every value is a candidate", func() {
				So(filterPrefix([]string{"a", "b"}, ""), ShouldHaveLength, 2)
			})
		})

		Convey("When a prefix has been typed", func() {
			Convey("Then only the matching values are candidates", func() {
				So(filterPrefix([]string{"ab", "ac", "b"}, "a"), ShouldHaveLength, 2)
				So(filterPrefix([]string{"ab", "b"}, "zzz"), ShouldBeEmpty)
			})
		})
	})
}

// TestHostIDsAreTheCanonicalSet guards the completion list against drifting
// away from the registry the rest of the CLI uses.
func TestHostIDsAreTheCanonicalSet(t *testing.T) {
	Convey("Given the host completion list", t, func() {
		Convey("When it is read", func() {
			Convey("Then it carries the known hosts, without duplicates", func() {
				values := hostIDs()

				So(len(values), ShouldBeGreaterThan, 5)
				So(values, ShouldContain, "claude")

				seen := map[string]bool{}

				for _, value := range values {
					So(seen[value], ShouldBeFalse)
					seen[value] = true
				}
			})
		})
	})
}
