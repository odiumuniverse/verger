package cli

import (
	"bytes"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func TestVersionCommand(t *testing.T) {
	Convey("Given a root command", t, func() {
		var out bytes.Buffer

		root := NewRootCmd(Options{Version: "1.2.3-test", Out: &out, Err: &out})

		Convey("When running version", func() {
			root.SetArgs([]string{"version"})

			err := root.Execute()

			Convey("Then it prints the injected version", func() {
				So(err, ShouldBeNil)
				So(strings.TrimSpace(out.String()), ShouldEqual, "verger 1.2.3-test")
			})
		})
	})
}

func TestRootCommandUnknownSubcommand(t *testing.T) {
	Convey("Given a root command", t, func() {
		root := NewRootCmd(Options{Version: "dev"})
		root.SetArgs([]string{"definitely-not-a-command"})

		Convey("When running an unknown subcommand", func() {
			err := root.Execute()

			Convey("Then it fails", func() {
				So(err, ShouldBeError)
			})
		})
	})
}
