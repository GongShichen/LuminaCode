package agentContext

import (
	"os"
	"path/filepath"
	"testing"

	"LuminaCode/apppaths"
	"LuminaCode/config"
)

func TestSystemPromptResourcePrecedence(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	configured := filepath.Join(root, "configured.md")
	writePromptFile(t, configured, "configured")

	cfg := config.Config{SystemPromptPath: configured}
	sections, path, err := loadSystemPromptTemplateSectionsWithConfig(project, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !sameTestFile(path, configured) || len(sections) != 1 || sections[0].Content != "configured" {
		t.Fatalf("configured prompt not selected: path=%q sections=%#v", path, sections)
	}

	projectPrompt := apppaths.ProjectSystemPrompt(project)
	writePromptFile(t, projectPrompt, "project")
	sections, path, err = loadSystemPromptTemplateSectionsWithConfig(project, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !sameTestFile(path, projectPrompt) || len(sections) != 1 || sections[0].Content != "project" {
		t.Fatalf("project prompt should override configured prompt: path=%q sections=%#v", path, sections)
	}
}

func sameTestFile(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

func writePromptFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[SECTION: test]\n"+content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
