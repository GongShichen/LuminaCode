package memory

import (
	"strings"
)

const (
	MemoryMetaKey      = "lumina_memory_context"
	MemoryRecallSource = "memory_recall"
)

func BuildRecalledMemoriesMessage(recalled []MemoryRecall) map[string]any {
	if len(recalled) == 0 {
		return nil
	}
	parts := make([]string, 0, len(recalled))
	filenames := make([]string, 0, len(recalled))
	recallIDs := make([]string, 0, len(recalled))
	for _, item := range recalled {
		parts = append(parts, strings.TrimSpace(item.Content))
		filenames = append(filenames, item.Filename)
		if item.RecallID != "" {
			recallIDs = append(recallIDs, item.RecallID)
		} else {
			recallIDs = append(recallIDs, item.Filename)
		}
	}
	combined := "<system-reminder>\n" + joinDoubleNewline(parts) + "\n</system-reminder>"
	message := BuildMetaUserMessage(combined, MemoryRecallSource)
	metadata := message["metadata"].(map[string]any)
	metadata["filenames"] = filenames
	metadata["recall_ids"] = recallIDs
	return message
}

func IsMemoryContextMessage(message map[string]any) bool {
	metadata, ok := message["metadata"].(map[string]any)
	return ok && metadata[MemoryMetaKey] == true
}

func RecalledMemoryIDs(messages []map[string]any, source ...string) map[string]struct{} {
	src := MemoryRecallSource
	if len(source) > 0 && source[0] != "" {
		src = source[0]
	}
	result := map[string]struct{}{}
	for _, message := range messages {
		if !IsMemoryContextMessage(message) {
			continue
		}
		metadata, _ := message["metadata"].(map[string]any)
		if metadata["source"] != src {
			continue
		}
		if values, ok := metadata["recall_ids"].([]string); ok {
			for _, value := range values {
				if strings.HasPrefix(value, "mem_") {
					result[value] = struct{}{}
				}
			}
			continue
		}
		if values, ok := metadata["recall_ids"].([]any); ok {
			for _, value := range values {
				if s, ok := value.(string); ok && strings.HasPrefix(s, "mem_") {
					result[s] = struct{}{}
				}
			}
			continue
		}
	}
	return result
}

func StripMemoryContextMessages(messages []map[string]any, source string) []map[string]any {
	var kept []map[string]any
	for _, message := range messages {
		if !IsMemoryContextMessage(message) {
			kept = append(kept, message)
			continue
		}
		metadata, _ := message["metadata"].(map[string]any)
		if source != "" && metadata["source"] != source {
			kept = append(kept, message)
		}
	}
	return kept
}

func BuildMetaUserMessage(text, source string) map[string]any {
	return map[string]any{
		"role":    "user",
		"content": []map[string]any{{"type": "text", "text": text}},
		"isMeta":  true,
		"metadata": map[string]any{
			MemoryMetaKey: true,
			"source":      source,
		},
	}
}

func joinDoubleNewline(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, part := range parts[1:] {
		out += "\n\n" + part
	}
	return out
}
