package agent

import (
	"strconv"
	"strings"
)

func RenderAgentToolUse(input AgentInput) string {
	subType := input.SubagentType
	if subType == "" {
		subType = "general-purpose"
	}
	bg := ""
	if input.RunInBackground {
		bg = " [bg]"
	}
	iso := ""
	if input.Isolation == "worktree" {
		iso = " [wt]"
	}
	if input.Model != "" {
		return "Agent(" + subType + bg + iso + ", model=" + input.Model + "): " + input.Description
	}
	return "Agent(" + subType + bg + iso + "): " + input.Description
}

func RenderAgentToolResult(content string, isError bool) string {
	if isError {
		return "Agent failed: " + truncateAgentString(content, 150)
	}
	var first string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(line, "[") {
			continue
		}
		first = truncateAgentString(line, 120)
		break
	}
	if first == "" {
		first = "Agent completed"
	}
	return "Agent done: " + first
}

func RenderTaskListToolUse() string {
	return "TaskList()"
}

func RenderTaskGetToolUse(input TaskGetInput) string {
	return "TaskGet(" + input.TaskID + ")"
}

func RenderTaskWaitToolUse(input TaskWaitInput) string {
	return "TaskWait(" + strconv.Itoa(len(input.TaskIDs)) + " tasks, timeout=" + strconv.Itoa(input.TimeoutSecond) + "s)"
}

func RenderTaskStopToolUse(input TaskStopInput) string {
	return "TaskStop(" + input.TaskID + ")"
}

func RenderSendMessageToolUse(input SendMessageInput) string {
	return "SendMessage(" + input.TaskID + ")"
}

func truncateAgentString(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
