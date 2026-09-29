package verger

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
)

// carriedHome is a home as a second machine receives it: the portable part —
// spec, lock and the local packages themselves — and nothing machine-local.
// Receipts, journal, consent, secrets, trust, lease and the file lock are
// deliberately left behind, which is the situation that made `status` say
// "delivered" about files that were not there.
type carriedHome struct {
	paths  Paths
	client *Client
	target string
	// host is the adapter the first machine delivered through. The cloned
	// client has to be given it explicitly: a fresh Open with no adapters
	// falls back to detecting the agents installed on whatever machine is
	// running the test, so the test would pass only where an agent happens
	// to be installed — and fail on a clean CI box.
	host host.Host
}

func newCarriedHome(t *testing.T) *carriedHome {
	t.Helper()

	world, client := newFacadeWorld(t)
	t.Chdir(world.root)

	paths, err := client.Paths(User, "")
	So(err, ShouldBeNil)

	ref := world.fixture(t, "caveman", "1.2.3")
	target := filepath.Join(world.root, "host", "one.md")

	world.fake.artifacts = []fakeArtifact{{Path: target, Data: "# one\n"}}

	plan, err := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})
	So(err, ShouldBeNil)

	_, err = client.Install(t.Context(), plan, ApplyOptions{Now: fixedClock})
	So(err, ShouldBeNil)

	return &carriedHome{paths: paths, client: client, target: target, host: world.fake}
}

// carryTo copies only the portable part into a fresh home and opens a client
// over it, standing in for `git clone` on another machine.
func (c *carriedHome) carryTo(t *testing.T) *Client {
	t.Helper()

	dir := filepath.Join(t.TempDir(), ".verger")
	So(os.MkdirAll(dir, 0o700), ShouldBeNil)

	for _, name := range []string{"verger.toml", "verger.lock"} {
		data, err := os.ReadFile(filepath.Join(c.paths.Root, name)) //nolint:gosec // G304: the test names its own fixture
		So(err, ShouldBeNil)

		So(os.WriteFile(filepath.Join(dir, name), data, 0o600), ShouldBeNil) //nolint:gosec // G703: the path is a Join of a temp dir and a name the test itself wrote
	}

	So(os.MkdirAll(filepath.Join(dir, "state"), 0o700), ShouldBeNil)

	client, err := Open(t.Context(), WithHome(dir), WithHosts(c.host))
	So(err, ShouldBeNil)

	t.Cleanup(func() { _ = client.Close() })

	return client
}

// TestClonedVaultRedeliversWhatIsNotThere pins FAIL4-B.
//
// Copying spec and lock to a machine with no receipts and no files used to
// produce a home that reported "delivered" and synced "nothing to do": the
// lock said the package was installed, and nothing contradicted it. A cell is
// only "what should be there" until something on disk agrees.
func TestClonedVaultRedeliversWhatIsNotThere(t *testing.T) {
	Convey("Given a home whose portable part was carried to a fresh machine", t, func() {
		carried := newCarriedHome(t)
		client := carried.carryTo(t)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		Convey("Then status does not claim the package is delivered", func() {
			status, statusErr := client.Status(t.Context(), StatusOptions{Paths: paths})
			So(statusErr, ShouldBeNil)
			So(status.Cells, ShouldNotBeEmpty)

			for _, cell := range status.Cells {
				So(cell.Status, ShouldNotEqual, StatusCurrent)
			}
		})

		Convey("Then sync redelivers it", func() {
			// No receipt and no file: this is a restore onto a new machine,
			// not a conflict, so it must happen quietly and succeed.
			plan, syncErr := client.SyncPlan(t.Context(), SyncOptions{Paths: paths})
			So(syncErr, ShouldBeNil)
			So(plan.Install, ShouldNotBeEmpty)

			_, _, syncErr = client.Sync(t.Context(), SyncOptions{Paths: paths, Hooks: HooksSkip, Confirm: yesOnce{}})
			So(syncErr, ShouldBeNil)
		})
	})
}

