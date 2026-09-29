package cli

import (
	"errors"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/exitcode"
)

// A run that wrote a report saying `failed` and then exited 0 is the one
// answer a script must never be able to read. The verdict is unit-tested on
// printReport, but this pins it through the command a person actually types:
// a cell that cannot be written has to leave the process non-zero.
func TestAFailedCellMakesTheCommandExitNonZero(t *testing.T) {
	Convey("Given a host that fails to deliver", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		ref := w.fixture(t)

		w.fake.mu.Lock()
		w.fake.deliverErr = errors.New("the disk said no")
		w.fake.mu.Unlock()

		Convey("Then install reports the failure and exits non-zero", func() {
			stdout, err := w.run("install", ref, "-y")

			So(err, ShouldNotBeNil)
			So(exitCode(err), ShouldNotEqual, 0)
			So(exitCode(err), ShouldEqual, exitcode.Unexpected)
			So(stdout, ShouldContainSubstring, "failed")

			Convey("And the typed error names the cell that failed", func() {
				So(err.Error(), ShouldContainSubstring, "@claude")
			})
		})
	})
}
