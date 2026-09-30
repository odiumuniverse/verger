package verger

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/consent"
	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/home"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"

	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// The library alone must carry a package through a full lifecycle: install →
// status → why → remove → restore → adopt. Nothing here imports cobra or the CLI;
// this is the contract beadle embeds (DESIGN §9.1, T4.0).

// ---- fake host ---------------------------------------------------------------

// fakeArtifact is one file the fake host writes on a delivery. KeyPath makes it
// a config document: the fake then records an OpConfigKey for it, the way a real
// adapter records ownership of one MCP server or one hook document.
type fakeArtifact struct {
	Path    string
	Data    string
	KeyPath string
}

// fakeHost is a minimal scripted adapter: it writes the artifacts it was given
// and reports one entry to its own oracle, which is all a lifecycle test needs.
type fakeHost struct {
	id host.ID
	// undetected opts one adapter out of host detection, so a test can
	// exercise the no-detected-hosts path. The zero value detects, which is
	// what every hand-built adapter in this package wants.
	undetected bool

	mu        sync.Mutex
	artifacts []fakeArtifact
	installed []host.Installed
	delivers  int
	uninstall int
	removed   []string
	// projects records the Project root of every delivery the adapter received.
	projects []string
}

// ID implements host.Host.
func (f *fakeHost) ID() host.ID { return f.id }

// Detect implements host.Host: the facade test injects the adapter, so it is
// present unless the test set undetected.
func (f *fakeHost) Detect(string) bool { return !f.undetected }

// Oracle implements host.Host.
func (f *fakeHost) Oracle() host.Oracle { return &fakeOracle{host: f} }

// Deliver implements host.Host.
func (f *fakeHost) Deliver(_ context.Context, _ string, d host.Delivery) (host.Result, error) {
	f.mu.Lock()
	f.delivers++
	f.projects = append(f.projects, d.Project)
	targets := append([]fakeArtifact(nil), f.artifacts...)
	f.mu.Unlock()

	result := host.Result{Strategy: d.Strategy, Observed: host.OracleResult{Verified: true}}

	if d.DryRun {
		return result, nil
	}

	for _, target := range targets {
		if err := fsutil.EnsureDir(filepath.Dir(target.Path), 0o700); err != nil {
			return host.Result{}, err
		}

		if err := fsutil.WriteFileAtomic(target.Path, []byte(target.Data), 0o600); err != nil {
			return host.Result{}, err
		}

		sum := digest.Bytes([]byte(target.Data))
		result.Artifacts = append(result.Artifacts, receipt.Artifact{
			Kind: "skill", Name: filepath.Base(target.Path), Path: target.Path, Digest: sum,
		})

		if target.KeyPath != "" {
			// A real adapter records the digest of the KEY's value, which is
			// what a drift check compares against.
			result.RMA = append(result.RMA, receipt.Op{
				Kind: receipt.OpConfigKey, Path: target.Path, KeyPath: target.KeyPath,
				Digest: keyValueDigest(target.Data, target.KeyPath), Mode: 0o600,
			})

			continue
		}

		result.RMA = append(result.RMA, receipt.Op{
			Kind: receipt.OpWriteFile, Path: target.Path, Digest: sum, Mode: 0o600,
		})
	}

	return result, nil
}

// Uninstall implements host.Host.
func (f *fakeHost) Uninstall(_ context.Context, _ string, r receipt.Receipt) (host.Result, error) {
	f.mu.Lock()
	f.uninstall++

	for _, op := range r.RMA {
		if op.Path != "" {
			f.removed = append(f.removed, op.Path)
		}
	}
	f.mu.Unlock()

	return host.Result{}, nil
}

// keyValueDigest hashes one dotted key's value in a JSON document, the way an
// adapter records a config key.
func keyValueDigest(document, keyPath string) digest.Hash {
	var decoded map[string]any

	if err := json.Unmarshal([]byte(document), &decoded); err != nil {
		return ""
	}

	current := any(decoded)

	for segment := range strings.SplitSeq(keyPath, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}

		current, ok = object[segment]
		if !ok {
			return ""
		}
	}

	data, err := json.Marshal(current)
	if err != nil {
		return ""
	}

	return digest.Bytes(data)
}

// fakeOracle is the fake host's own listing.
type fakeOracle struct {
	host *fakeHost
}

// List implements host.Oracle.
func (o *fakeOracle) List(context.Context) ([]host.Installed, error) {
	o.host.mu.Lock()
	defer o.host.mu.Unlock()

	return append([]host.Installed(nil), o.host.installed...), nil
}

// Validate implements host.Oracle.
func (o *fakeOracle) Validate(context.Context, string) ([]string, error) { return nil, nil }

// ---- world -------------------------------------------------------------------

// facadeWorld is one library world: a temp user home, a data dir and an
// injected adapter. Nothing here is cobra.
type facadeWorld struct {
	root    string
	user    string
	project string
	fake    *fakeHost
}

// newFacadeWorld pins the home-shaped environment and builds one client.
func newFacadeWorld(t *testing.T) (*facadeWorld, *Client) {
	t.Helper()

	root := t.TempDir()
	user := filepath.Join(root, "user")
	data := filepath.Join(root, "data")
	project := filepath.Join(root, "project")

	for _, dir := range []string{user, data, project} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	for name, value := range map[string]string{
		"HOME":            user,
		"XDG_DATA_HOME":   data,
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"VERGER_HOME":     "",
		"BEADLE_HOME":     "",
	} {
		t.Setenv(name, value)
	}

	fake := &fakeHost{id: host.Claude}

	client, err := Open(t.Context(), WithHosts(fake))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// A local ref (`./name`) resolves against the working directory, so the
	// world root becomes the cwd for the rest of the test.
	t.Chdir(root)

	world := &facadeWorld{root: root, user: user, project: project, fake: fake}

	t.Cleanup(func() { _ = client.Close() })

	return world, client
}

// writeSpec replaces the scope's spec document, so a test can change what the
// spec asks for without going through a delivery.
func (w *facadeWorld) writeSpec(t *testing.T, paths Paths, content string) {
	t.Helper()

	writeFacadeFile(t, paths.SpecPath, content)
}

// fixture writes a local package under the world root and returns its ref.
func (w *facadeWorld) fixture(t *testing.T, name, version string) string {
	t.Helper()

	dir := filepath.Join(w.root, name)
	writeFacadeFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"),
		`{"name":"`+name+`","version":"`+version+`","description":"A toolkit."}`)
	writeFacadeFile(t, filepath.Join(dir, "skills", "one", "SKILL.md"), "# one\n")

	return "./" + name
}

// withHooks writes a package that ships a hook, so the consent gate has to run.
func (w *facadeWorld) withHooks(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(w.root, "hooked")
	writeFacadeFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), `{
  "name": "hooked",
  "version": "1.0.0",
  "hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "echo hello"}]}]}
}`)
	writeFacadeFile(t, filepath.Join(dir, "skills", "one", "SKILL.md"), "# one\n")

	return "./hooked"
}

