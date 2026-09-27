package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	pkgid "github.com/odiumuniverse/verger/pkg/id"
)

const (
	entrySchema      = 1
	entryFileName    = "entry.json"
	payloadName      = "payload"
	bucketTimeLayout = "20060102T150405.000000000Z"
	maxBucketSuffix  = 10000

	trashLockName  = ".lock"
	trashLockRetry = 100 * time.Millisecond
)

// Test seams: production uses the fsutil implementations directly. These three
// unexported package vars are the accepted exception to "no production globals"
// (the fsutil.renameForTest pattern, phase0-decisions verify-1 T0.4 Q4).
var (
	renameEntry    = os.Rename
	moveTree       = fsutil.MoveTree
	writeEntryFile = fsutil.WriteFileAtomic
)

// errOrphan marks a bucket without entry.json (a crashed put).
var errOrphan = errors.New("orphan trash bucket")

// lockTrash takes the exclusive trash mutator flock, waiting until ctx ends.
func (t *Trash) lockTrash(ctx context.Context) (func() error, error) {
	fileLock, err := t.openTrashLock()
	if err != nil {
		return nil, err
	}

	locked, err := fileLock.TryLockContext(ctx, trashLockRetry)
	if err != nil {
		return nil, fmt.Errorf("lock trash %s: %w", fileLock.Path(), err)
	}

	if !locked {
		return nil, fmt.Errorf("lock trash %s: lock is held", fileLock.Path())
	}

	return t.finishTrashLock(fileLock)
}

// tryLockTrash takes the exclusive trash mutator flock without waiting.
func (t *Trash) tryLockTrash() (func() error, bool, error) {
	fileLock, err := t.openTrashLock()
	if err != nil {
		return nil, false, err
	}

	locked, err := fileLock.TryLock()
	if err != nil {
		return nil, false, fmt.Errorf("lock trash %s: %w", fileLock.Path(), err)
	}

	if !locked {
		return nil, false, nil
	}

	release, err := t.finishTrashLock(fileLock)
	if err != nil {
		return nil, false, err
	}

	return release, true, nil
}

// openTrashLock ensures the trash dir and returns an unlocked flock handle for
// <trash>/.lock. The handle opens/creates the file 0600; it carries no package
// state.
func (t *Trash) openTrashLock() (*flock.Flock, error) {
	if err := fsutil.EnsureDir(t.dir, 0o700); err != nil {
		return nil, err
	}

	return flock.New(filepath.Join(t.dir, trashLockName)), nil
}

// finishTrashLock enforces the 0600 lock-file mode and returns the idempotent
// release function.
func (t *Trash) finishTrashLock(fileLock *flock.Flock) (func() error, error) {
	// The lock file belongs to verger: enforce 0600 even when it pre-existed.
	//nolint:gosec // G302: a lock file is owner-only 0600
	if err := os.Chmod(fileLock.Path(), 0o600); err != nil {
		_ = fileLock.Unlock()

		return nil, fmt.Errorf("restrict trash lock %s: %w", fileLock.Path(), err)
	}

	var (
		once       sync.Once
		releaseErr error
	)

	return func() error {
		once.Do(func() { releaseErr = fileLock.Unlock() })

		return releaseErr
	}, nil
}

// Trash stores removed payloads in timestamped buckets so a later Restore can
// bring them back. It is backed by <store>/trash.
type Trash struct {
	dir       string
	now       func() time.Time
	retention time.Duration
}

// Entry describes one trashed payload.
type Entry struct {
	Schema    int         `json:"schema"`
	ID        string      `json:"id"`
	Original  string      `json:"original"` // absolute source path
	Stored    string      `json:"stored"`   // relative path inside the bucket
	Kind      string      `json:"kind"`     // file|dir
	Package   string      `json:"package,omitempty"`
	Host      string      `json:"host,omitempty"`
	Cause     string      `json:"cause,omitempty"` // user|capability|host-reset|disabled
	Digest    digest.Hash `json:"digest,omitempty"`
	RemovedAt time.Time   `json:"removed_at"`
}

