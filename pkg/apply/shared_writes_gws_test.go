package apply

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/receipt"
)

// A cell reports `current`, so a user believes the files are there. What decides
// that is not "the plan produced an artifact" — a receipt records intent — but
// the disk holding the bytes the artifact recorded.
//
// The silent success NIGHT-pR-24 measured was exactly this gap: three hosts, one
// shared file, nothing written, three `current` cells and exit 0. These tests
// pin the check itself, so removing it is a red test rather than a behaviour
// nobody notices until a user does.

// writeTree makes a skill-shaped directory and returns the path.
func writeTree(t *testing.T, dir string) string {
	t.Helper()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}

	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# skill\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", dir, err)
	}

	return dir
}

func treeArtifact(t *testing.T, dir, kind, name string) receipt.Artifact {
	t.Helper()

	sum, err := digest.Tree(dir)
	if err != nil {
		t.Fatalf("digest %s: %v", dir, err)
	}

	return receipt.Artifact{Kind: kind, Name: name, Path: dir, Digest: sum}
}

// TestUndeliveredArtifactsAreTheOnesTheDiskDoesNotHold is the check itself.
func TestUndeliveredArtifactsAreTheOnesTheDiskDoesNotHold(t *testing.T) {
	Convey("Given artifacts that some file backs and some do not", t, func() {
		root := t.TempDir()

		written := treeArtifact(t, writeTree(t, filepath.Join(root, "written")), "skill", "written")
		absent := receipt.Artifact{
			Kind: "skill", Name: "absent", Path: filepath.Join(root, "absent"),
			Digest: digest.Bytes([]byte("nothing was ever written")),
		}

		claimed := []receipt.Artifact{written, absent}

		Convey("Then exactly the one the disk does not hold is reported missing", func() {
			So(undeliveredArtifacts(claimed, nil), ShouldResemble, []string{absent.Path})
		})
	})

	Convey("Given an artifact whose bytes moved after the receipt recorded them", t, func() {
		root := t.TempDir()

		dir := writeTree(t, filepath.Join(root, "moved"))
		artifact := treeArtifact(t, dir, "skill", "moved")

		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# something else\n"), 0o600); err != nil {
			t.Fatalf("rewrite: %v", err)
		}

		Convey("Then it is missing, because a digest nobody matches is not delivered", func() {
			So(undeliveredArtifacts([]receipt.Artifact{artifact}, nil), ShouldResemble, []string{dir})
		})
	})

	Convey("Given artifacts only the host itself can confirm", t, func() {
		// An installed MCP server and a patch document are proven by the host's
		// own oracle, not by verger's bytes. Counting them as missing would turn
		// every such delivery into a failed cell.
		remote := []receipt.Artifact{
			{Kind: kindMCP, Name: "server", Path: "/nowhere/server"},
			{Kind: kindPatchDocument, Name: "home", Path: "/nowhere/home.toml"},
			{Kind: "skill", Name: "relative", Path: "relative/path"},
		}

		Convey("Then none of them is reported missing", func() {
			So(undeliveredArtifacts(remote, nil), ShouldBeEmpty)
		})
	})
}

// TestSplitSharedWritesRunsTheWriterAlone pins the phase split itself: the host
// that writes a shared target is separated from the parallel phase, and a plan
// with no shared target leaves every host where it was.
func TestSplitSharedWritesRunsTheWriterAlone(t *testing.T) {
	Convey("Given one shared target written by one of three hosts", t, func() {
		groups := []hostGroup{
			{host: "agy", index: []int{0}},
			{host: "codex", index: []int{1}},
			{host: "omp", index: []int{2}},
		}

		shared := []SharedTarget{{Path: "/shared", Writer: "codex", Hosts: []host.ID{"agy", "codex", "omp"}}}

		Convey("Then the writer runs first, alone", func() {
			writers, perHost := splitSharedWrites(groups, shared)

			So(writers, ShouldHaveLength, 1)
			So(writers[0].host, ShouldEqual, host.ID("codex"))
			So(perHost, ShouldHaveLength, 2)
		})
	})

	Convey("Given a plan with no shared target", t, func() {
		groups := []hostGroup{{host: "agy", index: []int{0}}, {host: "omp", index: []int{1}}}

		Convey("Then nothing is moved into the first phase", func() {
			writers, perHost := splitSharedWrites(groups, nil)

			So(writers, ShouldBeEmpty)
			So(perHost, ShouldHaveLength, 2)
		})
	})

	Convey("Given two shared targets written by different hosts", t, func() {
		groups := []hostGroup{
			{host: "agy", index: []int{0, 1}},
			{host: "omp", index: []int{2}},
		}

		shared := []SharedTarget{
			{Path: "/a", Writer: "agy", Hosts: []host.ID{"agy", "omp"}},
			{Path: "/b", Writer: "omp", Hosts: []host.ID{"agy", "omp"}},
		}

		Convey("Then each writer carries its WHOLE group, so no delivery is split", func() {
			writers, perHost := splitSharedWrites(groups, shared)

			So(writers, ShouldHaveLength, 2)
			So(perHost, ShouldBeEmpty)
			So(writers[0].index, ShouldResemble, []int{0, 1})
			So(writers[1].index, ShouldResemble, []int{2})
		})
	})
}

// TestTheSharedWriterFinishesBeforeAnyOtherHostDelivers is the phase seen from
// the outside.
//
// The writer is held at a gate while the other host is free to run. Were the
// shared write part of the parallel phase, the reader would deliver while the
// writer was still waiting and would find the shared file missing — which is
// the whole failure this phase exists to prevent, because a host that finds no
// file under a shared root is exactly the host that reports a delivery it did
// not make.
func TestTheSharedWriterFinishesBeforeAnyOtherHostDelivers(t *testing.T) {
	Convey("Given one host writing a path another host also references", t, func() {
		w := newWorld(t)

		shared := filepath.Join(w.root, "shared", "one", "SKILL.md")

		gate := make(chan struct{})

		writer := w.fake(t, fxClaude).
			write(fxPkg, fakeFile{Path: shared, Data: "# one\n"}).
			blockOn(fxPkg, gate)

		// The reader writes nothing of its own: it only asks whether the shared
		// file is on disk by the time its own delivery runs.
		w.fake(t, fxCodex).probe(fxPkg, shared)

		go func() {
			time.Sleep(50 * time.Millisecond)
			close(gate)
		}()

		report, err := w.run(t, t.Context(), Plan{
			Actions: []Action{
				installAction(fxClaude, ActionInstall, fxVersion, nil),
				installAction(fxCodex, ActionInstall, fxVersion, nil),
			},
			Shared: []SharedTarget{
				{Path: shared, Writer: fxClaude, Hosts: []host.ID{fxClaude, fxCodex}},
			},
		}, Options{})

		So(err, ShouldBeNil)

		Convey("Then the writer's delivery completed and wrote the shared file", func() {
			writer.mu.Lock()
			delivers := writer.delivers[fxPkg]
			writer.mu.Unlock()

			So(delivers, ShouldEqual, 1)
			So(fileExists(shared), ShouldBeTrue)
		})

		Convey("And the reader found the shared file already written", func() {
			So(w.fakes[fxCodex].probeOK[fxPkg], ShouldBeTrue)
		})

		So(report.Cells, ShouldHaveLength, 2)
	})
}
