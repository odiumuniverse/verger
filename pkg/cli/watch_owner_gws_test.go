package cli

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// The lease owner is a closed vocabulary — the facade accepts `verger` and
// `beadle` and nothing else, because a free-form owner is how a lease file
// ends up naming something nobody recognises. The default had drifted to a
// third spelling, so a bare `verger watch` was turned away by its own default
// before it ever reached a host.
func TestWatchDefaultsToAKnownLeaseOwner(t *testing.T) {
	Convey("Given the watch command's default lease owner", t, func() {
		Convey("Then it is the one the facade names for verger", func() {
			So(watchOwner, ShouldEqual, "verger")
		})

		Convey("And it is not a third spelling nobody recognises", func() {
			So(watchOwner, ShouldNotEqual, "verger-watch")
		})
	})
}
