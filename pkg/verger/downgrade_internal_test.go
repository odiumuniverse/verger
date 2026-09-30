package verger

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// Given an installed version and one a channel resolved to
// When the pair is asked whether it moves backwards
// Then the answer follows semver precedence, and only for a package that has a
// channel at all.
func TestDowngradesComparesOnlyWhatItCan(t *testing.T) {
	Convey("Given a package with a channel", t, func() {
		Convey("When the channel resolves to an older version", func() {
			Convey("Then it is a downgrade", func() {
				So(downgrades("stable", "1.2.0", "1.1.0"), ShouldBeTrue)
			})
		})

		Convey("When the channel resolves to a newer version", func() {
			Convey("Then it is not a downgrade", func() {
				So(downgrades("stable", "1.1.0", "1.2.0"), ShouldBeFalse)
			})
		})

		Convey("When the channel resolves to the same version", func() {
			Convey("Then it is not a downgrade", func() {
				// Nothing moves backwards; refusing this would report a
				// problem where there is only a re-install of what is there.
				So(downgrades("stable", "1.2.0", "1.2.0"), ShouldBeFalse)
			})
		})

		Convey("When a release is older than its own prerelease target", func() {
			Convey("Then it is a downgrade, by precedence rather than by number", func() {
				// v2.0.0-rc.1 is *older* than v2.0.0, not newer: a prerelease
				// precedes the release it leads to. A beta channel that
				// resolved to the prerelease while the release is installed is
				// exactly the move this guard exists to refuse.
				So(downgrades("beta", "2.0.0", "2.0.0-rc.1"), ShouldBeTrue)
			})
		})

		Convey("When a prerelease of a later major is older than a release of an earlier one", func() {
			Convey("Then it is an upgrade, because 2.0.0-rc.1 outranks 1.1.0", func() {
				// The case that reads as a downgrade and is not: by semver the
				// prerelease of 2.0.0 is greater than 1.1.0. Calling this a
				// downgrade would skip a real upgrade.
				So(downgrades("beta", "1.1.0", "2.0.0-rc.1"), ShouldBeFalse)
			})
		})
	})
	Convey("Given a package with no channel", t, func() {
		Convey("When the version moves backwards", func() {
			Convey("Then it is not a downgrade", func() {
				// A plain re-sync has no opinion about versions. Giving it one
				// would make every re-sync a question about semver.
				So(downgrades("", "2.0.0", "1.0.0"), ShouldBeFalse)
			})
		})
	})

	Convey("Given an unknown installed or resolved version", t, func() {
		Convey("When either side is missing or unparseable", func() {
			Convey("Then nothing is claimed", func() {
				// Refusing to compare would make a package whose version nobody
				// formats permanently un-updatable; claiming a downgrade would
				// skip an update the user asked for.
				So(downgrades("stable", "", "1.0.0"), ShouldBeFalse)
				So(downgrades("stable", "1.0.0", ""), ShouldBeFalse)
				So(downgrades("stable", "unknown", "1.0.0"), ShouldBeFalse)
				So(downgrades("stable", "1.0.0", "nightly-build"), ShouldBeFalse)
			})
		})
	})
}
