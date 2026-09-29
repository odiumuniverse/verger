package verger_test

import (
	"context"
	"errors"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/home"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// Two watchers on one home must contend. The lease is the only thing that
// stops them, and a mutation that lets a second watcher take the lease by
// clearing the first leaves the whole package green: two processes then
// reconcile the same files at once, which is the exact harm D19 forbids.
//
// The test drives Client.Watch, not home.AcquireLease, because the mutation
// lives in the path a front end actually takes.
func TestTwoWatchersOnOneHomeContend(t *testing.T) {
	Convey("Given a watcher already running on this home", t, func() {
		root := t.TempDir()
		client, err := verger.Open(t.Context(), verger.WithHome(root))
		So(err, ShouldBeNil)

		defer func() { _ = client.Close() }()

		// The engine is the far end of the first Watch: it must stay inside
		// the call, because the lease is held for the call's duration.
		running := make(chan struct{})

		verger.SetWatchEngine(func(ctx context.Context, _ []verger.WatchTarget, _ chan<- verger.WatchEvent, _ ...verger.WatchOption) error {
			close(running)
			<-ctx.Done()

			return ctx.Err()
		})

		defer verger.SetWatchEngine(nil)

		paths, err := client.Paths(verger.User, "")
		So(err, ShouldBeNil)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		done := make(chan error, 1)

		go func() { done <- client.Watch(ctx, verger.WatchOptions{Paths: paths, Owner: verger.LeaseOwnerVerger}) }()

		<-running

		Convey("Then a second watcher is refused, and the lease names the holder", func() {
			watchErr := client.Watch(t.Context(), verger.WatchOptions{
				Paths: paths, Owner: verger.LeaseOwnerBeadle,
			})

			So(watchErr, ShouldNotBeNil)

			var held *home.LeaseHeldError

			So(errors.As(watchErr, &held), ShouldBeTrue)
			So(held.Owner, ShouldEqual, verger.LeaseOwnerVerger)
		})

		Convey("And once the first one stops, the lease is free again", func() {
			cancel()
			So(<-done, ShouldNotBeNil)

			verger.SetWatchEngine(func(context.Context, []verger.WatchTarget, chan<- verger.WatchEvent, ...verger.WatchOption) error {
				return nil
			})

			So(client.Watch(t.Context(), verger.WatchOptions{
				Paths: paths, Owner: verger.LeaseOwnerBeadle,
			}), ShouldBeNil)
		})
	})
}

// TestWatchReleasesTheLeaseOnReturn pins the other half: a lease left behind
// by a watcher that returned would lock the next run out of its own home.
func TestWatchReleasesTheLeaseOnReturn(t *testing.T) {
	Convey("Given a watcher whose engine ends at once", t, func() {
		root := t.TempDir()
		client, err := verger.Open(t.Context(), verger.WithHome(root))
		So(err, ShouldBeNil)

		defer func() { _ = client.Close() }()

		verger.SetWatchEngine(func(context.Context, []verger.WatchTarget, chan<- verger.WatchEvent, ...verger.WatchOption) error {
			return nil
		})

		defer verger.SetWatchEngine(nil)

		paths, err := client.Paths(verger.User, "")
		So(err, ShouldBeNil)

		So(client.Watch(t.Context(), verger.WatchOptions{Paths: paths, Owner: verger.LeaseOwnerVerger}), ShouldBeNil)

		_, err = client.AcquireLease(verger.LeaseOwnerBeadle)
		So(err, ShouldBeNil)
	})
}
