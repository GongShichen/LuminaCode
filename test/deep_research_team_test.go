package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"LuminaCode/config"
	"LuminaCode/team"
)

func TestDeepResearchTeamLoadsWithConfiguredGates(t *testing.T) {
	root := repoRoot(t)
	cfg := config.NewConfigForCWD(root)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	spec, err := team.NewLoader(cfg).Load("deep-research")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "deep-research" {
		t.Fatalf("team name=%q", spec.Name)
	}
	if len(spec.AgentSpecs) != 8 {
		t.Fatalf("agent count=%d want 8", len(spec.AgentSpecs))
	}
	if len(spec.Gates.Checks) != 2 {
		t.Fatalf("gate checks=%d want 2", len(spec.Gates.Checks))
	}
	want := map[string]string{"citation_qa": "qa", "methodology_review": "reviewer"}
	for _, check := range spec.Gates.Checks {
		if want[check.Name] != check.Agent {
			t.Fatalf("unexpected gate check: %#v", check)
		}
	}
	if spec.Loop.A2ADefaultTimeoutSeconds != 900 {
		t.Fatalf("a2a timeout=%d want 900", spec.Loop.A2ADefaultTimeoutSeconds)
	}
}

func TestDefaultAPIStreamIdleTimeoutSupportsLongResearchReports(t *testing.T) {
	cfg := config.NewConfigForCWD(t.TempDir())
	if cfg.APIStreamIdleTimeoutSeconds < 600 {
		t.Fatalf("long DeepResearch/report-writing runs need >=600s stream idle timeout, got %v", cfg.APIStreamIdleTimeoutSeconds)
	}
}

func TestDeepResearchToolAllowlistExcludesShellAndSubagents(t *testing.T) {
	root := repoRoot(t)
	cfg := config.NewConfigForCWD(root)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = filepath.Join(t.TempDir(), "sessions")
	spec, err := team.NewLoader(cfg).Load("deep-research")
	if err != nil {
		t.Fatal(err)
	}
	session := team.NewSession("parent", cfg, spec, nil, nil)
	names := map[string]struct{}{}
	for _, name := range session.AgentToolNames("source-reader") {
		names[name] = struct{}{}
	}
	for _, forbidden := range []string{"Agent", "TaskList", "TaskGet", "TaskWait", "TaskStop", "SendMessage"} {
		if _, ok := names[forbidden]; ok {
			t.Fatalf("DeepResearch source-reader should not expose %s; tools=%v", forbidden, session.AgentToolNames("source-reader"))
		}
	}
	for _, required := range []string{"read_file", "write_file", "run_shell", "WebSearch", "WebFetch", "Skill", "GetTeamContext", "SendA2AMessage"} {
		if _, ok := names[required]; !ok {
			t.Fatalf("DeepResearch source-reader missing required tool %s; tools=%v", required, session.AgentToolNames("source-reader"))
		}
	}
	searchTools := map[string]struct{}{}
	for _, name := range session.AgentToolNames("search-strategist") {
		searchTools[name] = struct{}{}
	}
	if _, ok := searchTools["run_shell"]; !ok {
		t.Fatalf("DeepResearch search-strategist should expose run_shell fallback; tools=%v", session.AgentToolNames("search-strategist"))
	}
	qaTools := map[string]struct{}{}
	for _, name := range session.AgentToolNames("qa") {
		qaTools[name] = struct{}{}
	}
	if _, ok := qaTools["run_shell"]; ok {
		t.Fatalf("DeepResearch qa should not expose run_shell fallback; tools=%v", session.AgentToolNames("qa"))
	}
}

func TestDeepResearchPromptsAndSkillsAreSubstantive(t *testing.T) {
	root := filepath.Join(repoRoot(t), ".Lumina", "TEAM", "deep-research")
	agents := []string{"team-leader", "scope-planner", "search-strategist", "source-reader", "evidence-analyst", "report-writer", "qa", "reviewer"}
	for _, agent := range agents {
		systemPath := filepath.Join(root, agent, "system.md")
		data, err := os.ReadFile(systemPath)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, want := range []string{"Identity and boundaries", "How to work"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s should contain %q", systemPath, want)
			}
		}
		if len(strings.Fields(text)) < 90 {
			t.Fatalf("%s is too short to guide an agent", systemPath)
		}
	}
	skillPaths, err := filepath.Glob(filepath.Join(root, "*", "skills", "*", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(skillPaths) < 8 {
		t.Fatalf("expected at least 8 DeepResearch skill files, got %d", len(skillPaths))
	}
	for _, path := range skillPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, want := range []string{"Procedure:", "Output"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s should contain %q", path, want)
			}
		}
		if len(strings.Fields(text)) < 120 {
			t.Fatalf("%s is too short to be a complete skill", path)
		}
	}
}

