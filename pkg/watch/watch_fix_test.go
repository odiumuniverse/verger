package watch_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/watch"
)

// prime starts a watcher and returns once its initial watches are armed.
// WithReady is the liveness signal; writing a probe file in a loop instead
// livelocks against the debounce, because every probe re-arms the timer and
// the window never closes.
func prime(t *testing.T, dir string, opts ...watch.Option) (<-chan watch.Event, func()) {
	t.Helper()

	out := make(chan watch.Event, 256)
	ready := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})

	go func() {
		defer close(done)

		if err := watch.Run(ctx, []watch.Target{{Host: "claude", Paths: []string{dir}}}, out,
			append([]watch.Option{watch.WithReady(ready)}, opts...)...); err != nil {
			t.Errorf("watch.Run: %v", err)
		}
	}()

	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("Run never armed its initial watches")
	}

	return out, func() {
		cancel()
		<-done
	}
}

func waitBatch(t *testing.T, out <-chan watch.Event, within time.Duration) watch.Event {
	t.Helper()

	select {
	case ev := <-out:
		return ev
	case <-time.After(within):
		t.Fatalf("no batch within %s", within)

		return watch.Event{}
	}
}

// waitBatchMatching waits for the first batch that satisfies want, discarding
// the batches before it.
//
// The churn burst in this file produces its own events, and how many of them
// arrive before the survivor depends on how the debounce window happened to
// fall. Taking the first event is therefore a race: under load the first batch
// is a churn batch, and the assertion fails even though the watcher behaved
// correctly. The test is about the *later* change, so it waits for that one.
func waitBatchMatching(t *testing.T, out <-chan watch.Event, within time.Duration, want func(watch.Event) bool) watch.Event {
	t.Helper()

	deadline := time.After(within)

	for {
		select {
		case ev := <-out:
			if want(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("no matching batch within %s", within)

			return watch.Event{}
		}
	}
}

const shortDebounce = 120 * time.Millisecond

// Finding 1 (BLOCKER): a create that vanishes before the engine reaches it is
// ordinary traffic — an editor swap, a lock file, any write+unlink — and it
// must not take the watcher down.
func TestVanishingCreateDoesNotKillTheEngine(t *testing.T) {
	Convey("Given a running watcher", t, func() {
		dir := t.TempDir()
		out, stop := prime(t, dir, watch.WithDebounce(shortDebounce))

		defer stop()

		Convey("When files are created and removed faster than the engine reads them", func() {
			for range 40 {
				name := filepath.Join(dir, "swap")
				So(os.WriteFile(name, []byte("x"), 0o600), ShouldBeNil)
				So(os.Remove(name), ShouldBeNil)
			}

			Convey("Then the engine is still alive and reports a later change", func() {
				So(os.WriteFile(filepath.Join(dir, "survivor.md"), []byte("s"), 0o600), ShouldBeNil)

				ev := waitBatchMatching(t, out, 30*time.Second, func(e watch.Event) bool {
					return strings.Contains(strings.Join(e.Paths, "\n"), "survivor.md")
				})
				So(ev.Host, ShouldEqual, "claude")
				So(strings.Join(ev.Paths, "\n"), ShouldContainSubstring, "survivor.md")
			})
		})
	})
}

// Finding 1, at the level below: Run must not return an error for a vanished
// path even when the whole burst is churn.
func TestVanishingCreateLeavesRunReturningNil(t *testing.T) {
	Convey("Given a directory that only ever sees churn", t, func() {
		dir := t.TempDir()
		out := make(chan watch.Event, 256)
		ctx, cancel := context.WithCancel(t.Context())

		ready := make(chan struct{}, 1)

		runErr := make(chan error, 1)

		go func() {
			runErr <- watch.Run(ctx, []watch.Target{{Host: "claude", Paths: []string{dir}}}, out, watch.WithReady(ready))
		}()

		// Synchronise on the watcher's own readiness signal rather than a
		// sleep. A fixed sleep is a race: under load Run may not have
		// registered its watches yet and the churn below would run against
		// nothing. That is how this test failed under `go test ./...`.
		select {
		case <-ready:
		case <-time.After(30 * time.Second):
			t.Fatal("watch.Run never reported ready")
		}

		// 200 create+remove pairs: the engine will process a Create for a path
		// that is already gone.
		for range 200 {
			name := filepath.Join(dir, "churn")
			_ = os.WriteFile(name, []byte("x"), 0o600)
			_ = os.Remove(name)
		}

		cancel()

		select {
		case err := <-runErr:
			So(err, ShouldBeNil)
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not return after cancel")
		}
	})
}

// Finding 2 (MAJOR): a file written into a brand-new subdirectory, with no
// pause between the mkdir and the write, must still be reported. The old code
// armed the sub-watch only when it processed the Create event for the
// directory, so the file's own event was gone before any watch existed.
func TestFileLandingInABrandNewSubdir(t *testing.T) {
	Convey("Given a running watcher on a directory", t, func() {
		dir := t.TempDir()
		out, stop := prime(t, dir, watch.WithDebounce(shortDebounce))

		defer stop()

		Convey("When a subdirectory is created and written to in one step", func() {
			// MkdirAll then WriteFile with NO pause: this is the case the
			// author's own test avoided by waiting for the directory batch.
			deep := filepath.Join(dir, "brand", "new")
			So(os.MkdirAll(deep, 0o750), ShouldBeNil)
			So(os.WriteFile(filepath.Join(deep, "SKILL.md"), []byte("s"), 0o600), ShouldBeNil)

			Convey("Then the file is reported, not only the directory", func() {
				ev := waitBatch(t, out, 15*time.Second)
				So(ev.Host, ShouldEqual, "claude")

				seen := strings.Join(ev.Paths, "\n")
				So(seen, ShouldContainSubstring, "SKILL.md")
			})
		})
	})
}

// Finding 2, deeper: the rescan must reach the whole new subtree, not just its
// top level, because the same window exists at every level.
func TestDeepFileInANewSubtreeIsReported(t *testing.T) {
	Convey("Given a running watcher", t, func() {
		dir := t.TempDir()
		out, stop := prime(t, dir, watch.WithDebounce(shortDebounce))

		defer stop()

		Convey("When three levels appear at once, each with a file", func() {
			deep := filepath.Join(dir, "a", "b", "c")
			So(os.MkdirAll(deep, 0o750), ShouldBeNil)
			So(os.WriteFile(filepath.Join(deep, "leaf.md"), []byte("l"), 0o600), ShouldBeNil)

			Convey("Then the leaf is reported", func() {
				ev := waitBatch(t, out, 15*time.Second)
				So(strings.Join(ev.Paths, "\n"), ShouldContainSubstring, "leaf.md")
			})
		})
	})
}

// Finding 3: the ENOSPC typing must be reachable. The public test cannot drive
// the watcher add path, so this lives in the internal test file; see
// limit_internal_test.go for the mutation-proof assertion on isLimit.
func TestLimitErrorMessageNamesBothRemedies(t *testing.T) {
	Convey("Given a typed watch-limit error", t, func() {
		err := &watch.LimitError{Path: "/h/.cursor", Op: "add", Cause: syscall.ENOSPC}

		Convey("Then it names the path, the op, the limit and both remedies", func() {
			msg := err.Error()
			So(msg, ShouldContainSubstring, "/h/.cursor")
			So(msg, ShouldContainSubstring, "watch limit is exhausted")
			So(msg, ShouldContainSubstring, "fs.inotify.max_user_watches")
			So(msg, ShouldContainSubstring, "narrow the target paths")
		})

		Convey("Then it unwraps to the platform error", func() {
			So(errors.Is(err, syscall.ENOSPC), ShouldBeTrue)
		})
	})
}
