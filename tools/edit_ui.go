package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

func RenderEditFileToolUse(filePath, oldString, newString string, replaceAll bool) string {
	fileName := filepath.Base(filePath)
	modeLabel := ""
	if replaceAll {
		modeLabel = " (all)"
	}
	header := "edit_file" + modeLabel + ": " + fileName
	diff := renderInputDiff(fileName, oldString, newString, 2)
	if diff == "" {
		oldLines := difflib.SplitLines(oldString)
		newLines := difflib.SplitLines(newString)
		if len(oldLines) > 0 && len(newLines) > 0 {
			return strings.Join([]string{
				header,
				"  [-]" + truncateDisplayLineWithEllipsis(strings.TrimRight(oldLines[0], "\r\n"), 100),
				"  [+]" + truncateDisplayLineWithEllipsis(strings.TrimRight(newLines[0], "\r\n"), 100),
			}, "\n")
		}
		return header
	}
	return header + "\n" + diff
}

func RenderGroupedEditFileToolUse(inputs []EditFileInput) string {
	if len(inputs) == 0 {
		return ""
	}
	if len(inputs) == 1 {
		in := inputs[0]
		return RenderEditFileToolUse(in.FilePath, in.OldString, in.NewString, in.ReplaceAll)
	}
	type group struct {
		filePath string
		edits    []EditFileInput
	}
	seen := map[string]int{}
	var groups []group
	for _, in := range inputs {
		idx, ok := seen[in.FilePath]
		if !ok {
			seen[in.FilePath] = len(groups)
			groups = append(groups, group{filePath: in.FilePath})
			idx = len(groups) - 1
		}
		groups[idx].edits = append(groups[idx].edits, in)
	}
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		if len(g.edits) == 1 {
			in := g.edits[0]
			parts = append(parts, RenderEditFileToolUse(in.FilePath, in.OldString, in.NewString, in.ReplaceAll))
			continue
		}
		oldLines := 0
		newLines := 0
		for _, edit := range g.edits {
			oldLines += countDisplayLines(edit.OldString)
			newLines += countDisplayLines(edit.NewString)
		}
		parts = append(parts, fmt.Sprintf("edit_file: %s (%d edits, [-]%d lines, [+]%d lines)", filepath.Base(g.filePath), len(g.edits), oldLines, newLines))
	}
	return strings.Join(parts, "\n")
}

func RenderEditFileToolResult(content string, isError bool) string {
	if isError {
		return renderEditError(content)
	}
	if strings.Contains(content, "\n---DIFF---\n") {
		parts := strings.SplitN(content, "\n---DIFF---\n", 2)
		summary := parts[0]
		diffText := strings.TrimSpace(parts[1])
		if diffText != "" {
			return summary + "\n" + diffText
		}
		return summary
	}
	if strings.Contains(content, "occurrence(s) replaced") {
		return content
	}
	return "Edit: " + truncateDisplayLine(content, 120)
}

func RenderEditErrorContext(filePath, oldString string, fileContent ...string) string {
	content := ""
	if len(fileContent) > 0 {
		content = fileContent[0]
	} else {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return "Cannot read " + filePath + " for diagnostics."
		}
		content = string(data)
	}
	oldFirstLine := strings.TrimSpace(strings.Split(oldString, "\n")[0])
	if oldFirstLine == "" {
		return "Error: old_string appears to be empty or whitespace-only."
	}
	fileLines := strings.Split(content, "\n")
	bestIdx := -1
	bestScore := 0.0
	for i, line := range fileLines {
		score := lineSimilarity(oldFirstLine, strings.TrimSpace(line))
		if score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	if bestScore < 0.3 {
		return fmt.Sprintf("No similar lines found in %s.\n  Searched for: '%s'\n  The file may have been completely rewritten.", filepath.Base(filePath), truncateDisplayLineWithEllipsis(oldFirstLine, 100))
	}
	start := max(0, bestIdx-3)
	end := min(len(fileLines), bestIdx+4)
	lines := []string{
		fmt.Sprintf("Closest match at line %d (%.0f%% similarity):", bestIdx+1, bestScore*100),
		"",
	}
	for i := start; i < end; i++ {
		marker := "   "
		if i == bestIdx {
			marker = ">>>"
		}
		lines = append(lines, fmt.Sprintf("  %s %4d  %s", marker, i+1, truncateDisplayLineWithEllipsis(fileLines[i], 120)))
	}
	lines = append(lines, "", "  Expected: [-]"+truncateDisplayLineWithEllipsis(oldFirstLine, 100))
	return strings.Join(lines, "\n")
}

