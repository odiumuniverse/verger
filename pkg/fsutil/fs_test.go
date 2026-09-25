package fsutil_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/fsutil"
)

func TestFSType(t *testing.T) {
	Convey("Given a real directory and a missing one", t, func() {
		dir := t.TempDir()

		Convey("When the filesystem type is read", func() {
			fsType, err := fsutil.FSType(dir)
			_, missingErr := fsutil.FSType(filepath.Join(dir, "missing"))

			Convey("Then a named type is returned and missing paths fail", func() {
				So(err, ShouldBeNil)
				So(fsType, ShouldNotBeEmpty)
				So(missingErr, ShouldBeError)
			})
		})
	})
}

func TestSameFS(t *testing.T) {
	Convey("Given directories on the local filesystem", t, func() {
		first := t.TempDir()
		second := t.TempDir()

		Convey("When same-filesystem is checked", func() {
			same, err := fsutil.SameFS(first, second)
			So(err, ShouldBeNil)

			sameSelf, err := fsutil.SameFS(first, first)
			So(err, ShouldBeNil)

			_, missingErr := fsutil.SameFS(first, filepath.Join(first, "missing"))

			Convey("Then local paths share a device and missing paths fail", func() {
				So(same, ShouldBeTrue)
				So(sameSelf, ShouldBeTrue)
				So(missingErr, ShouldBeError)
			})
		})
	})
}

func TestIsCrossDevice(t *testing.T) {
	Convey("Given cross-device and unrelated errors", t, func() {
		wrapped := fmt.Errorf("rename failed: %w", &os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.EXDEV})

		Convey("When they are classified", func() {
			Convey("Then EXDEV is detected through wrapping", func() {
				So(fsutil.IsCrossDevice(syscall.EXDEV), ShouldBeTrue)
				So(fsutil.IsCrossDevice(wrapped), ShouldBeTrue)
				So(fsutil.IsCrossDevice(syscall.ENOENT), ShouldBeFalse)
				So(fsutil.IsCrossDevice(errors.New("plain")), ShouldBeFalse)
				So(fsutil.IsCrossDevice(nil), ShouldBeFalse)
			})
		})
	})
}
