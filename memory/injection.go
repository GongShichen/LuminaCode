package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	MemoryMetaKey      = "lumina_memory_context"
	MemoryIndexSource  = "memory_index"
	MemoryRecallSource = "memory_recall"
)

func BuildMemoryIndexMessage(memoryDir string) map[string]any {
	memoryIndex := LoadMemoryIndex(memoryDir)
	if memoryIndex == "" {
		memoryIndex = "(no indexed memories yet)"
	}
	indexPath := filepath.Join(memoryDir, IndexFilename)
	text := "<system-reminder>\n" +
		"Contents of " + indexPath + " (user's auto-memory, persists across conversations):\n\n" +
		memoryIndex + "\n</system-reminder>"
	return BuildMetaUserMessage(text, MemoryIndexSource)
}

func BuildRecalledMemoriesMessage(recalled []MemoryRecall) map[string]any {
	if len(recalled) == 0 {
		return nil
	}
	parts := make([]string, 0, len(recalled))
	filenames := make([]string, 0, len(recalled))
	recallIDs := make([]string, 0, len(recalled))
	for _, item := range recalled {
		parts = append(parts, MemoryHeader(item.FilePath, time.Now())+"\n\n"+item.Content)
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

func MemoryHeader(path string, now time.Time) string {
	info, err := os.Stat(path)
	if err != nil {
		return "Memory: " + path + ":"
	}
	if now.IsZero() {
		now = time.Now()
	}
	saved := time.Date(info.ModTime().Year(), info.ModTime().Month(), info.ModTime().Day(), 0, 0, 0, 0, now.Location())
	current := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	ageDays := int(current.Sub(saved).Hours() / 24)
	if ageDays < 0 {
		ageDays = 0
	}
	if ageDays == 0 {
		return "Memory (saved today): " + path + ":"
	}
	if ageDays == 1 {
		return "Memory (saved yesterday): " + path + ":"
	}
	warning := fmt.Sprintf("This memory is %d days old. Memories are point-in-time observations, not live state - claims about code behavior or file:line citations may be outdated. Verify against current code before asserting as fact.", ageDays)
	return warning + "\n\nMemory: " + path + ":"
}

func IsMemoryContextMessage(message map[string]any) bool {
	metadata, ok := message["metadata"].(map[string]any)
	return ok && metadata[MemoryMetaKey] == true
}

func RecalledMemoryFilenames(messages []map[string]any) map[string]struct{} {
	result := map[string]struct{}{}
	for _, message := range messages {
		if !IsMemoryContextMessage(message) {
			continue
		}
		metadata, _ := message["metadata"].(map[string]any)
		if metadata["source"] != MemoryRecallSource {
			continue
		}
		for _, value := range stringSliceFromAny(metadata["filenames"]) {
			if filepath.Ext(value) == ".md" {
				result[value] = struct{}{}
			}
		}
	}
	return result
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
				if filepath.Ext(value) == ".md" {
					result[value] = struct{}{}
				}
			}
			continue
		}
		if values, ok := metadata["recall_ids"].([]any); ok {
			for _, value := range values {
				if s, ok := value.(string); ok && filepath.Ext(s) == ".md" {
					result[s] = struct{}{}
				}
			}
			continue
		}
		for _, value := range stringSliceFromAny(metadata["filenames"]) {
			if filepath.Ext(value) == ".md" {
				result[value] = struct{}{}
			}
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

func stringSliceFromAny(raw any) []string {
	switch values := raw.(type) {
	case []string:
		return values
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if s, ok := value.(string); ok {
				result = append(result, s)
			}
		}
		return result
	default:
		return nil
	}
}
