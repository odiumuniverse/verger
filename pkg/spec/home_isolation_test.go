package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// isolatedHome is the temp home TestMain pins the environment to.
var isolatedHome string

// TestMain pins HOME, VERGER_HOME, BEADLE_HOME, XDG_CONFIG_HOME and
// XDG_DATA_HOME to a temp directory before the suite runs: a test that forgets
// its own t.Setenv must never touch the developer's real home. t.Setenv in a
// test still wins over these values.
func TestMain(m *testing.M) {
	code, err := runIsolated(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "spec test isolation:", err)

		os.Exit(1)
	}

	os.Exit(code)
}

func runIsolated(m *testing.M) (int, error) {
	home, err := os.MkdirTemp("", "verger-test-home")
	if err != nil {
		return 0, err
	}

	defer func() { _ = os.RemoveAll(home) }()

	for name, value := range map[string]string{
		"HOME":            home,
		"VERGER_HOME":     filepath.Join(home, ".verger"),
		"BEADLE_HOME":     filepath.Join(home, ".beadle"),
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
	} {
		if err := os.Setenv(name, value); err != nil {
			return 0, fmt.Errorf("set %s: %w", name, err)
		}
	}

	isolatedHome = home

	return m.Run(), nil
}

func TestSuiteHomeIsolation(t *testing.T) {
	Convey("Given the isolated test suite", t, func() {
		Convey("When the environment is inspected", func() {
			Convey("Then every home-shaped variable points into the temp isolation dir", func() {
				So(isolatedHome, ShouldNotBeEmpty)

				home, err := os.UserHomeDir()
				So(err, ShouldBeNil)
				So(strings.HasPrefix(home, isolatedHome), ShouldBeTrue)

				for _, name := range []string{"HOME", "VERGER_HOME", "BEADLE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME"} {
					So(os.Getenv(name), ShouldContainSubstring, isolatedHome)
				}
			})
		})
	})
}
