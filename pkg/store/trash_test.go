package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/gofrs/flock"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
)

// fixedClock is a mutable injected clock for trash tests.
type fixedClock struct {
	at time.Time
}

func (c *fixedClock) now() time.Time { return c.at }

// newStore opens a store rooted in a fresh temp dir with the clock injected.
func newStore(t *testing.T, now func() time.Time, opts ...Option) *Store {
	t.Helper()

	st, err := Open(filepath.Join(t.TempDir(), "store"), append([]Option{WithClock(now)}, opts...)...)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	return st
}

// mkFile writes a test file, creating parents.
func mkFile(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// bucketNames lists the bucket directories inside a trash dir, sorted.
func bucketNames(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		t.Fatalf("read trash dir: %v", err)
	}

	var names []string

	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}

	return names
}

// readEntry parses one bucket's entry.json.
func readEntry(t *testing.T, dir, id string) Entry {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, id, entryFileName)) //nolint:gosec // G304: the test reads a path it created
	if err != nil {
		t.Fatalf("read entry.json: %v", err)
	}

	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("parse entry.json: %v", err)
	}

	return entry
}

// readTestFile reads a file the test itself created.
func readTestFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: the test reads a path it created
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return data
}

func assertNotFound(t *testing.T, err error) {
	t.Helper()

	target, ok := errors.AsType[*NotFoundError](err)
	if !ok {
		t.Fatalf("expected *NotFoundError, got %v", err)
	}

	if target.Path == "" {
		t.Fatal("NotFoundError.Path is empty")
	}
}

func TestTrashPutDirectory(t *testing.T) {
	Convey("Given a trash with an injected clock", t, func() {
		base := time.Date(2026, 9, 25, 12, 30, 45, 123456789, time.UTC)
		clock := &fixedClock{at: base}
		st := newStore(t, clock.now)
		tr := st.Trash()

		src := filepath.Join(t.TempDir(), "plugin")
		mkFile(t, filepath.Join(src, "nested", "file.txt"), "hello", 0o640)
		mkFile(t, filepath.Join(src, "run.sh"), "#!/bin/sh\n", 0o750)

		wantDigest := digest.Bytes([]byte("recorded-verbatim"))

		Convey("When a directory is trashed", func() {
			entry, err := tr.Put(context.Background(), src, PutOptions{
				Package: "acme/review-kit",
				Host:    "claude",
				Cause:   "user",
				Digest:  wantDigest,
			})

			Convey("Then the bucket, payload and entry.json land per spec", func() {
				So(err, ShouldBeNil)
				So(entry.Schema, ShouldEqual, 1)
				So(entry.ID, ShouldEqual, "20260925T123045.123456789Z")
				So(entry.Original, ShouldEqual, src)
				So(entry.Stored, ShouldEqual, "payload")
				So(entry.Kind, ShouldEqual, "dir")
				So(entry.Package, ShouldEqual, "acme/review-kit")
				So(entry.Host, ShouldEqual, "claude")
				So(entry.Cause, ShouldEqual, "user")
				So(entry.Digest, ShouldEqual, wantDigest)
				So(entry.RemovedAt, ShouldEqual, base)

				assertMissing(t, src)

				bucket := filepath.Join(st.TrashDir(), entry.ID)
				assertMode(t, bucket, 0o700)
				assertMode(t, filepath.Join(bucket, entryFileName), 0o600)

				payload := filepath.Join(bucket, "payload")
				assertMode(t, payload, 0o700)

				nested := filepath.Join(payload, "nested", "file.txt")
				So(string(readTestFile(t, nested)), ShouldEqual, "hello")
				assertMode(t, nested, 0o640)
				assertMode(t, filepath.Join(payload, "run.sh"), 0o750)

				So(json.Valid(readTestFile(t, filepath.Join(bucket, entryFileName))), ShouldBeTrue)

				onDisk := readEntry(t, st.TrashDir(), entry.ID)
				So(onDisk.ID, ShouldEqual, entry.ID)
				So(onDisk.Original, ShouldEqual, entry.Original)
				So(onDisk.Digest, ShouldEqual, wantDigest)
			})

			Convey("Then List and Get return the entry", func() {
				So(err, ShouldBeNil)

				list, listErr := tr.List()
				So(listErr, ShouldBeNil)
				So(list, ShouldResemble, []Entry{entry})

				got, getErr := tr.Get(entry.ID)
				So(getErr, ShouldBeNil)
				So(got, ShouldResemble, entry)
			})
		})
	})
}

func TestTrashPutFileAndSymlink(t *testing.T) {
	Convey("Given a trash", t, func() {
		clock := &fixedClock{at: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)}
		st := newStore(t, clock.now)
		tr := st.Trash()

		Convey("When a zero-byte file is trashed", func() {
			src := filepath.Join(t.TempDir(), "empty.txt")
			mkFile(t, src, "", 0o600)

			entry, err := tr.Put(context.Background(), src, PutOptions{})

			Convey("Then the kind is file and the payload exists byte-empty", func() {
				So(err, ShouldBeNil)
				So(entry.Kind, ShouldEqual, "file")

				So(string(readTestFile(t, filepath.Join(st.TrashDir(), entry.ID, "payload"))), ShouldEqual, "")
				assertMissing(t, src)
			})
		})

		Convey("When a symlink is trashed", func() {
			dir := t.TempDir()
			target := filepath.Join(dir, "target.txt")
			mkFile(t, target, "data", 0o600)

			link := filepath.Join(dir, "link.txt")
			So(os.Symlink(target, link), ShouldBeNil)

			entry, err := tr.Put(context.Background(), link, PutOptions{})

			Convey("Then it is recorded as a file and stays a symlink in the payload", func() {
				So(err, ShouldBeNil)
				So(entry.Kind, ShouldEqual, "file")

				payload := filepath.Join(st.TrashDir(), entry.ID, "payload")

				info, lstatErr := os.Lstat(payload)
				So(lstatErr, ShouldBeNil)
				So(info.Mode()&fs.ModeSymlink != 0, ShouldBeTrue)

				got, readErr := os.Readlink(payload)
				So(readErr, ShouldBeNil)
				So(got, ShouldEqual, target)

				assertMissing(t, link)
			})
		})
	})
}

