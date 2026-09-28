// Package fsutil provides atomic file writes, path helpers and tree moves.
package fsutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

// ErrSymlinkUnsupported reports that symlinks cannot be created in a directory.
var ErrSymlinkUnsupported = errors.New("symlinks are not supported here")

// Test seams: unexported function variables that let tests force otherwise
// non-deterministic OS outcomes (EXDEV, symlink EPERM, device ids) without
// sleeps, skips or fake filesystems. Each defaults to the real implementation
// and is restored per test via t.Cleanup (decision T0.4 Q4).
//
// renameForTest lets tests force the EXDEV path deterministically.
var renameForTest = os.Rename

// symlinkLinker lets tests force symlink-creation failures deterministically.
var symlinkLinker = os.Symlink

// deviceIDForTest lets tests simulate a second device deterministically.
var deviceIDForTest = deviceID

// DestinationExistsError reports a copy or move destination that already exists.
type DestinationExistsError struct {
	Path string
}

// Error implements error.
func (e *DestinationExistsError) Error() string {
	return "destination exists: " + e.Path
}

// SourceMissingError reports a copy or move source that does not exist.
type SourceMissingError struct {
	Path string
}

// Error implements error.
func (e *SourceMissingError) Error() string {
	return "source missing: " + e.Path
}

// InvalidMoveError reports a copy or move that cannot start.
type InvalidMoveError struct {
	From   string
	To     string
	Reason string
}

// Error implements error.
func (e *InvalidMoveError) Error() string {
	return fmt.Sprintf("invalid move %s -> %s: %s", e.From, e.To, e.Reason)
}

// SameFS reports whether a and b live on the same filesystem, comparing the
// device ids behind os.Stat (symlinks resolve through the stat).
func SameFS(a, b string) (bool, error) {
	first, err := deviceIDForTest(a)
	if err != nil {
		return false, err
	}

	second, err := deviceIDForTest(b)
	if err != nil {
		return false, err
	}

	return first == second, nil
}

func deviceID(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat %s: not a unix stat", path)
	}

	// Stat_t.Dev is int32 on darwin and uint64 on linux: the conversion is
	// needed on one platform and redundant on the other.
	return uint64(stat.Dev), nil //nolint:gosec,unconvert // G115: device ids are small non-negative kernel values
}

// IsCrossDevice reports whether err is a cross-filesystem rename (EXDEV),
// seen through any %w chain.
func IsCrossDevice(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}

// CopyTree copies the directory tree, regular file or symlink at from to the
// non-existing path to. Directory and file modes are preserved exactly,
// symlinks are recreated with their target text, and FIFOs/sockets/devices are
// skipped inside a tree. from is never modified; on failure the partially
// created to is removed.
func CopyTree(ctx context.Context, from, to string) error {
	from, to, err := checkTreeArgs(from, to)
	if err != nil {
		return err
	}

	info, err := os.Lstat(from)
	if err != nil {
		return fmt.Errorf("stat source %s: %w", from, err)
	}

	if err := copyNode(ctx, from, to, info); err != nil {
		_ = removeTree(to)

		return err
	}

	return nil
}

// MoveTree moves the directory tree, regular file or symlink at from to the
// non-existing path to. It renames when possible; on EXDEV it stages a copy
// next to to, renames it into place and only then removes from. A parent-dir
// fsync failure after the rename is reported, but the move is already
// effective (a retry sees DestinationExistsError). Crash orphans (to.tmp-*)
// are not reaped here; verger gc/doctor collects them later (decision T0.3
// Q3).
func MoveTree(ctx context.Context, from, to string) error {
	from, to, err := checkTreeArgs(from, to)
	if err != nil {
		return err
	}

	err = renameForTest(from, to)
	if err == nil {
		if syncErr := syncDir(filepath.Dir(to)); syncErr != nil {
			return fmt.Errorf("sync directory: %w", syncErr)
		}

		return nil
	}

	if !IsCrossDevice(err) {
		return fmt.Errorf("move %s to %s: %w", from, to, err)
	}

	suffix, err := randomSuffix()
	if err != nil {
		return err
	}

	staging := to + ".tmp-" + suffix

	if err := CopyTree(ctx, from, staging); err != nil {
		_ = removeTree(staging)

		return err
	}

	if err := renameForTest(staging, to); err != nil {
		_ = removeTree(staging)

		return fmt.Errorf("rename staging %s to %s: %w", staging, to, err)
	}

	if err := syncDir(filepath.Dir(to)); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}

	if err := removeTree(from); err != nil {
		return fmt.Errorf("remove source %s: %w", from, err)
	}

	return nil
}

// removeTree removes path, restoring owner-write on read-only directories
// first so a tree that was copied with exact modes can still be taken away.
// Used on the MoveTree source it widens directory modes, and a partial
// RemoveAll failure can leave them changed; both are acceptable because the
// source is slated for deletion (T1.x gc/doctor owns any follow-up).
func removeTree(path string) error {
	_ = filepath.WalkDir(path, func(current string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.IsDir() {
			return nil //nolint:nilerr // best-effort chmod: unreadable entries are skipped and RemoveAll reports what is left
		}

		info, infoErr := entry.Info()
		if infoErr == nil && info.Mode().Perm()&0o200 == 0 {
			_ = os.Chmod(current, info.Mode().Perm()|0o700) //nolint:gosec // G122: paths come from our own walk of a caller-owned tree
		}

		return nil
	})

	return os.RemoveAll(path)
}

