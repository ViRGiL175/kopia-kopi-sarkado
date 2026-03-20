//go:build !windows

package storage

import (
	"fmt"
	"syscall"
)

func FreeBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("statfs %q: %w", path, err)
	}

	return int64(stat.Bavail) * int64(stat.Bsize), nil
}
