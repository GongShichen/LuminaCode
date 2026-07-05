package agentContext

import (
	"regexp"
	"strings"

	"github.com/mohae/deepcopy"
)

type SnipPattern struct {
	Pattern     *regexp.Regexp
	Description string
	Replacement string
}

var snipPatterns = []SnipPattern{
	// pip install progress
	{regexp.MustCompile(`(?m)^\s*Collecting\s+\S+`), "pip collecting", ""},
	{regexp.MustCompile(`(?m)^\s*Downloading\s+\S+`), "pip downloading", ""},
	{regexp.MustCompile(`(?m)^\s*Requirement already satisfied:.*`), "pip satisfied", ""},
	{regexp.MustCompile(`(?m)^\s*Installing collected packages:.*`), "pip installing", ""},
	{regexp.MustCompile(`(?m)^Successfully installed[\s\S]+?(\n\n|\z)`), "pip success block", "[pip packages installed]${1}"},

	// npm / yarn
	{regexp.MustCompile(`(?m)^\s*npm\s+(install|run|build).*`), "npm cmd", ""},
	{regexp.MustCompile(`(?m)^added \d+ packages.*`), "npm added", ""},

	// Progress bars and counters
	{regexp.MustCompile(`(?m)^\[\d+/\d+\].*(?:completed|done).*`), "progress", ""},
	{regexp.MustCompile(`(?m)^\s*\d+%\|[█▉▊▋▌▍▎▏ ]+\|.*`), "progress bar", ""},

	// Docker pull/build noise
	{regexp.MustCompile(`(?m)^\s*[a-f0-9]{12}: (Pull|Download|Extract).*`), "docker layer", ""},
	{regexp.MustCompile(`(?m)^\s*Status: Downloaded.*`), "docker status", ""},

	// Cargo/cmake noise
	{regexp.MustCompile(`(?m)^\s*Compiling\s+\S+`), "cargo compiling", ""},
	{regexp.MustCompile(`(?m)^\s*Checking\s+\S+`), "cargo checking", ""},
}

var manyBlankLinesPattern = regexp.MustCompile(`\n{4,}`)
var snipTag = "<snip>Content snipped to save context space</snip>"

const (
	defaultPreserveLastNTurns = 2
	defaultSnipCharThreshold  = 2000
)

func SnipToolResult(content string) string {
	result := content
	for _, pattern := range snipPatterns {
		result = pattern.Pattern.ReplaceAllString(result, pattern.Replacement)
	}
	result = manyBlankLinesPattern.ReplaceAllString(result, "\n\n\n")
	result = strings.TrimSpace(result)
	return result
}

func copyMap(m map[string]any) map[string]any {
	copied := make(map[string]any, len(m))
	for k, v := range m {
		copied[k] = v
	}
	return copied
}

func SnipMessages(messages []map[string]any) []map[string]any {
	result := make([]map[string]any, 0, len(messages))

	for _, msg := range messages {
		content, ok := contentBlocks(msg["content"])
		if !ok {
			result = append(result, msg)
			continue
		}

		newContent := make([]map[string]any, 0, len(content))

		for _, block := range content {
			if GetString(block, "type", "") == "tool_result" {
				raw := GetString(block, "content", "").(string)
				snipped := SnipToolResult(raw)

				newBlock := copyMap(block)
				newBlock["content"] = snipped

				newContent = append(newContent, newBlock)
			} else {
				newContent = append(newContent, block)
			}
		}

		newMsg := copyMap(msg)
		newMsg["content"] = contentLike(msg["content"], newContent)
		result = append(result, newMsg)
	}

	return result
}

func isToolResultMessage(msg map[string]any) bool {
	if role, ok := msg["role"].(string); !ok || role != "user" {
		return false
	}
	content, ok := contentBlocks(msg["content"])
	if !ok {
		return false
	}
	for _, block := range content {
		blockType, ok := block["type"].(string)
		if !ok {
			continue
		}
		if blockType == "tool_result" {
			return true
		}
	}
	return false
}

func SnipCompactIfNeeded(
	messages []map[string]any,
	preserveLastNTurns int,
	snipCharThreshold int,
) ([]map[string]any, int) {
	copied := deepcopy.Copy(messages)

	result, ok := copied.([]map[string]any)
	if !ok {
		return messages, 0
	}

	var toolResultIndices []int
	for i, msg := range result {
		if isToolResultMessage(msg) {
			toolResultIndices = append(toolResultIndices, i)
		}
	}

	if len(toolResultIndices) == 0 {
		return result, 0
	}

	freshStart := MaxInt(0, len(toolResultIndices)-preserveLastNTurns)

	staleIndices := make(map[int]struct{}, freshStart)
	for _, idx := range toolResultIndices[:freshStart] {
		staleIndices[idx] = struct{}{}
	}

	snipTokensFreed := 0

	for idx := range staleIndices {
		msg := result[idx]

		blocks, ok := contentBlocks(msg["content"])
		if !ok {
			continue
		}

		for _, block := range blocks {
			blockType, _ := block["type"].(string)
			if blockType != "tool_result" {
				continue
			}

			originalContent, ok := block["content"].(string)
			if !ok {
				continue
			}

			if len([]rune(originalContent)) > snipCharThreshold {
				snipTokensFreed += RoughEstimate(originalContent) - RoughEstimate(snipTag)
				block["content"] = snipTag
			}
		}
	}

	return result, snipTokensFreed
}

func SnipCompactIfNeededDefault(messages []map[string]any) ([]map[string]any, int) {
	return SnipCompactIfNeeded(messages, defaultPreserveLastNTurns, defaultSnipCharThreshold)
}
