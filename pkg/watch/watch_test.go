package watch_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/watch"
)

// wait bounds every event wait so a broken engine fails instead of hanging.
const wait = 10 * time.Second

// fixed is the injected clock: the stamp, not the timer (WithClock).
var fixed = time.Date(2026, 9, 29, 1, 0, 0, 0, time.UTC)

func clock() time.Time { return fixed }

// harness runs the engine against a temp root and collects events.
type harness struct {
	out    chan watch.Event
	cancel context.CancelFunc
	done   chan error
}

// start runs the engine with the given targets and options, and waits until it
// is armed (WithReady) before returning.
func start(t *testing.T, targets []watch.Target, opts ...watch.Option) *harness {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	out := make(chan watch.Event, 16)
	done := make(chan error, 1)
	ready := make(chan struct{}, 1)

	all := append([]watch.Option{watch.WithClock(clock), watch.WithReady(ready)}, opts...)

	go func() { done <- watch.Run(ctx, targets, out, all...) }()

	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("e2e: Run returned before it was armed: %v", err)
	case <-time.After(wait):
		cancel()
		t.Fatal("the engine never became armed")
	}

	h := &harness{out: out, cancel: cancel, done: done}
	t.Cleanup(h.stop)

	return h
}

// stop cancels and asserts a clean, prompt return.
func (h *harness) stop() {
	h.cancel()

	select {
	case err := <-h.done:
		if err != nil {
			panic("watch: Run returned " + err.Error())
		}
	case <-time.After(wait):
		panic("watch: Run did not stop on cancel")
	}
}

// next waits for one batch.
func (h *harness) next(t *testing.T) watch.Event {
	t.Helper()

	select {
	case ev := <-h.out:
		sort.Strings(ev.Paths)

		return ev
	case <-time.After(wait):
		t.Fatal("no batch arrived")

		return watch.Event{}
	}
}

// quiet asserts no batch arrives within d.
func (h *harness) quiet(t *testing.T, d time.Duration) {
	t.Helper()

	select {
	case ev := <-h.out:
		t.Fatalf("unexpected batch: %+v", ev)
	case <-time.After(d):
	}
}

func TestRunDeliversOneBatchPerBurst(t *testing.T) {
	Convey("Given a watched directory", t, func() {
		dir := t.TempDir()
		h := start(t, []watch.Target{{Host: "claude", Paths: []string{dir}}}, watch.WithDebounce(80*time.Millisecond))

		Convey("When three files appear in a burst", func() {
			for _, name := range []string{"a.md", "b.md", "c.md"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			}

			Convey("Then one batch names all three and carries the injected clock", func() {
				ev := h.next(t)
				So(ev.Host, ShouldEqual, "claude")
				So(ev.Paths, ShouldHaveLength, 3)
				So(ev.At, ShouldEqual, fixed)
			})
		})
	})
}

