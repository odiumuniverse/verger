package secret_test

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/secret"
)

// TestFileKeyringRoundTrip pins the backend a headless machine has to use:
// a CI runner has no keyring daemon, and a secret store that refuses to work
// there is a secret store nobody can use unattended.
func TestFileKeyringRoundTrip(t *testing.T) {
	Convey("Given a file-backed keyring in an empty directory", t, func() {
		dir := t.TempDir()
		kr, err := secret.NewFileKeyring(dir)
		So(err, ShouldBeNil)

		Convey("Then an unknown account is absent, not an error", func() {
			value, ok, getErr := kr.Get("TOKEN")
			So(getErr, ShouldBeNil)
			So(ok, ShouldBeFalse)
			So(value, ShouldBeEmpty)
		})

		Convey("Then a value round-trips", func() {
			So(kr.Set("TOKEN", "s3cret"), ShouldBeNil)

			value, ok, getErr := kr.Get("TOKEN")
			So(getErr, ShouldBeNil)
			So(ok, ShouldBeTrue)
			So(value, ShouldEqual, "s3cret")
		})

		Convey("Then the value is not world-readable", func() {
			So(kr.Set("TOKEN", "s3cret"), ShouldBeNil)

			// A secret readable by another user on a shared build box is not
			// stored, it is published. 0600 is the whole point of choosing
			// this backend over an env var.
			path, pathErr := kr.Path("TOKEN")
			So(pathErr, ShouldBeNil)

			info, statErr := os.Stat(path)
			So(statErr, ShouldBeNil)
			So(info.Mode().Perm(), ShouldEqual, os.FileMode(0o600))
		})

		Convey("Then Set replaces rather than appends", func() {
			So(kr.Set("TOKEN", "one"), ShouldBeNil)
			So(kr.Set("TOKEN", "two"), ShouldBeNil)

			value, ok, getErr := kr.Get("TOKEN")
			So(getErr, ShouldBeNil)
			So(ok, ShouldBeTrue)
			So(value, ShouldEqual, "two")
		})

		Convey("Then Delete removes the value and reports it", func() {
			So(kr.Set("TOKEN", "s3cret"), ShouldBeNil)

			gone, delErr := kr.Delete("TOKEN")
			So(delErr, ShouldBeNil)
			So(gone, ShouldBeTrue)

			_, ok, getErr := kr.Get("TOKEN")
			So(getErr, ShouldBeNil)
			So(ok, ShouldBeFalse)
		})

		Convey("Then deleting an absent value says so instead of failing", func() {
			gone, delErr := kr.Delete("TOKEN")
			So(delErr, ShouldBeNil)
			So(gone, ShouldBeFalse)
		})
	})
}

// TestFileKeyringRejectsAnEscapingAccount pins that an account name is a name,
// not a path. A name that climbs out of the store would let `verger secret
// set` write anywhere the user can, which is how a helper becomes a backdoor.
func TestFileKeyringRejectsAnEscapingAccount(t *testing.T) {
	Convey("Given a file keyring and an account name with a path separator", t, func() {
		kr, err := secret.NewFileKeyring(t.TempDir())
		So(err, ShouldBeNil)

		Convey("Then it is rejected rather than resolved", func() {
			So(kr.Set("../escape", "x"), ShouldNotBeNil)
			So(kr.Set("nested/name", "x"), ShouldNotBeNil)

			_, statErr := os.Stat(filepath.Join(t.TempDir(), "escape"))
			So(os.IsNotExist(statErr), ShouldBeTrue)
		})
	})
}
