package bash

import (
	"os"
	"os/exec"
	"runtime"
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
	switch m.platform {
	case "darwin":
		// macOS sandbox-exec with a deny-default profile is brittle for shell
		// commands because even simple sh -c invocations may require system
		// service permissions beyond file/process rules. Keep Lumina's own
		// permission model in charge on macOS instead of silently killing tools.
		return false
	case "linux":
		_, err := exec.LookPath("bwrap")
		return err == nil
	default:
		return false
	}
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
	if !m.IsSandboxingEnabled() {
		return []string{"sh", "-c", command}
	}
	switch m.platform {
	case "darwin":
		return m.macosSandbox(command, config, cwd)
	case "linux":
		return m.linuxSandbox(command, config, cwd)
	default:
		return []string{"sh", "-c", command}
	}
}

func (m *SandboxManager) macosSandbox(command string, config SandboxConfig, cwd string) []string {
	profile := m.buildMacosProfile(config, cwd)
	return []string{"sandbox-exec", "-p", profile, "sh", "-c", command}
}

func (m *SandboxManager) buildMacosProfile(config SandboxConfig, cwd string) string {
	lines := []string{
		"(version 1)",
		"(deny default)",
		"(allow process-exec)",
		`(allow file-read* (subpath "/usr/lib")`,
		`             (subpath "/usr/share")`,
		`             (subpath "/System/Library")`,
		`             (subpath "/Library/Frameworks")`,
	}
	for _, path := range []string{"/usr/local", "/opt/homebrew", "/usr/bin", "/bin"} {
		if _, err := os.Stat(path); err == nil {
			lines = append(lines, `             (subpath "`+path+`")`)
		}
	}
	lines = append(lines, `             (subpath "`+cwd+`")`)
	for _, path := range []string{"/tmp", "/private/tmp"} {
		if _, err := os.Stat(path); err == nil {
			lines = append(lines, `             (subpath "`+path+`")`)
		}
	}
	for _, path := range config.AllowRead {
		lines = append(lines, `             (subpath "`+path+`")`)
	}
	lines = append(lines, ")")
	if len(config.AllowWrite) > 0 {
		lines = append(lines, "(allow file-write*")
		for _, path := range config.AllowWrite {
			lines = append(lines, `             (subpath "`+path+`")`)
		}
		lines = append(lines, `             (subpath "`+cwd+`")`)
		for _, path := range []string{"/tmp", "/private/tmp"} {
			if _, err := os.Stat(path); err == nil {
				lines = append(lines, `             (subpath "`+path+`")`)
			}
		}
		lines = append(lines, ")")
	}
	if config.AllowNetwork {
		lines = append(lines, "(allow network*)")
	}
	if config.AllowProcesses {
		lines = append(lines, "(allow process-fork)")
	}
	lines = append(lines, "(allow signal)")
	return strings.Join(lines, "\n")
}

func (m *SandboxManager) linuxSandbox(command string, config SandboxConfig, cwd string) []string {
	args := []string{
		"bwrap", "--unshare-all", "--clearenv", "--new-session", "--die-with-parent",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/lib64", "/lib64",
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/etc", "/etc",
		"--ro-bind", "/opt", "/opt",
		"--bind", cwd, cwd,
		"--chdir", cwd,
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
	}
	for _, path := range []string{"/usr/local", "/opt/homebrew"} {
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	for _, path := range config.AllowRead {
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	for _, path := range config.AllowWrite {
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--bind", path, path)
		}
	}
	if !config.AllowNetwork {
		args = append(args, "--unshare-net")
	}
	args = append(args, "--", "sh", "-c", command)
	return args
}

var sandboxExcludedCommands = map[string]bool{
	"docker": true, "podman": true, "kubectl": true, "systemctl": true,
	"launchctl": true, "brew": true, "apt": true, "apt-get": true,
	"yum": true, "dnf": true, "pacman": true, "snap": true,
	"flatpak": true, "nix": true, "guix": true, "ssh": true,
	"scp": true, "sftp": true, "rsync": true, "git": true,
}

func ShouldUseSandbox(command string, manager *SandboxManager, dangerouslyDisableSandbox bool, excludedCommands map[string]bool) bool {
	if manager == nil || !manager.IsSandboxingEnabled() {
		return false
	}
	if dangerouslyDisableSandbox {
		return false
	}
	exclusions := sandboxExcludedCommands
	if excludedCommands != nil {
		exclusions = excludedCommands
	}
	base := ExtractBaseCommand(command)
	return base == "" || !exclusions[base]
}
