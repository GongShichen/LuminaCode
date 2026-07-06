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
	cfg := config.NewConfigForCWD(cwd)
	cfg.CWD = cwd
	cfg.SessionDir = filepath.Join(cwd, "sessions")
	return cfg
}