func TestRunNeverDropsTheLastChange(t *testing.T) {
	Convey("Given a watched directory with a short window", t, func() {
		dir := t.TempDir()
		h := start(t, []watch.Target{{Host: "claude", Paths: []string{dir}}}, watch.WithDebounce(120*time.Millisecond))

		Convey("When a second change lands inside the window", func() {
			if err := os.WriteFile(filepath.Join(dir, "first.md"), []byte("1"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			time.Sleep(60 * time.Millisecond)

			if err := os.WriteFile(filepath.Join(dir, "last.md"), []byte("2"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			Convey("Then one batch carries both, the later one included", func() {
				ev := h.next(t)
				names := []string{}

				for _, p := range ev.Paths {
					names = append(names, filepath.Base(p))
				}

				So(names, ShouldContain, "last.md")
				So(names, ShouldContain, "first.md")
			})
		})
	})
}

func TestRunDebouncesPerHost(t *testing.T) {
	Convey("Given two hosts watching their own directories", t, func() {
		claudeDir := t.TempDir()
		codexDir := t.TempDir()
		h := start(t, []watch.Target{
			{Host: "claude", Paths: []string{claudeDir}},
			{Host: "codex", Paths: []string{codexDir}},
		}, watch.WithDebounce(80*time.Millisecond))

		Convey("When both change in one burst", func() {
			if err := os.WriteFile(filepath.Join(claudeDir, "a.md"), []byte("x"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			if err := os.WriteFile(filepath.Join(codexDir, "b.md"), []byte("x"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			Convey("Then each host gets its own batch, naming only its own path", func() {
				first := h.next(t)
				second := h.next(t)

				hosts := []string{first.Host, second.Host}
				sort.Strings(hosts)

				So(hosts, ShouldResemble, []string{"claude", "codex"})
				So(first.Paths, ShouldHaveLength, 1)
				So(second.Paths, ShouldHaveLength, 1)

				if first.Host == "claude" {
					So(filepath.Base(first.Paths[0]), ShouldEqual, "a.md")
					So(filepath.Base(second.Paths[0]), ShouldEqual, "b.md")
				} else {
					So(filepath.Base(first.Paths[0]), ShouldEqual, "b.md")
					So(filepath.Base(second.Paths[0]), ShouldEqual, "a.md")
				}
			})
		})
	})
}

func TestRunAtomicRenameIsOneBatch(t *testing.T) {
	Convey("Given a watched directory", t, func() {
		dir := t.TempDir()
		h := start(t, []watch.Target{{Host: "claude", Paths: []string{dir}}}, watch.WithDebounce(120*time.Millisecond))

		Convey("When a writer does tmp+rename", func() {
			tmp := filepath.Join(dir, ".final.md.tmp")
			final := filepath.Join(dir, "final.md")

			So(os.WriteFile(tmp, []byte("x"), 0o600), ShouldBeNil)
			So(os.Rename(tmp, final), ShouldBeNil)

			Convey("Then exactly one batch arrives and names the final path", func() {
				ev := h.next(t)
				So(ev.Paths, ShouldContain, final)
				h.quiet(t, 250*time.Millisecond)
			})
		})
	})
}

func TestRunWatchesNewSubdirectories(t *testing.T) {
	Convey("Given a watched directory", t, func() {
		dir := t.TempDir()
		h := start(t, []watch.Target{{Host: "claude", Paths: []string{dir}}}, watch.WithDebounce(60*time.Millisecond))

		Convey("When a subdirectory is created and then written into", func() {
			sub := filepath.Join(dir, "sub")

			if err := os.MkdirAll(sub, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			first := h.next(t)
			So(filepath.Base(first.Paths[0]), ShouldEqual, "sub")

			if err := os.WriteFile(filepath.Join(sub, "deep.md"), []byte("x"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			Convey("Then the write inside the new subdirectory is reported too", func() {
				ev := h.next(t)
				So(filepath.Base(ev.Paths[0]), ShouldEqual, "deep.md")
			})
		})
	})
}

func TestRunPromotesMissingPaths(t *testing.T) {
	Convey("Given a target path that does not exist yet", t, func() {
		dir := t.TempDir()
		missing := filepath.Join(dir, "later")
		h := start(t, []watch.Target{{Host: "claude", Paths: []string{missing}}}, watch.WithDebounce(60*time.Millisecond))

		Convey("When the directory appears and a file lands inside it", func() {
			So(os.MkdirAll(missing, 0o700), ShouldBeNil)

			_ = h.next(t) // the creation of the path itself

			So(os.WriteFile(filepath.Join(missing, "a.md"), []byte("x"), 0o600), ShouldBeNil)

			Convey("Then the promoted watch reports the file", func() {
				ev := h.next(t)
				So(filepath.Base(ev.Paths[0]), ShouldEqual, "a.md")
			})
		})
	})
}

func TestRunWatchesAFileTarget(t *testing.T) {
	Convey("Given a target that is one file", t, func() {
		dir := t.TempDir()
		file := filepath.Join(dir, "settings.json")
		So(os.WriteFile(file, []byte("{}"), 0o600), ShouldBeNil)

		h := start(t, []watch.Target{{Host: "claude", Paths: []string{file}}}, watch.WithDebounce(60*time.Millisecond))

		Convey("When the file is written", func() {
			So(os.WriteFile(file, []byte("{\"a\":1}"), 0o600), ShouldBeNil)

			Convey("Then its path is reported", func() {
				ev := h.next(t)
				So(ev.Paths, ShouldContain, file)
			})
		})
	})
}

func TestRunIgnoresPathsOutsideTheTargets(t *testing.T) {
	Convey("Given one watched directory and one that is not watched", t, func() {
		watched := t.TempDir()
		other := t.TempDir()
		h := start(t, []watch.Target{{Host: "claude", Paths: []string{watched}}}, watch.WithDebounce(60*time.Millisecond))

		Convey("When only the unwatched directory changes", func() {
			So(os.WriteFile(filepath.Join(other, "a.md"), []byte("x"), 0o600), ShouldBeNil)

			Convey("Then no batch is emitted", func() {
				h.quiet(t, 250*time.Millisecond)
			})
		})
	})
}

func TestRunStopsOnContextCancel(t *testing.T) {
	Convey("Given a running engine", t, func() {
		before := runtime.NumGoroutine()
		dir := t.TempDir()
		h := start(t, []watch.Target{{Host: "claude", Paths: []string{dir}}}, watch.WithDebounce(40*time.Millisecond))

		Convey("When the context is cancelled", func() {
			h.cancel()

			Convey("Then Run returns nil and no goroutine leaks", func() {
				// stop() in t.Cleanup verifies Run returned nil
				settled := false

				for range 50 {
					if runtime.NumGoroutine() <= before {
						settled = true

						break
					}

					time.Sleep(20 * time.Millisecond)
				}

				So(settled, ShouldBeTrue)
			})
		})
	})
}

func TestRunShutsDownWithASlowConsumer(t *testing.T) {
	Convey("Given a caller that never reads the batch channel", t, func() {
		dir := t.TempDir()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		out := make(chan watch.Event) // unbuffered and unread
		done := make(chan error, 1)
		ready := make(chan struct{}, 1)

		go func() {
			done <- watch.Run(ctx, []watch.Target{{Host: "claude", Paths: []string{dir}}}, out,
				watch.WithDebounce(30*time.Millisecond), watch.WithReady(ready), watch.WithClock(clock))
		}()

		select {
		case <-ready:
		case <-time.After(wait):
			t.Fatal("never armed")
		}

		Convey("When a change lands and the context is cancelled", func() {
			So(os.WriteFile(filepath.Join(dir, "a.md"), []byte("x"), 0o600), ShouldBeNil)
			time.Sleep(120 * time.Millisecond)
			cancel()

			Convey("Then Run still returns", func() {
				select {
				case err := <-done:
					So(err, ShouldBeNil)
				case <-time.After(wait):
					t.Fatal("Run wedged on an unread channel")
				}
			})
		})
	})
}

func TestRunWithoutTargetsIsIdle(t *testing.T) {
	Convey("Given no targets at all", t, func() {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		out := make(chan watch.Event, 1)
		done := make(chan error, 1)

		go func() { done <- watch.Run(ctx, nil, out, watch.WithClock(clock)) }()

		Convey("When it runs and is cancelled", func() {
			time.Sleep(50 * time.Millisecond)
			cancel()

			Convey("Then it returns nil without emitting anything", func() {
				select {
				case err := <-done:
					So(err, ShouldBeNil)
				case <-time.After(wait):
					t.Fatal("Run did not return")
				}

				So(len(out), ShouldEqual, 0)
			})
		})
	})
}

func TestDebounceFallsBackToTheDefault(t *testing.T) {
	Convey("Given the debounce option", t, func() {
		Convey("When a zero or negative window is asked for", func() {
			Convey("Then the default window is used instead", func() {
				So(watch.DebounceFor(0), ShouldEqual, watch.DefaultDebounce)
				So(watch.DebounceFor(-time.Second), ShouldEqual, watch.DefaultDebounce)
				So(watch.DebounceFor(150*time.Millisecond), ShouldEqual, 150*time.Millisecond)
			})
		})
	})
}

func TestLimitErrorIsTypedAndActionable(t *testing.T) {
	Convey("Given a watch limit failure", t, func() {
		err := error(&watch.LimitError{Path: "/home/u/.claude", Op: "add", Cause: errors.New("no space left on device")})

		Convey("When a caller classifies it", func() {
			limit, ok := errors.AsType[*watch.LimitError](err)

			Convey("Then the type carries the path and both remedies", func() {
				So(ok, ShouldBeTrue)
				So(limit.Path, ShouldEqual, "/home/u/.claude")
				So(err.Error(), ShouldContainSubstring, "inotify")
				So(err.Error(), ShouldContainSubstring, "max_user_watches")
				So(strings.Contains(err.Error(), "narrow"), ShouldBeTrue)
			})
		})
	})
}

func TestRunReportsAWatchFailure(t *testing.T) {
	Convey("Given a target path that cannot be watched", t, func() {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		out := make(chan watch.Event, 1)
		done := make(chan error, 1)
		// A path that does not exist anywhere: its nearest existing parent is
		// /, which is watchable — so use a path under a file, where no parent
		// directory can be watched at all.
		file := filepath.Join(t.TempDir(), "a-file")
		So(os.WriteFile(file, []byte("x"), 0o600), ShouldBeNil)

		go func() {
			done <- watch.Run(ctx, []watch.Target{{Host: "claude", Paths: []string{filepath.Join(file, "impossible")}}}, out, watch.WithClock(clock))
		}()

		Convey("Then Run fails with the reason instead of hanging", func() {
			select {
			case err := <-done:
				So(err, ShouldNotBeNil)
				So(strings.Contains(err.Error(), "impossible"), ShouldBeTrue)
			case <-time.After(wait):
				t.Fatal("Run neither failed nor returned")
			}
		})
	})
}
