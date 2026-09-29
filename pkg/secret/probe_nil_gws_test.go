package secret_test

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/secret"
)

// A nil probe is the shape a caller reaches by wiring nothing: an unset
// option, a host with no keyring support, a front end that never built one.
// Asking such a caller whether it is a usage error is one branch; panicking
// on the way to the answer is a crash in a diagnostic, and a keyring is
// exactly the kind of subsystem a tool is allowed not to have.
func TestRequireKeyringHandlesAMissingProbe(t *testing.T) {
	Convey("Given no probe at all", t, func() {
		Convey("Then asking is a refusal, not a crash", func() {
			var probe secret.KeyringProbe

			So(func() { _ = secret.RequireKeyring(probe) }, ShouldNotPanic)
		})

		Convey("And the answer is the typed no-keyring error", func() {
			err := secret.RequireKeyring(nil)

			So(err, ShouldNotBeNil)
			So(secret.Unavailable(err), ShouldBeTrue)
			So(err.Error(), ShouldContainSubstring, "no keychain available here")
		})
	})
}