// TestDeletedFileIsRedelivered pins rule 4: a receipt that outlived its files
// is not evidence the files are still there.
func TestDeletedFileIsRedelivered(t *testing.T) {
	Convey("Given a home whose delivered file was deleted by hand", t, func() {
		carried := newCarriedHome(t)

		So(os.Remove(carried.target), ShouldBeNil)

		status, statusErr := carried.client.Status(t.Context(), StatusOptions{Paths: carried.paths})
		So(statusErr, ShouldBeNil)

		Convey("Then the cell is reported missing, not current", func() {
			// The receipt is intact, so before the fix the cell read "current"
			// and `verger sync` had nothing to do.
			So(status.Cells, ShouldNotBeEmpty)
			So(status.Cells[0].Status, ShouldEqual, StatusMissing)
		})

		Convey("Then sync reinstalls it", func() {
			_, _, syncErr := carried.client.Sync(t.Context(), SyncOptions{Paths: carried.paths, Hooks: HooksSkip})
			So(syncErr, ShouldBeNil)

			_, statErr := os.Stat(carried.target)
			So(statErr, ShouldBeNil)
		})
	})
}

// TestEditedFileIsNotOverwritten pins rule 3, and the safety rule behind it:
// verger must never overwrite an edit the user made. A plain delivered file
// that has moved is the user's work, not a stale install, so the cell is
// hands-off and sync declines instead of reinstalling over it.
//
// RED until the write-file digest is compared: markDrift covers config keys
// and MCP servers, not ordinary files.
func TestEditedFileIsNotOverwritten(t *testing.T) {
	Convey("Given a delivered file the user edited", t, func() {
		carried := newCarriedHome(t)

		So(os.WriteFile(carried.target, []byte("# mine\n"), 0o600), ShouldBeNil)

		status, statusErr := carried.client.Status(t.Context(), StatusOptions{Paths: carried.paths})
		So(statusErr, ShouldBeNil)

		Convey("Then the cell is hands-off rather than missing or current", func() {
			So(status.Cells, ShouldHaveLength, 1)
			So(status.Cells[0].Status, ShouldEqual, StatusHandsOff)
		})

		Convey("Then sync declines rather than overwriting the edit", func() {
			_, _, syncErr := carried.client.Sync(t.Context(), SyncOptions{
				Paths: carried.paths, Hooks: HooksSkip, Confirm: yesOnce{},
			})

			So(syncErr, ShouldNotBeNil)

			Convey("And the edit survives", func() {
				data, readErr := os.ReadFile(carried.target) //nolint:gosec // G304: the test names its own fixture
				So(readErr, ShouldBeNil)
				So(string(data), ShouldEqual, "# mine\n")
			})
		})
	})
}

// TestForcedSyncBacksUpTheUserEdit pins what --force must never do: lose the
// edit. The overwrite is the point, so the target gets our content; the copy
// under state/backups is what makes that safe, and its path is reported so a
// person can find it without guessing.
func TestForcedSyncBacksUpTheUserEdit(t *testing.T) {
	Convey("Given a delivered file the user edited", t, func() {
		carried := newCarriedHome(t)

		So(os.WriteFile(carried.target, []byte("# mine\n"), 0o600), ShouldBeNil)

		_, report, forceErr := carried.client.Sync(t.Context(), SyncOptions{
			Paths: carried.paths, Hooks: HooksSkip, Confirm: yesOnce{}, Force: true,
		})
		So(forceErr, ShouldBeNil)

		Convey("Then the file is ours again", func() {
			data, readErr := os.ReadFile(carried.target) //nolint:gosec // G304: the test names its own fixture
			So(readErr, ShouldBeNil)
			So(string(data), ShouldEqual, "# one\n")
		})

		Convey("And the edit is kept under state/backups and reported", func() {
			So(report, ShouldNotBeNil)
			So(report.Cells, ShouldNotBeEmpty)
			So(report.Cells[0].Backup, ShouldNotBeEmpty)

			kept, readErr := os.ReadFile(report.Cells[0].Backup) //nolint:gosec // G304: the test names its own fixture
			So(readErr, ShouldBeNil)
			So(string(kept), ShouldEqual, "# mine\n")
		})
	})
}

