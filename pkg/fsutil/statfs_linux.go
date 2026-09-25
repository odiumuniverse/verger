//go:build linux

package fsutil

import (
	"fmt"
	"syscall"
)

const (
	exfatSuperMagic = 0x2011bab0
	msdosSuperMagic = 0x4d44
)

// FSType returns the filesystem type name of the directory, or its hexadecimal
// statfs magic when the type has no name mapping.
func FSType(dir string) (string, error) {
	var st syscall.Statfs_t

	if err := syscall.Statfs(dir, &st); err != nil {
		return "", err
	}

	switch st.Type {
	case exfatSuperMagic:
		return "exfat", nil
	case msdosSuperMagic:
		return "msdos", nil
	default:
		return fmt.Sprintf("0x%x", st.Type), nil
	}
}
