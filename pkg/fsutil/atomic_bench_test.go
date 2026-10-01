package fsutil

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// The two disciplines, spelled out rather than reached through a production flag,
// so the comparison is between two real sequences and not between two call sites.
//
// On darwin a File.Sync is F_FULLFSYNC — a request to the drive to flush its own
// write cache — and it dominates the cost of a write. On linux the same call is a
// cheap journal barrier. This benchmark is what tells us which of the two
// numbers applies, so the classification of which writes may drop the barrier is
// argued from a measurement rather than from an assumption about the platform.
func benchPayload() []byte {
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i % 251)
	}

	return data
}

// BenchmarkWriteFileAtomic is the discipline everything uses today: write, sync
// the bytes, rename, sync the directory.
func BenchmarkWriteFileAtomic(b *testing.B) {
	dir := b.TempDir()
	data := benchPayload()

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		if err := WriteFileAtomic(filepath.Join(dir, "object"), data, 0o600); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteTempRenameNoSync is the same sequence with the two barriers
// removed — the discipline a content-verified or re-derivable file can use.
func BenchmarkWriteTempRenameNoSync(b *testing.B) {
	dir := b.TempDir()
	data := benchPayload()

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		if err := writeTempRename(filepath.Join(dir, "object"), data, 0o600, nil, false); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSyncDirs is the batch barrier that replaces the per-file ones: one
// directory sync, however many objects the batch put in it.
func BenchmarkSyncDirs(b *testing.B) {
	dir := b.TempDir()
	data := benchPayload()

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		if err := writeTempRename(filepath.Join(dir, "object"), data, 0o600, nil, false); err != nil {
			b.Fatal(err)
		}

		if err := SyncDirs(dir); err != nil {
			b.Fatal(err)
		}
	}
}

// The two benchmarks below are the "before" and the "after" of the write
// classification, measured at the shape a run actually has rather than one write
// in isolation.
//
// The per-write numbers above are honest but they answer the wrong question: they
// price a single write, and a run does not do a single write. What the
// classification changed is the cost of a PACKAGE — several objects, in a couple
// of directories, flushed once at the end.
//
// "Before" is every object durable: each one pays a file barrier AND a directory
// barrier, because the durable writer renames and then flushes the directory
// itself. That second barrier is the part the per-write benchmark hides, and it
// is half the saving: batching collapses the directory barrier from once per
// object to once per directory, not just the file barrier from once per object to
// not at all.
//
// Four objects across two directories is the shape of a package delivered to four
// hosts whose skill file is shared: one object per host plus the shared tree.
const benchPackageObjects = 4

const benchPackageDirs = 2

// BenchmarkPackageDurable is the discipline before the classification: every
// object pays both barriers.
func BenchmarkPackageDurable(b *testing.B) {
	root := b.TempDir()
	data := benchPayload()

	dirs := make([]string, benchPackageDirs)
	for i := range dirs {
		dirs[i] = filepath.Join(root, "tree", strconv.Itoa(i))
		if err := os.MkdirAll(dirs[i], 0o700); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		for i := range benchPackageObjects {
			if err := WriteFileAtomic(filepath.Join(dirs[i%len(dirs)], "object"), data, 0o600); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkPackageBarrierFree is the discipline after it: no per-object barrier,
// and one directory barrier per directory at the end of the batch.
func BenchmarkPackageBarrierFree(b *testing.B) {
	root := b.TempDir()
	data := benchPayload()

	dirs := make([]string, benchPackageDirs)
	for i := range dirs {
		dirs[i] = filepath.Join(root, "tree", strconv.Itoa(i))
		if err := os.MkdirAll(dirs[i], 0o700); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		for i := range benchPackageObjects {
			dir := dirs[i%len(dirs)]

			if err := WriteFileAtomicCAS(filepath.Join(dir, "object"), data, 0o600); err != nil {
				b.Fatal(err)
			}
		}

		if err := SyncDirs(dirs...); err != nil {
			b.Fatal(err)
		}
	}
}
