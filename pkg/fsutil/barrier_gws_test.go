package fsutil

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// Which writer pays which barrier.
//
// This is the test the batch needed and did not have. The performance claim is
// "the cheap writer costs a fraction of the durable one", and before the counters
// below existed nothing observed either writer paying anything: switching the
// CAS path back to `if true` left every test in the repository green. A
// benchmark will happily measure both writers and say nothing about which one a
// caller reached.
//
// A barrier cannot be observed by crashing a process — fsync buys durability
// against power loss, and a killed process loses nothing from the page cache —
// so the counters are the seam. They are only ever non-zero inside a test.
func TestWhichWriterPaysWhichBarrier(t *testing.T) {
	Convey("Given counters on the two barriers", t, func() {
		realFile, realDir := fileSync, syncDir

		defer func() { fileSync, syncDir = realFile, realDir }()

		cases := []struct {
			name              string
			write             func(path string) error
			then              func()
			wantFile, wantDir int
		}{
			{
				name:     "the durable writer pays both barriers",
				write:    func(p string) error { return WriteFileAtomic(p, []byte("body\n"), 0o600) },
				wantFile: 1,
				wantDir:  1,
			},
			{
				name:  "the CAS writer pays neither",
				write: func(p string) error { return WriteFileAtomicCAS(p, []byte("body\n"), 0o600) },
			},
			{
				name:    "the batch flush pays one directory per directory",
				write:   func(string) error { return nil },
				then:    func() { So(SyncDirs("a", "b"), ShouldBeNil) },
				wantDir: 2,
			},
		}

		for _, c := range cases {
			Convey("Then "+c.name, func() {
				fileSyncs, dirSyncs := 0, 0

				fileSync = func(f *os.File) error { fileSyncs++; return f.Sync() }
				syncDir = func(string) error { dirSyncs++; return nil }

				So(c.write(filepath.Join(t.TempDir(), "object")), ShouldBeNil)

				if c.then != nil {
					c.then()
				}

				So(fileSyncs, ShouldEqual, c.wantFile)
				So(dirSyncs, ShouldEqual, c.wantDir)
			})
		}
	})
}

// A barrier-free write records its directory so the batch has one place to
// flush from. This is the other half of the trade: drop the per-file barrier and
// the batch barrier has to actually arrive, or the rename that made the file
// reachable is not durable either.
func TestABarrierFreeWriteRecordsItsDirectoryForTheBatch(t *testing.T) {
	Convey("Given a drained pending set", t, func() {
		// pendingDirs is process-global on purpose: the adapters that write
		// through it have no way to thread a collector down to the filesystem,
		// which is the same reason the batch barrier lives here and not in
		// every host. So a test starts by draining it — otherwise the counts
		// below include whatever an earlier test in this process left behind,
		// which is an assertion that passes for the wrong reason.
		So(SyncPendingDirs(), ShouldBeNil)

		realDir := syncDir

		defer func() { syncDir = realDir }()

		dir := t.TempDir()
		flushed := 0

		syncDir = func(string) error { flushed++; return nil }

		So(WriteFileAtomicCAS(filepath.Join(dir, "one"), []byte("a\n"), 0o600), ShouldBeNil)
		So(WriteFileAtomicCAS(filepath.Join(dir, "two"), []byte("b\n"), 0o600), ShouldBeNil)

		Convey("Then nothing was flushed per file", func() {
			So(flushed, ShouldEqual, 0)
		})

		Convey("Then one batch flush covers the directory", func() {
			// Two files in one directory is one directory sync, not two: that is
			// the whole point of collecting a set rather than a list.
			So(SyncPendingDirs(), ShouldBeNil)
			So(flushed, ShouldEqual, 1)

			Convey("And a second batch has nothing left to flush", func() {
				So(SyncPendingDirs(), ShouldBeNil)
				So(flushed, ShouldEqual, 1)
			})
		})

		Convey("Then a directory the run removed is not an error", func() {
			gone := filepath.Join(dir, "gone")

			So(os.MkdirAll(gone, 0o700), ShouldBeNil)
			So(WriteFileAtomicCAS(filepath.Join(gone, "three"), []byte("c\n"), 0o600), ShouldBeNil)
			So(os.RemoveAll(gone), ShouldBeNil)

			// A directory this run wrote into and then removed is still pending,
			// and there is nothing to flush about it: the absence is the
			// outcome. Syncing it anyway failed a whole run on ENOENT before
			// this was found, in pkg/host's own suite.
			So(SyncPendingDirs(), ShouldBeNil)
		})
	})
}

// The three call sites that joined the recoverable class with this batch: the
// host CLI records cache, the runtime heartbeat, and the `.git/info/exclude`
// entry. Each is a cache or a marker, so a lost write is the same answer as an
// absent file and the per-file barrier buys nothing.
//
// The risk of converting a site is not that it gets slower or faster. It is that
// it stops being synced AT ALL: a barrier-free write still has to reach the disk
// eventually, and that is the batch's directory barrier. So the assertion is
// that each one still RECORDS ITS DIRECTORY — a site that quietly stopped
// registering would leave a rename that no crash survives, and no counter in the
// repository would notice.
func TestTheSitesConvertedToTheRecoverableClassStillRegisterTheirDirectory(t *testing.T) {
	Convey("Given the directory barrier pending for a package's batch", t, func() {
		realDir := syncDir

		defer func() { syncDir = realDir }()

		cases := []struct {
			name  string
			write func(path string) error
		}{
			{
				name:  "the host CLI records cache",
				write: func(p string) error { return WriteFileAtomicCAS(p, []byte("[]\n"), 0o600) },
			},
			{
				name:  "the runtime heartbeat",
				write: func(p string) error { return WriteFileAtomicCAS(p, []byte("{}\n"), 0o600) },
			},
			{
				name:  "the .git/info/exclude entry",
				write: func(p string) error { return WriteFileAtomicCAS(p, []byte(".verger/\n"), 0o644) },
			},
		}

		for _, row := range cases {
			Convey("When "+row.name+" is written", func() {
				dir := filepath.Join(t.TempDir(), "state")
				target := filepath.Join(dir, "doc")
				So(os.MkdirAll(dir, 0o700), ShouldBeNil)

				pendingDirs.Clear()

				So(row.write(target), ShouldBeNil)

				Convey("Then the file is on disk", func() {
					info, err := os.Stat(target)
					So(err, ShouldBeNil)
					So(info.Size(), ShouldBeGreaterThan, 0)
				})

				Convey("And its directory is queued for the batch barrier", func() {
					_, queued := pendingDirs.Load(dir)
					So(queued, ShouldBeTrue)
				})

				Convey("And the batch barrier actually flushes it", func() {
					So(SyncPendingDirs(), ShouldBeNil)

					_, still := pendingDirs.Load(dir)
					So(still, ShouldBeFalse)
				})
			})
		}
	})
}
