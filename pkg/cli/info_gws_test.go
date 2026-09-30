package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/exitcode"
)

// infoEnvelope is what a consumer of `info --json` decodes into. The struct is
// spelled out here rather than reused from the command, so a field renamed or
// dropped on the producing side fails this test instead of silently agreeing
// with itself.
type infoEnvelope struct {
	Schema struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
	} `json:"schema"`
	Package     string `json:"package"`
	Scope       string `json:"scope"`
	Source      string `json:"source"`
	Channel     string `json:"channel"`
	Pinned      string `json:"pinned"`
	Disabled    bool   `json:"disabled"`
	AdoptedFrom string `json:"adopted_from"`
	Hosts       []struct {
		Host     string   `json:"host"`
		Version  string   `json:"version"`
		Strategy string   `json:"strategy"`
		Status   string   `json:"status"`
		Kind     string   `json:"kind"`
		Level    string   `json:"level"`
		Notes    []string `json:"notes"`
		Detail   string   `json:"detail"`
	} `json:"hosts"`
}

// specPath and lockPath are spelled out here rather than added to the shared
// world, which pR owns: two tests are not a reason to widen a file another
// worker edits.
func (w *world) specPath() string {
	return filepath.Join(w.homeDir, "verger.toml")
}

func (w *world) lockPath() string {
	return filepath.Join(w.homeDir, "verger.lock")
}

func (w *world) writeSpec(t *testing.T, body string) error {
	t.Helper()

	return os.WriteFile(w.specPath(), []byte(body), 0o600)
}

func (w *world) readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: the test's own temp path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(data)
}

func TestInfoJSON(t *testing.T) {
	Convey("Given an installed package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")

		stdout, err := w.run("info", "local:caveman", "--json")

		Convey("When info runs", func() {
			Convey("Then the document carries the envelope, the source and the host row", func() {
				So(err, ShouldBeNil)

				var doc infoEnvelope
				So(json.Unmarshal([]byte(stdout), &doc), ShouldBeNil)

				So(doc.Schema.Name, ShouldEqual, "verger.info")
				So(doc.Schema.Version, ShouldEqual, 1)
				So(doc.Package, ShouldEqual, "local:caveman")
				So(doc.Scope, ShouldEqual, "user")

				// The source is the ref the spec declared, not the cell's
				// version: `info` answers "where did this come from" and
				// "what is on disk" with different fields, because they are
				// different facts and a skew between them is what the user
				// is looking for.
				So(doc.Source, ShouldEqual, "local:caveman")

				So(len(doc.Hosts), ShouldEqual, 1)
				So(doc.Hosts[0].Host, ShouldEqual, "claude")
				So(doc.Hosts[0].Version, ShouldEqual, "1.2.3")
				So(doc.Hosts[0].Strategy, ShouldEqual, "loose")
				So(doc.Hosts[0].Detail, ShouldEqual, "current")
				So(doc.Hosts[0].Status, ShouldEqual, "delivered")
			})
		})
	})

	Convey("Given a package declared but delivered nowhere", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.warmHome(t)
		So(w.writeSpec(t, "schema = 1\n\n[[package]]\nid = \"local:ghost\"\n"), ShouldBeNil)

		stdout, err := w.run("info", "local:ghost", "--json")

		Convey("When info runs", func() {
			Convey("Then it answers rather than failing, with no host rows", func() {
				So(err, ShouldBeNil)

				var doc infoEnvelope
				So(json.Unmarshal([]byte(stdout), &doc), ShouldBeNil)
				So(doc.Package, ShouldEqual, "local:ghost")
				// Not omitted, and not null: a consumer ranging over the rows
				// must not have to distinguish "no hosts" from "field absent".
				So(doc.Hosts, ShouldNotBeNil)
				So(len(doc.Hosts), ShouldEqual, 0)
			})
		})
	})

	Convey("Given an id the spec does not declare", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.warmHome(t)

		_, err := w.run("info", "nope/none", "--json")

		Convey("When info runs", func() {
			Convey("Then it exits 2 and says how to fix the call", func() {
				So(err, ShouldNotBeNil)
				// 2, not 1: the invocation named a package and what it named
				// is not there. A script can act on that; a crash it cannot.
				So(exitcode.Classify(err), ShouldEqual, exitcode.Usage)
				So(err.Error(), ShouldContainSubstring, "nope/none")
				// The hint is the difference between "not in the spec" and a
				// dead end the user has to guess their way out of.
				So(err.Error(), ShouldContainSubstring, "verger install")
			})
		})
	})

	Convey("Given a home with no spec at all", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		_, err := w.run("info", "local:caveman", "--json")

		Convey("When info runs", func() {
			Convey("Then it is the same answer as an id that is not in one", func() {
				So(err, ShouldNotBeNil)
				So(exitcode.Classify(err), ShouldEqual, exitcode.Usage)
			})
		})
	})

	Convey("Given the wrong number of arguments", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		_, err := w.run("info")

		Convey("When info runs", func() {
			Convey("Then usage is refused before any home is opened", func() {
				So(err, ShouldNotBeNil)
				So(exitcode.Classify(err), ShouldEqual, exitcode.Usage)
			})
		})
	})
}

func TestInfoText(t *testing.T) {
	Convey("Given an installed package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")

		stdout, err := w.run("info", "local:caveman")

		Convey("When info runs without --json", func() {
			Convey("Then the human form names the source and the host row", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "local:caveman")
				So(stdout, ShouldContainSubstring, "source:")
				So(stdout, ShouldContainSubstring, "claude")
				So(stdout, ShouldContainSubstring, "1.2.3")
				// Read-only: the text form carries no JSON envelope, so a
				// human reading it and a script parsing it cannot be served
				// the same bytes.
				So(stdout, ShouldNotContainSubstring, `"schema"`)
			})
		})
	})

	Convey("Given a package delivered nowhere", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.warmHome(t)
		So(w.writeSpec(t, "schema = 1\n\n[[package]]\nid = \"local:ghost\"\n"), ShouldBeNil)

		stdout, err := w.run("info", "local:ghost")

		Convey("When info runs without --json", func() {
			Convey("Then it says so in words, not by printing an empty list", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "not installed on any host")
			})
		})
	})
}

func TestInfoDoesNotWrite(t *testing.T) {
	Convey("Given an installed package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")

		lockBefore := w.readFile(t, w.lockPath())

		_, err := w.run("info", "local:caveman", "--json")

		Convey("When info runs", func() {
			Convey("Then the lock is untouched, because info is read-only", func() {
				// A verb named after looking something up must not move
				// anything. If this fails, `info` grew a write - a resolver
				// call, a fetch, a receipt - and the name stopped being true.
				So(err, ShouldBeNil)
				So(w.readFile(t, w.lockPath()), ShouldEqual, lockBefore)
			})
		})
	})
}
