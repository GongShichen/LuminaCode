package test

import (
	"path/filepath"
	"runtime"
	"testing"

	"LuminaCode/apppaths"
	"LuminaCode/config"
)

func isolatedConfig(t *testing.T) config.Config {
	t.Helper()
	return isolatedConfigForCWD(t, t.TempDir())
}

func isolatedConfigForCWD(t *testing.T, cwd string) config.Config {
	t.Helper()
	t.Setenv("HOME", filepath.Join(cwd, "home"))
	t.Setenv("LUMINA_APP_ROOT", "")
	t.Setenv("LUMINA_RESOURCE_ROOT", "")
	t.Setenv("LUMINA_HOME", "")
	cfg := config.NewConfigForCWD(cwd)
	cfg.CWD = cwd
	cfg.SessionDir = filepath.Join(cwd, "sessions")
	// Tests that exercise query expansion opt in explicitly and provide a
	// dedicated expansion-model response. Other tests keep their API mock
	// focused on the main agent request.
	return cfg
}

func initializeTestAppRoot(t *testing.T, home string) apppaths.AppPaths {
	t.Helper()
	paths, err := apppaths.Resolve(apppaths.ResolveOptions{
		GOOS: runtime.GOOS, HomeDir: home, Env: map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := apppaths.WriteLayout(paths, "test"); err != nil {
		t.Fatal(err)
	}
	return paths
}