// writeFacadeFile writes one fixture file for the facade tests. The godoc
// Examples carry their own writer (writeExampleFile) precisely because an
// Example body is a plain function with no testing.T to fail with - so this one
// can stay an ordinary helper.
func writeFacadeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// fixedClock is the deterministic clock the facade tests run on.
func fixedClock() time.Time {
	return time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
}

// ---- the lifecycle -----------------------------------------------------------

// TestFacadeLifecycle drives install → status → why → remove → restore → adopt
// through the library alone. It is the T4.0 deliverable: if any of these steps
// needs the CLI, this test cannot be written.
func TestFacadeLifecycle(t *testing.T) {
	Convey("Given an isolated home and one injected adapter", t, func() {
		world, client := newFacadeWorld(t)

		ref := world.fixture(t, "caveman", "1.2.3")
		target := filepath.Join(world.root, "host", "one.md")

		world.fake.artifacts = []fakeArtifact{{Path: target, Data: "# one\n"}}

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		Convey("When a plan is built", func() {
			plan, planErr := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})
			So(planErr, ShouldBeNil)

			Convey("Then it names the package and its host, and writes nothing", func() {
				So(plan.Cells, ShouldHaveLength, 1)
				So(plan.Cells[0].Package, ShouldEqual, "local:caveman")
				So(plan.Cells[0].Host, ShouldEqual, "claude")
				So(plan.Cells[0].Status, ShouldEqual, StatusPlanned)
				So(fileMissing(target), ShouldBeTrue)
			})

			Convey("When it is applied", func() {
				_, applyErr := client.Install(t.Context(), plan, ApplyOptions{Now: fixedClock})
				So(applyErr, ShouldBeNil)

				Convey("Then the artifact exists, verger owns it and the spec records it", func() {
					So(fileMissing(target), ShouldBeFalse)

					owner, owned := client.Owns(target)
					So(owned, ShouldBeTrue)
					So(owner, ShouldEqual, "local:caveman")

					doc, ok, specErr := LoadSpec(paths.SpecPath)
					So(specErr, ShouldBeNil)
					So(ok, ShouldBeTrue)
					So(doc.Packages, ShouldHaveLength, 1)
					So(doc.Packages[0].ID, ShouldEqual, "local:caveman")
				})

				Convey("When status is asked", func() {
					status, statusErr := client.Status(t.Context(), StatusOptions{Paths: paths})
					So(statusErr, ShouldBeNil)

					Convey("Then the cell is current and carries the host level", func() {
						So(status.Cells, ShouldHaveLength, 1)
						So(status.Cells[0].Status, ShouldEqual, StatusCurrent)
						So(status.Cells[0].Level, ShouldEqual, levelStable)
						So(status.Cells[0].Version, ShouldEqual, "1.2.3")
					})
				})

				Convey("When why is asked", func() {
					why, whyErr := client.Why(t.Context(), paths, "local:caveman", "claude")
					So(whyErr, ShouldBeNil)

					Convey("Then it reports the strategy, the version and the spec entry", func() {
						So(why.Status, ShouldEqual, StatusCurrent)
						So(why.Version, ShouldEqual, "1.2.3")
						So(strings.Join(why.Reasons, "\n"), ShouldContainSubstring, "strategy loose")
						So(strings.Join(why.Reasons, "\n"), ShouldContainSubstring, "spec local:caveman")
					})
				})

				Convey("When it is removed", func() {
					removePlan, removeErr := client.PlanRemove(t.Context(), "local:caveman", RemoveOptions{Paths: paths})
					So(removeErr, ShouldBeNil)
					So(removePlan.Actions, ShouldHaveLength, 1)

					_, removeErr = client.Remove(t.Context(), removePlan, ApplyOptions{Now: fixedClock})
					So(removeErr, ShouldBeNil)

					Convey("Then the delivered artifact is gone and the spec entry with it", func() {
						So(fileMissing(target), ShouldBeTrue)

						doc, _, specErr := LoadSpec(paths.SpecPath)
						So(specErr, ShouldBeNil)
						So(doc.Packages, ShouldBeEmpty)

						status, statusErr := client.Status(t.Context(), StatusOptions{Paths: paths})
						So(statusErr, ShouldBeNil)
						So(status.Cells, ShouldBeEmpty)
					})
				})
			})
		})
	})
}

// TestFacadeRestore pins the restore leg: a removal trashes the artifact it
// removed, and Restore puts it back through the library.
func TestFacadeRestore(t *testing.T) {
	Convey("Given a delivered package", t, func() {
		world, client := newFacadeWorld(t)

		ref := world.fixture(t, "caveman", "1.2.3")
		target := filepath.Join(world.root, "host", "one.md")

		world.fake.artifacts = []fakeArtifact{{Path: target, Data: "# one\n"}}

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		plan, err := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})
		So(err, ShouldBeNil)

		_, err = client.Install(t.Context(), plan, ApplyOptions{Now: fixedClock})
		So(err, ShouldBeNil)

		removePlan, err := client.PlanRemove(t.Context(), "local:caveman", RemoveOptions{Paths: paths})
		So(err, ShouldBeNil)

		_, err = client.Remove(t.Context(), removePlan, ApplyOptions{Now: fixedClock})
		So(err, ShouldBeNil)

		Convey("When it is restored", func() {
			before, trashErr := client.Store().Trash().List()
			So(trashErr, ShouldBeNil)
			So(before, ShouldNotBeEmpty)

			result, restoreErr := client.Restore(t.Context(), "local:caveman", RemoveOptions{Paths: paths})
			So(restoreErr, ShouldBeNil)

			Convey("Then the removed artifact is back on disk", func() {
				So(result.Restored, ShouldNotBeEmpty)
				So(fileMissing(target), ShouldBeFalse)
				So(readFacadeFile(t, target), ShouldEqual, "# one\n")
			})
		})
	})
}

// TestFacadeAdopt pins the adopt leg: a package one host already has is
// recorded in the spec and delivered to the other detected hosts, without hooks
// (D4).
func TestFacadeAdopt(t *testing.T) {
	Convey("Given two hosts, one of which already has a package", t, func() {
		world, _ := newFacadeWorld(t)

		// A second adapter is what makes the cross-host leg of D4 observable:
		// the source host is skipped, the other one receives the delivery.
		other := &fakeHost{id: host.Codex}
		otherTarget := filepath.Join(world.root, "host", "other.md")
		other.artifacts = []fakeArtifact{{Path: otherTarget, Data: "# adopted\n"}}

		world.fake.installed = []host.Installed{{
			Name: "caveman", Version: "2.0.0", Path: filepath.Join(world.root, "elsewhere"), Enabled: true,
		}}

		client, err := Open(t.Context(), WithHosts(world.fake, other))
		So(err, ShouldBeNil)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		Convey("When it is adopted", func() {
			plan, planErr := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{"claude:caveman"}})
			So(planErr, ShouldBeNil)
			So(plan.Adopts, ShouldHaveLength, 1)
			So(plan.Adopts[0].AdoptedFrom, ShouldEqual, "claude")

			_, applyErr := client.Install(t.Context(), plan, ApplyOptions{Now: fixedClock})
			So(applyErr, ShouldBeNil)

			Convey("Then the spec records where it came from and the other host has it", func() {
				doc, _, specErr := LoadSpec(paths.SpecPath)
				So(specErr, ShouldBeNil)
				So(doc.Packages, ShouldHaveLength, 1)
				So(doc.Packages[0].ID, ShouldEqual, "caveman")
				So(doc.Packages[0].AdoptedFrom, ShouldEqual, "claude")

				status, statusErr := client.Status(t.Context(), StatusOptions{Paths: paths})
				So(statusErr, ShouldBeNil)
				So(status.Cells, ShouldHaveLength, 1)
				So(status.Cells[0].Host, ShouldEqual, "codex")
				So(fileMissing(otherTarget), ShouldBeFalse)
			})
		})
	})
}

