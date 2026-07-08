package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/config"
)

func TestSendA2AWaitTimeoutDoesNotMarkTargetInterruptedByUser(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	session, err := NewManager(cfg, nil, nil).Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	session.mu.Lock()
	session.runCtx = runCtx
	session.mu.Unlock()

	target := session.agents["backend"]
	target.mu.Lock()
	target.busy = true
	target.mu.Unlock()
	defer func() {
		cancel()
		target.mu.Lock()
		target.busy = false
		target.mu.Unlock()
	}()

	result := session.SendA2AMessage(context.Background(), "team-leader", A2AMessageInput{
		To:             []string{"backend"},
		TaskType:       "analysis",
		Message:        "wait briefly",
		TimeoutSeconds: 1,
	})
	results, _ := result["results"].([]map[string]any)
	if len(results) != 1 || results[0]["status"] != "task_pending" {
		t.Fatalf("expected task_pending result, got %#v", result)
	}

	session.mu.Lock()
	row := session.activity["backend"]
	dialogue := append([]DialogueEntry(nil), session.dialogue...)
	session.mu.Unlock()
	if row.Status == "interrupted" || row.Summary == "interrupted by user" {
		t.Fatalf("A2A wait timeout should not be shown as user interrupt: %#v", row)
	}
	if row.Summary != "continuing after A2A wait timeout" {
		t.Fatalf("unexpected timeout activity summary: %#v", row)
	}
	foundTimeoutDialogue := false
	for _, entry := range dialogue {
		if entry.FromAgent == "backend" && entry.Kind == "timeout" {
			foundTimeoutDialogue = true
			break
		}
	}
	if !foundTimeoutDialogue {
		t.Fatalf("expected readable timeout dialogue entry, got %#v", dialogue)
	}

	cancel()
	target.mu.Lock()
	target.busy = false
	target.mu.Unlock()
	waitNoActiveA2A(t, session, "backend")
	waitAgentIdle(t, session, "backend")
	waitTeamPersistenceSettled()
}

