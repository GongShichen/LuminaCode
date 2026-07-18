//go:build !windows

package apppaths

import "os"

func chmodPath(path string, mode os.FileMode) error {
	return os.Chmod(path, mode)
}

func chmodOpenFile(file *os.File, mode os.FileMode) error {
	return file.Chmod(mode)
}