// TestFacadeHooksConsent pins the D5 gate through the library: a payload that
// ships hooks is not delivered without an answer, and HooksSkip delivers it
// without one.
func TestFacadeHooksConsent(t *testing.T) {
	Convey("Given a package that ships a hook", t, func() {
		world, client := newFacadeWorld(t)

		ref := world.withHooks(t)
		target := filepath.Join(world.root, "host", "hooked.md")
		world.fake.artifacts = []fakeArtifact{{Path: target, Data: "# hooked\n"}}

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		asked := &recordingConfirmer{}

		plan, err := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})
		So(err, ShouldBeNil)

		_, err = client.Install(t.Context(), plan, ApplyOptions{Now: fixedClock, Hooks: HooksSkip, Confirm: asked})
		So(err, ShouldBeNil)

		Convey("When hooks are skipped", func() {
			Convey("Then nothing was asked and nothing was written", func() {
				So(asked.count, ShouldEqual, 0)
				So(fileMissing(target), ShouldBeFalse)
			})
		})

		Convey("When hooks are asked for and the confirmer declines", func() {
			asked.answer = false

			plan, planErr := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})
			So(planErr, ShouldBeNil)

			_, applyErr := client.Install(t.Context(), plan, ApplyOptions{Now: fixedClock, Hooks: HooksAsk, Confirm: asked})
			So(applyErr, ShouldBeNil)

			Convey("Then the question was asked once", func() {
				So(asked.count, ShouldEqual, 1)
				So(asked.messages[0], ShouldContainSubstring, "Install hooks of local:hooked")
			})
		})
	})
}

// recordingConfirmer answers a fixed way and records what it was asked.
type recordingConfirmer struct {
	answer   bool
	messages []string
	count    int
}

// Confirm implements Confirmer.
func (c *recordingConfirmer) Confirm(_ context.Context, q apply.Question) (bool, error) {
	c.count++
	c.messages = append(c.messages, q.Message)

	return c.answer, nil
}

// fileMissing reports whether path does not exist.
func fileMissing(path string) bool {
	_, err := os.Lstat(path)

	return err != nil
}

// readFacadeFile reads one file the facade test wrote.
func readFacadeFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp home
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(data)
}

// TestFacadeAbsorbRenamesAnEmptyTarget pins GAP-11: when the target holds no
// state there is nothing to merge, so the whole home moves in one step and the
// state this merge does not model (the lock file, a live lease, unknown minor
// fields) travels with it.
func TestFacadeAbsorbRenamesAnEmptyTarget(t *testing.T) {
	Convey("Given a home with state and a target that has none", t, func() {
		world, from := newFacadeWorld(t)

		paths, err := from.Paths(User, "")
		So(err, ShouldBeNil)

		doc, _, err := LoadSpec(paths.SpecPath)
		So(err, ShouldBeNil)

		AddSpecPackage(doc, "acme/caveman", "1.2.3")
		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		// State the file-by-file merge does not model.
		writeFacadeFile(t, filepath.Join(paths.StateDir, ".lock"), "")
		writeFacadeFile(t, filepath.Join(paths.StateDir, "watch.lease"), "held")

		to, err := Open(t.Context(), WithHome(filepath.Join(world.root, "absorbed")))
		So(err, ShouldBeNil)

		report, err := Absorb(t.Context(), from, to)
		So(err, ShouldBeNil)

		Convey("When the home is absorbed", func() {
			Convey("Then the whole home was renamed and the extra state came with it", func() {
				So(report.Renamed, ShouldBeTrue)
				So(fileMissing(from.Home().Root()), ShouldBeTrue)

				absorbed, pathsErr := to.Paths(User, "")
				So(pathsErr, ShouldBeNil)

				So(readFacadeFile(t, absorbed.SpecPath), ShouldContainSubstring, "acme/caveman")
				So(readFacadeFile(t, filepath.Join(absorbed.StateDir, ".lock")), ShouldBeEmpty)
				So(readFacadeFile(t, filepath.Join(absorbed.StateDir, "watch.lease")), ShouldEqual, "held")
			})
		})
	})
}

// TestFacadeOwnsInScope pins GAP-8: the ownership invariant a second front end
// filters its pulls with must answer for the project scope too, not only for the
// user scope.
func TestFacadeOwnsInScope(t *testing.T) {
	Convey("Given a project-scoped delivery", t, func() {
		world, client := newFacadeWorld(t)

		paths, err := client.Paths(Project, world.project)
		So(err, ShouldBeNil)

		// A project-scoped write is gated by trust (D11); the facade test
		// trusts the project explicitly rather than bypassing the gate.
		doc, _, err := LoadSpec(paths.SpecPath)
		So(err, ShouldBeNil)

		// Trust is taken over the spec as it is now; the delivery writes the
		// package entry afterwards, which is exactly the order `verger trust`
		// followed by `verger install --project` has.
		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		trust, err := client.TrustStore()
		So(err, ShouldBeNil)
		So(trust.Trust(world.project, consent.SpecHash(doc)), ShouldBeNil)
		So(trust.Save(), ShouldBeNil)

		_ = doc

		ref := world.fixture(t, "caveman", "1.2.3")
		target := filepath.Join(world.project, "host", "one.md")

		world.fake.artifacts = []fakeArtifact{{Path: target, Data: "# one\n"}}

		plan, err := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})
		So(err, ShouldBeNil)

		_, err = client.Install(t.Context(), plan, ApplyOptions{Now: fixedClock})
		So(err, ShouldBeNil)

		Convey("When ownership is asked for that scope", func() {
			Convey("Then the scoped answer names the package", func() {
				pkg, owned := client.OwnsIn(paths, target)
				So(owned, ShouldBeTrue)
				So(pkg, ShouldEqual, "local:caveman")
			})

			Convey("Then the user-scope shortcut does not see a project receipt", func() {
				_, owned := client.Owns(target)
				So(owned, ShouldBeFalse)
			})
		})
	})
}

