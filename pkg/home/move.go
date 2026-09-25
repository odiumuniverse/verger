package home

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/odiumuniverse/verger/pkg/fsutil"
)

// move is the tree mover used by MoveHome; tests replace it to assert
// delegation and error propagation. It defaults to fsutil.MoveTree.
var move = fsutil.MoveTree

// MoveHome moves a whole home directory (portable + state) to to. The store is
// never touched. from must be an existing directory, to must not exist and its
// parent must exist; the move renames when possible and falls back to a staged
// copy across filesystems. No symlink is left at from.
func MoveHome(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	src, err := resolveMovePath(from)
	if err != nil {
		return &InvalidMoveError{From: from, To: to, Reason: "source: " + err.Error()}
	}

	dst, err := resolveMovePath(to)
	if err != nil {
		return &InvalidMoveError{From: from, To: to, Reason: "destination: " + err.Error()}
	}

	if src == dst {
		return &InvalidMoveError{From: from, To: to, Reason: "source and destination are the same"}
	}

	info, err := os.Lstat(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &SourceMissingError{Path: src}
		}

		return fmt.Errorf("stat source %s: %w", src, err)
	}

	if !info.IsDir() {
		return &InvalidMoveError{From: from, To: to, Reason: "source is not a directory"}
	}

	if _, err := os.Lstat(dst); err == nil {
		return &DestinationExistsError{Path: dst}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat destination %s: %w", dst, err)
	}

	parent, err := os.Stat(filepath.Dir(dst))
	if err != nil || !parent.IsDir() {
		return &InvalidMoveError{From: from, To: to, Reason: "destination parent is missing"}
	}

	return move(ctx, src, dst)
}

// resolveMovePath expands a tilde and makes one move argument absolute; the
// caller attaches the endpoint names to InvalidMoveError.
func resolveMovePath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("path is empty")
	}

	expanded, err := fsutil.ExpandHome(value)
	if err != nil {
		return "", err
	}

	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}

	return abs, nil
}

// SourceMissingError reports a MoveHome source that does not exist.
type SourceMissingError struct {
	Path string
}

// Error implements error.
func (e *SourceMissingError) Error() string {
	return "home source is missing: " + e.Path
}

// DestinationExistsError reports a MoveHome destination that already exists.
type DestinationExistsError struct {
	Path string
}

// Error implements error.
func (e *DestinationExistsError) Error() string {
	return "home destination already exists: " + e.Path
}

// InvalidMoveError reports a MoveHome that cannot start.
type InvalidMoveError struct {
	From   string
	To     string
	Reason string
}

// Error implements error.
func (e *InvalidMoveError) Error() string {
	if e.To == "" {
		return fmt.Sprintf("invalid home move from %q: %s", e.From, e.Reason)
	}

	return fmt.Sprintf("invalid home move %q -> %q: %s", e.From, e.To, e.Reason)
}
