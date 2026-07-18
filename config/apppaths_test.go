package config

import (
	"os"
	"path/filepath"
	"testing"

	"LuminaCode/apppaths"
)

func TestAppRootV2ConfigPrecedence(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LUMINA_APP_ROOT", root)
	t.Setenv("LUMINA_RESOURCE_ROOT", "")
	t.Setenv("LUMINA_HOME", "")
	t.Setenv("LUMINA_API_MODEL", "")
	t.Setenv("LLM_DEFAULT_MODEL", "")
	t.Setenv("ANTHROPIC_MODEL", "")

	paths, err := apppaths.ResolveCurrent()
	if err != nil {
		t.Fatal(err)
	}
	writeConfigFixture(t, paths.SettingsFile, `{"api_model":"user","session_dir":"~/.lumina/sessions"}`)
	writeConfigFixture(t, apppaths.ProjectDefaultsFile(project), `{"api_model":"project"}`)

	cfg := NewConfigForCWD(project)
	if cfg.APIModel != "project" {
		t.Fatalf("project defaults should override user settings, got %q", cfg.APIModel)
	}
	if filepath.Clean(cfg.SessionDir) != filepath.Clean(paths.ActiveSessionsDir) {
		t.Fatalf("legacy default session path should be removed, got %q", cfg.SessionDir)
	}

	t.Setenv("LUMINA_API_MODEL", "environment")
	cfg = NewConfigForCWD(project)
	if cfg.APIModel != "environment" {
		t.Fatalf("environment should override project settings, got %q", cfg.APIModel)
	}
}

func TestAppPathsRemainStableWhenResourcesAreOverridden(t *testing.T) {
	root := t.TempDir()
	resources := filepath.Join(t.TempDir(), "resources")
	writeConfigFixture(t, filepath.Join(resources, "system", "system-prompt.md"), "[SECTION: test]\nresource\n")
	t.Setenv("LUMINA_APP_ROOT", root)
	t.Setenv("LUMINA_RESOURCE_ROOT", resources)
	t.Setenv("LUMINA_HOME", "")

	cfg := NewConfigForCWD(t.TempDir())
	if filepath.Clean(cfg.Paths.ResourcesDir) != filepath.Join(root, "app", "resources") {
		t.Fatalf("resource override changed the AppRoot contract: %q", cfg.Paths.ResourcesDir)
	}
	if filepath.Clean(cfg.SystemPromptPath) != filepath.Join(resources, "system", "system-prompt.md") {
		t.Fatalf("resource override was not applied to bundled resources: %q", cfg.SystemPromptPath)
	}
}

func TestCustomStoragePathDoesNotMutateAppPathContract(t *testing.T) {
	root := t.TempDir()
	customSessions := filepath.Join(t.TempDir(), "custom-sessions")
	t.Setenv("LUMINA_APP_ROOT", root)
	t.Setenv("LUMINA_RESOURCE_ROOT", "")
	t.Setenv("LUMINA_HOME", "")
	writeConfigFixture(t, filepath.Join(root, "config", "settings.json"), `{"session_dir":"`+filepath.ToSlash(customSessions)+`"}`)

	cfg := NewConfigForCWD(t.TempDir())
	if filepath.Clean(cfg.SessionDir) != filepath.Clean(customSessions) {
		t.Fatalf("custom session path was not preserved: %q", cfg.SessionDir)
	}
	if filepath.Clean(cfg.Paths.ActiveSessionsDir) != filepath.Join(root, "data", "sessions", "active") {
		t.Fatalf("custom setting mutated AppPaths: %q", cfg.Paths.ActiveSessionsDir)
	}
}

func TestNestedCWDUsesStableProjectIdentity(t *testing.T) {
	appRoot := t.TempDir()
	projectRoot := filepath.Join(t.TempDir(), "repo")
	nested := filepath.Join(projectRoot, "src", "nested")
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LUMINA_APP_ROOT", appRoot)
	t.Setenv("LUMINA_RESOURCE_ROOT", "")
	t.Setenv("LUMINA_HOME", "")

	rootConfig := NewConfigForCWD(projectRoot)
	nestedConfig := NewConfigForCWD(nested)
	if rootConfig.ProjectPaths.ID != nestedConfig.ProjectPaths.ID || nestedConfig.ProjectPaths.CanonicalRoot != rootConfig.ProjectPaths.CanonicalRoot {
		t.Fatalf("nested cwd changed project identity: root=%#v nested=%#v", rootConfig.ProjectPaths, nestedConfig.ProjectPaths)
	}
}

func writeConfigFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
