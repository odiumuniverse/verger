package pack

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/store"
)

// PackWriteError reports a staging or rename failure. The target keeps its
// previous content, or is left empty.
//
// After the swap has started the target is moved aside to a `.old-*` sibling
// before the new tree is renamed in. If that second rename fails the backup is
// moved back, so the target holds either its previous content or the new one —
// but a **crash** (SIGKILL, power loss) between the two renames leaves the
// target absent and the previous content in the `.old-*` sibling. Write owns
// that recovery: the next call restores it. See recoverStale.
type PackWriteError struct {
	Dir   string
	Cause error
}

// Error implements error.
func (e *PackWriteError) Error() string {
	if e.Dir != "" {
		return fmt.Sprintf("write synth %s: %v", e.Dir, e.Cause)
	}

	return fmt.Sprintf("write synth: %v", e.Cause)
}

// Unwrap returns the underlying cause.
func (e *PackWriteError) Unwrap() error {
	return e.Cause
}

// Result is one written synth directory.
type Result struct {
	Dir      string
	Artifact Artifact
}

// Write renders in and installs it at st.EnsureSynthPath(in.ID, in.Version).
// Files go through fsutil.WriteFileAtomic into a staging sibling, which is
// renamed into place; a target that already carries the exact artifact is a
// no-op returning the same dir. The store path validation errors pass through
// unchanged; every staging/rename failure is a *PackWriteError.
//
// The install is crash-safe rather than crash-proof: a killed process can
// leave a `<synth>.tmp-*` staging tree and a `<synth>.old-*` backup of the
// previous version beside the target. Every call first recovers the state a
// previous call left behind — a backup is renamed back when the target is
// missing, and every leftover is removed once the target is in place — so
// those siblings have an owner and never accumulate.
func Write(ctx context.Context, st *store.Store, in Input) (Result, error) {
	if st == nil {
		return Result{}, &PackWriteError{Cause: errors.New("store is required")}
	}

	target, err := st.SynthPath(in.ID, in.Version)
	if err != nil {
		return Result{}, err
	}

	if err := ctx.Err(); err != nil {
		return Result{}, &PackWriteError{Dir: target, Cause: err}
	}

	art, err := Render(in)
	if err != nil {
		return Result{}, err
	}

	if _, err := st.EnsureSynthPath(in.ID, in.Version); err != nil {
		return Result{}, &PackWriteError{Dir: target, Cause: err}
	}

	if sameTarget(target, art) {
		return Result{Dir: target, Artifact: art}, nil
	}

	// Recover whatever a crashed earlier install left beside the target, so
	// the swap below always starts from a known state.
	recoverStale(target)

	staging, err := os.MkdirTemp(filepath.Dir(target), filepath.Base(target)+".tmp-*")
	if err != nil {
		return Result{}, &PackWriteError{Dir: target, Cause: err}
	}

	defer func() { _ = os.RemoveAll(staging) }()

	if err := stageArtifact(ctx, staging, art); err != nil {
		return Result{}, &PackWriteError{Dir: target, Cause: err}
	}

	if err := syncDir(staging); err != nil {
		return Result{}, &PackWriteError{Dir: target, Cause: err}
	}

	if err := ctx.Err(); err != nil {
		return Result{}, &PackWriteError{Dir: target, Cause: err}
	}

	if err := replaceTarget(target, staging); err != nil {
		return Result{}, &PackWriteError{Dir: target, Cause: err}
	}

	return Result{Dir: target, Artifact: art}, nil
}

// stageArtifact writes every artifact file and symlink into the staging dir and
// normalizes the directory modes.
func stageArtifact(ctx context.Context, staging string, art Artifact) error {
	for _, rel := range slices.Sorted(maps.Keys(art.Files)) {
		if err := ctx.Err(); err != nil {
			return err
		}

		target, err := stagedPath(staging, rel)
		if err != nil {
			return err
		}

		if err := fsutil.EnsureDir(filepath.Dir(target), 0o700); err != nil {
			return err
		}

		if err := fsutil.WriteFileAtomicCAS(target, art.Files[rel], 0o600); err != nil {
			return err
		}
	}

	for _, rel := range slices.Sorted(maps.Keys(art.symlinks)) {
		if err := ctx.Err(); err != nil {
			return err
		}

		target, err := stagedPath(staging, rel)
		if err != nil {
			return err
		}

		if err := fsutil.EnsureDir(filepath.Dir(target), 0o700); err != nil {
			return err
		}

		if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("staging path %s already exists", rel)
		}

		if err := os.Symlink(art.symlinks[rel], target); err != nil {
			return err
		}
	}

	return chmodDirs(staging)
}

