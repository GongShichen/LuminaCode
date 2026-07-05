package test

import (
	"strings"
	"testing"

	"LuminaCode/agent"
	coretools "LuminaCode/tools"
)

type helperSlot struct {
	name    string
	isError bool
}

func (s helperSlot) IsErrorSlot() bool { return s.isError }
func (s helperSlot) ToolName() string  { return s.name }

type helperExecutor struct {
	slot helperSlot
}

func (e helperExecutor) GetSlotInfo(string) agent.SlotInfo { return e.slot }

func TestAgentHelperCommitAssistantTurnAndUsageMatchPython(t *testing.T) {
	stateValue := agent.NewAgentState()
	state := &stateValue
	agent.AddUsage(state, map[string]any{
		"input_tokens":                 11,
		"output_tokens":                7,
		"cache_read_input_tokens":      5,
		"cache_creation_input_tokens":  3,
		"server_tool_use_input_tokens": 2,
	})
	agent.AddUsage(state, map[string]int{
		"input_tokens":                 4,
		"output_tokens":                6,
		"cache_read_input_tokens":      1,
		"cache_creation_input_tokens":  2,
		"server_tool_use_input_tokens": 9,
	})
	if state.TotalInputTokens != 15 || state.TotalOutputTokens != 13 ||
		state.CacheReadInputTokens != 6 || state.CacheCreateInputTokens != 5 ||
		state.ServerToolUseInputTokens != 11 {
		t.Fatalf("usage accumulation should match Python helper, got %#v", state)
	}

	agent.CommitAssistantTurn(
		state,
		[]map[string]any{{"type": "thinking", "thinking": "hmm"}},
		"I will read it.",
		[]coretools.ToolCall{{ID: "tool-1", Name: "read_file", Input: map[string]any{"file_path": "README.md"}}},
		"msg-1",
		2,
		3,
	)
	msg := state.Messages[0]
	if msg["role"] != "assistant" || msg["id"] != "msg-1" {
		t.Fatalf("assistant message metadata mismatch: %#v", msg)
	}
	content := msg["content"].([]map[string]any)
	if len(content) != 3 || content[0]["type"] != "thinking" || content[1]["text"] != "I will read it." {
		t.Fatalf("assistant content should preserve thinking/text/tool order like Python, got %#v", content)
	}
	toolUse := content[2]
	if toolUse["type"] != "tool_use" || toolUse["id"] != "tool-1" || toolUse["name"] != "read_file" {
		t.Fatalf("tool_use block mismatch: %#v", toolUse)
	}
}

func TestAgentHelperCommitToolResultsDedupesAndHintsLikePython(t *testing.T) {
	stateValue := agent.NewAgentState()
	state := &stateValue
	state.ToolErrors["read_file"] = 2
	results := []map[string]any{
		{"type": "tool_result", "tool_use_id": "tool-1", "content": "missing"},
		{"type": "tool_result", "tool_use_id": "tool-1", "content": "duplicate should be dropped"},
	}
	agent.CommitToolResultsTurn(state, results, helperExecutor{slot: helperSlot{name: "read_file", isError: true}})

	if state.ToolErrors["read_file"] != 3 {
		t.Fatalf("error count should increment like Python, got %#v", state.ToolErrors)
	}
	if len(state.Messages) != 1 || state.Messages[0]["role"] != "user" {
		t.Fatalf("expected one user tool-results message, got %#v", state.Messages)
	}
	content := state.Messages[0]["content"].([]map[string]any)
	if len(content) != 2 {
		t.Fatalf("duplicate tool result should be dropped and hint appended, got %#v", content)
	}
	if content[0]["content"] != "missing" {
		t.Fatalf("first tool result should be preserved, got %#v", content[0])
	}
	hint := content[1]["text"].(string)
	if !strings.Contains(hint, "failed 3 times") || !strings.Contains(hint, "DO NOT call it again") {
		t.Fatalf("third failure hint mismatch: %q", hint)
	}
}
