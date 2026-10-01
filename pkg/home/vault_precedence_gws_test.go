package home

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// pR-27 and beadle pS-17 were both right and together they broke the machine.
//
// pR-27: an emptied vault home is not a home, so discovery stops answering from
// it — because `beadle plugins eject` used to LEAVE an empty directory behind.
//
// pS-17: eject removes that directory instead of leaving it empty.
//
// Put together, pR-27's guard no longer fires. It tests whether the directory is
// present and empty; after pS-17 the directory is ABSENT, so the guard is silent,
// the fresh-machine rule answers for it, and every command then runs against
// $vault/verger — a directory that does not exist — and reports no cells on a
// machine that is full of them.
//
// The rule that survives both is three clauses, and the order is the whole point:
//
//	vault exists → vault home NOT EMPTY  → the vault home
//	             → ~/.verger   NOT EMPTY  → ~/.verger
//	             → otherwise (fresh)      → the vault home
//
// "Not empty", not "holds a marker". A marker check is what made this fragile in
// the first place: it couples discovery to a file layout, so a home whose
// contents move on reads as fresh.
func TestDiscoverPicksTheHomeTheVaultRuleSaysItShould(t *testing.T) {
	cases := []struct {
		name       string
		vaultHome  string // "", "empty" or "full"
		defaultDir string // "", "empty" or "full"
		want       string // "vault" or "default"
	}{
		{
			// Nothing anywhere: a machine that has never run verger. The home
			// belongs beside the vault — a second tree in ~/.verger is the state
			// this product refuses to create.
			name:      "a fresh machine has no home anywhere and gets the vault home",
			vaultHome: "", defaultDir: "",
			want: "vault",
		},
		{
			// pS-17's exact shape: eject moved the state and REMOVED the
			// directory. This is the case pR-27 alone did not cover.
			name:      "an absent vault home with a full ~/.verger keeps ~/.verger",
			vaultHome: "", defaultDir: "full",
			want: "default",
		},
		{
			name:      "an emptied vault home with a full ~/.verger keeps ~/.verger",
			vaultHome: "empty", defaultDir: "full",
			want: "default",
		},
		{
			name:      "a full vault home wins even when ~/.verger is also full",
			vaultHome: "full", defaultDir: "full",
			want: "vault",
		},
		{
			name:      "a full vault home wins when ~/.verger is absent",
			vaultHome: "full", defaultDir: "",
			want: "vault",
		},
		{
			name:      "a full vault home wins when ~/.verger is emptied",
			vaultHome: "full", defaultDir: "empty",
			want: "vault",
		},
	}

	for _, row := range cases {
		t.Run(row.name, func(t *testing.T) {
			Convey("Given that state", t, func() {
				userHome := t.TempDir()

				vault := filepath.Join(userHome, BeadleDirName, BeadleSubdir)
				So(os.MkdirAll(filepath.Dir(vault), 0o700), ShouldBeNil)
				makeHomeState(t, vault, row.vaultHome)

				makeHomeState(t, filepath.Join(userHome, DirName), row.defaultDir)

				h, err := Discover(WithEnv(envMap(nil)), WithUserHome(userHome))
				So(err, ShouldBeNil)

				Convey("Then the expected home is chosen", func() {
					if row.want == "vault" {
						So(h.Root(), ShouldEqual, vault)
						So(h.Source(), ShouldEqual, SourceBeadleVault)

						return
					}

					So(h.Root(), ShouldEqual, filepath.Join(userHome, DirName))
					So(h.Source(), ShouldEqual, SourceDefault)
				})
			})
		})
	}
}

// Two real homes is the state this product refuses to create, and the vault
// winning silently is how it stays invisible. The rule picks the vault in that
// case; this is what makes the other one say so out loud.
func TestTwoFullHomesAreBothThereAndTheOtherIsNamed(t *testing.T) {
	Convey("Given a full vault home and a full ~/.verger", t, func() {
		userHome := t.TempDir()

		vault := filepath.Join(userHome, BeadleDirName, BeadleSubdir)
		So(os.MkdirAll(filepath.Dir(vault), 0o700), ShouldBeNil)
		makeHomeState(t, vault, "full")

		defaultHome := filepath.Join(userHome, DirName)
		makeHomeState(t, defaultHome, "full")

		h, err := Discover(WithEnv(envMap(nil)), WithUserHome(userHome))
		So(err, ShouldBeNil)
		So(h.Root(), ShouldEqual, vault)

		Convey("Then discovery still chooses the vault", func() {
			other, competing := CompetingHome(WithEnv(envMap(nil)), WithUserHome(userHome))
			So(competing, ShouldBeTrue)
			So(other, ShouldEqual, defaultHome)
		})
	})

	Convey("Given one full home and one absent", t, func() {
		userHome := t.TempDir()

		vault := filepath.Join(userHome, BeadleDirName, BeadleSubdir)
		So(os.MkdirAll(filepath.Dir(vault), 0o700), ShouldBeNil)
		makeHomeState(t, vault, "full")

		Convey("Then there is nothing to warn about", func() {
			_, competing := CompetingHome(WithEnv(envMap(nil)), WithUserHome(userHome))
			So(competing, ShouldBeFalse)
		})
	})
}

// makeHomeState puts a home directory into one of three shapes: absent, present
// and empty, or present with something in it. The three are different states to
// this rule and the whole point is that they are no longer conflated.
func makeHomeState(t *testing.T, path, shape string) {
	t.Helper()

	switch shape {
	case "":
		return
	case "empty":
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	case "full":
		if err := os.MkdirAll(filepath.Join(path, "state"), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}

		if err := os.WriteFile(filepath.Join(path, "state", "receipts"), []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("write receipts: %v", err)
		}
	default:
		t.Fatalf("unknown shape %q", shape)
	}
}
