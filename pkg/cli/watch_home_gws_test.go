package cli

import (
	"context"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/home"
	"github.com/odiumuniverse/verger/pkg/watchbind"
)

// The home a watcher runs in is the one thing that decides whether verger and
// beadle share a lease or a vault. A watcher that starts without saying so
// leaves the operator with two live watchers and no way to tell which home
// each took — which is exactly the split this line makes visible.
func TestWatchSaysWhichHomeItChose(t *testing.T) {
	Convey("Given a watch with a resolved home", t, func() {
		watchbind.Install()

		w := newWorld(t)
		w.chdir(t, w.root)
		w.fixture(t)

		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan error, 1)

		go func() {
			_, runErr := w.runCtx(ctx, "watch", "--owner", "verger")
			done <- runErr
		}()

		// Long enough to reach the banner, short enough to stay a test.
		time.Sleep(300 * time.Millisecond)

		cancel()

		select {
		case runErr := <-done:
			So(runErr, ShouldBeNil)
		case <-time.After(5 * time.Second):
			t.Fatal("verger watch did not stop within 5s of cancellation")
		}

		Convey("Then the watcher names the home it took", func() {
			So(w.err.String(), ShouldContainSubstring, w.homeDir)
		})

		Convey("And it names the discovery rule, so a split is diagnosable", func() {
			// The world clears VERGER_HOME and ships no beadle vault, so
			// the fallback rule is the one under test here.
			So(w.err.String(), ShouldContainSubstring, string(home.SourceDefault))
		})

		Convey("And it goes to stderr, so a redirected run keeps a clean stdout", func() {
			So(w.out.String(), ShouldNotContainSubstring, "home")
		})
	})
}
