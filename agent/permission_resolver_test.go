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
