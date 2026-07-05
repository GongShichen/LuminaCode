package cli

import "strings"

var PromptSymbols = map[string]string{
	"normal": "❯",
	"yolo":   "⚡",
}

var ToolIcons = map[string]string{
	"read_file":   "📖",
	"write_file":  "✍️",
	"edit_file":   "📝",
	"grep_search": "🔍",
	"glob_match":  "🔎",
	"run_shell":   "💻",
}

var ToolDisplay = map[string]string{
	"read_file":   "Read",
	"write_file":  "Write",
	"edit_file":   "Edit",
	"grep_search": "Grep",
	"glob_match":  "Glob",
	"run_shell":   "Bash",
}

var RiskStyles = map[string]string{
	"low":    "yellow",
	"medium": "orange1",
	"high":   "bold red",
}

var RiskBorders = map[string]string{
	"low":    "yellow",
	"medium": "orange1",
	"high":   "red",
}

var RiskLabels = map[string]string{
	"low":    "Low Risk",
	"medium": "Medium Risk - Review Carefully",
	"high":   "High Risk - Dangerous Operation",
}

var RichTheme = map[string]string{
	"prompt.normal":    "bold cyan",
	"prompt.yolo":      "bold yellow",
	"header.brand":     "bold bright_cyan",
	"header.dim":       "dim",
	"header.highlight": "bold cyan",
	"header.label":     "bold white",
	"header.value":     "cyan",
	"thinking":         "dim italic",
	"tool.name":        "bold bright_cyan",
	"tool.args":        "dim",
	"tool.result":      "dim",
	"tool.success":     "green",
	"tool.error":       "bold red",
	"cost":             "dim",
	"separator":        "dim",
	"info":             "dim",
	"warning":          "bold yellow",
	"danger":           "bold red",
	"heading":          "bold white",
}

func ToolRiskLevel(toolName string, toolInput map[string]any) string {
	switch toolName {
	case "read_file", "grep_search", "glob_match":
		return "low"
	case "run_shell":
		command := inputString(toolInput, "command")
		for _, marker := range []string{"rm -rf", "sudo rm", "format", "mkfs", "dd if=", "> /dev/sd", "chmod 777"} {
			if strings.Contains(command, marker) {
				return "high"
			}
		}
		for _, marker := range []string{"rm ", "sudo", "chmod", "chown", "kill ", "pip", "npm install -g"} {
			if strings.Contains(command, marker) {
				return "medium"
			}
		}
		return "low"
	case "write_file", "edit_file":
		path := inputString(toolInput, "file_path")
		for _, marker := range []string{"/etc/", "/proc/", "/sys/", "/dev/", `C:\Windows\`, `C:\windows\`} {
			if strings.Contains(path, marker) {
				return "high"
			}
		}
		return "medium"
	default:
		return "low"
	}
}

func inputString(input map[string]any, key string) string {
	if input == nil {
		return ""
	}
	value, ok := input[key]
	if !ok || value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}
