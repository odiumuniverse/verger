package verger

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// TestFindInstalledMatchesTheHostsSelectorSpelling pins ORACLE-CLAUDE-1:
// findInstalled must peel the marketplace suffix off the selector before
// comparing, because the oracle splits id at the last @.
func TestFindInstalledMatchesTheHostsSelectorSpelling(t *testing.T) {
	listed := []host.Installed{
		{Name: "beadle-canon", Marketplace: "beadle", Version: "0.0.0-4489052ce4dd"},
		{Name: "frontend-design", Marketplace: "claude-plugins-official", Version: "fbe07fb6ce7d"},
	}

	Convey("Given a selector in the host's own <name>@<marketplace> spelling", t, func() {
		entry, found := findInstalled(listed, "frontend-design@claude-plugins-official")

		Convey("Then it finds the entry whose marketplace the parser split off", func() {
			So(found, ShouldBeTrue)
			So(entry.Name, ShouldEqual, "frontend-design")
			So(entry.Version, ShouldEqual, "fbe07fb6ce7d")
		})
	})

	Convey("Given a bare name, with no marketplace at all", t, func() {
		entry, found := findInstalled(listed, "beadle-canon")

		Convey("Then it still matches", func() {
			So(found, ShouldBeTrue)
			So(entry.Marketplace, ShouldEqual, "beadle")
		})
	})

	Convey("Given the right name and the wrong marketplace", t, func() {
		_, found := findInstalled(listed, "frontend-design@somebody-else")

		Convey("Then it does not match", func() {
			So(found, ShouldBeFalse)
		})
	})
}

// TestPlanScopePinsDeliveryProject pins API-REQ 8/7: PlanOptions.Scope is
// threaded into host.Delivery.Project so the adapters' project plumbing is
// reachable live.
func TestPlanScopePinsDeliveryProject(t *testing.T) {
	Convey("Given a plan at project scope", t, func() {
		world, client := newFacadeWorld(t)

		ref := world.fixture(t, "caveman", "1.2.3")

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		Convey("When the plan is installed with a project root", func() {
			plan, planErr := client.Plan(t.Context(), PlanOptions{
				Paths: paths,
				Refs:  []string{ref},
				Scope: world.project,
			})
			So(planErr, ShouldBeNil)

			_, applyErr := client.Install(t.Context(), plan, ApplyOptions{Scope: world.project})
			So(applyErr, ShouldBeNil)

			Convey("Then the adapter received the project root", func() {
				// The adapter's own observation is the only proof that the
				// scope reached the delivery, so read it there.
				So(world.fake.projects, ShouldNotBeEmpty)

				for _, project := range world.fake.projects {
					So(project, ShouldEqual, world.project)
				}
			})
		})
	})
}

// TestEjectAndApproveHooksForReturnTypedErrors pins API-REQ-FIX: both methods
// fail with their own exported error type, so a caller matches with errors.As
// and never reads the message.
func TestEjectAndApproveHooksForReturnTypedErrors(t *testing.T) {
	Convey("Given a client whose Eject target is not a usable home", t, func() {
		_, client := newFacadeWorld(t)

		// A regular file is not a home: Open refuses it.
		blocker := filepath.Join(t.TempDir(), "blocked")
		So(os.WriteFile(blocker, []byte("not a home\n"), 0o600), ShouldBeNil)

		Convey("Then Eject reports an *EjectError naming the target", func() {
			err := client.Eject(t.Context(), blocker)
			So(err, ShouldNotBeNil)

			ejected, ok := errors.AsType[*EjectError](err)
			So(ok, ShouldBeTrue)
			So(ejected.Target, ShouldEqual, blocker)
			So(ejected.Unwrap(), ShouldNotBeNil)
		})
	})

	Convey("Given a client whose consent store cannot be written", t, func() {
		world, client := newFacadeWorld(t)

		Convey("Then ApproveHooksFor reports a *HookApprovalError", func() {
			// Replace the state directory with a regular file: creating
			// secrets.json underneath it cannot succeed.
			state := client.Home().StateDir()
			So(os.MkdirAll(filepath.Dir(state), 0o700), ShouldBeNil)
			So(os.RemoveAll(state), ShouldBeNil)
			So(os.WriteFile(state, []byte("not a directory\n"), 0o600), ShouldBeNil)

			_ = world

			sum := digest.Bytes([]byte("hook"))

			_, err := client.ApproveHooksFor("local:caveman", "claude", sum)
			So(err, ShouldNotBeNil)

			approval, ok := errors.AsType[*HookApprovalError](err)
			So(ok, ShouldBeTrue)
			So(approval.Package, ShouldEqual, "local:caveman")
			So(approval.Host, ShouldEqual, "claude")
			So(approval.Unwrap(), ShouldNotBeNil)
		})
	})
}

