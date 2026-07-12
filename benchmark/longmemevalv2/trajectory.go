package longmemevalv2

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

var hiddenTrajectoryFields = map[string]struct{}{
	"thought": {}, "thoughts": {}, "reasoning": {}, "chain_of_thought": {},
	"model_reasoning": {}, "scratchpad": {},
}

// MessagesFromTrajectory converts a generic agent trajectory into visible
// production messages. Hidden reasoning is deliberately omitted.
func MessagesFromTrajectory(trajectory map[string]any) []map[string]any {
	var messages []map[string]any
	if goal := valueText(trajectory["goal"]); goal != "" {
		messages = append(messages, textMessage("user", "Goal: "+goal))
	}
	if startURL := strings.TrimSpace(stringValue(trajectory["start_url"])); startURL != "" {
		messages = append(messages, textMessage("tool", "Initial URL: "+startURL))
	}
	if states, ok := trajectory["states"].([]any); ok {
		for index, raw := range states {
			state, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text := visibleObjectText(state); text != "" {
				messages = append(messages, textMessage("tool", fmt.Sprintf("State %d:\n%s", index+1, text)))
			}
		}
	}
	if outcome := valueText(trajectory["outcome"]); outcome != "" {
		messages = append(messages, textMessage("assistant", "Outcome: "+outcome))
	}
	return messages
}

func textMessage(role, text string) map[string]any {
	return map[string]any{"role": role, "content": []map[string]any{{"type": "text", "text": text}}}
}

func visibleObjectText(value map[string]any) string {
	keys := make([]string, 0, len(value))
	for key := range value {
		if _, hidden := hiddenTrajectoryFields[strings.ToLower(strings.TrimSpace(key))]; !hidden {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		if text := valueText(value[key]); text != "" {
			parts = append(parts, key+": "+text)
		}
	}
	return strings.Join(parts, "\n")
}

func valueText(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	if value == nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil || string(encoded) == "null" || string(encoded) == "{}" || string(encoded) == "[]" {
		return ""
	}
	return string(encoded)
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
