package cli

import (
	"path/filepath"
	"testing"

	"github.com/odiumuniverse/verger/pkg/host"

	. "github.com/smartystreets/goconvey/convey"
)

// A machine has more than one agent. A skill delivered to four of them is four
// files, and the user who removes it from one of them is asking about that one
// file.
//
// `verger remove --hosts claude` answered a different question. The flag was
// registered, the plan printed all four cells, and then every host was
// uninstalled: `RemoveOptions.Filter` was never read when the caller supplied
// its own adapter list, and the CLI supplies one. The three files the user
// wanted to keep were gone before the run reported what it had done, and the
// plan on screen had said the opposite.
//
// The assertions are on the files and on the cells, because those are what the
// user has afterwards. A host adapter's Uninstall is not asked in this path —
// the receipt's own reversal does the work — so counting calls to it would pin
// an implementation detail and miss the bug that matters.
func TestRemoveNarrowsToTheHostItWasGiven(t *testing.T) {
	Convey("Given one skill delivered to four hosts", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		files := fourHosts(w)

		w.mustRun(t, "install", w.fixture(t), "-y")

		for _, path := range files {
			So(fileExists(path), ShouldBeTrue)
		}

		Convey("When it is removed from one of them", func() {
			stdout := w.mustRun(t, "remove", "local:caveman", "-y", "--hosts", "claude")

			Convey("Then the hosts left behind are named as a choice, not as a fault", func() {
				// "adapter cursor is not available" was the sentence for every
				// host the filter dropped, and it is a lie about a host the
				// user excluded on purpose: it turns their own flag into a
				// broken installation.
				So(stdout, ShouldContainSubstring, "cursor was excluded by --hosts")
				So(stdout, ShouldNotContainSubstring, "is not available")
			})

			Convey("Then that host's file is gone and the other three are untouched", func() {
				So(fileExists(files[host.Claude]), ShouldBeFalse)

				for _, id := range []host.ID{host.Cursor, host.OpenCode, host.Kilo} {
					So(fileExists(files[id]), ShouldBeTrue)
				}
			})

			Convey("Then only that host lost the cell", func() {
				stdout := w.mustRun(t, "status", "--json")

				So(stdout, ShouldContainSubstring, `"host":"cursor"`)
				So(stdout, ShouldContainSubstring, `"host":"opencode"`)
				So(stdout, ShouldContainSubstring, `"host":"kilo"`)
				So(stdout, ShouldNotContainSubstring, `"host":"claude"`)
			})
		})
	})
}

// The mirror of the above, because a filter that only ever narrows is a filter
// with a bug in the other direction: `--except` has to leave the named host
// alone and touch the rest.
func TestRemoveExceptNarrowsTheOtherWay(t *testing.T) {
	Convey("Given one skill delivered to four hosts", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		files := fourHosts(w)

		w.mustRun(t, "install", w.fixture(t), "-y")

		Convey("When it is removed from every host but one", func() {
			w.mustRun(t, "remove", "local:caveman", "-y", "--except", "cursor")

			Convey("Then the named host keeps its file and the rest lose theirs", func() {
				So(fileExists(files[host.Cursor]), ShouldBeTrue)

				for _, id := range []host.ID{host.Claude, host.OpenCode, host.Kilo} {
					So(fileExists(files[id]), ShouldBeFalse)
				}
			})
		})
	})
}

// fourHosts gives the world four independent adapters, each with its own target
// path, and returns those paths by host id. The world ships with one host; the
// whole subject of these two cases is what a command does when it meets more.
func fourHosts(w *world) map[host.ID]string {
	files := map[host.ID]string{}

	for _, id := range []host.ID{host.Claude, host.Cursor, host.OpenCode, host.Kilo} {
		fake := newFakeHost(id)
		path := filepath.Join(w.root, "host", string(id))

		fake.mu.Lock()
		fake.files = []fakeFile{{Path: path, Data: "# one\n"}}
		fake.mu.Unlock()

		w.hosts = append(w.hosts, fake)
		files[id] = path
	}

	return files
}
