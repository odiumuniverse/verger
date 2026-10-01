package cli

import (
	"errors"
	"fmt"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/exitcode"
)

// Four error types in this file carry their own exit class, because the
// classifier cannot import this package: it sits ABOVE the facade, and a
// classifier that imported it would be a cycle waiting for the first command
// that calls Classify itself. The four say so on the type instead.
//
// Three of them had no test. A class that is only asserted to be "not the
// unexpected one" is not pinned: `LockedError` returning HostUnavailable instead
// of Policy would pass such a check and tell a script the wrong thing — that a
// rule refusal is a host that could not be reached. So each one is pinned to its
// own class here, by name, and the wrong answer fails.
//
// The wrapping matters as much as the class. Every command wraps on the way out,
// so a class found only on a bare error is a class no script will ever see.
// That is the same trap the beadle hands-off mapping walked into, and this table
// walks it deliberately.
func TestEachCLICarriesItsOwnExitClass(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"a feature this phase does not have", &NotAvailableError{Feature: "restore", Hint: "run later"}, exitcode.HostUnavailable},
		{"a rule refused the write", &LockedError{Reason: "rule 7"}, exitcode.Policy},
		{"a project waiting to be trusted", &TrustError{Path: "/p", Cause: errors.New("no")}, exitcode.Consent},
	}

	for _, tc := range cases {
		Convey("Given "+tc.name, t, func() {
			for _, shape := range []struct {
				name string
				err  error
			}{
				{"bare", tc.err},
				{"wrapped by fmt", fmt.Errorf("run: %w", tc.err)},
				{"wrapped twice", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", tc.err))},
				{"joined with another error", errors.Join(errors.New("second"), tc.err)},
				{"joined the other way round", errors.Join(tc.err, errors.New("second"))},
			} {
				Convey("Then "+shape.name+" is "+exitcode.Name(tc.want), func() {
					So(exitcode.Classify(shape.err), ShouldEqual, tc.want)
					So(exitcode.Classify(shape.err), ShouldNotEqual, exitcode.Unexpected)
				})
			}
		})
	}

	Convey("Given the three classes are all distinct answers", t, func() {
		// The table would still be "correct" if all three answered the same
		// code, and a reader would have no way to tell that from the rows
		// agreeing. Asserting the classes differ says the distinctions are
		// real ones and not three copies of one decision.
		Convey("Then a user is told three different things", func() {
			seen := map[int]bool{}

			for _, tc := range cases {
				seen[tc.want] = true
			}

			So(len(seen), ShouldEqual, 3)
		})
	})
}
