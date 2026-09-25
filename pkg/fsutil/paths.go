package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExpandHome expands a leading "~" or "~/…" to the current user's home
// directory: "~" becomes the home itself and "~/x" becomes home/x. Every other
// string — including "" and "~user" — is returned unchanged. A home resolution
// failure is wrapped.
func ExpandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}

	if path == "~" {
		return home, nil
	}

	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}

// Exists reports whether path resolves to an existing file or directory.
// Broken symlinks do not exist.
func Exists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}
