package cli

import (
	"fmt"
	"strings"
)

const (
	RichUnicode = "rich_unicode"
	ASCIISafe   = "ascii_safe"
	YoloLabel   = "⚡ YOLO"
)

var Phase1PermissionActionLabels = []string{"允许一次", "本会话总是允许", "拒绝"}

var DisplayRiskLabels = map[string]string{
	"low":    "低风险",
	"medium": "需确认",
	"high":   "高风险",
}

var richSymbols = map[string]string{
	"prompt.normal":     "❯",
	"prompt.yolo":       "⚡",
	"toolbar.separator": " │ ",
	"marker.success":    "✓",
	"marker.error":      "✗",
	"marker.pointer":    "❯",
	"marker.permission": "⏺",
	"tool.read_file":    "📖",
	"tool.write_file":   "✍️",
	"tool.edit_file":    "📝",
	"tool.grep_search":  "🔍",
	"tool.glob_match":   "🔎",
	"tool.run_shell":    "💻",
	"tool.default":      "🔧",
}

var asciiSymbols = map[string]string{
	"prompt.normal":     ">",
	"prompt.yolo":       "!",
	"toolbar.separator": " | ",
	"marker.success":    "OK",
	"marker.error":      "X",
	"marker.pointer":    ">",
	"marker.permission": "*",
	"tool.read_file":    "[R]",
	"tool.write_file":   "[W]",
	"tool.edit_file":    "[E]",
	"tool.grep_search":  "[G]",
	"tool.glob_match":   "[O]",
	"tool.run_shell":    "[S]",
	"tool.default":      "[T]",
}

var ToolbarSeparator = richSymbols["toolbar.separator"]

type ToolbarState interface {
	TurnCountValue() int
	TokenTotals() (int, int)
	YoloEnabled() bool
}

func DetectDisplayMode(encoding string) string {
	normalized := strings.ToLower(strings.TrimSpace(encoding))
	switch normalized {
	case "utf-8", "utf8", "utf_8", "cp65001":
		return RichUnicode
	default:
		return ASCIISafe
	}
}

func GetDisplaySymbols(mode string) map[string]string {
	source := asciiSymbols
	if mode == RichUnicode {
		source = richSymbols
	}
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func TranslateBackendRiskLevel(level string) string {
	normalized := strings.ToLower(strings.TrimSpace(level))
	switch normalized {
	case "high":
		return "high"
	case "normal":
		return "medium"
	case "low", "medium":
		return normalized
	default:
		return "medium"
	}
}

func NormalizePermissionAnswer(answer string) string {
	normalized := strings.ToLower(strings.TrimSpace(answer))
	if normalized == "" {
		return "deny"
	}
	direct := map[string]string{
		"y":      "once",
		"yes":    "once",
		"n":      "deny",
		"no":     "deny",
		"a":      "always",
		"always": "always",
		"d":      "deny",
		"deny":   "deny",
		"never":  "deny",
	}
	if mapped, ok := direct[normalized]; ok {
		return mapped
	}
	switch normalized[0] {
	case 'y':
		return "once"
	case 'a':
		return "always"
	default:
		return "deny"
	}
}

func FormatCWDForDisplay(cwd string, maxWidth ...int) string {
	width := 55
	if len(maxWidth) > 0 && maxWidth[0] > 0 {
		width = maxWidth[0]
	}
	runes := []rune(cwd)
	if len(runes) <= width {
		return cwd
	}
	headEnd := width / 2
	tailLen := width/2 - 3
	tailStart := len(runes) - tailLen
	if tailLen == 0 {
		tailStart = 0
	} else if tailLen < 0 {
		tailStart = -tailLen
	}
	if tailStart < 0 {
		tailStart = 0
	}
	if tailStart > len(runes) {
		tailStart = len(runes)
	}
	return string(runes[:headEnd]) + "..." + string(runes[tailStart:])
}

func BuildSessionToolbar(state ToolbarState, separator ...string) string {
	if state == nil {
		return ""
	}
	sep := ToolbarSeparator
	if len(separator) > 0 {
		sep = separator[0]
	}
	var parts []string
	turns := state.TurnCountValue()
	if turns > 0 {
		parts = append(parts, fmt.Sprintf("T%d", turns))
	}
	inputTokens, outputTokens := state.TokenTotals()
	totalTokens := inputTokens + outputTokens
	if totalTokens > 0 {
		if totalTokens >= 1000 {
			parts = append(parts, fmt.Sprintf("%dK tok", totalTokens/1000))
		} else {
			parts = append(parts, fmt.Sprintf("%d tok", totalTokens))
		}
	}
	if state.YoloEnabled() {
		parts = append(parts, YoloLabel)
	}
	return strings.Join(parts, sep)
}
