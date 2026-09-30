package cli

import (
	"encoding/json"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/exitcode"
)

// searchEnvelope is what a consumer of `search --json` decodes into. The
// struct is spelled out here rather than reused from the command, so a field
// renamed or dropped on the producing side fails this test instead of
// silently agreeing with itself.
type searchEnvelope struct {
	Schema struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
	} `json:"schema"`
	Query   string `json:"query"`
	Matches []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Source    string `json:"source"`
		InSpec    bool   `json:"in_spec"`
		InLock    bool   `json:"in_lock"`
		Installed bool   `json:"installed"`
		OfferedBy string `json:"offered_by"`
	} `json:"matches"`
	Skipped []struct {
		Source string `json:"source"`
		Reason string `json:"reason"`
	} `json:"skipped"`
}

func decodeSearch(t *testing.T, stdout string) searchEnvelope {
	t.Helper()

	var doc searchEnvelope
	So(json.Unmarshal([]byte(stdout), &doc), ShouldBeNil)

	return doc
}

// searchRow returns the whitespace-separated fields of the table row for one
// package id. Reading the row as fields rather than as a padded string is what
// lets a test say what the row says; matching the padding would only pin the
// column widths.
func searchRow(stdout, id string) []string {
	for line := range strings.SplitSeq(stdout, "\n") {
		if !strings.HasPrefix(line, id+" ") {
			continue
		}

		return strings.Fields(line)
	}

	return nil
}

// The question a user actually asks of a spec with a dozen packages is "which
// of these do I already have", and until this existed the answer was read out
// of `verger status` by eye. The four states are kept apart on purpose: "do I
// have this" is four different questions, and collapsing them into a yes/no is
// how a user installs something they already had.
func TestSearchJSON(t *testing.T) {
	Convey("Given an installed package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y", "--hooks", "yes")

		stdout, err := w.run("search", "caveman", "--json")

		Convey("When search runs", func() {
			Convey("Then the document carries the envelope and the four states", func() {
				So(err, ShouldBeNil)

				doc := decodeSearch(t, stdout)
				So(doc.Schema.Name, ShouldEqual, "verger.search")
				So(doc.Schema.Version, ShouldEqual, 1)
				So(doc.Query, ShouldEqual, "caveman")

				So(len(doc.Matches), ShouldEqual, 1)
				So(doc.Matches[0].ID, ShouldEqual, "local:caveman")
				So(doc.Matches[0].InSpec, ShouldBeTrue)
				So(doc.Matches[0].InLock, ShouldBeTrue)
				So(doc.Matches[0].Installed, ShouldBeTrue)
			})
		})
	})

	Convey("Given a package declared but delivered nowhere", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.warmHome(t)
		So(w.writeSpec(t, "schema = 1\n\n[[package]]\nid = \"local:ghost\"\n"), ShouldBeNil)

		stdout, err := w.run("search", "ghost", "--json")

		Convey("When search runs", func() {
			Convey("Then it answers with the spec state and no delivery", func() {
				So(err, ShouldBeNil)

				doc := decodeSearch(t, stdout)
				So(len(doc.Matches), ShouldEqual, 1)
				So(doc.Matches[0].InSpec, ShouldBeTrue)
				So(doc.Matches[0].InLock, ShouldBeFalse)
				So(doc.Matches[0].Installed, ShouldBeFalse)
			})
		})
	})

	Convey("Given a query that matches nothing", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.warmHome(t)
		So(w.writeSpec(t, "schema = 1\n\n[[package]]\nid = \"local:ghost\"\n"), ShouldBeNil)

		stdout, err := w.run("search", "zzz-not-a-package", "--json")

		Convey("When search runs", func() {
			Convey("Then it is an empty list and not an error", func() {
				// No match is an answer a script can act on ("install it"),
				// not a failure it has to distinguish from a broken home.
				So(err, ShouldBeNil)

				doc := decodeSearch(t, stdout)
				// Not omitted, and not null: a consumer ranging over the rows
				// must not have to tell "nothing matched" from "field absent".
				So(doc.Matches, ShouldNotBeNil)
				So(len(doc.Matches), ShouldEqual, 0)
			})
		})
	})

	Convey("Given the wrong number of arguments", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		_, err := w.run("search")

		Convey("When search runs without a query", func() {
			Convey("Then usage is refused before any home is opened", func() {
				So(err, ShouldNotBeNil)
				So(exitcode.Classify(err), ShouldEqual, exitcode.Usage)
			})
		})
	})
}

