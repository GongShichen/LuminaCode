package test

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestLuminaAssetsLayoutMatchesRenamedPythonBundle(t *testing.T) {
	root := repoRoot(t)
	required := []string{
		"LUMINA.md",
		".Lumina/CONFIG/defaults.json",
		".Lumina/CONFIG/defaults.json.example",
		".Lumina/CONFIG/mcp.json",
		".Lumina/SKILLS/commit/SKILL.md",
		".Lumina/SKILLS/jupyter-notebook/SKILL.md",
		".Lumina/SKILLS/pdf/SKILL.md",
		".Lumina/SKILLS/review/SKILL.md",
		".Lumina/SKILLS/security-best-practices/SKILL.md",
		".Lumina/SKILLS/security-threat-model/SKILL.md",
		".Lumina/SYSTEM/extraction_system.md",
		".Lumina/SYSTEM/system-prompt.md",
		".Lumina/TEAM/product-development/backend/agent.yaml",
		".Lumina/TEAM/product-development/backend/skills/ipc-contract/SKILL.md",
		".Lumina/TEAM/product-development/backend/skills/persistence-plan/SKILL.md",
		".Lumina/TEAM/product-development/backend/skills/runtime-architecture/SKILL.md",
		".Lumina/TEAM/product-development/backend/system.md",
		".Lumina/TEAM/product-development/completion-policy.md",
		".Lumina/TEAM/product-development/devops/agent.yaml",
		".Lumina/TEAM/product-development/devops/skills/benchmark-runner-plan/SKILL.md",
		".Lumina/TEAM/product-development/devops/skills/install-flow-audit/SKILL.md",
		".Lumina/TEAM/product-development/devops/skills/release-check/SKILL.md",
		".Lumina/TEAM/product-development/devops/system.md",
		".Lumina/TEAM/product-development/frontend/agent.yaml",
		".Lumina/TEAM/product-development/frontend/skills/frontend-implementation-plan/SKILL.md",
		".Lumina/TEAM/product-development/frontend/skills/terminal-ui-design/SKILL.md",
		".Lumina/TEAM/product-development/frontend/skills/tui-interaction-review/SKILL.md",
		".Lumina/TEAM/product-development/frontend/system.md",
		".Lumina/TEAM/product-development/qa/agent.yaml",
		".Lumina/TEAM/product-development/qa/skills/acceptance-runbook/SKILL.md",
		".Lumina/TEAM/product-development/qa/skills/regression-risk/SKILL.md",
		".Lumina/TEAM/product-development/qa/skills/test-matrix/SKILL.md",
		".Lumina/TEAM/product-development/qa/system.md",
		".Lumina/TEAM/product-development/research/agent.yaml",
		".Lumina/TEAM/product-development/research/skills/benchmark-research/SKILL.md",
		".Lumina/TEAM/product-development/research/skills/evidence-report/SKILL.md",
		".Lumina/TEAM/product-development/research/skills/repo-map/SKILL.md",
		".Lumina/TEAM/product-development/research/system.md",
		".Lumina/TEAM/product-development/reviewer/agent.yaml",
		".Lumina/TEAM/product-development/reviewer/skills/architecture-risk-review/SKILL.md",
		".Lumina/TEAM/product-development/reviewer/skills/code-review-checklist/SKILL.md",
		".Lumina/TEAM/product-development/reviewer/skills/security-review/SKILL.md",
		".Lumina/TEAM/product-development/reviewer/system.md",
		".Lumina/TEAM/product-development/team-leader/agent.yaml",
		".Lumina/TEAM/product-development/team-leader/skills/completion-check/SKILL.md",
		".Lumina/TEAM/product-development/team-leader/skills/handoff-synthesis/SKILL.md",
		".Lumina/TEAM/product-development/team-leader/skills/task-breakdown/SKILL.md",
		".Lumina/TEAM/product-development/team-leader/system.md",
		".Lumina/TEAM/product-development/team-system.md",
		".Lumina/TEAM/product-development/team.yaml",
		".Lumina/TEAM/product-development/ux-design/agent.yaml",
		".Lumina/TEAM/product-development/ux-design/skills/conversation-ia/SKILL.md",
		".Lumina/TEAM/product-development/ux-design/skills/interaction-copy/SKILL.md",
		".Lumina/TEAM/product-development/ux-design/skills/workflow-critique/SKILL.md",
		".Lumina/TEAM/product-development/ux-design/system.md",
	}
	for _, rel := range required {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("required Lumina asset %s missing: %v", rel, err)
		}
		if info.IsDir() {
			t.Fatalf("required Lumina asset %s should be a file", rel)
		}
	}

	var got []string
	err := filepath.WalkDir(filepath.Join(root, ".Lumina"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := required[1:]
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected .Lumina asset set:\nwant:\n%s\n got:\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
}

func TestLuminaBundledPromptsUseRenamedInstructionFile(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{
		".Lumina/SYSTEM/system-prompt.md",
		".Lumina/SYSTEM/extraction_system.md",
	} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		oldNames := []string{"XX" + "CODE.md", "XX" + "Code", "Xx" + "Code"}
		for _, oldName := range oldNames {
			if strings.Contains(text, oldName) {
				t.Fatalf("%s still contains old project naming", rel)
			}
		}
		if !strings.Contains(text, "LUMINA.md") {
			t.Fatalf("%s should reference LUMINA.md", rel)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller unavailable")
	}
	return filepath.Dir(filepath.Dir(file))
}
