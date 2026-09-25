package secret

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// isolatedHome is the temp home TestMain pins the environment to.
var isolatedHome string

// isolatedBin is the only PATH entry the suite sees.
var isolatedBin string

// TestMain pins HOME, VERGER_HOME, BEADLE_HOME, XDG_CONFIG_HOME,
// XDG_DATA_HOME and PATH before the suite runs. The PATH pin is the keychain
// guarantee: the suite cannot reach a real `security`/`secret-tool` binary,
// so a test that forgets to inject a Keyring or a Runner fails with
// ErrKeyringUnavailable instead of touching the developer's real keychain.
func TestMain(m *testing.M) {
	os.Exit(isolateTestHome(m)) // os.Exit skips defers: clean up inside the helper
}

func isolateTestHome(m *testing.M) int {
	home, err := os.MkdirTemp("", "verger-secret-test-home") //nolint:usetesting // TestMain has no *testing.T to hang t.TempDir on
	if err != nil {
		trace("create test home: " + err.Error())

		return 1
	}

	defer func() { _ = os.RemoveAll(home) }()

	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		trace("create test bin dir: " + err.Error())

		return 1
	}

	for name, value := range map[string]string{
		"HOME":            home,
		"VERGER_HOME":     "",
		"BEADLE_HOME":     "",
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
		"PATH":            bin,
	} {
		//nolint:usetesting // TestMain cannot use t.Setenv; tests override per test
		if err := os.Setenv(name, value); err != nil {
			trace("set " + name + ": " + err.Error())

			return 1
		}
	}

	isolatedHome, isolatedBin = home, bin

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
				So(isolatedBin, ShouldContainSubstring, isolatedHome)

				home, err := os.UserHomeDir()
				So(err, ShouldBeNil)
				So(strings.HasPrefix(home, isolatedHome), ShouldBeTrue)

				So(os.Getenv("VERGER_HOME"), ShouldBeEmpty)
				So(os.Getenv("BEADLE_HOME"), ShouldBeEmpty)
				So(os.Getenv("XDG_CONFIG_HOME"), ShouldContainSubstring, isolatedHome)
				So(os.Getenv("XDG_DATA_HOME"), ShouldContainSubstring, isolatedHome)
			})

			Convey("Then the real keychain tools are unreachable", func() {
				for _, tool := range []string{"security", "secret-tool"} {
					_, err := exec.LookPath(tool)
					So(errors.Is(err, exec.ErrNotFound), ShouldBeTrue)
				}
			})
		})
	})
}