// PutOptions tags one trash entry; Digest is recorded verbatim.
type PutOptions struct {
	Package string
	Host    string
	Cause   string
	Digest  digest.Hash
}

// Put moves src into a new trash bucket and returns its entry. A bucket is
// named by the UTC removal time; collisions get a -2, -3, … suffix.
// entry.json is written after the payload lands, so a bucket without it is an
// orphan. Mutators are serialized by the trash lock. Any failure removes the
// bucket and leaves src intact (or reports that the rollback could not).
func (t *Trash) Put(ctx context.Context, src string, opts PutOptions) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}

	info, err := os.Lstat(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Entry{}, &NotFoundError{Path: src}
		}

		return Entry{}, fmt.Errorf("stat source %s: %w", src, err)
	}

	original, err := filepath.Abs(src)
	if err != nil {
		return Entry{}, fmt.Errorf("absolute source %s: %w", src, err)
	}

	unlock, err := t.lockTrash(ctx)
	if err != nil {
		return Entry{}, err
	}

	defer func() { _ = unlock() }()

	at := t.now().UTC()

	id, bucket, err := t.newBucket(at)
	if err != nil {
		return Entry{}, err
	}

	payload := filepath.Join(bucket, payloadName)

	if err := moveEntry(ctx, original, payload); err != nil {
		_ = os.RemoveAll(bucket)

		return Entry{}, fmt.Errorf("move %s to trash: %w", original, err)
	}

	entry := Entry{
		Schema:    entrySchema,
		ID:        id,
		Original:  original,
		Stored:    payloadName,
		Kind:      entryKind(info),
		Package:   opts.Package,
		Host:      opts.Host,
		Cause:     opts.Cause,
		Digest:    opts.Digest,
		RemovedAt: at,
	}

	data, err := encodeEntry(entry)
	if err != nil {
		return Entry{}, rollbackPut(ctx, bucket, payload, original, err)
	}

	if err := writeEntryFile(filepath.Join(bucket, entryFileName), data, 0o600); err != nil {
		return Entry{}, rollbackPut(ctx, bucket, payload, original, err)
	}

	return entry, nil
}

// List returns every readable trash entry, sorted by removal time then id.
// Buckets without entry.json are skipped.
func (t *Trash) List() ([]Entry, error) {
	entries, err := os.ReadDir(t.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read trash dir: %w", err)
	}

	list := make([]Entry, 0, len(entries))

	for _, dir := range entries {
		if !dir.IsDir() || !pkgid.ValidateElement(dir.Name()) {
			continue
		}

		entry, err := t.loadEntry(dir.Name())
		if errors.Is(err, errOrphan) {
			continue
		}

		if err != nil {
			return nil, err
		}

		list = append(list, entry)
	}

	slices.SortStableFunc(list, func(a, b Entry) int {
		if c := a.RemovedAt.Compare(b.RemovedAt); c != 0 {
			return c
		}

		return strings.Compare(a.ID, b.ID)
	})

	return list, nil
}

// Get returns one trash entry; a missing bucket is a NotFoundError.
func (t *Trash) Get(id string) (Entry, error) {
	if !pkgid.ValidateElement(id) {
		return Entry{}, &InvalidIDError{Value: id, Reason: "invalid trash id"}
	}

	entry, err := t.loadEntry(id)
	if errors.Is(err, errOrphan) {
		return Entry{}, &NotFoundError{Path: filepath.Join(t.dir, id)}
	}

	if err != nil {
		return Entry{}, err
	}

	return entry, nil
}

