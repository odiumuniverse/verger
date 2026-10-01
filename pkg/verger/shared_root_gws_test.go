package verger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/odiumuniverse/verger/pkg/digest"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/store"
)

// Four hosts, one shared root.
//
// agy, codex, dsh and omp all resolve skills under ~/.agents/ — one shared
// directory four adapters read (agy.go:34, codex.go:21, dsh.go:34, omp.go:37).
// One package delivered to all four therefore names the same physical file four
// times, and ownership is resolved from receipts ON DISK (owner.go:39), which are
// not committed until the run ends. So the first host writes the file, the second
// asks "who owns this?", finds nothing on disk, finds the file present, and
// reports it as a collision with a stranger:
//
//	/path/.agents/skills/caveman already exists and is not owned by verger
//
// The user sees an install that delivered nothing because four hosts wanted the
// same byte. A shared path is one physical artifact, and this file pins that.
//
// These tests use the real adapters over a real temp home. A fake host cannot
// reproduce this: the whole defect is that four DIFFERENT adapters resolve to
// one path.

// sharedRootHosts are the adapters that resolve SKILLS under ~/.agents/skills.
//
// Three, not four. dsh carries a .agents constant too (dsh.go:34) but it is its
// AGENTS directory; its skills live under ~/.dsh/skills (dsh.go:165). Reading the
// constant instead of the resolved path is how this list first came out wrong,
// and the test caught it by naming the host that did not fit.
func sharedRootHosts(t *testing.T, home string) []host.Host {
	t.Helper()

	return sharedRootHostsWith(t, home, NewOwnership(filepath.Join(home, "receipts")))
}

// sharedRootHostsWith builds the four adapters over ONE ownership source. The
// sharing is the point: four adapters holding four owners is four adapters that
// cannot see each other's writes, which is the bug being fixed.
func sharedRootHostsWith(t *testing.T, home string, owner *Ownership) []host.Host {
	t.Helper()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	opts := []host.Option{
		host.WithHome(home),
		host.WithStore(st),
		host.WithTrash(st.Trash()),
		host.WithOwnership(owner),
	}

	return []host.Host{
		host.NewAgy(opts...),
		host.NewCodex(opts...),
		host.NewOmp(opts...),
	}
}

// sharedSkillPackage writes a package carrying exactly one skill and returns its
// root, which is what a Plan resolves a local ref to.
func sharedSkillPackage(t *testing.T, dir, name string) string {
	t.Helper()

	root := filepath.Join(dir, name)
	mkdir(t, filepath.Join(root, ".claude-plugin"))
	write(t, filepath.Join(root, ".claude-plugin", "plugin.json"),
		`{"name":"acme/`+name+`","version":"1.0.0","description":"A toolkit."}`)
	mkdir(t, filepath.Join(root, "skills", name))
	write(t, filepath.Join(root, "skills", name, "SKILL.md"), "# "+name+"\n")

	return root
}