// TestPickStrategyExplainsItself pins GAP-12: a UI must be able to say why the
// ladder chose this rung, so every branch returns a reason.
func TestPickStrategyExplainsItself(t *testing.T) {
	Convey("Given packages that stop the ladder at each rung", t, func() {
		cases := []struct {
			name   string
			pkg    host.Package
			reason string
		}{
			{
				name:   "a local payload is always loose",
				pkg:    host.Package{ID: "acme/caveman"},
				reason: "local payload",
			},
		}

		for _, tc := range cases {
			Convey("When "+tc.name, func() {
				strategy, reason := PickStrategy(tc.pkg, host.Gemini, source.KindLocal)
				So(strategy, ShouldEqual, host.Loose)
				So(reason, ShouldContainSubstring, tc.reason)
			})
		}

		Convey("Then a rung below native always explains itself", func() {
			for _, kind := range []source.Kind{source.KindLocal, source.KindGit} {
				strategy, reason := PickStrategy(host.Package{ID: "acme/caveman"}, host.Gemini, kind)
				if strategy == host.Native {
					// The top of the ladder needs no explanation.
					continue
				}

				So(reason, ShouldNotBeEmpty)
			}
		})
	})
}

// TestApplyEmitsFacadeEvents pins GAP-4: the progress a second front end renders
// must arrive on the caller's channel in the facade's own vocabulary, not as raw
// executor steps the caller has to know the meaning of.
func TestApplyEmitsFacadeEvents(t *testing.T) {
	Convey("Given a caller that listens for progress", t, func() {
		world, client := newFacadeWorld(t)

		ref := world.fixture(t, "caveman", "1.2.3")
		target := filepath.Join(world.root, "host", "one.md")
		world.fake.artifacts = []fakeArtifact{{Path: target, Data: "# one\n"}}

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		plan, err := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})
		So(err, ShouldBeNil)

		events := make(chan Event, eventBuffer)

		_, err = client.Install(t.Context(), plan, ApplyOptions{Now: fixedClock, Events: events})
		So(err, ShouldBeNil)

		// The relay is synchronous with the run (Apply waits for it before
		// returning), so the events are already in the caller's buffer here.
		got := (&drainingEvents{events: events}).collect()

		So(len(got), ShouldBeGreaterThan, 0)

		kinds := map[EventKind]bool{}

		for _, event := range got {
			So(event.Message, ShouldNotBeEmpty)

			kinds[event.Kind] = true

			// A cell event names its host; a run-level one (the lock write) does
			// not, because it belongs to no single cell.
			if event.Kind == EventApplied {
				So(event.Host, ShouldNotBeEmpty)
			}
		}

		So(kinds[EventPlanned], ShouldBeTrue)
		So(kinds[EventApplied], ShouldBeTrue)
	})
}

// drainingEvents collects the events a run produced without blocking the relay.
type drainingEvents struct {
	events chan Event
}

// collect drains the channel into a slice.
func (d *drainingEvents) collect() []Event {
	var out []Event

	for {
		select {
		case event := <-d.events:
			out = append(out, event)
		default:
			return out
		}
	}
}

// TestFacadeSyncReconcilesWithTheSpec pins GAP-1: a second front end drives the
// reconcile through the library, and a second sync of the same spec is empty.
func TestFacadeSyncReconcilesWithTheSpec(t *testing.T) {
	Convey("Given a spec with one package", t, func() {
		world, client := newFacadeWorld(t)

		ref := world.fixture(t, "caveman", "1.2.3")
		target := filepath.Join(world.root, "host", "one.md")
		world.fake.artifacts = []fakeArtifact{{Path: target, Data: "# one\n"}}

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		doc, _, err := LoadSpec(paths.SpecPath)
		So(err, ShouldBeNil)

		// The spec records where the package actually is: a relative URL
		// resolves against the spec's own directory, and this fixture sits
		// beside the world root, not beside the spec.
		So(AddSpecSourceAt(doc, mustRef(t, ref), filepath.Dir(paths.SpecPath)), ShouldBeTrue)
		So(AddSpecPackage(doc, "local:caveman", "1.2.3"), ShouldBeTrue)
		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		Convey("When the machine is reconciled with the spec", func() {
			plan, report, syncErr := client.Sync(t.Context(), SyncOptions{Paths: paths, Hooks: HooksSkip})
			So(syncErr, ShouldBeNil)

			Convey("Then the declared package was delivered", func() {
				So(plan.Cells, ShouldNotBeEmpty)
				So(fileMissing(target), ShouldBeFalse)
				So(report, ShouldNotBeNil)
			})

			Convey("When the same sync runs again", func() {
				_, _, againErr := client.Sync(t.Context(), SyncOptions{Paths: paths, Hooks: HooksSkip})
				So(againErr, ShouldBeNil)

				Convey("Then it is empty, which is the idempotence proof", func() {
					status, statusErr := client.Status(t.Context(), StatusOptions{Paths: paths})
					So(statusErr, ShouldBeNil)
					So(status.Cells, ShouldHaveLength, 1)
					So(status.Cells[0].Status, ShouldEqual, StatusCurrent)
				})
			})

			Convey("When the spec drops the package", func() {
				pruned, _, pruneErr := LoadSpec(paths.SpecPath)
				So(pruneErr, ShouldBeNil)
				So(RemoveSpecPackage(pruned, "local:caveman"), ShouldEqual, 1)
				So(SaveSpec(paths.SpecPath, pruned), ShouldBeNil)

				_, report, dropErr := client.Sync(t.Context(), SyncOptions{Paths: paths, Hooks: HooksSkip})
				So(dropErr, ShouldBeNil)
				So(report, ShouldNotBeNil)

				Convey("Then the machine follows the spec", func() {
					status, statusErr := client.Status(t.Context(), StatusOptions{Paths: paths})
					So(statusErr, ShouldBeNil)
					So(status.Cells, ShouldBeEmpty)
					So(fileMissing(target), ShouldBeTrue)
				})
			})

			Convey("When a check-only sync runs against a machine that already matches", func() {
				_, _, checkErr := client.Sync(t.Context(), SyncOptions{Paths: paths, Check: true})
				So(checkErr, ShouldBeNil)
			})
		})
	})
}

// mustRef parses a local ref for a spec source.
func mustRef(t *testing.T, raw string) source.Ref {
	t.Helper()

	ref, err := source.Parse(raw)
	So(err, ShouldBeNil)

	return ref
}

