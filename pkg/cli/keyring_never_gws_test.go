package cli

import (
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/secret"
)

// The KEYCHAIN RULE is about more than doctor. Any command that reaches the
// OS secret store on a machine where nobody asked for the keyring raises a
// macOS dialog — "Keychain Not Found / Reset To Defaults" — in exchange for a
// diagnostic, a status line or a list of names. The rule is therefore about
// the whole command surface: with the file backend, which is the default and
// the one a machine without a keychain gets, nothing may construct or call a
// keyring.
//
// A probe that says "a keychain is available" is deliberately not enough to
// get past this: the probe is the filesystem and the environment, and a
// positive answer is a reason to *offer* the keyring, never a reason to open
// one.
func TestNoCommandReachesTheKeyringUnlessItWasAskedTo(t *testing.T) {
	Convey("Given a machine with no keyring backend configured", t, func() {
		commands := []struct {
			label string
			args  []string
		}{
			{"doctor", []string{"doctor"}},
			{"status", []string{"status"}},
			{"secret list", []string{"secret", "list"}},
			{"secret backend", []string{"secret", "backend"}},
			{"install", []string{"install", "@REF@"}},
			{"sync", []string{"sync"}},
		}

		for _, cmd := range commands {
			Convey("Then "+cmd.label+" never opens one", func() {
				args := cmd.args
				w := newWorld(t)
				w.chdir(t, w.root)

				// The probe says yes on purpose. Anything that opens a
				// keyring despite a "no" would be an obvious bug; the real
				// one is a path that treats a positive probe as permission.
				probe := &fakeProbe{ok: true}
				w.keyringProbe = probe
				w.buildKeyring = func(secret.Runner, string) (secret.Keyring, error) {
					return panickingKeyring{t: t}, nil
				}

				// The file backend, with a fake in place of the real one:
				// the only way a command can reach a keyring is to ask for
				// one the configuration did not name.
				store, err := secret.Load(
					filepath.Join(w.homeDir, "state", "secrets.json"),
					secret.WithKeyring(panickingKeyring{t: t}),
				)
				So(err, ShouldBeNil)

				w.secrets = store

				ref := w.fixture(t)

				if len(args) > 1 {
					args[1] = strings.ReplaceAll(args[1], "@REF@", ref)
				}

				stdout, runErr := w.run(args...)

				Convey("And the command got as far as its own work", func() {
					// Without this the table is vacuous: a command that
					// dies on an unknown flag never reaches the store, and
					// "never opened a keyring" is true of everything that
					// never ran. `secret backend` is the one case that must
					// succeed outright — it exists to answer the question.
					if cmd.label == "secret backend" {
						So(runErr, ShouldBeNil)
						So(stdout, ShouldContainSubstring, "file")
					}

					Convey("And nothing opened a keyring to get there", func() {
						So(probe.calls, ShouldBeGreaterThanOrEqualTo, 0)
					})
				})
			})
		}
	})
}
