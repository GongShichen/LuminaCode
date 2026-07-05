package tools

import (
	"path/filepath"
	"strconv"
	"strings"
)

func RenderNotebookEditToolUse(input NotebookEditInput) string {
	name := filepath.Base(input.NotebookPath)
	mode := notebookEditMode(input.EditMode)
	modeLabel := ""
	if mode != "replace" {
		modeLabel = " (" + mode + ")"
	}
	cellID := input.CellID
	if len(cellID) > 8 {
		cellID = cellID[:8]
	}
	return "Edit notebook" + modeLabel + ": " + name + " cell[" + cellID + "]"
}

func RenderNotebookEditToolResult(content string, isError bool) string {
	if isError {
		return "Notebook edit failed: " + truncateDisplayLine(content, 150)
	}
	return "Notebook edit OK"
}

func RenderGroupedNotebookEditToolUse(inputs []NotebookEditInput) string {
	if len(inputs) == 0 {
		return ""
	}
	if len(inputs) == 1 {
		return RenderNotebookEditToolUse(inputs[0])
	}
	names := map[string]struct{}{}
	for _, input := range inputs {
		names[filepath.Base(input.NotebookPath)] = struct{}{}
	}
	orderedNames := make([]string, 0, len(names))
	for _, input := range inputs {
		name := filepath.Base(input.NotebookPath)
		if _, ok := names[name]; !ok {
			continue
		}
		orderedNames = append(orderedNames, name)
		delete(names, name)
	}
	return "Edit notebook: " + strconv.Itoa(len(inputs)) + " edits in " + strings.Join(orderedNames, ", ")
}

func BackfillNotebookEditInput(input NotebookEditInput, execCtx ExecutionContext) NotebookEditInput {
	path := input.NotebookPath
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
	return NotebookEditInput{
		NotebookPath: path,
		CellID:       input.CellID,
		NewSource:    input.NewSource,
		CellType:     input.CellType,
		EditMode:     notebookEditMode(input.EditMode),
	}
}