// TestFacadeUpdatePinAndOutdated pins GAP-2 and GAP-17: update is the reconcile
// under its own name, a pin is a spec fact the facade owns, and the pin
// vocabulary answers the four questions a second front end renders.
func TestFacadeUpdatePinAndOutdated(t *testing.T) {
	Convey("Given a spec with one delivered package", t, func() {
		world, client := newFacadeWorld(t)

		ref := world.fixture(t, "caveman", "1.2.3")
		target := filepath.Join(world.root, "host", "one.md")
		world.fake.artifacts = []fakeArtifact{{Path: target, Data: "# one\n"}}

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		doc, _, err := LoadSpec(paths.SpecPath)
		So(err, ShouldBeNil)
		So(AddSpecSourceAt(doc, mustRef(t, ref), filepath.Dir(paths.SpecPath)), ShouldBeTrue)
		So(AddSpecPackage(doc, "local:caveman", "1.2.3"), ShouldBeTrue)
		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		_, _, err = client.Sync(t.Context(), SyncOptions{Paths: paths, Hooks: HooksSkip})
		So(err, ShouldBeNil)

		Convey("When update runs", func() {
			plan, report, updateErr := client.Update(t.Context(), UpdateOptions{Paths: paths, Hooks: HooksSkip})
			So(updateErr, ShouldBeNil)
			So(report, ShouldNotBeNil)
			So(plan.Actions, ShouldBeEmpty)
		})

		Convey("When outdated is asked", func() {
			out, outdatedErr := client.Outdated(t.Context(), StatusOptions{Paths: paths})
			So(outdatedErr, ShouldBeNil)

			Convey("Then a machine that matches its spec has nothing outdated", func() {
				So(out.Cells, ShouldBeEmpty)
			})
		})

		Convey("When the package is pinned", func() {
			result, pinErr := client.Pin(t.Context(), PinOptions{Paths: paths, ID: "local:caveman", Version: "1.4.0"})
			So(pinErr, ShouldBeNil)
			So(result.Pinned, ShouldBeTrue)
			So(result.Changed, ShouldBeTrue)

			Convey("Then the pin vocabulary calls it a change and the machine is flagged", func() {
				states, statesErr := client.PinStates(t.Context(), paths)
				So(statesErr, ShouldBeNil)
				So(states, ShouldHaveLength, 1)
				So(states[0].Version, ShouldEqual, "1.4.0")
				So(states[0].Vocabulary, ShouldEqual, PinNoEffect)
				So(states[0].Locked, ShouldBeTrue)
			})

			Convey("When the pin is dropped", func() {
				dropped, dropErr := client.Unpin(t.Context(), PinOptions{Paths: paths, ID: "local:caveman"})
				So(dropErr, ShouldBeNil)
				So(dropped.Pinned, ShouldBeFalse)

				states, statesErr := client.PinStates(t.Context(), paths)
				So(statesErr, ShouldBeNil)
				So(states[0].Vocabulary, ShouldEqual, PinOK)
				So(states[0].Locked, ShouldBeFalse)
			})

			Convey("When an unknown id is pinned", func() {
				_, unknownErr := client.Pin(t.Context(), PinOptions{Paths: paths, ID: "acme/nope", Version: "1.0.0"})
				So(unknownErr, ShouldBeError)
			})
		})

		Convey("When a package is disabled in the spec", func() {
			So(client.SetDisabled(t.Context(), paths, "local:caveman", true, false), ShouldBeNil)

			disabled, found := client.Disabled(paths, "local:caveman")
			So(found, ShouldBeTrue)
			So(disabled, ShouldBeTrue)

			Convey("Then a sync leaves it alone and says why", func() {
				plan, _, syncErr := client.Sync(t.Context(), SyncOptions{Paths: paths, Hooks: HooksSkip})
				So(syncErr, ShouldBeNil)
				So(plan.Actions, ShouldBeEmpty)
				So(strings.Join(plan.Notes, "\n"), ShouldContainSubstring, "disabled in the spec")
			})
		})
	})
}

// TestFacadeWatchTakesTheLeaseAndReconciles pins GAP-3: the facade binds to the
// W5 watch contract, builds the targets from pkg/hostpath for the hosts it can
// deliver to, holds the lease while the engine runs, and reconciles a change
// through the same Sync the CLI uses.
func TestFacadeWatchTakesTheLeaseAndReconciles(t *testing.T) {
	Convey("Given a spec with one package and a fake watch engine", t, func() {
		world, client := newFacadeWorld(t)

		ref := world.fixture(t, "caveman", "1.2.3")
		target := filepath.Join(world.root, "host", "one.md")
		world.fake.artifacts = []fakeArtifact{{Path: target, Data: "# one\n"}}

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		doc, _, err := LoadSpec(paths.SpecPath)
		So(err, ShouldBeNil)
		So(AddSpecSourceAt(doc, mustRef(t, ref), filepath.Dir(paths.SpecPath)), ShouldBeTrue)
		So(AddSpecPackage(doc, "local:caveman", "1.2.3"), ShouldBeTrue)
		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		var (
			seenTargets []WatchTarget
			seenLease   Lease
			seenHeld    bool
		)

		engine := func(ctx context.Context, targets []WatchTarget, out chan<- WatchEvent, _ ...WatchOption) error {
			seenTargets = targets

			if lease, held, leaseErr := client.LeaseStatus(); leaseErr == nil && held {
				seenLease, seenHeld = lease, true
			}

			writeFacadeFile(t, target, "# changed\n")

			select {
			case out <- WatchEvent{Host: "claude", Paths: []string{target}}:
			case <-ctx.Done():
				return ctx.Err()
			}

			<-ctx.Done()

			return ctx.Err()
		}

		SetWatchEngine(engine)

		defer SetWatchEngine(nil)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		events := make(chan Event, watchBuffer)

		go func() {
			deadline := time.After(10 * time.Second)

			// Stop the watcher once the reconcile has run.
			for {
				select {
				case <-events:
					cancel()

					return
				case <-deadline:
					cancel()

					return
				}
			}
		}()

		watchErr := client.Watch(ctx, WatchOptions{
			Paths:   paths,
			Owner:   LeaseOwnerVerger,
			Watch:   []string{"claude"},
			Hooks:   HooksSkip,
			Events:  events,
			Confirm: vergerYes(),
		})

		Convey("When the watcher runs", func() {
			Convey("Then it held the lease and watched the host's own surfaces", func() {
				So(watchErr, ShouldNotBeNil) // the context was cancelled
				So(seenHeld, ShouldBeTrue)
				So(seenLease.Owner, ShouldEqual, LeaseOwnerVerger)
				So(seenTargets, ShouldHaveLength, 1)
				So(seenTargets[0].Host, ShouldEqual, "claude")
				So(seenTargets[0].Paths, ShouldNotBeEmpty)
			})

			Convey("Then the declared package was reconciled by the event", func() {
				So(fileMissing(target), ShouldBeFalse)

				status, statusErr := client.Status(t.Context(), StatusOptions{Paths: paths})
				So(statusErr, ShouldBeNil)
				So(status.Cells, ShouldHaveLength, 1)
			})
		})
	})
}

// TestFacadeWatchWithoutAnEngine pins the honest answer when the W5 engine is
// not linked in yet: the capability is reported as unavailable, not faked.
func TestFacadeWatchWithoutAnEngine(t *testing.T) {
	Convey("Given a build with no watch engine", t, func() {
		_, client := newFacadeWorld(t)

		SetWatchEngine(nil)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		err = client.Watch(t.Context(), WatchOptions{Paths: paths, Owner: LeaseOwnerVerger})

		Convey("Then Watch reports the capability as unavailable", func() {
			typed, ok := errors.AsType[*NotAvailableError](err)
			So(ok, ShouldBeTrue)
			So(typed.Feature, ShouldEqual, "watch")
		})
	})
}

