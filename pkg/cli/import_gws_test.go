package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/exitcode"
	"github.com/odiumuniverse/verger/pkg/host"
)

// importEnvelope is what a consumer of `import --json` decodes into. The struct
// is spelled out here rather than reused from the command, so a field renamed
// or dropped on the producing side fails this test instead of silently
// agreeing with itself.
type importEnvelope struct {
	Schema struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
	} `json:"schema"`
	Wrote      int  `json:"wrote"`
	DryRun     bool `json:"dry_run"`
	Added      int  `json:"added"`
	Already    int  `json:"already_in_spec"`
	Candidates []struct {
		ID      string   `json:"id"`
		Version string   `json:"version"`
		Host    string   `json:"host"`
		Notes   []string `json:"notes"`
	} `json:"candidates"`
	Skipped []struct {
		Host   string `json:"host"`
		Reason string `json:"reason"`
	} `json:"skipped"`
	Spec string `json:"spec"`
}

// importWorld is one world whose single fake host already has a package
// installed natively - the situation `import` exists for.
func importWorld(t *testing.T) *world {
	t.Helper()

	w := newWorld(t)
	w.chdir(t, w.root)
	w.warmHome(t)
	w.fake.listed = []host.Installed{{Name: "caveman", Version: "1.2.3", Enabled: true}}

	return w
}

// specBody reads the scope's spec, or "" when there is none: `import` writes no
// file at all when it has nothing to record, and a test that cannot read a
// missing file would be testing the wrong thing.
func specBody(t *testing.T, w *world) string {
	t.Helper()

	body, err := os.ReadFile(w.specPath())
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}

	So(err, ShouldBeNil)

	return string(body)
}

func TestImportJSON(t *testing.T) {
	Convey("Given a host with a package installed natively", t, func() {
		w := importWorld(t)

		stdout, err := w.run("import", "--json", "-y")

		Convey("When import runs", func() {
			Convey("Then the document carries the envelope, the candidate and the count", func() {
				So(err, ShouldBeNil)

				var doc importEnvelope
				So(json.Unmarshal([]byte(stdout), &doc), ShouldBeNil)

				So(doc.Schema.Name, ShouldEqual, "verger.import")
				So(doc.Schema.Version, ShouldEqual, 1)

				So(doc.Added, ShouldEqual, 1)
				So(doc.Wrote, ShouldEqual, 1)
				So(doc.DryRun, ShouldBeFalse)

				So(len(doc.Candidates), ShouldEqual, 1)
				So(doc.Candidates[0].ID, ShouldEqual, "caveman")
				So(doc.Candidates[0].Version, ShouldEqual, "1.2.3")
				So(doc.Candidates[0].Host, ShouldEqual, "claude")

				So(doc.Spec, ShouldEqual, w.specPath())
			})
		})
	})
}

// The one question that matters about this verb: does it deliver? It must not.
// A user who runs `import` expecting their agents to be brought in line with a
// spec, and gets a config record instead, has to find out before they trust it.
func TestImportWritesOnlyTheSpec(t *testing.T) {
	Convey("Given a host with a package installed natively", t, func() {
		w := importWorld(t)

		_, err := w.run("import", "-y")

		Convey("When import runs", func() {
			Convey("Then the spec gains the entry and no host file is written", func() {
				So(err, ShouldBeNil)

				So(specBody(t, w), ShouldContainSubstring, `id = "caveman"`)

				// The delivery is a different verb: `verger sync`. If import
				// ever started writing hosts, this counter would move.
				w.fake.mu.Lock()
				delivers := w.fake.delivers
				w.fake.mu.Unlock()
				So(delivers, ShouldEqual, 0)
			})
		})
	})
}

// The second law. `import` adds lines; every byte the user wrote stays.
func TestImportPreservesAHandWrittenSpec(t *testing.T) {
	Convey("Given a spec written by hand, with comments", t, func() {
		w := importWorld(t)

		original := "# the canon, kept in step with beadle's\n" +
			"schema = 1\n" +
			"\n" +
			"# the review agent, per the incident of 2026-06\n" +
			"[[package]]\n" +
			"id = \"gws-review\"\n" +
			"channel = \"stable\" # earns its keep\n"
		So(w.writeSpec(t, original), ShouldBeNil)

		_, err := w.run("import", "-y")

		Convey("When import runs", func() {
			Convey("Then the comments survive and only the new block appears", func() {
				So(err, ShouldBeNil)

				after := specBody(t, w)
				So(after, ShouldContainSubstring, "# the canon, kept in step with beadle's")
				So(after, ShouldContainSubstring, "# the review agent, per the incident of 2026-06")
				So(after, ShouldContainSubstring, `channel = "stable" # earns its keep`)
				So(after, ShouldContainSubstring, `id = "caveman"`)

				// Every line the user wrote is still there, in order: the file
				// grew by exactly one block and lost nothing.
				So(strings.HasPrefix(after, original), ShouldBeTrue)
			})
		})
	})
}