// stagedPath joins one artifact path below the staging dir, refusing escapes.
func stagedPath(staging, rel string) (string, error) {
	local := filepath.FromSlash(rel)

	if rel == "" || strings.ContainsRune(rel, 0) || !filepath.IsLocal(local) {
		return "", fmt.Errorf("artifact path %q escapes the synth dir", rel)
	}

	return filepath.Join(staging, local), nil
}

// chmodDirs forces every directory of a staged tree to 0700 regardless of the
// process umask.
func chmodDirs(root string) error {
	return filepath.WalkDir(root, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return os.Chmod(current, 0o700) //nolint:gosec // G302: synth directories are 0700 by convention
		}

		return nil
	})
}

// sameTarget reports whether the target already carries exactly the artifact:
// every file byte-equal, every symlink equal, no foreign file.
func sameTarget(target string, art Artifact) bool {
	seen := 0
	differ := false

	err := filepath.WalkDir(target, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(target, current)
		if relErr != nil {
			return relErr
		}

		if rel == "." || entry.IsDir() {
			return nil
		}

		matched, matchErr := sameEntry(current, filepath.ToSlash(rel), entry, art)
		if matchErr != nil {
			return matchErr
		}

		if !matched {
			differ = true

			return fs.SkipAll
		}

		seen++

		return nil
	})

	return err == nil && !differ && seen == len(art.Files)+len(art.symlinks)
}

// sameEntry compares one non-directory target entry against the artifact; a
// mismatch reports (false, nil) so the walk can stop early.
func sameEntry(current, rel string, entry fs.DirEntry, art Artifact) (bool, error) {
	switch {
	case entry.Type()&fs.ModeSymlink != 0:
		want, ok := art.symlinks[rel]
		if !ok {
			return false, nil
		}

		got, err := os.Readlink(current)
		if err != nil {
			return false, err
		}

		return got == want, nil
	case entry.Type().IsRegular():
		want, ok := art.Files[rel]
		if !ok {
			return false, nil
		}

		got, err := os.ReadFile(current) //nolint:gosec // G304: the target is a store synth dir
		if err != nil {
			return false, err
		}

		return bytes.Equal(got, want), nil
	default:
		return false, nil
	}
}

// recoverStale takes ownership of the siblings a crashed install left beside a
// synth target. A `<synth>.old-*` backup means the process died between the
// "move the target aside" and the "rename the new tree in" renames: the target
// is then missing and the backup holds the last good version, so the backup is
// renamed back. Leftover `<synth>.tmp-*` staging trees and any backup that is
// no longer needed are removed. It is best effort — every step is ignored on
// failure, because a leftover directory must never fail the write that found
// it.
func recoverStale(target string) {
	parent := filepath.Dir(target)
	base := filepath.Base(target)

	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}

	_, targetErr := os.Lstat(target)
	targetMissing := errors.Is(targetErr, fs.ErrNotExist)

	// Oldest first: the first backup is the version the crashed swap moved
	// aside, and any later one is debris from an even older run.
	slices.SortFunc(entries, func(a, b fs.DirEntry) int {
		return cmp.Compare(a.Name(), b.Name())
	})

	for _, entry := range entries {
		name := entry.Name()

		switch {
		case strings.HasPrefix(name, base+".tmp-"):
			_ = os.RemoveAll(filepath.Join(parent, name))
		case strings.HasPrefix(name, base+".old-") && targetMissing:
			if err := os.Rename(filepath.Join(parent, name), target); err == nil {
				targetMissing = false

				continue
			}

			_ = os.RemoveAll(filepath.Join(parent, name))
		case strings.HasPrefix(name, base+".old-"):
			_ = os.RemoveAll(filepath.Join(parent, name))
		}
	}
}

// replaceTarget atomically swaps the staged tree in. An existing target is
// moved aside first and restored when the swap fails; the backup is removed
// after a successful swap, and the parent directory is fsynced.
func replaceTarget(target, staging string) error {
	parent := filepath.Dir(target)

	if _, err := os.Lstat(target); errors.Is(err, fs.ErrNotExist) {
		if err := os.Rename(staging, target); err != nil {
			return err
		}

		return syncDir(parent)
	} else if err != nil {
		return err
	}

	backup, err := os.MkdirTemp(parent, filepath.Base(target)+".old-*")
	if err != nil {
		return err
	}

	if err := os.Remove(backup); err != nil {
		return err
	}

	if err := os.Rename(target, backup); err != nil {
		return err
	}

	if err := os.Rename(staging, target); err != nil {
		if restoreErr := os.Rename(backup, target); restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restore %s: %w", target, restoreErr))
		}

		return err
	}

	if err := syncDir(parent); err != nil {
		return err
	}

	return os.RemoveAll(backup)
}

// syncDir flushes one directory to disk.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: dir is a store synth dir or its staging sibling
	if err != nil {
		return err
	}

	defer func() { _ = d.Close() }()

	return d.Sync()
}
