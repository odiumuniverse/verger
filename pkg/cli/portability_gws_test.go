package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// TestDoctorReportsNonPortableSources covers the check pR asked for
// (docs/reviews/FAIL4-portability.md §5.1): a source that lives outside the
// vault must surface as a warning, because a clone to another machine will
// not resolve it.
func TestDoctorReportsNonPortableSources(t *testing.T) {
	Convey("Given a spec with a source outside the vault", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		outside := filepath.Join(t.TempDir(), "elsewhere")

		writeWorldFile(t, filepath.Join(w.homeDir, "verger.toml"), `schema = 1

[[source]]
name = 'stray'
url = '`+outside+`'

[[package]]
id = 'local:caveman'
version = '1.2.3'
`)

		stdout, err := w.run("doctor", "--json")

		Convey("When doctor runs", func() {
			Convey("Then the finding is a warning, and not an autofix", func() {
				// A warning must not fail the run: the vault is usable here,
				// it just will not travel.
				So(err, ShouldBeNil)

				var doc doctorDoc

				So(json.Unmarshal([]byte(stdout), &doc), ShouldBeNil)

				found := findFinding(doc.Findings, "source.not-portable")
				So(found, ShouldNotBeNil)
				So(found.Severity, ShouldEqual, severityWarning)
				So(found.SafeToAutofix, ShouldBeFalse)
				So(found.Message, ShouldContainSubstring, outside)
			})
		})
	})
}

// TestDoctorStaysQuietAboutPortableSources is the other half: the check must
// not cry wolf. A vault whose sources all travel is silent.
func TestDoctorStaysQuietAboutPortableSources(t *testing.T) {
	Convey("Given a spec whose source is inside the vault", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		writeWorldFile(t, filepath.Join(w.homeDir, "verger.toml"), `schema = 1

[[source]]
name = 'local'
url = './vault/caveman'

[[package]]
id = 'local:caveman'
version = '1.2.3'
`)

		stdout, _ := w.run("doctor", "--json")

		var doc doctorDoc

		So(json.Unmarshal([]byte(stdout), &doc), ShouldBeNil)

		Convey("When doctor runs", func() {
			Convey("Then no portability finding is raised", func() {
				So(findFinding(doc.Findings, "source.not-portable"), ShouldBeNil)
			})
		})
	})
}

// TestDoctorNonPortableSourceCarriesNoActionableFix pins the decision pR
// recorded: verger must not rewrite a path it did not write, so the finding
// carries no command that pretends to fix it.
func TestDoctorNonPortableSourceCarriesNoActionableFix(t *testing.T) {
	Convey("Given a non-portable source", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		outside := filepath.Join(t.TempDir(), "elsewhere")

		writeWorldFile(t, filepath.Join(w.homeDir, "verger.toml"), `schema = 1

[[source]]
name = 'stray'
url = '`+outside+`'
`)

		stdout, _ := w.run("doctor", "--json")

		var doc doctorDoc

		So(json.Unmarshal([]byte(stdout), &doc), ShouldBeNil)

		found := findFinding(doc.Findings, "source.not-portable")

		Convey("When doctor reports it", func() {
			Convey("Then the message names the offending path and nothing more", func() {
				So(found, ShouldNotBeNil)
				So(found.Message, ShouldContainSubstring, "clone")

				for _, step := range found.Fix {
					So(strings.Join(step, " "), ShouldNotContainSubstring, "rewrite")
				}
			})
		})
	})
}

// TestDoctorNonPortableSourceIsNeverAutofixed proves the negative at the
// behavioural level: `doctor --fix` must leave the spec alone, because the
// repair belongs to the vault's author.
func TestDoctorNonPortableSourceIsNeverAutofixed(t *testing.T) {
	Convey("Given a non-portable source and --fix", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		specPath := filepath.Join(w.homeDir, "verger.toml")
		outside := filepath.Join(t.TempDir(), "elsewhere")

		original := `schema = 1

[[source]]
name = 'stray'
url = '` + outside + `'
`
		writeWorldFile(t, specPath, original)

		_, err := w.run("doctor", "--fix", "-y")
		So(err, ShouldBeNil)

		Convey("When doctor --fix runs", func() {
			Convey("Then the spec is byte-identical afterwards", func() {
				after := readWorldFile(t, specPath)

				So(after, ShouldEqual, original)
			})
		})
	})
}
