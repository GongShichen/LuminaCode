package test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"LuminaCode/config"
	"LuminaCode/skills"
	luminateam "LuminaCode/team"
)

func TestProductDevelopmentTeamBundleIsComplete(t *testing.T) {
	root := repoRoot(t)
	cfg := config.NewConfigForCWD(root)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	spec, err := luminateam.NewLoader(cfg).Load("product-development")
	if err != nil {
		t.Fatal(err)
	}
	if spec.DisplayName != "Product Development Team" {
		t.Fatalf("display name mismatch: %s", spec.DisplayName)
	}
	requiredAgents := []string{"team-leader", "research", "frontend", "backend", "qa", "reviewer", "devops", "ux-design"}
	if len(spec.AgentSpecs) != len(requiredAgents) {
		t.Fatalf("agent count mismatch: got %d want %d", len(spec.AgentSpecs), len(requiredAgents))
	}
	for _, id := range requiredAgents {
		index, ok := spec.AgentMap[id]
		if !ok {
			t.Fatalf("missing agent %s", id)
		}
		agent := spec.AgentSpecs[index]
		if agent.SystemPromptPath == "" || agent.SkillsDir == "" {
			t.Fatalf("agent %s missing prompt or skills path", id)
		}
		privateSkills := skills.NewSkillLoader(config.Config{UserSkillsDir: agent.SkillsDir, IsolatedSkillsOnly: true}).LoadFrontmatterOnly()
		if len(privateSkills) != 3 {
			t.Fatalf("agent %s private skill count = %d, want 3", id, len(privateSkills))
		}
	}
}

func TestCreateTeamTemplateCreatesLeaderOnlyTeam(t *testing.T) {
	cfg := config.NewConfigForCWD(t.TempDir())
	cfg.TeamDir = t.TempDir()
	loader := luminateam.NewLoader(cfg)
	result, err := loader.CreateTemplate("Data Analysis Team")
	if err != nil {
		t.Fatal(err)
	}
	if result.TeamName != "data-analysis-team" || result.AgentCount != 1 {
		t.Fatalf("unexpected template result: %#v", result)
	}
	for _, rel := range []string{
		"team.yaml",
		"team-system.md",
		"completion-policy.md",
		filepath.Join("team-leader", "agent.yaml"),
		filepath.Join("team-leader", "system.md"),
		filepath.Join("team-leader", "skills"),
	} {
		if _, err := os.Stat(filepath.Join(result.Path, rel)); err != nil {
			t.Fatalf("expected template path %s: %v", rel, err)
		}
	}
	spec, err := loader.Load("data-analysis-team")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.AgentSpecs) != 1 || spec.AgentSpecs[0].Name != "team-leader" {
		t.Fatalf("template should contain only team-leader, got %#v", spec.AgentSpecs)
	}
	if spec.Gates.QAAgent != "" || spec.Gates.ReviewerAgent != "" || spec.Gates.RequireContract {
		t.Fatalf("template should not enable QA/Reviewer gates by default: %#v", spec.Gates)
	}
	if _, err := loader.CreateTemplate("Data Analysis Team"); err == nil {
		t.Fatal("expected duplicate template creation to fail")
	}
}

func TestLeaderOnlyTeamCanCompleteWithoutQAGates(t *testing.T) {
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = t.TempDir()
	cfg.SessionDir = t.TempDir()
	if _, err := luminateam.NewLoader(cfg).CreateTemplate("Solo Team"); err != nil {
		t.Fatal(err)
	}
	session, err := luminateam.NewManager(cfg, nil, nil).Start("parent-session", "solo-team", workdir)
	if err != nil {
		t.Fatal(err)
	}
	result := session.CompleteTask("team-leader", luminateam.CompleteTeamTaskInput{
		FinalAnswer: "done",
		Summary:     "done",
	})
	if !strings.Contains(result, "marked complete") {
		t.Fatalf("leader-only team should complete without QA/Reviewer gates, got %q", result)
	}
}

func TestProductDevelopmentTeamPreservesNamedProjectRoot(t *testing.T) {
	root := repoRoot(t)
	required := map[string]string{
		filepath.Join(root, ".Lumina", "TEAM", "product-development", "team-system.md"):                                      "Do not flatten a named project into the current working directory",
		filepath.Join(root, ".Lumina", "TEAM", "product-development", "team-leader", "system.md"):                            "use `<current working directory>/<requested name>`",
		filepath.Join(root, ".Lumina", "TEAM", "product-development", "team-leader", "skills", "task-breakdown", "SKILL.md"): "Project/artifact root",
	}
	for path, want := range required {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), want) {
			t.Fatalf("%s missing root-preservation rule %q", path, want)
		}
	}
}

