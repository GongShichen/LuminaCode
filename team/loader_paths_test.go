package team

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"LuminaCode/apppaths"
	"LuminaCode/config"
)

func TestTeamResourcePrecedence(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	user := filepath.Join(root, "user")
	bundled := filepath.Join(root, "bundled")
	writeTeamFixture(t, bundled, "sample", "bundled")
	writeTeamFixture(t, user, "sample", "user")
	writeTeamFixture(t, apppaths.ProjectTeamsDir(project), "sample", "project")

	loader := NewLoader(config.Config{
		CWD:            filepath.Join(project, "nested"),
		TeamDir:        user,
		BundledTeamDir: bundled,
	})
	spec, err := loader.Load("sample")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Description != "project" {
		t.Fatalf("project team should win, got %q", spec.Description)
	}

	if err := os.RemoveAll(apppaths.ProjectTeamsDir(project)); err != nil {
		t.Fatal(err)
	}
	spec, err = loader.Load("sample")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Description != "user" {
		t.Fatalf("user team should override bundled team, got %q", spec.Description)
	}
}

func writeTeamFixture(t *testing.T, root, name, description string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "leader"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		TeamConfigFile:                                 fmt.Sprintf("name: %s\ndescription: %s\nentry_agent: leader\nagents:\n  - leader\n", name, description),
		TeamSystemFile:                                 "team system",
		CompletionPolicyFile:                           "complete",
		filepath.Join("leader", AgentConfigFile):       "name: leader\n",
		filepath.Join("leader", AgentSystemPromptFile): "leader prompt",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