// Restore moves the payload back to its original path and removes the bucket.
// An existing original is never overwritten; missing parents are created 0700.
// A missing bucket is a NotFoundError; a present bucket whose entry.json or
// payload is unreadable is a CorruptTrashError. Mutators are serialized by the
// trash lock.
func (t *Trash) Restore(ctx context.Context, id string) (Entry, error) {
	if !pkgid.ValidateElement(id) {
		return Entry{}, &InvalidIDError{Value: id, Reason: "invalid trash id"}
	}

	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}

	bucket := filepath.Join(t.dir, id)

	unlock, err := t.lockExistingBucket(ctx, bucket)
	if err != nil {
		return Entry{}, err
	}

	if unlock == nil {
		return Entry{}, &NotFoundError{Path: bucket}
	}

	defer func() { _ = unlock() }()

	entry, err := t.loadEntry(id)
	if errors.Is(err, errOrphan) {
		return Entry{}, &CorruptTrashError{ID: id, Cause: errors.New("entry.json is missing")}
	}

	if err != nil {
		return Entry{}, err
	}

	return t.restoreLocked(ctx, bucket, entry)
}

// restoreLocked performs the payload move of Restore; the caller holds the
// trash lock.
func (t *Trash) restoreLocked(ctx context.Context, bucket string, entry Entry) (Entry, error) {
	exists, err := lstatExists(entry.Original)
	if err != nil {
		return Entry{}, err
	}

	if exists {
		return Entry{}, &RestoreConflictError{Path: entry.Original}
	}

	payload := filepath.Join(bucket, entry.Stored)

	if _, err := os.Lstat(payload); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Entry{}, &CorruptTrashError{ID: entry.ID, Cause: fmt.Errorf("payload %s is missing", payload)}
		}

		return Entry{}, fmt.Errorf("stat payload %s: %w", payload, err)
	}

	if err := fsutil.EnsureDir(filepath.Dir(entry.Original), 0o700); err != nil {
		return Entry{}, err
	}

	if err := moveEntry(ctx, payload, entry.Original); err != nil {
		return Entry{}, fmt.Errorf("restore %s: %w", entry.Original, err)
	}

	if err := os.RemoveAll(bucket); err != nil {
		return entry, fmt.Errorf("remove trash bucket %s: %w", bucket, err)
	}

	return entry, nil
}

// lockExistingBucket takes the mutator lock and re-checks the bucket under it.
// A nil unlock with a nil error reports a missing bucket.
func (t *Trash) lockExistingBucket(ctx context.Context, bucket string) (func() error, error) {
	if _, err := os.Lstat(bucket); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("stat trash bucket %s: %w", bucket, err)
	}

	unlock, err := t.lockTrash(ctx)
	if err != nil {
		return nil, err
	}

	if _, err := os.Lstat(bucket); errors.Is(err, fs.ErrNotExist) {
		_ = unlock()

		return nil, nil
	} else if err != nil {
		_ = unlock()

		return nil, fmt.Errorf("stat trash bucket %s: %w", bucket, err)
	}

	return unlock, nil
}

// Remove permanently deletes one trash bucket. Mutators are serialized by the
// trash lock.
func (t *Trash) Remove(id string) error {
	if !pkgid.ValidateElement(id) {
		return &InvalidIDError{Value: id, Reason: "invalid trash id"}
	}

	bucket := filepath.Join(t.dir, id)

	unlock, err := t.lockExistingBucket(context.Background(), bucket)
	if err != nil {
		return err
	}

	if unlock == nil {
		return &NotFoundError{Path: bucket}
	}

	defer func() { _ = unlock() }()

	if err := os.RemoveAll(bucket); err != nil {
		return fmt.Errorf("remove trash bucket %s: %w", bucket, err)
	}

	return nil
}

// Purge removes every bucket older than the configured retention window.
func (t *Trash) Purge() (int, error) {
	return t.PurgeBefore(t.now().UTC().Add(-t.retention))
}

