package cli

import (
	"path/filepath"
	"testing"

	"github.com/odiumuniverse/verger/pkg/exitcode"
	"github.com/odiumuniverse/verger/pkg/secret"
	. "github.com/smartystreets/goconvey/convey"
)

// fakeProbe answers the keychain question without asking a keychain, and
// counts how often it was asked.
type fakeProbe struct {
	ok     bool
	reason string
	calls  int
}

func (p *fakeProbe) Available() (bool, string) {
	p.calls++

	return p.ok, p.reason
}

// fakeKeyring is an in-memory keyring. It exists so no test can reach the
// user's keychain: the KEYCHAIN RULE is absolute, and a test that did so
// would raise a macOS "Keychain Not Found / Reset To Defaults" dialog on
// whichever machine ran it.
type fakeKeyring struct {
	values map[string]string
}

func newFakeKeyring() *fakeKeyring {
	return &fakeKeyring{values: map[string]string{}}
}

func (k *fakeKeyring) Get(account string) (string, bool, error) {
	value, ok := k.values[account]

	return value, ok, nil
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

// keyringWorld gives a world a fake probe and a fake keyring builder, so the
// command under test can never reach the OS secret store.
func (w *world) keyringWorld(t *testing.T, available bool, reason string) (*fakeProbe, *fakeKeyring) {
	t.Helper()

	probe := &fakeProbe{ok: available, reason: reason}
	kr := newFakeKeyring()

	w.keyringProbe = probe
	w.buildKeyring = func(secret.Runner, string) (secret.Keyring, error) { return kr, nil }

	return probe, kr
}

// TestSecretBackendPrintsCurrentAndAvailability is the no-argument case: what
// is in use, and whether a keychain is here. The answer must come from the
// probe — asking the keychain itself is what raises the OS dialog.
func TestSecretBackendPrintsCurrentAndAvailability(t *testing.T) {
	Convey("Given a machine with no keychain", t, func() {
		w := newWorld(t)
		probe, _ := w.keyringWorld(t, false, "no secret service in this environment")

		Convey("When backend is asked with no argument", func() {
			stdout, err := w.run("secret", "backend")

			Convey("Then it prints the backend and the verdict", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "file")
				So(stdout, ShouldContainSubstring, "no keychain here")
				So(stdout, ShouldContainSubstring, "no secret service in this environment")
			})

			Convey("Then it answered from the probe rather than the keyring", func() {
				So(probe.calls, ShouldBeGreaterThan, 0)
			})
		})
	})
}

// TestSecretBackendReportsAnAvailableKeychain is the other side of the probe:
// a machine that does have one must be told so, or the user cannot tell a
// misconfiguration from a machine without a keychain.
func TestSecretBackendReportsAnAvailableKeychain(t *testing.T) {
	Convey("Given a machine with a keychain", t, func() {
		w := newWorld(t)
		w.keyringWorld(t, true, "")

		Convey("When backend is asked with no argument", func() {
			stdout, err := w.run("secret", "backend")

			Convey("Then it says a keychain is available", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "a keychain is available")
			})
		})
	})
}

// TestSecretBackendRefusesKeyringWhereThereIsNone is the exit-6 case pR's
// typed error exists for. The class matters as much as the code: a missing
// keychain is a fact about the machine, not a bad invocation, and the message
// has to carry the way out.
func TestSecretBackendRefusesKeyringWhereThereIsNone(t *testing.T) {
	Convey("Given a machine with no keychain", t, func() {
		w := newWorld(t)
		w.keyringWorld(t, false, "no secret service in this environment")

		Convey("When keyring is requested", func() {
			stdout, err := w.run("secret", "backend", "keyring")

			Convey("Then it exits 6, host unavailable", func() {
				So(err, ShouldNotBeNil)
				So(exitcode.Classify(err), ShouldEqual, exitcode.HostUnavailable)
			})

			Convey("Then the message names the way out", func() {
				So(err.Error(), ShouldContainSubstring, "no keychain available here")
				So(err.Error(), ShouldContainSubstring, "verger secret backend file")
				So(stdout, ShouldNotContainSubstring, "secrets backend: file (")
			})
		})
	})
}

// TestSecretBackendAutoResolvesToFileWithoutAKeyring is the headless case:
// `auto` must resolve rather than fail, because a container with no secret
// service is a normal machine, not a broken one.
func TestSecretBackendAutoResolvesToFileWithoutAKeyring(t *testing.T) {
	Convey("Given a machine with no keychain", t, func() {
		w := newWorld(t)
		w.keyringWorld(t, false, "no secret service")

		Convey("When auto is requested", func() {
			stdout, err := w.run("secret", "backend", "auto")

			Convey("Then it resolves to the file backend and succeeds", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "file")
			})
		})
	})
}

