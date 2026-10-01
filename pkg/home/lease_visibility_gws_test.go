package home

import (
	"errors"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// The lease exists so the two tools do not fight over the vault: beadle holding
// it makes the verger watcher sleep, and the rule only means anything if the
// rule is symmetric. A beadle lease must stop a verger run exactly as a verger
// lease must stop a beadle run.
//
// The property that broke in `watch.lease.holder` was one-sided. The refusal test
// that existed asked a single question — beadle holds, can verger take it — and
// the mirror question was never asked, so a lease could stop one tool and not
// the other and every unit test would still pass. That is a coin landing on
// heads: green on both the run that works and the run that does not.
//
// So both directions are asked here, and each is asked twice: once as the
// acquirer being refused, and once as the reader being told. The second half is
// the one a fix usually leaves behind — refusing is easy, reporting the right
// holder is the part that goes wrong, because a reader that filtered by owner
// would report "nobody holds it" and let a second run start.
func TestALeaseIsMutuallyVisibleBetweenTheTwoOwners(t *testing.T) {
	for _, tc := range []struct {
		holder string
		asker  string
	}{
		{holder: LeaseOwnerBeadle, asker: LeaseOwnerVerger},
		{holder: LeaseOwnerVerger, asker: LeaseOwnerBeadle},
	} {
		Convey("Given "+tc.holder+" holds the lease", t, func() {
			path := leasePath(t)

			handle, err := AcquireLease(path, tc.holder)
			So(err, ShouldBeNil)

			Convey("Then "+tc.asker+" can read who holds it", func() {
				// The reader is asked, not the holder. LeaseStatus takes no
				// owner and must not grow one: a reader that could filter by
				// owner is a reader that can be wrong about a foreign holder,
				// and that is the whole question.
				status, held, err := LeaseStatus(path)
				So(err, ShouldBeNil)
				So(held, ShouldBeTrue)
				So(status.Owner, ShouldEqual, tc.holder)
				So(status.PID, ShouldEqual, handle.Info().PID)
			})

			Convey("Then "+tc.asker+" is refused, and the refusal names the holder", func() {
				_, err := AcquireLease(path, tc.asker)
				So(err, ShouldNotBeNil)

				held, ok := errors.AsType[*LeaseHeldError](err)
				So(ok, ShouldBeTrue)
				// The owner in the error is what tells the second tool whose
				// run to wait for. An error that merely says "held" sends the
				// reader to the filesystem to find out, which is the work the
				// lease existed to save.
				So(held.Owner, ShouldEqual, tc.holder)
				So(err.Error(), ShouldContainSubstring, tc.holder)
			})

			Convey("And once released, the other owner takes it", func() {
				// Mutual visibility is not only about saying no. If the first
				// holder's release left the file unreadable to the second,
				// the pair would deadlock on a lease nobody can drop.
				So(handle.Release(), ShouldBeNil)

				second, err := AcquireLease(path, tc.asker)
				So(err, ShouldBeNil)

				status, held, err := LeaseStatus(path)
				So(err, ShouldBeNil)
				So(held, ShouldBeTrue)
				So(status.Owner, ShouldEqual, tc.asker)

				So(second.Release(), ShouldBeNil)
			})
		})
	}
}