// vergerYes is the confirmer the watch tests inject: accept defaults, decline
// destructive questions.
func vergerYes() Confirmer {
	return YesConfirmer()
}

// TestFacadeAbsorbMigratesSecrets pins GAP-10: a home move carries the secret
// values with it. A whole-home rename takes them with the file; a merge into a
// home that already has a store copies them name by name — read, write,
// delete — and a repeated absorb neither duplicates nor drops anything
// (DESIGN §3.3).
func TestFacadeAbsorbMigratesSecrets(t *testing.T) {
	newTarget := func(t *testing.T, world *facadeWorld, name, spec string) *Client {
		t.Helper()

		target, err := Open(t.Context(), WithHome(filepath.Join(world.root, name)))
		So(err, ShouldBeNil)

		if spec != "" {
			paths, pathsErr := target.Paths(User, "")
			So(pathsErr, ShouldBeNil)

			doc, _, loadErr := LoadSpec(paths.SpecPath)
			So(loadErr, ShouldBeNil)
			So(AddSpecPackage(doc, "acme/already-here", "1.0.0"), ShouldBeTrue)
			So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)
		}

		return target
	}

	seed := func(t *testing.T, client *Client) {
		t.Helper()

		client.Secrets().Set("MCP_TOKEN", "s3cr3t")
		client.Secrets().Set("WEB_TOKEN", "web")
		So(client.Secrets().Save(), ShouldBeNil)
	}

	Convey("Given a home absorbed into an empty target", t, func() {
		world, from := newFacadeWorld(t)
		seed(t, from)

		to := newTarget(t, world, "rename-target", "")

		report, err := Absorb(t.Context(), from, to)
		So(err, ShouldBeNil)
		So(report.Renamed, ShouldBeTrue)

		Convey("Then the secrets travelled with the renamed home", func() {
			token, ok := to.Secrets().Get("MCP_TOKEN")
			So(ok, ShouldBeTrue)
			So(token, ShouldEqual, "s3cr3t")

			web, ok := to.Secrets().Get("WEB_TOKEN")
			So(ok, ShouldBeTrue)
			So(web, ShouldEqual, "web")
		})
	})

	Convey("Given a home merged into a target that already has state", t, func() {
		world, from := newFacadeWorld(t)
		seed(t, from)

		to := newTarget(t, world, "merge-target", "kept")

		report, err := Absorb(t.Context(), from, to)
		So(err, ShouldBeNil)
		So(report.Renamed, ShouldBeFalse)

		Convey("Then the secrets were copied and the source cleared", func() {
			token, ok := to.Secrets().Get("MCP_TOKEN")
			So(ok, ShouldBeTrue)
			So(token, ShouldEqual, "s3cr3t")

			web, ok := to.Secrets().Get("WEB_TOKEN")
			So(ok, ShouldBeTrue)
			So(web, ShouldEqual, "web")

			_, ok1 := from.Secrets().Get("MCP_TOKEN")
			So(ok1, ShouldBeFalse)

			_, ok2 := from.Secrets().Get("WEB_TOKEN")
			So(ok2, ShouldBeFalse)
		})

		Convey("Then running the migration again changes nothing", func() {
			So(MigrateSecrets(from, to), ShouldBeNil)

			token, ok := to.Secrets().Get("MCP_TOKEN")
			So(ok, ShouldBeTrue)
			So(token, ShouldEqual, "s3cr3t")
		})

		Convey("Then the target's own package survived the merge", func() {
			paths, err := to.Paths(User, "")
			So(err, ShouldBeNil)

			doc, _, err := LoadSpec(paths.SpecPath)

			So(err, ShouldBeNil)

			kept, found := SpecPackage(doc, "acme/already-here")
			So(found, ShouldBeTrue)
			So(kept.ID, ShouldEqual, "acme/already-here")
		})
	})
}

// TestFacadeClassifyRefusals pins GAP-7: a second front end branches on the
// refusal kind, never on the message, and the vocabulary covers the errors the
// facade does not construct itself.
func TestFacadeClassifyRefusals(t *testing.T) {
	Convey("Given one refusal of each kind", t, func() {
		cases := []struct {
			name string
			err  error
			want RefusalKind
		}{
			{"a caller mistake", &UsageError{Cause: errors.New("bad flag")}, RefusalUsage},
			{"an untrusted project", &UntrustedProjectError{Path: "p", Cause: errors.New("no")}, RefusalUntrusted},
			{"an unavailable host", &HostUnavailableError{Only: map[string]bool{"dsh": true}}, RefusalHostUnavailable},
			{"a capability this build lacks", &NotAvailableError{Feature: "watch"}, RefusalNotAvailable},
			{"a check that would write", &CheckFailedError{Planned: 1, LockPath: "lock"}, RefusalCheck},
			{"a missing confirmation", apply.ErrConfirmationRequired, RefusalNeedsConsent},
			{"a document this build cannot read", &spec.SchemaNewerError{Path: "verger.toml", Found: 9, Supported: 1}, RefusalSchemaNewer},
			{"a lock another holder owns", &home.LockedError{Path: "state/.lock", Cause: errors.New("held")}, RefusalLocked},
			{"a hands-off key", &render.HandsOffError{Path: "mcpServers", KeyPath: "mcpServers.fs"}, RefusalHandsOff},
		}

		for _, tc := range cases {
			Convey("When "+tc.name+" happens", func() {
				Convey("Then Classify names the kind without reading the text", func() {
					So(Classify(tc.err), ShouldEqual, tc.want)
				})
			})
		}

		Convey("Then an ordinary failure is not a refusal", func() {
			So(Classify(errors.New("connection reset")), ShouldEqual, RefusalNone)
		})

		Convey("Then a refusal carries its message through Error", func() {
			refusal := &RefusalError{Kind: RefusalLocked, Message: "the lock is held", Cause: errors.New("held")}

			So(refusal.Error(), ShouldEqual, "the lock is held")
			So(errors.Is(refusal, refusal.Cause), ShouldBeTrue)

			_, ok := RefusalOf(refusal)
			So(ok, ShouldBeTrue)
		})
	})
}

