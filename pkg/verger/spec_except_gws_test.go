package verger

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// `except` in the spec names the hosts one package must NOT reach.
//
// It reached nobody. The exclusion was applied where a reconcile wrote its
// CELLS, but the writes themselves are built later by buildInstallActions, which
// re-derived the host set from the full adapter list — so a plan could say
// "cursor excluded" while the executor wrote cursor anyway. Every writing
// command resolves its host set and then builds actions through ONE function,
// propagateTargets; that is where the spec package is already in hand, so that
// is where `except` belongs. One decision, four commands, no second copy of it.

// TestInstallLeavesOutAHostTheSpecExcludes is the defect, on the command that
// writes the most.
func TestInstallLeavesOutAHostTheSpecExcludes(t *testing.T) {
	Convey("Given a spec that excludes one host for a package", t, func() {
		world := newSharedWorld(t)

		paths, err := world.client.Paths(User, "")
		So(err, ShouldBeNil)

		pkgRoot := sharedSkillPackage(t, t.TempDir(), "caveman")

		// The spec names the package by the id the plan resolves, so the test
		// never has to guess verger's id convention.
		first, err := world.client.Plan(t.Context(), PlanOptions{
			Paths: paths, Refs: []string{pkgRoot}, Hosts: world.adapters,
		})
		So(err, ShouldBeNil)
		So(first.Packages, ShouldNotBeEmpty)

		id := first.Packages[0].Package.ID

		doc := spec.New()
		doc.Packages = []spec.Package{{ID: id, Except: []string{string(host.Omp)}}}
		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)

		plan, err := world.client.Plan(t.Context(), PlanOptions{
			Paths: paths, Refs: []string{pkgRoot}, Hosts: world.adapters,
		})
		So(err, ShouldBeNil)

		report, err := world.client.Install(t.Context(), plan, ApplyOptions{Confirm: allowAllConfirmer{}})
		So(err, ShouldBeNil)

		Convey("Then the excluded host is not among the delivered cells", func() {
			So(hostsIn(report), ShouldNotContain, host.Omp)
		})

		Convey("And the hosts the spec did not exclude still get it", func() {
			So(hostsIn(report), ShouldContain, host.Agy)
			So(hostsIn(report), ShouldContain, host.Codex)
		})

		Convey("And the shared file exists, because the two remaining hosts wrote it", func() {
			So(fileOnDisk(filepath.Join(world.home, ".agents", "skills", "caveman", "SKILL.md")), ShouldBeTrue)
		})
	})
}

// TestTheSeamNarrowsOnlyWhenThePlanCarriesTheDocument is the seam every writing
// command shares, tested as the pure function it is.
//
// install, update, sync and import all build their actions through
// buildInstallActions, which asks propagateTargets for each package's host set.
// So a decision made here IS the decision those four commands make — and the two
// ways it used to go wrong are both visible from here:
//
//   - no document, and nothing is narrowed. That is what a reconcile handed the
//     executor: SyncPlan kept the spec in its own field, so the plan that built
//     the writes could not read it, and `except` — and `[propagate]` with it —
//     silently became "all".
//   - a document present, and the named host is dropped.
//
// An integration test per command would have been the obvious way to write this
// and it was wrong: Sync and Update resolve their adapters themselves, and
// whether agy, codex and omp are "detected" depends on a marker directory or a
// binary on PATH. Those tests passed on the machine that wrote them and failed
// under `env -i`, which is the definition of a broken test.
func TestTheSeamNarrowsOnlyWhenThePlanCarriesTheDocument(t *testing.T) {
	Convey("Given a spec that excludes one host for a package", t, func() {
		home := t.TempDir()
		adapters := sharedRootHosts(t, home)

		doc := spec.New()
		doc.Packages = []spec.Package{{ID: "acme/caveman", Except: []string{string(host.Omp)}}}

		item := &PlannedPackage{
			Ref:     source.Ref{ID: "acme/caveman"},
			Package: host.Package{ID: "acme/caveman"},
		}

		Convey("Then a plan carrying the document narrows", func() {
			allowed, _ := propagateTargets(&Plan{doc: doc, Adapters: adapters}, item, adapters)

			So(allowed, ShouldHaveLength, 2)

			for _, a := range allowed {
				So(a.ID(), ShouldNotEqual, host.Omp)
			}
		})

		Convey("And a plan without one narrows nothing, which is the bug sync shipped", func() {
			allowed, _ := propagateTargets(&Plan{Adapters: adapters}, item, adapters)

			So(allowed, ShouldHaveLength, len(adapters))
		})
	})
}

