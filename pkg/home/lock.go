package home

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// lockRetryDelay is the retry cadence while waiting for the home flock.
const lockRetryDelay = 100 * time.Millisecond

// Unlock releases a held home lock; it is safe to call more than once.
type Unlock func() error

// Lock takes the shared home flock at <home>/state/.lock. ctx bounds the wait:
// expiry or cancellation yields *LockedError wrapping ctx.Err(). Locks are per
// open file description, so a second Lock in the same process fails instead of
// re-entering.
func (h *Home) Lock(ctx context.Context) (Unlock, error) {
	path := h.FileLockPath()

	if err := ctx.Err(); err != nil {
		return nil, &LockedError{Path: path, Cause: err}
	}

	if err := ensureDir(h.StateDir()); err != nil {
		return nil, err
	}

	fileLock := flock.New(path)

	locked, err := fileLock.TryLockContext(ctx, lockRetryDelay)
	if err != nil {
		return nil, &LockedError{Path: path, Cause: err}
	}

	if !locked {
		// Defensive guard: TryLockContext reports a miss only together with a
		// context error (handled above); this branch keeps the contract if
		// that ever changes.
		return nil, &LockedError{Path: path, Cause: errors.New("lock is held by another process")}
	}

	// The lock file belongs to verger: enforce 0600 even when it pre-existed.
	//nolint:gosec // G302: a lock file is owner-only 0600
	if err := os.Chmod(path, 0o600); err != nil {
		_ = fileLock.Unlock()

		return nil, fmt.Errorf("restrict lock %s: %w", path, err)
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

// LockedError reports a home lock that could not be taken within the context.
type LockedError struct {
	Path  string
	Cause error
}

// Error implements error.
func (e *LockedError) Error() string {
	return fmt.Sprintf("home is locked: %s: %v", e.Path, e.Cause)
}

// Unwrap returns the underlying cause (a context error or flock failure).
func (e *LockedError) Unwrap() error {
	return e.Cause
}
