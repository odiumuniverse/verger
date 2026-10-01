package home

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// A directory that is there but empty is not a home.
//
// This is the shape `beadle plugins eject` leaves behind: the state moves to
// ~/.verger, and ~/.beadle/verger stays behind as an empty directory. Discovery
// used to take the empty directory for a vault home, so every later command ran
// against a home with nothing in it and reported no cells — the CLI answering
// "nothing to do" about a machine that had plenty installed elsewhere.
//
// The rule now is the one that was always intended: a vault home counts only if
// it actually CONTAINS a home. The vault existing is not enough; something of
// verger's has to be in it.

// markers writes the two things that mean "this directory is a verger home".
// Either one is enough: a fresh home has one or the other, not both.
func markers(t *testing.T, home string) {
	t.Helper()

	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", home, err)
	}
}

func writeSpec(t *testing.T, home string) {
	t.Helper()

	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", home, err)
	}

	if err := os.WriteFile(filepath.Join(home, "verger.toml"), []byte("# spec\n"), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
}

func writeState(t *testing.T, home string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(home, "state"), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", home, err)
	}

	path := filepath.Join(home, "state", "receipts")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write receipts: %v", err)
	}
}

// aHome is a discovered home laid out beside a user home, the way a real
// machine has them after an eject.
func aHome(t *testing.T, userHome string) (vaultHome, defaultHome string) {
	t.Helper()

	vaultHome = filepath.Join(userHome, BeadleDirName, BeadleSubdir)
	defaultHome = filepath.Join(userHome, DirName)

	return vaultHome, defaultHome
}

func TestAnEmptiedVaultDirectoryIsNotAHome(t *testing.T) {
	Convey("Given a user home whose ~/.beadle/verger is an empty directory", t, func() {
		userHome := t.TempDir()
		vaultHome, defaultHome := aHome(t, userHome)

		markers(t, vaultHome)
		writeSpec(t, defaultHome)

		Convey("Then ~/.verger is chosen, not the empty vault", func() {
			h, err := Discover(WithEnv(envMap(nil)), WithUserHome(userHome))

			So(err, ShouldBeNil)
			So(h.Root(), ShouldEqual, defaultHome)
			So(h.Source(), ShouldEqual, SourceDefault)
		})
	})

	Convey("Given a vault home holding only state/", t, func() {
		userHome := t.TempDir()
		vaultHome, _ := aHome(t, userHome)

		writeState(t, vaultHome)

		Convey("Then the vault is chosen", func() {
			h, err := Discover(WithEnv(envMap(nil)), WithUserHome(userHome))

			So(err, ShouldBeNil)
			So(h.Root(), ShouldEqual, vaultHome)
			So(h.Source(), ShouldEqual, SourceBeadleVault)
		})
	})

	Convey("Given a vault home holding only verger.toml", t, func() {
		userHome := t.TempDir()
		vaultHome, _ := aHome(t, userHome)

		writeSpec(t, vaultHome)

		Convey("Then the vault is chosen", func() {
			h, err := Discover(WithEnv(envMap(nil)), WithUserHome(userHome))

			So(err, ShouldBeNil)
			So(h.Root(), ShouldEqual, vaultHome)
			So(h.Source(), ShouldEqual, SourceBeadleVault)
		})
	})
}

// Two real homes on one machine is the state this product refuses to create,
// because two locks and two receipt trees stop agreeing. The vault still wins,
// and the other one is reported rather than silently ignored.
func TestTwoRealHomesReportTheOtherOne(t *testing.T) {
	Convey("Given both the vault home and ~/.verger holding a home", t, func() {
		userHome := t.TempDir()
		vaultHome, defaultHome := aHome(t, userHome)

		writeSpec(t, vaultHome)
		writeState(t, defaultHome)

		Convey("Then the vault is chosen, as it always was", func() {
			h, err := Discover(WithEnv(envMap(nil)), WithUserHome(userHome))

			So(err, ShouldBeNil)
			So(h.Root(), ShouldEqual, vaultHome)
			So(h.Source(), ShouldEqual, SourceBeadleVault)
		})

		Convey("Then the other home is reported, so a user can be told", func() {
			other, found := CompetingHome(WithEnv(envMap(nil)), WithUserHome(userHome))

			So(found, ShouldBeTrue)
			So(other, ShouldEqual, defaultHome)
		})
	})

	Convey("Given only the vault home", t, func() {
		userHome := t.TempDir()
		vaultHome, _ := aHome(t, userHome)

		writeSpec(t, vaultHome)

		Convey("Then there is nothing to report", func() {
			_, found := CompetingHome(WithEnv(envMap(nil)), WithUserHome(userHome))

			So(found, ShouldBeFalse)
		})
	})

	Convey("Given an emptied vault directory and a real ~/.verger", t, func() {
		userHome := t.TempDir()
		vaultHome, _ := aHome(t, userHome)

		markers(t, vaultHome)
		writeSpec(t, filepath.Join(userHome, DirName))

		Convey("Then the emptied directory is not reported as a second home", func() {
			// It is not a home, so warning about it would be a false alarm about
			// a directory that nothing is going to use.
			_, found := CompetingHome(WithEnv(envMap(nil)), WithUserHome(userHome))

			So(found, ShouldBeFalse)
		})
	})
}

// The scenario as reported: beadle's plugins eject moves the home to ~/.verger
// and leaves ~/.beadle/verger empty, and every verger command afterwards answers
// "no cells". End to end from the eject's shape, not from a unit of Discover.
func TestAfterAPluginsEjectTheHomeIsNotTheEmptiedDirectory(t *testing.T) {
	Convey("Given a machine where plugins eject has moved the home", t, func() {
		userHome := t.TempDir()
		vaultHome, defaultHome := aHome(t, userHome)

		// Before: one real home in the vault.
		writeSpec(t, vaultHome)

		before, err := Discover(WithEnv(envMap(nil)), WithUserHome(userHome))
		So(err, ShouldBeNil)
		So(before.Root(), ShouldEqual, vaultHome)

		// The eject: state moves to ~/.verger, and the old directory is left
		// behind rather than removed.
		So(os.MkdirAll(defaultHome, 0o700), ShouldBeNil)
		So(os.WriteFile(filepath.Join(defaultHome, "verger.toml"), []byte("# spec\n"), 0o600), ShouldBeNil)
		So(os.RemoveAll(filepath.Join(vaultHome, "verger.toml")), ShouldBeNil)
		So(os.RemoveAll(filepath.Join(vaultHome, "state")), ShouldBeNil)

		Convey("Then every later command resolves to the home that has state", func() {
			after, err := Discover(WithEnv(envMap(nil)), WithUserHome(userHome))

			So(err, ShouldBeNil)
			So(after.Root(), ShouldEqual, defaultHome)
			So(after.Source(), ShouldEqual, SourceDefault)

			Convey("And the directory left behind is still there, untouched", func() {
				// Discovery stops claiming it; it does not clean up after
				// another tool.
				info, statErr := os.Stat(vaultHome)
				So(statErr, ShouldBeNil)
				So(info.IsDir(), ShouldBeTrue)
			})
		})
	})
}
