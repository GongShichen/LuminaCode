package bash

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func ShellArgv(command, executable string) []string {
	if executable != "" {
		name := strings.ToLower(filepath.Base(executable))
		if name == "cmd" || name == "cmd.exe" {
			return []string{executable, "/C", command}
		}
		if strings.HasPrefix(name, "powershell") || name == "pwsh" || name == "pwsh.exe" {
			return []string{executable, "-NoProfile", "-Command", command}
		}
		return []string{executable, "-c", command}
	}
	if runtime.GOOS == "windows" {
		if shell := findWindowsPOSIXShell(); shell != "" {
			name := strings.ToLower(filepath.Base(shell))
			if strings.HasPrefix(name, "bash") {
				return []string{shell, "-o", "pipefail", "-c", command}
			}
			return []string{shell, "-c", command}
		}
		comspec := os.Getenv("COMSPEC")
		if comspec == "" {
			comspec = "cmd.exe"
		}
		return []string{comspec, "/C", command}
	}
	if _, err := os.Stat("/bin/bash"); err == nil {
		return []string{"/bin/bash", "-o", "pipefail", "-c", command}
	}
	if path, err := exec.LookPath("bash"); err == nil {
		return []string{path, "-o", "pipefail", "-c", command}
	}
	if shell := os.Getenv("SHELL"); shell != "" {
		return []string{shell, "-c", command}
	}
	return []string{"/bin/sh", "-c", command}
}

func findWindowsPOSIXShell() string {
	candidates := []string{}
	if git, err := exec.LookPath("git"); err == nil && git != "" {
		root := filepath.Dir(filepath.Dir(git))
		candidates = append(candidates,
			filepath.Join(root, "bin", "bash.exe"),
			filepath.Join(root, "usr", "bin", "bash.exe"),
			filepath.Join(root, "bin", "sh.exe"),
			filepath.Join(root, "usr", "bin", "sh.exe"),
		)
	}
	candidates = append(candidates,
		filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "Git", "usr", "bin", "bash.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "sh.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "Git", "usr", "bin", "sh.exe"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Git", "bin", "bash.exe"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Git", "usr", "bin", "bash.exe"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Git", "bin", "sh.exe"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Git", "usr", "bin", "sh.exe"),
	)
	if shell := os.Getenv("SHELL"); shell != "" {
		candidates = append(candidates, shell)
	}
	if bash, err := exec.LookPath("bash"); err == nil && bash != "" {
		candidates = append(candidates, bash)
	}
	if sh, err := exec.LookPath("sh"); err == nil && sh != "" {
		candidates = append(candidates, sh)
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if isWindowsWSLShim(candidate) {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

func isWindowsWSLShim(path string) bool {
	cleaned := strings.ToLower(filepath.Clean(path))
	return strings.HasSuffix(cleaned, `\wsl.exe`) ||
		strings.HasSuffix(cleaned, `\bash.exe`) && (strings.Contains(cleaned, `\windows\system32\`) || strings.Contains(cleaned, `\windows\sysnative\`))
}