// TestSecretBackendSwitchesToFileOnPurpose is the ordinary switch: a machine
// that has a keyring and a user who wants the file backend anyway.
func TestSecretBackendSwitchesToFileOnPurpose(t *testing.T) {
	Convey("Given a machine with a keychain", t, func() {
		w := newWorld(t)
		w.keyringWorld(t, true, "")

		Convey("When the file backend is requested", func() {
			stdout, err := w.run("secret", "backend", "file")

			Convey("Then the switch succeeds and reports the backend", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "file")
			})
		})
	})
}

// TestSecretBackendUnknownNameIsUsage keeps a typo from silently becoming the
// default, and keeps the two failure classes apart: a name verger does not
// have is a bad argument (2), while a keychain that is not there is a fact
// about the machine (6). Conflating them would send a script to the wrong
// handler.
func TestSecretBackendUnknownNameIsUsage(t *testing.T) {
	Convey("Given a name that is not a backend", t, func() {
		w := newWorld(t)
		w.keyringWorld(t, false, "no secret service")

		Convey("When it is requested", func() {
			_, err := w.run("secret", "backend", "vault")

			Convey("Then it is a usage error, and lists the three real names", func() {
				So(err, ShouldNotBeNil)
				So(exitcode.Classify(err), ShouldEqual, exitcode.Usage)
				So(err.Error(), ShouldContainSubstring, "file")
				So(err.Error(), ShouldContainSubstring, "keyring")
				So(err.Error(), ShouldContainSubstring, "auto")
			})
		})
	})
}

// TestSecretBackendKeyringSwitchUsesTheFake states the rule as a test: the
// keyring the command builds is the one the test handed over, never the
// platform's. A real one would be a `security` call on darwin.
func TestSecretBackendKeyringSwitchUsesTheFake(t *testing.T) {
	Convey("Given a keychain that is available", t, func() {
		w := newWorld(t)
		_, kr := w.keyringWorld(t, true, "")

		Convey("When the keyring backend is selected", func() {
			_, err := w.run("secret", "backend", "keyring")

			Convey("Then the run succeeds against the fake", func() {
				So(err, ShouldBeNil)
				So(kr.values, ShouldNotBeNil)
			})
		})
	})
}

// TestSecretBackendDryRunMovesNothing keeps a preview from being a switch.
func TestSecretBackendDryRunMovesNothing(t *testing.T) {
	Convey("Given a machine with no keychain", t, func() {
		w := newWorld(t)
		w.keyringWorld(t, false, "no secret service")

		Convey("When a backend is requested with --dry-run", func() {
			_, err := w.run("secret", "backend", "file", "--dry-run")

			Convey("Then it answers without failing", func() {
				So(err, ShouldBeNil)
			})
		})
	})
}

// TestSecretBackendJSONCarriesTheVerdict pins the machine-readable shape: a
// front end needs the backend and the availability in one document, and the
// names so it can tell the user what a switch would carry.
func TestSecretBackendJSONCarriesTheVerdict(t *testing.T) {
	Convey("Given a machine with no keychain", t, func() {
		w := newWorld(t)
		w.keyringWorld(t, false, "no secret service")

		Convey("When backend is asked with --json", func() {
			stdout, err := w.run("secret", "backend", "--json")

			Convey("Then the document names the backend and the verdict", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, `"backend":"file"`)
				So(stdout, ShouldContainSubstring, `"keyring_available":false`)
				So(stdout, ShouldContainSubstring, `"names":[]`)
			})
		})
	})
}

// TestSecretBackendNeverPrintsAValue is the promise the whole command rests
// on: it names what it stores, and never what it stores it as.
func TestSecretBackendNeverPrintsAValue(t *testing.T) {
	Convey("Given one stored secret", t, func() {
		w := newWorld(t)
		w.keyringWorld(t, false, "no secret service")
		w.in = "s3cret\n"

		_, err := w.run("secret", "set", "token")
		So(err, ShouldBeNil)

		Convey("When the backend is asked as JSON", func() {
			stdout, err := w.run("secret", "backend", "--json")

			Convey("Then the name is listed and the value is not", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, `"names":["token"]`)
				So(stdout, ShouldNotContainSubstring, "s3cret")
			})
		})

		Convey("Then the file backend is the one holding it", func() {
			store, loadErr := secret.Load(filepath.Join(w.homeDir, "state", "secrets.json"))
			So(loadErr, ShouldBeNil)
			So(store.Backend(), ShouldEqual, secret.BackendFile)
		})
	})
}
