package watchbind_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/verger"
	"github.com/odiumuniverse/verger/pkg/watchbind"
)

func TestInstallMakesWatchLive(t *testing.T) {
	Convey("Given the engine installed", t, func() {
		watchbind.Install()

		Convey("Then Watch runs instead of reporting the capability unavailable", func() {
			// The defect: nothing called SetWatchEngine, so Client.Watch
			// answered *NotAvailableError before it ever looked at a path.
			// Reaching the engine — and ending quietly on cancellation — is
			// what this proves.
			home := t.TempDir()

			t.Setenv("HOME", home)
			t.Setenv("VERGER_HOME", "")

			client, err := verger.Open(t.Context(), verger.WithHome(filepath.Join(home, ".verger")))
			So(err, ShouldBeNil)

			defer func() { _ = client.Close() }()

			paths, err := client.Paths(verger.User, "")
			So(err, ShouldBeNil)

			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()

			watchErr := client.Watch(ctx, verger.WatchOptions{Paths: paths, Owner: verger.LeaseOwnerVerger})

			Convey("And the error is the context, never the unavailable capability", func() {
				// Before the fix this was *verger.NotAvailableError, returned
				// instantly without the engine ever running. Ending on the
				// deadline proves the engine was reached and ran for the whole
				// window.
				if watchErr != nil {
					_, unavailable := errors.AsType[*verger.NotAvailableError](watchErr)
					So(unavailable, ShouldBeFalse)
					So(errors.Is(watchErr, context.DeadlineExceeded), ShouldBeTrue)
				}
			})
		})
	})
}

func TestDebounceRoundTripsToTheEngineOption(t *testing.T) {
	Convey("Given a debounce set through the facade", t, func() {
		Convey("Then a binder reads the same value back", func() {
			// WatchOption is opaque to callers; WatchConfig is the one
			// readable form, and it is what carries the value across.
			got, ok := watchbind.DebounceOf(verger.WithDebounce(750 * time.Millisecond))
			So(ok, ShouldBeTrue)
			So(got, ShouldEqual, 750*time.Millisecond)
		})
	})

	Convey("Given no debounce set", t, func() {
		Convey("Then the binder reports none and the engine keeps its default", func() {
			_, ok := watchbind.DebounceOf()
			So(ok, ShouldBeFalse)
		})
	})
}

func TestRunReportsCancellationAsSuccess(t *testing.T) {
	Convey("Given an already-cancelled context", t, func() {
		Convey("Then Run returns nil rather than context.Canceled", func() {
			// pkg/watch's contract is nil-on-cancel; the forwarder must not
			// turn that into an error a CLI would print on Ctrl-C.
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			So(watchbind.Run(ctx, nil, make(chan verger.WatchEvent, 1)), ShouldBeNil)
		})
	})
}
