package cli

import (
	"bytes"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func TestVersionCommand(t *testing.T) {
	Convey("Given a root command", t, func() {
		root := NewRootCmd("1.2.3-test")

		Convey("When running version", func() {
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
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
		root := NewRootCmd("dev")
		root.SetArgs([]string{"definitely-not-a-command"})

		Convey("When running an unknown subcommand", func() {
			err := root.Execute()

			Convey("Then it fails", func() {
				So(err, ShouldBeError)
			})
		})
	})
}
