package cli

import (
	"context"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// TestUnavailableHostsExplainsTheCause pins that a host this build ships no
// adapter for is never reported as a host that is merely undetected: the user
// must be able to tell "not implemented yet" from "install the CLI".
func TestUnavailableHostsExplainsTheCause(t *testing.T) {
	Convey("Given a requested host without a registered adapter", t, func() {
		err := verger.UnavailableHostsError(
			map[string]bool{"agy": true},
			map[string]bool{"claude": true, "codex": true},
			nil,
		)

		Convey("When the failure is reported", func() {
			Convey("Then it says the adapter is not in this build, not that it is undetected", func() {
				So(err.Error(), ShouldContainSubstring, "agy")
				So(err.Error(), ShouldContainSubstring, "no adapter in this build")
				So(err.Error(), ShouldNotContainSubstring, "not detected")
			})
		})
	})

	Convey("Given a registered adapter the machine does not detect", t, func() {
		err := verger.UnavailableHostsError(
			map[string]bool{"codex": true},
			map[string]bool{"claude": true, "codex": true},
			nil,
		)

		Convey("When the failure is reported", func() {
			Convey("Then it says the host is supported and undetected", func() {
				So(err.Error(), ShouldContainSubstring, "codex")
				So(err.Error(), ShouldContainSubstring, "adapter registered, but not detected")
				So(err.Error(), ShouldNotContainSubstring, "no adapter in this build")
			})
		})
	})

	Convey("Given a requested host the user excluded", t, func() {
		err := verger.UnavailableHostsError(
			map[string]bool{"omp": true},
			map[string]bool{"omp": true},
			map[string]bool{"omp": true},
		)

		Convey("When the failure is reported", func() {
			Convey("Then it names --except rather than a detection problem", func() {
				So(err.Error(), ShouldContainSubstring, "excluded by --except")
			})
		})
	})

	Convey("Given requested hosts of every kind at once", t, func() {
		err := verger.UnavailableHostsError(
			map[string]bool{"omp": true, "kilo": true, "gemini": true},
			map[string]bool{"omp": true, "gemini": true},
			map[string]bool{"omp": true},
		)

		Convey("When the failure is reported", func() {
			Convey("Then every requested host is named with its own cause", func() {
				So(err.Error(), ShouldContainSubstring, "kilo (no adapter in this build")
				So(err.Error(), ShouldContainSubstring, "omp (excluded by --except)")
				So(err.Error(), ShouldContainSubstring, "gemini (adapter registered, but not detected")
			})
		})
	})
}

// TestHostsFlagNamesBothCauses runs the two cases through the command line: a
// known id with no adapter in this build, and a registered adapter on a machine
// where nothing is detected (empty PATH, empty HOME).
func TestHostsFlagNamesBothCauses(t *testing.T) {
	Convey("Given a build without an adapter for the requested host", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.fixture(t)

		_, err := w.run("install", "./fixture", "-y", "--hosts", "agy")

		Convey("When install names a host this build does not implement", func() {
			Convey("Then the error says the adapter is not in this build", func() {
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "agy (no adapter in this build")
			})
		})
	})

	Convey("Given a registered adapter and a machine where no host is detected", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.fixture(t)

		// No injected hosts: the real factory registers every landed adapter,
		// and an empty PATH with the temp HOME leaves all of them undetected.

		w.hosts = nil

		t.Setenv("PATH", "")

		_, err := w.run("install", "./fixture", "-y", "--hosts", "codex")

		Convey("When install names a supported host that is not detected", func() {
			Convey("Then the error says supported-but-undetected, not unimplemented", func() {
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "codex (adapter registered, but not detected")
				So(err.Error(), ShouldNotContainSubstring, "no adapter in this build")
			})
		})
	})
}

// TestAdapterIDsCoversTheFactory keeps the registry the error message reads in
// step with the adapters the CLI actually builds.
func TestAdapterIDsCoversTheFactory(t *testing.T) {
	Convey("Given the default host factory", t, func() {
		newWorld(t)

		client, err := verger.Open(context.Background())
		So(err, ShouldBeNil)

		ids := verger.AdapterIDs(newApp(Options{}).hosts(client))

		Convey("When the adapters are built", func() {
			Convey("Then every landed adapter is in the registry the message reads", func() {
				for _, id := range host.All() {
					So(ids[string(id)], ShouldBeTrue)
				}
			})
		})
	})
}
