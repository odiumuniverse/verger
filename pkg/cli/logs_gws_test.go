package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// TestVerboseWritesDiagnosticsToStderr pins the observable behaviour of
// --verbose: the run must leave a diagnostic line on stderr naming the command,
// and stdout must stay clean. A logger that is constructed but never called
// passes every other test in this package, so this is the one that catches it.
func TestVerboseWritesDiagnosticsToStderr(t *testing.T) {
	Convey("Given --verbose", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		stdout, err := w.run("--verbose", "sync")

		Convey("When the command runs", func() {
			Convey("Then stderr carries a debug line naming the command", func() {
				So(err, ShouldBeNil)
				So(w.err.String(), ShouldContainSubstring, "verger sync")
				So(w.err.String(), ShouldContainSubstring, "command start")
			})

			Convey("Then stdout carries only the command's own output", func() {
				So(stdout, ShouldEqual, "nothing to sync\n")
			})
		})
	})
}

// TestLogJSONWritesJSONDiagnosticsToStderr pins --log-json: the same run must
// emit one parseable JSON object per line, so a log shipper can consume it.
func TestLogJSONWritesJSONDiagnosticsToStderr(t *testing.T) {
	Convey("Given --log-json", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		stdout, err := w.run("--log-json", "sync")

		Convey("When the command runs", func() {
			Convey("Then stderr is JSON, not prose", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldEqual, "nothing to sync\n")

				lines := nonEmptyLines(w.err.String())
				So(lines, ShouldNotBeEmpty)

				var record map[string]any
				So(json.Unmarshal([]byte(lines[0]), &record), ShouldBeNil)
				So(record["msg"], ShouldEqual, "command start")
				So(record["cmd"], ShouldEqual, "verger sync")
			})
		})
	})
}

// TestNoLogFlagsStaySilent keeps the diagnostics opt-in: a plain run must not
// write to stderr at all.
func TestNoLogFlagsStaySilent(t *testing.T) {
	Convey("Given neither --verbose nor --log-json", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		_, err := w.run("sync")

		Convey("When the command runs", func() {
			Convey("Then stderr stays empty", func() {
				So(err, ShouldBeNil)
				So(w.err.String(), ShouldBeEmpty)
			})
		})
	})
}

// TestSyncWithoutSpecIsSuccess pins the empty-state contract: a machine with
// no spec has nothing to reconcile, which is a success, not a usage error.
func TestSyncWithoutSpecIsSuccess(t *testing.T) {
	Convey("Given a home with no spec", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		Convey("When sync runs without -y", func() {
			stdout, err := w.run("sync")

			Convey("Then it reports an empty state and succeeds", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "nothing to sync")
			})
		})

		Convey("When sync runs with -y", func() {
			stdout, err := w.run("sync", "-y")

			Convey("Then it reports the same empty state", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "nothing to sync")
			})
		})
	})
}

// TestSyncWithBrokenSpecStillFails guards the other half of the empty-state
// rule: a spec that exists but does not parse is a real error and must not be
// swallowed as "nothing to sync".
func TestSyncWithBrokenSpecStillFails(t *testing.T) {
	Convey("Given a spec that does not parse", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		writeWorldFile(t, filepath.Join(w.homeDir, "verger.toml"), "schema = 1\n[package\nbroken")

		_, err := w.run("sync")

		Convey("When sync runs", func() {
			Convey("Then it fails rather than reporting an empty state", func() {
				So(err, ShouldNotBeNil)
			})
		})
	})
}

// nonEmptyLines splits captured output into its non-blank lines.
func nonEmptyLines(out string) []string {
	var lines []string

	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}

	return lines
}
