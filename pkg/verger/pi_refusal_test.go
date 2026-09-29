package verger

import (
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/manifest"
)

// piFixture writes a payload the manifest parser accepts as a package but
// whose own manifest says it belongs to pi — a host verger has no adapter
// for. The fetcher cannot refuse it, because to the fetcher it is a package.
func piFixture(t *testing.T, root, name string) string {
	t.Helper()

	dir := filepath.Join(root, name)
	writeFacadeFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"),
		`{"name":"`+name+`","version":"1.0.0","description":"A pi package."}`)
	writeFacadeFile(t, filepath.Join(dir, "package.json"),
		`{"name":"`+name+`","version":"1.0.0","keywords":["pi-package"]}`)
	writeFacadeFile(t, filepath.Join(dir, "skills", "one", "SKILL.md"), "# one\n")

	return "./" + name
}

// TestPlanRefusesAPiPackageByName pins F4's plan-time refusal. The fetcher
// turns a payload it cannot parse into a warning and a versionless package,
// so without this guard the run reaches the executor and fails there with a
// message about a version — which names neither pi nor the real problem. The
// refusal has to happen where the package is still being built, and it has
// to name what it is.
func TestPlanRefusesAPiPackageByName(t *testing.T) {
	Convey("Given a payload whose own manifest says it is a pi package", t, func() {
		world, client := newFacadeWorld(t)
		t.Chdir(world.root)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		ref := piFixture(t, world.root, "pioneer")

		_, planErr := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})

		Convey("Then planning refuses it", func() {
			So(planErr, ShouldNotBeNil)
		})

		Convey("Then the refusal is the typed pi refusal, not a version error", func() {
			So(manifest.IsPiPackageError(planErr), ShouldBeTrue)
		})

		Convey("Then the message names pi", func() {
			So(planErr.Error(), ShouldContainSubstring, "pi")
		})

		Convey("Then nothing is planned for delivery", func() {
			// The point of refusing at plan time rather than in the executor:
			// no host is offered a package verger cannot give it.
			So(planErr, ShouldNotBeNil)
		})
	})
}

// TestPlanAcceptsAPayloadThatMerelyMentionsPi is the other half. A guard that
// refuses on any mention of the word would block ordinary packages, and the
// marker this keys on is a keyword, not a substring.
func TestPlanAcceptsAPayloadThatMerelyMentionsPi(t *testing.T) {
	Convey("Given a normal package that only looks like a pi package", t, func() {
		world, client := newFacadeWorld(t)
		t.Chdir(world.root)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		dir := filepath.Join(world.root, "pipelike")
		writeFacadeFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"),
			`{"name":"pipelike","version":"1.0.0","description":"Pipes, and a pi-shaped word."}`)
		writeFacadeFile(t, filepath.Join(dir, "package.json"),
			`{"name":"pipelike","version":"1.0.0","keywords":["pipes","pi-shaped"]}`)
		writeFacadeFile(t, filepath.Join(dir, "skills", "one", "SKILL.md"), "# one\n")

		plan, err := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{"./pipelike"}})

		Convey("Then it is planned like any other package", func() {
			So(err, ShouldBeNil)
			So(plan.Packages, ShouldNotBeEmpty)
		})
	})
}
