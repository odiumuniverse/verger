package verger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
	"github.com/vmkteam/embedlog"

	"github.com/odiumuniverse/verger/pkg/home"
	"github.com/odiumuniverse/verger/pkg/secret"
)

// assertMissing fails when path exists.
func assertMissing(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s should not exist (err=%v)", path, err)
	}
}

// openDefault opens a client against the pinned environment.
func openDefault(t *testing.T) *Client {
	t.Helper()

	c, err := Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	return c
}

func TestOpenDefaults(t *testing.T) {
	Convey("Given the pinned test environment", t, func() {
		Convey("When the facade opens with defaults", func() {
			c := openDefault(t)
			defer func() { _ = c.Close() }()

			m := c.Machine()

			Convey("Then machine paths resolve to the default home and XDG store", func() {
				wantHome := filepath.Join(isolatedHome, home.DirName)
				wantStore := filepath.Join(os.Getenv("XDG_DATA_HOME"), "verger")

				So(m.Home, ShouldEqual, wantHome)
				So(m.HomeSource, ShouldEqual, string(home.SourceDefault))
				So(m.SpecPath, ShouldEqual, filepath.Join(wantHome, "verger.toml"))
				So(m.LockPath, ShouldEqual, filepath.Join(wantHome, "verger.lock"))
				So(m.StateDir, ShouldEqual, filepath.Join(wantHome, "state"))
				So(m.JournalPath, ShouldEqual, filepath.Join(wantHome, "state", "journal.jsonl"))
				So(m.StoreRoot, ShouldEqual, wantStore)
				So(m.DataDir, ShouldEqual, filepath.Join(wantStore, "data"))
				So(m.TrashDir, ShouldEqual, filepath.Join(wantStore, "trash"))
				So(m.RuntimeDir, ShouldEqual, filepath.Join(wantStore, "runtime"))
				So(m.BinDir, ShouldEqual, filepath.Join(wantStore, "bin"))
				So(m.CacheDir, ShouldEqual, filepath.Join(wantStore, "cache"))
			})

			Convey("Then the resolved handles match the machine paths", func() {
				So(c.Home().Root(), ShouldEqual, c.Machine().Home)
				So(c.Store().Root(), ShouldEqual, c.Machine().StoreRoot)
			})

			Convey("Then Open creates nothing on disk", func() {
				assertMissing(t, c.Machine().Home)
				assertMissing(t, c.Machine().StoreRoot)
				assertMissing(t, c.Home().FileLockPath())
			})
		})
	})
}

func TestOpenHomeSources(t *testing.T) {
	Convey("Given a VERGER_HOME override", t, func() {
		envRoot := filepath.Join(t.TempDir(), "explicit-home")
		t.Setenv("VERGER_HOME", envRoot)

		Convey("When the facade opens", func() {
			c := openDefault(t)
			defer func() { _ = c.Close() }()

			Convey("Then the machine reports the env source", func() {
				So(c.Machine().Home, ShouldEqual, envRoot)
				So(c.Machine().HomeSource, ShouldEqual, string(home.SourceEnv))
			})
		})
	})

	Convey("Given a beadle vault next to an empty VERGER_HOME", t, func() {
		beadle := filepath.Join(t.TempDir(), "beadle")
		vault := filepath.Join(beadle, home.BeadleSubdir)

		if err := os.MkdirAll(vault, 0o700); err != nil {
			t.Fatalf("mkdir vault: %v", err)
		}

		t.Setenv("VERGER_HOME", "")
		t.Setenv("BEADLE_HOME", beadle)

		Convey("When the facade opens", func() {
			c := openDefault(t)
			defer func() { _ = c.Close() }()

			Convey("Then the machine reports the beadle vault source", func() {
				So(c.Machine().Home, ShouldEqual, vault)
				So(c.Machine().HomeSource, ShouldEqual, string(home.SourceBeadleVault))
			})
		})
	})
}

func TestOpenOptions(t *testing.T) {
	Convey("Given explicit option values", t, func() {
		explicitHome := filepath.Join(t.TempDir(), "explicit-verger")
		explicitStore := filepath.Join(t.TempDir(), "explicit-store")

		Convey("When WithHome and WithStore are given", func() {
			c, err := Open(context.Background(), WithHome(explicitHome), WithStore(explicitStore))

			Convey("Then the machine uses them without touching disk", func() {
				So(err, ShouldBeNil)

				defer func() { _ = c.Close() }()

				So(c.Machine().Home, ShouldEqual, explicitHome)
				So(c.Machine().HomeSource, ShouldEqual, string(home.SourceEnv))
				So(c.Machine().StoreRoot, ShouldEqual, explicitStore)
				assertMissing(t, explicitHome)
				assertMissing(t, explicitStore)
			})
		})

		Convey("When options use a tilde", func() {
			c, err := Open(
				context.Background(),
				WithHome("~/facade-home"),
				WithStore("~/facade-store"),
			)

			Convey("Then both expand against HOME", func() {
				So(err, ShouldBeNil)

				defer func() { _ = c.Close() }()

				So(c.Machine().Home, ShouldEqual, filepath.Join(isolatedHome, "facade-home"))
				So(c.Machine().StoreRoot, ShouldEqual, filepath.Join(isolatedHome, "facade-store"))
			})
		})

		Convey("When the same option is repeated", func() {
			first := filepath.Join(t.TempDir(), "first")
			second := filepath.Join(t.TempDir(), "second")

			c, err := Open(context.Background(), WithHome(first), WithHome(second))

			Convey("Then the last value wins", func() {
				So(err, ShouldBeNil)

				defer func() { _ = c.Close() }()

				So(c.Machine().Home, ShouldEqual, second)
			})
		})
	})
}

