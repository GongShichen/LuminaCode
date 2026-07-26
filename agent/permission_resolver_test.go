package agent

import (
	"context"
	"strings"
	"testing"

	"LuminaCode/config"
	coretools "LuminaCode/tools"
)

type unavailableSandboxTool struct {
	coretools.BaseTool
	executed bool
}

func newUnavailableSandboxTool() *unavailableSandboxTool {
	return &unavailableSandboxTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name: "run_shell", InputPrototype: coretools.BashInput{}, ReadOnly: coretools.BoolPtr(true), ConcurrencySafe: coretools.BoolPtr(false),
	}}}
}

func (t *unavailableSandboxTool) SandboxStatus() (bool, string, string) {
	return false, "linux", "bwrap"
}

func (t *unavailableSandboxTool) Execute(context.Context, coretools.ExecutionContext, any) (string, error) {
	t.executed = true
	return "executed", nil
}

type setupUnavailableSandboxTool struct {
	*unavailableSandboxTool
	canInstall         bool
	allowLocalFallback bool
}

func newSetupUnavailableSandboxTool() *setupUnavailableSandboxTool {
	return &setupUnavailableSandboxTool{unavailableSandboxTool: &unavailableSandboxTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name: "run_shell", InputPrototype: coretools.BashInput{}, ReadOnly: coretools.BoolPtr(false), ConcurrencySafe: coretools.BoolPtr(false), Destructive: coretools.BoolPtr(true),
	}}}, canInstall: true, allowLocalFallback: false}
}

func (t *setupUnavailableSandboxTool) SandboxSetupRequest(coretools.ExecutionContext, any) (bool, map[string]any) {
	return true, map[string]any{
		"sandbox_setup_request": map[string]any{
			"kind":                 "wsl-bwrap",
			"distro":               "LuminaSandbox",
			"reason":               "WSL sandbox distro is not installed.",
			"can_install":          t.canInstall,
			"allow_local_fallback": t.allowLocalFallback,
		},
		"risk":      "high",
		"dangerous": true,
	}
}

func TestSandboxUnavailablePermissionEnablesYoloEvenForReadOnlyCommand(t *testing.T) {
	tool := newUnavailableSandboxTool()
	registry := coretools.NewToolRegistry(tool)
	state := NewAgentState()
	executor := NewStreamingToolExecutor(registry, testExecutorConfig(), &state, coretools.ExecutionContext{"parent_state": &state})
	call := coretools.ToolCall{ID: "shell", Name: "run_shell", Input: map[string]any{"command": "git status"}}
	executor.AddTool(call)

	prompted := false
	enabled := false
	resolver := PermissionResolver{
		Registry: registry,
		RequestDecision: func(_ context.Context, event StreamEvent) (string, string) {
			prompted = event.Metadata["sandbox_unavailable"] == true && strings.Contains(stringFromAny(event.Metadata["reason"]), "enable YOLO mode")
			return PermissionOnce, "run_shell"
		},
		EnableYolo: func(*AgentState) { enabled = true },
	}
	events := resolver.Resolve(context.Background(), []coretools.ToolCall{call}, executor, &state, nil)
	executor.GetRemainingResults(context.Background())
	if !prompted || !enabled || !state.YoloEnabled() || !tool.executed {
		t.Fatalf("sandbox fallback approval mismatch: prompted=%t enabled=%t yolo=%t executed=%t", prompted, enabled, state.YoloEnabled(), tool.executed)
	}
	if len(events) != 1 || !strings.Contains(events[0].Content, "YOLO mode enabled") {
		t.Fatalf("expected a user-visible YOLO notice, got %#v", events)
	}
}

