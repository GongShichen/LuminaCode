//go:build windows

package apppaths

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
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
	pointer, err := windows.UTF16PtrFromString(current)
	if err != nil {
		return 0, err
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(pointer, &available, nil, nil); err != nil {
		return 0, err
	}
	return available, nil
}
