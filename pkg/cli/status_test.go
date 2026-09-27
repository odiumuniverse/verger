package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/lock"
)

func TestStatusJSONGolden(t *testing.T) {
	Convey("Given an installed package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")

		stdout, err := w.run("status", "--json")

		Convey("When status runs", func() {
			Convey("Then the document is the stable matrix", func() {
				So(err, ShouldBeNil)

				want := fmt.Sprintf(
					"{\"home\":%q,\"cells\":[{\"package\":\"local:caveman\",\"host\":\"claude\",\"scope\":\"user\",\"status\":\"current\",\"version\":\"1.2.3\",\"strategy\":\"loose\"}]}\n",
					w.homeDir,
				)
				So(stdout, ShouldEqual, want)
			})
		})
	})

	Convey("Given a lock cell without a receipt", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")

		lockPath := filepath.Join(w.homeDir, "verger.lock")
		doc, err := lock.ParseFile(lockPath)
		So(err, ShouldBeNil)
		So(doc.Upsert(lock.Cell{
			Package: "ghost", Host: "claude", Scope: "user",
			Version: "9.9.9", Strategy: lock.StrategyLoose,
		}), ShouldBeNil)
		So(doc.Save(lockPath), ShouldBeNil)

		all, err := w.run("status", "--json")
		outdated, outdatedErr := w.run("status", "--outdated-only", "--json")

		Convey("When status runs", func() {
			Convey("Then the lock-only cell is missing and --outdated-only filters", func() {
				So(err, ShouldBeNil)
				So(all, ShouldContainSubstring, `"package":"local:caveman"`)
				So(all, ShouldContainSubstring, `"package":"ghost"`)

				So(outdatedErr, ShouldBeNil)
				So(outdated, ShouldNotContainSubstring, `"package":"local:caveman"`)
				So(outdated, ShouldContainSubstring, `"package":"ghost"`)
				So(outdated, ShouldContainSubstring, `"status":"missing"`)
			})
		})
	})
}

func TestWhyJSON(t *testing.T) {
	Convey("Given an installed package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")

		stdout, err := w.run("why", "local:caveman", "claude", "--json")

		Convey("When why runs", func() {
			Convey("Then the explanation struct is stable", func() {
				So(err, ShouldBeNil)

				var doc struct {
					Package  string   `json:"package"`
					Host     string   `json:"host"`
					Status   string   `json:"status"`
					Version  string   `json:"version"`
					Strategy string   `json:"strategy"`
					Reasons  []string `json:"reasons"`
					Blockers []string `json:"blockers"`
				}

				So(json.Unmarshal([]byte(stdout), &doc), ShouldBeNil)
				So(doc.Package, ShouldEqual, "local:caveman")
				So(doc.Host, ShouldEqual, "claude")
				So(doc.Status, ShouldEqual, "current")
				So(doc.Version, ShouldEqual, "1.2.3")
				So(doc.Strategy, ShouldEqual, "loose")
				So(doc.Reasons, ShouldNotBeEmpty)
				So(doc.Blockers, ShouldBeEmpty)
			})
		})
	})

	Convey("Given an unknown cell", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		stdout, err := w.run("why", "nope/none", "claude", "--json")

		Convey("When why runs", func() {
			Convey("Then it reports the missing cell without failing the run", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, `"status":"missing"`)
			})
		})
	})
}

func TestDoctorJSON(t *testing.T) {
	Convey("Given a fresh machine", t, func() {
		w := newWorld(t)

		stdout, err := w.run("doctor", "--json")

		Convey("When doctor runs", func() {
			Convey("Then it reports checks and stays read-only", func() {
				So(err, ShouldBeNil)

				var checks []struct {
					Severity string `json:"severity"`
					Check    string `json:"check"`
					Message  string `json:"message"`
				}

				So(json.Unmarshal([]byte(stdout), &checks), ShouldBeNil)
				So(checks, ShouldNotBeEmpty)

				names := map[string]bool{}

				for _, check := range checks {
					names[check.Check] = true
					So(check.Severity, ShouldBeIn, []string{"ok", "warning", "error"})
				}

				So(names, ShouldContainKey, "home")
				So(names, ShouldContainKey, "store")
			})
		})
	})

	Convey("Given a corrupt receipt", t, func() {
		w := newWorld(t)

		writeWorldFile(t, filepath.Join(w.homeDir, "state", "receipts", "caveman", "claude-user.json"), "{not json")

		_, err := w.run("doctor", "--json")

		Convey("When doctor runs", func() {
			Convey("Then the receipt check is an error and the run fails", func() {
				So(err, ShouldNotBeNil)
			})
		})
	})
}

func TestPropagateShow(t *testing.T) {
	Convey("Given a spec with the default propagation policy", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		writeWorldFile(t, filepath.Join(w.homeDir, "verger.toml"), `schema = 1

[propagate]
remove = "origin"
`)

		stdout, err := w.run("propagate", "show")

		Convey("When propagate show runs", func() {
			Convey("Then the effective policy is printed", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "remove")
				So(stdout, ShouldContainSubstring, "origin")
			})
		})
	})
}

func TestPropagateScopedShowRoundTrip(t *testing.T) {
	Convey("Given kind- and host-scoped propagation rules", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		w.mustRun(t, "propagate", "set", "remove", "origin", "--kind", "skill")
		w.mustRun(t, "propagate", "set", "install", "ask", "--host", "codex")

		Convey("When propagate show runs", func() {
			Convey("Then the scoped policy round-trips as JSON and as text", func() {
				stdout, err := w.run("propagate", "show", "--json")
				So(err, ShouldBeNil)

				var doc struct {
					Kind map[string]map[string]string `json:"kind"`
					Host map[string]map[string]string `json:"host"`
				}

				So(json.Unmarshal([]byte(stdout), &doc), ShouldBeNil)
				So(doc.Kind["skill"]["remove"], ShouldEqual, "origin")
				So(doc.Host["codex"]["install"], ShouldEqual, "ask")

				text, err := w.run("propagate", "show")
				So(err, ShouldBeNil)
				So(text, ShouldContainSubstring, "kind/skill")
				So(text, ShouldContainSubstring, "origin")
				So(text, ShouldContainSubstring, "host/codex")
				So(text, ShouldContainSubstring, "ask")
			})
		})
	})
}
