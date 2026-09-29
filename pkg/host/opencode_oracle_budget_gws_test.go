package host_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/odiumuniverse/verger/pkg/host"
	. "github.com/smartystreets/goconvey/convey"
)

// The oracle's own bound is fifteen seconds; a caller may impose a shorter
// one. When it does, the note has to name the time actually waited — a message
// that says "did not answer within 15s" after three seconds of waiting is not
// a rounding error, it sends the reader looking for a fifteen-second stall
// that never happened.
func TestOpenCodeOracleNamesTheBudgetItActuallyWaited(t *testing.T) {
	Convey("Given a caller's deadline shorter than the oracle's own bound", t, func() {
		h := host.NewOpenCode(
			host.WithHome(t.TempDir()),
			host.WithRunner(blockingRunner{}),
			host.WithOracleWait(15*time.Second),
		)

		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()

		start := time.Now()

		_, err := h.Oracle().List(ctx)
		elapsed := time.Since(start)

		Convey("Then the call ends on the caller's deadline", func() {
			So(err, ShouldNotBeNil)
			So(elapsed, ShouldBeLessThan, 5*time.Second)
		})

		Convey("And the note names a wait in the caller's scale, not the oracle's", func() {
			So(err.Error(), ShouldContainSubstring, "did not answer within")
			So(err.Error(), ShouldNotContainSubstring, "15s")
			So(namedWait(err.Error()), ShouldBeGreaterThan, 0)
		})
	})
}

// namedWait reads the duration out of a "did not answer within <d>" note, so
// the assertion is about the number the reader sees rather than about
// formatting.
func namedWait(note string) time.Duration {
	_, rest, ok := strings.Cut(note, "did not answer within ")
	if !ok {
		return 0
	}

	rest = strings.TrimLeft(rest, " ")

	token, _, _ := strings.Cut(rest, " ")
	token, _, _ = strings.Cut(token, ":")
	token, _, _ = strings.Cut(token, ",")

	d, err := time.ParseDuration(token)
	if err != nil {
		return 0
	}

	return d
}
