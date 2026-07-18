package test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	goModCache := goEnvironmentValue("GOMODCACHE")
	goBuildCache := goEnvironmentValue("GOCACHE")
	home, err := os.MkdirTemp("", "lumina-test-home-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HOME", home)
	_ = os.Setenv("USERPROFILE", home)
	_ = os.Setenv("LOCALAPPDATA", home)
	_ = os.Unsetenv("LUMINA_APP_ROOT")
	_ = os.Unsetenv("LUMINA_RESOURCE_ROOT")
	_ = os.Unsetenv("LUMINA_HOME")
	if goModCache != "" {
		_ = os.Setenv("GOMODCACHE", goModCache)
	}
	if goBuildCache != "" {
		_ = os.Setenv("GOCACHE", goBuildCache)
	}

	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

func goEnvironmentValue(name string) string {
	output, err := exec.Command("go", "env", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}
