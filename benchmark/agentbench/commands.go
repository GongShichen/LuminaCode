package agentbench

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"

	bashpkg "LuminaCode/tools/bash"
)

func RunShellCommand(ctx context.Context, dir string, command string, timeout time.Duration) CommandResult {
	if timeout <= 0 {
		timeout = time.Duration(DefaultCaseTimeout) * time.Second
	}
	start := time.Now()
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := bashpkg.ShellArgv(command, "")
	cmd := exec.CommandContext(cmdCtx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	result := CommandResult{
		Command: command,
	}
	err := cmd.Run()
	result.DurationSecond = time.Since(start).Seconds()
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	if cmdCtx.Err() != nil {
		result.TimedOut = errors.Is(cmdCtx.Err(), context.DeadlineExceeded)
	}
	if err != nil {
		result.Error = err.Error()
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = 1
		}
		return result
	}
	result.ExitCode = 0
	return result
}

func commandsPassed(results []CommandResult) bool {
	for _, result := range results {
		if result.ExitCode != 0 || result.TimedOut {
			return false
		}
	}
	return true
}
