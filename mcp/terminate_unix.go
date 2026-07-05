//go:build !windows

package mcp

import (
	"os"
	"syscall"
)

func terminateProcess(process *os.Process) error {
	if process == nil {
		return nil
	}
	return process.Signal(syscall.SIGTERM)
}