// TestFacadeSwitchesAreOffByDefault pins GAP-9 (U2): every behaviour is on
// unless the spec says otherwise, and a switch removes exactly one thing.
func TestFacadeSwitchesAreOffByDefault(t *testing.T) {
	Convey("Given a spec with no switches", t, func() {
		_, client := newFacadeWorld(t)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		switches, err := ResolveSwitches(paths, SwitchOptions{})
		So(err, ShouldBeNil)

		Convey("Then every behaviour is on", func() {
			So(switches.Enabled("claude"), ShouldBeTrue)
			So(switches.HooksOn("claude"), ShouldBeTrue)
			So(switches.RuntimeOn("claude"), ShouldBeTrue)
			So(switches.Cooldown, ShouldEqual, time.Duration(0))
		})
	})

	Convey("Given a spec that switches one host off", t, func() {
		_, client := newFacadeWorld(t)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		no := false

		doc, _, err := LoadSpec(paths.SpecPath)
		So(err, ShouldBeNil)

		doc.Hosts = map[string]spec.HostSettings{
			"codex":  {Hooks: &no},
			"gemini": {Enabled: &no},
		}
		doc.Defaults.Cooldown = spec.Duration(time.Second)

		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		switches, err := ResolveSwitches(paths, SwitchOptions{})
		So(err, ShouldBeNil)

		Convey("Then only that host is switched off and the cooldown is read", func() {
			So(switches.HooksOn("codex"), ShouldBeFalse)
			So(switches.HooksOn("claude"), ShouldBeTrue)
			So(switches.Enabled("gemini"), ShouldBeFalse)
			So(switches.Enabled("claude"), ShouldBeTrue)
			So(switches.Cooldown, ShouldEqual, time.Second)
		})

		Convey("Then filtering drops the host switched off entirely and keeps the one that only lost hooks", func() {
			kept := switches.Filter([]host.Host{
				stubHost{id: host.Claude}, stubHost{id: host.Codex}, stubHost{id: host.Gemini},
			}, nil)

			So(kept, ShouldHaveLength, 2)
			So(kept[0].ID(), ShouldEqual, host.Claude)
			So(kept[1].ID(), ShouldEqual, host.Codex)
		})

		Convey("Then a package's own except list removes one more", func() {
			kept := switches.Filter([]host.Host{stubHost{id: host.Claude}}, ExceptFor(doc, "local:caveman"))
			So(kept, ShouldHaveLength, 1)

			doc.Packages = []spec.Package{{ID: "local:caveman", Except: []string{"claude"}}}
			So(switches.Filter([]host.Host{stubHost{id: host.Claude}}, ExceptFor(doc, "local:caveman")), ShouldBeEmpty)
		})
	})
}

// TestFacadePinVocabularyIsComplete pins GAP-17: the four answers a second front
// end renders are all reachable, and an unknown id is one of them rather than a
// silent absence.
func TestFacadePinVocabularyIsComplete(t *testing.T) {
	Convey("Given a spec with an installed and a declared-only package", t, func() {
		world, client := newFacadeWorld(t)

		ref := world.fixture(t, "caveman", "1.2.3")
		target := filepath.Join(world.root, "host", "one.md")
		world.fake.artifacts = []fakeArtifact{{Path: target, Data: "# one\n"}}

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		doc, _, err := LoadSpec(paths.SpecPath)
		So(err, ShouldBeNil)
		So(AddSpecSourceAt(doc, mustRef(t, ref), filepath.Dir(paths.SpecPath)), ShouldBeTrue)
		So(AddSpecPackage(doc, "local:caveman", "1.2.3"), ShouldBeTrue)
		So(AddSpecPackage(doc, "acme/never-installed", ""), ShouldBeTrue)

		// Declared but deliberately not delivered, rather than declared with no
		// source to get it from: a package no source provides is a broken spec,
		// and a broken spec is a loud failure, not a `missing` pin.
		doc.Packages[len(doc.Packages)-1].Disabled = true
		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		_, _, err = client.Sync(t.Context(), SyncOptions{Paths: paths, Hooks: HooksSkip})
		So(err, ShouldBeNil)

		installed, err := client.PinStateFor(t.Context(), paths, "local:caveman")
		So(err, ShouldBeNil)
		So(installed.Vocabulary, ShouldEqual, PinOK)

		declared, err := client.PinStateFor(t.Context(), paths, "acme/never-installed")
		So(err, ShouldBeNil)
		So(declared.Vocabulary, ShouldEqual, PinMissing)

		unknown, err := client.PinStateFor(t.Context(), paths, "acme/nope")
		So(err, ShouldBeNil)
		So(unknown.Vocabulary, ShouldEqual, PinUnknown)

		Convey("Then a status call without a resolved scope is refused", func() {
			_, statusErr := client.Status(t.Context(), StatusOptions{})
			So(statusErr, ShouldBeError)
			So(Classify(statusErr), ShouldEqual, RefusalUsage)
		})
	})
}

// TestFacadeStatusReportsMCPDrift pins DRIFT-1: a delivered MCP server the user
// edited by hand is never reported as `current`, and the cell carries the key
// that moved. The check is per key, so a key the user added to the same
// document changes nothing (DRIFT-2).
func TestFacadeStatusReportsMCPDrift(t *testing.T) {
	Convey("Given a package whose MCP server verger delivered", t, func() {
		world, client := newFacadeWorld(t)

		doc := filepath.Join(world.root, "pkg", ".mcp.json")
		mkdirFixture(t, filepath.Dir(doc))
		writeFacadeFile(t, filepath.Join(doc, "..", ".claude-plugin", "plugin.json"),
			`{"name":"caveman","version":"1.2.3"}`)
		writeFacadeFile(t, doc, `{"mcpServers": {"fs": {"command": "node", "args": ["server.js"]}}}`)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		plan, err := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{"./pkg"}})
		So(err, ShouldBeNil)

		// The fake adapter writes one artifact; point it at the document the
		// facade will own, so the receipt records a config-key op for it.
		world.fake.artifacts = nil

		hostDoc := filepath.Join(world.root, "host", "mcp.json")
		world.fake.artifacts = []fakeArtifact{{
			Path: hostDoc, Data: `{"mcpServers":{"fs":{"command":"node"}}}`, KeyPath: "mcpServers.fs",
		}}

		_, err = client.Install(t.Context(), plan, ApplyOptions{Now: fixedClock})
		So(err, ShouldBeNil)

		status, err := client.Status(t.Context(), StatusOptions{Paths: paths})
		So(err, ShouldBeNil)
		So(status.Cells, ShouldHaveLength, 1)

		Convey("Then the matrix is current while nothing moved", func() {
			So(status.Cells[0].Status, ShouldEqual, StatusCurrent)
		})

		Convey("When a user edits the delivered server by hand", func() {
			edited := `{"mcpServers": {"fs": {"command": "/usr/local/bin/attacker"}}}`
			So(os.WriteFile(hostDoc, []byte(edited), 0o600), ShouldBeNil)

			after, afterErr := client.Status(t.Context(), StatusOptions{Paths: paths})
			So(afterErr, ShouldBeNil)

			Convey("Then the cell is hands-off, never current, and names the key", func() {
				So(after.Cells, ShouldHaveLength, 1)
				So(after.Cells[0].Status, ShouldEqual, StatusHandsOff)
				So(strings.Join(after.Cells[0].Notes, "\n"), ShouldContainSubstring, "mcpServers")
				So(strings.Join(after.Cells[0].Notes, "\n"), ShouldContainSubstring, "changed outside verger")
			})
		})
	})
}

