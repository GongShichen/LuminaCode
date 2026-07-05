package tools

import (
	"path/filepath"
	"strconv"
	"strings"

	bashpkg "LuminaCode/tools/bash"
)

func RenderReadFileToolUse(input ReadFileInput) string {
	name := filepath.Base(input.FilePath)
	if input.Offset != 0 || input.Limit != nil {
		limit := 0
		if input.Limit != nil {
			limit = *input.Limit
		}
		return "📖 Read " + name + " (L" + strconv.Itoa(input.Offset) + "-" + strconv.Itoa(input.Offset+limit) + ")"
	}
	return "📖 Read " + name
}

func RenderReadFileToolResult(content string, isError bool) string {
	if isError {
		return "Read failed: " + truncateString(content, 150)
	}
	lineCount := 0
	if content != "" {
		lineCount = strings.Count(content, "\n") + 1
	}
	return "Read " + strconv.Itoa(lineCount) + " lines"
}

func RenderGroupedReadFileToolUse(inputs []ReadFileInput) string {
	if len(inputs) == 0 {
		return ""
	}
	if len(inputs) == 1 {
		return RenderReadFileToolUse(inputs[0])
	}
	names := make([]string, 0, min(len(inputs), 5))
	for i, input := range inputs {
		if i >= 5 {
			break
		}
		names = append(names, filepath.Base(input.FilePath))
	}
	return "📖 Read " + strconv.Itoa(len(inputs)) + " files: " + strings.Join(names, ", ")
}

func BackfillReadFileInput(input ReadFileInput, execCtx ExecutionContext) ReadFileInput {
	path := input.FilePath
	if path != "" && !filepath.IsAbs(path) {
		cwd := stringFromExecCtx(execCtx, "cwd")
		if cwd == "" {
			cwd = "."
		}
		if resolved, err := filepath.Abs(filepath.Join(cwd, path)); err == nil {
			path = resolved
		} else {
			path = filepath.Join(cwd, path)
		}
	}
	return ReadFileInput{FilePath: path, Offset: input.Offset, Limit: input.Limit}
}

func RenderWriteFileToolUse(input WriteFileInput) string {
	name := filepath.Base(input.FilePath)
	size := len([]rune(input.Content))
	if size >= 1024 {
		return "Write " + name + " (" + strconv.Itoa(size/1024) + "KB)"
	}
	return "Write " + name + " (" + strconv.Itoa(size) + "B)"
}

func RenderWriteFileToolResult(content string, isError bool) string {
	if isError {
		return "Write failed: " + truncateString(content, 150)
	}
	return "Write OK"
}

func RenderGroupedWriteFileToolUse(inputs []WriteFileInput) string {
	if len(inputs) == 0 {
		return ""
	}
	if len(inputs) == 1 {
		return RenderWriteFileToolUse(inputs[0])
	}
	names := make([]string, 0, min(len(inputs), 5))
	for i, input := range inputs {
		if i >= 5 {
			break
		}
		names = append(names, filepath.Base(input.FilePath))
	}
	return "Write " + strconv.Itoa(len(inputs)) + " files: " + strings.Join(names, ", ")
}

func BackfillWriteFileInput(input WriteFileInput, execCtx ExecutionContext) WriteFileInput {
	path := input.FilePath
	if path != "" && !filepath.IsAbs(path) {
		cwd := stringFromExecCtx(execCtx, "cwd")
		if cwd == "" {
			cwd = "."
		}
		if resolved, err := filepath.Abs(filepath.Join(cwd, path)); err == nil {
			path = resolved
		} else {
			path = filepath.Join(cwd, path)
		}
	}
	return WriteFileInput{FilePath: path, Content: input.Content}
}

func RenderBashToolUse(input BashInput) string {
	command := input.Command
	if len([]rune(command)) > 120 {
		command = string([]rune(command)[:117]) + "..."
	}
	return "Bash " + command
}

func RenderBashToolResult(content string, isError bool) string {
	if isError {
		return "Bash failed: " + truncateString(content, 150)
	}
	if strings.HasPrefix(content, "[Exit code:") {
		return strings.Split(content, "\n")[0]
	}
	firstLine := ""
	trimmed := strings.TrimSpace(content)
	if trimmed != "" {
		firstLine = strings.Split(trimmed, "\n")[0]
	}
	firstLine = truncateString(firstLine, 150)
	if firstLine == "" {
		return "(empty output)"
	}
	return firstLine
}

func RenderGroupedBashToolUse(inputs []BashInput) string {
	if len(inputs) == 0 {
		return ""
	}
	if len(inputs) == 1 {
		return RenderBashToolUse(inputs[0])
	}
	commands := make([]string, 0, min(len(inputs), 3))
	for i, input := range inputs {
		if i >= 3 {
			break
		}
		commands = append(commands, truncateString(input.Command, 60))
	}
	return "Bash (" + strconv.Itoa(len(inputs)) + " commands): " + strings.Join(commands, "; ")
}

func BackfillBashInput(input BashInput) BashInput {
	description := input.Description
	if description == "" {
		description = bashpkg.ClassifyBashCommand(input.Command)
	}
	return BashInput{
		Command:                   input.Command,
		Timeout:                   input.Timeout,
		Description:               description,
		RunInBackground:           input.RunInBackground,
		DangerouslyDisableSandbox: input.DangerouslyDisableSandbox,
	}
}