// checkTreeArgs expands ~ and validates the shared preconditions of CopyTree
// and MoveTree, returning the cleaned absolute paths (a non-absolute path after
// expansion is an InvalidMoveError per phase0 §T0.3).
func checkTreeArgs(from, to string) (string, string, error) {
	from, to, err := expandTreePaths(from, to)
	if err != nil {
		return "", "", err
	}

	info, err := os.Lstat(from)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", &SourceMissingError{Path: from}
		}

		return "", "", fmt.Errorf("stat source %s: %w", from, err)
	}

	if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0 {
		return "", "", &InvalidMoveError{From: from, To: to, Reason: "source is not a file, symlink or directory"}
	}

	if err := checkDestination(from, to); err != nil {
		return "", "", err
	}

	return from, to, nil
}

// expandTreePaths resolves ~ and enforces absolute, distinct paths.
func expandTreePaths(from, to string) (string, string, error) {
	expandedFrom, err := ExpandHome(from)
	if err != nil {
		return "", "", err
	}

	expandedTo, err := ExpandHome(to)
	if err != nil {
		return "", "", err
	}

	from = filepath.Clean(expandedFrom)
	to = filepath.Clean(expandedTo)

	if !filepath.IsAbs(from) || !filepath.IsAbs(to) {
		return "", "", &InvalidMoveError{From: from, To: to, Reason: "paths must be absolute"}
	}

	if from == to {
		return "", "", &InvalidMoveError{From: from, To: to, Reason: "source and destination are the same"}
	}

	return from, to, nil
}

// checkDestination enforces that to does not exist yet and its parent does.
func checkDestination(from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return &DestinationExistsError{Path: to}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat destination %s: %w", to, err)
	}

	parent, err := os.Stat(filepath.Dir(to))
	if err != nil || !parent.IsDir() {
		return &InvalidMoveError{From: from, To: to, Reason: "destination parent is missing"}
	}

	return nil
}

// copyNode copies one already-validated source: a directory tree, a regular
// file or a symlink (the link itself, matching the T0.4 trash payload).
func copyNode(ctx context.Context, from, to string, info fs.FileInfo) error {
	switch {
	case info.IsDir():
		return copyTree(ctx, from, to)
	case info.Mode()&fs.ModeSymlink != 0:
		link, err := os.Readlink(from)
		if err != nil {
			return fmt.Errorf("read symlink %s: %w", from, err)
		}

		if err := symlinkLinker(link, to); err != nil {
			return fmt.Errorf("create symlink %s: %w", to, classifySymlinkErr(err))
		}

		return nil
	default:
		return copyFile(from, to, info.Mode().Perm())
	}
}

// dirMode records one directory created during a copy that still waits for
// its exact mode.
type dirMode struct {
	path string
	mode fs.FileMode
}

func copyTree(ctx context.Context, from, to string) error {
	dirs := []dirMode{}

	err := filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}

		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		rel, err := filepath.Rel(from, path)
		if err != nil {
			return fmt.Errorf("relative path %s: %w", path, err)
		}

		return copyTreeEntry(filepath.Join(to, rel), path, entry, &dirs)
	})
	if err != nil {
		return err
	}

	for _, dir := range slices.Backward(dirs) {
		if err := os.Chmod(dir.path, dir.mode); err != nil {
			return fmt.Errorf("chmod dir %s: %w", dir.path, err)
		}

		if err := syncDir(dir.path); err != nil {
			return fmt.Errorf("sync dir %s: %w", dir.path, err)
		}
	}

	return nil
}

func copyTreeEntry(target, path string, entry fs.DirEntry, dirs *[]dirMode) error {
	info, err := entry.Info()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	switch {
	case entry.IsDir():
		// The final mode is applied after the children exist, so a read-only
		// source directory (0500) can still be populated.
		if err := os.Mkdir(target, 0o700); err != nil {
			return fmt.Errorf("create dir %s: %w", target, err)
		}

		*dirs = append(*dirs, dirMode{path: target, mode: info.Mode().Perm()})

		return nil
	case entry.Type()&fs.ModeSymlink != 0:
		link, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("read symlink %s: %w", path, err)
		}

		if err := symlinkLinker(link, target); err != nil { //nolint:gosec // G122: target is inside the fresh destination tree
			return fmt.Errorf("create symlink %s: %w", target, classifySymlinkErr(err))
		}

		return nil
	case entry.Type().IsRegular():
		return copyFile(path, target, info.Mode().Perm())
	default:
		// FIFOs, sockets and devices are skipped silently (T4.2 revisits).
		return nil
	}
}

func copyFile(from, to string, perm fs.FileMode) error {
	src, err := os.Open(from) //nolint:gosec // G304: the caller names the tree being moved
	if err != nil {
		return fmt.Errorf("open %s: %w", from, err)
	}

	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // G304: to is inside the destination tree
	if err != nil {
		return fmt.Errorf("create %s: %w", to, err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()

		return fmt.Errorf("copy %s: %w", to, err)
	}

	if err := dst.Sync(); err != nil {
		_ = dst.Close()

		return fmt.Errorf("sync %s: %w", to, err)
	}

	if err := dst.Close(); err != nil {
		return fmt.Errorf("close %s: %w", to, err)
	}

	if err := os.Chmod(to, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", to, err)
	}

	return nil
}
