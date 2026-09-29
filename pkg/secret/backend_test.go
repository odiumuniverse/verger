package secret_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/secret"
)

// fakeProbe answers from a fixed verdict. Every backend test goes through one
// of these and through a fake keyring: the KEYCHAIN RULE forbids reaching the
// real OS store from a test, and an "available" verdict that happened to be
// true on the developer's laptop would make this suite a keyring test in
// disguise.
type fakeProbe struct {
	ok     bool
	reason string
	calls  int
}

func (p *fakeProbe) Available() (bool, string) {
	p.calls++

	return p.ok, p.reason
}

// fakeKeyring is an in-memory Keyring.
type fakeKeyring struct{ values map[string]string }

func newFakeKeyring() *fakeKeyring { return &fakeKeyring{values: map[string]string{}} }

func (k *fakeKeyring) Get(account string) (string, bool, error) {
	v, ok := k.values[account]

	return v, ok, nil
}

func (k *fakeKeyring) Set(account, value string) error {
	k.values[account] = value

	return nil
}

func (k *fakeKeyring) Delete(account string) (bool, error) {
	_, ok := k.values[account]
	delete(k.values, account)

	return ok, nil
}

// shellBuilder records whether the platform keyring was ever constructed.
// A test that asserts on it is asserting the rule, not the behaviour: no
// probe verdict of "no" may lead to a keyring being built at all.
func shellBuilder(built *bool, kr secret.Keyring) func(secret.Runner, string) (secret.Keyring, error) {
	return func(secret.Runner, string) (secret.Keyring, error) {
		*built = true

		return kr, nil
	}
}

// TestSelectBackendNeverBuildsAKeyringWithoutAProbe pins the rule that
// started all this: a machine with no keyring gets the file store and a
// typed error, and no code path constructs a keyring first "to see".
func TestSelectBackendNeverBuildsAKeyringWithoutAProbe(t *testing.T) {
	Convey("Given a probe that says no keyring is available", t, func() {
		dir := t.TempDir()
		probe := &fakeProbe{ok: false, reason: "no keychain database"}
		built := false

		kr, name, err := secret.SelectBackend(secret.BackendRequest{
			Want:  secret.BackendAuto,
			Dir:   dir,
			Probe: probe,
			Shell: shellBuilder(&built, newFakeKeyring()),
		})

		Convey("Then the file backend is used", func() {
			So(err, ShouldBeNil)
			So(name, ShouldEqual, secret.BackendFile)
			So(kr, ShouldNotBeNil)
		})

		Convey("Then no keyring was ever built", func() {
			So(built, ShouldBeFalse)
		})

		Convey("Then the probe was consulted exactly once", func() {
			So(probe.calls, ShouldEqual, 1)
		})

		Convey("And the file backend really keeps the value", func() {
			So(kr.Set("TOKEN", "s3cret"), ShouldBeNil)

			value, ok, getErr := kr.Get("TOKEN")
			So(getErr, ShouldBeNil)
			So(ok, ShouldBeTrue)
			So(value, ShouldEqual, "s3cret")

			info, statErr := os.Stat(filepath.Join(dir, "TOKEN"))
			So(statErr, ShouldBeNil)
			So(info.Mode().Perm(), ShouldEqual, os.FileMode(0o600))
		})
	})
}

// TestSelectBackendKeyringRequestedButAbsent pins the explicit request: the
// user asked for the keyring, so falling back to a file behind their back
// would be worse than refusing. The refusal is typed and it names the way out.
func TestSelectBackendKeyringRequestedButAbsent(t *testing.T) {
	Convey("Given an explicit keyring request on a machine with none", t, func() {
		dir := t.TempDir()
		built := false

		kr, name, err := secret.SelectBackend(secret.BackendRequest{
			Want:  secret.BackendKeyring,
			Dir:   dir,
			Probe: &fakeProbe{ok: false, reason: "no keychain database"},
			Shell: shellBuilder(&built, newFakeKeyring()),
		})

		Convey("Then it is refused, not silently downgraded", func() {
			So(kr, ShouldBeNil)
			So(name, ShouldBeEmpty)
			So(err, ShouldNotBeNil)
		})

		Convey("Then the error is the typed unavailable one", func() {
			var typed *secret.KeyringUnavailableError
			So(errors.As(err, &typed), ShouldBeTrue)
			So(secret.Unavailable(err), ShouldBeTrue)
		})

		Convey("Then the message names the way out", func() {
			So(err.Error(), ShouldContainSubstring, "verger secret backend file")
		})

		Convey("Then no keyring was built and no file was written", func() {
			So(built, ShouldBeFalse)

			entries, readErr := os.ReadDir(dir)
			So(readErr, ShouldBeNil)
			So(entries, ShouldBeEmpty)
		})
	})
}

