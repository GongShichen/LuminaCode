package agent

import (
	"LuminaCode/agentContext"
	"LuminaCode/harness"
	"bytes"
	"encoding/json"
	"fmt"
)

// ReplayAgentState rebuilds original history independently of the working view.
// It also applies facts committed after the last checkpoint, including usage and
// terminal tool results that were durable when a process stopped mid-step.
func ReplayAgentState(base AgentSnapshot, events []harness.Event, after int64, streamID string) AgentSnapshot {
	state := base.LegacyState()
	lastReset := int64(0)
	for _, event := range events {
		if event.StreamID == streamID && event.Type == harness.EventSessionReset {
			lastReset = event.Seq
		}
	}
	resetAfter := lastReset > after
	if resetAfter {
		fresh := NewAgentState()
		state = fresh
		after = lastReset
		state.generation = base.Generation + 1
	}
	if lastReset > 0 {
		state.Transcript = nil
	}
	transcript := agentContext.MessageMaps(base.Transcript)
	if lastReset > 0 {
		transcript = nil
	}
	seen := map[string]bool{}
	for _, m := range transcript {
		seen[stringFromAny(m["context_id"])] = true
	}
	contains := func(id string) bool {
		for _, m := range state.Messages {
			if stringFromAny(m["context_id"]) == id {
				return true
			}
		}
		return false
	}
	if state.UsageRequests == nil {
		state.UsageRequests = map[string]bool{}
	}
	plans := map[string]harness.ToolLifecyclePayload{}
	plannedSeq := map[string]int64{}
	futureCalls := map[string]bool{}
	for _, event := range events {
		if event.StreamID != streamID || event.Seq <= lastReset {
			continue
		}
		if event.Type == harness.EventToolCallPlanned {
			var plan harness.ToolLifecyclePayload
			if decodeReplay(event.Payload, &plan) == nil {
				plans[plan.ToolCallID] = plan
				plannedSeq[plan.ToolCallID] = event.Seq
			}
		}
		if event.Seq > after && event.Type == harness.EventMessageAssistantAppended {
			var payload harness.MessageAppendedPayload
			var message map[string]any
			if decodeReplay(event.Payload, &payload) == nil && decodeReplay(payload.Message, &message) == nil {
				for _, block := range contentBlocks(message["content"]) {
					if block["type"] == "tool_use" {
						futureCalls[stringFromAny(block["id"])] = true
					}
				}
			}
		}
	}
	representedTools := map[string]bool{}
	noteTools := func(message map[string]any) {
		for _, block := range contentBlocks(message["content"]) {
			if block["type"] == "tool_result" {
				representedTools[stringFromAny(block["tool_use_id"])] = true
			}
		}
	}
	legacyIDs := map[string][]string{}
	fingerprint := func(message map[string]any) string {
		copy := agentContext.MessageFromMap("fingerprint", 0, message).Map()
		delete(copy, "context_id")
		delete(copy, "event_seq")
		raw, _ := json.Marshal(copy)
		return string(raw)
	}
	for _, message := range transcript {
		noteTools(message)
		key := fingerprint(message)
		legacyIDs[key] = append(legacyIDs[key], stringFromAny(message["context_id"]))
	}
	for _, event := range events {
		if event.StreamID != streamID || event.Seq <= lastReset {
			continue
		}
		switch event.Type {
		case harness.EventMessageUserAppended, harness.EventMessageAssistantAppended, harness.EventMessageContextAppended:
			var payload harness.MessageAppendedPayload
			var message map[string]any
			if decodeReplay(event.Payload, &payload) == nil && decodeReplay(payload.Message, &message) == nil {
				noteTools(message)
			}
		}
	}
	toolSeen := map[string]bool{}
	for _, m := range state.Messages {
		for _, b := range contentBlocks(m["content"]) {
			if b["type"] == "tool_result" {
				toolSeen[stringFromAny(b["tool_use_id"])] = true
			}
		}
	}
	for _, event := range events {
		if event.StreamID != streamID || event.Seq <= lastReset {
			continue
		}
		var msg map[string]any
		switch event.Type {
		case harness.EventMessageUserAppended, harness.EventMessageAssistantAppended, harness.EventMessageContextAppended:
			var payload harness.MessageAppendedPayload
			if decodeReplay(event.Payload, &payload) != nil || decodeReplay(payload.Message, &msg) != nil {
				continue
			}
		case harness.EventToolExecutionCompleted, harness.EventToolExecutionFailed, harness.EventToolExecutionDenied, harness.EventToolExecutionCancelled, harness.EventToolExecutionInterrupted:
			var payload harness.ToolLifecyclePayload
			if decodeReplay(event.Payload, &payload) != nil {
				continue
			}
			if representedTools[payload.ToolCallID] {
				continue
			}
			if event.Seq > after && !futureCalls[payload.ToolCallID] {
				hasCall := false
				for _, message := range state.Messages {
					for _, block := range contentBlocks(message["content"]) {
						if block["type"] == "tool_use" && block["id"] == payload.ToolCallID {
							hasCall = true
						}
					}
				}
				if plan, ok := plans[payload.ToolCallID]; ok && !hasCall {
					var input any
					_ = decodeReplay(plan.Input, &input)
					call := map[string]any{"role": "assistant", "context_id": "recovered-call:" + payload.ToolCallID, "event_seq": plannedSeq[payload.ToolCallID], "metadata": map[string]any{"source": "runtime_recovery"}, "content": []map[string]any{{"type": "tool_use", "id": plan.ToolCallID, "name": plan.Name, "input": input}}}
					state.Messages = append(state.Messages, call)
					if !seen[stringFromAny(call["context_id"])] {
						transcript = append(transcript, call)
						seen[stringFromAny(call["context_id"])] = true
					}
				}
			}
			var result map[string]any
			_ = decodeReplay(payload.Result, &result)
			if result == nil {
				result = map[string]any{"type": "tool_result", "tool_use_id": payload.ToolCallID, "content": payload.Error, "is_error": true}
			}
			if result["type"] != "tool_result" {
				continue
			}
			msg = map[string]any{"role": "user", "content": []map[string]any{result}, "context_id": "tool-result:" + payload.ToolCallID}
			if event.Seq > after && !toolSeen[payload.ToolCallID] {
				state.Messages = append(state.Messages, msg)
				toolSeen[payload.ToolCallID] = true
			}
		case harness.EventUsageRecorded:
			if event.Seq <= after {
				continue
			}
			var u harness.UsageRecordedPayload
			if decodeReplay(event.Payload, &u) != nil {
				continue
			}
			id := u.RequestID
			if id == "" {
				id = event.ID
			}
			if id == "" {
				id = fmt.Sprintf("event:%d", event.Seq)
			}
			if !state.UsageRequests[id] {
				state.UsageRequests[id] = true
				state.TotalInputTokens += u.InputTokens
				state.TotalOutputTokens += u.OutputTokens
				state.CacheReadInputTokens += u.CacheReadTokens
				state.CacheCreateInputTokens += u.CacheCreateTokens
				state.ServerToolUseInputTokens += u.ServerToolTokens
			}
		case harness.EventContextReplaced:
			if event.Seq <= after {
				continue
			}
			var next agentContext.ContextSnapshot
			if decodeReplay(event.Payload, &next) == nil && next.Version == 3 {
				state.ContextSnapshot = &next
				state.Messages = agentContext.MessageMaps(next.Messages)
			}
		}
		if msg == nil {
			continue
		}
		if stringFromAny(msg["context_id"]) == "" && event.Seq <= after {
			key := fingerprint(msg)
			if ids := legacyIDs[key]; len(ids) > 0 {
				msg["context_id"] = ids[0]
				legacyIDs[key] = ids[1:]
			}
		}
		typed := agentContext.MessageFromMap(fmt.Sprintf("%s:event:%d", streamID, event.Seq), 0, msg)
		typed.Seq = event.Seq
		msg = typed.Map()
		if !transientRuntimeMessage(msg) && !stringsContainsCompaction(visibleMessageText(msg["content"])) && !seen[typed.ID] {
			transcript = append(transcript, msg)
			seen[typed.ID] = true
		}
		if event.Seq > after && event.Type != harness.EventToolExecutionCompleted && event.Type != harness.EventToolExecutionFailed && event.Type != harness.EventToolExecutionDenied && event.Type != harness.EventToolExecutionCancelled && event.Type != harness.EventToolExecutionInterrupted && !contains(typed.ID) {
			state.Messages = append(state.Messages, msg)
			if event.Type == harness.EventMessageUserAppended && !transientRuntimeMessage(msg) && !isToolResultCarrierMessage(msg) {
				state.UserTurnCount++
				state.LastQuery = visibleMessageText(msg["content"])
			}
			if event.Type == harness.EventMessageAssistantAppended {
				state.TurnCount++
			}
		}
	}
	if len(transcript) > 0 {
		state.Transcript = transcript
	}
	result := SnapshotFromState(&state)
	result.Revision = base.Revision
	result.Generation = base.Generation
	if resetAfter {
		result.Generation++
	}
	result.Context.Generation = result.Generation
	return result
}

func decodeReplay(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(target)
}