// The cache line. A non-local source has no business answering, so if the user
// declared one and we cannot reach it, the row has to carry that fact rather
// than the user discovering it by trusting an empty list.
func TestSearchReportsSourcesItCouldNotRead(t *testing.T) {
	Convey("Given a spec declaring a source verger must not fetch", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.warmHome(t)
		So(w.writeSpec(t, "schema = 1\n\n[[source]]\nname = \"remote\"\nurl = \"github:someone/anything\"\n\n[[package]]\nid = \"local:caveman\"\n"), ShouldBeNil)

		stdout, err := w.run("search", "caveman", "--json")

		Convey("When search runs", func() {
			Convey("Then the spec answer and the unreadable source both appear", func() {
				So(err, ShouldBeNil)

				doc := decodeSearch(t, stdout)
				So(len(doc.Matches), ShouldEqual, 1)
				So(doc.Matches[0].ID, ShouldEqual, "local:caveman")

				So(len(doc.Skipped), ShouldEqual, 1)
				So(doc.Skipped[0].Source, ShouldEqual, "remote")
				So(doc.Skipped[0].Reason, ShouldNotBeEmpty)
			})
		})

		Convey("And the text form names it too", func() {
			text, err := w.run("search", "caveman")

			So(err, ShouldBeNil)
			So(text, ShouldContainSubstring, "remote")
			So(text, ShouldContainSubstring, "local:caveman")
		})
	})
}

// A local source IS searchable, and that is the difference between the two
// rows: local means we may read the directory, non-local means we must not.
// It is also the only way `search` answers "does this package exist at all" —
// a package nothing declares and nothing offers is invisible to every other
// verb.
func TestSearchReadsADeclaredLocalSource(t *testing.T) {
	Convey("Given a spec declaring a local source with one package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.warmHome(t)

		// Next to the spec, because a relative source resolves against the
		// spec's own directory rather than against the working directory.
		w.noHooksFixtureIn(t, w.homeDir)
		So(w.writeSpec(t, "schema = 1\n\n[[source]]\nname = \"local\"\nurl = \"local:./nohooks\"\n"), ShouldBeNil)

		stdout, err := w.run("search", "nohooks", "--json")

		Convey("When search runs", func() {
			Convey("Then the offer is in the result and no source was skipped", func() {
				So(err, ShouldBeNil)

				doc := decodeSearch(t, stdout)
				So(len(doc.Matches), ShouldEqual, 1)
				So(doc.Matches[0].OfferedBy, ShouldEqual, "local")
				// Offered is not declared and not installed: three different
				// facts, and the row that blurred them would send the user
				// off to install what the machine can already hand them.
				So(doc.Matches[0].InSpec, ShouldBeFalse)
				So(doc.Matches[0].Installed, ShouldBeFalse)
				So(len(doc.Skipped), ShouldEqual, 0)
			})
		})
	})
}

