//go:build windows

package bash

import (
	"os/exec"
	"strconv"
)

func prepareProcessGroupForPlatform(_ *exec.Cmd) {}

func terminateProcessTreeForPlatform(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
}
