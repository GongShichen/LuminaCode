//go:build linux

package memory

import (
	"os"
	"syscall"
	"time"
)

func fileAccessTime(info os.FileInfo) time.Time {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}
	}
	return time.Unix(stat.Atim.Sec, stat.Atim.Nsec)
}
