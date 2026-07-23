package agent

import (
	"strings"

	"LuminaCode/memory"
)

func InjectRecalledMemories(state *AgentState, recalled []MemoryRecall) {
	if state == nil {
		return
	}
	state.Messages = memory.StripMemoryContextMessages(state.Messages, memory.MemoryRecallSource)
	message := memory.BuildRecalledMemoriesMessage(recalled)
	if message != nil {
		InsertBeforeCurrentUserMessage(state, message)
	}
}

func InsertBeforeCurrentUserMessage(state *AgentState, message map[string]any) {
	if state == nil {
		return
	}
	idx := len(state.Messages)
	for i := len(state.Messages) - 1; i >= 0; i-- {
		candidate := state.Messages[i]
		if stringFromAny(candidate["role"]) != "user" {
			continue
		}
		if isToolResultCarrierMessage(candidate) {
			continue
		}
		idx = i
		break
	}
	state.Messages = append(state.Messages, nil)
	copy(state.Messages[idx+1:], state.Messages[idx:])
	state.Messages[idx] = message
}

func StripMessageMetadata(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		cleaned := map[string]any{}
		for key, value := range msg {
			if key == "metadata" || key == "isMeta" {
				continue
			}
			cleaned[key] = value
		}
		out = append(out, cleaned)
	}
	return out
}

func isSystemHintBlock(block map[string]any) bool {
	if stringFromAny(block["type"]) != "text" {
		return false
	}
	return strings.Contains(stringFromAny(block["text"]), "<system_hint>")
}

func isToolResultCarrierMessage(message map[string]any) bool {
	if stringFromAny(message["role"]) != "user" {
		return false
	}
	blocks := contentBlocks(message["content"])
	if blocks == nil {
		return false
	}
	sawToolResult := false
	for _, block := range blocks {
		if stringFromAny(block["type"]) == "tool_result" {
			sawToolResult = true
			continue
		}
		if isSystemHintBlock(block) {
			continue
		}
		return false
	}
	return sawToolResult
}