// The host filter is the difference between "installed somewhere" and
// "installed where I am looking". It has to change the answer, not decorate
// it: a package delivered only to claude is not installed as far as a
// codex-only question is concerned.
func TestSearchHostFilterChangesTheAnswer(t *testing.T) {
	Convey("Given a package delivered to one of two detected hosts", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.hosts = append(w.hosts, newFakeHost("codex"))

		w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y", "--hooks", "yes", "--hosts", "claude")

		Convey("When search asks about the host that has it", func() {
			stdout, err := w.run("search", "caveman", "--hosts", "claude", "--json")

			So(err, ShouldBeNil)

			doc := decodeSearch(t, stdout)
			So(len(doc.Matches), ShouldEqual, 1)
			So(doc.Matches[0].Installed, ShouldBeTrue)
		})

		Convey("And when search asks about the host that does not", func() {
			stdout, err := w.run("search", "caveman", "--hosts", "codex", "--json")

			So(err, ShouldBeNil)

			doc := decodeSearch(t, stdout)
			So(len(doc.Matches), ShouldEqual, 1)
			So(doc.Matches[0].InSpec, ShouldBeTrue)
			So(doc.Matches[0].Installed, ShouldBeFalse)
		})
	})

	Convey("Given an unknown --hosts value", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		_, err := w.run("search", "caveman", "--hosts", "nope")

		Convey("When search runs", func() {
			Convey("Then the host is refused rather than answering about nothing", func() {
				// A host this machine does not have is a refusal everywhere
				// else; a quietly empty row would read as "installed nowhere"
				// and send the user off to install what they already have.
				So(err, ShouldNotBeNil)
				So(exitcode.Classify(err), ShouldEqual, exitcode.Usage)
			})
		})
	})
}

// The human form. A list the user reads has to say which of the four states
// applies, in words, without a JSON parser.
func TestSearchText(t *testing.T) {
	Convey("Given an installed package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y", "--hooks", "yes")

		stdout, err := w.run("search", "caveman")

		Convey("When search runs without --json", func() {
			Convey("Then the header names the states and the row fills them in", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "PACKAGE")
				So(stdout, ShouldContainSubstring, "SPEC")
				So(stdout, ShouldContainSubstring, "LOCK")
				So(stdout, ShouldContainSubstring, "INSTALLED")
				So(stdout, ShouldContainSubstring, "local:caveman")
				// The row itself, field by field: a table whose state columns
				// were always "yes" would still pass every substring check
				// above, and it would send the user to install what they have.
				So(searchRow(stdout, "local:caveman"), ShouldResemble,
					[]string{"local:caveman", "yes", "yes", "yes", "fixture", "claude"})

				// Read-only: the text form carries no JSON envelope, so a
				// human reading it and a script parsing it cannot be served
				// the same bytes.
				So(stdout, ShouldNotContainSubstring, `"schema"`)
			})
		})
	})

	Convey("Given a package declared but never delivered", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.warmHome(t)
		So(w.writeSpec(t, "schema = 1\n\n[[package]]\nid = \"local:ghost\"\n"), ShouldBeNil)

		stdout, err := w.run("search", "ghost")

		Convey("When search runs", func() {
			Convey("Then the row says declared but not installed, in those words", func() {
				// The distinction the whole verb exists for: "in the spec" and
				// "installed" are different facts, and a row that blurred them
				// answers "yes you have it" to a package the user never got.
				So(err, ShouldBeNil)
				So(searchRow(stdout, "local:ghost"), ShouldResemble,
					[]string{"local:ghost", "yes", "no", "no", "none", "none"})
			})
		})
	})

	Convey("Given a query that matches nothing", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.warmHome(t)

		stdout, err := w.run("search", "zzz-not-a-package")

		Convey("When search runs without --json", func() {
			Convey("Then it says so in words rather than printing an empty table", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "zzz-not-a-package")
				So(stdout, ShouldContainSubstring, "no matches")
			})
		})
	})
}

// A verb named after looking something up must not move anything.
func TestSearchDoesNotWrite(t *testing.T) {
	Convey("Given an installed package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y", "--hooks", "yes")

		specBefore := w.readFile(t, w.specPath())
		lockBefore := w.readFile(t, w.lockPath())

		_, err := w.run("search", "caveman", "--json")

		Convey("When search runs", func() {
			Convey("Then the spec and the lock are untouched, because search is read-only", func() {
				// If this fails, `search` grew a write - a resolver call, a
				// fetch, a receipt - and the name stopped being true.
				So(err, ShouldBeNil)
				So(w.readFile(t, w.specPath()), ShouldEqual, specBefore)
				So(w.readFile(t, w.lockPath()), ShouldEqual, lockBefore)
			})
		})
	})
}