func TestFinishA2ATaskNotifiesWaitersAndPreservesCompletedStatus(t *testing.T) {
	session := &Session{
		a2aTasks:       map[string]TeamTask{},
		agentActiveA2A: map[string]string{},
		rootDir:        t.TempDir(),
	}
	task, started := session.beginA2ATask("a2a-test", "leader", "qa", "qa")
	if !started {
		t.Fatal("expected task to start")
	}
	session.finishA2ATask(task.ID, "qa", "completed", "done", "")

	select {
	case got := <-task.done:
		if got.Status != "completed" || got.Result != "done" {
			t.Fatalf("unexpected task notification: %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("expected task completion notification")
	}

	session.finishA2ATask(task.ID, "qa", "error", "", "late timeout")
	session.mu.Lock()
	final := session.a2aTasks[a2aTaskKey(task.ID, "qa")]
	_, active := session.agentActiveA2A["qa"]
	session.mu.Unlock()
	if active {
		t.Fatal("expected active A2A marker to be cleared")
	}
	if final.Status != "completed" || final.Err != "" {
		t.Fatalf("late error should not overwrite completed task: %#v", final)
	}
}

func TestSubmitGateVerdictCompletesActiveGateA2ATask(t *testing.T) {
	cfg := config.NewConfigForCWD(t.TempDir())
	cfg.SessionDir = t.TempDir()
	spec := TeamSpec{
		Name:       "test-team",
		EntryAgent: "team-leader",
		Gates: TeamGateSpec{Checks: []TeamGateCheckSpec{{
			Name:            "qa",
			Agent:           "qa",
			PassStatuses:    []string{"pass"},
			AllowedStatuses: []string{"pass", "fail"},
		}}},
	}
	session := NewSession("parent-session", cfg, spec, nil, nil)
	task, started := session.beginA2ATask("a2a-gate", "team-leader", "qa", "qa-verification")
	if !started {
		t.Fatal("expected gate task to start")
	}

	result := session.SubmitGateVerdict("qa", GateVerdict{
		Status:  "pass",
		Summary: "all checks passed",
		Evidence: []GateEvidence{{
			Name:          "unit-tests",
			Passed:        true,
			OutputSummary: "ok",
		}},
	})
	if result != "Gate verdict recorded." {
		t.Fatalf("unexpected SubmitGateVerdict result: %s", result)
	}

	select {
	case got := <-task.done:
		if got.Status != "completed" || !strings.Contains(got.Result, "all checks passed") {
			t.Fatalf("unexpected gate completion notification: %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("expected gate verdict to complete active A2A task")
	}
	if _, active := session.activeA2ATask("qa"); active {
		t.Fatal("expected gate verdict to clear active A2A task")
	}
}

func TestStageViolationInvalidatesGateVerdictForAgent(t *testing.T) {
	cfg := config.NewConfigForCWD(t.TempDir())
	cfg.SessionDir = t.TempDir()
	session := NewSession("parent-session", cfg, TeamSpec{Name: "test-team", EntryAgent: "team-leader"}, nil, nil)
	session.gateVerdicts = map[string]GateVerdict{
		"qa":       {Role: "qa", AgentID: "qa", Status: "pass"},
		"reviewer": {Role: "reviewer", AgentID: "reviewer", Status: "pass"},
	}
	session.gate = GateStatus{"qa": "pass", "reviewer": "pass"}

	invalidated := session.invalidateGateVerdictsForAgent("reviewer", "stage violation")
	if !stringSliceContains(invalidated, "reviewer") {
		t.Fatalf("expected reviewer gate to be invalidated, got %#v", invalidated)
	}
	if _, ok := session.gateVerdicts["reviewer"]; ok {
		t.Fatal("expected reviewer verdict to be removed after stage violation")
	}
	if _, ok := session.gateVerdicts["qa"]; !ok {
		t.Fatal("expected unrelated qa verdict to remain")
	}
	if _, ok := session.gate["reviewer"]; ok || session.gate["qa"] != "pass" {
		t.Fatalf("unexpected gate status after invalidation: %#v", session.gate)
	}
}

func TestMergeGateVerdictStatusChangeSupersedesOldVerdict(t *testing.T) {
	existing := GateVerdict{
		Role:    "reviewer",
		AgentID: "reviewer",
		Status:  "reject",
		Summary: "README missing",
		Evidence: []GateEvidence{{
			Name:          "readme",
			Passed:        false,
			OutputSummary: "README.md missing",
		}},
		Findings:  []GateFinding{{Summary: "blocking", Blocking: true}},
		CreatedAt: "old",
	}
	next := GateVerdict{
		Role:    "reviewer",
		AgentID: "reviewer",
		Status:  "accepted_with_notes",
		Summary: "README fixed",
		Evidence: []GateEvidence{{
			Name:          "readme",
			Passed:        true,
			OutputSummary: "README.md exists",
		}},
		Findings:  []GateFinding{{Summary: "minor", Blocking: false}},
		CreatedAt: "new",
	}

	merged := mergeGateVerdict(existing, next)
	if merged.Status != "accepted_with_notes" || merged.Summary != "README fixed" || merged.CreatedAt != "new" {
		t.Fatalf("status-changing verdict should supersede old verdict, got %#v", merged)
	}
	if len(merged.Evidence) != 1 || !merged.Evidence[0].Passed || strings.Contains(merged.Evidence[0].OutputSummary, "missing") {
		t.Fatalf("expected stale failing evidence to be dropped, got %#v", merged.Evidence)
	}
	if len(merged.Findings) != 1 || merged.Findings[0].Blocking {
		t.Fatalf("expected latest non-blocking findings only, got %#v", merged.Findings)
	}
}

func TestSendA2AUsesConfiguredDefaultTimeout(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	session, err := NewManager(cfg, nil, nil).Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	if got := session.defaultA2ATimeoutSeconds(); got != 900 {
		t.Fatalf("product-development should use configured default A2A timeout, got %d", got)
	}

	tool := NewSendA2AMessageTool(session, "team-leader")
	if got := tool.TimeoutForInput(A2AMessageInput{To: []string{"qa"}, Message: "verify"}); got != 930*time.Second {
		t.Fatalf("tool timeout should include configured wait plus grace period, got %s", got)
	}
	if got := tool.TimeoutForInput(A2AMessageInput{To: []string{"qa"}, Message: "verify", TimeoutSeconds: 7}); got != 37*time.Second {
		t.Fatalf("explicit A2A timeout should override configured default, got %s", got)
	}
}

func TestTeamLoopWaitsForPendingA2ABeforeNextIteration(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	session, err := NewManager(cfg, nil, nil).Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	if !session.shouldWaitForPendingA2ABeforeNextIteration() {
		t.Fatal("product-development should wait for pending A2A tasks before the next leader iteration")
	}
	task, started := session.beginA2ATask("a2a-wait", "team-leader", "qa", "qa-testing")
	if !started {
		t.Fatal("expected pending qa task to start")
	}
	session.markA2ATaskPending(task.ID, "qa")
	done := make(chan struct{})
	go func() {
		time.Sleep(120 * time.Millisecond)
		session.finishA2ATask(task.ID, "qa", "completed", "ok", "")
		close(done)
	}()

	start := time.Now()
	if !session.waitForPendingA2ABeforeNextIteration(context.Background()) {
		t.Fatal("expected wait to run while A2A task is pending")
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("wait returned too early: %s", elapsed)
	}
	<-done
	if tasks := session.activeA2ATasks(); len(tasks) != 0 {
		t.Fatalf("expected no active A2A tasks after wait, got %#v", tasks)
	}
}

func TestSendA2ABlocksGateAgentsBeforeContract(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	session, err := NewManager(cfg, nil, nil).Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.ensureContractBeforeDispatch("team-leader", A2AMessageInput{
		To:       []string{"backend"},
		TaskType: "technical-plan",
		Message:  "write BACKEND_PLAN.md only",
	}); err != nil {
		t.Fatalf("technical planning before contract should be allowed, got %v", err)
	}
	qaErr := session.ensureContractBeforeDispatch("team-leader", A2AMessageInput{
		To:       []string{"qa"},
		TaskType: "document-check",
		Message:  "review early docs",
	})
	if qaErr == nil || !strings.Contains(qaErr.Error(), "RecordTeamContract") {
		t.Fatalf("QA dispatch before contract should be blocked, got %v", qaErr)
	}
	reviewerErr := session.ensureContractBeforeDispatch("team-leader", A2AMessageInput{
		To:       []string{"reviewer"},
		TaskType: "document-check",
		Message:  "review early docs",
	})
	if reviewerErr == nil || !strings.Contains(reviewerErr.Error(), "RecordTeamContract") {
		t.Fatalf("Reviewer dispatch before contract should be blocked, got %v", reviewerErr)
	}
}

func TestGetTeamContextToolReturnsRuntimeContract(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	session, err := NewManager(cfg, nil, nil).Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	result := session.RecordContract("team-leader", AcceptanceContract{
		ProjectRoot:         workdir,
		UserRequirements:    []string{"build a checklist tool"},
		ComponentBoundaries: []string{"frontend owns CLI", "backend owns storage"},
		IntegrationContract: "CLI calls backend storage manager",
		RequiredArtifacts:   []string{"PRD.md", "QA_REPORT.md"},
		RequiredCommands: []ContractCheck{{
			Name:     "unit tests",
			Command:  "go test ./...",
			CWD:      workdir,
			Required: true,
		}},
		CompletionCriteria: []string{"tests pass"},
	})
	if result != "Team acceptance contract recorded." {
		t.Fatalf("unexpected record result: %s", result)
	}
	tool := NewGetTeamContextTool(session, "qa")
	data, err := tool.Execute(context.Background(), nil, GetTeamContextInput{})
	if err != nil {
		t.Fatal(err)
	}
	var view TeamContextView
	if err := json.Unmarshal([]byte(data), &view); err != nil {
		t.Fatalf("GetTeamContext should return JSON, got %q: %v", data, err)
	}
	if view.CurrentAgent != "qa" || view.Contract == nil {
		t.Fatalf("expected qa context with runtime contract, got %#v", view)
	}
	if got := strings.Join(view.Contract.RequiredArtifacts, ","); got != "PRD.md,QA_REPORT.md" {
		t.Fatalf("unexpected contract artifacts: %s", got)
	}
}

func TestGateCompletionAllowsNonblockingRiskLanguage(t *testing.T) {
	session := &Session{
		Spec: TeamSpec{Gates: TeamGateSpec{Checks: []TeamGateCheckSpec{{
			Name:                 "reviewer",
			Agent:                "reviewer",
			PassStatuses:         []string{"pass"},
			BlockingFindingsFail: true,
		}}}},
		gateVerdicts: map[string]GateVerdict{
			"reviewer": {
				Status:  "pass",
				Summary: "Reviewer pass. Security, correctness, and data-loss risks were considered and are not blocking.",
				Findings: []GateFinding{{
					Category: "security",
					Summary:  "Security risk reviewed",
					Details:  "No exploitable issue found; this is a nonblocking observation.",
					Blocking: false,
				}},
			},
		},
	}
	err := session.validateCompletion(CompleteTeamTaskInput{
		GateStatuses: map[string]string{"reviewer": "pass"},
	})
	if err != nil {
		t.Fatalf("nonblocking risk language should not block completion: %v", err)
	}
}

func TestGateCompletionRejectsStructuredBlockingFinding(t *testing.T) {
	session := &Session{
		Spec: TeamSpec{Gates: TeamGateSpec{Checks: []TeamGateCheckSpec{{
			Name:                 "reviewer",
			Agent:                "reviewer",
			PassStatuses:         []string{"pass"},
			BlockingFindingsFail: true,
		}}}},
		gateVerdicts: map[string]GateVerdict{
			"reviewer": {
				Status: "pass",
				Findings: []GateFinding{{
					Category: "correctness",
					Summary:  "Delete command removes the wrong task",
					Blocking: true,
				}},
			},
		},
	}
	err := session.validateCompletion(CompleteTeamTaskInput{
		GateStatuses: map[string]string{"reviewer": "pass"},
	})
	if err == nil || !strings.Contains(err.Error(), "blocking finding") {
		t.Fatalf("structured blocking finding should block completion, got %v", err)
	}
}

func TestGateCompletionAcceptsNearMatchDeferralReason(t *testing.T) {
	session := &Session{
		Spec: TeamSpec{Gates: TeamGateSpec{
			NonblockingFindings:    "require_followup_or_deferral",
			DeferralRequiresReason: true,
			Checks: []TeamGateCheckSpec{{
				Name:                 "reviewer",
				Agent:                "reviewer",
				PassStatuses:         []string{"pass"},
				BlockingFindingsFail: true,
			}},
		}},
		gateVerdicts: map[string]GateVerdict{
			"reviewer": {
				Status: "pass",
				Findings: []GateFinding{{
					Category: "documentation",
					Summary:  "BACKEND_PLAN.md uses done_task() but INTERFACE_CONTRACT.md and implementation use mark_done()",
					Blocking: false,
				}},
			},
		},
	}
	err := session.validateCompletion(CompleteTeamTaskInput{
		GateStatuses: map[string]string{"reviewer": "pass"},
		DeferralReasons: map[string]string{
			"BACKEND_PLAN.md uses done_task() but INTERFACE_CONTRACT.md and implementation uses mark_done()": "Contract and implementation are authoritative; plan drift is nonblocking.",
		},
	})
	if err != nil {
		t.Fatalf("near-match deferral reason should satisfy nonblocking finding: %v", err)
	}
}

func TestGateCompletionAllowsResolvedNonblockingFindingWithoutDeferral(t *testing.T) {
	session := &Session{
		Spec: TeamSpec{Gates: TeamGateSpec{
			NonblockingFindings:    "require_followup_or_deferral",
			DeferralRequiresReason: true,
			Checks: []TeamGateCheckSpec{{
				Name:         "qa",
				Agent:        "qa",
				PassStatuses: []string{"pass"},
			}},
		}},
		gateVerdicts: map[string]GateVerdict{
			"qa": {
				Status: "pass",
				Findings: []GateFinding{{
					Category: "regression",
					Summary:  "add空描述错误消息已修复并验证通过",
					Details:  "add \"\" now outputs the expected Chinese error and 60/60 tests passed with no regression.",
					Blocking: false,
				}},
			},
		},
	}
	err := session.validateCompletion(CompleteTeamTaskInput{
		GateStatuses: map[string]string{"qa": "pass"},
	})
	if err != nil {
		t.Fatalf("resolved nonblocking finding should not require deferral: %v", err)
	}
}

func TestGateCompletionRejectsUnrelatedDeferralReason(t *testing.T) {
	session := &Session{
		Spec: TeamSpec{Gates: TeamGateSpec{
			NonblockingFindings:    "require_followup_or_deferral",
			DeferralRequiresReason: true,
			Checks: []TeamGateCheckSpec{{
				Name:                 "reviewer",
				Agent:                "reviewer",
				PassStatuses:         []string{"pass"},
				BlockingFindingsFail: true,
			}},
		}},
		gateVerdicts: map[string]GateVerdict{
			"reviewer": {
				Status: "pass",
				Findings: []GateFinding{{
					Category: "correctness",
					Summary:  "add command does not reject pure-whitespace descriptions",
					Blocking: false,
				}},
			},
		},
	}
	err := session.validateCompletion(CompleteTeamTaskInput{
		GateStatuses: map[string]string{"reviewer": "pass"},
		DeferralReasons: map[string]string{
			"Atomic write uses fixed .json.tmp suffix instead of tempfile module": "Acceptable for a single-user CLI.",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "without follow-up or deferral") {
		t.Fatalf("unrelated deferral reason should not satisfy finding, got %v", err)
	}
}

func TestMissingRequiredEvidenceAcceptsCombinedCommandCoverage(t *testing.T) {
	contract := &AcceptanceContract{
		RequiredCommands: []ContractCheck{
			{Name: "CLI冒烟-add", Command: "python mini_tasks.py add '冒烟测试任务'", Required: true},
			{Name: "CLI冒烟-list", Command: "python mini_tasks.py list", Required: true},
			{Name: "CLI冒烟-done", Command: "python mini_tasks.py done 1", Required: true},
			{Name: "CLI冒烟-delete", Command: "python mini_tasks.py delete 1", Required: true},
			{Name: "CLI冒烟-错误缺少参数", Command: "python mini_tasks.py add 2>&1; echo $?", Required: true},
		},
		IntegrationSmokes: []ContractCheck{
			{Name: "端到端流程", Command: "python mini_tasks.py add '测试任务' && python mini_tasks.py list && python mini_tasks.py done 1 && python mini_tasks.py delete 1 && python mini_tasks.py list", Required: true},
			{Name: "完整CRUD流程", Command: "python mini_tasks.py add 集成测试 && python mini_tasks.py list && python mini_tasks.py done 1 && python mini_tasks.py delete 1", Required: true},
			{Name: "错误路径-无效序号", Command: "python mini_tasks.py done abc 2>&1; test $? -eq 1", Required: true},
			{Name: "错误路径-序号不存在", Command: "python mini_tasks.py done 99 2>&1; test $? -eq 1", Required: true},
		},
	}
	evidence := []GateEvidence{
		{
			Name:          "CLI冒烟测试",
			Command:       "python mini_tasks.py add \"冒烟测试\" && python mini_tasks.py list && python mini_tasks.py done 1 && python mini_tasks.py delete 1 && python mini_tasks.py list",
			Passed:        true,
			OutputSummary: "add/list/done/delete/list-empty 全部输出正确中文，exit code 0",
		},
		{
			Name:          "错误路径测试",
			Command:       "python mini_tasks.py add 2>&1; echo $? && python mini_tasks.py done abc 2>&1; echo $? && python mini_tasks.py done 99 2>&1; echo $?",
			Passed:        true,
			OutputSummary: "add无参数 exit1, done abc exit1, done 99 exit1",
		},
		{
			Name:          "CRUD集成烟雾测试",
			Command:       "python mini_tasks.py add 集成测试任务 && python mini_tasks.py list && python mini_tasks.py done 1 && python mini_tasks.py delete 1",
			Passed:        true,
			OutputSummary: "完整 add/list/done/delete 流程通过",
		},
	}
	if missing := missingRequiredEvidence(contract, evidence); len(missing) != 0 {
		t.Fatalf("combined evidence should cover required commands, missing %#v", missing)
	}
}

func TestMissingRequiredEvidenceAcceptsLooseCommandNameEvidence(t *testing.T) {
	contract := &AcceptanceContract{RequiredCommands: []ContractCheck{
		{Name: "add命令验证", Command: "python -m mini_tasks add \"测试任务\"", Required: true},
		{Name: "list命令验证", Command: "python -m mini_tasks list", Required: true},
		{Name: "done命令验证", Command: "python -m mini_tasks done 1", Required: true},
		{Name: "delete命令验证", Command: "python -m mini_tasks delete 1", Required: true},
		{Name: "错误场景验证", Command: "python -m mini_tasks done 999", Required: true},
	}}
	evidence := []GateEvidence{
		{Name: "add 命令", Command: "python -m mini_tasks add \"测试任务\"", Passed: true, OutputSummary: "exit 0"},
		{Name: "list 命令", Command: "python -m mini_tasks list", Passed: true, OutputSummary: "exit 0"},
		{Name: "done 命令", Command: "python -m mini_tasks done 1", Passed: true, OutputSummary: "exit 0"},
		{Name: "delete 命令", Command: "python -m mini_tasks delete 1", Passed: true, OutputSummary: "exit 0"},
		{Name: "错误路径", Command: "python -m mini_tasks done 999", Passed: true, OutputSummary: "exit 1"},
	}
	if missing := missingRequiredEvidence(contract, evidence); len(missing) != 0 {
		t.Fatalf("loose command-name evidence should cover required checks, missing %#v", missing)
	}
}

func TestMissingRequiredEvidenceDoesNotConfuseDifferentCommandArguments(t *testing.T) {
	contract := &AcceptanceContract{IntegrationSmokes: []ContractCheck{
		{Name: "错误路径-无效序号", Command: "python mini_tasks.py done abc 2>&1; test $? -eq 1", Required: true},
		{Name: "错误路径-序号不存在", Command: "python mini_tasks.py done 99 2>&1; test $? -eq 1", Required: true},
	}}
	evidence := []GateEvidence{{
		Name:          "错误路径测试",
		Command:       "python mini_tasks.py done 99 2>&1; echo $?",
		Passed:        true,
		OutputSummary: "done 99 exit1",
	}}
	missing := missingRequiredEvidence(contract, evidence)
	got := strings.Join(missing, ",")
	if !strings.Contains(got, "错误路径-无效序号") || strings.Contains(got, "错误路径-序号不存在") {
		t.Fatalf("done 99 evidence should not cover done abc, missing %#v", missing)
	}
}

func TestGenericTeamDispatchGuardDoesNotHardcodeImplementationPolicy(t *testing.T) {
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = t.TempDir()
	cfg.SessionDir = t.TempDir()
	loader := NewLoader(cfg)
	if _, err := loader.CreateTemplate("Generic Runtime Team"); err != nil {
		t.Fatal(err)
	}
	session, err := NewManager(cfg, nil, nil).Start("parent-session", "generic-runtime-team", workdir)
	if err != nil {
		t.Fatal(err)
	}
	err = session.ensureContractBeforeDispatch("team-leader", A2AMessageInput{
		To:       []string{"team-leader"},
		TaskType: "implementation",
		Message:  "generic implementation task",
	})
	if err != nil {
		t.Fatalf("generic team without task_policies should not get product-development dispatch rules: %v", err)
	}
}

func TestSendA2ADoesNotQueueDuplicateWorkForActiveTarget(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	session, err := NewManager(cfg, nil, nil).Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	session.mu.Lock()
	session.runCtx = runCtx
	session.mu.Unlock()

	target := session.agents["ux-design"]
	target.mu.Lock()
	target.busy = true
	target.mu.Unlock()
	defer func() {
		cancel()
		target.mu.Lock()
		target.busy = false
		target.mu.Unlock()
	}()

	first := session.SendA2AMessage(context.Background(), "team-leader", A2AMessageInput{
		To:             []string{"ux-design"},
		TaskType:       "ux-design",
		Message:        "produce design",
		TimeoutSeconds: 1,
	})
	firstResults, _ := first["results"].([]map[string]any)
	if len(firstResults) != 1 || firstResults[0]["status"] != "task_pending" {
		t.Fatalf("expected first dispatch to become pending, got %#v", first)
	}
	firstID, _ := firstResults[0]["task_id"].(string)
	if firstID == "" {
		t.Fatalf("pending result should include task id: %#v", first)
	}

	session.mu.Lock()
	dialogueBefore := len(session.dialogue)
	session.mu.Unlock()

	second := session.SendA2AMessage(context.Background(), "team-leader", A2AMessageInput{
		To:             []string{"ux-design"},
		TaskType:       "ux-design",
		Message:        "produce design again",
		TimeoutSeconds: 1,
	})
	secondResults, _ := second["results"].([]map[string]any)
	if len(secondResults) != 1 || secondResults[0]["existing_task_id"] != firstID {
		t.Fatalf("expected duplicate dispatch to return existing task, got %#v", second)
	}

	session.mu.Lock()
	dialogueAfter := len(session.dialogue)
	session.mu.Unlock()
	statusText := session.a2aTaskStatusText()
	if dialogueAfter != dialogueBefore {
		t.Fatalf("duplicate active dispatch should not append a second message, before=%d after=%d", dialogueBefore, dialogueAfter)
	}
	if !strings.Contains(statusText, firstID) || !strings.Contains(statusText, "pending") {
		t.Fatalf("leader status text should expose pending A2A task, got %q", statusText)
	}

	cancel()
	target.mu.Lock()
	target.busy = false
	target.mu.Unlock()
	waitNoActiveA2A(t, session, "ux-design")
	waitAgentIdle(t, session, "ux-design")
	waitTeamPersistenceSettled()
}

func TestMemberMessageToEntryAgentIsDeliveredWithoutStartingLeaderTask(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	session, err := NewManager(cfg, nil, nil).Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}

	result := session.SendA2AMessage(context.Background(), "ux-design", A2AMessageInput{
		To:       []string{"team-leader"},
		TaskType: "ux-design",
		Message:  "Design artifact is ready.",
	})
	results, _ := result["results"].([]map[string]any)
	if len(results) != 1 || results[0]["status"] != "delivered" {
		t.Fatalf("member-to-leader message should be delivered without dispatch, got %#v", result)
	}
	if _, active := session.activeA2ATask("team-leader"); active {
		t.Fatal("member-to-leader message should not create an active A2A task for the entry agent")
	}
	leader := session.agents["team-leader"]
	leader.mu.Lock()
	leaderBusy := leader.busy
	leader.mu.Unlock()
	if leaderBusy {
		t.Fatal("member-to-leader message should not start the Team Leader runtime")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.dialogue) != 1 || session.dialogue[0].FromAgent != "ux-design" || session.dialogue[0].ToAgent[0] != "team-leader" {
		t.Fatalf("expected delivered message in dialogue, got %#v", session.dialogue)
	}
}

func waitNoActiveA2A(t *testing.T, session *Session, target string) {
	t.Helper()
	for i := 0; i < 80; i++ {
		if _, active := session.activeA2ATask(target); !active {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("A2A task for %s did not stop after cancellation", target)
}

func waitAgentIdle(t *testing.T, session *Session, agentID string) {
	t.Helper()
	runtime := session.agents[agentID]
	if runtime == nil {
		t.Fatalf("agent %s not found", agentID)
	}
	for i := 0; i < 80; i++ {
		runtime.mu.Lock()
		busy := runtime.busy
		runtime.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("agent %s did not become idle", agentID)
}

func waitTeamPersistenceSettled() {
	time.Sleep(250 * time.Millisecond)
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestTeamSharedPromptIsInjectedIntoLeaderAndAgentSystemPrompts(t *testing.T) {
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = t.TempDir()
	cfg.SessionDir = t.TempDir()
	loader := NewLoader(cfg)
	result, err := loader.CreateTemplate("Shared Prompt Team")
	if err != nil {
		t.Fatal(err)
	}
	shared := "Shared marker: follow PRD before implementation."
	if err := os.WriteFile(filepath.Join(result.Path, SharedPromptFile), []byte(shared), 0o644); err != nil {
		t.Fatal(err)
	}
	session, err := NewManager(cfg, nil, nil).Start("parent-session", "shared-prompt-team", workdir)
	if err != nil {
		t.Fatal(err)
	}

	leaderPrompt := session.leaderPrompt("build something", 1)
	if !strings.Contains(leaderPrompt, "Shared team prompt:") || !strings.Contains(leaderPrompt, shared) {
		t.Fatalf("leader prompt should include shared prompt, got %q", leaderPrompt)
	}

	runtime := session.agents["team-leader"]
	if runtime == nil || runtime.Engine == nil {
		t.Fatal("missing team-leader runtime")
	}
	if !strings.Contains(runtime.Engine.Config.SystemPromptPath, filepath.Join(session.rootDir, "prompts")) {
		t.Fatalf("team agent prompt should be materialized under session root, got %s", runtime.Engine.Config.SystemPromptPath)
	}
	data, err := os.ReadFile(runtime.Engine.Config.SystemPromptPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "# Team Leader") || !strings.Contains(text, "# Shared Team Prompt") || !strings.Contains(text, shared) {
		t.Fatalf("materialized agent prompt missing base or shared content:\n%s", text)
	}
	if strings.Index(text, "# Shared Team Prompt") > strings.Index(text, "# Team Leader") {
		t.Fatalf("shared prompt should appear before agent prompt:\n%s", text)
	}
	if len(runtime.State.Messages) != 0 {
		t.Fatalf("shared prompt should not be injected into conversation messages, got %#v", runtime.State.Messages)
	}
}

func TestLeaderPromptShortensLargeSharedPromptButAgentPromptKeepsFullText(t *testing.T) {
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = t.TempDir()
	cfg.SessionDir = t.TempDir()
	result, err := NewLoader(cfg).CreateTemplate("Large Shared Team")
	if err != nil {
		t.Fatal(err)
	}
	shared := "Shared intro.\n" + strings.Repeat("full shared workflow detail. ", 120) + "\nShared tail marker."
	if err := os.WriteFile(filepath.Join(result.Path, SharedPromptFile), []byte(shared), 0o644); err != nil {
		t.Fatal(err)
	}
	session, err := NewManager(cfg, nil, nil).Start("parent-session", "large-shared-team", workdir)
	if err != nil {
		t.Fatal(err)
	}

	leaderPrompt := session.leaderPrompt("build something", 1)
	if !strings.Contains(leaderPrompt, "Shared team prompt:") {
		t.Fatalf("leader prompt should include shared prompt section, got %q", leaderPrompt)
	}
	if !strings.Contains(leaderPrompt, "Shared prompt shortened here") {
		t.Fatalf("large shared prompt should be shortened in leader loop prompt, got %q", leaderPrompt)
	}
	if strings.Contains(leaderPrompt, "Shared tail marker.") {
		t.Fatalf("leader loop prompt should not include the entire large shared prompt")
	}

	runtime := session.agents["team-leader"]
	if runtime == nil || runtime.Engine == nil {
		t.Fatal("missing team-leader runtime")
	}
	data, err := os.ReadFile(runtime.Engine.Config.SystemPromptPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "Shared intro.") || !strings.Contains(text, "Shared tail marker.") {
		t.Fatalf("materialized agent system prompt should keep full shared prompt:\n%s", text)
	}
}

func TestEmptyTeamSharedPromptDoesNotAddLeaderSection(t *testing.T) {
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = t.TempDir()
	cfg.SessionDir = t.TempDir()
	if _, err := NewLoader(cfg).CreateTemplate("Empty Shared Team"); err != nil {
		t.Fatal(err)
	}
	session, err := NewManager(cfg, nil, nil).Start("parent-session", "empty-shared-team", workdir)
	if err != nil {
		t.Fatal(err)
	}
	prompt := session.leaderPrompt("build something", 1)
	if strings.Contains(prompt, "Shared team prompt:") {
		t.Fatalf("empty shared prompt should not add a leader section, got %q", prompt)
	}
}

func TestProductDevelopmentAgentSkillsAreIsolatedAndModelInvocable(t *testing.T) {
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.SessionDir = t.TempDir()
	session, err := NewManager(cfg, nil, nil).Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}

	backendNames := session.AgentSkillNames("backend")
	if !stringSliceContains(backendNames, "ipc-contract") || !stringSliceContains(backendNames, "persistence-plan") {
		t.Fatalf("backend should see backend private skills, got %v", backendNames)
	}
	if stringSliceContains(backendNames, "qa-report") || stringSliceContains(backendNames, "code-review-checklist") {
		t.Fatalf("backend should not see qa/reviewer private skills, got %v", backendNames)
	}

	qaNames := session.AgentSkillNames("qa")
	if !stringSliceContains(qaNames, "acceptance-runbook") || !stringSliceContains(qaNames, "qa-report") {
		t.Fatalf("qa should see qa private skills, got %v", qaNames)
	}
	if stringSliceContains(qaNames, "ipc-contract") || stringSliceContains(qaNames, "code-review-checklist") {
		t.Fatalf("qa should not see backend/reviewer private skills, got %v", qaNames)
	}

	runtime := session.agents["backend"]
	if runtime == nil || runtime.Engine == nil || runtime.Engine.SkillRegistry() == nil {
		t.Fatal("missing backend skill registry")
	}
	visibleToModel := runtime.Engine.SkillRegistry().ListModelInvocable(workdir)
	modelNames := make([]string, 0, len(visibleToModel))
	for _, skill := range visibleToModel {
		modelNames = append(modelNames, skill.CanonicalName)
		if skill.CanonicalName == "ipc-contract" && skill.Frontmatter.DisableModelInvocation {
			t.Fatalf("ipc-contract should be model invocable")
		}
	}
	if !stringSliceContains(modelNames, "ipc-contract") {
		t.Fatalf("backend model-visible skills should include ipc-contract, got %v", modelNames)
	}
}

func TestCompleteTeamTaskSchemaUsesGenericGateStatuses(t *testing.T) {
	schema := NewCompleteTeamTaskTool(nil, "team-leader").ToAPISchema()
	data, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "qa_status") || strings.Contains(text, "reviewer_status") {
		t.Fatalf("CompleteTeamTask schema should not expose legacy QA/Reviewer status fields: %s", text)
	}
	if !strings.Contains(text, "gate_statuses") {
		t.Fatalf("CompleteTeamTask schema should expose generic gate_statuses: %s", text)
	}
}

func TestConfiguredTaskPolicyStageGuardForbidsConfiguredWrites(t *testing.T) {
	policies := []TeamTaskPolicySpec{{
		Name:              "docs-only",
		Description:       "planning docs only",
		AuditWrites:       true,
		AllowedWriteGlobs: []string{"*.md", "**/*.md"},
		DeniedWriteGlobs:  []string{"**/*.py", "**/static/**"},
	}}
	guard := teamTaskStageGuard("backend", "technical-plan", []string{"tiny-notes/BACKEND_PLAN.md", "tiny-notes/INTERFACE_CONTRACT.md"}, policies)
	for _, want := range []string{
		"Active policy: docs-only",
		"planning docs only",
		"Allowed write globs: *.md, **/*.md",
		"Denied write globs: **/*.py, **/static/**",
		"tiny-notes/BACKEND_PLAN.md",
		"tiny-notes/INTERFACE_CONTRACT.md",
	} {
		if !strings.Contains(guard, want) {
			t.Fatalf("task policy guard should contain %q, got %q", want, guard)
		}
	}
}

func TestConfiguredTaskPolicyExclusiveWorkspaceGuard(t *testing.T) {
	policies := []TeamTaskPolicySpec{{
		Name:               "exclusive-gate",
		Description:        "acceptance must be isolated",
		ExclusiveWorkspace: true,
	}}
	guard := teamTaskStageGuard("qa", "qa-verification", []string{"QA_REPORT.md"}, policies)
	if !requiresExclusiveWorkspace(policies) {
		t.Fatal("expected exclusive workspace policy to require workspace lock")
	}
	if !strings.Contains(guard, "exclusive workspace access") {
		t.Fatalf("stage guard should mention exclusive workspace policy, got %q", guard)
	}
}

func TestProductDevelopmentTeamLeaderLoopCannotModifyImplementationFiles(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	session, err := NewManager(cfg, nil, nil).Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	policies := session.matchTaskPolicies([]string{"team-leader"}, "team-loop", false)
	if len(policies) == 0 {
		t.Fatal("expected team-leader team-loop policy")
	}

	before := snapshotWorkspaceFiles(workdir)
	writeTestFile(t, filepath.Join(workdir, "daily-notes", "README.md"), "# ok\n")
	writeTestFile(t, filepath.Join(workdir, "daily-notes", "daily_notes", "cli.py"), "print('bad')\n")
	writeTestFile(t, filepath.Join(workdir, "daily-notes", "pyproject.toml"), "[project]\n")
	violations := taskWriteViolations(workdir, before, snapshotWorkspaceFiles(workdir), nil, policies)
	if !stringSliceContains(violations, "daily-notes/daily_notes/cli.py") || !stringSliceContains(violations, "daily-notes/pyproject.toml") {
		t.Fatalf("team leader loop should reject implementation/config writes, got %v", violations)
	}
	if stringSliceContains(violations, "daily-notes/README.md") {
		t.Fatalf("team leader loop should allow markdown coordination artifacts, got %v", violations)
	}
}

func TestProductDevelopmentDeliveryWorkUsesExclusiveWorkspaceAudit(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	session, err := NewManager(cfg, nil, nil).Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	policies := session.matchTaskPolicies([]string{"frontend"}, "implementation", false)
	if len(policies) == 0 {
		t.Fatal("expected implementation policy")
	}
	if !requiresExclusiveWorkspace(policies) {
		t.Fatalf("implementation writes should be exclusive to avoid cross-agent audit attribution, got %#v", policies)
	}
	guard := teamTaskStageGuard("frontend", "implementation", []string{"daily_notes/cli.py"}, policies)
	if !strings.Contains(guard, "exclusive workspace access") || !strings.Contains(guard, "Writes are restricted") {
		t.Fatalf("implementation guard should mention exclusive audited writes, got %q", guard)
	}
}

func TestConfiguredTaskPolicyWriteViolationsRejectDeniedArtifacts(t *testing.T) {
	root := t.TempDir()
	before := snapshotWorkspaceFiles(root)
	writeTestFile(t, filepath.Join(root, "tiny-notes", "BACKEND_PLAN.md"), "# Backend plan\n")
	writeTestFile(t, filepath.Join(root, "tiny-notes", "INTERFACE_CONTRACT.md"), "# Contract\n")
	writeTestFile(t, filepath.Join(root, "tiny-notes", "server.py"), "print('oops')\n")
	writeTestFile(t, filepath.Join(root, "tiny-notes", "static", "index.html"), "<!doctype html>\n")
	after := snapshotWorkspaceFiles(root)

	policies := []TeamTaskPolicySpec{{
		Name:              "docs-only",
		AuditWrites:       true,
		AllowedWriteGlobs: []string{"*.md", "**/*.md"},
		DeniedWriteGlobs:  []string{"**/*.py", "**/static/**", "**/static/"},
	}}
	violations := taskWriteViolations(root, before, after, []string{"tiny-notes/BACKEND_PLAN.md", "tiny-notes/INTERFACE_CONTRACT.md"}, policies)
	got := strings.Join(violations, "\n")
	if !strings.Contains(got, "tiny-notes/server.py") || !strings.Contains(got, "tiny-notes/static/index.html") {
		t.Fatalf("expected denied artifacts to be rejected, got %#v", violations)
	}
	if strings.Contains(got, "BACKEND_PLAN.md") || strings.Contains(got, "INTERFACE_CONTRACT.md") {
		t.Fatalf("expected planning docs to be allowed, got %#v", violations)
	}
}

func TestConfiguredTaskPolicyDeniedGlobsOverrideExpectedArtifacts(t *testing.T) {
	root := t.TempDir()
	before := snapshotWorkspaceFiles(root)
	writeTestFile(t, filepath.Join(root, "tiny-notes", "server.py"), "print('oops')\n")
	after := snapshotWorkspaceFiles(root)

	policies := []TeamTaskPolicySpec{{
		Name:              "docs-only",
		AuditWrites:       true,
		AllowedWriteGlobs: []string{"*.md", "**/*.md"},
		DeniedWriteGlobs:  []string{"**/*.py"},
	}}
	violations := taskWriteViolations(root, before, after, []string{"tiny-notes/server.py"}, policies)
	got := strings.Join(violations, "\n")
	if !strings.Contains(got, "tiny-notes/server.py") {
		t.Fatalf("expected denied glob to override expected_artifacts allowlist, got %#v", violations)
	}
}

func TestConfiguredTaskPolicyCanRestrictWritesToExpectedArtifacts(t *testing.T) {
	root := t.TempDir()
	before := snapshotWorkspaceFiles(root)
	writeTestFile(t, filepath.Join(root, "cli.py"), "print('ok')\n")
	writeTestFile(t, filepath.Join(root, "tests", "test_cli.py"), "def test_ok(): pass\n")
	writeTestFile(t, filepath.Join(root, "pattern.py"), "print('unrelated')\n")
	writeTestFile(t, filepath.Join(root, ".pytest_cache", "README.md"), "cache\n")
	writeTestFile(t, filepath.Join(root, "data", "releases.json"), "[]\n")
	after := snapshotWorkspaceFiles(root)

	policies := []TeamTaskPolicySpec{{
		Name:                              "expected-only",
		AuditWrites:                       true,
		RestrictWritesToExpectedArtifacts: true,
		AllowedWriteGlobs:                 []string{".pytest_cache/**", "**/.pytest_cache/**", "data/**", "**/data/**"},
	}}
	violations := taskWriteViolations(root, before, after, []string{"cli.py", "tests/test_cli.py"}, policies)
	got := strings.Join(violations, "\n")
	if !strings.Contains(got, "pattern.py") {
		t.Fatalf("expected unrelated file to violate expected-artifact write policy, got %#v", violations)
	}
	for _, allowed := range []string{"cli.py", "tests/test_cli.py", ".pytest_cache/README.md", "data/releases.json"} {
		if strings.Contains(got, allowed) {
			t.Fatalf("expected %s to be allowed, got violations %#v", allowed, violations)
		}
	}
}

func TestConfiguredTaskPolicyRejectsDeletedWriteAttempts(t *testing.T) {
	root := t.TempDir()
	tempPath := filepath.Join(root, "tests", "_check_argparse.py")

	policies := []TeamTaskPolicySpec{{
		Name:                              "expected-only",
		AuditWrites:                       true,
		RestrictWritesToExpectedArtifacts: true,
		AllowedWriteGlobs:                 []string{".pytest_cache/**", "**/.pytest_cache/**"},
	}}
	violations := taskWriteAttemptViolations(root, []string{tempPath}, []string{"tests/test_cli.py"}, policies)
	if !stringSliceContains(violations, "tests/_check_argparse.py") {
		t.Fatalf("expected deleted temporary write attempt to violate expected-artifact policy, got %#v", violations)
	}

	allowed := taskWriteAttemptViolations(root, []string{filepath.Join(root, "tests", "test_cli.py")}, []string{"tests/test_cli.py"}, policies)
	if len(allowed) > 0 {
		t.Fatalf("expected declared artifact write attempt to be allowed, got %#v", allowed)
	}
}

func TestWorkspaceAuditAllowsContractProjectRootRelativeExpectedArtifacts(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "mini-tasks")
	before := snapshotWorkspaceFiles(root)
	writeTestFile(t, filepath.Join(projectRoot, "mini_tasks", "tasks.py"), "print('ok')\n")
	after := snapshotWorkspaceFiles(root)

	session := &Session{
		Config: config.Config{CWD: root},
		contract: &AcceptanceContract{
			ProjectRoot: projectRoot,
		},
	}
	policies := []TeamTaskPolicySpec{{
		Name:                              "expected-only",
		AuditWrites:                       true,
		RestrictWritesToExpectedArtifacts: true,
	}}
	expected := session.expectedArtifactsForWorkspaceAudit([]string{"mini_tasks/tasks.py"})
	violations := taskWriteViolations(root, before, after, expected, policies)
	if len(violations) > 0 {
		t.Fatalf("expected project-root-relative artifact to be allowed, got %#v", violations)
	}
}

func TestExtractArtifactsRegistersExpectedFiles(t *testing.T) {
	workdir := t.TempDir()
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(workdir, "BACKEND_PLAN.md"), "# Backend plan\n")
	writeTestFile(t, filepath.Join(workdir, "INTERFACE_CONTRACT.md"), "# Interface contract\n")
	session := &Session{
		ID:      "team-test",
		Config:  config.NewConfigForCWD(workdir),
		rootDir: rootDir,
	}

	refs := session.extractArtifacts("backend", "backend-plan", "done", []string{filepath.Join(workdir, "BACKEND_PLAN.md"), "INTERFACE_CONTRACT.md"})
	if len(refs) != 2 {
		t.Fatalf("expected both expected artifact files to be registered, got refs=%#v artifacts=%#v", refs, session.artifacts)
	}
	paths := map[string]string{}
	for _, artifact := range session.artifacts {
		paths[artifact.Name] = artifact.Path
	}
	for _, name := range []string{"BACKEND_PLAN.md", "INTERFACE_CONTRACT.md"} {
		if paths[name] != filepath.Join(workdir, name) {
			t.Fatalf("expected artifact %s to point at project file, got paths=%#v", name, paths)
		}
	}
}

func TestRecordContractRegistersExistingRequiredArtifacts(t *testing.T) {
	workdir := t.TempDir()
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(workdir, "PRD.md"), "# PRD\n")
	writeTestFile(t, filepath.Join(workdir, "INTERFACE_CONTRACT.md"), "# Contract\n")
	session := &Session{
		ID:     "team-test",
		Config: config.NewConfigForCWD(workdir),
		Spec: TeamSpec{
			EntryAgent: "team-leader",
		},
		rootDir: rootDir,
	}

	got := session.RecordContract("team-leader", AcceptanceContract{
		ProjectRoot:         workdir,
		UserRequirements:    []string{"ship"},
		IntegrationContract: "local CLI uses storage contract",
		RequiredArtifacts:   []string{"PRD.md", filepath.Join(workdir, "INTERFACE_CONTRACT.md")},
		RequiredCommands:    []ContractCheck{{Name: "tests", Command: "pytest", Required: true}},
		CompletionCriteria:  []string{"tests pass"},
	})
	if !strings.Contains(got, "recorded") {
		t.Fatalf("record contract failed: %s", got)
	}
	paths := map[string]string{}
	for _, artifact := range session.artifacts {
		paths[artifact.Name] = artifact.Path
	}
	for _, name := range []string{"PRD.md", "INTERFACE_CONTRACT.md"} {
		if paths[name] != filepath.Join(workdir, name) {
			t.Fatalf("expected required artifact %s to be registered, got %#v", name, paths)
		}
	}
}

func TestWriteFileToolResultRegistersWorkspaceArtifact(t *testing.T) {
	workdir := t.TempDir()
	rootDir := t.TempDir()
	session := &Session{
		ID:      "team-test",
		Config:  config.NewConfigForCWD(workdir),
		rootDir: rootDir,
	}
	reportPath := filepath.Join(workdir, "mini-tasks", "INTEGRATION_REPORT.md")
	writeTestFile(t, reportPath, "# Integration\n\nAll checks passed.\n")

	session.recordWriteFileArtifactFromEvent("team-leader", agent.StreamEvent{
		Metadata: map[string]any{
			"tool_name": "write_file",
			"result":    "File written successfully: " + reportPath + " (33 characters)",
		},
	})
	if len(session.artifacts) != 1 {
		t.Fatalf("expected written workspace file to be registered, got %#v", session.artifacts)
	}
	if session.artifacts[0].Name != "mini-tasks/INTEGRATION_REPORT.md" || session.artifacts[0].Path != reportPath {
		t.Fatalf("unexpected artifact: %#v", session.artifacts[0])
	}

	outsidePath := filepath.Join(t.TempDir(), "outside.md")
	writeTestFile(t, outsidePath, "# Outside\n")
	session.recordWriteFileArtifactFromEvent("team-leader", agent.StreamEvent{
		Metadata: map[string]any{
			"tool_name": "write_file",
			"result":    "File written successfully: " + outsidePath + " (10 characters)",
		},
	})
	if len(session.artifacts) != 1 {
		t.Fatalf("outside workspace write should not be registered, got %#v", session.artifacts)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTeamAgentRuntimeSessionDirIsNestedUnderTeamSession(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	sessionRoot := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = sessionRoot
	session, err := NewManager(cfg, nil, nil).Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	backend := session.agents["backend"]
	want := filepath.Join(session.rootDir, "agents")
	if backend.Engine.Config.SessionDir != want {
		t.Fatalf("team agent session dir = %s, want %s", backend.Engine.Config.SessionDir, want)
	}
	if backend.Engine.Config.SessionMemoryDir != sessionRoot {
		t.Fatalf("team agent session memory dir = %s, want parent session root %s", backend.Engine.Config.SessionMemoryDir, sessionRoot)
	}
	if _, err := os.Stat(filepath.Join(sessionRoot, session.ID+"-backend")); !os.IsNotExist(err) {
		t.Fatalf("team agent must not create top-level role session dir, stat err=%v", err)
	}
}
