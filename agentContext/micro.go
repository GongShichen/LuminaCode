package agentContext

import (
	"fmt"

	"github.com/mohae/deepcopy"
)

var compressibleTools = map[string]struct{}{
	"read_file":   {},
	"run_shell":   {},
	"grep_search": {},
	"glob_match":  {},
	"edit_file":   {},
	"write_file":  {},
}

const (
	defaultMaxResultChars = 20_000
	defaultMaxTotalChars  = 100_000
	cleanPlaceHolder      = "[Old tool result content cleared]"
)

type CacheEdit struct {
	ToolUseID string  `json:"tool_use_id"`
	Action    *string `json:"action"`
}

func NewCacheEdit(toolUseID string, action *string) CacheEdit {
	if action == nil {
		defaultAction := "delete"
		action = &defaultAction
	}
	return CacheEdit{
		ToolUseID: toolUseID,
		Action:    action,
	}
}

type ToolUseResultBlock struct {
	MessageIndex int
	BlockIndex   int
	Block        map[string]any
}

func BuildToolNameMap(messages []map[string]any) map[string]string {
	nameMap := make(map[string]string)
	for _, msg := range messages {
		if msg["role"] != "assistant" {
			continue
		}

		content, ok := contentBlocks(msg["content"])
		if !ok {
			continue
		}
		for _, item := range content {
			itemType, _ := item["type"].(string)

			if itemType != "tool_use" {
				continue
			}
			toolID := getValueFrom(item, "id")
			toolName := getValueFrom(item, "name")
			if toolID != "" {
				nameMap[toolID] = toolName
			}
		}
	}
	return nameMap
}

func getValueFrom(item map[string]any, k string) string {
	value, ok := item[k].(string)
	if !ok {
		return ""
	}
	return value
}

func MaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func iterToolResultBlock(messages []map[string]any) []ToolUseResultBlock {
	result := []ToolUseResultBlock{}
	for i, msg := range messages {
		content, ok := contentBlocks(msg["content"])
		if !ok {
			continue
		}
		for j, item := range content {
			blockType := getValueFrom(item, "type")
			if blockType == "tool_result" {
				result = append(result, ToolUseResultBlock{MessageIndex: i, BlockIndex: j, Block: item})
			}
		}
	}
	return result
}

func MicroCompactMessages(messages []map[string]any, isCacheHold bool, keepRecent int) ([]map[string]any, []CacheEdit) {
	result := deepcopy.Copy(messages).([]map[string]any)
	var edits []CacheEdit
	keepRecent = MaxInt(keepRecent, 1)
	toolNameMap := BuildToolNameMap(messages)
	allBlocks := iterToolResultBlock(result)
	var compressible []ToolUseResultBlock
	for _, block := range allBlocks {
		toolUseID, ok := block.Block["tool_use_id"].(string)
		if !ok {
			continue
		}
		toolName, ok := toolNameMap[toolUseID]
		if !ok {
			continue
		}
		if _, ok := compressibleTools[toolName]; ok {
			compressible = append(compressible, block)
		}
	}

	if len(compressible) == 0 {
		return result, edits
	}

	stableCount := MaxInt(0, len(compressible)-keepRecent)
	stableBlocks := compressible[:stableCount]
	if isCacheHold {
		for _, block := range stableBlocks {
			message := result[block.MessageIndex]
			messageContent, ok := contentBlocks(message["content"])
			if !ok {
				continue
			}
			if block.BlockIndex < 0 || block.BlockIndex >= len(messageContent) {
				continue
			}
			rawItem := messageContent[block.BlockIndex]
			rawItem["content"] = cleanPlaceHolder
		}
	} else {
		for _, block := range stableBlocks {
			toolUseID, ok := block.Block["tool_use_id"].(string)
			if !ok {
				continue
			}
			edits = append(edits, NewCacheEdit(toolUseID, nil))
		}
	}
	return result, edits
}

func CountClearedToolResult(messages []map[string]any) int {
	count := 0
	for _, msg := range messages {
		content, ok := contentBlocks(msg["content"])
		if !ok {
			continue
		}
		for _, item := range content {
			itemContent, ok := item["content"].(string)
			if !ok {
				continue
			}
			itemType := getValueFrom(item, "type")
			if itemType == "tool_result" && itemContent == cleanPlaceHolder {
				count++
			}
		}
	}
	return count
}

func MicroCompactResult(content string, maxCharsOpt ...int) string {
	maxChars := defaultMaxResultChars
	if len(maxCharsOpt) > 0 {
		maxChars = maxCharsOpt[0]
	}

	runes := []rune(content)
	if len(runes) <= maxChars {
		return content
	}

	half := pythonFloorDiv(maxChars, 2)
	head := string(runes[:pythonSliceEnd(len(runes), half)])
	tail := string(runes[pythonSliceStart(len(runes), -half):])
	removed := len(runes) - maxChars
	return fmt.Sprintf("%s\n\n... [%d characters truncated] ...\n\n%s", head, removed, tail)
}

func pythonFloorDiv(value, divisor int) int {
	result := value / divisor
	if value%divisor != 0 && ((value < 0) != (divisor < 0)) {
		result--
	}
	return result
}

func pythonSliceEnd(length, end int) int {
	if end < 0 {
		end = length + end
	}
	if end < 0 {
		return 0
	}
	if end > length {
		return length
	}
	return end
}

func pythonSliceStart(length, start int) int {
	if start < 0 {
		start = length + start
	}
	if start < 0 {
		return 0
	}
	if start > length {
		return length
	}
	return start
}