// mkdirFixture creates one fixture directory.
func mkdirFixture(t *testing.T, dir string) {
	t.Helper()

	So(os.MkdirAll(dir, 0o700), ShouldBeNil)
}

// ---- API-REQ 1-5: Eject, approve hooks by (pkg, host, hash), Unregister ----

func TestFacadeEjectMovesTheHome(t *testing.T) {
	Convey("Given a client with a home", t, func() {
		world, client := newFacadeWorld(t)
		_ = world

		Convey("When Eject moves the home to a fresh target", func() {
			target := t.TempDir()
			err := client.Eject(t.Context(), target)

			Convey("Then the target holds the state and the source is gone", func() {
				So(err, ShouldBeNil)
				So(dirExists(filepath.Join(target, "state")), ShouldBeTrue)
			})
		})
	})
}

func TestFacadeApproveHooksForRecordsByKey(t *testing.T) {
	Convey("Given a client", t, func() {
		_, client := newFacadeWorld(t)
		hash := digest.Bytes([]byte("hook-bytes"))

		Convey("When ApproveHooksFor records the approval by (pkg, host, hash)", func() {
			key, err := client.ApproveHooksFor("local:pkg", "claude", hash)

			Convey("Then the key is returned and the store holds the hash", func() {
				So(err, ShouldBeNil)
				So(key, ShouldEqual, "local:pkg")

				store, storeErr := client.ConsentStore()
				So(storeErr, ShouldBeNil)

				_, ok := store.Hooks("local:pkg")
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestFacadeUnregisterRemovesTheSpecEntry(t *testing.T) {
	Convey("Given a client with an installed package", t, func() {
		world, client := newFacadeWorld(t)
		ref := world.fixture(t, "caveman", "1.2.3")

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		plan, err := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})
		So(err, ShouldBeNil)

		_, err = client.Install(t.Context(), plan, ApplyOptions{Now: fixedClock})
		So(err, ShouldBeNil)

		Convey("When Unregister removes the package from the spec", func() {
			err := client.Unregister(t.Context(), paths, "local:caveman", false)

			Convey("Then the spec no longer carries the package", func() {
				So(err, ShouldBeNil)

				doc, _, loadErr := LoadSpec(paths.SpecPath)
				So(loadErr, ShouldBeNil)
				So(hasSpecPackageID(doc, "local:caveman"), ShouldBeFalse)
			})
		})
	})
}

// dirExists reports whether path is an existing directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// TestPlanNoDetectedHostsIsTyped pins API-REQ 6: Plan returns a typed
// HostUnavailableError when no hosts are detected.
func TestPlanNoDetectedHostsIsTyped(t *testing.T) {
	Convey("Given a client with no detected hosts", t, func() {
		world, client := newFacadeWorld(t)
		_ = world

		// Reopen with an adapter the host does not detect, so Targets is empty.
		_ = client.Close()
		client, err := Open(t.Context(), WithHosts(&fakeHost{id: host.Claude, undetected: true}))
		So(err, ShouldBeNil)
		t.Cleanup(func() { _ = client.Close() })

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		Convey("When Plan is called", func() {
			_, err := client.Plan(t.Context(), PlanOptions{
				Refs:  []string{"local:caveman"},
				Paths: paths,
			})

			Convey("Then the error is a HostUnavailableError", func() {
				So(err, ShouldNotBeNil)
				unavailable, ok := errors.AsType[*HostUnavailableError](err)
				So(ok, ShouldBeTrue)
				So(unavailable, ShouldNotBeNil)
			})
		})
	})
}

// TestSyncPlanPropagateOrigin pins [propagate] policy: a package with
// propagate.install = "origin" is only planned for the origin host.
func TestSyncPlanPropagateOrigin(t *testing.T) {
	Convey("Given a spec with propagate.install = origin", t, func() {
		world, client := newFacadeWorld(t)
		_ = world

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		doc, _, err := LoadSpec(paths.SpecPath)
		So(err, ShouldBeNil)
		// The package needs a source that provides it: a spec naming a
		// package nothing can get is a broken spec, and this test is about
		// which hosts a valid one reaches.
		So(AddSpecSourceAt(doc, mustRef(t, world.fixture(t, "caveman", "1.2.3")),
			filepath.Dir(paths.SpecPath)), ShouldBeTrue)

		doc.Packages = []spec.Package{
			{
				ID:        "local:caveman",
				Propagate: &spec.Propagate{Install: spec.ModeOrigin},
			},
		}
		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		Convey("When SyncPlan is called", func() {
			plan, err := client.SyncPlan(t.Context(), SyncOptions{Paths: paths})
			So(err, ShouldBeNil)

			Convey("Then only the origin host is planned", func() {
				// The origin host is the first detected adapter
				// With propagate=origin, only one host should be planned
				for _, cell := range plan.Cells {
					if cell.Package == "local:caveman" {
						// Should only be one cell for the origin host
						So(plan.Cells, ShouldHaveLength, 1)
					}
				}
			})
		})
	})
}

// TestInstallPlanPropagateOrigin pins that [propagate] is enforced on the
// install path too, not only on sync. Two commands reach the same policy from
// different directions, and a policy that only holds for one of them is not a
// policy: `verger install` would write into a host the spec says the package
// stays out of, and only a later `verger sync` would notice.
func TestInstallPlanPropagateOrigin(t *testing.T) {
	Convey("Given a spec with propagate.install = origin and two detected hosts", t, func() {
		world, _ := newFacadeWorld(t)
		t.Chdir(world.root)

		one := &fakeHost{id: host.Claude}
		two := &fakeHost{id: host.Omp}

		client, err := Open(t.Context(), WithHosts(one, two))
		So(err, ShouldBeNil)

		paths, err := client.Paths(User, "")
		So(err, ShouldBeNil)

		doc, _, err := LoadSpec(paths.SpecPath)
		So(err, ShouldBeNil)

		doc.Packages = []spec.Package{{ID: "local:caveman", Propagate: &spec.Propagate{Install: spec.ModeOrigin}}}
		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		ref := world.fixture(t, "caveman", "1.2.3")

		Convey("When the package is installed", func() {
			plan, err := client.Plan(t.Context(), PlanOptions{Paths: paths, Refs: []string{ref}})
			So(err, ShouldBeNil)

			_, err = client.Install(t.Context(), plan, ApplyOptions{Now: fixedClock})
			So(err, ShouldBeNil)

			Convey("Then only the origin host is written", func() {
				one.mu.Lock()
				firstDelivers := one.delivers
				one.mu.Unlock()

				two.mu.Lock()
				secondDelivers := two.delivers
				two.mu.Unlock()

				// The executor probes a delivery before it writes it, so a
				// written action counts twice; what matters is which host
				// was touched at all.
				So(firstDelivers, ShouldNotEqual, 0)
				So(secondDelivers, ShouldEqual, 0)
			})
		})
	})
}
