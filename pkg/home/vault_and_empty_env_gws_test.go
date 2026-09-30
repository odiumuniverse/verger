package home

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// TestAVaultWithoutAVergerSubdirIsStillTheHome is the second-home rule.
//
// vaultAt required <beadle>/verger to already EXIST before it would be chosen.
// On a fresh vault — one beadle has just created — it does not, so discovery fell
// through to ~/.verger and the first install created a second home beside the
// vault. That is precisely the state this product refuses to create: two homes
// on one machine, each with its own lock, so the beadle vault and the CLI stop
// agreeing about what is installed.
//
// Discovery is read-only, so pointing at a directory that does not exist yet is
// not a write and not a risk. The vault existing is the signal; the subdir inside
// it is verger's to create.
func TestAVaultWithoutAVergerSubdirIsStillTheHome(t *testing.T) {
	Convey("Given a beadle vault with no verger subdir yet", t, func() {
		root := t.TempDir()
		vault := filepath.Join(root, "beadle")
		So(os.MkdirAll(vault, 0o700), ShouldBeNil)

		So(dirExists(t, filepath.Join(vault, "verger")), ShouldBeFalse)

		Convey("When the home is discovered with that vault set", func() {
			found, err := Discover(
				WithEnv(envMap(map[string]string{EnvBeadleHome: vault})),
				WithUserHome(filepath.Join(root, "user")),
			)

			Convey("Then it is inside the vault", func() {
				So(err, ShouldBeNil)
				So(found.Root(), ShouldEqual, filepath.Join(vault, "verger"))
				So(found.Source(), ShouldEqual, SourceBeadleVault)
			})

			Convey("And the default home is never chosen beside it", func() {
				// The failure was not a wrong path; it was a second home being
				// created. This is the assertion that names it.
				So(dirExists(t, filepath.Join(root, "user", ".verger")), ShouldBeFalse)
			})
		})
	})

	Convey("Given no vault at all", t, func() {
		root := t.TempDir()

		Convey("Then discovery still finds the default home", func() {
			found, err := Discover(
				WithEnv(envMap(nil)),
				WithUserHome(root),
			)

			So(err, ShouldBeNil)
			So(found.Source(), ShouldEqual, SourceDefault)
			So(found.Root(), ShouldEqual, filepath.Join(root, ".verger"))
		})
	})
}

// TestAnEmptyEnvironmentVariableMeansUnset follows the XDG Base Directory
// specification, which the hosts implement: an empty value is indistinguishable
// from an unset one, so a caller that exports FOO= has not asked for anything.
//
// The gate sets XDG_CONFIG_HOME= (empty) while isolating a home, and a resolver
// that read "set" would look for a config root of "" — a relative path that
// resolves against whichever directory the user happened to be in.
func TestAnEmptyEnvironmentVariableMeansUnset(t *testing.T) {
	Convey("Given every variable that can name a home or a root", t, func() {
		for _, name := range []string{
			"VERGER_HOME", "BEADLE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
			"XDG_CACHE_HOME", "XDG_STATE_HOME",
		} {
			Convey("When "+name+" is set to the empty string", func() {
				root := t.TempDir()
				vault := filepath.Join(root, "beadle")
				So(os.MkdirAll(vault, 0o700), ShouldBeNil)

				found, err := Discover(
					WithEnv(envMap(map[string]string{name: ""})),
					WithUserHome(root),
				)

				Convey("Then it is treated as unset, not as a request for ''", func() {
					So(err, ShouldBeNil)
					// Never a relative path: "" would resolve against the cwd.
					So(filepath.IsAbs(found.Root()), ShouldBeTrue)
					So(found.Root(), ShouldNotEqual, "")
					So(found.Root(), ShouldEqual, filepath.Join(root, ".verger"))
				})
			})
		}
	})
}

// dirExists reports whether path is an existing directory.
func dirExists(t *testing.T, path string) bool {
	t.Helper()

	info, err := os.Stat(path)

	return err == nil && info.IsDir()
}

// TestTheDiscoveryLadderWithAnEmptyHomeVariable pins the whole ladder from
// DESIGN with the top rung EXPORTED BUT EMPTY, which is the state a user reaches
// by accident far more often than by intent:
//
//	$VERGER_HOME  ->  ~/.beadle/verger  ->  ~/.verger
//
// An empty first rung is a rung that is not taken. A resolver that reads "set"
// and asks "set to what?" lands on the empty string, and every path built from it
// is RELATIVE - it resolves against whichever directory the process happened to
// start in, which for a user is their project.
//
// The middle rung is the one that was untested: with no ~/.beadle present, an
// empty VERGER_HOME and an absent one look identical, so a resolver that dropped
// the vault entirely would still pass. The vault has to be ON DISK for this to
// say anything, which is why every case below builds one.
func TestTheDiscoveryLadderWithAnEmptyHomeVariable(t *testing.T) {
	Convey("Given a machine that has a beadle vault in the user's home", t, func() {
		userHome := t.TempDir()
		vault := filepath.Join(userHome, BeadleDirName)

		So(os.MkdirAll(vault, 0o700), ShouldBeNil)

		Convey("When VERGER_HOME is exported but empty", func() {
			found, err := Discover(
				WithEnv(envMap(map[string]string{EnvHome: ""})),
				WithUserHome(userHome),
			)

			Convey("Then the ladder falls to the vault, not to ~/.verger", func() {
				So(err, ShouldBeNil)
				So(found.Source(), ShouldEqual, SourceBeadleVault)
				So(found.Root(), ShouldEqual, filepath.Join(vault, BeadleSubdir))
			})
		})

		Convey("When VERGER_HOME holds only whitespace", func() {
			found, err := Discover(
				WithEnv(envMap(map[string]string{EnvHome: "  \t "})),
				WithUserHome(userHome),
			)

			Convey("Then whitespace is empty too", func() {
				// A padded value is a value, so `VERGER_HOME=" $HOME/verger "` is
				// a legitimate path. What is NOT legitimate is padding with no
				// path in it, which would make the root the cwd.
				So(err, ShouldBeNil)
				So(found.Source(), ShouldEqual, SourceBeadleVault)
				So(found.Root(), ShouldEqual, filepath.Join(vault, BeadleSubdir))
			})
		})

		Convey("When VERGER_HOME names a real directory", func() {
			explicit := t.TempDir()

			found, err := Discover(
				WithEnv(envMap(map[string]string{EnvHome: explicit})),
				WithUserHome(userHome),
			)

			Convey("Then it wins and the vault is ignored", func() {
				So(err, ShouldBeNil)
				So(found.Source(), ShouldEqual, SourceEnv)
				So(found.Root(), ShouldEqual, explicit)
			})
		})

		Convey("When the machine has no vault at all", func() {
			bare := t.TempDir()

			found, err := Discover(
				WithEnv(envMap(map[string]string{EnvHome: ""})),
				WithUserHome(bare),
			)

			Convey("Then the last rung is ~/.verger", func() {
				So(err, ShouldBeNil)
				So(found.Source(), ShouldEqual, SourceDefault)
				So(found.Root(), ShouldEqual, filepath.Join(bare, DirName))
			})
		})
	})
}
