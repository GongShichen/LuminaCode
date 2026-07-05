package tools

import (
	"path/filepath"
	"strconv"
	"strings"
)

func RenderGrepSearchToolUse(input GrepSearchInput) string {
	pattern := truncateString(input.Pattern, 60)
	target := "."
	if input.Path != "" && input.Path != "." {
		target = filepath.Base(input.Path)
	}
	flags := ""
	if input.CaseInsensitive {
		flags = " -i"
	}
	return "Grep '" + pattern + "' in " + target + flags
}

func RenderGrepSearchToolResult(content string, isError bool) string {
	if isError {
		return "Grep failed: " + truncateString(content, 150)
	}
	if strings.Contains(content, "No matches found") {
		return "0 matches"
	}
	lineCount := 0
	if content != "" {
		lineCount = strings.Count(content, "\n") + 1
	}
	return strconv.Itoa(lineCount) + " match(es)"
}

func RenderGroupedGrepSearchToolUse(inputs []GrepSearchInput) string {
	if len(inputs) == 0 {
		return ""
	}
	if len(inputs) == 1 {
		return RenderGrepSearchToolUse(inputs[0])
	}
	patterns := make([]string, 0, min(len(inputs), 5))
	for i, input := range inputs {
		if i >= 5 {
			break
		}
		patterns = append(patterns, truncateString(input.Pattern, 40))
	}
	return "Grep " + strconv.Itoa(len(inputs)) + " patterns: " + strings.Join(patterns, ", ")
}

func BackfillGrepSearchInput(input GrepSearchInput, execCtx ExecutionContext) GrepSearchInput {
	path := input.Path
	if path == "" {
		path = "."
	}
	if path != "." && !filepath.IsAbs(path) {
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
	return GrepSearchInput{
		Pattern:         input.Pattern,
		Path:            path,
		Glob:            input.Glob,
		HeadLimit:       grepHeadLimit(input.HeadLimit),
		OutputMode:      grepOutputMode(input.OutputMode),
		CaseInsensitive: input.CaseInsensitive,
	}
}

func RenderGlobMatchToolUse(input GlobMatchInput) string {
	target := "."
	if input.Path != "" && input.Path != "." {
		target = filepath.Base(input.Path)
	}
	return "Glob '" + input.Pattern + "' in " + target
}

func RenderGlobMatchToolResult(content string, isError bool) string {
	if isError {
		return "Glob failed: " + truncateString(content, 150)
	}
	if strings.Contains(content, "No files found") {
		return "0 files"
	}
	fileCount := 0
	if content != "" {
		fileCount = strings.Count(content, "\n") + 1
	}
	return strconv.Itoa(fileCount) + " file(s)"
}

func RenderGroupedGlobMatchToolUse(inputs []GlobMatchInput) string {
	if len(inputs) == 0 {
		return ""
	}
	if len(inputs) == 1 {
		return RenderGlobMatchToolUse(inputs[0])
	}
	patterns := make([]string, 0, min(len(inputs), 5))
	for i, input := range inputs {
		if i >= 5 {
			break
		}
		patterns = append(patterns, input.Pattern)
	}
	return "Glob " + strconv.Itoa(len(inputs)) + " patterns: " + strings.Join(patterns, ", ")
}

func BackfillGlobMatchInput(input GlobMatchInput, execCtx ExecutionContext) GlobMatchInput {
	path := input.Path
	if path == "" {
		path = "."
	}
	if path != "." && !filepath.IsAbs(path) {
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
	return GlobMatchInput{Pattern: input.Pattern, Path: path}
}

func RenderToolSearchToolUse(input ToolSearchInput) string {
	return "Tool search: " + truncateString(input.Query, 60)
}

func RenderToolSearchToolResult(content string, isError bool) string {
	if isError {
		return "Tool search failed: " + truncateString(content, 100)
	}
	return "Tool search (" + strconv.Itoa(len([]rune(content))) + " chars)"
}

func grepOutputMode(mode string) string {
	if mode == "" {
		return "files_with_matches"
	}
	return mode
}

func grepHeadLimit(limit *int) *int {
	if limit != nil {
		value := *limit
		return &value
	}
	value := 250
	return &value
}

func truncateString(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
