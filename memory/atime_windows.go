//go:build windows

package memory

import (
	"os"
	"syscall"
	"time"
)

func fileAccessTime(info os.FileInfo) time.Time {
	stat, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return time.Time{}
	}
	return time.Unix(0, stat.LastAccessTime.Nanoseconds())
}
