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
		".Lumina/TEAM/product-development/backend/skills/integration-handoff/SKILL.md",
		".Lumina/TEAM/product-development/backend/skills/ipc-contract/SKILL.md",
		".Lumina/TEAM/product-development/backend/skills/persistence-plan/SKILL.md",
		".Lumina/TEAM/product-development/backend/skills/runtime-architecture/SKILL.md",
		".Lumina/TEAM/product-development/backend/system.md",
		".Lumina/TEAM/product-development/completion-policy.md",
		".Lumina/TEAM/product-development/devops/agent.yaml",
		".Lumina/TEAM/product-development/devops/skills/benchmark-runner-plan/SKILL.md",
		".Lumina/TEAM/product-development/devops/skills/install-flow-audit/SKILL.md",
		".Lumina/TEAM/product-development/devops/skills/release-check/SKILL.md",
		".Lumina/TEAM/product-development/devops/skills/runtime-cleanliness/SKILL.md",
		".Lumina/TEAM/product-development/devops/system.md",
		".Lumina/TEAM/product-development/frontend/agent.yaml",
		".Lumina/TEAM/product-development/frontend/skills/api-consumption-contract/SKILL.md",
		".Lumina/TEAM/product-development/frontend/skills/frontend-implementation-plan/SKILL.md",
		".Lumina/TEAM/product-development/frontend/skills/terminal-ui-design/SKILL.md",
		".Lumina/TEAM/product-development/frontend/skills/tui-interaction-review/SKILL.md",
		".Lumina/TEAM/product-development/frontend/system.md",
		".Lumina/TEAM/product-development/qa/agent.yaml",
		".Lumina/TEAM/product-development/qa/skills/acceptance-runbook/SKILL.md",
		".Lumina/TEAM/product-development/qa/skills/qa-report/SKILL.md",
		".Lumina/TEAM/product-development/qa/skills/regression-risk/SKILL.md",
		".Lumina/TEAM/product-development/qa/skills/test-matrix/SKILL.md",
		".Lumina/TEAM/product-development/qa/system.md",
		".Lumina/TEAM/product-development/research/agent.yaml",
		".Lumina/TEAM/product-development/research/skills/benchmark-research/SKILL.md",
		".Lumina/TEAM/product-development/research/skills/domain-discovery-brief/SKILL.md",
		".Lumina/TEAM/product-development/research/skills/evidence-report/SKILL.md",
		".Lumina/TEAM/product-development/research/skills/repo-map/SKILL.md",
		".Lumina/TEAM/product-development/research/system.md",
		".Lumina/TEAM/product-development/reviewer/agent.yaml",
		".Lumina/TEAM/product-development/reviewer/skills/architecture-risk-review/SKILL.md",
		".Lumina/TEAM/product-development/reviewer/skills/code-review-checklist/SKILL.md",
		".Lumina/TEAM/product-development/reviewer/skills/process-compliance-review/SKILL.md",
		".Lumina/TEAM/product-development/reviewer/skills/security-review/SKILL.md",
		".Lumina/TEAM/product-development/reviewer/system.md",
		".Lumina/TEAM/product-development/shared-prompt.md",
		".Lumina/TEAM/product-development/team-leader/agent.yaml",
		".Lumina/TEAM/product-development/team-leader/skills/artifact-gate/SKILL.md",
		".Lumina/TEAM/product-development/team-leader/skills/completion-check/SKILL.md",
		".Lumina/TEAM/product-development/team-leader/skills/handoff-synthesis/SKILL.md",
		".Lumina/TEAM/product-development/team-leader/skills/prd-authoring/SKILL.md",
		".Lumina/TEAM/product-development/team-leader/skills/task-breakdown/SKILL.md",
		".Lumina/TEAM/product-development/team-leader/system.md",
		".Lumina/TEAM/product-development/team-system.md",
		".Lumina/TEAM/product-development/team.yaml",
		".Lumina/TEAM/product-development/ux-design/agent.yaml",
		".Lumina/TEAM/product-development/ux-design/skills/conversation-ia/SKILL.md",
		".Lumina/TEAM/product-development/ux-design/skills/design-spec/SKILL.md",
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

