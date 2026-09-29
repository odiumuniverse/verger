package cli

import (
	"bytes"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// beadle accepts --version and -v, and a user switching between the two tools
// types the same thing twice a day. A flag that only one of them has is a
// papercut nobody files, because the error is a rejection rather than a bug.
func TestVersionFlagMatchesTheVersionCommand(t *testing.T) {
	Convey("Given the root command", t, func() {
		Convey("Then --version, -v and `version` all print the same line", func() {
			for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
				var out bytes.Buffer

				root := NewRootCmd(Options{Version: "1.2.3", Out: &out, Err: &out})
				root.SetOut(&out)
				root.SetErr(&out)
				root.SetArgs(args)

				So(root.Execute(), ShouldBeNil)
				So(out.String(), ShouldEqual, "verger 1.2.3\n")
			}
		})
	})
}
