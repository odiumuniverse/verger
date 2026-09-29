package cli

import (
	"encoding/json"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// TestEveryJSONDocumentCarriesItsSchema is the check that keeps the promise in
// W7-UX-SPEC §2.1 from decaying: a new command that prints `--json` without an
// envelope fails here, because this walks the command tree rather than a list
// that a later change could forget to extend.
func TestEveryJSONDocumentCarriesItsSchema(t *testing.T) {
	Convey("Given the command tree", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")

		fixture := w.fixture(t)
		w.mustRun(t, "install", fixture, "-y")

		// Every command that emits a machine-readable document, with the
		// document name it must declare.
		surfaces := []struct {
			args []string
			name string
		}{
			{[]string{"status", "--json"}, schemaStatus},
			{[]string{"doctor", "--json"}, schemaDoctor},
			{[]string{"why", "local:caveman", "claude", "--json"}, schemaWhy},
			{[]string{"lint", fixture, "--json"}, schemaLint},
			{[]string{"propagate", "show", "--json"}, schemaPropagate},
			{[]string{"source", "list", "--json"}, schemaSource},
			{[]string{"secret", "list", "--json"}, schemaSecret},
			{[]string{"sync", "--json"}, schemaReport},
		}

		for _, surface := range surfaces {
			Convey("When "+strings.Join(surface.args, " ")+" runs", func() {
				stdout, _ := w.run(surface.args...)

				Convey("Then the first field names the document", func() {
					So(strings.TrimSpace(stdout), ShouldNotBeEmpty)

					var envelope struct {
						Schema *struct {
							Name    string `json:"name"`
							Version int    `json:"version"`
						} `json:"schema"`
					}

					So(json.Unmarshal([]byte(stdout), &envelope), ShouldBeNil)
					So(envelope.Schema, ShouldNotBeNil)
					So(envelope.Schema.Name, ShouldEqual, surface.name)
					So(envelope.Schema.Version, ShouldBeGreaterThanOrEqualTo, 1)
				})
			})
		}
	})
}

// TestSchemaIsTheFirstField pins the ordering the spec asks for, so a human
// reading the output sees what the document is before anything else.
func TestSchemaIsTheFirstField(t *testing.T) {
	Convey("Given a machine-readable run", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")

		stdout, _ := w.run("status", "--json")

		Convey("When the document is printed", func() {
			Convey("Then it opens with the schema envelope", func() {
				So(strings.HasPrefix(strings.TrimSpace(stdout), `{"schema":`), ShouldBeTrue)
			})
		})
	})
}

// TestJSONDocumentsKeepTheirExistingFields guards the additive promise: adding
// the envelope must not rename or drop what a consumer already reads.
func TestJSONDocumentsKeepTheirExistingFields(t *testing.T) {
	Convey("Given an installed package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")

		stdout, _ := w.run("status", "--json")

		Convey("When status prints JSON", func() {
			Convey("Then home and cells are still there, and the cell speaks the shared vocabulary", func() {
				var doc struct {
					Schema struct {
						Name string `json:"name"`
					} `json:"schema"`
					Home  string `json:"home"`
					Cells []struct {
						Package  string `json:"package"`
						Host     string `json:"host"`
						Scope    string `json:"scope"`
						Status   string `json:"status"`
						Strategy string `json:"strategy"`
					} `json:"cells"`
				}

				So(json.Unmarshal([]byte(stdout), &doc), ShouldBeNil)
				So(doc.Schema.Name, ShouldEqual, schemaStatus)
				So(doc.Home, ShouldEqual, w.homeDir)
				So(doc.Cells, ShouldHaveLength, 1)
				So(doc.Cells[0].Package, ShouldEqual, "local:caveman")
				So(doc.Cells[0].Host, ShouldEqual, "claude")

				// The status speaks the same stable enum the human table
				// does (W7-UX-SPEC §1.1): delivered, skipped, blocked,
				// your_edit, not_ours, failed. The strategy stays an
				// internal word: §1.2 keeps strategies out of the
				// user-facing vocabulary entirely.
				So(doc.Cells[0].Status, ShouldEqual, "delivered")
				So(doc.Cells[0].Strategy, ShouldEqual, "loose")
			})
		})
	})
}

