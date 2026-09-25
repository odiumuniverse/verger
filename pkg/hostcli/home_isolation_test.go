package hostcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// isolatedHome is the temp home TestMain pins the environment to.
var isolatedHome string

// TestMain pins HOME, VERGER_HOME, BEADLE_HOME, XDG_CONFIG_HOME and
// XDG_DATA_HOME before the suite runs, so a test that forgets its own
// t.Setenv cannot touch the developer's real home.
func TestMain(m *testing.M) {
	os.Exit(isolateTestHome(m)) // os.Exit skips defers: clean up inside the helper
}

func isolateTestHome(m *testing.M) int {
	home, err := os.MkdirTemp("", "verger-hostcli-test-home") //nolint:usetesting // TestMain has no *testing.T to hang t.TempDir on
	if err != nil {
		trace("create test home: " + err.Error())

		return 1
	}

	defer func() { _ = os.RemoveAll(home) }()

	for name, value := range map[string]string{
		"HOME":            home,
		"VERGER_HOME":     "",
		"BEADLE_HOME":     "",
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
	} {
		//nolint:usetesting // TestMain cannot use t.Setenv; tests override per test
		if err := os.Setenv(name, value); err != nil {
			trace("set " + name + ": " + err.Error())

			return 1
		}
	}

	isolatedHome = home

	return m.Run()
}

// trace writes a TestMain diagnostic to stderr.
func trace(message string) {
	_, _ = os.Stderr.WriteString(message + "\n")
}

func TestSuiteHomeIsolation(t *testing.T) {
	Convey("Given the isolated test suite", t, func() {
		Convey("When a test reads the environment", func() {
			Convey("Then every home-shaped variable points into the temp isolation dir or is empty", func() {
				So(isolatedHome, ShouldNotBeEmpty)

				home, err := os.UserHomeDir()
				So(err, ShouldBeNil)
				So(strings.HasPrefix(home, isolatedHome), ShouldBeTrue)

				So(os.Getenv("VERGER_HOME"), ShouldBeEmpty)
				So(os.Getenv("BEADLE_HOME"), ShouldBeEmpty)
				So(os.Getenv("XDG_CONFIG_HOME"), ShouldContainSubstring, isolatedHome)
				So(os.Getenv("XDG_DATA_HOME"), ShouldContainSubstring, isolatedHome)
			})
		})
	})
}
