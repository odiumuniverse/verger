package cli

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/odiumuniverse/verger/pkg/secret"
	. "github.com/smartystreets/goconvey/convey"
)

// panickingKeyring fails the test if anything reaches it.
//
// The KEYCHAIN RULE is that no test and no diagnostic may open the OS secret
// store. On macOS an isolated HOME makes the keychain look missing, and the OS
// answers with a modal "Keychain Not Found / Reset To Defaults" — an offer to
// wipe the user's keychain, triggered by a tool asking where to keep a string.
// A fake that panics turns that from a system dialog a human has to answer into
// a red line nobody has to be present for.
type panickingKeyring struct {
	t *testing.T
}

func (k panickingKeyring) Get(string) (string, bool, error) {
	k.t.Error("the OS keyring was opened by a Get")

	return "", false, errors.New("unreachable")
}

func (k panickingKeyring) Set(string, string) error {
	k.t.Error("the OS keyring was opened by a Set")

	return errors.New("unreachable")
}

func (k panickingKeyring) Delete(string) (bool, error) {
	k.t.Error("the OS keyring was opened by a Delete")

	return false, errors.New("unreachable")
}

// useKeyringBackend gives the world a secrets store configured for the
// keyring, with a fake in place of the real one. It is the only way a test can
// put verger in the state the F4 finding was about without a real keychain.
func (w *world) useKeyringBackend(t *testing.T, k secret.Keyring) {
	t.Helper()

	store, err := secret.Load(filepath.Join(w.homeDir, "state", "secrets.json"), secret.WithKeyring(k))
	if err != nil {
		t.Fatalf("load secrets store: %v", err)
	}

	if err := store.SetBackend(secret.BackendKeyring); err != nil {
		t.Fatalf("set keyring backend: %v", err)
	}

	w.secrets = store
}

// TestDoctorNeverOpensTheKeychain is the regression test for the F4 finding
// from VERIFY-W8-1: `verger doctor` on a clean HOME reported "keyring probe
// succeeded", which meant it had performed a real Get/Delete round trip against
// the OS keychain and raised a system dialog on the way. A diagnostic has to
// be safe to run at any time, on any machine, with nobody watching.
func TestDoctorNeverOpensTheKeychain(t *testing.T) {
	Convey("Given a machine whose secrets store is the keyring", t, func() {
		w := newWorld(t)
		w.useKeyringBackend(t, panickingKeyring{t: t})

		Convey("When doctor runs", func() {
			_, err := w.run("doctor", "--json")

			Convey("Then it completes without the keyring being opened", func() {
				So(err, ShouldBeNil)
			})
		})
	})
}

// TestDoctorNeverClaimsASuccessfulKeyringProbe pins the wording, because the
// wording is what made the old behaviour look healthy: a green "probe
// succeeded" reads as reassurance, and the price of finding out was a modal
// dialog on a stranger's screen.
func TestDoctorNeverClaimsASuccessfulKeyringProbe(t *testing.T) {
	Convey("Given a machine whose secrets store is the keyring", t, func() {
		w := newWorld(t)
		w.useKeyringBackend(t, panickingKeyring{t: t})
		w.keyringWorld(t, true, "")

		Convey("When doctor reports", func() {
			stdout, err := w.run("doctor", "--json")
			So(err, ShouldBeNil)

			Convey("Then it names the backend and the availability", func() {
				// The wording matters: a green "probe succeeded" reads as
				// reassurance, and the price of finding out was a modal
				// dialog on a stranger's screen.
				So(stdout, ShouldNotContainSubstring, "keyring probe succeeded")
				So(stdout, ShouldContainSubstring, "secrets backend: keyring")
				So(stdout, ShouldContainSubstring, "a keychain is available here")
			})
		})
	})
}

// TestDoctorReportsAnUnavailableKeychainWithoutOpeningIt is the case the
// earlier fix could not reach: the store wants the keyring, and there is no
// keychain here. The answer has to come from the non-interactive probe — the
// one that reads the filesystem and the environment — because the store's own
// probe is a Get/Delete round trip that raises a macOS dialog.
func TestDoctorReportsAnUnavailableKeychainWithoutOpeningIt(t *testing.T) {
	Convey("Given a keyring-configured store on a machine with no keychain", t, func() {
		w := newWorld(t)
		w.useKeyringBackend(t, panickingKeyring{t: t})
		w.keyringWorld(t, false, "no secret service in this environment")

		Convey("When doctor runs", func() {
			stdout, err := w.run("doctor", "--json")

			Convey("Then it completes and says why there is no keychain", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "secrets backend: keyring")
				So(stdout, ShouldContainSubstring, "no keychain here")
				So(stdout, ShouldContainSubstring, "no secret service in this environment")
			})
		})
	})
}

// TestDoctorReportsTheFileBackendWithoutProbing is the ordinary case. A file
// store involves no keychain at all, and the check should say so plainly
// rather than emit a success message that sounds like a keychain round trip.
func TestDoctorReportsTheFileBackendWithoutProbing(t *testing.T) {
	Convey("Given a fresh machine with the default file store", t, func() {
		w := newWorld(t)

		Convey("When doctor runs", func() {
			stdout, err := w.run("doctor", "--json")
			So(err, ShouldBeNil)

			Convey("Then it reports the backend and claims no keychain", func() {
				So(stdout, ShouldContainSubstring, "secrets backend: file")
				So(stdout, ShouldContainSubstring, "no keychain involved")
			})
		})
	})
}

// TestDoctorKeyringCheckIsNeverAnError keeps the change honest in the other
// direction. A configured keyring is not a defect: the check is informational,
// and turning "we will not probe this" into a warning would train people to
// ignore the section.
func TestDoctorKeyringCheckIsNeverAnError(t *testing.T) {
	Convey("Given a keyring-configured store", t, func() {
		w := newWorld(t)
		w.useKeyringBackend(t, panickingKeyring{t: t})

		Convey("When doctor runs with --json", func() {
			stdout, err := w.run("doctor", "--json")
			So(err, ShouldBeNil)

			Convey("Then the keyring finding is informational, not an error", func() {
				So(stdout, ShouldNotContainSubstring, `"subject":"keyring","message":"`+`secrets store unavailable`)
			})
		})
	})
}
