package verger

import (
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
)

// unversionedFixture writes a package whose manifest names no version, and
// returns the ref for it. It is the shape a hand-written manifest takes, and
// the shape the executor used to discover halfway through a run.
func unversionedFixture(t *testing.T, root, name string) string {
	t.Helper()

	dir := filepath.Join(root, name)
	writeFacadeFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"),
		`{"name":"`+name+`","description":"A toolkit with no version."}`)
	writeFacadeFile(t, filepath.Join(dir, "skills", "one", "SKILL.md"), "# one\n")

	return "./" + name
}

// TestPlanRefusesAPackageWithNoVersion pins the failure where it can still be
// useful. The executor checks the same fact, but it checks after the user has
// read a table of hosts that are about to be written — a plan that cannot run
// should not be printed as if it could.
func TestPlanRefusesAPackageWithNoVersion(t *testing.T) {
	Convey("Given a package whose manifest declares no version", t, func() {
		world, client := newFacadeWorld(t)
		t.Chdir(world.root)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		ref := unversionedFixture(t, world.root, "unversioned")

		_, err = client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})

		Convey("Then planning refuses it, before any host table is printed", func() {
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "version")
		})
	})
}

// TestDeliveryWithNoArtifactsIsSkippedNotDelivered pins the rule that ended
// the "delivered over an empty home" report: a cell may only say it wrote
// files when the receipt carries those files with their digests. Zero
// artifacts is a fact about the package, and it is reported as one.
func TestDeliveryWithNoArtifactsIsSkippedNotDelivered(t *testing.T) {
	Convey("Given a package the adapter has nothing to write for", t, func() {
		world, client := newFacadeWorld(t)
		t.Chdir(world.root)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		// A real fixture, and an adapter that writes nothing: the shape a
		// manifest with no matching files produces.
		ref := world.fixture(t, "caveman", "1.2.3")

		plan, err := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})
		So(err, ShouldBeNil)

		world.fake.artifacts = nil

		report, err := client.Install(t.Context(), plan, ApplyOptions{Now: fixedClock})
		So(err, ShouldBeNil)
		So(report, ShouldNotBeNil)
		So(report.Cells, ShouldNotBeEmpty)

		Convey("Then the cell is skipped, not current", func() {
			So(report.Cells[0].Status, ShouldEqual, apply.StatusSkipped)
		})

		Convey("Then the cell says why", func() {
			notes := strings.Join(report.Cells[0].Notes, "\n")

			So(notes, ShouldContainSubstring, "nothing to write")
		})
	})
}

// TestDeliveryWithArtifactsIsCurrent is the other half of the same rule: a
// guard that reported everything as skipped would pass the test above and be
// just as wrong. "current" still has to mean files were written.
func TestDeliveryWithArtifactsIsCurrent(t *testing.T) {
	Convey("Given a package the adapter does write", t, func() {
		world, client := newFacadeWorld(t)
		t.Chdir(world.root)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		ref := world.fixture(t, "caveman", "1.2.3")

		plan, err := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})
		So(err, ShouldBeNil)

		world.fake.artifacts = []fakeArtifact{
			{Path: filepath.Join(world.root, "host", "one.md"), Data: "# one\n"},
		}

		report, err := client.Install(t.Context(), plan, ApplyOptions{Now: fixedClock})
		So(err, ShouldBeNil)

		Convey("Then the cell is current", func() {
			So(report.Cells[0].Status, ShouldEqual, apply.StatusCurrent)
		})
	})
}
