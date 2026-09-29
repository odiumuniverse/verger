package home

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// maxLeaseBytes bounds how much of a lease file is read.
const maxLeaseBytes = 64 << 10

// Lease is the content of the watcher lease file (§3.3).
type Lease struct {
	PID   int       `json:"pid"`
	Owner string    `json:"owner"`
	Since time.Time `json:"since"`
}

// Lease owners: beadle holding the lease makes the verger watcher sleep.
const (
	LeaseOwnerVerger = "verger"
	LeaseOwnerBeadle = "beadle"
)

// LeaseHandle is a held lease: it keeps the flock until Release.
type LeaseHandle struct {
	mu       sync.Mutex
	lock     *flock.Flock
	path     string
	info     Lease
	released bool
}

// AcquireLease takes the watch lease at path for owner, non-blocking by
// design. A live holder yields *LeaseHeldError; a stale file without a holder
// is overwritten.
func AcquireLease(path, owner string) (*LeaseHandle, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, &InvalidOwnerError{Value: owner}
	}

	if err := ensureDir(filepath.Dir(path)); err != nil {
		return nil, err
	}

	fileLock := flock.New(path)

	locked, err := fileLock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock lease %s: %w", path, err)
	}

	if !locked {
		held, _ := loadLeaseFile(path) // best effort: the writer may be mid-write

		return nil, &LeaseHeldError{Owner: held.Owner, PID: held.PID, Since: held.Since}
	}

	// The lease file belongs to verger: enforce 0600 even when it pre-existed.
	//nolint:gosec // G302: a lease file is owner-only 0600
	if err := os.Chmod(path, 0o600); err != nil {
		_ = fileLock.Unlock()

		return nil, fmt.Errorf("restrict lease %s: %w", path, err)
	}

	info := Lease{PID: os.Getpid(), Owner: owner, Since: time.Now().UTC()}

	if err := writeLeaseFile(path, info); err != nil {
		_ = fileLock.Unlock()

		return nil, err
	}

	return &LeaseHandle{lock: fileLock, path: path, info: info}, nil
}

// Info returns a snapshot of the lease as held.
func (l *LeaseHandle) Info() Lease {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.info
}

// Release drops the lease, leaving the file in place to avoid unlink races with
// concurrent acquire/status calls. Idempotent.
func (l *LeaseHandle) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.released {
		return nil
	}

	l.released = true

	if err := l.lock.Unlock(); err != nil {
		return fmt.Errorf("unlock lease %s: %w", l.path, err)
	}

	return nil
}

// LeaseStatus reports whether the lease is currently held and, best effort, by
// whom. It never blocks and never mutates the file.
func LeaseStatus(path string) (Lease, bool, error) {
	return leaseStatus(path, loadLeaseFile)
}

// leaseStatus is LeaseStatus with the content reader injected, so a test can
// prove that the content is read while the probe lock is still held without
// the package carrying a mutable global (rule 13).
func leaseStatus(path string, read func(string) (Lease, error)) (Lease, bool, error) {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return Lease{}, false, nil
	} else if err != nil {
		return Lease{}, false, fmt.Errorf("stat lease %s: %w", path, err)
	}

	fileLock := flock.New(path, flock.SetFlag(os.O_RDONLY))

	locked, err := fileLock.TryLock()
	if err != nil {
		return Lease{}, false, fmt.Errorf("lock lease %s: %w", path, err)
	}

	if locked {
		// Read while the probe lock is still held: releasing it first would
		// open a window for a concurrent acquirer to truncate the file
		// mid-read.
		lease, readErr := read(path)

		_ = fileLock.Unlock()

		if readErr != nil {
			return Lease{}, false, readErr
		}

		return lease, false, nil
	}

	lease, readErr := read(path)
	if readErr != nil {
		return Lease{}, false, readErr
	}

	return lease, true, nil
}

// loadLeaseFile reads the current content; malformed, empty or missing content
// is reported as the zero lease, a true IO failure as an error.
func loadLeaseFile(path string) (Lease, error) {
	file, err := os.Open(path) //nolint:gosec // G304: the lease path is owned by the caller
	if errors.Is(err, fs.ErrNotExist) {
		return Lease{}, nil
	}

	if err != nil {
		return Lease{}, fmt.Errorf("open lease %s: %w", path, err)
	}

	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, maxLeaseBytes))
	if err != nil {
		return Lease{}, fmt.Errorf("read lease %s: %w", path, err)
	}

	lease := Lease{}
	if err := json.Unmarshal(bytes.TrimSpace(data), &lease); err != nil {
		return Lease{}, nil //nolint:nilerr // malformed content is reported as the zero lease
	}

	return lease, nil
}

// writeLeaseFile replaces the file content with lease and fsyncs it.
func writeLeaseFile(path string, lease Lease) error {
	data, err := json.Marshal(lease)
	if err != nil {
		return fmt.Errorf("encode lease: %w", err)
	}

	data = append(data, '\n')

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: the lease path is owned by the caller
	if err != nil {
		return fmt.Errorf("open lease %s: %w", path, err)
	}

	if _, err := file.Write(data); err != nil {
		_ = file.Close()

		return fmt.Errorf("write lease %s: %w", path, err)
	}

	if err := file.Sync(); err != nil {
		_ = file.Close()

		return fmt.Errorf("sync lease %s: %w", path, err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close lease %s: %w", path, err)
	}

	return nil
}

// LeaseHeldError reports a live lease held by another owner.
type LeaseHeldError struct {
	Owner string
	PID   int
	Since time.Time
}

// Error implements error.
func (e *LeaseHeldError) Error() string {
	return fmt.Sprintf("watch lease is held by %s (pid %d) since %s", e.Owner, e.PID, e.Since.Format(time.RFC3339))
}

// InvalidOwnerError reports an empty lease owner.
type InvalidOwnerError struct {
	Value string
}

// Error implements error.
func (e *InvalidOwnerError) Error() string {
	return fmt.Sprintf("invalid lease owner %q", e.Value)
}