func TestDeepResearchForbidsModelMemoryAsEvidence(t *testing.T) {
	root := filepath.Join(repoRoot(t), ".Lumina", "TEAM", "deep-research")
	files := []string{
		filepath.Join(root, "shared-prompt.md"),
		filepath.Join(root, "team-system.md"),
		filepath.Join(root, "team-leader", "system.md"),
		filepath.Join(root, "source-reader", "system.md"),
		filepath.Join(root, "qa", "system.md"),
		filepath.Join(root, "reviewer", "system.md"),
		filepath.Join(root, "source-reader", "skills", "source-note", "SKILL.md"),
		filepath.Join(root, "qa", "skills", "citation-qa-report", "SKILL.md"),
		filepath.Join(root, "reviewer", "skills", "methodology-review", "SKILL.md"),
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ToLower(string(data))
		if !strings.Contains(text, "training knowledge") && !strings.Contains(text, "model memory") {
			t.Fatalf("%s should explicitly forbid model memory/training knowledge as evidence", file)
		}
	}
}

func TestDeepResearchSourceOwnershipAndRetrievalStatusAreExplicit(t *testing.T) {
	root := filepath.Join(repoRoot(t), ".Lumina", "TEAM", "deep-research")
	filesAndNeedles := map[string][]string{
		filepath.Join(root, "search-strategist", "system.md"): {
			"do not write `sources.jsonl`",
			"candidate failure",
		},
		filepath.Join(root, "search-strategist", "skills", "search-plan", "SKILL.md"): {
			"Do not write `sources.jsonl`",
			"evidence_allowed: no",
		},
		filepath.Join(root, "source-reader", "skills", "source-note", "SKILL.md"): {
			"retrieval_status",
			"claim_support_allowed",
			"curl-fallback",
			"Do not write `success`, `fail`, `fetched`",
		},
		filepath.Join(root, "evidence-analyst", "skills", "evidence-matrix", "SKILL.md"): {
			"`retrieval_status`",
			"`claim_support_allowed: true`",
		},
		filepath.Join(root, "qa", "skills", "citation-qa-report", "SKILL.md"): {
			"retrieval_status, claim_support_allowed",
			"Search Strategist candidate list",
			"documented shell/curl fallback",
			"invalid retrieval status aliases",
		},
		filepath.Join(root, "reviewer", "skills", "methodology-review", "SKILL.md"): {
			"`retrieval_status`",
			"undocumented fallback",
			"skips Source Reader",
		},
	}
	for file, needles := range filesAndNeedles {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, needle := range needles {
			if !strings.Contains(text, needle) {
				t.Fatalf("%s should contain %q", file, needle)
			}
		}
	}
}

func TestDeepResearchGatesRequireSourceClassCoverageAndPromptFinalization(t *testing.T) {
	root := filepath.Join(repoRoot(t), ".Lumina", "TEAM", "deep-research")
	filesAndNeedles := map[string][]string{
		filepath.Join(root, "team-leader", "system.md"): {
			"Do not create extra ad hoc QA/review/smoke A2A tasks",
			"proceed to `CompleteTeamTask`",
		},
		filepath.Join(root, "completion-policy.md"): {
			"Do not create additional ad hoc QA, review, or smoke tasks",
			"Missing contract-required source classes are blocking",
		},
		filepath.Join(root, "search-strategist", "skills", "search-plan", "SKILL.md"): {
			"official/regulatory/medical authority",
			"bounded handoff",
			"coverage_summary",
		},
		filepath.Join(root, "source-reader", "skills", "source-note", "SKILL.md"): {
			"GetTeamContext.team_runtime_dir/.cache/",
			"do not paste large multiline heredocs",
			"WebFetch verification fails for an arXiv URL",
		},
		filepath.Join(root, "qa", "system.md"): {
			"A source class required by the contract is absent",
			"official/regulatory/medical-authority evidence",
		},
		filepath.Join(root, "reviewer", "system.md"): {
			"Contract-required source classes are missing",
			"regulatory claims without FDA/EMA/WHO/official guidance",
		},
		filepath.Join(root, "report-writer", "system.md"): {
			"Produce a brief outline first",
			"Avoid long idle gaps during generation",
		},
		filepath.Join(root, "report-writer", "skills", "research-report", "SKILL.md"): {
			"Long-report execution rule",
			"one complete `write_file` call",
		},
	}
	for file, needles := range filesAndNeedles {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, needle := range needles {
			if !strings.Contains(text, needle) {
				t.Fatalf("%s should contain %q", file, needle)
			}
		}
	}
}