// TestImportKeepsTheHostTheSpecExcludes is the fourth command, and its
// obligation is different in kind: `import` writes no host at all — it reads
// what the hosts already have and records it in the spec. So the way it can
// break an exclusion is by rewriting the document that carries it.
//
// AddSpecPackage leaves an entry that is already there alone, and this test is
// what keeps it that way: an import that rewrote `except` away would send the
// very next install straight at the host the user had excluded.
func TestImportKeepsTheHostTheSpecExcludes(t *testing.T) {
	Convey("Given a spec that excludes one host for a package import would re-add", t, func() {
		world := newSharedWorld(t)

		paths, err := world.client.Paths(User, "")
		So(err, ShouldBeNil)

		doc := spec.New()
		doc.Packages = []spec.Package{
			{ID: "local:caveman", Except: []string{string(host.Omp)}},
		}
		So(SaveSpec(paths.SpecPath, doc), ShouldBeNil)
		added, err := RecordImportsAt(paths.SpecPath, ImportPlan{
			Paths: paths,
			Candidates: []ImportCandidate{
				{ID: "local:caveman", Version: "1.2.3", Host: string(host.Omp)},
			},
		})
		So(err, ShouldBeNil)

		Convey("Then the import adds nothing, because the entry is already declared", func() {
			So(added, ShouldEqual, 0)
		})

		Convey("And the exclusion is still in the spec", func() {
			after, _, loadErr := LoadSpec(paths.SpecPath)
			So(loadErr, ShouldBeNil)
			So(ExceptFor(after, "local:caveman"), ShouldResemble, []string{string(host.Omp)})
		})
	})
}

// detectSharedRootHosts makes agy, codex and omp DETECTABLE in this test's own
// home, by creating the marker each adapter looks for before it falls back to a
// binary on PATH.
//
// This is what makes a test of `verger sync` hermetic. Sync resolves its own
// adapters — `SyncOptions` has no field to hand it any — so whether a host is
// detected decides whether the command has anything to write at all. Left to
// itself the answer came from whatever binaries happened to be installed: the
// tests passed on the machine that wrote them and failed under `env -i`.
//
// The paths are the adapters' own markers (agy.go:84, codex.go:409, omp.go:126),
// spelled here rather than referenced, because they are unexported. A host that
// renames its marker makes this test fail loudly rather than silently stop
// detecting — which is the right way round.
func detectSharedRootHosts(t *testing.T, home string) {
	t.Helper()

	for _, dir := range []string{
		filepath.Join(home, ".gemini", "antigravity-cli"), // agy state dir
		filepath.Join(home, ".codex"),                     // codex config dir
		filepath.Join(home, ".omp", "agent"),              // omp agent dir
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mark %s detectable: %v", dir, err)
		}
	}
}

// unwriteSharedRoot takes away what the shared-root hosts were given, so the
// next reconcile has work to do.
//
// Without it these tests prove nothing at all, and they passed for a while
// without it. A reconcile of a package already installed at this exact version
// is settled: `installedIsSettled` (sync.go:412) returns before a single cell is
// planned, the report comes back empty, and "the excluded host is not in the
// report" is true of a run that wrote to nobody. A receipt whose files are not
// on this disk is not an install — that is what a machine which received the
// vault looks like — so removing the files is what puts the hosts back to work.
func unwriteSharedRoot(t *testing.T, home string) {
	t.Helper()

	if err := os.RemoveAll(filepath.Join(home, ".agents", "skills")); err != nil {
		t.Fatalf("unwrite shared root: %v", err)
	}
}

