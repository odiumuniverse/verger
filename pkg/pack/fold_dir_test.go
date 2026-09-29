package pack

import (
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/manifest"
)

// TestRenderCaseVariantDirectoryWithDistinctFileNames pins the guard that
// TestRenderCaseVariantDirectoryRefused claims to pin but does not reach.
//
// That test renders two skills named "Alpha" and "alpha" whose trees are
// identical, so both produce a SKILL.md and the FILE-level fold map
// (pack.go foldPath) refuses the collision first. foldDir is never consulted,
// which is why neutering it — `func (r *renderer) foldDir(...) { return nil }`
// — leaves the whole pack suite green.
//
// This fixture is the case foldDir exists for, and the one its own doc names
// (pack.go: "skills/Alpha/a.md" and "skills/alpha/b.md" are distinct file paths,
// but on a case-insensitive store they are the same directory): the two trees
// have DIFFERENT file names, so no file-level key ever collides and the
// directory prefix is the only thing that can catch it.
//
// Mutation that must go red: foldDir -> return nil.
func TestRenderCaseVariantDirectoryWithDistinctFileNames(t *testing.T) {
	Convey("Given two skills whose directories differ only by case and whose file names differ", t, func() {
		root := t.TempDir()

		// Deliberately NOT two SKILL.md files: identical file names would be
		// refused by the file-level guard and the directory guard would never
		// run. Different names are what make this the directory case.
		writeFile(t, filepath.Join(root, "src", "one", "alpha-skill.md"), "# alpha\n")
		writeFile(t, filepath.Join(root, "src", "two", "beta-skill.md"), "# beta\n")

		in := Input{
			ID: goldenID, Name: goldenName, Owner: goldenOwner, Version: goldenVersion,
			Root: root,
			Components: []manifest.Component{
				{Kind: manifest.KindSkill, Name: "Alpha", Path: "src/one"},
				{Kind: manifest.KindSkill, Name: "alpha", Path: "src/two"},
			},
		}

		Convey("When the package is rendered", func() {
			art, err := Render(in)

			Convey("Then the case-variant directory is refused, naming the directory", func() {
				// A file-level collision would say "output path ... differs
				// only by case"; the directory guard says "output directory".
				// Asserting on that word is what makes this a directory test
				// rather than another file-level one.
				target := renderError(t, err)
				So(target.Cause.Error(), ShouldContainSubstring, "output directory")
				So(target.Cause.Error(), ShouldContainSubstring, "differs only by case")
				So(art.Files, ShouldBeEmpty)
			})
		})

		Convey("And the control: the same two skills with the SAME file name are caught by the file guard", func() {
			// Without this, the test above could pass for the wrong reason:
			// if the file-level guard fired, the message would say "output
			// path", not "output directory", and the first assertion would
			// fail. This case documents what the file guard is for.
			same := t.TempDir()
			writeFile(t, filepath.Join(same, "src", "one", "SKILL.md"), "# alpha\n")
			writeFile(t, filepath.Join(same, "src", "two", "SKILL.md"), "# alpha\n")

			_, err := Render(Input{
				ID: goldenID, Name: goldenName, Owner: goldenOwner, Version: goldenVersion,
				Root: same,
				Components: []manifest.Component{
					{Kind: manifest.KindSkill, Name: "Alpha", Path: "src/one"},
					{Kind: manifest.KindSkill, Name: "alpha", Path: "src/two"},
				},
			})

			So(renderError(t, err).Cause.Error(), ShouldContainSubstring, "output path")
		})
	})
}
