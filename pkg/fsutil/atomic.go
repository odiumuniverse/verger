package fsutil

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
)

// WriteFileAtomic writes data to path by creating a temporary file in the
// parent directory, syncing it and renaming it over path. The parent directory
// must exist. On success path holds exactly data with mode perm; no temporary
// files remain. When path is a symlink, the link itself is replaced by a
// regular file; its target is never written through.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	return WriteFileAtomicChecked(path, data, perm, nil)
}

// WriteFileAtomicChecked is WriteFileAtomic with a pre-commit check: after the
// temporary file is written, synced and chmodded but before the rename, check
// runs. A check error aborts the write, so path keeps its previous content
// (or stays absent) and no temporary file remains.
func WriteFileAtomicChecked(path string, data []byte, perm fs.FileMode, check func() error) error {
	return writeTempRename(path, data, perm, check, true)
}

// WriteFileAtomicCAS is WriteFileAtomic for a file verger can rebuild or verify:
// the same write-temp-rename sequence and the same atomicity, without the two
// durability barriers.
//
// It exists because those barriers are the single most expensive thing verger
// does on darwin. A File.Sync there is F_FULLFSYNC — a request to the drive to
// flush its own write cache — and it costs about twenty milliseconds; on linux
// the same call is a cheap journal barrier, tens of microseconds. Measured on
// this repository (pkg/fsutil, 4 KiB objects):
//
//	darwin  WriteFileAtomic ~11-19 ms   write+rename only ~0.2 ms
//	linux   WriteFileAtomic ~28 µs      write+rename only ~21 µs
//
// A file whose bytes are checked against a digest on the next read, or which
// the next sync re-renders from the canon, does not need a barrier per write: a
// lost write is a missing or short file, and both are detected rather than
// served as if they were intact. A file that is the ONLY record of something —
// the spec, the lock, receipts, consent, a secret, the trash that holds the
// user's own previous bytes — does, and goes through WriteFileAtomic.
//
// The rename still has to become durable or the file is not reachable after a
// crash, so the caller batches: SyncPendingDirs flushes every directory the
// batch touched, once, at the end of the run.

// # Which writer a call site uses
//
// The two writers are not a style choice; they are two answers to one question,
// and the question is: **if this write is lost, is the loss the same answer as
// the file being absent?**
//
// If yes, the file is RECOVERABLE. The next run re-derives it, and a lost write
// is indistinguishable from a file that was never there — which is safe,
// because the two states mean the same thing to every reader. These go through
// WriteFileAtomicCAS, with one directory fsync per package at the end of the run
// so the rename itself survives.
//
// If no, the file is IRRECOVERABLE: it is the only record of something, and
// nothing can rebuild it because the thing it records is gone. These go through
// WriteFileAtomic and keep both barriers.
//
// The distinction that decides the hard cases is not "is it regenerable" but
// "does its absence mean the same as its loss". Two examples, both of which
// look regenerable and are not:
//
//   - pkg/runtime.WriteManifest. It is rebuilt from the canon at every delivery,
//     so "lost" looks like "regenerable". But the runtime reads it WHEN VERGER
//     IS NOT RUNNING, to decide which hooks to execute. A lost manifest is not
//     an absent manifest: absence means no hooks run, and that is a silent
//     behaviour change with no recovery until the next delivery. Durable.
//   - pkg/apply.backupUserEdit and pkg/store/trash. Both look like copies of
//     something that still exists elsewhere. They are the user's PREVIOUS bytes,
//     and the canon holds the NEW value — so "rewrite it" has no meaning. Once
//     the backup is gone the bytes are gone. Durable.
//
// ## Irrecoverable — WriteFileAtomic, full fsync
//
//	pkg/spec.save              verger.toml: the declaration of what is installed
//	pkg/lock.Save              the lock; two writers is the failure it prevents
//	pkg/receipt.Put            receipts: what was delivered, with digests
//	pkg/receipt/tombstone.save tombstones: what was removed
//	pkg/consent.save           consent and trust decisions
//	pkg/secret.Save            the secrets file
//	pkg/store.trash.writeEntry the user's previous bytes, held in the trash
//	pkg/apply.backupUserEdit   the user's previous bytes, before an edit
//	pkg/apply.writeConfig      a key in the user's own settings.json
//	pkg/runtime.WriteManifest  what the runtime runs when verger is absent
//	pkg/verger.moveWholeFile   a file moved between homes
//
// pkg/receipt's journal is durable too and is not in this table because it is
// not an atomic-rename write: it appends and then calls file.Sync() itself.
//
// ## Recoverable — WriteFileAtomicCAS, one directory fsync per package
//
//	pkg/host.executeWrite          a delivered file; the receipt carries its digest
//	pkg/host.copyRegularFile       a delivered file, copied
//	pkg/host.writeDoc              a synth document, re-rendered from the canon
//	pkg/host.stageRenamedExtension the gemini overlay
//	pkg/pack.stageArtifact         pack staging, regenerated from the source
//	pkg/source.writeEntryMeta      fetch metadata, re-fetched
//	pkg/hostcli.Save               the host CLI records cache, re-derived by asking
//	pkg/runtime.WriteHeartbeat     a liveness marker; absence means "not running"
//	pkg/verger.excludeFromGit      an idempotent .git/info/exclude entry
func WriteFileAtomicCAS(path string, data []byte, perm fs.FileMode) error {
	if err := writeTempRename(path, data, perm, nil, false); err != nil {
		return err
	}

	notePendingDir(filepath.Dir(path))

	return nil
}

