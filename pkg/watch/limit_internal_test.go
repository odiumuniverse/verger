package watch

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

// wrap is how a platform error reaches the engine wrapped in context.
func wrap(err error) error { return fmt.Errorf("watch: add %s: %w", "/h/.cursor", err) }

// Finding 3: the typed watch-limit error had no live coverage. The verifier's
// mutation M5 — `isLimit` returning false unconditionally — left the whole
// suite green, because the only test built the error by hand and never went
// through the code that decides an error IS a limit.
//
// These tests are internal (package watch) for one reason: the classification
// lives behind runner.addWatch, and driving it through the public Run needs a
// real exhausted inotify budget — about a million registrations. The
// addWatchFunc seam makes the platform's own ENOSPC drive the real path.

func newSeamRunner(add func(string) error) *runner {
	return &runner{
		cfg:          config{debounce: DefaultDebounce, clock: time.Now},
		owners:       map[string]map[string]bool{},
		pending:      map[string]map[string]bool{},
		due:          map[string]time.Time{},
		addWatchFunc: add,
	}
}

func TestAddWatchTypesARealENOSPC(t *testing.T) {
	Convey("Given a runner whose platform add fails with a real ENOSPC", t, func() {
		// the kernel error itself, not a lookalike message: isLimit matches
		// ENOSPC, and a hand-written "no space left on device" string is not it.
		boom := syscall.ENOSPC
		r := newSeamRunner(func(string) error { return boom })

		Convey("Then the failure comes back typed, naming the path and the limit", func() {
			err := r.addWatch("claude", "/h/.cursor")

			var limit *LimitError

			So(errors.As(err, &limit), ShouldBeTrue)
			So(limit.Path, ShouldEqual, "/h/.cursor")
			So(limit.Op, ShouldEqual, "add")
			So(errors.Is(limit, boom), ShouldBeTrue)
			So(limit.Error(), ShouldContainSubstring, "fs.inotify.max_user_watches")
			So(limit.Error(), ShouldContainSubstring, "narrow the target paths")
		})

		Convey("Then the path is not recorded as watched", func() {
			_ = r.addWatch("claude", "/h/.cursor")

			_, watched := r.owners["/h/.cursor"]
			So(watched, ShouldBeFalse)
		})
	})
}

// The mutation the verifier could not kill: with isLimit neutered, the error
// above degrades to a plain wrapped error and this goes red.
func TestANonLimitErrorIsNotTypedAsALimit(t *testing.T) {
	Convey("Given a runner whose platform add fails with something else", t, func() {
		perm := errors.New("permission denied")
		r := newSeamRunner(func(string) error { return perm })

		Convey("Then the failure is a plain wrapped error, not a LimitError", func() {
			err := r.addWatch("claude", "/h/.cursor")

			var limit *LimitError

			So(errors.As(err, &limit), ShouldBeFalse)
			So(errors.Is(err, perm), ShouldBeTrue)
		})
	})
}

// The path that a hand-made error never covered: a wrapped ENOSPC, which is
// what the kernel actually produces through fsnotify.
func TestWrappedENOSPCIsStillTypedAsALimit(t *testing.T) {
	Convey("Given a wrapped platform ENOSPC", t, func() {
		So(isLimit(wrap(syscall.ENOSPC)), ShouldBeTrue)
		So(isLimit(syscall.ENOSPC), ShouldBeTrue)

		Convey("Then a near-miss is not a limit", func() {
			So(isLimit(syscall.EMFILE), ShouldBeFalse)
			So(isLimit(syscall.EACCES), ShouldBeFalse)
			So(isLimit(errors.New("no space left on device")), ShouldBeFalse)
		})
	})
}

func TestAlreadyWatchedPathIsNotAddedTwice(t *testing.T) {
	Convey("Given a path already owned by another host", t, func() {
		calls := 0
		r := newSeamRunner(func(string) error { calls++; return nil })
		So(r.addWatch("claude", "/h/.cursor"), ShouldBeNil)

		Convey("Then a second host is recorded without a second platform add", func() {
			So(r.addWatch("omp", "/h/.cursor"), ShouldBeNil)
			So(calls, ShouldEqual, 1)
			So(r.owners["/h/.cursor"]["omp"], ShouldBeTrue)
		})
	})
}