func TestOpenErrors(t *testing.T) {
	Convey("Given a canceled context", t, func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		Convey("When the facade opens", func() {
			_, err := Open(ctx)

			Convey("Then it reports OpenError option ctx wrapping context.Canceled", func() {
				target, ok := errors.AsType[*OpenError](err)
				So(ok, ShouldBeTrue)
				So(target.Option, ShouldEqual, "ctx")
				So(errors.Is(err, context.Canceled), ShouldBeTrue)
			})
		})
	})

	Convey("Given an empty WithHome", t, func() {
		_, err := Open(context.Background(), WithHome(""))

		Convey("Then it reports OpenError option home", func() {
			target, ok := errors.AsType[*OpenError](err)
			So(ok, ShouldBeTrue)
			So(target.Option, ShouldEqual, "home")
			So(target.Cause, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "home")
		})
	})

	Convey("Given a relative WithHome", t, func() {
		_, err := Open(context.Background(), WithHome(filepath.Join("rel", "home")))

		Convey("Then it reports OpenError option home", func() {
			target, ok := errors.AsType[*OpenError](err)
			So(ok, ShouldBeTrue)
			So(target.Option, ShouldEqual, "home")
		})
	})

	Convey("Given an empty WithStore", t, func() {
		_, err := Open(context.Background(), WithStore("   "))

		Convey("Then it reports OpenError option store", func() {
			target, ok := errors.AsType[*OpenError](err)
			So(ok, ShouldBeTrue)
			So(target.Option, ShouldEqual, "store")
			So(target.Cause, ShouldNotBeNil)
		})
	})
}

// writeFile creates path with 0600 and its parents with 0700.
func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestSecretsOption(t *testing.T) {
	Convey("Given an injected secrets store", t, func() {
		store, err := secret.Load(filepath.Join(t.TempDir(), "secrets.json"))
		So(err, ShouldBeNil)

		Convey("When the facade opens with it", func() {
			c, openErr := Open(context.Background(), WithHome(filepath.Join(t.TempDir(), "home")), WithSecrets(store))

			Convey("Then Secrets returns exactly the injected store", func() {
				So(openErr, ShouldBeNil)

				defer func() { _ = c.Close() }()

				So(c.Secrets() == store, ShouldBeTrue)
			})
		})
	})

	Convey("Given no secrets option", t, func() {
		homeRoot := filepath.Join(t.TempDir(), "home")

		Convey("When the facade opens", func() {
			c, err := Open(context.Background(), WithHome(homeRoot))
			So(err, ShouldBeNil)

			defer func() { _ = c.Close() }()

			Convey("Then the default store points at <home>/state/secrets.json and creates nothing", func() {
				So(c.Secrets(), ShouldNotBeNil)
				So(c.Secrets().Path(), ShouldEqual, filepath.Join(homeRoot, "state", "secrets.json"))
				So(c.Secrets().Backend(), ShouldEqual, secret.BackendFile)
				So(c.Secrets().Len(), ShouldEqual, 0)
				assertMissing(t, c.Home().StateDir())
			})
		})
	})

	Convey("Given a home holding a stored secret", t, func() {
		homeRoot := filepath.Join(t.TempDir(), "home")
		writeFile(t, filepath.Join(homeRoot, "state", "secrets.json"), `{"version":1,"secrets":{"TOKEN":"s3cr3t"}}`)

		Convey("When the facade opens", func() {
			c, err := Open(context.Background(), WithHome(homeRoot))
			So(err, ShouldBeNil)

			defer func() { _ = c.Close() }()

			Convey("Then the client resolves the stored value", func() {
				value, ok := c.Secrets().Get("TOKEN")
				So(ok, ShouldBeTrue)
				So(value, ShouldEqual, "s3cr3t")
			})
		})
	})

	Convey("Given a nil secrets store", t, func() {
		Convey("When the facade opens", func() {
			_, err := Open(context.Background(), WithHome(filepath.Join(t.TempDir(), "home")), WithSecrets(nil))

			target, ok := errors.AsType[*OpenError](err)

			Convey("Then Open reports OpenError option secrets", func() {
				So(ok, ShouldBeTrue)
				So(target.Option, ShouldEqual, "secrets")
			})
		})
	})

	Convey("Given a home with a corrupt secrets file", t, func() {
		homeRoot := filepath.Join(t.TempDir(), "home")
		writeFile(t, filepath.Join(homeRoot, "state", "secrets.json"), "{not json")

		Convey("When the facade opens", func() {
			_, err := Open(context.Background(), WithHome(homeRoot))

			target, openOK := errors.AsType[*OpenError](err)

			_, parseOK := errors.AsType[*secret.SecretParseError](err)

			Convey("Then the parse failure surfaces as OpenError option secrets", func() {
				So(openOK, ShouldBeTrue)
				So(target.Option, ShouldEqual, "secrets")
				So(target.Value, ShouldEqual, filepath.Join(homeRoot, "state", "secrets.json"))
				So(parseOK, ShouldBeTrue)
			})
		})
	})
}

