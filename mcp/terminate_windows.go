//go:build windows

package mcp

import "os"

func terminateProcess(process *os.Process) error {
	if process == nil {
		return nil
	}
	return process.Kill()
}
