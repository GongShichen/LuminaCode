package bash

import (
	"os"
	"os/exec"
	pathpkg "path"
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

type SandboxOptions struct {
	Backend               string
	WSLDistro             string
	WSLInstallDir         string
	WSLImageURL           string
	WSLImagePath          string
	WSLImageSHA256        string
	WSLAllowLocalFallback bool
}

type SandboxManager struct {
	enabled          bool
	platform         string
	sandboxAvailable bool
	options          SandboxOptions
}

func NewSandboxManager() *SandboxManager {
	manager := &SandboxManager{
		enabled:  true,
		platform: runtime.GOOS,
		options:  SandboxOptions{Backend: "auto", WSLDistro: "LuminaSandbox", WSLAllowLocalFallback: true},
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
	if m == nil {
		switch runtime.GOOS {
		case "darwin":
			return "sandbox-exec"
		case "linux":
			return "bwrap"
		default:
			return ""
		}
	}
	switch m.effectiveBackend() {
	case "macos":
		return "sandbox-exec"
	case "local-bwrap":
		return "bwrap"
	case "wsl-bwrap":
		return "wsl.exe"
	default:
		return ""
	}
}

func (m *SandboxManager) IsSandboxAvailable() bool {
	return m != nil && m.sandboxAvailable
}

func (m *SandboxManager) IsSandboxingEnabled() bool {
	if m == nil || !m.enabled {
		return false
	}
	switch m.effectiveBackend() {
	case "macos":
		return m.sandboxAvailable
	case "local-bwrap":
		return m.sandboxAvailable
	case "wsl-bwrap":
		return m.platform == "windows" && m.wslSandboxReady()
	default:
		return false
	}
}

func (m *SandboxManager) Disable() {
	m.enabled = false
}

func (m *SandboxManager) Enable() {
	m.enabled = true
}

func (m *SandboxManager) Configure(options SandboxOptions) {
	if m == nil {
		return
	}
	if options.Backend != "" {
		m.options.Backend = normalizeSandboxBackend(options.Backend)
	}
	if strings.TrimSpace(options.WSLDistro) != "" {
		m.options.WSLDistro = strings.TrimSpace(options.WSLDistro)
	}
	if strings.TrimSpace(options.WSLInstallDir) != "" {
		m.options.WSLInstallDir = strings.TrimSpace(options.WSLInstallDir)
	}
	if strings.TrimSpace(options.WSLImageURL) != "" {
		m.options.WSLImageURL = strings.TrimSpace(options.WSLImageURL)
	}
	if strings.TrimSpace(options.WSLImagePath) != "" {
		m.options.WSLImagePath = strings.TrimSpace(options.WSLImagePath)
	}
	if strings.TrimSpace(options.WSLImageSHA256) != "" {
		m.options.WSLImageSHA256 = strings.ToLower(strings.TrimSpace(options.WSLImageSHA256))
	}
	m.options.WSLAllowLocalFallback = options.WSLAllowLocalFallback
}

func (m *SandboxManager) GetSandboxCommand(command string, config SandboxConfig, cwd string) []string {
	if !config.Enabled || !m.IsSandboxingEnabled() {
		return nil
	}
	switch m.effectiveBackend() {
	case "macos":
		return m.macosSandbox(command, config, cwd)
	case "local-bwrap":
		return m.linuxSandbox(command, config, cwd)
	case "wsl-bwrap":
		return m.wslSandbox(command, config, cwd)
	default:
		return nil
	}
}

func (m *SandboxManager) NeedsSetup(command string, dangerouslyDisableSandbox bool) bool {
	if m == nil || !m.enabled || dangerouslyDisableSandbox {
		return false
	}
	if m.effectiveBackend() != "wsl-bwrap" || m.platform != "windows" {
		return false
	}
	if IsSandboxExcludedCommand(command, nil) {
		return false
	}
	return !m.wslSandboxReady()
}

func (m *SandboxManager) AllowLocalFallback() bool {
	return m == nil || m.options.WSLAllowLocalFallback
}

func (m *SandboxManager) WSLDistro() string {
	if m == nil || strings.TrimSpace(m.options.WSLDistro) == "" {
		return "LuminaSandbox"
	}
	return strings.TrimSpace(m.options.WSLDistro)
}

func (m *SandboxManager) effectiveBackend() string {
	if m == nil {
		return "none"
	}
	backend := normalizeSandboxBackend(m.options.Backend)
	if backend == "auto" {
		switch m.platform {
		case "linux":
			return "local-bwrap"
		case "windows":
			return "wsl-bwrap"
		case "darwin":
			return "macos"
		default:
			return "none"
		}
	}
	return backend
}

func normalizeSandboxBackend(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "_", "-")))
	switch normalized {
	case "", "auto":
		return "auto"
	case "none", "off", "disabled":
		return "none"
	case "macos", "sandbox-exec":
		return "macos"
	case "local-bwrap", "bwrap", "bubblewrap":
		return "local-bwrap"
	case "wsl-bwrap", "wsl2-bwrap", "wsl":
		return "wsl-bwrap"
	default:
		return "auto"
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
	return linuxSandboxArgs(command, config, cwd, func(path string) bool {
		return pathExists(path)
	})
}