func TestCloseContract(t *testing.T) {
	Convey("Given an open client", t, func() {
		c := openDefault(t)

		before := c.Machine()

		Convey("When Close runs twice", func() {
			first := c.Close()
			second := c.Close()

			Convey("Then both are nil and resolved data survives", func() {
				So(first, ShouldBeNil)
				So(second, ShouldBeNil)
				So(c.Machine(), ShouldResemble, before)
				So(c.Home(), ShouldNotBeNil)
				So(c.Store(), ShouldNotBeNil)
			})
		})
	})
}

func TestLoggerOption(t *testing.T) {
	Convey("Given a configured logger", t, func() {
		log := embedlog.NewDevLogger()

		Convey("When the facade opens with it", func() {
			c, err := Open(context.Background(), WithLogger(log))

			Convey("Then the client uses exactly that logger", func() {
				So(err, ShouldBeNil)

				defer func() { _ = c.Close() }()

				So(c.logger == log, ShouldBeTrue)
			})
		})

		Convey("When the facade opens without a logger", func() {
			c := openDefault(t)
			defer func() { _ = c.Close() }()

			Convey("Then the default errors-only logger is installed", func() {
				So(c.logger == embedlog.Logger{}, ShouldBeFalse)
			})
		})
	})
}

func TestOpenConcurrency(t *testing.T) {
	Convey("Given independent option sets", t, func() {
		const n = 8

		Convey("When clients open concurrently", func() {
			var (
				wg      sync.WaitGroup
				mu      sync.Mutex
				clients []*Client
				errs    []error
			)

			for i := range n {
				wg.Go(func() {
					root := filepath.Join(t.TempDir(), fmt.Sprintf("client-%d", i))

					c, err := Open(
						context.Background(),
						WithHome(filepath.Join(root, "home")),
						WithStore(filepath.Join(root, "store")),
					)

					mu.Lock()
					defer mu.Unlock()

					if err != nil {
						errs = append(errs, err)

						return
					}

					clients = append(clients, c)
				})
			}

			wg.Wait()

			Convey("Then every client resolves its own paths", func() {
				So(errs, ShouldBeEmpty)
				So(clients, ShouldHaveLength, n)

				seen := map[string]bool{}

				for _, c := range clients {
					So(seen[c.Machine().Home], ShouldBeFalse)

					seen[c.Machine().Home] = true
				}

				for _, c := range clients {
					So(c.Close(), ShouldBeNil)
				}
			})
		})
	})
}

func TestOpenNoSideEffectsOnExistingRoots(t *testing.T) {
	Convey("Given existing home and store roots", t, func() {
		root := t.TempDir()
		homeRoot := filepath.Join(root, "home")
		storeRoot := filepath.Join(root, "store")

		if err := os.MkdirAll(homeRoot, 0o700); err != nil {
			t.Fatalf("mkdir home: %v", err)
		}

		if err := os.MkdirAll(storeRoot, 0o700); err != nil {
			t.Fatalf("mkdir store: %v", err)
		}

		before, err := os.ReadDir(homeRoot)
		So(err, ShouldBeNil)

		Convey("When the facade opens", func() {
			c, openErr := Open(context.Background(), WithHome(homeRoot), WithStore(storeRoot))

			Convey("Then neither root gained entries and the caller ensures explicitly", func() {
				So(openErr, ShouldBeNil)

				defer func() { _ = c.Close() }()

				after, readErr := os.ReadDir(homeRoot)
				So(readErr, ShouldBeNil)
				So(after, ShouldResemble, before)

				So(c.Home().Ensure(), ShouldBeNil)
				So(c.Store().Ensure(), ShouldBeNil)

				state, statErr := os.Stat(c.Home().StateDir())
				So(statErr, ShouldBeNil)
				So(state.IsDir(), ShouldBeTrue)

				for _, dir := range []string{c.Store().DataDir(), c.Store().TrashDir(), c.Store().CacheDir()} {
					info, statErr := os.Stat(dir)
					So(statErr, ShouldBeNil)
					So(info.IsDir(), ShouldBeTrue)
				}
			})
		})
	})
}

func TestOpenErrorString(t *testing.T) {
	Convey("Given an OpenError", t, func() {
		cause := errors.New("boom")
		err := &OpenError{Option: "store", Value: "/x", Cause: cause}

		Convey("When it renders", func() {
			Convey("Then it names the option and wraps the cause", func() {
				So(err.Error(), ShouldContainSubstring, "store")
				So(errors.Is(err, cause), ShouldBeTrue)
				So(strings.Contains(err.Error(), "/x"), ShouldBeTrue)
			})
		})
	})
}