func TestSandboxSetupApprovalStillRequiresCommandPermission(t *testing.T) {
	tool := newSetupUnavailableSandboxTool()
	registry := coretools.NewToolRegistry(tool)
	state := NewAgentState()
	executor := NewStreamingToolExecutor(registry, testExecutorConfig(), &state, coretools.ExecutionContext{"parent_state": &state})
	call := coretools.ToolCall{ID: "shell", Name: "run_shell", Input: map[string]any{"command": "pwd"}}
	executor.AddTool(call)

	prompts := 0
	resolver := PermissionResolver{
		Registry: registry,
		RequestDecision: func(_ context.Context, event StreamEvent) (string, string) {
			prompts++
			if prompts == 1 && event.Metadata["sandbox_setup_request"] == nil {
				t.Fatalf("unexpected non-setup permission prompt: %#v", event.Metadata)
			}
			if prompts == 2 && event.Metadata["sandbox_setup_request"] != nil {
				t.Fatalf("setup approval must be followed by command permission, got another setup prompt: %#v", event.Metadata)
			}
			return PermissionOnce, "run_shell"
		},
		EnableYolo: func(*AgentState) { t.Fatal("sandbox setup must not enable YOLO") },
	}
	events := resolver.Resolve(context.Background(), []coretools.ToolCall{call}, executor, &state, nil)
	executor.GetRemainingResults(context.Background())
	decisions, _ := executor.Context[wslSandboxDecisionMapKey].(map[string]string)
	if prompts != 2 || state.YoloEnabled() || !tool.executed || decisions[call.ID] != "install" {
		t.Fatalf("setup flow mismatch: prompts=%d yolo=%t executed=%t decisions=%#v", prompts, state.YoloEnabled(), tool.executed, decisions)
	}
	for _, event := range events {
		if strings.Contains(event.Content, "YOLO mode enabled") {
			t.Fatalf("setup approval should not emit YOLO event: %#v", events)
		}
	}
}

func TestSandboxSetupDenialDoesNotUseLocalFallback(t *testing.T) {
	tool := newSetupUnavailableSandboxTool()
	tool.allowLocalFallback = true
	registry := coretools.NewToolRegistry(tool)
	state := NewAgentState()
	executor := NewStreamingToolExecutor(registry, testExecutorConfig(), &state, coretools.ExecutionContext{"parent_state": &state})
	call := coretools.ToolCall{ID: "shell", Name: "run_shell", Input: map[string]any{"command": "pwd"}}
	executor.AddTool(call)

	resolver := PermissionResolver{
		Registry: registry,
		RequestDecision: func(_ context.Context, event StreamEvent) (string, string) {
			if event.Metadata["sandbox_setup_request"] == nil {
				t.Fatalf("unexpected non-setup permission prompt: %#v", event.Metadata)
			}
			return PermissionDeny, ""
		},
		EnableYolo: func(*AgentState) { t.Fatal("sandbox setup denial must not enable YOLO") },
	}
	resolver.Resolve(context.Background(), []coretools.ToolCall{call}, executor, &state, nil)
	executor.GetRemainingResults(context.Background())
	decisions, _ := executor.Context[wslSandboxDecisionMapKey].(map[string]string)
	if state.YoloEnabled() || tool.executed || decisions[call.ID] != "" {
		t.Fatalf("denied setup must not execute or select fallback: yolo=%t executed=%t decisions=%#v", state.YoloEnabled(), tool.executed, decisions)
	}
}

func TestSandboxUnavailablePermissionDenialDoesNotExecute(t *testing.T) {
	tool := newUnavailableSandboxTool()
	registry := coretools.NewToolRegistry(tool)
	state := NewAgentState()
	executor := NewStreamingToolExecutor(registry, testExecutorConfig(), &state, nil)
	call := coretools.ToolCall{ID: "shell", Name: "run_shell", Input: map[string]any{"command": "git status"}}
	executor.AddTool(call)
	resolver := PermissionResolver{Registry: registry, RequestDecision: func(context.Context, StreamEvent) (string, string) {
		return PermissionDeny, ""
	}}
	resolver.Resolve(context.Background(), []coretools.ToolCall{call}, executor, &state, nil)
	if state.YoloEnabled() || tool.executed {
		t.Fatalf("denied fallback must not enable YOLO or execute: yolo=%t executed=%t", state.YoloEnabled(), tool.executed)
	}
}

func TestSubagentPermissionBypassDoesNotInventYolo(t *testing.T) {
	parent := NewAgentState()
	child := BuildSubagentState(&parent, "bypass")
	if child.YoloEnabled() {
		t.Fatal("permission bypass must not be treated as user-enabled YOLO")
	}
	parent.PermissionState.YoloMode = true
	child = BuildSubagentState(&parent, "bypass")
	if !child.YoloEnabled() {
		t.Fatal("subagent should inherit genuine user-enabled YOLO")
	}
}

func testExecutorConfig() config.Config {
	return config.Config{MaxMessageToolResultsChars: 10000}
}
