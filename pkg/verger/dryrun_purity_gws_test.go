package verger

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/store"
)

// A dry run that writes is not a dry run.
//
// This is the precondition for reading target paths out of a planning-time
// Deliver(DryRun): if any adapter touches the home on that path, then learning
// "where would this go" is itself a mutation, and the plan would be editing the
// machine it claims only to be reading.
//
// So the claim is tested the only way that means anything: every adapter, the
// whole home tree, byte for byte, before and after. A test that only counted
// files would miss a rewrite of one that already existed, which is the more
// dangerous half.
func TestADryRunTouchesNothing(t *testing.T) {
	Convey("Given every adapter this build ships", t, func() {
		factories := adapterFactories()

		So(len(factories), ShouldBeGreaterThanOrEqualTo, 10)

		for _, factory := range factories {
			Convey("When "+hostName(t, factory)+" is asked for a dry run", func() {
				home := t.TempDir()

				st := dryRunStore(t)
				adapter := factory(
					host.WithHome(home),
					host.WithStore(st),
					host.WithTrash(st.Trash()),
					host.WithOwnership(NewOwnership(filepath.Join(home, "receipts"))),
				)

				// The home already holds a file of its own, so a dry run that
				// rewrites something it did not create is caught too.
				existing := filepath.Join(home, "pre-existing.txt")
				if err := os.WriteFile(existing, []byte("untouched\n"), 0o600); err != nil {
					t.Fatalf("seed home: %v", err)
				}

				before := snapshotTree(t, home)

				result, err := adapter.Deliver(t.Context(), home, host.Delivery{
					Package:  sharedRootDelivery(t, sharedSkillPackage(t, t.TempDir(), "caveman")),
					Strategy: host.Loose,
					DryRun:   true,
				})
				So(string(adapter.ID())+": "+errText(err), ShouldEqual, string(adapter.ID())+": ")

				Convey("Then it reports what it would have done", func() {
					So(len(result.Artifacts), ShouldBeGreaterThan, 0)
				})

				Convey("Then the home is byte-for-byte what it was", func() {
					So(snapshotTree(t, home), ShouldResemble, before)
				})
			})
		}
	})
}

// snapshotTree is every file under root as "path|size|digest", sorted. Size and
// digest together catch a rewrite that keeps the length.
func snapshotTree(t *testing.T, root string) []string {
	t.Helper()

	var out []string

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		data, readErr := os.ReadFile(path) //nolint:gosec // G304: walking the test's own temp dir
		if readErr != nil {
			return readErr
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}

		out = append(out, rel+"|"+itoa(len(data))+"|"+digestOf(data))

		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}

	sort.Strings(out)

	return out
}

// dryRunStore is a store over its own temp dir; the dry run must not touch it
// either, but the tree snapshot above is the claim under test.
func dryRunStore(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	return st
}

// hostName labels the Convey so a failure says WHICH adapter misbehaved, which is
// the whole point of running all ten.
func hostName(t *testing.T, factory func(...host.Option) host.Host) string {
	t.Helper()

	return string(factory(host.WithHome(t.TempDir())).ID())
}

func digestOf(data []byte) string {
	return digest.Bytes(data).String()
}

func itoa(n int) string { return strconv.Itoa(n) }
