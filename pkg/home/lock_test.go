package home

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	. "github.com/smartystreets/goconvey/convey"
)

const (
	lockHelperEnv  = "VERGER_HOME_LOCK_HELPER"
	lockHelperRoot = "VERGER_HOME_LOCK_ROOT"
)

// TestLockHelperProcess is the child side of the two-process lock tests: it
// acquires the home lock, prints "locked" and holds it until stdin closes.
func TestLockHelperProcess(t *testing.T) {
	if os.Getenv(lockHelperEnv) != "1" {
		return
	}

	h, err := New(os.Getenv(lockHelperRoot))
	if err != nil {
		trace("helper: " + err.Error())

		os.Exit(2)
	}

	unlock, err := h.Lock(context.Background())
	if err != nil {
		trace("helper lock: " + err.Error())

		os.Exit(3)
	}

	fmt.Println("locked")

	_, _ = io.Copy(io.Discard, os.Stdin) // hold until the parent closes stdin

	if err := unlock(); err != nil {
		trace("helper unlock: " + err.Error())

		os.Exit(4)
	}

	os.Exit(0)
}

// lockHelper is one running helper process holding the home lock.
type lockHelper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

// startLockHelper re-runs the test binary as a helper and waits until it holds
// the lock.
func startLockHelper(t *testing.T, root string) *lockHelper {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=TestLockHelperProcess") //nolint:gosec // G204: the test binary re-runs itself

	cmd.Env = append(os.Environ(), lockHelperEnv+"=1", lockHelperRoot+"="+root)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("helper stdin: %v", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("helper start: %v", err)
	}

	ready := make(chan string, 1)

	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil {
			ready <- ""

			return
		}

		ready <- line
	}()

	select {
	case line := <-ready:
		if strings.TrimSpace(line) != "locked" {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()

			t.Fatalf("helper did not lock, said %q", line)
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()

		t.Fatal("helper did not lock in time")
	}

	return &lockHelper{cmd: cmd, stdin: stdin}
}

// release closes stdin and waits for a clean helper exit.
func (h *lockHelper) release(t *testing.T) {
	t.Helper()

	if err := h.stdin.Close(); err != nil {
		t.Fatalf("helper stdin close: %v", err)
	}

	if err := h.cmd.Wait(); err != nil {
		t.Fatalf("helper exit: %v", err)
	}
}

// kill terminates the helper without letting it release the lock.
func (h *lockHelper) kill(t *testing.T) {
	t.Helper()

	if err := h.cmd.Process.Kill(); err != nil {
		t.Fatalf("helper kill: %v", err)
	}

	if err := h.cmd.Wait(); err == nil {
		t.Fatal("killed helper reported a clean exit")
	}
}

// assertLockedErr checks the typed lock error and its context cause.
func assertLockedErr(t *testing.T, err, cause error) {
	t.Helper()

	target, ok := errors.AsType[*LockedError](err)
	if !ok {
		t.Fatalf("expected *LockedError, got %v", err)
	}

	if target.Path == "" || target.Cause == nil {
		t.Fatalf("LockedError is incomplete: %+v", target)
	}

	if cause != nil && !errors.Is(err, cause) {
		t.Fatalf("LockedError does not wrap %v: %v", cause, err)
	}

	if !strings.Contains(err.Error(), "locked") {
		t.Fatalf("LockedError message lacks the condition: %q", err.Error())
	}
}

func TestLockAcquireRelease(t *testing.T) {
	Convey("Given a home", t, func() {
		h, err := New(filepath.Join(t.TempDir(), "verger"))
		So(err, ShouldBeNil)

		Convey("When the lock is taken", func() {
			unlock, lockErr := h.Lock(context.Background())

			Convey("Then the state dir and lock file carry the spec'd modes", func() {
				So(lockErr, ShouldBeNil)
				assertMode(t, h.StateDir(), 0o700)
				assertMode(t, h.FileLockPath(), 0o600)
			})

			Convey("Then a second lock fails within the context deadline", func() {
				So(lockErr, ShouldBeNil)

				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer cancel()

				_, secondErr := h.Lock(ctx)
				assertLockedErr(t, secondErr, context.DeadlineExceeded)
			})

			Convey("Then Unlock is idempotent and the lock can be retaken", func() {
				So(lockErr, ShouldBeNil)
				So(unlock(), ShouldBeNil)
				So(unlock(), ShouldBeNil)

				again, againErr := h.Lock(context.Background())
				So(againErr, ShouldBeNil)
				So(again(), ShouldBeNil)
			})
		})

		Convey("When a stale lock file exists without a holder", func() {
			So(h.Ensure(), ShouldBeNil)
			So(os.WriteFile(h.FileLockPath(), []byte("stale bytes"), 0o600), ShouldBeNil)

			unlock, lockErr := h.Lock(context.Background())

			Convey("Then the lock is granted over the stale file", func() {
				So(lockErr, ShouldBeNil)
				So(unlock(), ShouldBeNil)
			})
		})
	})
}

