//go:build !windows

package apppaths

import (
	"os"
	"path/filepath"
	"syscall"
)

func availableDiskBytes(path string) (uint64, error) {
	current := path
	for {
		if _, err := os.Stat(current); err == nil {
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(current, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}
