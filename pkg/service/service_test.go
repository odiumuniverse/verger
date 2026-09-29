package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// recorder is a Runner that records every command and answers from a script, so
// a test never shells out and never touches the real launchd or systemd.
type recorder struct {
	calls    [][]string
	answers  map[string]string
	failWith map[string]error
}

func newRecorder() *recorder {
	return &recorder{answers: map[string]string{}, failWith: map[string]error{}}
}

func (r *recorder) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	r.calls = append(r.calls, call)

	key := strings.Join(call, " ")

	// Failures are registered by prefix for the same reason answers are: a
	// test wants "the registration fails" without spelling the domain and
	// the unit path, which the unit's own content decides.
	for prefix, failure := range r.failWith {
		if strings.HasPrefix(key, prefix) {
			return nil, failure
		}
	}

	// Answers are registered by prefix, so a test can say "launchctl print
	// answers this" without spelling the label. Longest prefix wins, so a
	// specific answer beats a general one.
	best, answer := "", ""

	for prefix, value := range r.answers {
		if strings.HasPrefix(key, prefix) && len(prefix) > len(best) {
			best, answer = prefix, value
		}
	}

	return []byte(answer), nil
}

// on answers a command by prefix match, so a test does not have to spell every
// argument to say "is-active says inactive".
func (r *recorder) on(prefix, answer string) *recorder {
	r.answers[prefix] = answer

	return r
}

func (r *recorder) ran(prefix string) bool {
	for _, call := range r.calls {
		if strings.HasPrefix(strings.Join(call, " "), prefix) {
			return true
		}
	}

	return false
}

// withRealHome points the temporary-home guard at an empty root list for the
// duration of a test, so t.TempDir() counts as a real home. Without this the
// guard — correctly — refuses every test home, and no idempotency test could
// install anything.
func withRealHome(t *testing.T) {
	t.Helper()

	previous := temporaryRoots
	temporaryRoots = func() []string { return nil }

	t.Cleanup(func() { temporaryRoots = previous })
}

// withPlatform runs body with the package's platform seam pointed at value.
func withPlatform(t *testing.T, value string) {
	t.Helper()

	previous := goos
	goos = value

	t.Cleanup(func() { goos = previous })
}

func renderFor(t *testing.T, goosValue string, spec Spec) (string, string) {
	t.Helper()

	withPlatform(t, goosValue)

	path, content, err := Render(spec)
	So(err, ShouldBeNil)

	return path, content
}

func TestLaunchdUnitIsRendered(t *testing.T) {
	Convey("Given a watch spec for the watcher's home", t, func() {
		home := t.TempDir()
		spec := WatchSpec("/usr/local/bin/verger", home, filepath.Join(home, ".verger"))

		path, content := renderFor(t, "darwin", spec)

		Convey("Then it is a launchd agent under the user's own LaunchAgents", func() {
			So(path, ShouldEqual, filepath.Join(home, "Library", "LaunchAgents", DefaultLabel+".plist"))
			So(content, ShouldContainSubstring, "<key>Label</key><string>"+DefaultLabel+"</string>")
		})

		Convey("Then it runs the binary with the watch verb", func() {
			So(content, ShouldContainSubstring, "<string>/usr/local/bin/verger</string>")
			So(content, ShouldContainSubstring, "<string>watch</string>")
		})

		Convey("Then it starts at login and comes back, with a throttle", func() {
			So(content, ShouldContainSubstring, "<key>RunAtLoad</key><true/>")
			So(content, ShouldContainSubstring, "<key>KeepAlive</key><true/>")
			So(content, ShouldContainSubstring, "<key>ThrottleInterval</key><integer>10</integer>")
		})

		Convey("Then it pins the environment the CLI would have used", func() {
			So(content, ShouldContainSubstring, "<key>HOME</key>")
			So(content, ShouldContainSubstring, "<key>EnvironmentVariables</key>")
		})

		Convey("Then both log paths are under the store", func() {
			So(content, ShouldContainSubstring, "watch.log")
			So(content, ShouldContainSubstring, "watch.err.log")

			// Both logs live under the store, which is the property that
			// matters. Asserting "not /tmp" instead would only hold on macOS,
			// where t.TempDir() is not under /tmp — a test that passes on one
			// platform for the wrong reason.
			store := filepath.Join(home, ".verger")
			So(content, ShouldContainSubstring, filepath.Join(store, "logs"))
			So(content, ShouldContainSubstring, "watch.err.log")
		})
	})
}