// TestSelectBackendUsesKeyringWhenProbed covers the happy path without ever
// naming a real platform tool.
func TestSelectBackendUsesKeyringWhenProbed(t *testing.T) {
	Convey("Given a probe that says a keyring is available", t, func() {
		fake := newFakeKeyring()
		built := false

		kr, name, err := secret.SelectBackend(secret.BackendRequest{
			Want:  secret.BackendAuto,
			Dir:   t.TempDir(),
			Probe: &fakeProbe{ok: true},
			Shell: shellBuilder(&built, fake),
		})

		Convey("Then the keyring is chosen", func() {
			So(err, ShouldBeNil)
			So(name, ShouldEqual, secret.BackendKeyring)
			So(kr, ShouldEqual, secret.Keyring(fake))
			So(built, ShouldBeTrue)
		})
	})
}

// TestSelectBackendRejectsAnUnknownName keeps a typo from becoming a silent
// default: a user who typed "keyringg" asked for something specific.
func TestSelectBackendRejectsAnUnknownName(t *testing.T) {
	Convey("Given a backend name verger does not have", t, func() {
		_, _, err := secret.SelectBackend(secret.BackendRequest{
			Want:  "keyringg",
			Dir:   t.TempDir(),
			Probe: &fakeProbe{ok: true},
		})

		Convey("Then it is rejected and the valid names are named", func() {
			So(err, ShouldNotBeNil)

			_, typed := errors.AsType[*secret.UnknownBackendError](err)
			So(typed, ShouldBeTrue)
			So(err.Error(), ShouldContainSubstring, secret.BackendFile)
			So(err.Error(), ShouldContainSubstring, secret.BackendKeyring)
		})
	})
}

// TestHostKeyringProbeReadsTheRealHome pins the isolated-HOME trap. $HOME is
// the variable a test or a sandbox sets; the account's home is the one the OS
// keychain lives under. Following $HOME is how "is there a keyring" turns
// into a dialog.
func TestHostKeyringProbeReadsTheRealHome(t *testing.T) {
	Convey("Given a HOME that holds a keychain database", t, func() {
		home := t.TempDir()
		dir := filepath.Join(home, "Library", "Keychains")
		So(os.MkdirAll(dir, 0o700), ShouldBeNil)
		So(os.WriteFile(filepath.Join(dir, "login.keychain-db"), []byte("data"), 0o600), ShouldBeNil)
		t.Setenv("HOME", home)

		Convey("Then the darwin probe says yes", func() {
			ok, reason := secret.HostKeyringProbe{GOOS: "darwin", UserHome: home}.Available()
			So(ok, ShouldBeTrue)
			So(reason, ShouldBeEmpty)
		})
	})

	Convey("Given a home with no Keychains directory", t, func() {
		Convey("Then the darwin probe says no, with a reason", func() {
			ok, reason := secret.HostKeyringProbe{GOOS: "darwin", UserHome: t.TempDir()}.Available()
			So(ok, ShouldBeFalse)
			So(reason, ShouldNotBeEmpty)
		})
	})

	Convey("Given a keychain file that exists but is empty", t, func() {
		home := t.TempDir()
		dir := filepath.Join(home, "Library", "Keychains")
		So(os.MkdirAll(dir, 0o700), ShouldBeNil)
		So(os.WriteFile(filepath.Join(dir, "login.keychain-db"), nil, 0o600), ShouldBeNil)
		t.Setenv("HOME", home)

		Convey("Then the probe says no: an empty file is not a keyring", func() {
			ok, _ := secret.HostKeyringProbe{GOOS: "darwin", UserHome: home}.Available()
			So(ok, ShouldBeFalse)
		})
	})

	// An isolated HOME is the case that produced the dialog. The Security
	// framework resolves a keychain through $HOME, so a probe that trusted
	// only the account's real home would answer "yes" and the very next
	// call would look under the sandbox, find nothing, and offer the user a
	// keychain reset. HOME and the account's home have to agree.
	Convey("Given a HOME that is not the account's home", t, func() {
		account := t.TempDir()
		dir := filepath.Join(account, "Library", "Keychains")
		So(os.MkdirAll(dir, 0o700), ShouldBeNil)
		So(os.WriteFile(filepath.Join(dir, "login.keychain-db"), []byte("data"), 0o600), ShouldBeNil)

		Convey("Then the probe refuses, however complete the real home looks", func() {
			t.Setenv("HOME", t.TempDir())

			ok, reason := secret.HostKeyringProbe{GOOS: "darwin", UserHome: account}.Available()
			So(ok, ShouldBeFalse)
			So(reason, ShouldContainSubstring, "not the account's home")
		})

		Convey("Then the typed error is the way out, not a dialog", func() {
			t.Setenv("HOME", t.TempDir())

			err := secret.RequireKeyring(secret.HostKeyringProbe{GOOS: "darwin", UserHome: account})
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "verger secret backend file")
			So(secret.Unavailable(err), ShouldBeTrue)
		})
	})

	Convey("Given HOME unset", t, func() {
		Convey("Then the probe says no rather than guessing a home", func() {
			t.Setenv("HOME", "")

			ok, reason := secret.HostKeyringProbe{GOOS: "darwin", UserHome: t.TempDir()}.Available()
			So(ok, ShouldBeFalse)
			So(reason, ShouldContainSubstring, "HOME")
		})
	})
	Convey("Given a platform with no keyring mapping", t, func() {
		Convey("Then the probe says no rather than guessing", func() {
			ok, reason := secret.HostKeyringProbe{GOOS: "plan9", UserHome: t.TempDir()}.Available()
			So(ok, ShouldBeFalse)
			So(reason, ShouldContainSubstring, "plan9")
		})
	})
}

