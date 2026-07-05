//go:build !darwin && !freebsd && !netbsd && !openbsd && !linux

package memory

import (
	"os"
	"time"
)

func fileAccessTime(os.FileInfo) time.Time {
	return time.Time{}
}
