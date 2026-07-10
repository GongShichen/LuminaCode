package bash

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

type SandboxConfig struct {
	Enabled        bool
	AllowNetwork   bool
	AllowWrite     []string
	AllowRead      []string
	AllowProcesses bool
}

type SandboxManager struct {
	enabled          bool
	platform         string
	sandboxAvailable bool
}

func NewSandboxManager() *SandboxManager {
	manager := &SandboxManager{
		enabled:  true,
		platform: runtime.GOOS,
	}
	manager.sandboxAvailable = manager.detectSandbox()
	return manager
}

func (m *SandboxManager) detectSandbox() bool {
	if m == nil {
		return false
	}
	_, err := exec.LookPath(m.BackendName())
	return err == nil
}

func (m *SandboxManager) Platform() string {
	if m == nil {
		return runtime.GOOS
	}
	return m.platform
}

func (m *SandboxManager) BackendName() string {
	switch m.Platform() {
	case "darwin":
		return "sandbox-exec"
	case "linux":
		return "bwrap"
	default:
		return ""
	}
}

func (m *SandboxManager) IsSandboxAvailable() bool {
	return m != nil && m.sandboxAvailable
}

func (m *SandboxManager) IsSandboxingEnabled() bool {
	return m != nil && m.enabled && m.sandboxAvailable
}

func (m *SandboxManager) Disable() {
	m.enabled = false
}

func (m *SandboxManager) Enable() {
	m.enabled = true
}

func (m *SandboxManager) GetSandboxCommand(command string, config SandboxConfig, cwd string) []string {
	if !config.Enabled || !m.IsSandboxingEnabled() {
		return nil
	}
	switch m.platform {
	case "darwin":
		return m.macosSandbox(command, config, cwd)
	case "linux":
		return m.linuxSandbox(command, config, cwd)
	default:
		return nil
	}
}

func (m *SandboxManager) macosSandbox(command string, config SandboxConfig, cwd string) []string {
	profile := m.buildMacosProfile(config, cwd)
	env := []string{
		"/usr/bin/env", "-i",
		"HOME=" + cwd,
		"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"TMPDIR=/private/tmp",
		"LANG=C.UTF-8",
	}
	args := append([]string{"sandbox-exec", "-p", profile}, env...)
	return append(args, ShellCommandArgs(command)...)
}

func (m *SandboxManager) buildMacosProfile(config SandboxConfig, cwd string) string {
	readRoots := sandboxRoots(append(append([]string{}, config.AllowRead...), cwd))
	writeRoots := sandboxRoots(append(append([]string{}, config.AllowWrite...), cwd))

	lines := []string{
		"(version 1)",
		"(deny default)",
		`(import "system.sb")`,
		"(allow process-exec process-fork signal)",
		"(allow file-read* file-test-existence file-map-executable",
	}
	for _, path := range existingSandboxPaths(
		"/usr/bin", "/bin", "/usr/sbin", "/sbin", "/usr/local", "/opt/homebrew",
		"/Library/Developer", "/Applications/Xcode.app/Contents/Developer", "/private/etc",
		"/tmp", "/private/tmp",
	) {
		lines = append(lines, "  (subpath "+sbplString(path)+")")
	}
	for _, path := range readRoots {
		lines = append(lines, "  (path-ancestors "+sbplString(path)+")")
		lines = append(lines, "  (subpath "+sbplString(path)+")")
	}
	lines = append(lines, ")")

	if len(writeRoots) > 0 {
		lines = append(lines, "(allow file-write*")
		for _, path := range writeRoots {
			lines = append(lines, "  (subpath "+sbplString(path)+")")
		}
		for _, path := range existingSandboxPaths("/tmp", "/private/tmp") {
			lines = append(lines, "  (subpath "+sbplString(path)+")")
		}
		lines = append(lines, ")")
	}
	if config.AllowNetwork {
		lines = append(lines, "(allow network*)")
	}
	return strings.Join(lines, "\n")
}

func (m *SandboxManager) linuxSandbox(command string, config SandboxConfig, cwd string) []string {
	if roots := sandboxRoots([]string{cwd}); len(roots) == 1 {
		cwd = roots[0]
	}
	args := []string{"bwrap", "--unshare-all", "--clearenv", "--new-session", "--die-with-parent"}
	for _, path := range existingSandboxPaths("/usr", "/lib", "/lib64", "/bin", "/etc", "/opt", "/usr/local", "/opt/homebrew") {
		args = append(args, "--ro-bind", path, path)
	}
	args = append(args,
		"--bind", cwd, cwd,
		"--chdir", cwd,
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--setenv", "HOME", cwd,
		"--setenv", "PATH", "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"--setenv", "TMPDIR", "/tmp",
		"--setenv", "LANG", "C.UTF-8",
	)
	for _, path := range sandboxRoots(config.AllowRead) {
		if path != cwd && pathExists(path) {
			args = append(args, "--ro-bind", path, path)
		}
	}
	for _, path := range sandboxRoots(config.AllowWrite) {
		if path != cwd && pathExists(path) {
			args = append(args, "--bind", path, path)
		}
	}
	if config.AllowNetwork {
		args = append(args, "--share-net")
	}
	args = append(args, "--")
	return append(args, ShellCommandArgs(command)...)
}

func ShellCommandArgs(command string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/C", command}
	}
	if _, err := os.Stat("/bin/bash"); err == nil {
		return []string{"/bin/bash", "-o", "pipefail", "-c", command}
	}
	if path, err := exec.LookPath("bash"); err == nil {
		return []string{path, "-o", "pipefail", "-c", command}
	}
	return []string{"sh", "-c", command}
}

func ShouldUseSandbox(_ string, manager *SandboxManager, yolo bool, _ map[string]bool) bool {
	return manager != nil && manager.IsSandboxingEnabled() && !yolo
}

func sandboxRoots(paths []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		path = filepath.Clean(path)
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = filepath.Clean(resolved)
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func existingSandboxPaths(paths ...string) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if pathExists(path) {
			result = append(result, path)
		}
	}
	return result
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func sbplString(value string) string {
	return strconv.Quote(value)
}
