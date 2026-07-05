package test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"LuminaCode/memory"
)

func TestResolveMemoryDirectoryPriorityAndLuminaDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LUMINA_COWORK_MEMORY_PATH_OVERRIDE", "  /forced/memory  ")
	setting := "/settings/memory/path"
	if got := memory.ResolveMemoryDirectory(t.TempDir(), &setting); got != "/forced/memory" {
		t.Fatalf("env override should beat setting and trim whitespace, got %q", got)
	}

	t.Setenv("LUMINA_COWORK_MEMORY_PATH_OVERRIDE", "")
	if got := memory.ResolveMemoryDirectory(t.TempDir(), &setting); got != filepath.Clean(setting) {
		t.Fatalf("absolute setting should be resolved, got %q", got)
	}

	cwd := filepath.Join(t.TempDir(), "agent", "LuminaCode")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	got := memory.ResolveMemoryDirectory(cwd, nil)
	wantPrefix := filepath.Join(home, ".Lumina", "projects") + string(filepath.Separator)
	if !strings.HasPrefix(got, wantPrefix) || filepath.Base(got) != "memory" {
		t.Fatalf("default memory directory should use Lumina project namespace, got %q prefix %q", got, wantPrefix)
	}
	legacyUserDir := ".Xx" + "Code"
	legacyProjectDir := ".xx" + "code"
	if strings.Contains(got, legacyUserDir) || strings.Contains(got, legacyProjectDir) {
		t.Fatalf("default memory directory should not use legacy Xx"+"Code paths: %q", got)
	}
}

func TestSanitizePathForPathKeepsDriveLetterLikePythonWithLuminaRename(t *testing.T) {
	got := memory.SanitizePathForPath("F:/agent/LuminaCode")
	if got != "F--agent-LuminaCode" {
		t.Fatalf("drive-letter path slug mismatch: %q", got)
	}
	unix := memory.SanitizePathForPath("/tmp/agent/LuminaCode")
	if !regexp.MustCompile(`tmp-agent-LuminaCode$`).MatchString(unix) {
		t.Fatalf("unexpected unix path slug: %q", unix)
	}
}

func TestEnsureMemoryDirectoryCreatesDirectoryLikePython(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", "nested", "memory")
	if got := memory.EnsureMemoryDirectory(path); got != path {
		t.Fatalf("ensure should return same path, got %q", got)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("memory directory not created: info=%#v err=%v", info, err)
	}
	if got := memory.EnsureMemoryDirectory(path); got != path {
		t.Fatalf("ensure should be idempotent, got %q", got)
	}
}