// TestRestoredFromLockSaysSo pins the word "restored": a package this machine
// has no receipt for arrived from the lock, and what the executor wrote is a
// restore. Calling that an install is how a user ends up wondering what
// touched their home.
func TestRestoredFromLockSaysSo(t *testing.T) {
	Convey("Given a lock and a spec on a machine with no receipt", t, func() {
		carried := newCarriedHome(t)

		// Drop the receipts: what is left is spec plus lock, which is what a
		// second machine receives.
		entries, readErr := os.ReadDir(carried.paths.ReceiptsDir)
		So(readErr, ShouldBeNil)

		for _, entry := range entries {
			So(os.RemoveAll(filepath.Join(carried.paths.ReceiptsDir, entry.Name())), ShouldBeNil)
		}

		_, report, syncErr := carried.client.Sync(t.Context(), SyncOptions{
			Paths: carried.paths, Hooks: HooksSkip, Confirm: yesOnce{},
		})
		So(syncErr, ShouldBeNil)
		So(report, ShouldNotBeNil)
		So(report.Cells, ShouldNotBeEmpty)

		Convey("Then the cell is marked restored", func() {
			So(report.Cells[0].Restored, ShouldBeTrue)
		})
	})
}

// TestForcedSyncGivesEachCellItsOwnBackup pins that the backup path is per
// cell. `verger sync --force` with no package named forces every drifted
// cell, and a single shared path would leave all but the last pointing at a
// copy that is not theirs — which is the one way "we kept your edit" can be
// a lie.
func TestForcedSyncGivesEachCellItsOwnBackup(t *testing.T) {
	Convey("Given two hosts whose delivered file the user edited", t, func() {
		world, _ := newFacadeWorld(t)
		t.Chdir(world.root)

		one := &fakeHost{id: host.Claude}
		two := &fakeHost{id: host.Omp}

		client, err := Open(t.Context(), WithHosts(one, two))
		So(err, ShouldBeNil)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		ref := world.fixture(t, "caveman", "1.2.3")
		first := filepath.Join(world.root, "host", "claude.md")
		second := filepath.Join(world.root, "host", "omp.md")

		one.artifacts = []fakeArtifact{{Path: first, Data: "# one\n"}}
		two.artifacts = []fakeArtifact{{Path: second, Data: "# two\n"}}

		plan, err := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})
		So(err, ShouldBeNil)

		_, err = client.Install(t.Context(), plan, ApplyOptions{Now: fixedClock})
		So(err, ShouldBeNil)

		So(os.WriteFile(first, []byte("# mine one\n"), 0o600), ShouldBeNil)
		So(os.WriteFile(second, []byte("# mine two\n"), 0o600), ShouldBeNil)

		_, report, forceErr := client.Sync(t.Context(), SyncOptions{
			Paths: paths, Hooks: HooksSkip, Confirm: yesOnce{}, Force: true,
		})
		So(forceErr, ShouldBeNil)
		So(report.Cells, ShouldHaveLength, 2)

		Convey("Then each cell names its own copy, not the other's", func() {
			backups := map[host.ID]string{}

			for _, cell := range report.Cells {
				So(cell.Backup, ShouldNotBeEmpty)
				backups[cell.Host] = cell.Backup
			}

			So(backups[host.Claude], ShouldNotEqual, backups[host.Omp])

			claudeCopy, readErr := os.ReadFile(backups[host.Claude]) //nolint:gosec // G304: the test names its own fixture
			So(readErr, ShouldBeNil)
			So(string(claudeCopy), ShouldEqual, "# mine one\n")

			ompCopy, readErr := os.ReadFile(backups[host.Omp]) //nolint:gosec // G304: the test names its own fixture
			So(readErr, ShouldBeNil)
			So(string(ompCopy), ShouldEqual, "# mine two\n")
		})
	})
}

// yesOnce answers every question "yes", so a redelivery driven by the test
// is not stopped by the consent gate an unattended run would hit.
type yesOnce struct{}

func (yesOnce) Confirm(context.Context, apply.Question) (bool, error) { return true, nil }