// reconcileExcludedHost is the shape both reconcile verbs are checked in, and it
// is a function rather than a copy because `Sync` and `Update` are the same call
// with a flag: two hand-written bodies would drift apart, and the drift would
// be invisible, because each would still pass its own assertions.
//
// It installs a shared-root package, takes the delivered files back so the
// reconcile has work to do, excludes one host in the spec, and returns the hosts
// the run wrote cells for.
func reconcileExcludedHost(t *testing.T, world *sharedWorld, verb string) []host.ID {
	t.Helper()

	paths, err := world.client.Paths(User, "")
	So(err, ShouldBeNil)

	pkgRoot := sharedSkillPackage(t, t.TempDir(), "caveman")

	plan, err := world.client.Plan(t.Context(), PlanOptions{
		Paths: paths, Refs: []string{pkgRoot}, Hosts: world.adapters,
	})
	So(err, ShouldBeNil)
	So(plan.Packages, ShouldNotBeEmpty)

	id := plan.Packages[0].Package.ID

	_, err = world.client.Install(t.Context(), plan, ApplyOptions{Confirm: allowAllConfirmer{}})
	So(err, ShouldBeNil)

	detectSharedRootHosts(t, world.home)
	unwriteSharedRoot(t, world.home)
	exceptHost(t, paths, id, host.Omp)

	var report *apply.Report

	switch verb {
	case "sync":
		_, report, err = world.client.Sync(t.Context(), SyncOptions{
			Paths: paths, Only: id, Confirm: allowAllConfirmer{},
		})
	case "update":
		_, report, err = world.client.Update(t.Context(), UpdateOptions{
			Paths: paths, Only: id, Confirm: allowAllConfirmer{},
		})
	default:
		t.Fatalf("unknown verb %q", verb)
	}

	So(err, ShouldBeNil)

	return hostsIn(report)
}

// TestSyncLeavesOutAHostTheSpecExcludes is the reconcile itself, over detected
// adapters rather than injected ones.
func TestSyncLeavesOutAHostTheSpecExcludes(t *testing.T) {
	Convey("Given a spec that excludes one host for an installed package", t, func() {
		world := newSharedWorld(t)

		Convey("Then a reconcile writes no cell for the excluded host", func() {
			So(reconcileExcludedHost(t, world, "sync"), ShouldNotContain, host.Omp)
		})

		Convey("And it writes cells for the hosts the spec did not exclude", func() {
			// Without this the exclusion above is satisfied by a reconcile
			// that wrote to nobody, which is the shape a broken filter takes.
			So(reconcileExcludedHost(t, world, "sync"), ShouldContain, host.Agy)
		})
	})
}

// TestUpdateLeavesOutAHostTheSpecExcludes is the fourth writing command, and the
// one this report named without a test to show for it.
//
// `Update` is `Sync` with a flag, so it reaches the filter through the same call
// — which is the argument for not needing a test at all, and is exactly why it
// needs one: a command that is a synonym is the one whose divergence nobody
// notices, because every other command's test still passes. It is also the
// command whose host set comes from the update cooldown rather than a
// reconcile, so it is the one where "the same options" is a claim worth making
// out loud.
func TestUpdateLeavesOutAHostTheSpecExcludes(t *testing.T) {
	Convey("Given a spec that excludes one host for an installed package", t, func() {
		world := newSharedWorld(t)

		Convey("Then an update writes no cell for the excluded host", func() {
			So(reconcileExcludedHost(t, world, "update"), ShouldNotContain, host.Omp)
		})

		Convey("And the host it did not exclude is still written", func() {
			// Without this the exclusion would be satisfied by an update that
			// writes to nobody at all, which is the shape a broken filter takes.
			So(reconcileExcludedHost(t, world, "update"), ShouldContain, host.Agy)
		})
	})
}

// TestAReconcilePlanCarriesTheDocumentTwiceOver pins the invariant behind that
// field: a SyncPlan holds the spec in its own field AND in the Plan it embeds,
// and the two copies have to be the same document.
//
// They were not, and nothing failed — the second copy was simply empty, so the
// pass that builds the writes had nothing to read. Both pre-filters in the
// reconcile masked it for `except` and `[propagate]`, which is why no other test
// noticed; what it leaves behind is a second answer to "which document is
// this", and the next caller that reads the embedded one gets a nil.
func TestAReconcilePlanCarriesTheDocumentTwiceOver(t *testing.T) {
	Convey("Given a spec with a package in it", t, func() {
		world := newSharedWorld(t)

		paths, err := world.client.Paths(User, "")
		So(err, ShouldBeNil)

		pkgRoot := sharedSkillPackage(t, t.TempDir(), "caveman")

		plan, err := world.client.Plan(t.Context(), PlanOptions{
			Paths: paths, Refs: []string{pkgRoot}, Hosts: world.adapters,
		})
		So(err, ShouldBeNil)
		So(plan.Packages, ShouldNotBeEmpty)

		_, err = world.client.Install(t.Context(), plan, ApplyOptions{Confirm: allowAllConfirmer{}})
		So(err, ShouldBeNil)

		detectSharedRootHosts(t, world.home)

		reconcile, err := world.client.SyncPlan(t.Context(), SyncOptions{Paths: paths})
		So(err, ShouldBeNil)

		Convey("Then both copies of the document are present and are one document", func() {
			So(reconcile.spec(), ShouldNotBeNil)
			So(reconcile.doc, ShouldNotBeNil)
			So(reconcile.doc, ShouldEqual, reconcile.spec())
		})
	})
}