// Idempotence, at the level the user experiences it: run it twice, the second
// run says there is nothing to do and does not touch the file.
func TestImportSecondRunIsANoOp(t *testing.T) {
	Convey("Given a spec that already declares the installed package", t, func() {
		w := importWorld(t)
		So(w.writeSpec(t, "schema = 1\n\n[[package]]\nid = \"caveman\"\nversion = \"1.2.3\"\n"), ShouldBeNil)

		before := specBody(t, w)

		stdout, err := w.run("import", "-y")

		Convey("When import runs", func() {
			Convey("Then it says there is nothing to add and the bytes do not move", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "nothing to import")
				So(specBody(t, w), ShouldEqual, before)
			})
		})
	})
}

// The preview. `--dry-run` prints the diff the real writer would produce and
// writes nothing - and the diff is produced by that writer, on a throwaway copy,
// so it cannot drift from what the save does.
func TestImportDryRunPrintsTheDiffAndWritesNothing(t *testing.T) {
	Convey("Given a host with a package installed natively", t, func() {
		w := importWorld(t)
		So(w.writeSpec(t, "schema = 1\n\n# a note the user wrote\n"), ShouldBeNil)

		before := specBody(t, w)

		stdout, err := w.run("import", "--dry-run")

		Convey("When import runs with --dry-run", func() {
			Convey("Then the added lines are shown and the spec is untouched", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "+[[package]]")
				So(stdout, ShouldContainSubstring, `+id = "caveman"`)
				So(specBody(t, w), ShouldEqual, before)
			})
		})
	})
}

// The confirmation. One question for the whole set, and no TTY without -y means
// the file is not written after the plan was printed.
func TestImportWithoutYesStopsAfterThePlan(t *testing.T) {
	Convey("Given a host with a package installed natively", t, func() {
		w := importWorld(t)
		before := specBody(t, w)

		_, err := w.run("import")

		Convey("When import runs without -y and without a terminal", func() {
			Convey("Then it stops with the typed pending question and writes nothing", func() {
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "-y")
				So(specBody(t, w), ShouldEqual, before)
			})
		})
	})

	Convey("Given nothing new to import", t, func() {
		w := importWorld(t)
		So(w.writeSpec(t, "schema = 1\n\n[[package]]\nid = \"caveman\"\n"), ShouldBeNil)

		stdout, err := w.run("import", "-y")

		Convey("When import runs", func() {
			Convey("Then it says so once, not twice", func() {
				// Two sentences saying the same thing is how a user stops
				// reading a tool's output.
				So(err, ShouldBeNil)
				So(strings.Count(stdout, "nothing to"), ShouldEqual, 1)
			})
		})
	})

	Convey("Given a terminal and a user who answers no", t, func() {
		w := importWorld(t)
		w.tty = true
		w.in = "n\n"
		before := specBody(t, w)

		stdout, err := w.run("import")

		Convey("When import runs and is declined", func() {
			Convey("Then the plan was shown, nothing was written, and it says so", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "caveman")
				So(stdout, ShouldContainSubstring, "nothing was recorded")
				So(specBody(t, w), ShouldEqual, before)
			})
		})
	})

	Convey("Given a terminal and a user who answers yes", t, func() {
		w := importWorld(t)
		w.tty = true
		w.in = "y\n"

		_, err := w.run("import")

		Convey("When import runs and is accepted", func() {
			Convey("Then the entry is recorded after the single question", func() {
				So(err, ShouldBeNil)
				So(specBody(t, w), ShouldContainSubstring, `id = "caveman"`)
			})
		})
	})
}

// The partial answer. One host that cannot list must not cost the user the
// hosts that can, and the failure has to be on screen.
func TestImportNamesAHostItCouldNotAsk(t *testing.T) {
	Convey("Given a host whose listing fails", t, func() {
		w := importWorld(t)
		w.fake.oracleErr = errors.New("no such command: plugin")

		stdout, err := w.run("import", "-y")

		Convey("When import runs", func() {
			Convey("Then the host is named with the reason and nothing is claimed", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "skipped claude")
				So(stdout, ShouldContainSubstring, "no such command")

				// An import that could not ask anyone must not write a spec
				// that looks like the machine's whole inventory.
				So(specBody(t, w), ShouldNotContainSubstring, `id = "caveman"`)
			})
		})
	})
}

func TestImportRejectsArguments(t *testing.T) {
	Convey("Given an argument import does not take", t, func() {
		w := importWorld(t)

		_, err := w.run("import", "caveman")

		Convey("When import runs with one", func() {
			Convey("Then it is a usage error: import takes no refs, that is the point", func() {
				So(err, ShouldNotBeNil)
				So(exitcode.Classify(err), ShouldEqual, exitcode.Usage)
				So(err.Error(), ShouldContainSubstring, "caveman")
			})
		})
	})
}
