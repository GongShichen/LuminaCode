package test

import (
	"strings"
	"testing"

	"LuminaCode/agent"
	coretools "LuminaCode/tools"
)

func TestAgentRenderToolUseAndResultMatchPython(t *testing.T) {
	use := agent.RenderAgentToolUse(agent.AgentInput{
		Description:     "Investigate flaky test",
		SubagentType:    "debugger",
		Model:           "gpt-test",
		RunInBackground: true,
		Isolation:       "worktree",
	})
	if use != "Agent(debugger [bg] [wt], model=gpt-test): Investigate flaky test" {
		t.Fatalf("unexpected agent use render: %q", use)
	}
	content := "[Sub-agent: debugger]\n[Task: work]\n\nFirst useful line\nsecond"
	if got := agent.RenderAgentToolResult(content, false); got != "Agent done: First useful line" {
		t.Fatalf("unexpected agent result render: %q", got)
	}
	if got := agent.RenderAgentToolResult("[Sub-agent: only-meta]\n[Tokens: 1 in / 2 out]", false); got != "Agent done: Agent completed" {
		t.Fatalf("unexpected metadata-only agent result: %q", got)
	}
	errText := strings.Repeat("e", 170)
	if got := agent.RenderAgentToolResult(errText, true); got != "Agent failed: "+strings.Repeat("e", 150) {
		t.Fatalf("unexpected agent error render: %q", got)
	}
}

func TestAgentAndTaskToolDescriptionsMatchPython(t *testing.T) {
	agentTool := agent.NewAgentTool()
	if !strings.Contains(agentTool.Description(), "Available agent types:") ||
		!strings.Contains(agentTool.Description(), "Coordinator: task orchestration only.") ||
		!strings.Contains(agentTool.Description(), "Workers are automatically isolated via git worktrees.") {
		t.Fatalf("agent description missing Python guidance:\n%s", agentTool.Description())
	}
	schema := agentTool.ToAPISchema()["input_schema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	subagentType := properties["subagent_type"].(map[string]any)
	if !strings.Contains(subagentType["description"].(string), "Available types include") ||
		!strings.Contains(subagentType["description"].(string), "docs-lookup") {
		t.Fatalf("subagent_type schema description mismatch: %#v", subagentType)
	}

	taskTools := []struct {
		tool coretools.Tool
		want string
	}{
		{agent.NewTaskListTool(), "including their status, usage, and result metadata"},
		{agent.NewTaskGetTool(), "including status, result text, error text, usage, and lifecycle metadata"},
		{agent.NewTaskWaitTool(), "until timeout_seconds elapses"},
		{agent.NewTaskStopTool(), "running workers are cancelled and converge to killed"},
		{agent.NewSendMessageTool(), "idle and ready for more work"},
	}
	for _, item := range taskTools {
		if !strings.Contains(item.tool.Description(), item.want) {
			t.Fatalf("%s description missing %q:\n%s", item.tool.Name(), item.want, item.tool.Description())
		}
	}
}

func TestTaskRenderToolUseMatchesPython(t *testing.T) {
	if got := agent.RenderTaskListToolUse(); got != "TaskList()" {
		t.Fatalf("unexpected TaskList render: %q", got)
	}
	if got := agent.RenderTaskGetToolUse(agent.TaskGetInput{TaskID: "task-1"}); got != "TaskGet(task-1)" {
		t.Fatalf("unexpected TaskGet render: %q", got)
	}
	if got := agent.RenderTaskWaitToolUse(agent.TaskWaitInput{TaskIDs: []string{"a", "b"}, TimeoutSecond: 300}); got != "TaskWait(2 tasks, timeout=300s)" {
		t.Fatalf("unexpected TaskWait render: %q", got)
	}
	if got := agent.RenderTaskStopToolUse(agent.TaskStopInput{TaskID: "task-1"}); got != "TaskStop(task-1)" {
		t.Fatalf("unexpected TaskStop render: %q", got)
	}
	if got := agent.RenderSendMessageToolUse(agent.SendMessageInput{TaskID: "task-1", Prompt: "next"}); got != "SendMessage(task-1)" {
		t.Fatalf("unexpected SendMessage render: %q", got)
	}
}

func TestMCPRenderFunctionsMatchPython(t *testing.T) {
	use := coretools.RenderMCPDynamicToolUse("server", "lookup", map[string]any{
		"query": strings.Repeat("q", 50),
		"n":     3,
	})
	for _, want := range []string{"MCP:server/lookup(", "n=3", "query=" + strings.Repeat("q", 40)} {
		if !strings.Contains(use, want) {
			t.Fatalf("dynamic MCP use render missing %q: %q", want, use)
		}
	}
	if got := coretools.RenderMCPDynamicToolUse("server", "noop", nil); got != "MCP:server/noop(no args)" {
		t.Fatalf("unexpected no-arg MCP render: %q", got)
	}
	if got := coretools.RenderMCPDynamicToolResult("Full output saved to: /tmp/out\npreview", false); got != "MCP output saved (38 chars)" {
		t.Fatalf("unexpected saved MCP result: %q", got)
	}
	if got := coretools.RenderMCPDynamicToolResult("abc", false); got != "MCP result (3 chars)" {
		t.Fatalf("unexpected MCP result: %q", got)
	}
	if got := coretools.RenderMCPDynamicToolResult("结果内容", false); got != "MCP result (4 chars)" {
		t.Fatalf("MCP result length should count Python characters, got %q", got)
	}
	errText := strings.Repeat("e", 170)
	if got := coretools.RenderMCPDynamicToolResult(errText, true); got != "MCP failed: "+strings.Repeat("e", 150) {
		t.Fatalf("unexpected MCP error result: %q", got)
	}

	if got := coretools.RenderListMCPResourcesToolUse(); got != "MCP: list_resources" {
		t.Fatalf("unexpected list resources render: %q", got)
	}
	if got := coretools.RenderListMCPResourcesToolResult("a\nb", false); got != "MCP resources (2 entries)" {
		t.Fatalf("unexpected list resources result: %q", got)
	}
	if got := coretools.RenderReadMCPResourceToolUse(coretools.ReadMCPResourceInput{URI: "file://demo"}); got != "MCP: read file://demo" {
		t.Fatalf("unexpected read resource render: %q", got)
	}
	if got := coretools.RenderReadMCPResourceToolResult("abcdef", false); got != "MCP resource (6 chars)" {
		t.Fatalf("unexpected read resource result: %q", got)
	}
	if got := coretools.RenderReadMCPResourceToolResult("资源内容", false); got != "MCP resource (4 chars)" {
		t.Fatalf("MCP resource length should count Python characters, got %q", got)
	}
}
