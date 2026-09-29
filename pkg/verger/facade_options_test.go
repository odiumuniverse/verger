package verger

import (
	"path/filepath"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/spec"
	"github.com/odiumuniverse/verger/pkg/store"

	"github.com/vmkteam/embedlog"
)

// TestInstallPlanHonoursDisabled pins that `disabled = true` in the spec is a
// decision, not a hint. It is read on the sync path and ignored on install, so
// `verger install` happily wrote a package the user had explicitly turned
// off — and the spec said so, in writing, one line above.
func TestInstallPlanHonoursDisabled(t *testing.T) {
	Convey("Given a spec that marks the package disabled", t, func() {
		world, client := newFacadeWorld(t)
		t.Chdir(world.root)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		doc, _, err := LoadSpec(paths.SpecPath)
		So(err, ShouldBeNil)

		doc.Packages = []spec.Package{{ID: "local:caveman", Version: "1.2.3", Disabled: true}}
		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		ref := world.fixture(t, "caveman", "1.2.3")

		plan, err := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})
		So(err, ShouldBeNil)

		Convey("Then the package is not planned for delivery", func() {
			So(plan.Packages, ShouldBeEmpty)
		})

		Convey("Then installing writes nothing", func() {
			world.fake.artifacts = []fakeArtifact{
				{Path: filepath.Join(world.root, "host", "one.md"), Data: "# one\n"},
			}

			_, applyErr := client.Install(t.Context(), plan, ApplyOptions{Now: fixedClock})
			So(applyErr, ShouldBeNil)

			world.fake.mu.Lock()
			delivers := world.fake.delivers
			world.fake.mu.Unlock()

			So(delivers, ShouldEqual, 0)
		})
	})

	Convey("Given a spec that does not disable the package", t, func() {
		Convey("Then the same install still plans and delivers", func() {
			world, client := newFacadeWorld(t)
			t.Chdir(world.root)

			paths, err := client.Paths(User, "")
			So(err, ShouldBeNil)

			doc, _, err := LoadSpec(paths.SpecPath)
			So(err, ShouldBeNil)

			doc.Packages = []spec.Package{{ID: "local:caveman", Version: "1.2.3"}}
			So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

			ref := world.fixture(t, "caveman", "1.2.3")

			plan, err := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})
			So(err, ShouldBeNil)
			So(plan.Packages, ShouldNotBeEmpty)
		})
	})
}

// TestWithTrashRetentionReachesTheStore pins that the option is not decorative.
// A facade option that sets a field nobody reads is a promise the caller
// believes and the machine ignores.
func TestWithTrashRetentionReachesTheStore(t *testing.T) {
	Convey("Given a client opened with a trash retention window", t, func() {
		world, _ := newFacadeWorld(t)
		_ = world

		client, err := Open(t.Context(), WithTrashRetention(90*24*time.Hour))
		So(err, ShouldBeNil)

		Convey("Then the store reports that window", func() {
			So(client.Store().TrashRetention(), ShouldEqual, 90*24*time.Hour)
		})
	})

	Convey("Given a client opened without one", t, func() {
		Convey("Then the store keeps its own default", func() {
			client, err := Open(t.Context())
			So(err, ShouldBeNil)
			So(client.Store().TrashRetention(), ShouldEqual, store.DefaultRetention)
		})
	})

	Convey("Given a non-positive window", t, func() {
		Convey("Then it is ignored, not taken as keep-nothing", func() {
			client, err := Open(t.Context(), WithTrashRetention(0))
			So(err, ShouldBeNil)
			So(client.Store().TrashRetention(), ShouldEqual, store.DefaultRetention)
		})
	})
}

// TestWithLoggerReachesTheExecutor pins that the logger is used, not merely
// stored. The field test above passes while the executor is handed a default
// logger, and then `--verbose` prints nothing and reports success: a flag
// wired to a variable nobody reads.
func TestWithLoggerReachesTheExecutor(t *testing.T) {
	log := embedlog.NewDevLogger()

	Convey("Given a client opened with a dev logger", t, func() {
		world, _ := newFacadeWorld(t)
		t.Chdir(world.root)

		client, err := Open(t.Context(), WithHosts(world.fake), WithLogger(log))
		So(err, ShouldBeNil)

		Convey("Then the run options carry that same logger", func() {
			opts := client.execOptions(ApplyOptions{})
			So(opts.Logger == log, ShouldBeTrue)
		})
	})

	Convey("Given a client opened without one", t, func() {
		world, _ := newFacadeWorld(t)

		client, err := Open(t.Context(), WithHosts(world.fake))
		So(err, ShouldBeNil)

		Convey("Then the executor gets the default, not a caller's", func() {
			// A default that logs somewhere is fine; what would not be fine
			// is a caller who passed --verbose and got this one.
			opts := client.execOptions(ApplyOptions{})
			So(opts.Logger == log, ShouldBeFalse)
		})
	})
}