func renderInputDiff(fileName, oldString, newString string, contextLines int) string {
	diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(oldString),
		B:        difflib.SplitLines(newString),
		FromFile: "a/" + fileName,
		ToFile:   "b/" + fileName,
		Context:  contextLines,
	})
	if err != nil {
		return ""
	}
	diff = strings.TrimRight(diff, "\n")
	if diff == "" {
		return ""
	}
	lines := strings.Split(diff, "\n")
	if len(lines) > 14 {
		lines = append(lines[:14], "  ... (diff truncated)")
	}
	return strings.Join(lines, "\n")
}

func renderEditError(content string) string {
	code, ok := parseEditErrorCode(content)
	if !ok {
		return "Edit failed: " + truncateDisplayLine(content, 200)
	}
	label, ok := editErrorLabel(code)
	if !ok {
		return "Edit failed: " + truncateDisplayLine(content, 200)
	}
	switch code {
	case 8:
		return "Edit failed (" + label + ").\n  The file may have changed since it was last read.\n  Tip: re-read the file to see current content, then retry the edit."
	case 5:
		return "Edit blocked (" + label + ").\n  Use the notebook_edit tool to modify .ipynb files.\n  If notebook_edit is not in your tool list, use tool_search\n  with 'select:notebook_edit' to load it first.\n  edit_file only works with plain-text files."
	case 13:
		return "Edit failed (" + label + ").\n  A previous edit on this file inserted text that matches\n  old_string. Re-read the file and adjust the edit to target\n  the original content."
	case 9:
		return "Edit failed (" + label + ").\n  Include more surrounding context to make the match unique,\n  or set replace_all=true to replace all occurrences."
	case 1:
		return "Edit skipped (" + label + ")."
	default:
		return "Edit failed (" + label + ")."
	}
}

func parseEditErrorCode(content string) (int, bool) {
	matches := regexp.MustCompile(`\[ErrCode (\d+)\]`).FindStringSubmatch(content)
	if len(matches) != 2 {
		return 0, false
	}
	code, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, false
	}
	return code, true
}

func editErrorLabel(code int) (string, bool) {
	labels := map[int]string{
		1:  "old_string and new_string are identical",
		2:  "permission denied",
		3:  "empty old_string on existing file",
		4:  "file not found",
		5:  "use notebook_edit for .ipynb files",
		6:  "file not yet read",
		7:  "file modified since last read",
		8:  "string to replace not found in file",
		9:  "multiple matches found",
		10: "file exceeds size limit",
		11: "file write error",
		12: "file read error",
		13: "cascading edit detected",
	}
	if label, ok := labels[code]; ok {
		return label, true
	}
	return "", false
}

func truncateDisplayLine(line string, maxLen int) string {
	runes := []rune(line)
	if len(runes) <= maxLen {
		return line
	}
	if maxLen <= 0 {
		return ""
	}
	return string(runes[:maxLen])
}

func truncateDisplayLineWithEllipsis(line string, maxLen int) string {
	runes := []rune(line)
	if len(runes) <= maxLen {
		return line
	}
	if maxLen <= 0 {
		return ""
	}
	if maxLen <= 3 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-3]) + "..."
}

func countDisplayLines(text string) int {
	count := strings.Count(text, "\n") + 1
	if count < 1 {
		return 1
	}
	return count
}
