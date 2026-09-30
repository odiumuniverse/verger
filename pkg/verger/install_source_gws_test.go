package verger

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// `verger install acme/caveman` on a spec that declares a local catalog
// reached for GitHub, because nothing consulted the spec's `[[source]]` entries
// when it resolved a ref. The spec said where the package lives and the
// install went somewhere else entirely — over the network, to a name the user
// only ever used locally.
func TestInstallResolvesAnIDThroughTheSpecSources(t *testing.T) {
	Convey("Given a spec that declares a local catalog source", t, func() {
		world, client := newFacadeWorld(t)

		// A catalog: one directory per offered package.
		catalog := filepath.Join(world.root, "catalog")
		writeFacadeFile(t, filepath.Join(catalog, "caveman", ".claude-plugin", "plugin.json"),
			`{"name":"acme/caveman","version":"1.0.0","description":"A toolkit."}`)
		writeFacadeFile(t, filepath.Join(catalog, "caveman", "skills", "one", "SKILL.md"), "# one\n")

		writeFacadeFile(t, filepath.Join(world.user, ".verger", "verger.toml"),
			"schema = 1\n\n[[source]]\nname = \"acme\"\nurl = '"+catalog+"'\n")

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		plan, err := client.Plan(t.Context(), PlanOptions{
			Paths: paths, Refs: []string{"acme/caveman"},
		})

		Convey("Then the package comes from the declared source", func() {
			So(err, ShouldBeNil)
			So(plan.Packages, ShouldHaveLength, 1)
			So(plan.Packages[0].Package.ID, ShouldEqual, "acme/caveman")
		})
	})
}

// The other half of the rule: a ref no declared source offers is a loud error
// at the user's terminal, not a silent trip to the default registry to look
// for it somewhere else.
func TestInstallRefusesAnIDNoSourceOffers(t *testing.T) {
	Convey("Given a spec whose sources do not offer the id", t, func() {
		world, client := newFacadeWorld(t)

		catalog := filepath.Join(world.root, "catalog")
		writeFacadeFile(t, filepath.Join(catalog, "caveman", ".claude-plugin", "plugin.json"),
			`{"name":"acme/caveman","version":"1.0.0","description":"A toolkit."}`)

		writeFacadeFile(t, filepath.Join(world.user, ".verger", "verger.toml"),
			"schema = 1\n\n[[source]]\nname = \"acme\"\nurl = '"+catalog+"'\n")

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		_, err = client.Plan(t.Context(), PlanOptions{
			Paths: paths, Refs: []string{"acme/absent"},
		})

		Convey("Then it fails naming the id and the sources that were asked", func() {
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "acme/absent")
			So(err.Error(), ShouldContainSubstring, "acme")
		})

		Convey("And nothing was fetched on the way to the refusal", func() {
			// The refusal happens before the fetcher builds a cache dir at
			// all, so the proof is that there is nothing to find: no entry
			// for this id, and no network round trip to have made one.
			_, readErr := os.ReadDir(client.Store().CacheDir())
			So(os.IsNotExist(readErr), ShouldBeTrue)
		})
	})
}

// A spec that declares no source keeps the old behaviour, and so does a ref
// that says where it comes from: an explicit `github:` prefix, a URL, a path
// and an npm specifier all name their own origin.
func TestInstallKeepsSelfDescribingRefsWorking(t *testing.T) {
	Convey("Given a spec with no declared source", t, func() {
		world, client := newFacadeWorld(t)
		world.fixture(t, "plain", "1.0.0")

		writeFacadeFile(t, filepath.Join(world.user, ".verger", "verger.toml"), "schema = 1\n")

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		doc, _, err := LoadSpec(paths.SpecPath)
		So(err, ShouldBeNil)
		So(doc.Sources, ShouldBeEmpty)

		plan, err := client.Plan(t.Context(), PlanOptions{
			Paths: paths, Refs: []string{"./plain"},
		})

		Convey("Then a self-describing ref still fetches", func() {
			So(err, ShouldBeNil)
			So(plan.Packages, ShouldHaveLength, 1)
		})
	})

	Convey("Given a spec that declares a source", t, func() {
		world, client := newFacadeWorld(t)
		world.fixture(t, "plain", "1.0.0")

		catalog := filepath.Join(world.root, "catalog")
		writeFacadeFile(t, filepath.Join(catalog, "caveman", ".claude-plugin", "plugin.json"),
			`{"name":"acme/caveman","version":"1.0.0","description":"A toolkit."}`)

		writeFacadeFile(t, filepath.Join(world.user, ".verger", "verger.toml"),
			"schema = 1\n\n[[source]]\nname = \"acme\"\nurl = '"+catalog+"'\n")

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		plan, err := client.Plan(t.Context(), PlanOptions{
			Paths: paths, Refs: []string{"./plain"},
		})

		Convey("Then a declared source does not hijack a ref that names its own origin", func() {
			// "./plain" is local by its own grammar. A source must not be
			// able to answer for it, or a spec with one catalog would make
			// every other way of naming a package depend on it.
			So(err, ShouldBeNil)
			So(plan.Packages, ShouldHaveLength, 1)
			So(plan.Packages[0].Package.ID, ShouldNotEqual, "acme/caveman")
		})
	})
}