// TestSyncReportsNewerSchemaBeforeAnythingElse pins W8-COEXIST-1 FAIL5: a lock
// or spec written by a newer verger must be reported as RefusalSchemaNewer
// before any other verdict — including "no spec at ...", which used to win
// and made `verger sync` exit 0 over a lock this build cannot read.
func TestSyncReportsNewerSchemaBeforeAnythingElse(t *testing.T) {
	Convey("Given a lock written by a newer verger and a readable spec", t, func() {
		world, client := newFacadeWorld(t)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		doc, _, err := LoadSpec(paths.SpecPath)
		So(err, ShouldBeNil)
		AddSpecPackage(doc, spec.Package{ID: "acme/caveman", Version: "1.2.3"})
		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		lockPath := filepath.Join(client.Home().Root(), "verger.lock")
		So(os.MkdirAll(client.Home().Root(), 0o700), ShouldBeNil)
		So(os.WriteFile(lockPath, []byte(`{"schema":99,"cells":[]}`), 0o600), ShouldBeNil)

		_ = world

		Convey("Then sync reports a schema-newer refusal, not success", func() {
			_, syncErr := client.SyncPlan(t.Context(), SyncOptions{Paths: paths})
			So(syncErr, ShouldNotBeNil)
			So(Classify(syncErr), ShouldEqual, RefusalSchemaNewer)

			Convey("And the message names the versions and the way out", func() {
				So(syncErr.Error(), ShouldContainSubstring, "written by a newer verger")
				So(syncErr.Error(), ShouldContainSubstring, "99")
				So(syncErr.Error(), ShouldContainSubstring, "update verger/beadle")
			})
		})

		Convey("Then nothing was written back over the newer lock", func() {
			_, _ = client.SyncPlan(t.Context(), SyncOptions{Paths: paths})

			after, readErr := os.ReadFile(lockPath) //nolint:gosec // G304: the path is the lock the test itself wrote two lines up
			So(readErr, ShouldBeNil)
			So(string(after), ShouldEqual, `{"schema":99,"cells":[]}`)
		})
	})

	Convey("Given a newer lock and no spec at all", t, func() {
		world, client := newFacadeWorld(t)
		_ = world

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		lockPath := filepath.Join(client.Home().Root(), "verger.lock")
		So(os.MkdirAll(client.Home().Root(), 0o700), ShouldBeNil)
		So(os.WriteFile(lockPath, []byte(`{"schema":99,"cells":[]}`), 0o600), ShouldBeNil)

		Convey("Then the schema verdict wins over the missing spec", func() {
			// Before the fix this was *UsageError "no spec at ...", which the
			// CLI turned into its usage path rather than exit 7.
			_, syncErr := client.SyncPlan(t.Context(), SyncOptions{Paths: paths})
			So(syncErr, ShouldNotBeNil)
			So(Classify(syncErr), ShouldEqual, RefusalSchemaNewer)
		})
	})
}