func TestSystemdUnitIsRendered(t *testing.T) {
	Convey("Given a watch spec on Linux", t, func() {
		home := t.TempDir()
		spec := WatchSpec("/home/u/bin/verger", home, filepath.Join(home, ".verger"))

		path, content := renderFor(t, "linux", spec)

		Convey("Then it is a --user unit, not a system one", func() {
			So(path, ShouldEqual, filepath.Join(home, ".config", "systemd", "user", DefaultLabel+".service"))
			So(content, ShouldNotContainSubstring, "/etc/systemd")
		})

		Convey("Then it runs the binary with the watch verb and restarts", func() {
			So(content, ShouldContainSubstring, "ExecStart=/home/u/bin/verger watch")
			So(content, ShouldContainSubstring, "Restart=on-failure")
		})

		Convey("Then it needs no privilege", func() {
			So(content, ShouldContainSubstring, "NoNewPrivileges=yes")
		})

		Convey("Then it is wanted by the default target so it starts at login", func() {
			So(content, ShouldContainSubstring, "WantedBy=default.target")
		})
	})
}

func TestUnsupportedPlatformIsTyped(t *testing.T) {
	Convey("Given a platform with no unit format", t, func() {
		withPlatform(t, "windows")

		_, _, err := Render(WatchSpec("/x/verger", t.TempDir(), ""))

		Convey("Then the error names the platform", func() {
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "windows")
		})
	})
}

func TestInstallIsIdempotent(t *testing.T) {
	withRealHome(t)

	Convey("Given a macOS home with the agent already installed", t, func() {
		home := t.TempDir()
		spec := WatchSpec("/usr/local/bin/verger", home, filepath.Join(home, ".verger"))

		withPlatform(t, "darwin")

		run := newRecorder()

		_, err := Install(t.Context(), spec, run)
		So(err, ShouldBeNil)

		first, err := os.ReadFile(UnitPathFor(t, home))
		So(err, ShouldBeNil)

		run = newRecorder()
		_, err = Install(t.Context(), spec, run)
		So(err, ShouldBeNil)

		second, err := os.ReadFile(UnitPathFor(t, home))
		So(err, ShouldBeNil)

		Convey("Then the unit is byte-identical and nothing errors", func() {
			So(string(second), ShouldEqual, string(first))
		})

		Convey("Then the manager is asked to load it again, so a changed unit replaces the running one", func() {
			// Bootout-then-bootstrap is what makes reinstall idempotent rather
			// than an "already loaded" failure.
			So(run.ran("launchctl bootout"), ShouldBeTrue)
			So(run.ran("launchctl bootstrap"), ShouldBeTrue)
		})
	})
}

func TestInstallRefusesATemporaryHome(t *testing.T) {
	Convey("Given a home under the temporary root", t, func() {
		home := filepath.Join(os.TempDir(), "verger-service-test")
		spec := WatchSpec("/usr/local/bin/verger", home, "")

		withPlatform(t, "darwin")

		run := newRecorder()

		_, err := Install(t.Context(), spec, run)

		Convey("Then it is refused, and no command is run", func() {
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "temporary home")
			So(run.calls, ShouldBeEmpty)
		})
	})
}

// TestTemporaryHomeSeesThroughASymlink pins the spelling trap: macOS hands
// out /var/folders and /tmp as links to /private/var/folders and
// /private/tmp. A guard that compares the caller's spelling against the root
// list alone would pass a home that is plainly temporary, and the unit would
// outlive the tree that built it.
func TestTemporaryHomeSeesThroughASymlink(t *testing.T) {
	Convey("Given a temporary home reached through a symlink", t, func() {
		resolved := t.TempDir()

		link := filepath.Join(t.TempDir(), "home-link")
		So(os.Symlink(resolved, link), ShouldBeNil)

		Convey("Then the guard still calls it temporary", func() {
			So(TemporaryHome(link), ShouldBeTrue)
		})

		Convey("And so does the home below it", func() {
			So(TemporaryHome(filepath.Join(link, ".verger")), ShouldBeTrue)
		})
	})
}