func TestProductDevelopmentTeamKeepsRuntimeOutOfParentWorkspace(t *testing.T) {
	root := repoRoot(t)
	required := map[string]string{
		filepath.Join(root, ".Lumina", "TEAM", "product-development", "team-system.md"):           "QA must verify parent workspace cleanliness",
		filepath.Join(root, ".Lumina", "TEAM", "product-development", "team-leader", "system.md"): "Include a parent-workspace-clean check",
		filepath.Join(root, ".Lumina", "TEAM", "product-development", "qa", "system.md"):          "verify parent workspace cleanliness",
		filepath.Join(root, ".Lumina", "TEAM", "product-development", "devops", "system.md"):      "~/.lumina/project/{project_root_name}/",
		filepath.Join(root, ".Lumina", "TEAM", "product-development", "completion-policy.md"):     "the parent working directory remains clean",
	}
	for path, want := range required {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), want) {
			t.Fatalf("%s missing parent workspace cleanliness rule %q", path, want)
		}
	}
}

func TestProductDevelopmentTeamRequiresBuildAndRegatesBlockingReviewNotes(t *testing.T) {
	root := repoRoot(t)
	required := map[string]string{
		filepath.Join(root, ".Lumina", "TEAM", "product-development", "team-system.md"):           "`accepted_with_notes` containing `CRITICAL`",
		filepath.Join(root, ".Lumina", "TEAM", "product-development", "completion-policy.md"):     "all relevant declared build/check/test commands pass",
		filepath.Join(root, ".Lumina", "TEAM", "product-development", "qa", "system.md"):          "`npm run build` when `package.json` has a `build` script",
		filepath.Join(root, ".Lumina", "TEAM", "product-development", "team-leader", "system.md"): "Treat Reviewer `accepted_with_notes` as a repair trigger",
	}
	for path, want := range required {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), want) {
			t.Fatalf("%s missing build/regate rule %q", path, want)
		}
	}
}

func TestTeamModeDisablesOrdinarySubagentTools(t *testing.T) {
	root := repoRoot(t)
	cfg := config.NewConfigForCWD(root)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	manager := luminateam.NewManager(cfg, nil, nil)
	session, err := manager.Start("parent-session", "product-development", root)
	if err != nil {
		t.Fatal(err)
	}
	disabled := map[string]struct{}{
		"Agent":       {},
		"TaskList":    {},
		"TaskGet":     {},
		"TaskWait":    {},
		"TaskStop":    {},
		"SendMessage": {},
	}
	for _, agentID := range []string{"team-leader", "backend", "frontend", "qa", "reviewer"} {
		names := session.AgentToolNames(agentID)
		seen := map[string]struct{}{}
		for _, name := range names {
			seen[name] = struct{}{}
		}
		for name := range disabled {
			if _, ok := seen[name]; ok {
				t.Fatalf("team agent %s should not expose ordinary subagent tool %s; tools=%v", agentID, name, names)
			}
		}
		if _, ok := seen["SendA2AMessage"]; !ok {
			t.Fatalf("team agent %s should expose SendA2AMessage; tools=%v", agentID, names)
		}
		if agentID == "team-leader" {
			if _, ok := seen["CompleteTeamTask"]; !ok {
				t.Fatalf("team leader should expose CompleteTeamTask; tools=%v", names)
			}
			if _, ok := seen["RecordTeamContract"]; !ok {
				t.Fatalf("team leader should expose RecordTeamContract; tools=%v", names)
			}
		}
		if agentID == "qa" || agentID == "reviewer" {
			if _, ok := seen["SubmitGateVerdict"]; !ok {
				t.Fatalf("team gate agent %s should expose SubmitGateVerdict; tools=%v", agentID, names)
			}
		}
	}
}

