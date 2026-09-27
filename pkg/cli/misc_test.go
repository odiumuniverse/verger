package cli

import (
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// ownedFixture writes a Claude-format package whose manifest name carries an
// owner, so author commands (pack) can derive owner/name.
func (w *world) ownedFixture(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(w.root, "owned")

	writeWorldFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), `{
  "name": "acme/caveman",
  "version": "1.2.3",
  "description": "Caveman toolkit."
}`)
	writeWorldFile(t, filepath.Join(dir, "skills", "one", "SKILL.md"), "# one\n")

	return "./owned"
}

func TestPackRendersChimera(t *testing.T) {
	Convey("Given an author package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		stdout, err := w.run("pack", w.ownedFixture(t), "--json")

		Convey("When pack runs", func() {
			Convey("Then a chimera is written to the store and reported", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, `"dir"`)

				dir := filepath.Join(w.dataDir, "synth", "acme", "caveman", "1.2.3")
				So(fileExists(filepath.Join(dir, "plugin.json")), ShouldBeTrue)
				So(fileExists(filepath.Join(dir, ".claude-plugin", "plugin.json")), ShouldBeTrue)
			})
		})
	})
}

func TestLintValidatesPackage(t *testing.T) {
	Convey("Given an author package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		stdout, err := w.run("lint", w.ownedFixture(t))

		Convey("When lint runs", func() {
			Convey("Then it reports the package shape", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "acme/caveman")
			})
		})
	})

	Convey("Given a directory with no manifest", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		dir := filepath.Join(w.root, "empty")
		writeWorldFile(t, filepath.Join(dir, "README.md"), "# nothing\n")

		_, err := w.run("lint", "./empty")

		Convey("When lint runs", func() {
			Convey("Then it fails", func() {
				So(err, ShouldNotBeNil)
			})
		})
	})
}
