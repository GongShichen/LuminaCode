package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"LuminaCode/agent"
	agentContext "LuminaCode/agentContext"
)

func TestBuildSystemPromptReadsLuminaSystemAndProjectInstructions(t *testing.T) {
	dir := t.TempDir()
	systemDir := filepath.Join(dir, ".Lumina", "SYSTEM")
	if err := os.MkdirAll(systemDir, 0o755); err != nil {
		t.Fatal(err)
	}
	template := `[SECTION: identity]
Identity from Lumina.

[SECTION: instruction-priority]
Priority mentions LUMINA.md.
`
	if err := os.WriteFile(filepath.Join(systemDir, "system-prompt.md"), []byte(template), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "LUMINA.md"), []byte("Workdir project instruction."), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := agent.BuildSystemPrompt(nested)
	if !strings.Contains(prompt, "Identity from Lumina.") ||
		!strings.Contains(prompt, "Priority mentions LUMINA.md.") ||
		!strings.Contains(prompt, "Workdir project instruction.") {
		t.Fatalf("unexpected system prompt: %q", prompt)
	}
}

func TestBuildSystemPromptUsesLuminaSystemFromProvidedCWD(t *testing.T) {
	dir := t.TempDir()
	systemDir := filepath.Join(dir, ".Lumina", "SYSTEM")
	if err := os.MkdirAll(systemDir, 0o755); err != nil {
		t.Fatal(err)
	}
	template := `[SECTION: identity]
Provided cwd system prompt.
`
	if err := os.WriteFile(filepath.Join(systemDir, "system-prompt.md"), []byte(template), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "pkg", "feature")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	prompt := agent.BuildSystemPrompt(nested)
	if !strings.Contains(prompt, "Provided cwd system prompt.") ||
		!strings.Contains(prompt, "## Environment") {
		t.Fatalf("system prompt should be built from provided cwd Lumina assets, got %q", prompt)
	}
}

func TestAgentContextDoesNotReadParentLuminaProjectInstructions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, "LUMINA.md"), []byte("Only root instructions."), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := agentContext.LoadProjectInstructions(nested)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("unexpected project instructions: %q", got)
	}
}

func TestAgentContextProjectInstructionsReplaceInvalidUTF8LikePython(t *testing.T) {
	dir := t.TempDir()
	content := []byte("Project ")
	content = append(content, 0xff, 'r', 'u', 'l', 'e')
	if err := os.WriteFile(filepath.Join(dir, "LUMINA.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := agentContext.LoadProjectInstructions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Project \uFFFDrule" {
		t.Fatalf("expected invalid UTF-8 to be replaced like Python, got %q", got)
	}
}

func TestAgentContextLoadsOnlyWorkingDirectoryLuminaProjectInstructions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, "LUMINA.md"), []byte("Root instructions."), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "pkg", "feature")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "LUMINA.md"), []byte("Package instructions."), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := agentContext.LoadProjectInstructions(filepath.Join(dir, "pkg"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "Package instructions." {
		t.Fatalf("unexpected working directory instructions: %q", got)
	}

	got, err = agentContext.LoadProjectInstructions(nested)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("nested child should not inherit parent instructions, got %q", got)
	}
}

func TestAgentContextLoadsHierarchicalProjectInstructionsWithinGitRoot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "LUMINA.md"), []byte("Root instructions."), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(dir, "pkg")
	nested := filepath.Join(pkg, "feature")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "AGENTS.md"), []byte("Package instructions."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "LUMINA.md"), []byte("Nested instructions."), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := agentContext.LoadProjectInstructions(nested)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"Root instructions.",
		"Package instructions.",
		"Nested instructions.",
	}, "\n\n---\n\n")
	if got != want {
		t.Fatalf("unexpected hierarchical instructions:\nwant %q\n got %q", want, got)
	}
}

func TestAgentContextDoesNotReadAboveGitRoot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, "LUMINA.md"), []byte("Outside instructions."), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "repo")
	nested := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("Repo instructions."), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := agentContext.LoadProjectInstructions(nested)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Repo instructions." {
		t.Fatalf("should not read instructions above .git root, got %q", got)
	}
}

func TestAgentContextLoadsV2UserInstructionsWhenWorkdirMissing(t *testing.T) {
	dir := t.TempDir()
	appRoot := setTestAppRoot(t)
	instructionsDir := filepath.Join(appRoot, "config", "instructions")
	if err := os.MkdirAll(instructionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instructionsDir, "LUMINA.md"), []byte("Home fallback."), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := agentContext.LoadProjectInstructions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Home fallback." {
		t.Fatalf("unexpected home fallback instructions: %q", got)
	}
}

func TestAgentContextLoadsAgentsCompatibilityFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Agents instructions."), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := agentContext.LoadProjectInstructions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Agents instructions." {
		t.Fatalf("expected AGENTS.md compatibility file, got %q", got)
	}
}

func TestAgentContextPrefersLuminaOverAgentsPrimaryFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "LUMINA.md"), []byte("Primary instructions."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Fallback instructions."), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := agentContext.LoadProjectInstructions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Primary instructions." {
		t.Fatalf("expected LUMINA.md to win over fallback, got %q", got)
	}
}

func TestAgentContextPrefersLuminaOverAgentsAtEachHierarchyLevel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "LUMINA.md"), []byte("Root primary."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Root fallback."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "LUMINA.md"), []byte("Nested primary."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "AGENTS.md"), []byte("Nested fallback."), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := agentContext.LoadProjectInstructions(nested)
	if err != nil {
		t.Fatal(err)
	}
	want := "Root primary.\n\n---\n\nNested primary."
	if got != want {
		t.Fatalf("expected LUMINA.md to win at each hierarchy level:\nwant %q\n got %q", want, got)
	}
}

func TestAgentContextMissingLuminaProjectInstructionsReturnsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := agentContext.LoadProjectInstructions(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("expected empty instructions when no LUMINA.md or AGENTS.md exists, got %q", got)
	}
}