func TestTeamLeaderMustRecordContractBeforeImplementationDispatch(t *testing.T) {
	root := repoRoot(t)
	cfg := config.NewConfigForCWD(root)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	manager := luminateam.NewManager(cfg, nil, nil)
	session, err := manager.Start("parent-session", "product-development", root)
	if err != nil {
		t.Fatal(err)
	}
	result := session.SendA2AMessage(context.Background(), "team-leader", luminateam.A2AMessageInput{
		To:       []string{"backend"},
		TaskType: "implementation",
		Message:  "build backend",
	})
	if result["status"] != "error" || !strings.Contains(result["error"].(string), "RecordTeamContract") {
		t.Fatalf("expected contract error before implementation dispatch, got %#v", result)
	}
}

func TestTeamAgentsInheritAndSyncYoloMode(t *testing.T) {
	root := repoRoot(t)
	cfg := config.NewConfigForCWD(root)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	cfg.Yolo = true
	manager := luminateam.NewManager(cfg, nil, nil)
	session, err := manager.Start("parent-session", "product-development", root)
	if err != nil {
		t.Fatal(err)
	}
	if !session.AgentYoloEnabled("team-leader") || !session.AgentYoloEnabled("backend") {
		t.Fatalf("team agents should inherit yolo=true from parent config")
	}
	cfg.Yolo = false
	manager.ApplyParentRuntimeConfig("parent-session", cfg)
	if session.AgentYoloEnabled("team-leader") || session.AgentYoloEnabled("backend") {
		t.Fatalf("team agents should sync yolo=false from parent config")
	}
	cfg.Yolo = true
	manager.ApplyParentRuntimeConfig("parent-session", cfg)
	if !session.AgentYoloEnabled("team-leader") || !session.AgentYoloEnabled("backend") {
		t.Fatalf("team agents should sync yolo=true from parent config")
	}
}

