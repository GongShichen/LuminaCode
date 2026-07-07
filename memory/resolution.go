package memory

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func SanitizePathForPath(path string) string {
	candidate := expandMemoryHome(path)
	candidate = filepath.Clean(candidate)
	if !filepath.IsAbs(candidate) && !regexp.MustCompile(`^[A-Za-z]:(?:[\\/]|$)`).MatchString(candidate) {
		if abs, err := filepath.Abs(candidate); err == nil {
			candidate = abs
		}
	}
	raw := filepath.ToSlash(candidate)
	drive := ""
	if regexp.MustCompile(`^[A-Za-z]:(?:/|$)`).MatchString(raw) {
		drive = raw[:1]
		raw = raw[2:]
	}
	raw = strings.TrimLeft(raw, "/")
	var parts []string
	for _, segment := range strings.Split(raw, "/") {
		if segment == "" || segment == "." {
			continue
		}
		part := sanitizePathComponent(segment)
		if part != "" {
			parts = append(parts, part)
		}
	}
	body := strings.Join(parts, "-")
	if body == "" {
		body = "root"
	}
	if drive != "" {
		return drive + "--" + body
	}
	return body
}

func expandMemoryHome(path string) string {
	if path == "~" {
		if home := memoryHomeDir(); home != "" {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home := memoryHomeDir(); home != "" {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func ResolveMemoryDirectory(configCWD string, autoMemoryDirectory *string) string {
	if override := strings.TrimSpace(os.Getenv("LUMINA_COWORK_MEMORY_PATH_OVERRIDE")); override != "" {
		return override
	}
	if autoMemoryDirectory != nil && *autoMemoryDirectory != "" {
		path := *autoMemoryDirectory
		if !isMemoryAbsolutePath(path) {
			path = filepath.Join(configCWD, path)
		}
		if isRootedMemoryPathWithoutDrive(path) {
			return filepath.Clean(path)
		}
		resolved, err := filepath.Abs(path)
		if err != nil {
			return path
		}
		return resolved
	}
	gitRoot := FindCanonicalGitRoot(configCWD)
	projectKey := ""
	if gitRoot == "" {
		projectKey = SanitizePathForPath(configCWD)
	} else {
		projectKey = SanitizeGitRootForPath(gitRoot)
	}
	home := memoryHomeDir()
	return filepath.Join(home, ".Lumina", "projects", projectKey, "memory")
}

func memoryHomeDir() string {
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return home
	}
	home, _ := os.UserHomeDir()
	return home
}

func isMemoryAbsolutePath(path string) bool {
	if filepath.IsAbs(path) || strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\`) {
		return true
	}
	return regexp.MustCompile(`^[A-Za-z]:(?:[\\/]|$)`).MatchString(path)
}

func isRootedMemoryPathWithoutDrive(path string) bool {
	if path == "" || regexp.MustCompile(`^[A-Za-z]:(?:[\\/]|$)`).MatchString(path) {
		return false
	}
	return strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\`)
}

func EnsureMemoryDirectory(path string) string {
	_ = os.MkdirAll(path, 0o755)
	return path
}

func sanitizePathComponent(component string) string {
	cleaned := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(component, "-")
	cleaned = regexp.MustCompile(`-{2,}`).ReplaceAllString(cleaned, "-")
	return strings.Trim(cleaned, ".-_")
}
