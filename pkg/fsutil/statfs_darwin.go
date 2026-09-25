//go:build darwin

package fsutil

import (
	"strings"
	"syscall"
)

// FSType returns the filesystem type name of the directory.
func FSType(dir string) (string, error) {
	var st syscall.Statfs_t

	if err := syscall.Statfs(dir, &st); err != nil {
		return "", err
	}

	return fstypeName(st.Fstypename[:]), nil
}

func fstypeName(name []int8) string {
	var b strings.Builder

	for _, c := range name {
		if c == 0 {
			break
		}

		b.WriteRune(rune(c))
	}

	return b.String()
}