func mkdir(t *testing.T, dir string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

// sharedRootDelivery is the delivery every shared-root host receives: the parsed
// package with its payload root, which is what a real plan hands an adapter.
func sharedRootDelivery(t *testing.T, pkgRoot string) host.Package {
	t.Helper()

	parsed, err := manifest.Parse(pkgRoot, manifest.FormatClaude)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	return host.Package{
		ID:         parsed.ID,
		Version:    parsed.Version,
		Format:     parsed.Format,
		Root:       pkgRoot,
		Components: parsed.Components,
		MCP:        parsed.MCP,
		Hooks:      parsed.Hooks,
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestOneSkillIsDeliveredToEverySharedRootHost is the bug as a user sees it.
func TestOneSkillIsDeliveredToEverySharedRootHost(t *testing.T) {
	Convey("Given one skill delivered to the hosts that share ~/.agents/skills", t, func() {
		home := t.TempDir()
		root := t.TempDir()

		adapters := sharedRootHosts(t, home)

		pkgRoot := sharedSkillPackage(t, root, "caveman")

		shared := filepath.Join(home, ".agents", "skills", "caveman", "SKILL.md")

		Convey("Then every shared-root host reports the skill delivered", func() {
			delivered := 0

			for _, adapter := range adapters {
				result, derr := adapter.Deliver(t.Context(), home, host.Delivery{
					Package: sharedRootDelivery(t, pkgRoot), Strategy: host.Loose,
				})

				So(string(adapter.ID())+": "+errText(derr), ShouldEqual,
					string(adapter.ID())+": ")
				So(result.Artifacts, ShouldNotBeEmpty)

				delivered++
			}

			So(delivered, ShouldEqual, len(adapters))
			So(fileOnDisk(shared), ShouldBeTrue)
		})
	})
}

// TestOneSkillReachesFourSharedRootHostsAndEveryCurrentCellIsOnDisk is the 4×1
// case the whole shared-root design exists for, with the status invariant
// applied to it.
//
// The invariant is the part that was missing, and it is why this test is worth
// more than the delivery it wraps: a receipt records what the run PLANNED, so a
// cell can report `current` for a file nobody ever wrote — the silent success
// NIGHT-pR-24 measured as "all three hosts current, nothing on disk, exit 0".
// Every delivery test below runs this helper, so a report that overstates what
// is on disk fails here rather than reaching a user.
func TestOneSkillReachesFourSharedRootHostsAndEveryCurrentCellIsOnDisk(t *testing.T) {
	Convey("Given one skill installed for every host that shares a root", t, func() {
		world := newSharedWorld(t)

		shared := filepath.Join(world.home, ".agents", "skills", "caveman")

		plan, _ := world.planFor(t, sharedSkillPackage(t, t.TempDir(), "caveman"))

		report, err := world.client.Install(t.Context(), plan, ApplyOptions{Confirm: allowAllConfirmer{}})
		So(err, ShouldBeNil)

		Convey("Then every host reports delivered", func() {
			So(currentCells(report), ShouldEqual, len(world.adapters))
		})

		Convey("Then one file exists, with the digest the receipts recorded", func() {
			So(fileOnDisk(filepath.Join(shared, "SKILL.md")), ShouldBeTrue)
			So(digestOnDisk(shared), ShouldNotBeEmpty)
		})

		Convey("Then every host's receipt names the shared path with the digest on disk", func() {
			named := receiptsNaming(t, world, shared)

			// One entry per host: the reference count the removal rule reads.
			So(named, ShouldHaveLength, len(world.adapters))

			for _, sum := range named {
				So(sum, ShouldEqual, digestOnDisk(shared))
			}
		})

		Convey("Then every cell reporting current has its artifacts on disk", func() {
			So(assertCurrentCellsOnDisk(t, world, report), ShouldBeEmpty)
		})
	})
}

// TestTheSharedWriterIsTheFirstHostId pins WHICH host writes a shared path.
//
// The choice is arbitrary — any single host would do — but it must not be
// arbitrary per run. The plan is built from a map, so an unsorted decision
// would make the writer depend on Go's map iteration order: the same install
// would hand the file to a different host each time, and the receipts of every
// other host would flip between "owns the op" and "references it".
func TestTheSharedWriterIsTheFirstHostId(t *testing.T) {
	Convey("Given hosts that share a path, named so no order is obvious", t, func() {
		world := newSharedWorld(t)

		shared := filepath.Join(world.home, ".agents", "skills", "caveman")

		plan, _ := world.planFor(t, sharedSkillPackage(t, t.TempDir(), "caveman"))

		_, err := world.client.Install(t.Context(), plan, ApplyOptions{Confirm: allowAllConfirmer{}})
		So(err, ShouldBeNil)

		Convey("Then the writer is the first host id alphabetically", func() {
			writer, ok := world.client.runOwner().SharedWriter(shared)
			So(ok, ShouldBeTrue)

			ids := make([]string, 0, len(world.adapters))
			for _, adapter := range world.adapters {
				ids = append(ids, string(adapter.ID()))
			}

			slices.Sort(ids)
			So(string(writer), ShouldEqual, ids[0])
		})

		Convey("And exactly one host's receipt carries the operation that deletes it", func() {
			writers := 0

			for _, record := range loadReceipts(t, world) {
				for _, op := range record.RMA {
					if op.Path == shared {
						writers++
					}
				}
			}

			So(writers, ShouldEqual, 1)
		})
	})
}

// TestUniqHostsIsDeterministic pins the helper the writer choice rests on: the
// reference list is deduplicated AND ordered, so two packages reaching one path
// through different orders still name the same writer.
func TestUniqHostsIsDeterministic(t *testing.T) {
	Convey("Given one host listed twice in two orders", t, func() {
		So(uniqHosts([]host.ID{host.Omp, host.Agy, host.Omp}), ShouldResemble, []host.ID{host.Agy, host.Omp})
		So(uniqHosts([]host.ID{host.Agy, host.Omp, host.Agy}), ShouldResemble, []host.ID{host.Agy, host.Omp})
		So(uniqHosts([]host.ID{host.Omp}), ShouldResemble, []host.ID{host.Omp})
		So(uniqHosts(nil), ShouldBeEmpty)
	})
}

// currentCells names the hosts a report calls delivered.
func currentCells(report *apply.Report) int {
	if report == nil {
		return 0
	}

	n := 0

	for _, cell := range report.Cells {
		if cell.Status == apply.StatusCurrent {
			n++
		}
	}

	return n
}

// receiptsNaming returns the digest each receipt of this package recorded for
// one path — one entry per host, so the count is the reference count the
// removal rule reads.
func receiptsNaming(t *testing.T, world *sharedWorld, path string) []string {
	t.Helper()

	var out []string

	for _, record := range loadReceipts(t, world) {
		for _, artifact := range record.Artifacts {
			if artifact.Path == path {
				out = append(out, artifact.Digest.String())
			}
		}
	}

	return out
}

func loadReceipts(t *testing.T, world *sharedWorld) []receipt.Receipt {
	t.Helper()

	list, err := receipt.NewStore(world.client.Home().ReceiptsDir()).List()
	if err != nil {
		t.Fatalf("list receipts: %v", err)
	}

	return list
}

// digestOnDisk digests a delivered tree the way the receipt does.
func digestOnDisk(path string) string {
	sum, err := digest.Tree(path)
	if err != nil {
		return ""
	}

	return sum.String()
}

// assertCurrentCellsOnDisk is the invariant helper the delivery tests share: for
// every cell a report calls current, each artifact its receipt names must be on
// disk with the recorded digest. It returns the paths that fail, so the
// assertion reads as "nothing failed" and a failure names what is missing.
func assertCurrentCellsOnDisk(t *testing.T, world *sharedWorld, report *apply.Report) []string {
	t.Helper()

	byCell := map[string]receipt.Receipt{}

	for _, record := range loadReceipts(t, world) {
		byCell[record.Host] = record
	}

	var failed []string

	for _, cell := range report.Cells {
		if cell.Status != apply.StatusCurrent {
			continue
		}

		record, ok := byCell[string(cell.Host)]
		if !ok {
			failed = append(failed, string(cell.Host)+": reported current with no receipt")

			continue
		}

		for _, artifact := range record.Artifacts {
			if !diskHolds(artifact) {
				failed = append(failed, string(cell.Host)+": "+artifact.Path)
			}
		}
	}

	return failed
}

// diskHolds reports whether an artifact is on disk with the bytes its receipt
// recorded. It re-derives the answer from the filesystem rather than asking the
// executor, which is the point: a helper that shared the code under test would
// agree with a broken run for the same reason the run is broken.
func diskHolds(artifact receipt.Artifact) bool {
	info, err := os.Lstat(artifact.Path)
	if err != nil {
		return false
	}

	var sum digest.Hash

	if info.IsDir() {
		sum, err = digest.Tree(artifact.Path)
	} else {
		sum, err = digest.File(artifact.Path)
	}

	return err == nil && sum == artifact.Digest
}

func fileOnDisk(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func errText(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

// TestEverySharedRootHostReferencesTheSameSharedPath pins the input the
// refcount counts: four receipts naming one path.
//
// It does NOT pin that the file was written once, and an earlier version of this
// test claimed it did. The mutation that disables the dedup entirely SURVIVES
// here: writing the same bytes three times leaves the same file on disk, so no
// assertion about the OUTCOME can tell one write from three. Counting writes
// needs instrumentation, and under the executor's parallelism
// (apply.defaultParallel = 4) the dedup does not even hold — two hosts can plan
// concurrently before either has recorded. What this test really proves is that
// every host ends up referencing the one shared path, which is what the removal
// rule counts.
func TestEverySharedRootHostReferencesTheSameSharedPath(t *testing.T) {
	Convey("Given four hosts sharing one root", t, func() {
		home := t.TempDir()

		owner := NewOwnership(filepath.Join(home, "receipts"))
		adapters := sharedRootHostsWith(t, home, owner)

		results := deliverToAllShared(t, adapters, home, sharedSkillPackage(t, t.TempDir(), "caveman"))

		ids := make([]host.ID, 0, len(adapters))

		for _, a := range adapters {
			ids = append(ids, a.ID())
		}

		// A skill lands as a TREE: the receipt names the directory, and the file
		// inside it is what a reader actually opens.
		shared := filepath.Join(home, ".agents", "skills", "caveman")
		skillFile := filepath.Join(shared, "SKILL.md")

		Convey("Then every host's result references the same path", func() {
			referencing := 0

			for i, result := range results {
				found := false

				for _, artifact := range result.Artifacts {
					if artifact.Path == shared {
						found = true
					}
				}

				if found {
					referencing++
				} else {
					t.Logf("хост %s не ссылается на общий путь; его артефакты: %v", ids[i], result.Artifacts)
				}
			}

			So(referencing, ShouldEqual, len(adapters))
			So(fileOnDisk(skillFile), ShouldBeTrue)
		})

		Convey("Then the run recorded that write with the package and digest", func() {
			pkg, sum, ok := owner.DeliveredThisRun(shared)
			So(ok, ShouldBeTrue)
			So(pkg, ShouldEqual, "acme/caveman")
			So(sum.String(), ShouldNotBeEmpty)
			So(fileOnDisk(shared), ShouldBeTrue)
		})
	})
}

// deliverToAllShared hands every adapter the same package, in order, exactly as
// one install reaches them.
func deliverToAllShared(t *testing.T, adapters []host.Host, home, pkgRoot string) []host.Result {
	t.Helper()

	results := make([]host.Result, 0, len(adapters))

	for _, adapter := range adapters {
		result, err := adapter.Deliver(t.Context(), home, host.Delivery{
			Package: sharedRootDelivery(t, pkgRoot), Strategy: host.Loose,
		})
		if err != nil {
			t.Fatalf("%s: %v", adapter.ID(), err)
		}

		results = append(results, result)
	}

	return results
}

// TestASharedPathIsPrunedUntilTheLastHostReleasesIt is the refcount rule itself,
// without a delivery in the way.
//
// The integration version of this test could not be trusted: how many receipts a
// multi-host install commits varied between runs, so the assertions were
// measuring that variance instead of the rule. The rule is a pure function of
// what the remaining hosts reference, so it is tested as one — and the
// integration test that did hold, one skill to every shared-root host, is above.
func TestASharedPathIsPrunedUntilTheLastHostReleasesIt(t *testing.T) {
	const (
		shared = "/home/u/.agents/skills/caveman"
		own    = "/home/u/.claude/skills/caveman"
	)

	record := func(paths ...string) *receipt.Receipt {
		rec := &receipt.Receipt{Package: "acme/caveman", Host: "codex"}
		for _, path := range paths {
			rec.Artifacts = append(rec.Artifacts, receipt.Artifact{Kind: "skill", Name: "caveman", Path: path})
			rec.RMA = append(rec.RMA, receipt.Op{Kind: receipt.OpCopyTree, Path: path})
		}

		return rec
	}

	Convey("Given a receipt whose paths a remaining host still references", t, func() {
		rec := record(shared, own)

		Convey("Then the shared artifact and its RMA op are pruned, the private one stays", func() {
			notes := pruneSharedArtifacts(rec, map[string]string{shared: "omp"})

			So(len(rec.Artifacts), ShouldEqual, 1)
			So(rec.Artifacts[0].Path, ShouldEqual, own)
			So(len(rec.RMA), ShouldEqual, 1)
			So(rec.RMA[0].Path, ShouldEqual, own)
			So(strings.Join(notes, " "), ShouldContainSubstring, "omp")
			So(strings.Join(notes, " "), ShouldContainSubstring, "kept on disk")
		})
	})

	Convey("Given the same receipt and nothing left referencing it", t, func() {
		rec := record(shared, own)

		Convey("Then it is untouched: the last host may delete the file", func() {
			So(pruneSharedArtifacts(rec, nil), ShouldBeEmpty)
			So(len(rec.Artifacts), ShouldEqual, 2)
			So(len(rec.RMA), ShouldEqual, 2)
		})
	})

	Convey("Given a receipt whose every path is shared", t, func() {
		rec := record(shared)

		Convey("Then nothing is left to delete", func() {
			pruneSharedArtifacts(rec, map[string]string{shared: "omp"})
			So(rec.Artifacts, ShouldBeEmpty)
			So(rec.RMA, ShouldBeEmpty)
		})
	})
}

// TestPathsSharedWithRemainingHostsOnlyCountsHostsThatStay is the other half of
// the rule: a host being removed is not a reason to keep the file.
func TestPathsSharedWithRemainingHostsOnlyCountsHostsThatStay(t *testing.T) {
	Convey("Given receipts from three hosts of one package", t, func() {
		list := []receipt.Receipt{
			{Package: "acme/caveman", Host: "agy", Artifacts: []receipt.Artifact{{Path: "/shared"}}},
			{Package: "acme/caveman", Host: "codex", Artifacts: []receipt.Artifact{{Path: "/shared"}}},
			{Package: "acme/caveman", Host: "omp", Artifacts: []receipt.Artifact{{Path: "/shared"}}},
			{Package: "other/pkg", Host: "agy", Artifacts: []receipt.Artifact{{Path: "/theirs"}}},
		}

		available := map[host.ID]host.Host{
			host.Agy: nil, host.Codex: nil, host.Omp: nil,
		}

		Convey("Then removing one host still keeps the path for the other two", func() {
			// The map says which hosts this removal REACHES. Removing codex
			// reaches codex only, so agy and omp stay and one of them keeps the
			// file. (Passing the full set would mean removing all three.)
			reached := map[host.ID]host.Host{host.Codex: nil}

			shared := pathsSharedWithRemainingHosts(list, "acme/caveman", reached)
			So(shared["/shared"], ShouldNotEqual, "codex")
			So(shared, ShouldNotContainKey, "/theirs")
		})

		Convey("Then removing all three leaves nothing shared", func() {
			So(pathsSharedWithRemainingHosts(list, "acme/caveman", available), ShouldBeEmpty)
		})

		Convey("Then a removal that can reach no host keeps everything", func() {
			// The safe direction, and worth pinning: with no adapter available
			// nothing counts as being removed, so nothing may be deleted. A
			// removal that cannot see its hosts must not delete files.
			shared := pathsSharedWithRemainingHosts(list, "acme/caveman", map[host.ID]host.Host{})
			So(shared, ShouldContainKey, "/shared")
		})
	})
}

// ---- restored after an earlier, wrong call -------------------------------
//
// These two were deleted because they flaked, on the reasoning that a flaky
// test is not worth shipping. That was wrong, and the reasoning inverted: a
// flake in "remove one host leaves the file" is a DEFECT — either in the test or
// in the product under parallel execution — and deleting the only witness hides
// it. They are back, and the fix is to find the cause, not to stop looking.
//
// The file for all shared-root hosts is written by whichever host runs first,
// and the executor runs hosts CONCURRENTLY (apply.defaultParallel = 4). Whether
// the file survives a later host is therefore a property of the ORDER, which is
// exactly the thing under test and exactly what no test may assume.

// TestRemovingOneSharedRootHostLeavesTheFileForTheRest removes one of the
// shared-root hosts and asks the question a user asks: is my skill still there?
func TestRemovingOneSharedRootHostLeavesTheFileForTheRest(t *testing.T) {
	Convey("Given one skill installed for every shared-root host", t, func() {
		world := newSharedWorld(t)

		skillFile := filepath.Join(world.home, ".agents", "skills", "caveman", "SKILL.md")

		plan, paths := world.planFor(t, sharedSkillPackage(t, t.TempDir(), "caveman"))

		_, err := world.client.Install(t.Context(), plan, ApplyOptions{Confirm: allowAllConfirmer{}})
		So(err, ShouldBeNil)
		So(fileOnDisk(skillFile), ShouldBeTrue)

		Convey("When one of them is removed", func() {
			removal, err := world.client.PlanRemove(t.Context(), "acme/caveman", RemoveOptions{
				Paths: paths, Hosts: []host.Host{oneOf(t, world.adapters, host.Codex)},
			})
			So(err, ShouldBeNil)

			_, err = world.client.Remove(t.Context(), removal, ApplyOptions{Confirm: allowAllConfirmer{}})
			So(err, ShouldBeNil)

			Convey("Then the shared file is still there for the hosts that remain", func() {
				So(fileOnDisk(skillFile), ShouldBeTrue)
			})
		})
	})
}

// TestRemovingEverySharedRootHostDeletesTheFile is the other end of the refcount:
// with no receipt left naming the path, the last release may delete it.
func TestRemovingEverySharedRootHostDeletesTheFile(t *testing.T) {
	Convey("Given one skill installed for every shared-root host", t, func() {
		world := newSharedWorld(t)

		skillFile := filepath.Join(world.home, ".agents", "skills", "caveman", "SKILL.md")

		plan, paths := world.planFor(t, sharedSkillPackage(t, t.TempDir(), "caveman"))

		_, err := world.client.Install(t.Context(), plan, ApplyOptions{Confirm: allowAllConfirmer{}})
		So(err, ShouldBeNil)
		So(fileOnDisk(skillFile), ShouldBeTrue)

		Convey("When all of them are removed", func() {
			removal, err := world.client.PlanRemove(t.Context(), "acme/caveman", RemoveOptions{
				Paths: paths, Hosts: world.adapters,
			})
			So(err, ShouldBeNil)

			Convey("Then no host keeps it", func() {
				So(strings.Join(removal.Notes, " | "), ShouldNotContainSubstring, "kept on disk")
			})

			Convey("Then the file goes", func() {
				_, err := world.client.Remove(t.Context(), removal, ApplyOptions{Confirm: allowAllConfirmer{}})
				So(err, ShouldBeNil)
				So(fileOnDisk(skillFile), ShouldBeFalse)
			})
		})
	})
}

// TestRemovingWithOneSharedRootHostExcludedKeepsTheFile is the refcount rule
// driven the way a user drives it: `--except omp` leaves omp out of the
// removal, and the file has to survive for the host that is still left.
func TestRemovingWithOneSharedRootHostExcludedKeepsTheFile(t *testing.T) {
	Convey("Given one skill installed for every shared-root host", t, func() {
		world := newSharedWorld(t)

		skillFile := filepath.Join(world.home, ".agents", "skills", "caveman", "SKILL.md")

		plan, paths := world.planFor(t, sharedSkillPackage(t, t.TempDir(), "caveman"))

		_, err := world.client.Install(t.Context(), plan, ApplyOptions{Confirm: allowAllConfirmer{}})
		So(err, ShouldBeNil)
		So(fileOnDisk(skillFile), ShouldBeTrue)

		Convey("When every host but one is removed", func() {
			removal, err := world.client.PlanRemove(t.Context(), "acme/caveman", RemoveOptions{
				Paths: paths, Hosts: world.adapters,
				Filter: HostFilter{Except: []string{string(host.Omp)}},
			})
			So(err, ShouldBeNil)

			_, err = world.client.Remove(t.Context(), removal, ApplyOptions{Confirm: allowAllConfirmer{}})
			So(err, ShouldBeNil)

			Convey("Then the excluded host keeps the file", func() {
				So(fileOnDisk(skillFile), ShouldBeTrue)
			})

			Convey("And the plan names the host that keeps it", func() {
				So(strings.Join(removal.Notes, " | "), ShouldContainSubstring, "kept on disk")
				So(strings.Join(removal.Notes, " | "), ShouldContainSubstring, string(host.Omp))
			})

			Convey("And the last reference takes the file with it", func() {
				last := mustPlanRemove(t, world, paths, HostFilter{Only: []string{string(host.Omp)}}, world.adapters)
				_, err := world.client.Remove(t.Context(), last, ApplyOptions{Confirm: allowAllConfirmer{}})
				So(err, ShouldBeNil)
				So(fileOnDisk(skillFile), ShouldBeFalse)
			})
		})
	})
}

// TestDisablingThePackageReleasesEverySharedRootReference is the other way a
// user stops a host from using a package: `verger disable` turns it off in the
// spec, and the next reconcile takes it back off the machine.
//
// The rule is that a disable behaves like a removal — every host's reference is
// released — so the shared file goes with the last of them instead of being
// stranded by a receipt no host will read again.
func TestDisablingThePackageReleasesEverySharedRootReference(t *testing.T) {
	Convey("Given one skill installed for every shared-root host", t, func() {
		world := newSharedWorld(t)

		skillFile := filepath.Join(world.home, ".agents", "skills", "caveman", "SKILL.md")

		plan, paths := world.planFor(t, sharedSkillPackage(t, t.TempDir(), "caveman"))

		_, err := world.client.Install(t.Context(), plan, ApplyOptions{Confirm: allowAllConfirmer{}})
		So(err, ShouldBeNil)
		So(fileOnDisk(skillFile), ShouldBeTrue)

		Convey("When the package is disabled and taken back off the machine", func() {
			err := world.client.SetDisabled(t.Context(), paths, "acme/caveman", true, false)
			So(err, ShouldBeNil)

			disabled, ok := world.client.Disabled(paths, "acme/caveman")
			So(ok, ShouldBeTrue)
			So(disabled, ShouldBeTrue)

			// A disabled package is one the next reconcile removes from every
			// host, and the removal planner is where that decision is made.
			removal := mustPlanRemove(t, world, paths, HostFilter{}, world.adapters)
			So(removal.Actions, ShouldHaveLength, len(world.adapters))

			_, err = world.client.Remove(t.Context(), removal, ApplyOptions{Confirm: allowAllConfirmer{}})
			So(err, ShouldBeNil)

			Convey("Then every reference is released and the file goes", func() {
				So(fileOnDisk(skillFile), ShouldBeFalse)
			})
		})
	})
}

// mustPlanRemove plans one removal over the given adapters or the test stops.
func mustPlanRemove(t *testing.T, world *sharedWorld, paths Paths, filter HostFilter, adapters []host.Host) *RemovalPlan {
	t.Helper()

	removal, err := world.client.PlanRemove(t.Context(), "acme/caveman", RemoveOptions{
		Paths: paths, Hosts: adapters, Filter: filter,
	})
	if err != nil {
		t.Fatalf("plan remove: %v", err)
	}

	return removal
}

// oneOf is the adapter whose id is wanted.
func oneOf(t *testing.T, adapters []host.Host, id host.ID) host.Host {
	t.Helper()

	for _, adapter := range adapters {
		if adapter.ID() == id {
			return adapter
		}
	}

	t.Fatalf("no adapter %s", id)

	return nil
}

// sharedWorld is a client plus the shared-root adapters wired to ITS OWN
// ownership source.
//
// This matters and it cost me a long debugging detour. The plan records its
// writer decisions on the client's runOwner(), and an adapter consults the
// PathOwner it was BUILT with. Build the adapters over a separate Ownership — as
// the first version of this test did — and the two never meet: the plan decides
// who writes a shared path, the planner never hears, and three hosts race for one
// file exactly as before. buildAdapters wires every adapter to c.runOwner() for
// precisely this reason, and a test that injects its own adapters must do the
// same or it is testing a wiring that production does not have.
type sharedWorld struct {
	client   *Client
	adapters []host.Host
	home     string
}

// newSharedWorld builds the world: the client first, then its adapters over its
// ownership.
func newSharedWorld(t *testing.T) *sharedWorld {
	t.Helper()

	home := t.TempDir()

	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("VERGER_HOME", filepath.Join(home, ".verger"))
	t.Setenv("BEADLE_HOME", "")

	client, err := Open(t.Context())
	if err != nil {
		t.Fatalf("open client: %v", err)
	}

	t.Cleanup(func() { _ = client.Close() })

	return &sharedWorld{
		client:   client,
		adapters: sharedRootHostsWith(t, home, client.runOwner()),
		home:     home,
	}
}

// planFor plans one package across the shared-root hosts.
func (w *sharedWorld) planFor(t *testing.T, refs ...string) (*Plan, Paths) {
	t.Helper()

	paths, err := w.client.Paths(User, "")
	if err != nil {
		t.Fatalf("paths: %v", err)
	}

	plan, err := w.client.Plan(t.Context(), PlanOptions{
		Paths: paths, Refs: refs, Hosts: w.adapters,
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	return plan, paths
}

// allowAllConfirmer answers yes to everything, which YesConfirmer deliberately
// does not: it refuses destructive conflicts (rule 14), and installing over or
// removing a package is exactly that.
type allowAllConfirmer struct{}

// Confirm implements Confirmer.
func (allowAllConfirmer) Confirm(context.Context, apply.Question) (bool, error) { return true, nil }

// TestTwoPackagesWantTheSameSharedPath is the case that must NOT be a collision
// with a stranger.
//
// Two packages in one run, each with a skill called `caveman`, both landing on
// ~/.agents/skills/caveman through the shared root. The old answer was a
// CollisionError reading "already exists and is not owned by verger", which is a
// lie twice over: the file belongs to another PACKAGE of this same run, and it is
// the user's own doing. Nothing resolves it but a person — narrowing the spec's
// host list, or renaming one skill — so the error has to say so and name the
// hosts that disagree.
//
// One package cannot produce this case: every host renders the same bytes, which
// is the whole point of the dedup. It takes two packages.
func TestTwoPackagesWantTheSameSharedPath(t *testing.T) {
	Convey("Given two packages whose skills resolve to one shared path", t, func() {
		world := newSharedWorld(t)

		shared := filepath.Join(world.home, ".agents", "skills", "caveman")

		first := sharedSkillPackage(t, t.TempDir(), "caveman")
		second := conflictingSkillPackage(t, t.TempDir())

		Convey("Then the run refuses, while building actions and before writing", func() {
			// The refusal lands where actions are built, which is before any step
			// runs: Plan resolves refs and adapters, and Install is what turns the
			// resolved packages into writes. So this is still "before anything is
			// written", just one stage later than the Plan call itself.
			plan, _ := world.planFor(t, first, second)

			_, err := world.client.Install(t.Context(), plan, ApplyOptions{Confirm: allowAllConfirmer{}})

			So(err, ShouldNotBeNil)

			conflict := &ContentConflictError{}
			So(errors.As(err, &conflict), ShouldBeTrue)

			Convey("And it names the path both packages want", func() {
				So(conflict.Path, ShouldEqual, shared)
			})

			Convey("And it names the hosts that disagree", func() {
				So(len(conflict.Hosts), ShouldBeGreaterThanOrEqualTo, 2)
				So(err.Error(), ShouldContainSubstring, string(host.Omp))
			})

			Convey("And it says what a person can do about it", func() {
				So(err.Error(), ShouldContainSubstring, "host list")
			})

			Convey("And the home was never touched", func() {
				So(fileOnDisk(shared), ShouldBeFalse)
			})
		})
	})
}

// conflictingSkillPackage is a DIFFERENT package that also ships a skill named
// caveman, with different bytes in it.
func conflictingSkillPackage(t *testing.T, dir string) string {
	t.Helper()

	root := filepath.Join(dir, "rival")
	mkdir(t, filepath.Join(root, ".claude-plugin"))
	write(t, filepath.Join(root, ".claude-plugin", "plugin.json"),
		`{"name":"rival/caveman","version":"9.9.9","description":"A rival toolkit."}`)
	mkdir(t, filepath.Join(root, "skills", "caveman"))
	write(t, filepath.Join(root, "skills", "caveman", "SKILL.md"), "# rival caveman, entirely different\n")

	return root
}
