package verger

import (
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/receipt"
)

// TestAChannelThatResolvesOlderEndsToEnd is the integration half of the channel
// feature, with no network: the catalog is a local directory holding a version
// ladder, so the channel resolves to something real on every machine.
//
// The two halves are deliberately opposed, because that is what makes this a
// feature rather than a guard. Without the flag nothing happens and the user is
// told EXACTLY how to make it happen; with the flag the older version is actually
// taken. A test that only pinned the skip would pass if --allow-downgrade did
// nothing at all.
func TestAChannelThatResolvesOlderEndsToEnd(t *testing.T) {
	Convey("Given a channel whose catalog resolves older than what is installed", t, func() {
		world, client := newFacadeWorld(t)

		// Two catalog DIRECTORIES, not one rewritten in place, and the reason is
		// the source cache: a fetch is keyed by reference, so republishing the
		// same path inside one process would hand back the version already
		// fetched and the channel would never resolve lower at all. A registry
		// that publishes an older version is the real shape of this anyway.
		newer := filepath.Join(world.root, "catalog-1")
		older := filepath.Join(world.root, "catalog-0")

		Convey("When the newer version is installed first", func() {
			writeCatalogVersion(t, newer, "1.0.0")
			writeSpecSource(t, world, newer)
			installFromCatalog(t, client, world)
			So(lockVersion(t, world), ShouldEqual, "1.0.0")

			// The ladder moves DOWN after the install. Leaving 1.0.0 published
			// would resolve the channel to the very version already installed,
			// and the guard would correctly do nothing - a test of a case that
			// never existed.
			writeCatalogVersion(t, older, "0.9.0")
			writeSpecSource(t, world, older)

			Convey("Then a channel resolving older is skipped without the flag", func() {
				plan, _, err := client.Sync(t.Context(), SyncOptions{Paths: syncPaths(t, client)})

				Convey("Then the skip is announced with the exact command", func() {
					// M2 from VERIFY-CP-channel.md survived: the SKIP was pinned and
					// the HINT was not, so rewording it to "re-run to take it" left
					// the suite green. When a channel silently will not roll back,
					// this message is the entire UX, so the command is the assertion.
					So(err, ShouldBeNil)

					joined := strings.Join(plan.Notes, "\n")
					So(joined, ShouldContainSubstring, "skipped")
					So(joined, ShouldContainSubstring, "--allow-downgrade")
					So(joined, ShouldContainSubstring, "verger update")
				})

				Convey("Then the lock and the receipt are untouched", func() {
					// A skip that still moved state would be worse than the skip:
					// the user would be looking at a version they never agreed to.
					So(lockVersion(t, world), ShouldEqual, "1.0.0")
					So(receiptVersion(t, world), ShouldEqual, "1.0.0")
				})
			})

			Convey("Then the flag takes the older version", func() {
				plan, report, err := client.Sync(t.Context(), SyncOptions{Paths: syncPaths(t, client), AllowDowngrade: true})

				Convey("Then nothing is skipped", func() {
					So(err, ShouldBeNil)
					So(strings.Join(plan.Notes, "\n"), ShouldNotContainSubstring, "skipped")
					So(report, ShouldNotBeNil)
				})

				Convey("Then the lock moves to the older version", func() {
					So(lockVersion(t, world), ShouldEqual, "0.9.0")
				})

				Convey("Then a receipt for the older version exists", func() {
					So(receiptVersion(t, world), ShouldEqual, "0.9.0")
				})
			})
		})
	})
}

// installCatalogVersion puts one version of the package into the catalog and
// installs it, so the lock and receipts record that version.
func writeCatalogVersion(t *testing.T, catalog, version string) {
	t.Helper()

	dir := filepath.Join(catalog, "caveman")
	writeFacadeFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"),
		`{"name":"acme/caveman","version":"`+version+`","description":"A toolkit."}`)
	writeFacadeFile(t, filepath.Join(dir, "skills", "one", "SKILL.md"), "# "+version+"\n")
}

// writeSpecSource points the spec at one catalog and declares the package whose
// channel is followed.
func writeSpecSource(t *testing.T, world *facadeWorld, catalog string) {
	t.Helper()

	writeFacadeFile(t, filepath.Join(world.user, ".verger", "verger.toml"),
		"schema = 1\n\n[[source]]\nname = \"acme\"\nurl = '"+catalog+"'\n\n"+
			"[[package]]\nid = \"acme/caveman\"\nchannel = \"latest\"\n")
}

// installFromCatalog installs whatever the spec currently resolves.
func installFromCatalog(t *testing.T, client *Client, world *facadeWorld) {
	t.Helper()

	// The fake host writes only the artifacts it was handed, so without this the
	// install produces a receipt with no artifacts. Sync then reads that receipt
	// as "a record of an install whose files are not here" and refuses to call
	// the package installed - which empties the very map the downgrade guard
	// compares against, so the guard could never fire for a reason that has
	// nothing to do with downgrades.
	world.fake.artifacts = []fakeArtifact{{
		Path: filepath.Join(world.user, "delivered", "caveman", "SKILL.md"),
		Data: "# delivered\n",
	}}

	paths, err := client.Paths(User, "")
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}

	plan, err := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{"acme/caveman"}})
	if err != nil {
		t.Fatalf("plan the install: %v", err)
	}

	if _, err := client.Install(t.Context(), plan, ApplyOptions{
		Confirm: YesConfirmer(), Hooks: HooksAsk,
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
}

// syncPaths resolves the user scope a sync runs against.
func syncPaths(t *testing.T, client *Client) Paths {
	t.Helper()

	paths, err := client.Paths(User, "")
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}

	return paths
}

// lockVersion reads the version the lock records for the package.
//
// A parse failure FAILS rather than reporting "". Returning "" for both "no
// lock" and "a lock I could not read" is the silent glob all over again: it is
// how NIGHT-pR-11 reported a clean machine over nine real credential copies.
func lockVersion(t *testing.T, world *facadeWorld) string {
	t.Helper()

	path := filepath.Join(world.user, ".verger", "verger.lock")

	doc, err := lock.ParseFile(path)
	if err != nil {
		t.Fatalf("read the lock at %s: %v", path, err)
	}

	for _, cell := range doc.Cells {
		if strings.HasSuffix(cell.Package, "acme/caveman") {
			return cell.Version
		}
	}

	t.Fatalf("the lock at %s records no acme/caveman cell: %+v", path, doc.Cells)

	return ""
}

// receiptVersion reads the version the newest receipt records.
func receiptVersion(t *testing.T, world *facadeWorld) string {
	t.Helper()

	store := receipt.NewStore(filepath.Join(world.user, ".verger", "state", "receipts"))

	list, err := store.List()
	if err != nil {
		t.Fatalf("list receipts: %v", err)
	}

	if len(list) == 0 {
		t.Fatal("no receipts at all, but the package was installed")
	}

	return list[len(list)-1].Version
}
