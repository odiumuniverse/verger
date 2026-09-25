package fsutil

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()

		cleanup()

		return fmt.Errorf("sync temp file: %w", err)
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

	if err = syncDir(dir); err != nil {
		return fmt.Errorf("sync directory: %w", err)
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

func syncDir(dir string) error {
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
