package home

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// envMap builds a lookup function over a fixed map; missing keys read as unset.
func envMap(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

// assertMode fails when path does not carry exactly want.
func assertMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode of %s = %o, want %o", path, got, want)
	}
}

// assertMissing fails when path exists.
func assertMissing(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s should not exist (err=%v)", path, err)
	}
}

// mkFile writes a test file, creating parents.
func mkFile(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// readTestFile reads a file the test itself created.
func readTestFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: the test reads a path it created
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(data)
}

func TestDiscoverPrecedence(t *testing.T) {
	Convey("Given a temp user home", t, func() {
		userHome := t.TempDir()

		Convey("When VERGER_HOME is set", func() {
			envRoot := filepath.Join(t.TempDir(), "custom-verger")

			h, err := Discover(
				WithEnv(envMap(map[string]string{EnvHome: envRoot})),
				WithUserHome(userHome),
			)

			Convey("Then it wins even though the directory does not exist", func() {
				So(err, ShouldBeNil)
				So(h.Root(), ShouldEqual, envRoot)
				So(h.Source(), ShouldEqual, SourceEnv)
				So(h.Exists(), ShouldBeFalse)
				assertMissing(t, envRoot)
			})
		})

		Convey("When VERGER_HOME is set next to an existing beadle vault", func() {
			beadle := filepath.Join(t.TempDir(), "beadle")
			So(os.MkdirAll(filepath.Join(beadle, BeadleSubdir), 0o700), ShouldBeNil)

			envRoot := filepath.Join(t.TempDir(), "explicit")
			h, err := Discover(
				WithEnv(envMap(map[string]string{EnvHome: envRoot, EnvBeadleHome: beadle})),
				WithUserHome(userHome),
			)

			Convey("Then the explicit override wins", func() {
				So(err, ShouldBeNil)
				So(h.Root(), ShouldEqual, envRoot)
				So(h.Source(), ShouldEqual, SourceEnv)
			})
		})

		Convey("When BEADLE_HOME points at an existing vault", func() {
			beadle := filepath.Join(t.TempDir(), "beadle")
			vault := filepath.Join(beadle, BeadleSubdir)
			So(os.MkdirAll(vault, 0o700), ShouldBeNil)
			So(os.WriteFile(filepath.Join(vault, "verger.toml"), []byte("# spec\n"), 0o600), ShouldBeNil)

			h, err := Discover(
				WithEnv(envMap(map[string]string{EnvBeadleHome: beadle})),
				WithUserHome(userHome),
			)

			Convey("Then the beadle vault is used", func() {
				So(err, ShouldBeNil)
				So(h.Root(), ShouldEqual, vault)
				So(h.Source(), ShouldEqual, SourceBeadleVault)
			})
		})
	})
}

func TestDiscoverBeadleFallbacks(t *testing.T) {
	Convey("Given a temp user home", t, func() {
		userHome := t.TempDir()

		Convey("When BEADLE_HOME is unset but ~/.beadle/verger exists", func() {
			vault := filepath.Join(userHome, BeadleDirName, BeadleSubdir)
			So(os.MkdirAll(vault, 0o700), ShouldBeNil)
			So(os.WriteFile(filepath.Join(vault, "verger.toml"), []byte("# spec\n"), 0o600), ShouldBeNil)

			h, err := Discover(WithEnv(envMap(nil)), WithUserHome(userHome))

			Convey("Then the default beadle vault is used", func() {
				So(err, ShouldBeNil)
				So(h.Root(), ShouldEqual, vault)
				So(h.Source(), ShouldEqual, SourceBeadleVault)
			})
		})

		Convey("When BEADLE_HOME is set but its vault is missing", func() {
			beadle := filepath.Join(t.TempDir(), "beadle")

			h, err := Discover(
				WithEnv(envMap(map[string]string{EnvBeadleHome: beadle})),
				WithUserHome(userHome),
			)

			Convey("Then the default ~/.verger is used", func() {
				So(err, ShouldBeNil)
				So(h.Root(), ShouldEqual, filepath.Join(userHome, DirName))
				So(h.Source(), ShouldEqual, SourceDefault)
			})
		})

		Convey("When the beadle candidate exists but is a file", func() {
			beadle := filepath.Join(t.TempDir(), "beadle")
			So(os.MkdirAll(beadle, 0o700), ShouldBeNil)
			mkFile(t, filepath.Join(beadle, BeadleSubdir), "not a dir", 0o600)

			h, err := Discover(
				WithEnv(envMap(map[string]string{EnvBeadleHome: beadle})),
				WithUserHome(userHome),
			)

			Convey("Then discovery falls through to the default", func() {
				So(err, ShouldBeNil)
				So(h.Root(), ShouldEqual, filepath.Join(userHome, DirName))
				So(h.Source(), ShouldEqual, SourceDefault)
			})
		})
	})
}

