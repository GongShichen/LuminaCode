package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"LuminaCode/agentContext"
)

const fakePromptCWD = "/fake/project"

func TestPromptSnapshotsMatchPythonWithLuminaRenames(t *testing.T) {
	sourceRoot := pythonSourceRoot(t)
	snapshotDir := filepath.Join(sourceRoot, "..", "..", "tests", "prompt_snapshots")
	if _, err := os.Stat(snapshotDir); err != nil {
		t.Skipf("Python prompt snapshots not available: %v", err)
	}

	restore := agentContext.SetPromptRuntimeHooksForTest(agentContext.PromptRuntimeHooks{
		EnvInfo: func() map[string]string {
			return map[string]string{
				"cwd":      fakePromptCWD,
				"platform": "Linux-5.0-test",
				"shell":    "/bin/bash",
			}
		},
		Now: func() time.Time {
			return time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
		},
		GitContext: func(_ string, _ float32, compact bool) string {
			return largeGitContext()
		},
	})
	t.Cleanup(restore)

	projectDir := t.TempDir()
	writeProjectInstructions(t, projectDir, "")

	assertPromptSnapshot(t, snapshotDir, "main_minimal.txt",
		mustBuildMainPrompt(t, projectDir, "", ""))

	assertPromptSnapshot(t, snapshotDir, "main_large_git.txt",
		mustBuildMainPrompt(t, projectDir, largeGitContext(), ""))

	largeProject := "local\n\n---\n\n" + strings.Repeat("x", 6000)
	writeProjectInstructions(t, projectDir, largeProject)
	assertPromptSnapshot(t, snapshotDir, "main_large_project_instructions.txt",
		mustBuildMainPrompt(t, projectDir, "", ""))

	subMinimal, err := agentContext.BuildSubagentPromptSections("test-subagent", "Snapshot test agent.", projectDir, 5, "", "")
	if err != nil {
		t.Fatal(err)
	}
	assertPromptSnapshot(t, snapshotDir, "subagent_minimal.txt", agentContext.AssemblePromptSections(subMinimal))

	subGit, err := agentContext.BuildSubagentPromptSections("test-subagent", "Snapshot test agent.", projectDir, 5, largeGitContext(), "")
	if err != nil {
		t.Fatal(err)
	}
	assertPromptSnapshot(t, snapshotDir, "subagent_compact_git.txt", agentContext.AssemblePromptSections(subGit))
}

func largeGitContext() string {
	return "Git branch: main\nWorking tree status:\n  M a.py\n" + strings.Repeat("x", 3000)
}

func mustBuildMainPrompt(t *testing.T, cwd, gitContext, memorySection string) string {
	t.Helper()
	restore := agentContext.SetPromptRuntimeHooksForTest(agentContext.PromptRuntimeHooks{
		EnvInfo: func() map[string]string {
			return map[string]string{
				"cwd":      fakePromptCWD,
				"platform": "Linux-5.0-test",
				"shell":    "/bin/bash",
			}
		},
		Now: func() time.Time {
			return time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
		},
		GitContext: func(_ string, _ float32, _ bool) string {
			return gitContext
		},
	})
	defer restore()

	prompt, err := agentContext.BuildSystemPrompt(cwd, memorySection)
	if err != nil {
		t.Fatal(err)
	}
	return prompt
}

func assertPromptSnapshot(t *testing.T, snapshotDir, filename, got string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(snapshotDir, filename))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(string(data), "XX"+"CODE.md", "LUMINA.md")
	want = strings.ReplaceAll(want, "`LUMINA.md` 中的项目指令", "`LUMINA.md` 或 `AGENTS.md` 中的项目指令")
	want = strings.ReplaceAll(want, "`LUMINA.md` 是项目约束", "`LUMINA.md` / `AGENTS.md` 是项目约束")
	want = strings.ReplaceAll(want, "Project Instructions (LUMINA.md)", "Project Instructions (LUMINA.md / AGENTS.md)")
	if got != want {
		t.Fatalf("%s mismatch\n--- want ---\n%s\n--- got ---\n%s", filename, want, got)
	}
}

func writeProjectInstructions(t *testing.T, dir, content string) {
	t.Helper()
	path := filepath.Join(dir, "LUMINA.md")
	if content == "" {
		_ = os.Remove(path)
		return
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func pythonSourceRoot(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, "Documents", "project", "X"+"x"+"Code", "src", "x"+"x"+"code")
}