// TestPlanReportsANewerSpecSchema pins the spec half of the same rule. The
// lock is read first and refused first, so a test that only writes a newer
// lock never reaches the spec branch — which is exactly how a spec written by
// a newer verger came to be reported as something other than "update", and
// rewritten by an older build that does not understand it.
func TestPlanReportsANewerSpecSchema(t *testing.T) {
	Convey("Given a spec written by a newer verger and no lock", t, func() {
		world, client := newFacadeWorld(t)
		_ = world

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		So(os.MkdirAll(filepath.Dir(paths.SpecPath), 0o700), ShouldBeNil)

		So(os.WriteFile(paths.SpecPath, []byte("schema = 99\n"), 0o600), ShouldBeNil)

		Convey("Then planning is refused as schema-newer", func() {
			_, planErr := client.Plan(t.Context(), PlanOptions{Paths: paths})
			So(planErr, ShouldNotBeNil)
			So(Classify(planErr), ShouldEqual, RefusalSchemaNewer)
		})

		Convey("Then sync is refused as schema-newer too", func() {
			_, syncErr := client.SyncPlan(t.Context(), SyncOptions{Paths: paths})
			So(syncErr, ShouldNotBeNil)
			So(Classify(syncErr), ShouldEqual, RefusalSchemaNewer)
		})

		Convey("Then the newer spec is not rewritten", func() {
			_, _ = client.Plan(t.Context(), PlanOptions{Paths: paths})

			after, readErr := os.ReadFile(paths.SpecPath)
			So(readErr, ShouldBeNil)
			So(string(after), ShouldEqual, "schema = 99\n")
		})

		Convey("Then the schema verdict wins over every later question", func() {
			// The observable difference: checkSchemaVersions runs before
			// trust, hosts and fetching, so a newer spec is refused as a
			// schema whatever else the run would have asked first. Drop the
			// spec branch and the trust question answers instead, and the
			// user is told to trust a file this build cannot read.
			fresh, freshClient := newFacadeWorld(t)
			_ = fresh

			freshPaths, pathsErr := freshClient.Paths(User, "")
			So(pathsErr, ShouldBeNil)
			So(os.MkdirAll(filepath.Dir(freshPaths.SpecPath), 0o700), ShouldBeNil)
			So(os.WriteFile(freshPaths.SpecPath, []byte("schema = 99\n"), 0o600), ShouldBeNil)

			_, planErr := freshClient.Plan(t.Context(), PlanOptions{
				Paths: freshPaths, Refs: []string{"./does-not-exist"},
			})
			So(planErr, ShouldNotBeNil)
			So(Classify(planErr), ShouldEqual, RefusalSchemaNewer)
		})
	})
}

// TestPlanRemoveHonoursHostFilter pins API-REQ 9: a removal narrows to the
// detected adapters the filter selects, exactly as an install does, so
// `--hosts` / `--except` mean the same thing on every mutating command.
func TestPlanRemoveHonoursHostFilter(t *testing.T) {
	newTwoHostWorld := func(t *testing.T) (*facadeWorld, *Client) {
		t.Helper()

		world, first := newFacadeWorld(t)

		other := &fakeHost{id: host.Codex}
		world.fake.artifacts = []fakeArtifact{{Path: filepath.Join(world.root, "host", "claude.md"), Data: "# claude\n"}}
		other.artifacts = []fakeArtifact{{Path: filepath.Join(world.root, "host", "codex.md"), Data: "# codex\n"}}

		reopened, err := Open(t.Context(), WithHosts(world.fake, other))
		So(err, ShouldBeNil)
		t.Cleanup(func() { _ = reopened.Close() })

		_ = first

		return world, reopened
	}

	Convey("Given a package installed on two hosts", t, func() {
		world, client := newTwoHostWorld(t)

		ref := world.fixture(t, "caveman", "1.2.3")

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		plan, err := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})
		So(err, ShouldBeNil)

		_, err = client.Install(t.Context(), plan, ApplyOptions{Now: fixedClock})
		So(err, ShouldBeNil)

		Convey("Then a removal without a filter touches both hosts", func() {
			removal, removeErr := client.PlanRemove(t.Context(), "local:caveman", RemoveOptions{Paths: paths})
			So(removeErr, ShouldBeNil)
			So(removal.Actions, ShouldHaveLength, 2)
		})

		Convey("Then a removal with Except touches only the other host", func() {
			removal, removeErr := client.PlanRemove(t.Context(), "local:caveman", RemoveOptions{
				Paths:  paths,
				Filter: HostFilter{Except: []string{string(host.Codex)}},
			})
			So(removeErr, ShouldBeNil)
			So(removal.Actions, ShouldHaveLength, 1)
			So(removal.Actions[0].Host, ShouldEqual, host.Claude)
		})

		Convey("Then a removal with Only touches only the named host", func() {
			removal, removeErr := client.PlanRemove(t.Context(), "local:caveman", RemoveOptions{
				Paths:  paths,
				Filter: HostFilter{Only: []string{string(host.Codex)}},
			})
			So(removeErr, ShouldBeNil)
			So(removal.Actions, ShouldHaveLength, 1)
			So(removal.Actions[0].Host, ShouldEqual, host.Codex)
		})

		Convey("Then a removal naming an unavailable host is refused as typed", func() {
			// The same HostUnavailableError an install gets, so the CLI maps it
			// to one exit code rather than a second spelling.
			_, removeErr := client.PlanRemove(t.Context(), "local:caveman", RemoveOptions{
				Paths:  paths,
				Filter: HostFilter{Only: []string{string(host.Gemini)}},
			})
			So(removeErr, ShouldNotBeNil)

			_, ok := errors.AsType[*HostUnavailableError](removeErr)
			So(ok, ShouldBeTrue)
		})
	})
}