func TestTrashPutMissingSource(t *testing.T) {
	Convey("Given a trash", t, func() {
		st := newStore(t, time.Now)
		tr := st.Trash()

		Convey("When a missing path is trashed", func() {
			_, err := tr.Put(context.Background(), filepath.Join(t.TempDir(), "nope"), PutOptions{})

			Convey("Then it fails with NotFoundError and creates no bucket", func() {
				So(err, ShouldBeError)
				assertNotFound(t, err)
				So(bucketNames(t, st.TrashDir()), ShouldBeEmpty)
			})
		})

		Convey("When a canceled context is passed", func() {
			src := filepath.Join(t.TempDir(), "file.txt")
			mkFile(t, src, "x", 0o600)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			_, err := tr.Put(ctx, src, PutOptions{})

			Convey("Then the source is untouched", func() {
				So(err, ShouldBeError)
				So(errors.Is(err, context.Canceled), ShouldBeTrue)
				So(fsutil.Exists(src), ShouldBeTrue)
			})
		})
	})
}

func TestTrashBucketCollisionSameClock(t *testing.T) {
	Convey("Given a frozen clock", t, func() {
		clock := &fixedClock{at: time.Date(2026, 9, 25, 12, 30, 45, 123456789, time.UTC)}
		st := newStore(t, clock.now)
		tr := st.Trash()

		dir := t.TempDir()
		first := filepath.Join(dir, "a.txt")
		second := filepath.Join(dir, "b.txt")

		mkFile(t, first, "a", 0o600)
		mkFile(t, second, "b", 0o600)

		Convey("When two paths are trashed in the same instant", func() {
			e1, err1 := tr.Put(context.Background(), first, PutOptions{})
			e2, err2 := tr.Put(context.Background(), second, PutOptions{})

			Convey("Then buckets are distinct and both listed", func() {
				So(err1, ShouldBeNil)
				So(err2, ShouldBeNil)
				So(e1.ID, ShouldEqual, "20260925T123045.123456789Z")
				So(e2.ID, ShouldEqual, "20260925T123045.123456789Z-2")

				names := bucketNames(t, st.TrashDir())
				So(names, ShouldResemble, []string{e1.ID, e2.ID})
			})
		})

		Convey("When the same source is trashed twice", func() {
			_, err1 := tr.Put(context.Background(), first, PutOptions{})
			_, err2 := tr.Put(context.Background(), first, PutOptions{})

			Convey("Then the second call is a NotFound", func() {
				So(err1, ShouldBeNil)
				assertNotFound(t, err2)
			})
		})
	})
}

func TestTrashPutFailureRollsBack(t *testing.T) {
	Convey("Given a trash", t, func() {
		st := newStore(t, time.Now)
		tr := st.Trash()

		Convey("When the move into the bucket fails", func() {
			// The seam is the whole move, not the rename inside it: the
			// cross-device fallback moved to fsutil when MoveTree learned to
			// take a file source, and it is tested there
			// (TestMoveTreeFileSourceCrossDevice).
			tr.moveTree = func(context.Context, string, string) error { return errors.New("boom") }

			src := filepath.Join(t.TempDir(), "file.txt")
			mkFile(t, src, "keep me", 0o600)

			_, err := tr.Put(context.Background(), src, PutOptions{})

			Convey("Then the source survives and no bucket remains", func() {
				So(err, ShouldBeError)
				So(fsutil.Exists(src), ShouldBeTrue)
				So(bucketNames(t, st.TrashDir()), ShouldBeEmpty)

				So(string(readTestFile(t, src)), ShouldEqual, "keep me")
			})
		})

		Convey("When writing entry.json fails", func() {
			tr.writeEntryFile = func(string, []byte, fs.FileMode) error { return errors.New("disk full") }

			src := filepath.Join(t.TempDir(), "file.txt")
			mkFile(t, src, "precious", 0o600)

			_, err := tr.Put(context.Background(), src, PutOptions{})

			Convey("Then the payload moves back and the bucket is removed", func() {
				So(err, ShouldBeError)
				So(fsutil.Exists(src), ShouldBeTrue)
				So(bucketNames(t, st.TrashDir()), ShouldBeEmpty)

				So(string(readTestFile(t, src)), ShouldEqual, "precious")
			})
		})
	})
}

// TestTrashFileRoundTrip pins what the trash promises for a plain file: it
// leaves the source, keeps the payload in the bucket, and puts the bytes back
// where they were.
//
// The cross-device path itself is no longer simulated here. It used to be,
// through a `renameEntry` seam that fed a hand-written EXDEV fallback living
// in this package. That fallback moved to fsutil when MoveTree learned to take
// a file source, and it is tested there — TestMoveTreeFileSourceCrossDevice
// and TestMoveTreeCrossDevice. Simulating it from here would have re-pinned a
// mechanism this package no longer owns.
func TestTrashFileRoundTrip(t *testing.T) {
	Convey("Given a store", t, func() {
		clock := &fixedClock{at: time.Date(2026, 9, 25, 1, 2, 3, 4, time.UTC)}
		st := newStore(t, clock.now)
		tr := st.Trash()

		Convey("When a regular file is trashed and restored", func() {
			dir := t.TempDir()
			src := filepath.Join(dir, "file.bin")
			mkFile(t, src, "round-trip", 0o640)

			entry, err := tr.Put(context.Background(), src, PutOptions{})

			Convey("Then the payload sits in the bucket and the bytes come back", func() {
				So(err, ShouldBeNil)
				assertMissing(t, src)

				payload := filepath.Join(st.TrashDir(), entry.ID, "payload")

				So(string(readTestFile(t, payload)), ShouldEqual, "round-trip")
				So(payload, ShouldNotEqual, src)

				restored, restoreErr := tr.Restore(context.Background(), entry.ID)
				So(restoreErr, ShouldBeNil)
				So(restored.ID, ShouldEqual, entry.ID)

				So(string(readTestFile(t, src)), ShouldEqual, "round-trip")
				So(bucketNames(t, st.TrashDir()), ShouldBeEmpty)
			})
		})

		Convey("When a symlink is trashed", func() {
			dir := t.TempDir()
			target := filepath.Join(dir, "target.txt")
			mkFile(t, target, "data", 0o600)

			link := filepath.Join(dir, "link")
			So(os.Symlink(target, link), ShouldBeNil)

			entry, err := tr.Put(context.Background(), link, PutOptions{})

			Convey("Then the payload is a symlink with the same target", func() {
				So(err, ShouldBeNil)

				payload := filepath.Join(st.TrashDir(), entry.ID, "payload")

				got, readErr := os.Readlink(payload)
				So(readErr, ShouldBeNil)
				So(got, ShouldEqual, target)
			})
		})
	})
}