// writeTempRename is the sequence both writers share: a temp file beside the
// target, the bytes, the mode, an optional guard, and a rename. The rename is
// what makes the write atomic, so it is in both; only the barriers differ.
func writeTempRename(path string, data []byte, perm fs.FileMode, check func() error, durable bool) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	tmpName := tmp.Name()

	cleanup := func() {
		_ = os.Remove(tmpName)
	}

	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()

		cleanup()

		return fmt.Errorf("write temp file: %w", err)
	}

	// Only the durable writer pays this. Skipping it is the whole point of the
	// CAS path: the file is either re-rendered from the canon or checked against
	// a digest, so a lost write is a missing or short file — never a wrong one.
	if durable {
		if err = fileSync(tmp); err != nil {
			_ = tmp.Close()

			cleanup()

			return fmt.Errorf("sync temp file: %w", err)
		}
	}

	if err = tmp.Close(); err != nil {
		cleanup()

		return fmt.Errorf("close temp file: %w", err)
	}

	if err = os.Chmod(tmpName, perm); err != nil {
		cleanup()

		return fmt.Errorf("chmod temp file: %w", err)
	}

	if check != nil {
		if err = check(); err != nil {
			cleanup()

			return err
		}
	}

	if err = os.Rename(tmpName, path); err != nil {
		cleanup()

		return fmt.Errorf("rename temp file: %w", err)
	}

	if !durable {
		return nil
	}

	if err = syncDir(dir); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}

	return nil
}

// fileSync is the file barrier, a var so a test can count it.
//
// A barrier cannot be observed by crashing a process: fsync buys durability
// against power loss, and a killed process loses nothing from the page cache.
// So the thing a test CAN check is which writer a call went through, and that
// is the difference the CAS path turns on — it is what stops a later edit from
// quietly making the cheap writer pay for a durability nothing reads.
var fileSync = func(f *os.File) error { return f.Sync() }

// syncDir is the directory barrier, a var for the same reason. Making the NAME
// the seam rather than the call sites means a new caller cannot forget it:
// every existing syncDir(dir) call site goes through this var unchanged.
var syncDir = syncDirDefault