func TestProductDevelopmentPromptsPreserveRoleBoundaries(t *testing.T) {
	root := repoRoot(t)
	checks := map[string][]string{
		".Lumina/TEAM/product-development/shared-prompt.md": {
			"Team Leader coordinates and may inspect files, but must not directly implement or repair product source",
			"For CLI, TUI, desktop, plugin, or other local tools",
			"Data models, persistence, business services, package manifests, and durable storage stay Backend-owned",
			"In-process monkeypatches are not valid for subprocess integration tests",
			"A function parameter-only override is also not subprocess-visible",
			"A phrase like \"use env var or monkeypatch\" is not a contract",
			"do not create throwaway helper scripts or temporary source/test files outside the expected artifacts",
		},
		".Lumina/TEAM/product-development/team-leader/system.md": {
			"Do not directly implement or repair product source, tests, package manifests, runtime data, or generated build artifacts",
			"dispatch recovery to the owner with exact failing commands and expected files",
		},
		".Lumina/TEAM/product-development/team-leader/skills/task-breakdown/SKILL.md": {
			"assign command parsing, output formatting, help/error copy, and interaction behavior to Frontend",
			"assign data model, business services, persistence, package manifests, storage paths, and backend tests to Backend",
			"Team Leader should not edit product source, tests, package manifests, runtime data, or generated build artifacts directly",
			"Do not list monkeypatch as a subprocess integration mechanism",
		},
		".Lumina/TEAM/product-development/team-leader/skills/artifact-gate/SKILL.md": {
			"names one concrete storage path resolution and subprocess-visible test isolation mechanism",
			"monkeypatch does not cross subprocess boundaries",
			"Reject function parameter-only overrides",
		},
		".Lumina/TEAM/product-development/backend/skills/persistence-plan/SKILL.md": {
			"define exactly how the storage path is resolved in real execution",
			"monkeypatching an in-process module global is not a valid isolation mechanism",
			"A function parameter-only storage override is valid for in-process unit tests",
			"Do not ship a package-relative-only data path",
			"treat the contract as incomplete",
		},
		".Lumina/TEAM/product-development/backend/skills/ipc-contract/SKILL.md": {
			"storage path resolution and any subprocess-visible test override",
			"in-process monkeypatches do not cross this boundary",
		},
		".Lumina/TEAM/product-development/frontend/skills/frontend-implementation-plan/SKILL.md": {
			"Subprocess-based tests must use mechanisms visible to the subprocess",
			"Do not rely on monkeypatching in-process module globals",
			"Do not accept a backend function parameter as subprocess isolation",
			"Do not create temporary scripts or extra test files during implementation",
			"stop and ask Team Leader/Backend to fix the contract",
		},
		".Lumina/TEAM/product-development/frontend/skills/api-consumption-contract/SKILL.md": {
			"storage fixtures and overrides must be visible to the spawned process",
			"do not rely on in-process monkeypatches",
		},
		".Lumina/TEAM/product-development/reviewer/system.md": {
			"Do not create throwaway smoke scripts, helper programs, or temporary source/test files",
			"use existing tests, existing product commands, or single-line shell checks",
		},
		".Lumina/TEAM/product-development/reviewer/skills/code-review-checklist/SKILL.md": {
			"Review verification must not create undeclared helper scripts",
			"ask Team Leader to add an expected artifact before writing a helper",
		},
		".Lumina/TEAM/product-development/reviewer/skills/process-compliance-review/SKILL.md": {
			"A reviewer-created smoke script or helper file is a process violation",
			"declared as an expected artifact before the review task started",
		},
	}
	for rel, wants := range checks {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Fatalf("%s should contain %q", rel, want)
			}
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
