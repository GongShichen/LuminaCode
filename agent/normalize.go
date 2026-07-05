package agent

import "strings"

var errorBlockPatterns = map[string][]string{
	"PDF too large":     {"document"},
	"image too large":   {"image"},
	"file too large":    {"file"},
	"unsupported_media": {"image", "document"},
}

var attachmentBlockTypes = map[string]struct{}{
	"image":    {},
	"document": {},
	"file":     {},
}

var internalBlockTypes = map[string]struct{}{
	"tool_reference": {},
	"advisor_block":  {},
}

var thinkingBlockTypes = map[string]struct{}{
	"thinking":          {},
	"redacted_thinking": {},
	"signature":         {},
}

var thinkingModelPrefixes = []string{
	"deepseek",
	"claude-3-7",
	"claude-4",
	"claude-sonnet-4",
	"claude-opus-4",
}

func ReorderAttachments(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		blocks := contentBlocks(msg["content"])
		if blocks == nil {
			out = append(out, msg)
			continue
		}
		attachments := make([]map[string]any, 0, len(blocks))
		others := make([]map[string]any, 0, len(blocks))
		for _, block := range blocks {
			if _, ok := attachmentBlockTypes[stringFromAny(block["type"])]; ok {
				attachments = append(attachments, block)
			} else {
				others = append(others, block)
			}
		}
		cp := copyMap(msg)
		cp["content"] = append(attachments, others...)
		out = append(out, cp)
	}
	return out
}

func FilterVirtualMessages(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		if isTruthy(msg["virtual"]) || isTruthy(msg["isVirtual"]) {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func BuildErrorBlockMap(recentErrors []string) map[string][]string {
	out := map[string][]string{}
	for _, err := range recentErrors {
		for keyword, blockTypes := range errorBlockPatterns {
			if strings.Contains(err, keyword) {
				out[keyword] = append([]string{}, blockTypes...)
			}
		}
	}
	return out
}

func StripInternalElements(messages []map[string]any) []map[string]any {
	return StripInternalElementsWithErrors(messages, nil)
}

func StripInternalElementsWithErrors(messages []map[string]any, errorBlockMap map[string][]string) []map[string]any {
	stripTypes := map[string]struct{}{}
	for _, blockTypes := range errorBlockMap {
		for _, blockType := range blockTypes {
			stripTypes[blockType] = struct{}{}
		}
	}
	out := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		blocks := contentBlocks(msg["content"])
		if blocks == nil {
			out = append(out, msg)
			continue
		}
		filtered := make([]map[string]any, 0, len(blocks))
		for _, block := range blocks {
			blockType := stringFromAny(block["type"])
			if _, ok := internalBlockTypes[blockType]; ok {
				continue
			}
			if _, ok := stripTypes[blockType]; ok {
				continue
			}
			filtered = append(filtered, block)
		}
		if len(filtered) == 0 {
			continue
		}
		cp := copyMap(msg)
		cp["content"] = filtered
		out = append(out, cp)
	}
	return out
}

func HandleThinkingBlocks(messages []map[string]any, modelFamily string, recentErrors []string) []map[string]any {
	supports := modelSupportsThinking(modelFamily)
	out := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		blocks := contentBlocks(msg["content"])
		if blocks == nil {
			out = append(out, msg)
			continue
		}
		role := stringFromAny(msg["role"])
		stripThinking := role == "user" || !supports
		filtered := make([]map[string]any, 0, len(blocks))
		for _, block := range blocks {
			if _, ok := thinkingBlockTypes[stringFromAny(block["type"])]; ok && stripThinking {
				continue
			}
			filtered = append(filtered, block)
		}
		if len(filtered) == 0 {
			continue
		}
		cp := copyMap(msg)
		cp["content"] = filtered
		out = append(out, cp)
	}
	return out
}

func MergeSplitMessages(messages []map[string]any) []map[string]any {
	if len(messages) == 0 {
		return messages
	}
	out := make([]map[string]any, 0, len(messages))
	i := 0
	for i < len(messages) {
		msg := messages[i]
		role := stringFromAny(msg["role"])
		msgID := stringFromAny(msg["id"])
		if role != "assistant" || msgID == "" {
			out = append(out, msg)
			i++
			continue
		}
		merged := append([]map[string]any{}, contentBlocks(msg["content"])...)
		j := i + 1
		for j < len(messages) {
			next := messages[j]
			if stringFromAny(next["role"]) != "assistant" || stringFromAny(next["id"]) != msgID {
				break
			}
			merged = append(merged, contentBlocks(next["content"])...)
			j++
		}
		cp := copyMap(msg)
		cp["content"] = merged
		out = append(out, cp)
		i = j
	}
	return out
}

