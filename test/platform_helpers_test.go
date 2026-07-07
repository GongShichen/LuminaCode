package test

import (
	"path/filepath"
	"runtime"
	"strings"

	bashpkg "LuminaCode/tools/bash"
)

func shellPath(path string) string {
	if runtime.GOOS == "windows" {
		return filepath.ToSlash(path)
	}
	return path
}

func shellCommandArgv(command string) (string, []string) {
	argv := bashpkg.ShellArgv(command, "")
	return argv[0], argv[1:]
}

func testBinaryPath(path string) string {
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(path), ".exe") {
		return path + ".exe"
	}
	return path
}

func pythonCommandName() string {
	if runtime.GOOS == "windows" {
		return "python"
	}
	return "python3"
}
