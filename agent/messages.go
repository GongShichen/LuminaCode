package agent

import (
	"fmt"

	coretools "LuminaCode/tools"
)

const ContinuationPrompt = "Your previous response was cut off due to output length limits. Please continue exactly where you left off. Do not repeat any content you already generated."

type SlotInfo interface {
	IsErrorSlot() bool
	ToolName() string
}

type SlotLookup interface {
	GetSlotInfo(toolUseID string) SlotInfo
}

func AddUsage(state *AgentState, usageOrInput any, outputTokens ...int) {
	switch usage := usageOrInput.(type) {
	case map[string]int:
		state.TotalInputTokens += usage["input_tokens"]
		state.TotalOutputTokens += usage["output_tokens"]
		state.CacheReadInputTokens += usage["cache_read_input_tokens"]
		state.CacheCreateInputTokens += usage["cache_creation_input_tokens"]
		state.ServerToolUseInputTokens += usage["server_tool_use_input_tokens"]
	case map[string]any:
		state.TotalInputTokens += intFromAny(usage["input_tokens"])
		state.TotalOutputTokens += intFromAny(usage["output_tokens"])
		state.CacheReadInputTokens += intFromAny(usage["cache_read_input_tokens"])
		state.CacheCreateInputTokens += intFromAny(usage["cache_creation_input_tokens"])
		state.ServerToolUseInputTokens += intFromAny(usage["server_tool_use_input_tokens"])
	case int:
		state.TotalInputTokens += usage
		if len(outputTokens) > 0 {
			state.TotalOutputTokens += outputTokens[0]
		}
	}
}

func CommitAssistantTurn(state *AgentState, thinkingContent []map[string]any, fullText string, toolCalls []coretools.ToolCall, messageID string, inputTokens, outputTokens int) {
	AddUsage(state, inputTokens, outputTokens)
	AppendAssistantMessage(state, thinkingContent, fullText, toolCalls, messageID)
	tagLastMessageWithSessionTurn(state)
}

func BuildAssistantMessage(thinkingContent []map[string]any, fullText string, toolCalls []coretools.ToolCall, messageID string) map[string]any {
	content := make([]map[string]any, 0, len(thinkingContent)+1+len(toolCalls))
	content = append(content, thinkingContent...)
	if fullText != "" {
		content = append(content, map[string]any{"type": "text", "text": fullText})
	}
	for _, tc := range toolCalls {
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Name,
			"input": tc.Input,
		})
	}
	msg := map[string]any{"role": "assistant", "content": content}
	if messageID != "" {
		msg["id"] = messageID
	}
	return msg
}

func AppendAssistantMessage(state *AgentState, thinkingContent []map[string]any, fullText string, toolCalls []coretools.ToolCall, messageID string) {
	state.Messages = append(state.Messages, BuildAssistantMessage(thinkingContent, fullText, toolCalls, messageID))
}

func AppendContinuationPrompt(state *AgentState) {
	state.Messages = append(state.Messages, map[string]any{
		"role":    "user",
		"content": []map[string]any{{"type": "text", "text": ContinuationPrompt}},
	})
}

func BuildProgressEvent(progress map[string]any) StreamEvent {
	return NewStreamEvent("tool_progress", stringFromAny(progress["chunk"]), map[string]any{
		"tool_use_id": stringFromAny(progress["tool_use_id"]),
		"tool_name":   stringFromAny(progress["tool_name"]),
	})
}

func AddToolFailureHint(state *AgentState, toolResults []map[string]any, executor SlotLookup) []map[string]any {
	if len(toolResults) == 0 || executor == nil {
		return toolResults
	}
	hasError := false
	errorToolName := ""
	for _, result := range toolResults {
		tid := stringFromAny(result["tool_use_id"])
		slot := executor.GetSlotInfo(tid)
		if slot != nil && slot.IsErrorSlot() {
			hasError = true
			errorToolName = slot.ToolName()
			state.ToolErrors[errorToolName] = state.ToolErrors[errorToolName] + 1
			break
		}
	}
	if !hasError {
		return toolResults
	}
	errorCount := state.ToolErrors[errorToolName]
	if errorCount >= 3 {
		toolResults = append(toolResults, map[string]any{
			"type": "text",
			"text": fmt.Sprintf("<system_hint>\nThe tool '%s' has failed %d times in a row. DO NOT call it again with the same parameters.\n1. Carefully analyze the error message above to understand root cause.\n2. Pivot to a completely different approach or tool.\n3. If you believe the error is environmental, explain it to the user.\n</system_hint>", errorToolName, errorCount),
		})
	} else {
		toolResults = append(toolResults, map[string]any{
			"type": "text",
			"text": "<system_hint>\nOne or more tool executions failed.\n1. Carefully analyze the error message above to understand what went wrong.\n2. DO NOT repeat the exact same tool call or command.\n3. Consider checking file paths, verifying syntax, or trying an alternative approach.\n</system_hint>",
		})
	}
	return toolResults
}

func CommitToolResultsTurn(state *AgentState, toolResults []map[string]any, executor SlotLookup) {
	AppendToolResultsMessage(state, AddToolFailureHint(state, toolResults, executor))
}

func AppendToolResultsMessage(state *AgentState, toolResults []map[string]any) {
	if len(toolResults) == 0 {
		return
	}
	seen := map[string]struct{}{}
	deduped := make([]map[string]any, 0, len(toolResults))
	for _, result := range toolResults {
		tid := stringFromAny(result["tool_use_id"])
		if tid != "" {
			if _, ok := seen[tid]; ok {
				continue
			}
			seen[tid] = struct{}{}
		}
		deduped = append(deduped, result)
	}
	state.Messages = append(state.Messages, map[string]any{"role": "user", "content": deduped})
	tagLastMessageWithSessionTurn(state)
}