func TestTrashMoveDirUsesMoveTree(t *testing.T) {
	Convey("Given a spy on the tree mover", t, func() {
		type call struct {
			from string
			to   string
		}

		var (
			mu    sync.Mutex
			calls []call
		)

		st := newStore(t, time.Now)
		tr := st.Trash()

		realMove := fsutil.MoveTree
		tr.moveTree = func(_ context.Context, from, to string) error {
			mu.Lock()

			calls = append(calls, call{from: from, to: to})

			mu.Unlock()

			return realMove(context.Background(), from, to)
		}

		src := filepath.Join(t.TempDir(), "tree")
		mkFile(t, filepath.Join(src, "a.txt"), "a", 0o600)

		entry, err := tr.Put(context.Background(), src, PutOptions{})

		Convey("Then the directory payload goes through fsutil.MoveTree", func() {
			So(err, ShouldBeNil)

			mu.Lock()
			defer mu.Unlock()

			So(calls, ShouldHaveLength, 1)
			So(calls[0].from, ShouldEqual, src)
			So(calls[0].to, ShouldEqual, filepath.Join(st.TrashDir(), entry.ID, "payload"))
		})
	})
}

func TestTrashListSorting(t *testing.T) {
	Convey("Given trashed entries at different times", t, func() {
		base := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
		clock := &fixedClock{at: base}
		st := newStore(t, clock.now)
		tr := st.Trash()

		mk := func(at time.Time, name string) Entry {
			clock.at = at
			src := filepath.Join(t.TempDir(), name)
			mkFile(t, src, name, 0o600)

			entry, err := tr.Put(context.Background(), src, PutOptions{})
			So(err, ShouldBeNil)

			return entry
		}

		late := mk(base.Add(time.Hour), "late")
		early := mk(base.Add(-time.Hour), "early")
		sameA := mk(base, "same-a")
		sameB := mk(base, "same-b")

		Convey("When the list is read", func() {
			list, err := tr.List()

			Convey("Then entries sort by RemovedAt and then ID", func() {
				So(err, ShouldBeNil)
				So(list, ShouldHaveLength, 4)

				ids := []string{list[0].ID, list[1].ID, list[2].ID, list[3].ID}
				So(ids, ShouldResemble, []string{early.ID, sameA.ID, sameB.ID, late.ID})
			})
		})
	})
}

func TestTrashListSkipsOrphansAndNonDirs(t *testing.T) {
	Convey("Given a trash dir with junk", t, func() {
		st := newStore(t, time.Now)
		tr := st.Trash()
		So(st.Ensure(), ShouldBeNil)

		Convey("When an orphan bucket and a stray file exist", func() {
			So(os.Mkdir(filepath.Join(st.TrashDir(), "orphan-bucket"), 0o700), ShouldBeNil)
			mkFile(t, filepath.Join(st.TrashDir(), "stray.txt"), "junk", 0o600)

			list, err := tr.List()

			Convey("Then neither is listed", func() {
				So(err, ShouldBeNil)
				So(list, ShouldBeEmpty)
			})
		})
	})
}