// TestDoctorJSONIsAnObjectNotAnArray records the one breaking shape change
// this checkpoint makes, so nobody mistakes it for a regression later: a bare
// array cannot carry a schema field, so the findings moved under one.
func TestDoctorJSONIsAnObjectNotAnArray(t *testing.T) {
	Convey("Given doctor on a fresh machine", t, func() {
		w := newWorld(t)

		stdout, err := w.run("doctor", "--json")

		Convey("When doctor runs", func() {
			Convey("Then the document is an object whose findings are a field", func() {
				So(err, ShouldBeNil)
				So(strings.HasPrefix(strings.TrimSpace(stdout), "{"), ShouldBeTrue)

				var doc struct {
					Findings []json.RawMessage `json:"findings"`
				}

				So(json.Unmarshal([]byte(stdout), &doc), ShouldBeNil)
				So(doc.Findings, ShouldNotBeEmpty)
			})
		})
	})
}

// TestEmptyDocumentsStillCarryTheirSchema keeps the envelope present on the
// empty results, where it is easiest to forget and where a consumer most
// needs to know it read a real document.
func TestEmptyDocumentsStillCarryTheirSchema(t *testing.T) {
	Convey("Given a machine with no packages", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		stdout, _ := w.run("status", "--json")

		Convey("When status prints an empty matrix", func() {
			Convey("Then the envelope is still there", func() {
				So(stdout, ShouldContainSubstring, `"schema"`)
				So(stdout, ShouldContainSubstring, schemaStatus)
			})
		})
	})
}

// TestSchemaNamesAreDistinct guards against two surfaces claiming one name,
// which would make the name useless for telling documents apart.
func TestSchemaNamesAreDistinct(t *testing.T) {
	Convey("Given every declared document name", t, func() {
		names := []string{
			schemaStatus, schemaReport, schemaPlan, schemaWhy, schemaDoctor,
			schemaLint, schemaPack, schemaPropagate, schemaSource, schemaRemove,
			schemaSecret,
		}

		Convey("When they are compared", func() {
			Convey("Then no two documents share a name", func() {
				seen := map[string]bool{}

				for _, name := range names {
					So(seen[name], ShouldBeFalse)
					So(strings.HasPrefix(name, "verger."), ShouldBeTrue)
					seen[name] = true
				}
			})
		})
	})
}

// TestPackAndRestoreCarryTheirSchema covers the two write surfaces that a
// status/status-only walk would miss.
func TestPackAndRestoreCarryTheirSchema(t *testing.T) {
	Convey("Given a local author package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		owned := w.ownedFixture(t)
		packOut, _ := w.run("pack", owned, "--json")

		Convey("When pack runs", func() {
			Convey("Then the document names itself", func() {
				So(packOut, ShouldContainSubstring, schemaPack)
			})
		})

		Convey("Given a package to remove and restore", func() {
			w2 := newWorld(t)
			w2.chdir(t, w2.root)
			w2.target(t, "SKILL.md", "# installed\n")
			w2.mustRun(t, "install", w2.fixture(t), "-y")
			w2.mustRun(t, "remove", "local:caveman", "-y")

			restoreOut, _ := w2.run("restore", "local:caveman", "-y", "--json")

			Convey("When restore runs", func() {
				Convey("Then the document names itself", func() {
					So(restoreOut, ShouldContainSubstring, schemaRemove)
				})
			})
		})
	})
}

// TestSecretListDocumentShape keeps the secret surface honest: the envelope is
// the first field, and the names a consumer already read are still under it.
func TestSecretListDocumentShape(t *testing.T) {
	Convey("Given the secrets store", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		stdout, _ := w.run("secret", "list", "--json")

		Convey("When secret list runs", func() {
			Convey("Then the envelope comes first and names are still present", func() {
				So(strings.HasPrefix(strings.TrimSpace(stdout), `{"schema":`), ShouldBeTrue)

				var doc struct {
					Schema struct {
						Name string `json:"name"`
					} `json:"schema"`
					Names []string `json:"names"`
				}

				So(json.Unmarshal([]byte(stdout), &doc), ShouldBeNil)
				So(doc.Schema.Name, ShouldEqual, schemaSecret)
				So(doc.Names, ShouldNotBeNil)
			})
		})
	})
}
