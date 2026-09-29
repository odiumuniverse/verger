package apply

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/receipt"
)

// A skill is delivered as a directory, so the artifact a forced run has to
// keep is a tree, not a file. Reading it as a file failed on every
// tree-shaped package; the caller turned that failure back into a hands-off
// note that still said "rerun with --force", which sends a reader to a flag
// they already used. These pin the three shapes the receipt can carry.
func TestBackupUserEditKeepsTheWholeArtifact(t *testing.T) {
	Convey("Given an artifact a person edited, and a forced run with a backup root", t, func() {
		root := t.TempDir()
		backups := filepath.Join(root, "backups")

		r := &runner{
			ctx:  context.Background(),
			opts: Options{BackupsRoot: backups},
			now:  func() time.Time { return time.Unix(0, 0).UTC() },
		}

		Convey("Then a tree is copied whole, so an edit inside it survives", func() {
			tree := filepath.Join(root, "skills", "caveman")
			So(os.MkdirAll(tree, 0o750), ShouldBeNil)
			So(os.WriteFile(filepath.Join(tree, "SKILL.md"), []byte("# MY EDIT\n"), 0o600), ShouldBeNil)

			saved, err := r.backupUserEdit(receipt.Op{Kind: receipt.OpCopyTree, Path: tree})
			So(err, ShouldBeNil)
			So(readBackup(t, filepath.Join(saved, "SKILL.md")), ShouldEqual, "# MY EDIT\n")
		})

		Convey("Then a file is copied with its bytes", func() {
			file := filepath.Join(root, "one.md")
			So(os.WriteFile(file, []byte("# mine\n"), 0o600), ShouldBeNil)

			saved, err := r.backupUserEdit(receipt.Op{Kind: receipt.OpWriteFile, Path: file})
			So(err, ShouldBeNil)
			So(readBackup(t, saved), ShouldEqual, "# mine\n")
		})

		Convey("Then a symlink is kept as the link it is", func() {
			target := filepath.Join(root, "two.md")
			So(os.WriteFile(target, []byte("# two\n"), 0o600), ShouldBeNil)

			link := filepath.Join(root, "link.md")
			So(os.Symlink(target, link), ShouldBeNil)

			saved, err := r.backupUserEdit(receipt.Op{Kind: receipt.OpSymlink, Path: link})
			So(err, ShouldBeNil)

			kept, err := os.Readlink(saved)
			So(err, ShouldBeNil)
			So(kept, ShouldEqual, target)
		})
	})
}

func readBackup(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: the test names its own fixture
	So(err, ShouldBeNil)

	return string(data)
}