func FixToolPairings(messages []map[string]any) []map[string]any {
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

	repaired := make([]map[string]any, len(messages))
	for i, msg := range messages {
		blocks := contentBlocks(msg["content"])
		if blocks == nil {
			repaired[i] = msg
			continue
		}
		cp := copyMap(msg)
		cp["content"] = append([]map[string]any{}, blocks...)
		repaired[i] = cp
	}

	for orphanID := range orphanUses {
		useIdx := findToolUseIndex(repaired, orphanID)
		if useIdx < 0 {
			continue
		}
		synthetic := map[string]any{
			"type":        "tool_result",
			"tool_use_id": orphanID,
			"content":     "Tool execution was interrupted or lost during session recovery.",
		}
		repaired = injectToolResultAfter(repaired, useIdx, synthetic)
	}
	for orphanID := range orphanResults {
		resultIdx := findToolResultIndex(repaired, orphanID)
		if resultIdx < 0 {
			continue
		}
		synthetic := map[string]any{
			"type":  "tool_use",
			"id":    orphanID,
			"name":  "unknown",
			"input": map[string]any{},
		}
		repaired = injectToolUseBefore(repaired, resultIdx, synthetic)
	}
	return repaired
}

func EnforceImmediateToolResults(messages []map[string]any) []map[string]any {
	working := copyMessages(messages)
	out := make([]map[string]any, 0, len(working))
	for i := 0; i < len(working); i++ {
		msg := working[i]
		if msg == nil {
			continue
		}
		if len(contentBlocks(msg["content"])) == 0 {
			continue
		}
		out = append(out, msg)
		if stringFromAny(msg["role"]) != "assistant" {
			continue
		}
		toolUseIDs := toolUseIDsInMessage(msg)
		if len(toolUseIDs) == 0 {
			continue
		}
		found := map[string]map[string]any{}
		needed := map[string]struct{}{}
		for _, id := range toolUseIDs {
			needed[id] = struct{}{}
		}
		for j := i + 1; j < len(working) && len(needed) > 0; j++ {
			next := working[j]
			if next == nil {
				continue
			}
			blocks := contentBlocks(next["content"])
			if len(blocks) == 0 {
				continue
			}
			kept := make([]map[string]any, 0, len(blocks))
			for _, block := range blocks {
				if block["type"] == "tool_result" {
					id := stringFromAny(block["tool_use_id"])
					if _, ok := needed[id]; ok {
						found[id] = block
						delete(needed, id)
						continue
					}
				}
				kept = append(kept, block)
			}
			if len(kept) == 0 {
				working[j] = nil
			} else if len(kept) != len(blocks) {
				cp := copyMap(next)
				cp["content"] = kept
				working[j] = cp
			}
		}
		resultBlocks := make([]map[string]any, 0, len(toolUseIDs))
		for _, id := range toolUseIDs {
			if block, ok := found[id]; ok {
				resultBlocks = append(resultBlocks, block)
				continue
			}
			resultBlocks = append(resultBlocks, map[string]any{
				"type":        "tool_result",
				"tool_use_id": id,
				"content":     "Tool execution was interrupted or lost during message normalization.",
			})
		}
		out = append(out, map[string]any{"role": "user", "content": resultBlocks})
	}
	return out
}

func copyMessages(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, len(messages))
	for i, msg := range messages {
		if msg == nil {
			continue
		}
		cp := copyMap(msg)
		if blocks := contentBlocks(msg["content"]); blocks != nil {
			cp["content"] = append([]map[string]any{}, blocks...)
		}
		out[i] = cp
	}
	return out
}

func toolUseIDsInMessage(msg map[string]any) []string {
	var ids []string
	for _, block := range contentBlocks(msg["content"]) {
		if block["type"] == "tool_use" {
			if id := stringFromAny(block["id"]); id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func NormalizeMessages(messages []map[string]any, modelFamily string, recentErrors []string) []map[string]any {
	normalized := ReorderAttachments(messages)
	normalized = FilterVirtualMessages(normalized)
	errorBlockMap := BuildErrorBlockMap(recentErrors)
	normalized = StripInternalElementsWithErrors(normalized, errorBlockMap)
	normalized = HandleThinkingBlocks(normalized, modelFamily, recentErrors)
	normalized = MergeSplitMessages(normalized)
	normalized = FixToolPairings(normalized)
	normalized = EnforceImmediateToolResults(normalized)
	return normalized
}

func modelSupportsThinking(modelFamily string) bool {
	low := strings.ToLower(modelFamily)
	for _, prefix := range thinkingModelPrefixes {
		if strings.HasPrefix(low, prefix) {
			return true
		}
	}
	return false
}

func isTruthy(v any) bool {
	b, ok := v.(bool)
	return ok && b
}
