package team

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "lumina-team-test-home-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HOME", home)
	_ = os.Setenv("USERPROFILE", home)
	_ = os.Setenv("LOCALAPPDATA", home)
	_ = os.Unsetenv("LUMINA_APP_ROOT")
	_ = os.Unsetenv("LUMINA_RESOURCE_ROOT")
	_ = os.Unsetenv("LUMINA_HOME")

	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