func TestTrashCorruptAndSchema(t *testing.T) {
	Convey("Given a trash", t, func() {
		st := newStore(t, time.Now)
		tr := st.Trash()
		So(st.Ensure(), ShouldBeNil)

		writeBucket := func(id, content string) {
			bucket := filepath.Join(st.TrashDir(), id)
			So(os.Mkdir(bucket, 0o700), ShouldBeNil)
			mkFile(t, filepath.Join(bucket, entryFileName), content, 0o600)
		}

		Convey("When entry.json is not valid JSON", func() {
			writeBucket("broken", "{not json")

			_, listErr := tr.List()
			_, getErr := tr.Get("broken")

			Convey("Then both report CorruptTrashError", func() {
				_, listOK := errors.AsType[*CorruptTrashError](listErr)
				So(listOK, ShouldBeTrue)

				_, getOK := errors.AsType[*CorruptTrashError](getErr)
				So(getOK, ShouldBeTrue)
			})
		})

		Convey("When entry.json has a newer schema", func() {
			writeBucket("newer", `{"schema":2,"id":"newer","original":"/tmp/x","stored":"payload","kind":"file"}`)

			_, getErr := tr.Get("newer")

			Convey("Then it reports SchemaNewerError", func() {
				target, ok := errors.AsType[*SchemaNewerError](getErr)
				So(ok, ShouldBeTrue)
				So(target.Found, ShouldEqual, 2)
				So(target.Supported, ShouldEqual, 1)
			})
		})

		Convey("When entry.json has schema zero", func() {
			writeBucket("zero", `{"schema":0,"id":"zero","original":"/tmp/x","stored":"payload","kind":"file"}`)

			_, getErr := tr.Get("zero")

			Convey("Then it reports CorruptTrashError", func() {
				_, ok := errors.AsType[*CorruptTrashError](getErr)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When the entry points at a relative original", func() {
			writeBucket("relative", `{"schema":1,"id":"relative","original":"relative/path","stored":"payload","kind":"file"}`)

			_, getErr := tr.Get("relative")

			Convey("Then it reports CorruptTrashError", func() {
				_, ok := errors.AsType[*CorruptTrashError](getErr)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When the entry id does not match its bucket", func() {
			writeBucket("mismatch", `{"schema":1,"id":"other","original":"/tmp/x","stored":"payload","kind":"file"}`)

			_, getErr := tr.Get("mismatch")

			Convey("Then it reports CorruptTrashError", func() {
				_, ok := errors.AsType[*CorruptTrashError](getErr)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When the stored name escapes the bucket", func() {
			writeBucket("escape", `{"schema":1,"id":"escape","original":"/tmp/x","stored":"../payload","kind":"file"}`)

			_, getErr := tr.Get("escape")

			Convey("Then it reports CorruptTrashError", func() {
				_, ok := errors.AsType[*CorruptTrashError](getErr)
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestTrashGetAndRemoveInvalidID(t *testing.T) {
	Convey("Given a trash", t, func() {
		st := newStore(t, time.Now)
		tr := st.Trash()

		Convey("When an unsafe id is requested", func() {
			_, getErr := tr.Get("../escape")
			removeErr := tr.Remove("../escape")

			Convey("Then both report InvalidIDError", func() {
				_, getOK := errors.AsType[*InvalidIDError](getErr)
				So(getOK, ShouldBeTrue)

				_, removeOK := errors.AsType[*InvalidIDError](removeErr)
				So(removeOK, ShouldBeTrue)
			})
		})

		Convey("When a missing bucket is requested", func() {
			_, getErr := tr.Get("20260925T000000.000000000Z")

			Convey("Then it reports NotFoundError", func() {
				assertNotFound(t, getErr)
				assertNotFound(t, tr.Remove("20260925T000000.000000000Z"))
			})
		})
	})
}

func TestTrashRestore(t *testing.T) {
	Convey("Given a trashed tree", t, func() {
		clock := &fixedClock{at: time.Date(2026, 9, 25, 3, 0, 0, 0, time.UTC)}
		st := newStore(t, clock.now)
		tr := st.Trash()

		src := filepath.Join(t.TempDir(), "payload-dir")
		mkFile(t, filepath.Join(src, "a", "b.txt"), "nested", 0o644)

		before, err := digest.Tree(src)
		So(err, ShouldBeNil)

		entry, err := tr.Put(context.Background(), src, PutOptions{Package: "acme/x", Host: "claude"})
		So(err, ShouldBeNil)

		Convey("When the entry is restored", func() {
			restored, restoreErr := tr.Restore(context.Background(), entry.ID)

			Convey("Then the tree comes back byte-identical and the bucket is gone", func() {
				So(restoreErr, ShouldBeNil)
				So(restored.ID, ShouldEqual, entry.ID)

				after, digestErr := digest.Tree(src)
				So(digestErr, ShouldBeNil)
				So(after, ShouldEqual, before)

				So(bucketNames(t, st.TrashDir()), ShouldBeEmpty)
			})
		})

		Convey("When the original path exists again", func() {
			mkFile(t, src, "conflict", 0o600)

			_, restoreErr := tr.Restore(context.Background(), entry.ID)

			Convey("Then it reports RestoreConflictError and keeps the bucket", func() {
				target, ok := errors.AsType[*RestoreConflictError](restoreErr)
				So(ok, ShouldBeTrue)
				So(target.Path, ShouldEqual, src)
				So(bucketNames(t, st.TrashDir()), ShouldResemble, []string{entry.ID})
			})
		})

		Convey("When the payload is missing", func() {
			So(os.RemoveAll(filepath.Join(st.TrashDir(), entry.ID, "payload")), ShouldBeNil)

			_, restoreErr := tr.Restore(context.Background(), entry.ID)

			Convey("Then it reports CorruptTrashError", func() {
				_, ok := errors.AsType[*CorruptTrashError](restoreErr)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When the original parents were removed", func() {
			deep := filepath.Join(t.TempDir(), "one", "two", "file.txt")
			mkFile(t, deep, "deep", 0o600)

			deepEntry, deepErr := tr.Put(context.Background(), deep, PutOptions{})
			So(deepErr, ShouldBeNil)
			So(os.RemoveAll(filepath.Dir(filepath.Dir(deep))), ShouldBeNil)

			_, deepRestoreErr := tr.Restore(context.Background(), deepEntry.ID)

			Convey("Then missing parents are recreated 0700", func() {
				So(deepRestoreErr, ShouldBeNil)

				So(string(readTestFile(t, deep)), ShouldEqual, "deep")
				assertMode(t, filepath.Dir(deep), 0o700)
			})
		})
	})
}

func TestTrashRemove(t *testing.T) {
	Convey("Given a trashed file", t, func() {
		st := newStore(t, time.Now)
		tr := st.Trash()

		src := filepath.Join(t.TempDir(), "file.txt")
		mkFile(t, src, "gone", 0o600)

		entry, err := tr.Put(context.Background(), src, PutOptions{})
		So(err, ShouldBeNil)

		Convey("When the entry is removed", func() {
			removeErr := tr.Remove(entry.ID)

			Convey("Then the bucket is gone and a second remove is NotFound", func() {
				So(removeErr, ShouldBeNil)
				So(bucketNames(t, st.TrashDir()), ShouldBeEmpty)
				assertNotFound(t, tr.Remove(entry.ID))
			})
		})
	})
}

func TestTrashPurge(t *testing.T) {
	Convey("Given a trash with a frozen clock", t, func() {
		base := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
		clock := &fixedClock{at: base}
		st := newStore(t, clock.now)
		tr := st.Trash()

		putAt := func(at time.Time, name string) Entry {
			clock.at = at
			src := filepath.Join(t.TempDir(), name)
			mkFile(t, src, name, 0o600)

			entry, err := tr.Put(context.Background(), src, PutOptions{})
			So(err, ShouldBeNil)

			return entry
		}

		old := putAt(base.Add(-40*24*time.Hour), "old")
		fresh := putAt(base.Add(-1*time.Hour), "fresh")
		boundary := putAt(base.Add(-30*24*time.Hour), "boundary")

		Convey("When Purge runs with the default retention", func() {
			clock.at = base
			count, err := tr.Purge()

			Convey("Then only entries strictly older than 30 days are removed", func() {
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 1)

				names := bucketNames(t, st.TrashDir())
				So(names, ShouldResemble, []string{boundary.ID, fresh.ID})
				So(fsutil.Exists(filepath.Join(st.TrashDir(), old.ID)), ShouldBeFalse)
			})
		})

		Convey("When PurgeBefore cuts one nanosecond before the boundary", func() {
			count, err := tr.PurgeBefore(base.Add(-30*24*time.Hour - time.Nanosecond))

			Convey("Then the boundary entry survives", func() {
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 1)

				names := bucketNames(t, st.TrashDir())
				So(names, ShouldResemble, []string{boundary.ID, fresh.ID})
			})
		})

		Convey("When PurgeBefore cuts one nanosecond after the boundary", func() {
			count, err := tr.PurgeBefore(base.Add(-30*24*time.Hour + time.Nanosecond))

			Convey("Then the boundary entry is removed too", func() {
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 2)

				names := bucketNames(t, st.TrashDir())
				So(names, ShouldResemble, []string{fresh.ID})
			})
		})
	})
}

func TestTrashPurgeJunkAndCustomRetention(t *testing.T) {
	Convey("Given junk and newer-schema buckets", t, func() {
		base := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
		clock := &fixedClock{at: base}
		st := newStore(t, clock.now)
		tr := st.Trash()
		So(st.Ensure(), ShouldBeNil)

		orphan := filepath.Join(st.TrashDir(), "orphan")
		So(os.Mkdir(orphan, 0o700), ShouldBeNil)

		newerBucket := filepath.Join(st.TrashDir(), "newer")
		So(os.Mkdir(newerBucket, 0o700), ShouldBeNil)
		mkFile(t, filepath.Join(newerBucket, entryFileName),
			`{"schema":2,"id":"newer","original":"/tmp/x","stored":"payload","kind":"file"}`, 0o600)

		corruptBucket := filepath.Join(st.TrashDir(), "corrupt")
		So(os.Mkdir(corruptBucket, 0o700), ShouldBeNil)
		mkFile(t, filepath.Join(corruptBucket, entryFileName), "{broken", 0o600)

		Convey("When PurgeBefore runs", func() {
			count, err := tr.PurgeBefore(base.Add(24 * time.Hour))

			Convey("Then orphans go and unreadable/newer entries stay", func() {
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 1)

				assertMissing(t, orphan)
				So(fsutil.Exists(newerBucket), ShouldBeTrue)
				So(fsutil.Exists(corruptBucket), ShouldBeTrue)
			})
		})
	})

	Convey("Given a short custom retention", t, func() {
		base := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
		clock := &fixedClock{at: base}
		st := newStore(t, clock.now, WithTrashRetention(time.Hour))
		tr := st.Trash()

		putAt := func(at time.Time, name string) Entry {
			clock.at = at
			src := filepath.Join(t.TempDir(), name)
			mkFile(t, src, name, 0o600)

			entry, err := tr.Put(context.Background(), src, PutOptions{})
			So(err, ShouldBeNil)

			return entry
		}

		putAt(base.Add(-2*time.Hour), "old")
		fresh := putAt(base.Add(-30*time.Minute), "fresh")

		Convey("When Purge runs", func() {
			clock.at = base
			count, err := tr.Purge()

			Convey("Then the custom retention applies", func() {
				So(err, ShouldBeNil)
				So(count, ShouldEqual, 1)
				So(bucketNames(t, st.TrashDir()), ShouldResemble, []string{fresh.ID})
			})
		})
	})
}

// TestTrashListNamesBothErrorClasses closes N005. The doc used to name only
// *CorruptTrashError, which left a reader assuming that was the only way List
// could fail. It is not, and the difference is not cosmetic: a corrupt bucket
// is this build's problem, a newer schema is not, and treating the second like
// the first — skipping it — would let an older verger hide entries it cannot
// read, so a remove would report the package as gone while its bytes sat in
// the trash.
//
// The two cases are separate on purpose. List walks the buckets in name
// order, so a trash holding both would report whichever sorts first and the
// assertion would be about the accident of the names rather than the class.
func TestTrashListNamesBothErrorClasses(t *testing.T) {
	Convey("Given a bucket written by a newer verger", t, func() {
		clock := &fixedClock{at: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
		st := newStore(t, clock.now)
		tr := st.Trash()
		So(st.Ensure(), ShouldBeNil)

		newer := filepath.Join(st.TrashDir(), "newer")
		So(os.Mkdir(newer, 0o700), ShouldBeNil)
		mkFile(t, filepath.Join(newer, entryFileName),
			`{"schema":2,"id":"newer","original":"/tmp/x","stored":"payload","kind":"file"}`, 0o600)

		Convey("When List runs", func() {
			_, err := tr.List()

			Convey("Then it refuses, naming the newer schema", func() {
				So(err, ShouldNotBeNil)

				newerErr := &SchemaNewerError{}
				So(errors.As(err, &newerErr), ShouldBeTrue)
				So(newerErr.Supported, ShouldEqual, entrySchema)
			})
		})
	})

	Convey("Given a corrupt bucket only", t, func() {
		clock := &fixedClock{at: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
		st := newStore(t, clock.now)
		tr := st.Trash()
		So(st.Ensure(), ShouldBeNil)

		corrupt := filepath.Join(st.TrashDir(), "corrupt")
		So(os.Mkdir(corrupt, 0o700), ShouldBeNil)
		mkFile(t, filepath.Join(corrupt, entryFileName), "{broken", 0o600)

		Convey("When List runs", func() {
			_, err := tr.List()

			Convey("Then it refuses with the corrupt class, not the schema one", func() {
				So(err, ShouldNotBeNil)

				corruptErr := &CorruptTrashError{}
				So(errors.As(err, &corruptErr), ShouldBeTrue)

				newerErr := &SchemaNewerError{}
				So(errors.As(err, &newerErr), ShouldBeFalse)
			})
		})
	})

	Convey("Given only an orphan and a readable entry", t, func() {
		clock := &fixedClock{at: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
		st := newStore(t, clock.now)
		tr := st.Trash()
		So(st.Ensure(), ShouldBeNil)

		src := filepath.Join(t.TempDir(), "kept.txt")
		mkFile(t, src, "kept", 0o600)

		kept, err := tr.Put(context.Background(), src, PutOptions{})
		So(err, ShouldBeNil)

		So(os.Mkdir(filepath.Join(st.TrashDir(), "orphan"), 0o700), ShouldBeNil)

		Convey("When List runs", func() {
			entries, err := tr.List()

			Convey("Then the orphan is skipped and the real entry is listed", func() {
				So(err, ShouldBeNil)
				So(entries, ShouldHaveLength, 1)
				So(entries[0].ID, ShouldEqual, kept.ID)
			})
		})
	})
}

func TestTrashUnicodeAndSpaces(t *testing.T) {
	Convey("Given paths with unicode and spaces", t, func() {
		clock := &fixedClock{at: time.Date(2026, 9, 25, 5, 6, 7, 8, time.UTC)}
		st := newStore(t, clock.now)
		tr := st.Trash()

		dir := filepath.Join(t.TempDir(), "папка с пробелом")
		file := filepath.Join(dir, "файл 2.txt")
		mkFile(t, file, "юникод", 0o600)

		before, err := digest.Tree(dir)
		So(err, ShouldBeNil)

		Convey("When the tree is trashed and listed", func() {
			entry, putErr := tr.Put(context.Background(), dir, PutOptions{})
			So(putErr, ShouldBeNil)

			list, listErr := tr.List()

			Convey("Then entry.json stays valid JSON and restore is byte-identical", func() {
				So(listErr, ShouldBeNil)
				So(list, ShouldHaveLength, 1)

				So(json.Valid(readTestFile(t, filepath.Join(st.TrashDir(), entry.ID, entryFileName))), ShouldBeTrue)

				_, restoreErr := tr.Restore(context.Background(), entry.ID)
				So(restoreErr, ShouldBeNil)

				after, digestErr := digest.Tree(dir)
				So(digestErr, ShouldBeNil)
				So(after, ShouldEqual, before)
			})
		})
	})
}

func TestTrashConcurrentPuts(t *testing.T) {
	Convey("Given a frozen clock", t, func() {
		clock := &fixedClock{at: time.Date(2026, 9, 25, 7, 8, 9, 10, time.UTC)}
		st := newStore(t, clock.now)
		tr := st.Trash()

		Convey("When eight distinct sources are trashed concurrently", func() {
			const n = 8

			dir := t.TempDir()

			var (
				wg   sync.WaitGroup
				mu   sync.Mutex
				errs []error
				ids  = map[string]struct{}{}
			)

			for i := range n {
				src := filepath.Join(dir, "file", string(rune('a'+i))+".txt")
				mkFile(t, src, "concurrent", 0o600)

				wg.Go(func() {
					entry, err := tr.Put(context.Background(), src, PutOptions{})

					mu.Lock()
					defer mu.Unlock()

					if err != nil {
						errs = append(errs, err)

						return
					}

					ids[entry.ID] = struct{}{}
				})
			}

			wg.Wait()

			Convey("Then every put got its own bucket", func() {
				So(errs, ShouldBeEmpty)
				So(ids, ShouldHaveLength, n)
				So(bucketNames(t, st.TrashDir()), ShouldHaveLength, n)

				list, err := tr.List()
				So(err, ShouldBeNil)
				So(list, ShouldHaveLength, n)
			})
		})
	})
}

// ---- mutator serialization and restore distinction (fix p2) ------------------

// trashHelperEnv marks the re-executed test binary as the helper process; the
// payload (store root, source, mode, signal path) arrives as JSON on stdin so no
// environment value ever flows into a filesystem path.
const trashHelperEnv = "VERGER_STORE_TRASH_HELPER"

// trashHelperConfig is the helper handshake payload.
type trashHelperConfig struct {
	Root   string `json:"root"`
	Src    string `json:"src"`
	Mode   string `json:"mode"`
	Signal string `json:"signal"`
}

// readHelperConfig decodes and validates the stdin handshake.
func readHelperConfig() (trashHelperConfig, error) {
	cfg := trashHelperConfig{}
	if err := json.NewDecoder(os.Stdin).Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("decode helper config: %w", err)
	}

	if !filepath.IsAbs(cfg.Root) {
		return cfg, errors.New("helper store root must be absolute")
	}

	return cfg, nil
}

// TestTrashHelperProcess is the child side of the two-process trash tests: it
// puts one payload and, depending on the mode, holds the critical section.
func TestTrashHelperProcess(t *testing.T) {
	if os.Getenv(trashHelperEnv) != "1" {
		return
	}

	cfg, err := readHelperConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper config:", err)

		os.Exit(2)
	}

	st, err := Open(cfg.Root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper open:", err)

		os.Exit(2)
	}

	switch cfg.Mode {
	case "slow-entry":
		// The put is held in flight by the entry write, not by a forced EXDEV
		// copy: the move completes first, which is exactly the window this
		// helper needs — the payload exists while put-done has not been
		// printed. The cross-device path is fsutil's to test now.
		tr := st.Trash()
		tr.writeEntryFile = func(path string, data []byte, mode fs.FileMode) error {
			fmt.Println("payload-ready")

			for range 1500 {
				if _, statErr := os.Stat(cfg.Signal); statErr == nil {
					break
				}

				time.Sleep(10 * time.Millisecond)
			}

			return fsutil.WriteFileAtomic(path, data, mode)
		}

		if _, err := st.Trash().Put(context.Background(), cfg.Src, PutOptions{}); err != nil {
			fmt.Fprintln(os.Stderr, "helper put:", err)

			os.Exit(3)
		}

		fmt.Println("put-done")
	case "hold":
		hold := st.Trash()
		hold.writeEntryFile = func(string, []byte, fs.FileMode) error {
			fmt.Println("payload-ready")

			select {}
		}

		_, _ = hold.Put(context.Background(), cfg.Src, PutOptions{})

		os.Exit(4)
	default:
		fmt.Fprintln(os.Stderr, "helper: unknown mode", cfg.Mode)

		os.Exit(5)
	}

	os.Exit(0)
}

// trashHelper is one running helper process.
type trashHelper struct {
	cmd   *exec.Cmd
	lines *bufio.Reader
}

// startTrashHelper re-runs the test binary as a trash helper and hands it the
// configuration over stdin.
func startTrashHelper(t *testing.T, root, src, mode, signal string) *trashHelper {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=TestTrashHelperProcess") //nolint:gosec // G204: the test binary re-runs itself

	cmd.Env = append(os.Environ(), trashHelperEnv+"=1")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("helper stdin: %v", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("helper start: %v", err)
	}

	payload, err := json.Marshal(trashHelperConfig{Root: root, Src: src, Mode: mode, Signal: signal})
	if err != nil {
		t.Fatalf("helper config: %v", err)
	}

	if _, err := stdin.Write(payload); err != nil {
		t.Fatalf("helper config write: %v", err)
	}

	if err := stdin.Close(); err != nil {
		t.Fatalf("helper stdin close: %v", err)
	}

	return &trashHelper{cmd: cmd, lines: bufio.NewReader(stdout)}
}

// expectLine waits for one exact stdout line from the helper.
func (h *trashHelper) expectLine(t *testing.T, want string) {
	t.Helper()

	done := make(chan string, 1)

	go func() {
		line, err := h.lines.ReadString('\n')
		if err != nil {
			done <- ""

			return
		}

		done <- strings.TrimSpace(line)
	}()

	select {
	case line := <-done:
		if line != want {
			_ = h.cmd.Process.Kill()
			_ = h.cmd.Wait()

			t.Fatalf("helper said %q, want %q", line, want)
		}
	case <-time.After(20 * time.Second):
		_ = h.cmd.Process.Kill()
		_ = h.cmd.Wait()

		t.Fatalf("helper did not say %q in time", want)
	}
}

// wait blocks until the helper exits.
func (h *trashHelper) wait() error {
	return h.cmd.Wait()
}

// kill terminates the helper without a clean exit.
func (h *trashHelper) kill(t *testing.T) {
	t.Helper()

	if err := h.cmd.Process.Kill(); err != nil {
		t.Fatalf("helper kill: %v", err)
	}

	if err := h.cmd.Wait(); err == nil {
		t.Fatal("killed helper reported a clean exit")
	}
}

// writeLargeFile writes size deterministic bytes.
func writeLargeFile(t *testing.T, path string, size int) {
	t.Helper()

	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write large file: %v", err)
	}
}

func TestTrashRestoreOrphanVsMissingBucket(t *testing.T) {
	Convey("Given a trash with a missing bucket and an orphan bucket", t, func() {
		st := newStore(t, time.Now)
		tr := st.Trash()
		So(st.Ensure(), ShouldBeNil)

		orphan := filepath.Join(st.TrashDir(), "20260101T000000.000000000Z")
		mkFile(t, filepath.Join(orphan, "payload"), "orphan", 0o600)

		Convey("When a bucket without entry.json is restored", func() {
			_, err := tr.Restore(context.Background(), filepath.Base(orphan))

			Convey("Then it reports CorruptTrashError, not NotFoundError", func() {
				target, ok := errors.AsType[*CorruptTrashError](err)
				So(ok, ShouldBeTrue)
				So(target.ID, ShouldEqual, filepath.Base(orphan))
			})
		})

		Convey("When a truly missing bucket is restored", func() {
			_, err := tr.Restore(context.Background(), "20250101T000000.000000000Z")

			Convey("Then it reports NotFoundError", func() {
				assertNotFound(t, err)
			})
		})
	})
}

func TestTrashPutPurgeRaceSameProcess(t *testing.T) {
	Convey("Given a Put paused between the payload and entry.json", t, func() {
		clock := &fixedClock{at: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
		st := newStore(t, clock.now)
		tr := st.Trash()

		src := filepath.Join(t.TempDir(), "precious.txt")
		mkFile(t, src, "precious", 0o600)

		blocked := make(chan struct{})
		release := make(chan struct{})

		tr.writeEntryFile = func(path string, data []byte, mode fs.FileMode) error {
			close(blocked)
			<-release

			return fsutil.WriteFileAtomic(path, data, mode)
		}

		errCh := make(chan error, 1)

		go func() {
			_, err := tr.Put(context.Background(), src, PutOptions{})
			errCh <- err
		}()

		<-blocked

		Convey("When PurgeBefore runs concurrently", func() {
			count, purgeErr := tr.PurgeBefore(clock.at.Add(-time.Hour))

			Convey("Then the sweep defers and the put completes without data loss", func() {
				So(purgeErr, ShouldBeNil)
				So(count, ShouldEqual, 0)

				close(release)
				So(<-errCh, ShouldBeNil)
				assertMissing(t, src)

				list, listErr := tr.List()
				So(listErr, ShouldBeNil)
				So(list, ShouldHaveLength, 1)
				So(string(readTestFile(t, filepath.Join(st.TrashDir(), list[0].ID, "payload"))), ShouldEqual, "precious")
			})
		})
	})
}

func TestTrashTwoProcessPutPurgeRace(t *testing.T) {
	Convey("Given a helper putting a large payload across a forced EXDEV copy", t, func() {
		root := filepath.Join(t.TempDir(), "store")
		src := filepath.Join(t.TempDir(), "large.bin")
		writeLargeFile(t, src, 32<<20)

		before, err := digest.File(src)
		So(err, ShouldBeNil)

		signal := filepath.Join(t.TempDir(), "go")

		helper := startTrashHelper(t, root, src, "slow-entry", signal)
		helper.expectLine(t, "payload-ready")

		Convey("When another process sweeps the trash while the put is in flight", func() {
			st, openErr := Open(root)
			So(openErr, ShouldBeNil)

			count, purgeErr := st.Trash().PurgeBefore(time.Now().Add(-time.Hour))

			Convey("Then the sweep defers, the put completes and the payload survives", func() {
				So(purgeErr, ShouldBeNil)
				So(count, ShouldEqual, 0)

				So(os.WriteFile(signal, nil, 0o600), ShouldBeNil)
				So(helper.wait(), ShouldBeNil)

				list, listErr := st.Trash().List()
				So(listErr, ShouldBeNil)
				So(list, ShouldHaveLength, 1)

				payload := filepath.Join(st.TrashDir(), list[0].ID, "payload")

				after, digestErr := digest.File(payload)
				So(digestErr, ShouldBeNil)
				So(after, ShouldEqual, before)
			})
		})
	})
}

func TestTrashCrashOrphanSweep(t *testing.T) {
	Convey("Given a helper killed between the payload and entry.json", t, func() {
		root := filepath.Join(t.TempDir(), "store")
		src := filepath.Join(t.TempDir(), "payload.txt")
		mkFile(t, src, "crash payload", 0o600)

		helper := startTrashHelper(t, root, src, "hold", "")
		helper.expectLine(t, "payload-ready")
		helper.kill(t)

		st, err := Open(root)
		So(err, ShouldBeNil)

		tr := st.Trash()

		legit := filepath.Join(t.TempDir(), "legit.txt")
		mkFile(t, legit, "legit", 0o600)

		_, putErr := tr.Put(context.Background(), legit, PutOptions{})
		So(putErr, ShouldBeNil)

		Convey("When orphan buckets are purged", func() {
			count, purgeErr := tr.PurgeBefore(time.Now().Add(-time.Hour))

			Convey("Then the crash orphan is reaped and the entry-bearing bucket is kept", func() {
				So(purgeErr, ShouldBeNil)
				So(count, ShouldEqual, 1)

				list, listErr := tr.List()
				So(listErr, ShouldBeNil)
				So(list, ShouldHaveLength, 1)
				So(list[0].Original, ShouldEqual, legit)
			})
		})
	})
}

func TestTrashLockFileNotABucket(t *testing.T) {
	Convey("Given a trash with one entry", t, func() {
		st := newStore(t, time.Now)
		tr := st.Trash()

		src := filepath.Join(t.TempDir(), "file.txt")
		mkFile(t, src, "x", 0o600)

		_, err := tr.Put(context.Background(), src, PutOptions{})
		So(err, ShouldBeNil)

		Convey("When the lock file and the listing are inspected", func() {
			lockPath := filepath.Join(st.TrashDir(), ".lock")
			assertMode(t, lockPath, 0o600)

			list, listErr := tr.List()
			So(listErr, ShouldBeNil)
			So(list, ShouldHaveLength, 1)

			count, purgeErr := tr.PurgeBefore(time.Now().Add(time.Hour))

			Convey("Then the lock file is never treated as a bucket", func() {
				So(purgeErr, ShouldBeNil)
				So(count, ShouldEqual, 1)

				_, statErr := os.Stat(lockPath)
				So(statErr, ShouldBeNil)

				empty, emptyErr := tr.List()
				So(emptyErr, ShouldBeNil)
				So(empty, ShouldBeEmpty)
			})
		})
	})
}

func TestTrashPutRollbackMessage(t *testing.T) {
	Convey("Given entry.json writing fails after the bucket vanished", t, func() {
		st := newStore(t, time.Now)
		tr := st.Trash()

		src := filepath.Join(t.TempDir(), "precious.txt")
		mkFile(t, src, "precious", 0o600)

		var bucket string

		tr.writeEntryFile = func(path string, data []byte, mode fs.FileMode) error {
			bucket = filepath.Dir(path)
			_ = os.RemoveAll(bucket)

			return errors.New("disk full")
		}

		_, err := tr.Put(context.Background(), src, PutOptions{})

		Convey("Then the rollback error names the bucket it did not remove", func() {
			So(err, ShouldBeError)
			So(strings.Contains(err.Error(), "rollback"), ShouldBeTrue)
			So(strings.Contains(err.Error(), bucket), ShouldBeTrue)
		})
	})
}

func TestTrashPutRollbackKeepsBucket(t *testing.T) {
	Convey("Given entry.json writing fails with the payload still in the bucket", t, func() {
		st := newStore(t, time.Now)
		tr := st.Trash()

		src := filepath.Join(t.TempDir(), "precious.txt")
		mkFile(t, src, "precious", 0o600)

		ctx, cancel := context.WithCancel(context.Background())

		tr.writeEntryFile = func(string, []byte, fs.FileMode) error {
			cancel() // the rollback move must fail before it touches the payload

			return errors.New("disk full")
		}

		_, err := tr.Put(ctx, src, PutOptions{})

		Convey("Then the bucket and its payload survive and the error names the bucket", func() {
			So(err, ShouldBeError)

			names := bucketNames(t, st.TrashDir())
			So(names, ShouldHaveLength, 1)

			bucket := filepath.Join(st.TrashDir(), names[0])
			So(strings.Contains(err.Error(), bucket), ShouldBeTrue)
			So(fsutil.Exists(filepath.Join(bucket, "payload")), ShouldBeTrue)
			So(string(readTestFile(t, filepath.Join(bucket, "payload"))), ShouldEqual, "precious")
			So(fsutil.Exists(src), ShouldBeFalse)
		})
	})
}

func TestTrashLockUsesGofrsFlock(t *testing.T) {
	Convey("Given an external gofrs/flock holder on <trash>/.lock", t, func() {
		st := newStore(t, time.Now)
		tr := st.Trash()
		So(st.Ensure(), ShouldBeNil)

		lockPath := filepath.Join(st.TrashDir(), ".lock")
		external := flock.New(lockPath)

		locked, err := external.TryLock()
		So(err, ShouldBeNil)
		So(locked, ShouldBeTrue)

		src := filepath.Join(t.TempDir(), "file.txt")
		mkFile(t, src, "x", 0o600)

		Convey("When Put waits for the lock with a short context", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()

			_, putErr := tr.Put(ctx, src, PutOptions{})

			Convey("Then it fails bounded by the context and leaves the source", func() {
				So(putErr, ShouldBeError)
				So(errors.Is(putErr, context.DeadlineExceeded), ShouldBeTrue)
				So(fsutil.Exists(src), ShouldBeTrue)
			})
		})

		Convey("When PurgeBefore runs against the held lock", func() {
			count, purgeErr := tr.PurgeBefore(time.Now().Add(-time.Hour))

			Convey("Then it defers without waiting", func() {
				So(purgeErr, ShouldBeNil)
				So(count, ShouldEqual, 0)
			})
		})

		Convey("When the external holder releases", func() {
			So(external.Unlock(), ShouldBeNil)

			Convey("Then mutators take the same lock and release it", func() {
				_, putErr := tr.Put(context.Background(), src, PutOptions{})
				So(putErr, ShouldBeNil)

				probe := flock.New(lockPath)

				free, probeErr := probe.TryLock()
				So(probeErr, ShouldBeNil)
				So(free, ShouldBeTrue)
				So(probe.Unlock(), ShouldBeNil)
			})
		})
	})
}