// TestInstallRollsBackTheUnitWhenRegistrationFails pins the second half of
// the temporary-home fix: a unit file the platform manager never accepted is
// a claim about this machine that is false. `service status` would find it
// and the next install would call it "unchanged" instead of re-registering.
func TestInstallRollsBackTheUnitWhenRegistrationFails(t *testing.T) {
	withRealHome(t)

	Convey("Given a real home and a platform that refuses the registration", t, func() {
		home := t.TempDir()
		spec := WatchSpec("/usr/local/bin/verger", home, filepath.Join(home, ".verger"))

		withPlatform(t, "darwin")

		run := newRecorder()
		run.failWith["launchctl bootstrap"] = errors.New("refused")

		_, err := Install(t.Context(), spec, run)

		Convey("Then the install fails", func() {
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "refused")
		})

		Convey("Then no unit file is left behind", func() {
			_, statErr := os.Stat(UnitPathFor(t, home))
			So(os.IsNotExist(statErr), ShouldBeTrue)
		})
	})

	Convey("Given a unit that was already installed", t, func() {
		home := t.TempDir()
		spec := WatchSpec("/usr/local/bin/verger", home, filepath.Join(home, ".verger"))

		withPlatform(t, "darwin")
		So(nil, ShouldBeNil)

		_, err := Install(t.Context(), spec, newRecorder())
		So(err, ShouldBeNil)

		before, err := os.ReadFile(UnitPathFor(t, home))
		So(err, ShouldBeNil)

		Convey("When a later install fails, the old unit is restored", func() {
			changed := WatchSpec("/usr/local/bin/other", home, filepath.Join(home, ".verger"))
			run := newRecorder()
			run.failWith["launchctl bootstrap"] = errors.New("refused")

			_, installErr := Install(t.Context(), changed, run)
			So(installErr, ShouldNotBeNil)

			after, readErr := os.ReadFile(UnitPathFor(t, home))
			So(readErr, ShouldBeNil)
			So(string(after), ShouldEqual, string(before))
		})
	})
}

func TestUninstallIsIdempotent(t *testing.T) {
	withRealHome(t)

	Convey("Given an installed agent", t, func() {
		home := t.TempDir()
		spec := WatchSpec("/usr/local/bin/verger", home, filepath.Join(home, ".verger"))

		withPlatform(t, "darwin")
		_, err := Install(t.Context(), spec, newRecorder())
		So(err, ShouldBeNil)

		Convey("Then uninstalling twice is not an error", func() {
			_, err := Uninstall(t.Context(), spec, newRecorder())
			So(err, ShouldBeNil)

			_, err = Uninstall(t.Context(), spec, newRecorder())
			So(err, ShouldBeNil)

			_, statErr := os.Stat(UnitPathFor(t, home))
			So(os.IsNotExist(statErr), ShouldBeTrue)
		})
	})
}