// exceptHost adds an exclusion to one package of the spec that is already there,
// leaving the sources `install` recorded alone. A reconcile resolves a package
// through those sources, so replacing the document would fail for a reason that
// has nothing to do with `except`.
func exceptHost(t *testing.T, paths Paths, id string, excluded host.ID) {
	t.Helper()

	doc, _, err := LoadSpec(paths.SpecPath)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}

	for i := range doc.Packages {
		if doc.Packages[i].ID != id {
			continue
		}

		doc.Packages[i].Except = append(doc.Packages[i].Except, string(excluded))
	}

	if err := SaveSpec(paths.SpecPath, doc); err != nil {
		t.Fatalf("save spec: %v", err)
	}
}

// TestTheSeamHonoursPropagateOnlyWhenThePlanCarriesTheDocument is why the
// document had to reach the plan a reconcile builds, and it is a second, quieter
// bug that went with the first.
//
// `[propagate]` is a property of the document rather than of a package row, so
// the install path could only honour it while a document was in hand. A
// SyncPlan kept the spec in its own field and left the embedded Plan's copy
// empty — which does not fail, it silently means "all": a spec that asked for
// `origin` or `ask` was reconciled as though it had said nothing.
func TestTheSeamHonoursPropagateOnlyWhenThePlanCarriesTheDocument(t *testing.T) {
	Convey("Given a spec that asks for the package to stay at one host", t, func() {
		home := t.TempDir()
		adapters := sharedRootHosts(t, home)

		doc := spec.New()
		doc.Packages = []spec.Package{
			{ID: "acme/caveman", Propagate: &spec.Propagate{Install: spec.ModeOrigin}},
		}

		item := &PlannedPackage{
			Ref:     source.Ref{ID: "acme/caveman"},
			Package: host.Package{ID: "acme/caveman"},
		}

		Convey("Then a plan carrying the document keeps only the origin", func() {
			allowed, origin := propagateTargets(&Plan{doc: doc, Adapters: adapters}, item, adapters)

			So(allowed, ShouldHaveLength, 1)
			So(origin, ShouldEqual, host.Agy)
		})

		Convey("And a plan without one keeps every host, which is what sync shipped", func() {
			allowed, origin := propagateTargets(&Plan{Adapters: adapters}, item, adapters)

			So(allowed, ShouldHaveLength, len(adapters))
			So(origin, ShouldEqual, host.ID(""))
		})
	})
}

// TestExceptDropsExactlyTheNamedHost pins the decision itself, over the real
// adapters. Two commands disagreeing about `except` is the whole failure, so the
// rule is worth one test of its own rather than only as a side effect.
func TestExceptDropsExactlyTheNamedHost(t *testing.T) {
	Convey("Given one package excluding one of the shared-root hosts", t, func() {
		home := t.TempDir()
		adapters := sharedRootHosts(t, home)

		doc := spec.New()
		doc.Packages = []spec.Package{{ID: "acme/caveman", Except: []string{string(host.Omp)}}}

		Convey("Then every call drops exactly that host", func() {
			for range 3 {
				kept := exceptTargets(doc, "acme/caveman", adapters)

				So(kept, ShouldHaveLength, 2)

				for _, adapter := range kept {
					So(adapter.ID(), ShouldNotEqual, host.Omp)
				}
			}
		})

		Convey("And a package naming no host excludes nobody", func() {
			So(exceptTargets(doc, "acme/other", adapters), ShouldHaveLength, len(adapters))
		})

		Convey("And a document naming no package excludes nobody", func() {
			So(exceptTargets(spec.New(), "acme/caveman", adapters), ShouldHaveLength, len(adapters))
		})
	})
}

// hostsIn names the hosts a report delivered to.
func hostsIn(report *apply.Report) []host.ID {
	var out []host.ID

	for _, cell := range report.Cells {
		out = append(out, cell.Host)
	}

	return out
}
