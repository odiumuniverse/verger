package digest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// SkipFunc decides whether a tree entry is excluded. rel is slash-separated and
// relative to the walk root; isDir tells directories from files. Returning true
// for a directory prunes its whole subtree.
type SkipFunc func(rel string, isDir bool) bool

// Tree returns the digest of the regular files below root: every file, sorted
// by its slash-separated relative path, contributes `rel \x00 hex \n` to a
// SHA-256 stream. Symlinks, directories and special files (FIFO, socket,
// device) are skipped; empty directories do not participate. An empty tree
// digests as Bytes(nil).
func Tree(root string) (Hash, error) {
	return TreeWithSkip(root, nil)
}

// TreeWithSkip is Tree with a filter: skip is consulted for every non-root
// entry, and the root itself is never skipped.
func TreeWithSkip(root string, skip SkipFunc) (Hash, error) {
	resolved, err := treeRoot(root)
	if err != nil {
		return "", err
	}

	lines := []string{}

	walkErr := filepath.WalkDir(resolved, func(path string, entry fs.DirEntry, err error) error {
		line, keep, err := treeLine(resolved, path, entry, err, skip)
		if err != nil {
			return err
		}

		if keep {
			lines = append(lines, line)
		}

		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}

	slices.Sort(lines)

	return Bytes([]byte(strings.Join(lines, ""))), nil
}

// treeRoot validates the walk root and resolves a symlink root to the
// directory behind it.
func treeRoot(root string) (string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return "", &UnreadableError{Path: root, Cause: err}
	}

	if !info.IsDir() {
		return "", &UnreadableError{Path: root, Cause: errors.New("not a directory")}
	}

	// A symlink root is followed (like File), so the alias digests the tree
	// behind it instead of an empty set.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", &UnreadableError{Path: root, Cause: err}
	}

	return resolved, nil
}

// treeLine returns the canonical line of one walk entry; keep is false for
// skipped entries and filepath.SkipDir prunes a skipped directory.
func treeLine(root, path string, entry fs.DirEntry, walkErr error, skip SkipFunc) (string, bool, error) {
	if walkErr != nil {
		return "", false, &UnreadableError{Path: path, Cause: walkErr}
	}

	if path == root {
		return "", false, nil
	}

	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", false, &UnreadableError{Path: path, Cause: err}
	}

	rel = filepath.ToSlash(rel)

	if skip != nil && skip(rel, entry.IsDir()) {
		if entry.IsDir() {
			return "", false, filepath.SkipDir
		}

		return "", false, nil
	}

	if !entry.Type().IsRegular() {
		return "", false, nil
	}

	sum, err := File(path)
	if err != nil {
		return "", false, err
	}

	return rel + "\x00" + sum.String() + "\n", true, nil
}

// Files hashes an in-memory tree with the same algorithm as Tree: keys are used
// verbatim as slash-separated relative paths and their values are hashed.
func Files(files map[string][]byte) Hash {
	lines := make([]string, 0, len(files))

	for rel, data := range files {
		lines = append(lines, rel+"\x00"+Bytes(data).String()+"\n")
	}

	slices.Sort(lines)

	return Bytes([]byte(strings.Join(lines, "")))
}