// PurgeBefore removes buckets whose RemovedAt is strictly before before, plus
// orphan buckets without entry.json. Unreadable and newer-schema buckets stay
// for doctor. When another mutator holds the trash lock the sweep is deferred:
// it returns (0, nil) and the next call reaps.
func (t *Trash) PurgeBefore(before time.Time) (int, error) {
	if _, err := os.Lstat(t.dir); errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("stat trash dir: %w", err)
	}

	unlock, locked, err := t.tryLockTrash()
	if err != nil {
		return 0, err
	}

	if !locked {
		return 0, nil
	}

	defer func() { _ = unlock() }()

	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return 0, fmt.Errorf("read trash dir: %w", err)
	}

	count := 0

	for _, dir := range entries {
		if !dir.IsDir() || !pkgid.ValidateElement(dir.Name()) {
			continue
		}

		entry, loadErr := t.loadEntry(dir.Name())

		switch {
		case errors.Is(loadErr, errOrphan):
			// Crash orphans are always purged; fall through to the removal.
		case loadErr != nil:
			continue
		case !entry.RemovedAt.Before(before):
			continue
		}

		if err := os.RemoveAll(filepath.Join(t.dir, dir.Name())); err != nil {
			return count, fmt.Errorf("remove trash bucket %s: %w", dir.Name(), err)
		}

		count++
	}

	return count, nil
}

// newBucket claims a fresh bucket directory by atomic mkdir, stepping the
// suffix until a name is free.
func (t *Trash) newBucket(at time.Time) (string, string, error) {
	stamp := at.Format(bucketTimeLayout)

	for i := 1; i <= maxBucketSuffix; i++ {
		id := stamp
		if i > 1 {
			id = fmt.Sprintf("%s-%d", stamp, i)
		}

		path := filepath.Join(t.dir, id)

		err := os.Mkdir(path, 0o700)
		switch {
		case err == nil:
			return id, path, nil
		case errors.Is(err, fs.ErrExist):
			continue
		default:
			return "", "", fmt.Errorf("create trash bucket %s: %w", path, err)
		}
	}

	return "", "", fmt.Errorf("create trash bucket %s: too many collisions", stamp)
}

// loadEntry reads and validates one bucket's entry.json. A bucket without the
// file reports errOrphan; every other failure is typed.
func (t *Trash) loadEntry(id string) (Entry, error) {
	path := filepath.Join(t.dir, id, entryFileName)

	data, err := os.ReadFile(path) //nolint:gosec // G304: id is validated before the read
	if errors.Is(err, fs.ErrNotExist) {
		return Entry{}, errOrphan
	}

	if err != nil {
		return Entry{}, &CorruptTrashError{ID: id, Cause: err}
	}

	entry := Entry{}
	if err := json.Unmarshal(data, &entry); err != nil {
		return Entry{}, &CorruptTrashError{ID: id, Cause: err}
	}

	switch {
	case entry.Schema > entrySchema:
		return Entry{}, &SchemaNewerError{Path: path, Found: entry.Schema, Supported: entrySchema}
	case entry.Schema < entrySchema:
		return Entry{}, &CorruptTrashError{ID: id, Cause: fmt.Errorf("unsupported schema %d", entry.Schema)}
	}

	if entry.ID != id {
		return Entry{}, &CorruptTrashError{ID: id, Cause: errors.New("entry id does not match its bucket")}
	}

	if entry.Original == "" || !filepath.IsAbs(entry.Original) {
		return Entry{}, &CorruptTrashError{ID: id, Cause: errors.New("entry original is not absolute")}
	}

	if !pkgid.ValidateElement(entry.Stored) {
		return Entry{}, &CorruptTrashError{ID: id, Cause: fmt.Errorf("stored name %q is not a safe element", entry.Stored)}
	}

	return entry, nil
}

// rollbackPut returns the payload to its original path and drops the bucket;
// when the move back fails the bucket is kept — the payload inside it is the
// only copy — and the error names the bucket.
func rollbackPut(ctx context.Context, bucket, payload, original string, cause error) error {
	if moveErr := moveEntry(ctx, payload, original); moveErr != nil {
		return fmt.Errorf("write entry: %w (rollback failed to restore %s; bucket not removed: %s: %w)",
			cause, original, bucket, moveErr)
	}

	_ = os.RemoveAll(bucket)

	return fmt.Errorf("write entry: %w", cause)
}

// encodeEntry renders one entry as indented JSON with a trailing newline.
func encodeEntry(entry Entry) ([]byte, error) {
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode entry: %w", err)
	}

	return append(data, '\n'), nil
}

