package agent

import (
	"fmt"
	"strings"

	coretools "LuminaCode/tools"
)

var readLikeToolNames = stringSet("read_file", "grep_search", "glob_match")

func GetRecentToolNames(messages []map[string]any, maxTurns ...int) []string {
	limit := 10
	if len(maxTurns) > 0 && maxTurns[0] > 0 {
		limit = maxTurns[0]
	}
	var names []string
	start := len(messages) - limit
	if start < 0 {
		start = 0
	}
	for i := len(messages) - 1; i >= start; i-- {
		msg := messages[i]
		if stringFromAny(msg["role"]) != "assistant" {
			continue
		}
		for _, block := range contentBlocks(msg["content"]) {
			if block["type"] == "tool_use" {
				if name := stringFromAny(block["name"]); name != "" {
					seen := false
					for _, existing := range names {
						if existing == name {
							seen = true
							break
						}
					}
					if !seen {
						names = append(names, name)
					}
				}
			}
		}
	}
	return names
}

func IsReadLikeTool(name string) bool {
	_, ok := readLikeToolNames[name]
	return ok
}

func isReadLikeToolWithNames(name string, readLikeNames map[string]struct{}) bool {
	if readLikeNames == nil {
		readLikeNames = readLikeToolNames
	}
	_, ok := readLikeNames[name]
	return ok
}

func FormatToolInputForRecall(rawInput map[string]any) string {
	keys := []string{"file_path", "path", "pattern", "query", "command"}
	var parts []string
	for _, key := range keys {
		if value := strings.TrimSpace(stringFromAny(rawInput[key])); value != "" {
			parts = append(parts, fmt.Sprintf("%s=%s", key, value))
		}
		if len(parts) == 2 {
			break
		}
	}
	return strings.Join(parts, ", ")
}

func ClipRecallText(text string, limit ...int) string {
	max := 400
	if len(limit) > 0 && limit[0] > 0 {
		max = limit[0]
	}
	cleaned := strings.Join(strings.Fields(text), " ")
	runes := []rune(cleaned)
	if len(runes) <= max {
		return cleaned
	}
	cutoff := max - 3
	if cutoff < 1 {
		cutoff = 1
	}
	return string(runes[:cutoff]) + "..."
}

func ShouldTriggerFollowupRecall(toolObservations []map[string]any) bool {
	for _, observation := range toolObservations {
		if isTruthy(observation["is_error"]) {
			return true
		}
		if IsReadLikeObservation(observation) {
			return true
		}
	}
	return false
}

func IsReadLikeObservation(observation map[string]any) bool {
	return IsReadLikeObservationWithNames(observation, readLikeToolNames)
}

func IsReadLikeObservationWithNames(observation map[string]any, readLikeNames map[string]struct{}) bool {
	call, ok := observation["call"].(coretools.ToolCall)
	if !ok {
		name := stringFromAny(observation["tool_name"])
		input, _ := observation["input"].(map[string]any)
		return hasRecallLocationHint(input) && isReadLikeToolWithNames(name, readLikeNames)
	}
	if !hasRecallLocationHint(call.Input) {
		return false
	}
	if isReadLikeToolWithNames(call.Name, readLikeNames) {
		return true
	}
	tool, _ := observation["tool"].(coretools.Tool)
	if tool == nil {
		return false
	}
	input, err := tool.DecodeInput(call.Input)
	if err != nil {
		input = nil
	}
	return tool.IsReadOnly(input)
}

func hasRecallLocationHint(rawInput map[string]any) bool {
	for _, key := range []string{"file_path", "path", "pattern", "query"} {
		if strings.TrimSpace(stringFromAny(rawInput[key])) != "" {
			return true
		}
	}
	return false
}
