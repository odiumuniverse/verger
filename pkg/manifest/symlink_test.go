package manifest

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func TestDeclaredSkillsRejectsSymlinkEscape(t *testing.T) {
	Convey("Given a package root with a symlink pointing outside", t, func() {
		root := t.TempDir()
		outside := t.TempDir()

		// Create a skill outside the package
		outsideSkill := filepath.Join(outside, "evil")
		So(os.MkdirAll(outsideSkill, 0o750), ShouldBeNil)
		So(os.WriteFile(filepath.Join(outsideSkill, "SKILL.md"), []byte("---\nname: evil\n---\n"), 0o600), ShouldBeNil)

		// Create a symlink inside the package pointing outside
		linkPath := filepath.Join(root, "linked")
		So(os.Symlink(outsideSkill, linkPath), ShouldBeNil)

		Convey("When declaredSkills resolves a path through the symlink", func() {
			var warnings []string

			components := declaredSkills(root, FormatClaude, "test.json", []string{"linked"}, &warnings)

			Convey("Then it does not follow the symlink outside the root", func() {
				So(components, ShouldBeEmpty)
				So(warnings, ShouldNotBeEmpty)
			})
		})
	})
}

func TestDeclaredSkillsAcceptsSymlinkInside(t *testing.T) {
	Convey("Given a package root with a real skill", t, func() {
		root := t.TempDir()

		// Create a real skill inside the package
		realSkill := filepath.Join(root, "real")
		So(os.MkdirAll(realSkill, 0o750), ShouldBeNil)
		So(os.WriteFile(filepath.Join(realSkill, "SKILL.md"), []byte("---\nname: real\n---\n"), 0o600), ShouldBeNil)

		Convey("When declaredSkills resolves the real path", func() {
			var warnings []string

			components := declaredSkills(root, FormatClaude, "test.json", []string{"real"}, &warnings)

			Convey("Then it finds the skill", func() {
				So(components, ShouldHaveLength, 1)
				So(components[0].Name, ShouldEqual, "real")
			})
		})
	})
}

func TestLocalPathRejectsSymlinkEscape(t *testing.T) {
	Convey("Given a root and a symlink pointing outside", t, func() {
		root := t.TempDir()
		outside := t.TempDir()

		linkPath := filepath.Join(root, "escape")
		So(os.Symlink(outside, linkPath), ShouldBeNil)

		Convey("When localPath resolves the symlink", func() {
			_, ok := localPath(root, "escape")

			Convey("Then it returns false", func() {
				So(ok, ShouldBeFalse)
			})
		})
	})
}

func TestLocalPathAcceptsNormalPath(t *testing.T) {
	Convey("Given a root and a normal relative path", t, func() {
		root := t.TempDir()

		Convey("When localPath resolves a normal path", func() {
			got, ok := localPath(root, "skills")

			Convey("Then it returns the joined path", func() {
				So(ok, ShouldBeTrue)
				So(got, ShouldEqual, filepath.Join(root, "skills"))
			})
		})
	})
}
