package tools

import "path/filepath"

func (t *ReadFileTool) BackfillObservableInput(input any, execCtx ExecutionContext) any {
	return BackfillReadFileInput(deref[ReadFileInput](input), execCtx)
}

func (t *WriteFileTool) BackfillObservableInput(input any, execCtx ExecutionContext) any {
	return BackfillWriteFileInput(deref[WriteFileInput](input), execCtx)
}

func (t *EditFileTool) BackfillObservableInput(input any, execCtx ExecutionContext) any {
	in := deref[EditFileInput](input)
	path := in.FilePath
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
	return EditFileInput{
		FilePath:   path,
		OldString:  in.OldString,
		NewString:  in.NewString,
		ReplaceAll: in.ReplaceAll,
	}
}

func (t *BashTool) BackfillObservableInput(input any, _ ExecutionContext) any {
	return BackfillBashInput(deref[BashInput](input))
}

func (t *GrepSearchTool) BackfillObservableInput(input any, execCtx ExecutionContext) any {
	return BackfillGrepSearchInput(deref[GrepSearchInput](input), execCtx)
}

func (t *GlobMatchTool) BackfillObservableInput(input any, execCtx ExecutionContext) any {
	return BackfillGlobMatchInput(deref[GlobMatchInput](input), execCtx)
}

func (t *NotebookEditTool) BackfillObservableInput(input any, execCtx ExecutionContext) any {
	return BackfillNotebookEditInput(deref[NotebookEditInput](input), execCtx)
}