func TestStatusDistinguishesFourStates(t *testing.T) {
	withRealHome(t)

	Convey("Given a home with no unit", t, func() {
		home := t.TempDir()
		withPlatform(t, "darwin")

		run := newRecorder().on("launchctl print", "service could not be found\n")

		status, err := Check(t.Context(), home, "", run)

		Convey("Then it is not-installed", func() {
			So(err, ShouldBeNil)
			So(status.State, ShouldEqual, StateNotInstalled)
			So(status.Installed, ShouldBeFalse)
		})
	})

	Convey("Given a unit whose manager has it stopped", t, func() {
		home := t.TempDir()
		withPlatform(t, "darwin")

		binary := filepath.Join(home, "verger")
		writeFile(t, binary, "#!/bin/sh\n")
		writeUnit(t, home, binary)

		run := newRecorder().on("launchctl print", "\tstate = exited\n")

		status, err := Check(t.Context(), home, "", run)

		Convey("Then it is installed-stopped", func() {
			So(err, ShouldBeNil)
			So(status.State, ShouldEqual, StateStopped)
			So(status.Installed, ShouldBeTrue)
		})
	})

	Convey("Given a unit the manager is running", t, func() {
		home := t.TempDir()
		withPlatform(t, "darwin")

		binary := filepath.Join(home, "verger")
		writeFile(t, binary, "#!/bin/sh\n")
		writeUnit(t, home, binary)

		run := newRecorder().on("launchctl print", "\tstate = running\n")

		status, err := Check(t.Context(), home, "", run)

		Convey("Then it is running", func() {
			So(err, ShouldBeNil)
			So(status.State, ShouldEqual, StateRunning)
		})
	})

	Convey("Given a unit whose binary has moved", t, func() {
		home := t.TempDir()
		withPlatform(t, "darwin")
		writeUnit(t, home, filepath.Join(home, "gone", "verger"))

		run := newRecorder().on("launchctl print", "\tstate = running\n")

		status, err := Check(t.Context(), home, "", run)

		Convey("Then it is installed-but-binary-moved, not running", func() {
			// The manager may well have it loaded, but it names a program that
			// is not there, so "running" would be the misleading answer.
			So(err, ShouldBeNil)
			So(status.State, ShouldEqual, StateBinaryMoved)
			So(status.Binary, ShouldContainSubstring, "verger")
		})
	})
}

func TestNoUserSystemdIsTypedAndNeverRoot(t *testing.T) {
	withRealHome(t)

	Convey("Given a Linux box with no user bus", t, func() {
		home := t.TempDir()
		spec := WatchSpec("/home/u/bin/verger", home, filepath.Join(home, ".verger"))

		withPlatform(t, "linux")
		t.Setenv("XDG_RUNTIME_DIR", "")

		_, err := Install(t.Context(), spec, newRecorder())

		Convey("Then the error is typed and carries a hint", func() {
			So(err, ShouldNotBeNil)
			So(IsNoUserSystemd(err), ShouldBeTrue)
			So(err.Error(), ShouldContainSubstring, "user bus")
			So(err.Error(), ShouldContainSubstring, "XDG_RUNTIME_DIR")
		})

		Convey("Then it refuses rather than installing a root unit", func() {
			So(err.Error(), ShouldContainSubstring, "will not install a root unit")
		})
	})
}

func TestExecStartQuotesAPathWithASpace(t *testing.T) {
	Convey("Given a binary under a path with a space", t, func() {
		home := t.TempDir()
		spec := WatchSpec("/Users/a b/verger", home, "")

		_, content := renderFor(t, "linux", spec)

		Convey("Then the whole path is one quoted argument", func() {
			So(content, ShouldContainSubstring, `ExecStart="/Users/a b/verger" watch`)
		})
	})
}

func TestUnitEnvPinsHomeAndVergerHome(t *testing.T) {
	Convey("Given a verger root that differs from the default", t, func() {
		env := UnitEnv("/home/u", "/srv/verger")

		Convey("Then both are explicit", func() {
			So(env["HOME"], ShouldEqual, "/home/u")
			So(env["VERGER_HOME"], ShouldEqual, "/srv/verger")
		})
	})

	Convey("Given the default verger root", t, func() {
		env := UnitEnv("/home/u", filepath.Join("/home/u", ".verger"))

		Convey("Then VERGER_HOME is not pinned redundantly", func() {
			_, ok := env["VERGER_HOME"]
			So(ok, ShouldBeFalse)
		})
	})
}

// --- helpers ---

func UnitPathFor(t *testing.T, home string) string {
	t.Helper()

	path, err := UnitPath(home, "")
	So(err, ShouldBeNil)

	return path
}

func writeUnit(t *testing.T, home, binary string) {
	t.Helper()

	path := UnitPathFor(t, home)
	So(os.MkdirAll(filepath.Dir(path), 0o750), ShouldBeNil)

	_, content, err := RenderLaunchd(Spec{Binary: binary, Home: home, Label: ""})
	So(err, ShouldBeNil)

	So(os.WriteFile(path, []byte(content), 0o600), ShouldBeNil)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	So(os.MkdirAll(filepath.Dir(path), 0o750), ShouldBeNil)
	So(os.WriteFile(path, []byte(content), 0o600), ShouldBeNil)
}
