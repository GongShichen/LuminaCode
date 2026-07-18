//go:build windows

package apppaths

import "os"

func chmodPath(string, os.FileMode) error {
	return nil
}

func chmodOpenFile(*os.File, os.FileMode) error {
	return nil
}
