//go:build !darwin && !freebsd && !netbsd && !openbsd && !linux && !windows

package memory

import (
	"os"
	"time"
)

func fileAccessTime(os.FileInfo) time.Time {
	return time.Time{}
}