func TestLockHoldsRealFlock(t *testing.T) {
	Convey("Given a home lock held by this process", t, func() {
		h, err := New(filepath.Join(t.TempDir(), "verger"))
		So(err, ShouldBeNil)

		unlock, err := h.Lock(context.Background())
		So(err, ShouldBeNil)

		Convey("When an external gofrs/flock handle probes the lock file", func() {
			probe := flock.New(h.FileLockPath())

			locked, probeErr := probe.TryLock()
			So(probeErr, ShouldBeNil)

			Convey("Then it sees the lock held and can take it after release", func() {
				So(locked, ShouldBeFalse)

				So(unlock(), ShouldBeNil)

				free, freeErr := probe.TryLock()
				So(freeErr, ShouldBeNil)
				So(free, ShouldBeTrue)
				So(probe.Unlock(), ShouldBeNil)
			})
		})
	})
}

func TestLockCanceledContext(t *testing.T) {
	Convey("Given a home", t, func() {
		h, err := New(filepath.Join(t.TempDir(), "verger"))
		So(err, ShouldBeNil)

		Convey("When the context is already canceled", func() {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			_, lockErr := h.Lock(ctx)

			Convey("Then it reports LockedError wrapping context.Canceled", func() {
				assertLockedErr(t, lockErr, context.Canceled)
			})
		})
	})
}

func TestLockRaceGoroutines(t *testing.T) {
	Convey("Given a home", t, func() {
		h, err := New(filepath.Join(t.TempDir(), "verger"))
		So(err, ShouldBeNil)

		Convey("When two goroutines race for the lock", func() {
			var (
				wg       sync.WaitGroup
				mu       sync.Mutex
				unlocks  []Unlock
				failures int
			)

			for range 2 {
				wg.Go(func() {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()

					unlock, err := h.Lock(ctx)

					mu.Lock()
					defer mu.Unlock()

					if err != nil {
						failures++

						return
					}

					unlocks = append(unlocks, unlock)
				})
			}

			wg.Wait()

			Convey("Then exactly one wins and the loser gets LockedError", func() {
				So(unlocks, ShouldHaveLength, 1)
				So(failures, ShouldEqual, 1)
				So(unlocks[0](), ShouldBeNil)
			})
		})
	})
}

func TestLockTwoProcesses(t *testing.T) {
	Convey("Given a helper process holding the lock", t, func() {
		root := filepath.Join(t.TempDir(), "verger")

		helper := startLockHelper(t, root)

		h, err := New(root)
		So(err, ShouldBeNil)

		Convey("When this process tries to lock", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()

			_, lockErr := h.Lock(ctx)

			Convey("Then it reports LockedError and succeeds after the helper releases", func() {
				assertLockedErr(t, lockErr, context.DeadlineExceeded)

				helper.release(t)

				unlock, retryErr := h.Lock(context.Background())
				So(retryErr, ShouldBeNil)
				So(unlock(), ShouldBeNil)
			})
		})
	})
}

func TestLockStaleAfterProcessDeath(t *testing.T) {
	Convey("Given a helper process killed while holding the lock", t, func() {
		root := filepath.Join(t.TempDir(), "verger")

		helper := startLockHelper(t, root)
		helper.kill(t)

		h, err := New(root)
		So(err, ShouldBeNil)

		Convey("When this process locks the same home", func() {
			unlock, lockErr := h.Lock(context.Background())

			Convey("Then the kernel released the dead holder's lock", func() {
				So(lockErr, ShouldBeNil)
				So(unlock(), ShouldBeNil)
			})
		})
	})
}
