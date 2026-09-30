package verger

import (
	"errors"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/source"
)

func TestChannelErrorIsAUsageError(t *testing.T) {
	Convey("Given a channel the source refused", t, func() {
		raw := &source.ChannelError{Ref: "acme/thing", Channel: "nightly", Available: []string{"stable", "beta"}}

		Convey("Then the facade reports it as usage, so it exits 2", func() {
			// Exit 2 is the same code a misspelled flag gets, which is the
			// point: a mistyped channel is a mistyped invocation.
			// UsageError is what the exit-code classifier turns into 2; the
			// mapping itself is covered in pkg/exitcode, which cannot be
			// imported from here without a cycle.
			So(classifyChannelError(raw), ShouldHaveSameTypeAs, &UsageError{})
		})

		Convey("And the list of real channels survives the wrapping", func() {
			So(classifyChannelError(raw).Error(), ShouldContainSubstring, "nightly")
			So(classifyChannelError(raw).Error(), ShouldContainSubstring, "stable")
		})

		Convey("And an error that is not about a channel is left alone", func() {
			// A ref that will not fetch because the network is down is not a
			// usage error, and re-labelling it would tell the user to fix
			// their command line.
			other := errors.New("dial tcp: connection refused")
			So(classifyChannelError(other), ShouldEqual, other)
			So(classifyChannelError(other), ShouldNotEqual, &UsageError{})
		})
	})
}