func tagLastMessageWithSessionTurn(state *AgentState) {
	if state == nil || len(state.Messages) == 0 || state.UserTurnCount <= 0 {
		return
	}
	msg := state.Messages[len(state.Messages)-1]
	metadata, _ := msg["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["session_user_turn"] = state.UserTurnCount
	msg["metadata"] = metadata
}

func RepairOrphanTools(messages []map[string]any) []map[string]any {
	uses := map[string]struct{}{}
	results := map[string]struct{}{}
	for _, msg := range messages {
		for _, block := range contentBlocks(msg["content"]) {
			switch block["type"] {
			case "tool_use":
				if id := stringFromAny(block["id"]); id != "" {
					uses[id] = struct{}{}
				}
			case "tool_result":
				if id := stringFromAny(block["tool_use_id"]); id != "" {
					results[id] = struct{}{}
				}
			}
		}
	}
	orphanUses := map[string]struct{}{}
	for id := range uses {
		if _, ok := results[id]; !ok {
			orphanUses[id] = struct{}{}
		}
	}
	orphanResults := map[string]struct{}{}
	for id := range results {
		if _, ok := uses[id]; !ok {
			orphanResults[id] = struct{}{}
		}
	}
	if len(orphanUses) == 0 && len(orphanResults) == 0 {
		return messages
	}

	repaired := make([]map[string]any, 0, len(messages)+len(orphanUses))
	for _, msg := range messages {
		blocks := contentBlocks(msg["content"])
		if blocks == nil {
			repaired = append(repaired, msg)
			continue
		}

		newContent := make([]map[string]any, 0, len(blocks))
		hasOrphanUse := false
		for _, block := range blocks {
			switch block["type"] {
			case "tool_result":
				if _, ok := orphanResults[stringFromAny(block["tool_use_id"])]; ok {
					continue
				}
			case "tool_use":
				if _, ok := orphanUses[stringFromAny(block["id"])]; ok {
					hasOrphanUse = true
				}
			}
			newContent = append(newContent, block)
		}
		if len(newContent) > 0 {
			cp := copyMap(msg)
			cp["content"] = newContent
			repaired = append(repaired, cp)
		}
		if hasOrphanUse {
			syntheticResults := make([]map[string]any, 0)
			for _, block := range blocks {
				if block["type"] == "tool_use" {
					if id := stringFromAny(block["id"]); id != "" {
						if _, ok := orphanUses[id]; ok {
							syntheticResults = append(syntheticResults, map[string]any{
								"type":        "tool_result",
								"tool_use_id": id,
								"content":     "[System: tool execution interrupted - no result available]",
							})
						}
					}
				}
			}
			if len(syntheticResults) > 0 {
				repaired = append(repaired, map[string]any{"role": "user", "content": syntheticResults})
			}
		}
	}
	return repaired
}

func findToolUseIndex(messages []map[string]any, toolID string) int {
	for i, msg := range messages {
		for _, block := range contentBlocks(msg["content"]) {
			if block["type"] == "tool_use" && stringFromAny(block["id"]) == toolID {
				return i
			}
		}
	}
	return -1
}

func findToolResultIndex(messages []map[string]any, toolID string) int {
	for i, msg := range messages {
		for _, block := range contentBlocks(msg["content"]) {
			if block["type"] == "tool_result" && stringFromAny(block["tool_use_id"]) == toolID {
				return i
			}
		}
	}
	return -1
}

func injectToolResultAfter(messages []map[string]any, idx int, resultBlock map[string]any) []map[string]any {
	if idx+1 < len(messages) && stringFromAny(messages[idx+1]["role"]) == "user" {
		msg := copyMap(messages[idx+1])
		blocks := append([]map[string]any{}, contentBlocks(msg["content"])...)
		blocks = append(blocks, resultBlock)
		msg["content"] = blocks
		messages[idx+1] = msg
		return messages
	}
	newMsg := map[string]any{"role": "user", "content": []map[string]any{resultBlock}}
	out := make([]map[string]any, 0, len(messages)+1)
	out = append(out, messages[:idx+1]...)
	out = append(out, newMsg)
	out = append(out, messages[idx+1:]...)
	return out
}

func injectToolUseBefore(messages []map[string]any, idx int, useBlock map[string]any) []map[string]any {
	if idx > 0 && stringFromAny(messages[idx-1]["role"]) == "assistant" {
		msg := copyMap(messages[idx-1])
		blocks := append([]map[string]any{useBlock}, contentBlocks(msg["content"])...)
		msg["content"] = blocks
		messages[idx-1] = msg
		return messages
	}
	newMsg := map[string]any{"role": "assistant", "content": []map[string]any{useBlock}}
	out := make([]map[string]any, 0, len(messages)+1)
	out = append(out, messages[:idx]...)
	out = append(out, newMsg)
	out = append(out, messages[idx:]...)
	return out
}

func intFromAny(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case jsonNumber:
		i, _ := x.Int64()
		return int(i)
	default:
		return 0
	}
}

type jsonNumber interface {
	Int64() (int64, error)
}

func stringFromAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func copyMap(m map[string]any) map[string]any {
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func contentBlocks(content any) []map[string]any {
	switch c := content.(type) {
	case []map[string]any:
		return c
	case []any:
		out := make([]map[string]any, 0, len(c))
		for _, item := range c {
			if block, ok := item.(map[string]any); ok {
				out = append(out, block)
			}
		}
		return out
	default:
		return nil
	}
}