func TestDiscoverPathForms(t *testing.T) {
	Convey("Given a temp user home", t, func() {
		userHome := t.TempDir()

		Convey("When env values are empty or whitespace", func() {
			h, err := Discover(
				WithEnv(envMap(map[string]string{EnvHome: "   ", EnvBeadleHome: " "})),
				WithUserHome(userHome),
			)

			Convey("Then they count as unset", func() {
				So(err, ShouldBeNil)
				So(h.Root(), ShouldEqual, filepath.Join(userHome, DirName))
				So(h.Source(), ShouldEqual, SourceDefault)
			})
		})

		Convey("When VERGER_HOME is relative", func() {
			h, err := Discover(
				WithEnv(envMap(map[string]string{EnvHome: filepath.Join("rel", "verger")})),
				WithUserHome(userHome),
			)

			Convey("Then it is made absolute", func() {
				So(err, ShouldBeNil)
				So(filepath.IsAbs(h.Root()), ShouldBeTrue)
				So(filepath.Base(h.Root()), ShouldEqual, "verger")
			})
		})

		Convey("When VERGER_HOME uses a tilde", func() {
			// Tilde expansion goes through fsutil.ExpandHome, which reads $HOME.
			t.Setenv("HOME", userHome)

			h, err := Discover(
				WithEnv(envMap(map[string]string{EnvHome: "~/tilde-verger"})),
				WithUserHome(userHome),
			)

			Convey("Then the tilde expands against the injected user home", func() {
				So(err, ShouldBeNil)
				So(h.Root(), ShouldEqual, filepath.Join(userHome, "tilde-verger"))
			})
		})

		Convey("When BEADLE_HOME is relative but its vault exists", func() {
			beadle := filepath.Join(t.TempDir(), "rel-beadle")
			vault := filepath.Join(beadle, BeadleSubdir)
			So(os.MkdirAll(vault, 0o700), ShouldBeNil)
			So(os.WriteFile(filepath.Join(vault, "verger.toml"), []byte("# spec\n"), 0o600), ShouldBeNil)

			cwd, err := os.Getwd()
			So(err, ShouldBeNil)

			rel, err := filepath.Rel(cwd, beadle)
			So(err, ShouldBeNil)

			h, err := Discover(
				WithEnv(envMap(map[string]string{EnvBeadleHome: rel})),
				WithUserHome(userHome),
			)

			Convey("Then it resolves and detects the vault", func() {
				So(err, ShouldBeNil)
				So(h.Root(), ShouldEqual, vault)
				So(h.Source(), ShouldEqual, SourceBeadleVault)
			})
		})

		Convey("When the user home override is empty", func() {
			_, err := Discover(WithEnv(envMap(nil)), WithUserHome(""))

			Convey("Then it reports NoHomeError", func() {
				So(err, ShouldBeError)

				_, ok := errors.AsType[*NoHomeError](err)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When discovery succeeds", func() {
			h, err := Discover(WithEnv(envMap(nil)), WithUserHome(userHome))

			Convey("Then it creates nothing", func() {
				So(err, ShouldBeNil)
				assertMissing(t, h.Root())
				assertMissing(t, filepath.Join(userHome, BeadleDirName))
			})
		})
	})
}

func TestDiscoverUserHomeError(t *testing.T) {
	Convey("Given an unset process HOME", t, func() {
		t.Setenv("HOME", "")

		Convey("When discovery needs the user home", func() {
			_, err := Discover(WithEnv(envMap(nil)))

			Convey("Then it reports NoHomeError with a usable message and cause", func() {
				So(err, ShouldBeError)

				target, ok := errors.AsType[*NoHomeError](err)
				So(ok, ShouldBeTrue)
				So(target.Error(), ShouldContainSubstring, "home")
				So(target.Unwrap(), ShouldNotBeNil)
			})
		})
	})
}

func TestHomeErrorStrings(t *testing.T) {
	Convey("Given invalid home roots", t, func() {
		Convey("When New rejects a root", func() {
			_, err := New("")

			Convey("Then the error renders path and reason", func() {
				So(err.Error(), ShouldContainSubstring, "invalid home")
				So(err.Error(), ShouldContainSubstring, "empty")
			})
		})

		Convey("When a tilde root cannot expand without HOME", func() {
			t.Setenv("HOME", "")

			_, err := New("~/verger")

			Convey("Then it reports InvalidHomeError", func() {
				So(err, ShouldBeError)

				_, ok := errors.AsType[*InvalidHomeError](err)
				So(ok, ShouldBeTrue)
				So(err.Error(), ShouldContainSubstring, "invalid home")
			})
		})
	})
}

func TestNewHome(t *testing.T) {
	Convey("Given explicit home roots", t, func() {
		Convey("When the root is absolute", func() {
			root := filepath.Join(t.TempDir(), "verger")
			h, err := New(root)

			Convey("Then it is kept and marked as an explicit environment root", func() {
				So(err, ShouldBeNil)
				So(h.Root(), ShouldEqual, root)
				So(h.Source(), ShouldEqual, SourceEnv)
			})
		})

		Convey("When the root uses a tilde", func() {
			userHome, err := os.UserHomeDir()
			So(err, ShouldBeNil)

			h, err := New("~/custom")

			Convey("Then the tilde expands", func() {
				So(err, ShouldBeNil)
				So(h.Root(), ShouldEqual, filepath.Join(userHome, "custom"))
			})
		})

		Convey("When the root is empty or whitespace", func() {
			_, err := New("   ")

			Convey("Then it reports InvalidHomeError", func() {
				So(err, ShouldBeError)

				target, ok := errors.AsType[*InvalidHomeError](err)
				So(ok, ShouldBeTrue)
				So(target.Reason, ShouldNotBeEmpty)
			})
		})

		Convey("When the root is relative", func() {
			_, err := New(filepath.Join("rel", "home"))

			Convey("Then it reports InvalidHomeError because the root must stay absolute", func() {
				So(err, ShouldBeError)

				_, ok := errors.AsType[*InvalidHomeError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestHomePaths(t *testing.T) {
	Convey("Given a home", t, func() {
		root := filepath.Join(t.TempDir(), "verger")
		h, err := New(root)
		So(err, ShouldBeNil)

		state := filepath.Join(root, "state")

		Convey("When every path accessor is read", func() {
			Convey("Then the values match the state layout", func() {
				So(h.SpecPath(), ShouldEqual, filepath.Join(root, "verger.toml"))
				So(h.LockPath(), ShouldEqual, filepath.Join(root, "verger.lock"))
				So(h.StateDir(), ShouldEqual, state)
				So(h.JournalPath(), ShouldEqual, filepath.Join(state, "journal.jsonl"))
				So(h.TombstonesPath(), ShouldEqual, filepath.Join(state, "tombstones.json"))
				So(h.ConsentPath(), ShouldEqual, filepath.Join(state, "consent.json"))
				So(h.TrustPath(), ShouldEqual, filepath.Join(state, "trust.json"))
				So(h.ReceiptsDir(), ShouldEqual, filepath.Join(state, "receipts"))
				So(h.LeasePath(), ShouldEqual, filepath.Join(state, "watch.lease"))
				So(h.FileLockPath(), ShouldEqual, filepath.Join(state, ".lock"))
				So(h.SecretsPath(), ShouldEqual, filepath.Join(state, "secrets.json"))
			})
		})

		Convey("When Exists is checked before Ensure", func() {
			Convey("Then it is false and becomes true after Ensure", func() {
				So(h.Exists(), ShouldBeFalse)
				So(h.Ensure(), ShouldBeNil)
				So(h.Exists(), ShouldBeTrue)
			})
		})
	})
}

func TestHomeEnsure(t *testing.T) {
	Convey("Given a home with missing parents", t, func() {
		root := filepath.Join(t.TempDir(), "one", "two", "verger")
		h, err := New(root)
		So(err, ShouldBeNil)

		Convey("When Ensure runs", func() {
			err := h.Ensure()

			Convey("Then root and state exist with 0700", func() {
				So(err, ShouldBeNil)
				assertMode(t, root, 0o700)
				assertMode(t, h.StateDir(), 0o700)
			})

			Convey("Then Ensure is idempotent and tightens the leaf dirs", func() {
				So(err, ShouldBeNil)
				So(h.Ensure(), ShouldBeNil)
				assertMode(t, root, 0o700)

				//nolint:gosec // G302: the test deliberately loosens a dir to assert Ensure tightens it
				So(os.Chmod(h.StateDir(), 0o755), ShouldBeNil)
				So(h.Ensure(), ShouldBeNil)
				assertMode(t, h.StateDir(), 0o700)
			})
		})
	})
}

func TestHomeStoreIndependence(t *testing.T) {
	Convey("Given a decoy store directory", t, func() {
		decoy := filepath.Join(t.TempDir(), "store")
		mkFile(t, filepath.Join(decoy, "data", "keep.txt"), "store bytes", 0o600)

		userHome := t.TempDir()
		h, err := Discover(
			WithEnv(envMap(map[string]string{"XDG_DATA_HOME": decoy, EnvHome: filepath.Join(t.TempDir(), "home")})),
			WithUserHome(userHome),
		)
		So(err, ShouldBeNil)

		Convey("When the home is ensured", func() {
			So(h.Ensure(), ShouldBeNil)

			Convey("Then the store path is untouched", func() {
				So(readTestFile(t, filepath.Join(decoy, "data", "keep.txt")), ShouldEqual, "store bytes")
				assertMode(t, filepath.Join(decoy, "data", "keep.txt"), 0o600)
			})
		})
	})
}

func TestDiscoverVaultWithoutUserHome(t *testing.T) {
	Convey("Given an existing BEADLE_HOME vault and no resolvable user home", t, func() {
		beadle := filepath.Join(t.TempDir(), "beadle")
		vault := filepath.Join(beadle, BeadleSubdir)
		So(os.MkdirAll(vault, 0o700), ShouldBeNil)
		So(os.WriteFile(filepath.Join(vault, "verger.toml"), []byte("# spec\n"), 0o600), ShouldBeNil)

		Convey("When discovery runs", func() {
			h, err := Discover(
				WithEnv(envMap(map[string]string{EnvBeadleHome: beadle})),
				WithUserHome(""),
			)

			Convey("Then the explicit vault wins without needing a user home", func() {
				So(err, ShouldBeNil)
				So(h.Root(), ShouldEqual, vault)
				So(h.Source(), ShouldEqual, SourceBeadleVault)
			})
		})

		Convey("When the vault is missing and no user home is available", func() {
			emptyBeadle := filepath.Join(t.TempDir(), "empty-beadle")

			_, err := Discover(
				WithEnv(envMap(map[string]string{EnvBeadleHome: emptyBeadle})),
				WithUserHome(""),
			)

			Convey("Then it still reports NoHomeError", func() {
				So(err, ShouldBeError)

				_, ok := errors.AsType[*NoHomeError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})
}
