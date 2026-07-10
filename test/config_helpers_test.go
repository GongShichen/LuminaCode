package test

import (
	"path/filepath"
	"testing"

	"LuminaCode/config"
)

func isolatedConfig(t *testing.T) config.Config {
	t.Helper()
	return isolatedConfigForCWD(t, t.TempDir())
}

func isolatedConfigForCWD(t *testing.T, cwd string) config.Config {
	t.Helper()
	t.Setenv("HOME", filepath.Join(cwd, "home"))
	cfg := config.NewConfigForCWD(cwd)
	cfg.CWD = cwd
	cfg.SessionDir = filepath.Join(cwd, "sessions")
	// Tests that exercise query expansion opt in explicitly and provide a
	// dedicated expansion-model response. Other tests keep their API mock
	// focused on the main agent request.
	cfg.MemoryQueryExpansionEnabled = false
	return cfg
}