func linuxSandboxArgs(command string, config SandboxConfig, cwd string, exists func(string) bool) []string {
	if exists == nil {
		exists = pathExists
	}
	if roots := linuxSandboxRoots([]string{cwd}); len(roots) == 1 {
		cwd = roots[0]
	}
	args := []string{"bwrap", "--unshare-all", "--clearenv", "--new-session", "--die-with-parent"}
	for _, path := range []string{"/usr", "/lib", "/lib64", "/bin", "/etc", "/opt", "/usr/local", "/opt/homebrew"} {
		args = append(args, "--ro-bind-try", path, path)
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
	for _, path := range linuxSandboxRoots(config.AllowRead) {
		if path != cwd && exists(path) {
			args = append(args, "--ro-bind", path, path)
		}
	}
	for _, path := range linuxSandboxRoots(config.AllowWrite) {
		if path != cwd && exists(path) {
			args = append(args, "--bind", path, path)
		}
	}
	if config.AllowNetwork {
		args = append(args, "--share-net")
	}
	args = append(args, "--")
	args = append(args, LinuxShellCommandArgs(command)...)
	return args
}

func linuxSandboxRoots(paths []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(paths))
	for _, value := range paths {
		value = strings.TrimSpace(filepath.ToSlash(value))
		if value == "" {
			continue
		}
		if !strings.HasPrefix(value, "/") {
			if abs, err := filepath.Abs(value); err == nil {
				value = filepath.ToSlash(abs)
			}
		}
		value = pathpkg.Clean(value)
		if runtime.GOOS != "windows" {
			if resolved, err := filepath.EvalSymlinks(value); err == nil {
				value = filepath.ToSlash(filepath.Clean(resolved))
			}
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func ShellCommandArgs(command string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/C", command}
	}
	return LinuxShellCommandArgs(command)
}

func LinuxShellCommandArgs(command string) []string {
	if runtime.GOOS == "windows" {
		return []string{"/usr/bin/bash", "-o", "pipefail", "-c", command}
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

var sandboxExcludedCommands = map[string]bool{
	"docker": true, "podman": true, "kubectl": true, "systemctl": true,
	"launchctl": true, "brew": true, "apt": true, "apt-get": true,
	"yum": true, "dnf": true, "pacman": true, "snap": true,
	"flatpak": true, "nix": true, "guix": true, "ssh": true,
	"scp": true, "sftp": true, "rsync": true, "git": true,
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

func IsSandboxExcludedCommand(command string, excludedCommands map[string]bool) bool {
	exclusions := sandboxExcludedCommands
	if excludedCommands != nil {
		exclusions = excludedCommands
	}
	base := ExtractBaseCommand(command)
	return base != "" && exclusions[base]
}