func TestTeamCompleteAcceptsExistingFileArtifacts(t *testing.T) {
	root := repoRoot(t)
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	manager := luminateam.NewManager(cfg, nil, nil)
	session, err := manager.Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(workdir, "todolite", "README.md")
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, []byte("# TodoLite\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordPassingContractAndGates(t, session, []string{artifactPath})
	result := session.CompleteTask("team-leader", luminateam.CompleteTeamTaskInput{
		FinalAnswer:       "done",
		QAStatus:          "pass",
		ReviewerStatus:    "pass",
		RequiredArtifacts: []string{artifactPath},
	})
	if !strings.Contains(result, "marked complete") {
		t.Fatalf("expected completion with file artifact, got %q", result)
	}
	snapshot := session.Snapshot()
	if snapshot.GateStatus.QA != "pass" || snapshot.GateStatus.Reviewer != "pass" {
		t.Fatalf("session gate should be persisted after completion, got %#v", snapshot.GateStatus)
	}
	if got := snapshot.Dialogue[len(snapshot.Dialogue)-1]; got.Kind != "final" {
		t.Fatalf("last dialogue should be final, got %#v", got)
	}
	for _, row := range snapshot.ActivityRows {
		if row.AgentID == "team-leader" && row.Status != "completed" {
			t.Fatalf("team leader activity should be completed, got %#v", row)
		}
	}
}

func TestTeamCompleteAcceptsArtifactsUnderNamedProjectRoot(t *testing.T) {
	root := repoRoot(t)
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	manager := luminateam.NewManager(cfg, nil, nil)
	session, err := manager.Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"README.md",
		"backend/main.go",
		"cli/src/index.ts",
	} {
		path := filepath.Join(workdir, "todolite", rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	recordPassingContractAndGates(t, session, []string{"README.md", "backend/main.go", "cli/src/index.ts"})
	result := session.CompleteTask("team-leader", luminateam.CompleteTeamTaskInput{
		FinalAnswer:       "done",
		QAStatus:          "pass",
		ReviewerStatus:    "pass",
		RequiredArtifacts: []string{"README.md", "backend/main.go", "cli/src/index.ts"},
	})
	if !strings.Contains(result, "marked complete") {
		t.Fatalf("expected completion with named project artifacts, got %q", result)
	}
}

func TestTeamCompleteRequiresStructuredGateVerdicts(t *testing.T) {
	root := repoRoot(t)
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	manager := luminateam.NewManager(cfg, nil, nil)
	session, err := manager.Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	writeRequiredArtifacts(t, workdir, "todolite", []string{"README.md"})
	session.RecordContract("team-leader", testContract(workdir, []string{"README.md"}))
	result := session.CompleteTask("team-leader", luminateam.CompleteTeamTaskInput{
		FinalAnswer:       "done",
		QAStatus:          "pass",
		ReviewerStatus:    "accepted_with_notes",
		RequiredArtifacts: []string{"README.md"},
	})
	if !strings.Contains(result, "missing structured QA gate verdict") {
		t.Fatalf("expected missing QA verdict rejection, got %q", result)
	}
}

func TestTeamCompleteRejectsMissingQAEvidence(t *testing.T) {
	root := repoRoot(t)
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	manager := luminateam.NewManager(cfg, nil, nil)
	session, err := manager.Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	writeRequiredArtifacts(t, workdir, "todolite", []string{"README.md"})
	session.RecordContract("team-leader", testContract(workdir, []string{"README.md"}))
	session.SubmitGateVerdict("qa", luminateam.GateVerdict{
		Role: "qa", Status: "pass", Summary: "partial evidence",
		Evidence: []luminateam.GateEvidence{{Name: "go test", Passed: true}},
	})
	session.SubmitGateVerdict("reviewer", luminateam.GateVerdict{Role: "reviewer", Status: "pass", Summary: "ok"})
	result := session.CompleteTask("team-leader", luminateam.CompleteTeamTaskInput{
		FinalAnswer:       "done",
		QAStatus:          "pass",
		ReviewerStatus:    "pass",
		RequiredArtifacts: []string{"README.md"},
	})
	if !strings.Contains(result, "QA evidence missing required contract checks") {
		t.Fatalf("expected missing evidence rejection, got %q", result)
	}
}

func TestTeamCompleteRejectsBlockingReviewerFinding(t *testing.T) {
	root := repoRoot(t)
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	manager := luminateam.NewManager(cfg, nil, nil)
	session, err := manager.Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	writeRequiredArtifacts(t, workdir, "todolite", []string{"README.md"})
	submitPassingContractAndQA(t, session, workdir, []string{"README.md"})
	session.SubmitGateVerdict("reviewer", luminateam.GateVerdict{
		Role: "reviewer", Status: "accepted_with_notes", Summary: "has blocking architecture mismatch",
		Findings: []luminateam.GateFinding{{Category: "architecture", Summary: "CLI bypasses backend", Blocking: true}},
	})
	result := session.CompleteTask("team-leader", luminateam.CompleteTeamTaskInput{
		FinalAnswer:       "done",
		QAStatus:          "pass",
		ReviewerStatus:    "accepted_with_notes",
		RequiredArtifacts: []string{"README.md"},
	})
	if !strings.Contains(result, "blocking") {
		t.Fatalf("expected blocking reviewer rejection, got %q", result)
	}
}

func TestProductTeamRejectsNonblockingReviewerNotesWithoutDeferral(t *testing.T) {
	root := repoRoot(t)
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	manager := luminateam.NewManager(cfg, nil, nil)
	session, err := manager.Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	writeRequiredArtifacts(t, workdir, "todolite", []string{"README.md"})
	submitPassingContractAndQA(t, session, workdir, []string{"README.md"})
	session.SubmitGateVerdict("reviewer", luminateam.GateVerdict{
		Role:    "reviewer",
		Status:  "accepted_with_notes",
		Summary: "nonblocking notes only",
		Findings: []luminateam.GateFinding{{
			Category: "style",
			Summary:  "minor improvement suggestion",
			Blocking: false,
		}},
	})
	result := session.CompleteTask("team-leader", luminateam.CompleteTeamTaskInput{
		FinalAnswer:       "done",
		QAStatus:          "pass",
		ReviewerStatus:    "accepted_with_notes",
		RequiredArtifacts: []string{"README.md"},
	})
	if !strings.Contains(result, "nonblocking finding without follow-up or deferral reason") {
		t.Fatalf("expected nonblocking reviewer notes without deferral to reject, got %q", result)
	}
}

func TestProductTeamAllowsNonblockingReviewerNotesWithDeferral(t *testing.T) {
	root := repoRoot(t)
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	manager := luminateam.NewManager(cfg, nil, nil)
	session, err := manager.Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	writeRequiredArtifacts(t, workdir, "todolite", []string{"README.md"})
	submitPassingContractAndQA(t, session, workdir, []string{"README.md"})
	session.SubmitGateVerdict("reviewer", luminateam.GateVerdict{
		Role:    "reviewer",
		Status:  "accepted_with_notes",
		Summary: "nonblocking notes only",
		Findings: []luminateam.GateFinding{{
			Category: "style",
			Summary:  "minor improvement suggestion",
			Blocking: false,
		}},
	})
	result := session.CompleteTask("team-leader", luminateam.CompleteTeamTaskInput{
		FinalAnswer:       "done",
		QAStatus:          "pass",
		ReviewerStatus:    "accepted_with_notes",
		RequiredArtifacts: []string{"README.md"},
		DeferralReasons: map[string]string{
			"Reviewer:style:minor improvement suggestion": "Safe to defer because it does not affect correctness or requested behavior.",
		},
	})
	if !strings.Contains(result, "marked complete") {
		t.Fatalf("expected nonblocking reviewer notes with deferral to allow completion, got %q", result)
	}
	snapshot := session.Snapshot()
	if snapshot.TeamContract == nil || snapshot.GateVerdicts["qa"].Status != "pass" {
		t.Fatalf("snapshot should include contract and gate verdicts, got %#v", snapshot)
	}
}

func TestTeamContractUpdateCannotRemoveRequiredChecks(t *testing.T) {
	root := repoRoot(t)
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	manager := luminateam.NewManager(cfg, nil, nil)
	session, err := manager.Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	writeRequiredArtifacts(t, workdir, "todolite", []string{"README.md"})
	if got := session.RecordContract("team-leader", testContract(workdir, []string{"README.md"})); !strings.Contains(got, "recorded") {
		t.Fatalf("record contract failed: %s", got)
	}
	weaker := luminateam.AcceptanceContract{
		ProjectRoot:         filepath.Join(workdir, "todolite"),
		UserRequirements:    []string{"deliver TodoLite"},
		IntegrationContract: "Go backend exposes REST API. TS CLI sends HTTP requests to localhost:8080.",
		RequiredArtifacts:   []string{"README.md"},
		RequiredCommands:    []luminateam.ContractCheck{{Name: "all-tests", Command: "go test ./... && cd cli && npm test", CWD: filepath.Join(workdir, "todolite"), Required: true}},
		CompletionCriteria:  []string{"all tests pass"},
	}
	session.RecordContract("team-leader", weaker)
	snapshot := session.Snapshot()
	if len(snapshot.TeamContract.RequiredCommands) < 3 || len(snapshot.TeamContract.IntegrationSmokes) != 1 {
		t.Fatalf("contract update should preserve prior required checks/smokes, got %#v", snapshot.TeamContract)
	}
	session.SubmitGateVerdict("qa", luminateam.GateVerdict{
		Role: "qa", Status: "pass", Summary: "only all-tests",
		Evidence: []luminateam.GateEvidence{{Name: "all-tests", Command: "go test ./... && cd cli && npm test", Passed: true}},
	})
	session.SubmitGateVerdict("reviewer", luminateam.GateVerdict{Role: "reviewer", Status: "pass", Summary: "ok"})
	result := session.CompleteTask("team-leader", luminateam.CompleteTeamTaskInput{
		FinalAnswer:       "done",
		QAStatus:          "pass",
		ReviewerStatus:    "pass",
		RequiredArtifacts: []string{"README.md"},
	})
	if !strings.Contains(result, "QA evidence missing required contract checks") {
		t.Fatalf("expected preserved contract checks to block weak QA evidence, got %q", result)
	}
}

func TestTeamQAGateVerdictsMergeEvidence(t *testing.T) {
	root := repoRoot(t)
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	manager := luminateam.NewManager(cfg, nil, nil)
	session, err := manager.Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	writeRequiredArtifacts(t, workdir, "todolite", []string{"README.md"})
	session.RecordContract("team-leader", testContract(workdir, []string{"README.md"}))
	for _, evidence := range []luminateam.GateEvidence{
		{Name: "go test", Command: "go test ./...", Passed: true, OutputSummary: "pass"},
		{Name: "npm test", Command: "npm test", Passed: true, OutputSummary: "pass"},
		{Name: "cli backend smoke", Command: "todo add/list/done/delete through backend", Passed: true, OutputSummary: "pass"},
	} {
		session.SubmitGateVerdict("qa", luminateam.GateVerdict{
			Role:     "qa",
			Status:   "pass",
			Summary:  evidence.Name,
			Evidence: []luminateam.GateEvidence{evidence},
		})
	}
	session.SubmitGateVerdict("reviewer", luminateam.GateVerdict{Role: "reviewer", Status: "pass", Summary: "ok"})
	snapshot := session.Snapshot()
	if got := len(snapshot.GateVerdicts["qa"].Evidence); got != 3 {
		t.Fatalf("QA evidence should merge across verdict submissions, got %d: %#v", got, snapshot.GateVerdicts["qa"].Evidence)
	}
	result := session.CompleteTask("team-leader", luminateam.CompleteTeamTaskInput{
		FinalAnswer:       "done",
		QAStatus:          "pass",
		ReviewerStatus:    "pass",
		RequiredArtifacts: []string{"README.md"},
	})
	if !strings.Contains(result, "marked complete") {
		t.Fatalf("expected merged QA evidence to allow completion, got %q", result)
	}
}

func TestTeamCompletionRejectsQABlockingFinding(t *testing.T) {
	root := repoRoot(t)
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	manager := luminateam.NewManager(cfg, nil, nil)
	session, err := manager.Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	writeRequiredArtifacts(t, workdir, "todolite", []string{"README.md"})
	session.RecordContract("team-leader", testContract(workdir, []string{"README.md"}))
	session.SubmitGateVerdict("qa", luminateam.GateVerdict{
		Role:    "qa",
		Status:  "pass",
		Summary: "qa found blocker",
		Evidence: []luminateam.GateEvidence{
			{Name: "go test", Command: "go test ./...", Passed: true, OutputSummary: "pass"},
			{Name: "npm test", Command: "npm test", Passed: true, OutputSummary: "pass"},
			{Name: "cli backend smoke", Command: "todo add/list/done/delete through backend", Passed: true, OutputSummary: "pass"},
		},
		Findings: []luminateam.GateFinding{{Category: "correctness", Summary: "filtering is broken", Blocking: true}},
	})
	session.SubmitGateVerdict("reviewer", luminateam.GateVerdict{Role: "reviewer", Status: "pass", Summary: "ok"})
	result := session.CompleteTask("team-leader", luminateam.CompleteTeamTaskInput{
		FinalAnswer:       "done",
		QAStatus:          "pass",
		ReviewerStatus:    "pass",
		RequiredArtifacts: []string{"README.md"},
	})
	if !strings.Contains(result, "QA has blocking finding") {
		t.Fatalf("expected QA blocking finding to reject completion, got %q", result)
	}
}

func TestTeamQAGateVerdictRefreshClearsResolvedFindings(t *testing.T) {
	root := repoRoot(t)
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	manager := luminateam.NewManager(cfg, nil, nil)
	session, err := manager.Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	writeRequiredArtifacts(t, workdir, "todolite", []string{"README.md"})
	session.RecordContract("team-leader", testContract(workdir, []string{"README.md"}))
	session.SubmitGateVerdict("qa", luminateam.GateVerdict{
		Role:    "qa",
		Status:  "pass",
		Summary: "initial qa blocker",
		Evidence: []luminateam.GateEvidence{
			{Name: "go test", Command: "go test ./...", Passed: true, OutputSummary: "pass"},
		},
		Findings: []luminateam.GateFinding{{Category: "correctness", Summary: "filtering is broken", Blocking: true}},
	})
	session.SubmitGateVerdict("qa", luminateam.GateVerdict{
		Role:    "qa",
		Status:  "pass",
		Summary: "qa recheck passed",
		Evidence: []luminateam.GateEvidence{
			{Name: "npm test", Command: "npm test", Passed: true, OutputSummary: "pass"},
			{Name: "cli backend smoke", Command: "todo add/list/done/delete through backend", Passed: true, OutputSummary: "pass"},
		},
	})
	session.SubmitGateVerdict("reviewer", luminateam.GateVerdict{Role: "reviewer", Status: "pass", Summary: "ok"})
	snapshot := session.Snapshot()
	if got := len(snapshot.GateVerdicts["qa"].Evidence); got != 3 {
		t.Fatalf("QA evidence should still merge after refresh, got %d", got)
	}
	for _, finding := range snapshot.GateVerdicts["qa"].Findings {
		if finding.Blocking {
			t.Fatalf("resolved QA blocker should not remain in latest findings: %#v", snapshot.GateVerdicts["qa"].Findings)
		}
	}
	result := session.CompleteTask("team-leader", luminateam.CompleteTeamTaskInput{
		FinalAnswer:       "done",
		QAStatus:          "pass",
		ReviewerStatus:    "pass",
		RequiredArtifacts: []string{"README.md"},
	})
	if !strings.Contains(result, "marked complete") {
		t.Fatalf("expected refreshed QA findings to allow completion, got %q", result)
	}
}

func TestTeamPrivateSkillsDoNotLeakIntoOrdinarySkillLoader(t *testing.T) {
	root := repoRoot(t)
	cfg := config.NewConfigForCWD(root)
	cfg.UserSkillsDir = filepath.Join(root, ".Lumina", "TEAM", "product-development", "frontend", "skills")
	cfg.IsolatedSkillsOnly = true
	privateSkills := skills.NewSkillLoader(cfg).LoadFrontmatterOnly()
	if len(privateSkills) != 3 {
		t.Fatalf("private skill loader got %d skills, want 3", len(privateSkills))
	}
	cfg.IsolatedSkillsOnly = false
	cfg.UserSkillsDir = filepath.Join(t.TempDir(), "no-user-skills")
	ordinary := skills.NewSkillLoader(cfg).LoadFrontmatterOnly()
	for _, skill := range ordinary {
		switch skill.CanonicalName {
		case "tui-interaction-review", "terminal-ui-design", "frontend-implementation-plan":
			t.Fatalf("team private skill leaked into ordinary loader: %s", skill.CanonicalName)
		}
	}
}

func TestTeamA2ABasicMethodsAndSnapshot(t *testing.T) {
	root := repoRoot(t)
	cfg := config.NewConfigForCWD(root)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	var events []string
	manager := luminateam.NewManager(cfg, func(_ string, eventType string, _ any) {
		events = append(events, eventType)
	}, nil)
	session, err := manager.Start("parent-session", "product-development", root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := session.Snapshot()
	if !snapshot.TeamMode || snapshot.ActiveTeamName != "Product Development Team" {
		t.Fatalf("bad team snapshot: %#v", snapshot)
	}
	card, err := manager.HandleA2A(t.Context(), session.ID, "team-leader", "agent.card.get", nil)
	if err != nil {
		t.Fatal(err)
	}
	cardJSON, _ := json.Marshal(card)
	if !json.Valid(cardJSON) {
		t.Fatalf("agent card is not json serializable")
	}
	artifacts, err := manager.HandleA2A(t.Context(), session.ID, "team-leader", "artifact/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	if list, ok := artifacts.([]luminateam.Artifact); !ok || len(list) != 0 {
		t.Fatalf("unexpected artifact list: %#v", artifacts)
	}
	if len(events) == 0 || events[0] != "team.started" {
		t.Fatalf("expected team.started event, got %#v", events)
	}
}

func TestTeamRestorePersistedSessionForResume(t *testing.T) {
	root := repoRoot(t)
	cfg := config.NewConfigForCWD(root)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	parent := "parent-session"
	teamID := "team-restore"
	teamRoot := filepath.Join(cfg.SessionDir, parent, "teams", teamID)
	mustWriteJSON(t, filepath.Join(teamRoot, "team.json"), map[string]any{
		"id":                teamID,
		"parent_session_id": parent,
		"team":              "product-development",
		"snapshot": map[string]any{
			"team_session_id":     teamID,
			"team_loop_iteration": 3,
			"team_gate_status":    map[string]any{"qa": "pass", "reviewer": "accepted_with_notes"},
			"team_contract":       testContract(root, []string{"README.md"}),
			"team_gate_verdicts": map[string]any{
				"qa":       luminateam.GateVerdict{Role: "qa", Status: "pass", Summary: "qa pass"},
				"reviewer": luminateam.GateVerdict{Role: "reviewer", Status: "accepted_with_notes", Summary: "nonblocking notes"},
			},
			"team_activity_rows":    []map[string]any{{"agent_id": "backend", "display_name": "Backend", "status": "completed", "summary": "ipc artifact"}},
			"team_dialogue_entries": []map[string]any{},
		},
	})
	mustWriteText(t, filepath.Join(teamRoot, "dialogue.jsonl"), `{"id":"dlg-1","from_agent":"backend","to_agent":["team-leader"],"kind":"response","summary":"Done","content":"Backend result","artifact_refs":["ipc-contract-backend"],"task_id":"task-1","created_at":"2026-07-06T00:00:00Z"}`+"\n")
	mustWriteText(t, filepath.Join(teamRoot, "timeline.jsonl"), `{"type":"team_dialogue","created_at":"2026-07-06T00:00:00Z","payload":{"id":"dlg-1"}}`+"\n")
	mustWriteJSON(t, filepath.Join(teamRoot, "artifacts", "index.json"), []map[string]any{{
		"id": "art-1", "name": "ipc-contract-backend", "owner": "backend", "summary": "IPC contract", "path": filepath.Join(teamRoot, "artifacts", "art-1.md"), "created_at": "2026-07-06T00:00:00Z",
	}})

	manager := luminateam.NewManager(cfg, nil, nil)
	snapshots := manager.RestorePersistedForParent(parent, root)
	if len(snapshots) != 1 {
		t.Fatalf("restored snapshots = %d, want 1", len(snapshots))
	}
	snapshot := snapshots[0]
	if snapshot.TeamSessionID != teamID || len(snapshot.Dialogue) != 1 || len(snapshot.Artifacts) != 1 {
		t.Fatalf("bad restored snapshot: %#v", snapshot)
	}
	if snapshot.TeamContract == nil || snapshot.GateVerdicts["qa"].Status != "pass" || snapshot.GateVerdicts["reviewer"].Status != "accepted_with_notes" {
		t.Fatalf("restored snapshot missing contract/gate verdicts: %#v", snapshot)
	}
	restored, err := manager.Get(teamID)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Dialogue()) != 1 || len(restored.Timeline()) != 1 || len(restored.Artifacts()) != 1 {
		t.Fatalf("restored session content missing")
	}
}

func mustWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	mustWriteText(t, path, string(data)+"\n")
}

func mustWriteText(t *testing.T, path string, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testContract(workdir string, artifacts []string) luminateam.AcceptanceContract {
	return luminateam.AcceptanceContract{
		ProjectRoot:         filepath.Join(workdir, "todolite"),
		UserRequirements:    []string{"deliver TodoLite"},
		ComponentBoundaries: []string{"Go backend", "TypeScript CLI"},
		IntegrationContract: "TypeScript CLI must consume the Go backend API unless explicitly allowed otherwise.",
		RequiredArtifacts:   artifacts,
		RequiredCommands: []luminateam.ContractCheck{
			{Name: "go test", Command: "go test ./...", CWD: filepath.Join(workdir, "todolite", "backend"), Required: true},
			{Name: "npm test", Command: "npm test", CWD: filepath.Join(workdir, "todolite", "cli"), Required: true},
		},
		IntegrationSmokes: []luminateam.ContractCheck{
			{Name: "cli backend smoke", Command: "todo add/list/done/delete through backend", CWD: filepath.Join(workdir, "todolite"), Required: true},
		},
		CompletionCriteria: []string{"QA pass", "Reviewer pass or nonblocking notes"},
	}
}

func recordPassingContractAndGates(t *testing.T, session *luminateam.Session, artifacts []string) {
	t.Helper()
	submitPassingContractAndQA(t, session, t.TempDir(), artifacts)
	session.SubmitGateVerdict("reviewer", luminateam.GateVerdict{
		Role:    "reviewer",
		Status:  "pass",
		Summary: "ok",
	})
}

func submitPassingContractAndQA(t *testing.T, session *luminateam.Session, workdir string, artifacts []string) {
	t.Helper()
	contract := testContract(workdir, artifacts)
	if got := session.RecordContract("team-leader", contract); !strings.Contains(got, "recorded") {
		t.Fatalf("record contract failed: %s", got)
	}
	if got := session.SubmitGateVerdict("qa", luminateam.GateVerdict{
		Role:    "qa",
		Status:  "pass",
		Summary: "all required checks pass",
		Evidence: []luminateam.GateEvidence{
			{Name: "go test", Command: "go test ./...", Passed: true, OutputSummary: "pass"},
			{Name: "npm test", Command: "npm test", Passed: true, OutputSummary: "pass"},
			{Name: "cli backend smoke", Command: "todo add/list/done/delete through backend", Passed: true, OutputSummary: "pass"},
		},
	}); !strings.Contains(got, "recorded") {
		t.Fatalf("submit QA verdict failed: %s", got)
	}
}

func writeRequiredArtifacts(t *testing.T, workdir, project string, artifacts []string) {
	t.Helper()
	for _, rel := range artifacts {
		path := rel
		if !filepath.IsAbs(path) {
			path = filepath.Join(workdir, project, rel)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