// CountBarriers runs fn with both barriers wrapped by counters and reports how
// many of each it paid. It is the only way to observe which writer a call
// reached from another package: fileSync and syncDir are unexported, so a test
// outside fsutil had no seam at all, and the comparison between the two
// disciplines in pkg/apply asserted a barrier difference it could not see — it
// compared bytes and modes, which are identical whichever writer ran.
//
// The real barriers still run underneath the counters. Counting must not change
// what the writer does, or the test would be measuring a different program than
// the one that ships — and a caller that returned early would otherwise be
// measured as a cheaper writer than it is.
func CountBarriers(fn func() error) (fileSyncs, dirSyncs int, err error) {
	realFile, realDir := fileSync, syncDir

	defer func() { fileSync, syncDir = realFile, realDir }()

	fileSync = func(f *os.File) error {
		fileSyncs++

		return realFile(f)
	}

	syncDir = func(dir string) error {
		dirSyncs++

		return realDir(dir)
	}

	err = fn()

	return fileSyncs, dirSyncs, err
}

// pendingDirs is the set of directories a barrier-free write landed in, so the
// batch has one place to flush from rather than a list threaded through every
// adapter. A directory is a set entry, not a counter: a batch that writes ten
// objects into one tree needs one directory sync for that tree.
var pendingDirs sync.Map

func notePendingDir(dir string) {
	pendingDirs.Store(dir, struct{}{})
}

// SyncDirs flushes the directories a batch of barrier-free writes touched. It
// is the one barrier that replaces the per-file ones, and it wants every
// directory, not just the last.
func SyncDirs(dirs ...string) error {
	for _, dir := range dirs {
		if err := syncDir(dir); err != nil {
			return fmt.Errorf("sync directory %s: %w", dir, err)
		}
	}

	return nil
}

// SyncPendingDirs flushes every directory recorded since the last call and
// clears the set. The runner calls it once, at the end of a batch, while it
// still holds the lock that stops another writer from interleaving its own
// flush with this one.
//
// It is safe to call when nothing was written: an empty set is not an error,
// and a run that only touched durable files has nothing to flush.
func SyncPendingDirs() error {
	var dirs []string

	pendingDirs.Range(func(key, _ any) bool {
		dir, ok := key.(string)
		if !ok {
			return true
		}

		// A directory a run wrote into and then REMOVED is still pending, and
		// there is nothing to flush about it: the absence is the outcome. Only
		// a same-run removal can produce this, because a barrier-free write is
		// the thing that recorded the directory and the removal is a later step
		// of the same plan. Syncing it anyway failed the run on ENOENT.
		if _, err := os.Stat(dir); err != nil && os.IsNotExist(err) {
			return true
		}

		dirs = append(dirs, dir)

		return true
	})

	if len(dirs) == 0 {
		return nil
	}

	// Sorted, so a failure names the same directory on every machine and two
	// runs of the same batch flush in the same order.
	slices.Sort(dirs)

	if err := SyncDirs(dirs...); err != nil {
		return err
	}

	for _, dir := range dirs {
		pendingDirs.Delete(dir)
	}

	return nil
}

// EnsureDir creates path and its parents with perm and makes the leaf carry
// exactly perm regardless of umask. An existing directory is never
// re-chmodded.
func EnsureDir(path string, perm fs.FileMode) error {
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("ensure dir %s: not a directory", path)
		}

		return nil
	}

	if err := os.MkdirAll(path, perm); err != nil {
		return fmt.Errorf("ensure dir %s: %w", path, err)
	}

	if err := os.Chmod(path, perm); err != nil {
		return fmt.Errorf("ensure dir %s: %w", path, err)
	}

	return nil
}

func syncDirDefault(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: dir is the parent of the file being written, resolved by callers
	if err != nil {
		return err
	}

	defer func() { _ = d.Close() }()

	return d.Sync()
}

// classifySymlinkErr maps "this filesystem cannot hold symlinks" errors to
// ErrSymlinkUnsupported.
func classifySymlinkErr(err error) error {
	for _, denied := range []error{syscall.EPERM, syscall.EOPNOTSUPP, syscall.ENOTSUP, syscall.ENOSYS} {
		if errors.Is(err, denied) {
			return fmt.Errorf("%w: %w", ErrSymlinkUnsupported, err)
		}
	}

	return err
}

// randomSuffix returns a short random hex token for staging names.
func randomSuffix() (string, error) {
	var suffix [4]byte

	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate temp name: %w", err)
	}

	return hex.EncodeToString(suffix[:]), nil
}