// TestLinuxProbeNeedsASessionBus covers the headless Linux case: the address
// is often exported while nothing listens, and connecting anyway is what
// wakes a prompt agent.
func TestLinuxProbeNeedsASessionBus(t *testing.T) {
	Convey("Given the linux probe", t, func() {
		probe := secret.HostKeyringProbe{GOOS: "linux", UserHome: t.TempDir()}

		Convey("With no session bus in the environment it says no", func() {
			t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")

			ok, reason := probe.Available()
			So(ok, ShouldBeFalse)
			So(reason, ShouldContainSubstring, "D-Bus")
		})

		Convey("With an address whose socket does not exist it says no", func() {
			t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(t.TempDir(), "absent"))

			ok, reason := probe.Available()
			So(ok, ShouldBeFalse)
			So(reason, ShouldContainSubstring, "socket")
		})

		Convey("With a real socket file present it says yes", func() {
			socket := filepath.Join(t.TempDir(), "bus")
			So(os.WriteFile(socket, nil, 0o600), ShouldBeNil)
			t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+socket)

			ok, _ := probe.Available()
			So(ok, ShouldBeTrue)
		})
	})
}

// TestRequireKeyringErrorShape pins what a caller sees: a typed error whose
// message tells the user what to type instead.
func TestRequireKeyringErrorShape(t *testing.T) {
	Convey("Given a probe with no keyring", t, func() {
		err := secret.RequireKeyring(&fakeProbe{ok: false, reason: "no keychain database"})

		Convey("Then the error is typed and actionable", func() {
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "no keychain available here")
			So(err.Error(), ShouldContainSubstring, "verger secret backend file")
			So(err.Error(), ShouldContainSubstring, "no keychain database")
		})

		Convey("Then the older sentinels still match", func() {
			So(errors.Is(err, secret.ErrKeyringUnavailable), ShouldBeTrue)
			So(errors.Is(err, secret.ErrKeyringUnsupported), ShouldBeTrue)
		})
	})

	Convey("Given a probe with a keyring", t, func() {
		Convey("Then there is no error", func() {
			So(secret.RequireKeyring(&fakeProbe{ok: true}), ShouldBeNil)
		})
	})
}

// The real keyring type must keep satisfying the interface the facade uses;
// this is a compile-time statement, not a test of behaviour.
var _ secret.Keyring = (*secret.FileKeyring)(nil)
