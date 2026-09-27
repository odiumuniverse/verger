package source

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
// XDG_DATA_HOME to a temp directory before the suite runs.
func TestMain(m *testing.M) {
	os.Exit(isolateTestHome(m)) // os.Exit skips defers: clean up inside the helper
}

func isolateTestHome(m *testing.M) int {
	home, err := os.MkdirTemp("", "verger-source-test-home") //nolint:usetesting // TestMain has no *testing.T to hang t.TempDir on
	if err != nil {
		trace("create test home: " + err.Error())

		return 1
	}

	defer func() { _ = os.RemoveAll(home) }()

	for name, value := range map[string]string{
		"HOME":            home,
		"VERGER_HOME":     filepath.Join(home, ".verger"),
		"BEADLE_HOME":     filepath.Join(home, ".beadle"),
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
			Convey("Then every home-shaped variable points into the temp isolation dir", func() {
				So(isolatedHome, ShouldNotBeEmpty)

				home, err := os.UserHomeDir()
				So(err, ShouldBeNil)
				So(strings.HasPrefix(home, isolatedHome), ShouldBeTrue)

				for _, name := range []string{"HOME", "VERGER_HOME", "BEADLE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME"} {
					So(os.Getenv(name), ShouldContainSubstring, isolatedHome)
				}

				So(os.Getenv("XDG_DATA_HOME"), ShouldEqual, filepath.Join(isolatedHome, ".local", "share"))
			})
		})
	})
}