// entryKind reports the entry kind: symlinks count as files.
func entryKind(info fs.FileInfo) string {
	if info.IsDir() {
		return "dir"
	}

	return "file"
}

// lstatExists reports whether path is present without following a symlink.
func lstatExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
}

// moveEntry moves one filesystem entry. Directory trees go through
// fsutil.MoveTree; files and symlinks use rename with an EXDEV copy fallback,
// because fsutil.MoveTree accepts directory sources only.
func moveEntry(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	info, err := os.Lstat(from)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &fsutil.SourceMissingError{Path: from}
		}

		return fmt.Errorf("stat %s: %w", from, err)
	}

	switch {
	case info.IsDir():
		return moveTree(ctx, from, to)
	case info.Mode()&fs.ModeSymlink != 0:
		return moveSymlink(ctx, from, to)
	case info.Mode().IsRegular():
		return moveRegular(ctx, from, to, info.Mode().Perm())
	default:
		return fmt.Errorf("move %s: unsupported file type %s", from, info.Mode())
	}
}

// moveRegular renames one file, falling back to a copy across filesystems.
func moveRegular(ctx context.Context, from, to string, perm fs.FileMode) error {
	err := renameEntry(from, to)
	if err == nil {
		return nil
	}

	if !fsutil.IsCrossDevice(err) {
		return fmt.Errorf("rename %s to %s: %w", from, to, err)
	}

	return copyRegular(ctx, from, to, perm)
}

// moveSymlink renames one symlink, falling back to a recreated link.
func moveSymlink(ctx context.Context, from, to string) error {
	err := renameEntry(from, to)
	if err == nil {
		return nil
	}

	if !fsutil.IsCrossDevice(err) {
		return fmt.Errorf("rename %s to %s: %w", from, to, err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	target, err := os.Readlink(from)
	if err != nil {
		return fmt.Errorf("read symlink %s: %w", from, err)
	}

	if err := os.Symlink(target, to); err != nil {
		return fmt.Errorf("create symlink %s: %w", to, err)
	}

	if err := os.Remove(from); err != nil {
		_ = os.Remove(to)

		return fmt.Errorf("remove source %s: %w", from, err)
	}

	return nil
}

// copyRegular copies one regular file and removes the source after the
// destination is synced and chmodded.
func copyRegular(ctx context.Context, from, to string, perm fs.FileMode) error {
	src, err := os.Open(from) //nolint:gosec // G304: the caller names the entry being moved
	if err != nil {
		return fmt.Errorf("open %s: %w", from, err)
	}

	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // G304: to is inside the trash bucket
	if err != nil {
		return fmt.Errorf("create %s: %w", to, err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(to)

		return fmt.Errorf("copy %s: %w", to, err)
	}

	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		_ = os.Remove(to)

		return fmt.Errorf("sync %s: %w", to, err)
	}

	if err := dst.Close(); err != nil {
		_ = os.Remove(to)

		return fmt.Errorf("close %s: %w", to, err)
	}

	if err := os.Chmod(to, perm); err != nil {
		_ = os.Remove(to)

		return fmt.Errorf("chmod %s: %w", to, err)
	}

	if err := ctx.Err(); err != nil {
		_ = os.Remove(to)

		return err
	}

	if err := os.Remove(from); err != nil {
		_ = os.Remove(to)

		return fmt.Errorf("remove source %s: %w", from, err)
	}

	return nil
}

// RestoreConflictError reports a restore whose original path already exists.
type RestoreConflictError struct {
	Path string
}

// Error implements error.
func (e *RestoreConflictError) Error() string {
	return "restore conflict: " + e.Path + " already exists"
}

// CorruptTrashError reports an unreadable or inconsistent trash entry.
type CorruptTrashError struct {
	ID    string
	Cause error
}

// Error implements error.
func (e *CorruptTrashError) Error() string {
	return fmt.Sprintf("corrupt trash entry %s: %v", e.ID, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *CorruptTrashError) Unwrap() error {
	return e.Cause
}
