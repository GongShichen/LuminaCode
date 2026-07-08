package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"LuminaCode/config"
	bashpkg "LuminaCode/tools/bash"
	filetool "LuminaCode/tools/file"

	"github.com/bmatcuk/doublestar/v4"
	orderedmap "github.com/pb33f/ordered-map/v2"
	"github.com/pmezard/go-difflib/difflib"
)

type readFileStateOwner interface {
	SetReadFileState(path string, entry filetool.FileStateEntry)
	GetReadFileState(path string) (filetool.FileStateEntry, bool)
}

type ReadFileInput struct {
	FilePath string `json:"file_path" jsonschema_description:"The absolute path to the file to read"`
	Offset   int    `json:"offset,omitempty" jsonschema:"default=0" jsonschema_description:"Line number to start reading from (0-indexed)"`
	Limit    *int   `json:"limit,omitempty" jsonschema:"nullable,default=null" jsonschema_description:"Maximum number of lines to read"`
}

type ReadFileTool struct{ BaseTool }

func NewReadFileTool() *ReadFileTool {
	return &ReadFileTool{BaseTool{Spec: ToolSpec{
		Name:            "read_file",
		Description:     "Read a file from the local filesystem with line numbers. Supports offset and limit for large files.",
		InputPrototype:  ReadFileInput{},
		ReadOnly:        BoolPtr(true),
		ConcurrencySafe: BoolPtr(true),
		Destructive:     BoolPtr(false),
		MaxOutputChars:  200_000,
	}}}
}

func (t *ReadFileTool) ValidateInput(execCtx ExecutionContext, input any) (bool, string) {
	in := deref[ReadFileInput](input)
	path := resolveToolPath(execCtx, in.FilePath)
	if ok, msg := checkAllowedReadRoots(path, execCtx["allowed_read_roots"]); !ok {
		return false, msg
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Sprintf("File not found: %s", in.FilePath)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Sprintf("Not a file: %s", in.FilePath)
	}
	return true, ""
}

func (t *ReadFileTool) Execute(_ context.Context, execCtx ExecutionContext, input any) (string, error) {
	in := deref[ReadFileInput](input)
	path := resolveToolPath(execCtx, in.FilePath)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("Error reading file: %s", err), nil
	}
	content := decodeToolBytes(data)
	lineEndings := detectLineEndings(content)
	if lineEndings == "\r\n" {
		content = strings.ReplaceAll(content, "\r\n", "\n")
	}
	updateReadState(execCtx, path, content, lineEndings, in.Offset > 0 || in.Limit != nil)
	lines := strings.Split(content, "\n")
	start := in.Offset
	if start < 0 {
		start = 0
	}
	end := len(lines)
	if in.Limit != nil && *in.Limit != 0 {
		end = start + *in.Limit
		if end > len(lines) {
			end = len(lines)
		}
	}
	if start > len(lines) || end <= start {
		return "", nil
	}
	var out strings.Builder
	for i := start; i < end; i++ {
		if i > start {
			out.WriteString("\n")
		}
		out.WriteString(fmt.Sprintf("%d\t%s", i+1, lines[i]))
	}
	return out.String(), nil
}

type WriteFileInput struct {
	FilePath string `json:"file_path" jsonschema_description:"The absolute path to the file to write"`
	Content  string `json:"content" jsonschema_description:"The content to write to the file"`
}

type WriteFileTool struct{ BaseTool }

func NewWriteFileTool() *WriteFileTool {
	return &WriteFileTool{BaseTool{Spec: ToolSpec{
		Name:             "write_file",
		Description:      "Create or overwrite a file. Parent directories are created automatically if they do not exist.",
		InputPrototype:   WriteFileInput{},
		ReadOnly:         BoolPtr(false),
		ConcurrencySafe:  BoolPtr(false),
		Destructive:      BoolPtr(true),
		ConfirmFilePaths: true,
	}}}
}

func (t *WriteFileTool) ValidateInput(execCtx ExecutionContext, input any) (bool, string) {
	in := deref[WriteFileInput](input)
	path := resolveToolPath(execCtx, in.FilePath)
	return checkAllowedWriteRoots(path, execCtx["allowed_write_roots"])
}

func (t *WriteFileTool) Execute(_ context.Context, execCtx ExecutionContext, input any) (string, error) {
	in := deref[WriteFileInput](input)
	path := resolveToolPath(execCtx, in.FilePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Sprintf("Error writing file: %s", err), nil
	}
	if err := os.WriteFile(path, []byte(in.Content), 0o644); err != nil {
		return fmt.Sprintf("Error writing file: %s", err), nil
	}
	return fmt.Sprintf("File written successfully: %s (%d characters)", path, len([]rune(in.Content))), nil
}

type EditFileInput struct {
	FilePath   string `json:"file_path" jsonschema_description:"The absolute path to the file to edit"`
	OldString  string `json:"old_string" jsonschema_description:"The exact text to replace"`
	NewString  string `json:"new_string" jsonschema_description:"The text to replace it with (must differ from old_string)"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"default=false" jsonschema_description:"If true, replace all occurrences. Otherwise old_string must appear exactly once."`
}

type EditFileTool struct{ BaseTool }

func NewEditFileTool() *EditFileTool {
	return &EditFileTool{BaseTool{Spec: ToolSpec{
		Name:             "edit_file",
		Description:      "Perform exact string replacement in an existing file. By default, old_string must appear exactly once in the file. Use replace_all=true to substitute every occurrence.",
		InputPrototype:   EditFileInput{},
		ReadOnly:         BoolPtr(false),
		ConcurrencySafe:  BoolPtr(false),
		Destructive:      BoolPtr(false),
		ConfirmFilePaths: true,
	}}}
}

func (t *EditFileTool) ValidateInput(execCtx ExecutionContext, input any) (bool, string) {
	in := deref[EditFileInput](input)
	path := resolveToolPath(execCtx, in.FilePath)
	if ok, msg := checkAllowedWriteRoots(path, execCtx["allowed_write_roots"]); !ok {
		return false, formatEditError(editFileNotFound, msg)
	}
	if in.OldString == in.NewString {
		return false, formatEditError(editNoOp, "old_string and new_string must be different.")
	}
	info, err := os.Stat(path)
	if err != nil {
		hint := ""
		if !filepath.IsAbs(in.FilePath) {
			hint = " (path is relative — use absolute paths)"
		}
		return false, formatEditError(editFileNotFound, fmt.Sprintf("File not found: %s%s", in.FilePath, hint))
	}
	if !info.Mode().IsRegular() {
		return false, formatEditError(editFileNotFound, fmt.Sprintf("Not a file: %s", in.FilePath))
	}
	if strings.EqualFold(filepath.Ext(path), ".ipynb") {
		return false, formatEditError(editNotebookRedirect, fmt.Sprintf("Notebook files must use notebook_edit tool: %s", in.FilePath))
	}
	if in.OldString != "" && !truthy(execCtx["skip_read_before_edit"]) {
		owner, _ := execCtx["parent_state"].(readFileStateOwner)
		if owner == nil {
			return false, formatEditError(editUnreadFile, "File has not been read yet. Read it first before editing.")
		}
		fileState, ok := owner.GetReadFileState(path)
		if !ok {
			fileState, ok = owner.GetReadFileState(in.FilePath)
		}
		if !ok {
			return false, formatEditError(editUnreadFile, "File has not been read yet. Read it first before editing.")
		}
		if fileState.IsPartialView {
			return false, formatEditError(editUnreadFile, "File was only partially read. Re-read the full file before editing.")
		}
		currentMTime := fileMTime(path)
		if currentMTime > fileState.TimeStamp {
			if runtime.GOOS == "windows" {
				data, err := os.ReadFile(path)
				if err != nil {
					return false, formatEditError(editStaleRead, "File has been modified since it was last read. Re-read the file before editing.")
				}
				currentContent := strings.ReplaceAll(decodeToolBytes(data), "\r\n", "\n")
				cachedContent := strings.ReplaceAll(fileState.Content, "\r\n", "\n")
				if currentContent == cachedContent {
					fileState.TimeStamp = currentMTime
					owner.SetReadFileState(path, fileState)
				} else {
					return false, formatEditError(editStaleRead, "File has been modified since it was last read. Re-read the file before editing.")
				}
			} else {
				return false, formatEditError(editStaleRead, "File has been modified since it was last read. Re-read the file before editing.")
			}
		}
	}
	return true, ""
}

func (t *EditFileTool) Execute(_ context.Context, execCtx ExecutionContext, input any) (string, error) {
	in := deref[EditFileInput](input)
	path := resolveToolPath(execCtx, in.FilePath)
	data, err := os.ReadFile(path)
	if err != nil {
		return formatEditError(editReadFailed, fmt.Sprintf("Error reading file: %s", err)), nil
	}
	fileContent := decodeToolBytes(data)
	lineEndings := detectLineEndings(fileContent)
	if lineEndings == "\r\n" {
		fileContent = strings.ReplaceAll(fileContent, "\r\n", "\n")
	}
	cleanedNew := stripTrailingWhitespace(in.NewString, path)
	if cascading := checkCascadingEdit(execCtx, path, in.OldString); cascading != "" {
		return cascading, nil
	}
	actualOld := findActualString(fileContent, in.OldString)
	if actualOld == "" && in.OldString != "" {
		return formatEditError(editStringNotFound, fmt.Sprintf("String to replace not found in file.\n  Searched for: %s\n  Hint: the file may have changed since you last read it.\n%s", truncateForMsg(in.OldString, 120), matchFailureSnippet(fileContent, in.OldString))), nil
	}
	if in.OldString == "" {
		actualOld = ""
	}
	count := countOccurrences(fileContent, in.OldString)
	if !in.ReplaceAll && count > 1 {
		return formatEditError(editMultipleMatches, fmt.Sprintf("Found %d matches of the string to replace, but replace_all is false.\n  To replace all occurrences, set replace_all=true.\n  To replace only one, include more surrounding context to make the match unique.", count)), nil
	}
	actualNew := cleanedNew
	if actualOld != in.OldString {
		actualNew = preserveQuoteStyle(in.OldString, actualOld, cleanedNew)
	}
	if actualNew == "" && !strings.HasSuffix(in.OldString, "\n") && strings.Contains(fileContent, actualOld+"\n") {
		actualOld += "\n"
	}
	newContent := strings.ReplaceAll(fileContent, actualOld, actualNew)
	if !in.ReplaceAll && actualOld != "" {
		newContent = strings.Replace(fileContent, actualOld, actualNew, 1)
	}
	recordAppliedEdit(execCtx, path, in.OldString, actualNew)
	writeContent := newContent
	if lineEndings == "\r\n" {
		writeContent = strings.ReplaceAll(writeContent, "\n", "\r\n")
	}
	if err := os.WriteFile(path, []byte(writeContent), 0o644); err != nil {
		return formatEditError(editWriteFailed, fmt.Sprintf("Error writing file: %s", err)), nil
	}
	updateReadState(execCtx, path, writeContent, detectLineEndings(writeContent), false)
	replaced := count
	if !in.ReplaceAll {
		replaced = 1
	}
	diff := generateUnifiedDiff(path, fileContent, writeContent, 3)
	result := fmt.Sprintf("Edit applied successfully: %d occurrence(s) replaced in %s", replaced, in.FilePath)
	if diff != "" {
		result += "\n---DIFF---\n" + diff
	}
	return result, nil
}

type NotebookEditInput struct {
	NotebookPath string  `json:"notebook_path" jsonschema_description:"The absolute path to the .ipynb notebook file to edit"`
	CellID       string  `json:"cell_id" jsonschema_description:"The ID of the cell to edit. When inserting a new cell, the new cell will be inserted after the cell with this ID, or at the beginning if not specified."`
	NewSource    string  `json:"new_source" jsonschema_description:"The new source for the cell"`
	CellType     *string `json:"cell_type,omitempty" jsonschema:"enum=code,enum=markdown,nullable,default=null" jsonschema_description:"The type of the cell (code or markdown). If not specified, it defaults to the current cell type. If using edit_mode=insert, this is required."`
	EditMode     string  `json:"edit_mode,omitempty" jsonschema:"enum=replace,enum=insert,enum=delete,default=replace" jsonschema_description:"The type of edit to make (replace, insert, delete). Defaults to replace."`
}

type NotebookEditTool struct{ BaseTool }

func NewNotebookEditTool() *NotebookEditTool {
	return &NotebookEditTool{BaseTool{Spec: ToolSpec{
		Name:             "notebook_edit",
		Description:      "Completely replaces the contents of a specific cell in a Jupyter notebook (.ipynb file) with new source. cell_id identifies the cell to edit. Use edit_mode=insert to add a new cell after the identified cell, or edit_mode=delete to remove the cell.",
		InputPrototype:   NotebookEditInput{},
		ReadOnly:         BoolPtr(false),
		ConcurrencySafe:  BoolPtr(false),
		Destructive:      BoolPtr(false),
		ConfirmFilePaths: true,
	}}}
}

func (t *NotebookEditTool) ValidateInput(execCtx ExecutionContext, input any) (bool, string) {
	in := deref[NotebookEditInput](input)
	path := resolveToolPath(execCtx, in.NotebookPath)
	if ok, msg := checkAllowedWriteRoots(path, execCtx["allowed_write_roots"]); !ok {
		return false, msg
	}
	info, err := os.Stat(path)
	if err != nil {
		hint := ""
		if !filepath.IsAbs(path) {
			hint = " (path is relative — use absolute paths)"
		}
		return false, "File not found: " + in.NotebookPath + hint
	}
	if !info.Mode().IsRegular() {
		return false, "Not a file: " + in.NotebookPath
	}
	if !strings.EqualFold(filepath.Ext(path), ".ipynb") {
		return false, "Not a notebook file: " + in.NotebookPath + ". notebook_edit only works with .ipynb files."
	}
	mode := notebookEditMode(in.EditMode)
	if mode == "insert" && in.CellType == nil {
		return false, "cell_type is required when edit_mode=insert. Specify 'code' or 'markdown'."
	}
	if in.CellType != nil && *in.CellType != "code" && *in.CellType != "markdown" {
		return false, "cell_type must be 'code' or 'markdown'."
	}
	if mode != "replace" && mode != "insert" && mode != "delete" {
		return false, "edit_mode must be 'replace', 'insert', or 'delete'."
	}
	return true, ""
}

func (t *NotebookEditTool) Execute(_ context.Context, execCtx ExecutionContext, input any) (string, error) {
	in := deref[NotebookEditInput](input)
	mode := notebookEditMode(in.EditMode)
	path := resolveToolPath(execCtx, in.NotebookPath)
	rawBytes, err := os.ReadFile(path)
	if err != nil {
		return "Error reading notebook: " + err.Error(), nil
	}
	raw := string(rawBytes)
	parsed, err := parseOrderedJSON(rawBytes)
	if err != nil {
		return "Failed to parse notebook as JSON: " + err.Error() + ". The file may be corrupted or not a valid .ipynb file.", nil
	}
	nb, ok := parsed.(*orderedmap.OrderedMap[string, any])
	if !ok {
		return "Notebook has no 'cells' key. The file may be corrupted or not a valid .ipynb file.", nil
	}
	rawCells, ok := nb.Get("cells")
	if !ok {
		return "Notebook has no 'cells' key. The file may be corrupted or not a valid .ipynb file.", nil
	}
	cells, ok := rawCells.([]any)
	if !ok {
		return "Notebook has no 'cells' key. The file may be corrupted or not a valid .ipynb file.", nil
	}
	targetIdx := -1
	for idx, rawCell := range cells {
		cell, _ := rawCell.(*orderedmap.OrderedMap[string, any])
		if cell == nil {
			continue
		}
		id, _ := cell.Get("id")
		if id == in.CellID {
			targetIdx = idx
			break
		}
	}
	switch mode {
	case "replace":
		if targetIdx == -1 {
			return fmt.Sprintf("Cell with id '%s' not found in notebook. The notebook has %d cells.", in.CellID, len(cells)), nil
		}
		cell, ok := cells[targetIdx].(*orderedmap.OrderedMap[string, any])
		if !ok || cell == nil {
			cell = newOrderedJSONMap()
			cells[targetIdx] = cell
		}
		cellType := "code"
		if existingRaw, ok := cell.Get("cell_type"); ok {
			existing, _ := existingRaw.(string)
			if existing != "" {
				cellType = existing
			}
		}
		if in.CellType != nil {
			cellType = *in.CellType
		}
		cell.Set("source", in.NewSource)
		cell.Set("cell_type", cellType)
	case "insert":
		cellType := "code"
		if in.CellType != nil && *in.CellType != "" {
			cellType = *in.CellType
		}
		newCell := newOrderedJSONMap()
		newCell.Set("cell_type", cellType)
		newCell.Set("metadata", newOrderedJSONMap())
		newCell.Set("source", in.NewSource)
		insertAt := 0
		if targetIdx >= 0 {
			insertAt = targetIdx + 1
		}
		cells = append(cells, nil)
		copy(cells[insertAt+1:], cells[insertAt:])
		cells[insertAt] = newCell
	case "delete":
		if targetIdx == -1 {
			return fmt.Sprintf("Cell with id '%s' not found in notebook. Cannot delete a non-existent cell.", in.CellID), nil
		}
		cells = append(cells[:targetIdx], cells[targetIdx+1:]...)
	}
	nb.Set("cells", cells)
	encoded, err := marshalNotebookJSON(nb, detectJSONIndent(raw))
	if err != nil {
		return "Error writing notebook: " + err.Error(), nil
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		return "Error writing notebook: " + err.Error(), nil
	}
	return "Notebook edit applied: " + mode + " cell in " + in.NotebookPath, nil
}

func notebookEditMode(mode string) string {
	if strings.TrimSpace(mode) == "" {
		return "replace"
	}
	return mode
}

func newOrderedJSONMap() *orderedmap.OrderedMap[string, any] {
	return orderedmap.New[string, any](orderedmap.WithDisableHTMLEscape[string, any]())
}

func parseOrderedJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := parseOrderedJSONValue(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err == nil {
		return nil, fmt.Errorf("unexpected trailing JSON data")
	} else if err != io.EOF {
		return nil, err
	}
	return value, nil
}

func parseOrderedJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	return parseOrderedJSONToken(decoder, token)
}

func parseOrderedJSONToken(decoder *json.Decoder, token any) (any, error) {
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		obj := newOrderedJSONMap()
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("expected object key")
			}
			value, err := parseOrderedJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			obj.Set(key, value)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return obj, nil
	case '[':
		var arr []any
		for decoder.More() {
			value, err := parseOrderedJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			arr = append(arr, value)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return arr, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func detectJSONIndent(raw string) int {
	lines := strings.Split(raw, "\n")
	limit := 10
	if len(lines) < limit {
		limit = len(lines)
	}
	for _, line := range lines[1:limit] {
		stripped := strings.TrimLeft(line, " \t")
		if stripped == "" || strings.HasPrefix(stripped, "{") {
			continue
		}
		indentLen := len(line) - len(stripped)
		if indentLen > 0 {
			return indentLen
		}
	}
	return 1
}

func marshalNotebookJSON(value any, indent int) ([]byte, error) {
	if indent <= 0 {
		indent = 1
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", strings.Repeat(" ", indent))
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type GrepSearchInput struct {
	Pattern         string `json:"pattern" jsonschema_description:"The regular expression pattern to search for"`
	Path            string `json:"path,omitempty" jsonschema:"default=." jsonschema_description:"File or directory to search in"`
	Glob            string `json:"glob,omitempty" jsonschema:"nullable,default=null" jsonschema_description:"Glob pattern to filter files (e.g. '*.py')"`
	HeadLimit       *int   `json:"head_limit,omitempty" jsonschema:"default=250" jsonschema_description:"Maximum number of matching lines to return"`
	OutputMode      string `json:"output_mode,omitempty" jsonschema:"enum=content,enum=files_with_matches,enum=count,default=files_with_matches" jsonschema_description:"Output mode: 'content', 'files_with_matches', or 'count'"`
	CaseInsensitive bool   `json:"case_insensitive,omitempty" jsonschema:"default=false" jsonschema_description:"Case insensitive search"`
}

type GrepSearchTool struct{ BaseTool }

func NewGrepSearchTool() *GrepSearchTool {
	return &GrepSearchTool{BaseTool{Spec: ToolSpec{
		Name:            "grep_search",
		Description:     "Search file contents using ripgrep. Returns matching lines or file paths. Supports full regex syntax, glob filtering, and output modes.",
		InputPrototype:  GrepSearchInput{},
		ReadOnly:        BoolPtr(true),
		ConcurrencySafe: BoolPtr(true),
		Destructive:     BoolPtr(false),
		MaxOutputChars:  100_000,
	}}}
}

func (t *GrepSearchTool) IsEnabled() bool {
	_, err := exec.LookPath("rg")
	return err == nil
}

func (t *GrepSearchTool) ValidateInput(execCtx ExecutionContext, input any) (bool, string) {
	in := deref[GrepSearchInput](input)
	if in.Path == "" {
		in.Path = "."
	}
	searchPath := resolveToolPath(execCtx, in.Path)
	if isBroadSearchRoot(searchPath) {
		return false, "Cannot search from a filesystem root or home directory directly. Specify a more specific path within the workspace."
	}
	if ok, msg := checkAllowedReadRoots(searchPath, execCtx["allowed_read_roots"]); !ok {
		return false, msg
	}
	if _, err := os.Stat(searchPath); err != nil {
		return false, fmt.Sprintf("Search path not found: %s", in.Path)
	}
	return true, ""
}

func (t *GrepSearchTool) Execute(ctx context.Context, execCtx ExecutionContext, input any) (string, error) {
	if _, err := exec.LookPath("rg"); err != nil {
		return "Error: ripgrep (rg) is not installed. Please install it from https://github.com/BurntSushi/ripgrep", nil
	}
	in := deref[GrepSearchInput](input)
	if in.Path == "" {
		in.Path = "."
	}
	if in.OutputMode == "" {
		in.OutputMode = "files_with_matches"
	}
	headLimit := 250
	if in.HeadLimit != nil {
		headLimit = *in.HeadLimit
	}
	searchPath := resolveToolPath(execCtx, in.Path)
	cwd := stringFromExecCtx(execCtx, "cwd")
	if cwd == "" {
		cwd = config.GetConfig().CWD
	}
	args := []string{"--no-heading", "--with-filename", "--line-number", "--color=never"}
	if in.CaseInsensitive {
		args = append(args, "-i")
	}
	if in.Glob != "" {
		args = append(args, "--glob", in.Glob)
	}
	switch in.OutputMode {
	case "files_with_matches":
		args = append(args, "-l")
	case "count":
		args = append(args, "-c")
	}
	args = append(args, in.Pattern, searchPath)
	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "rg", args...)
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if cmdCtx.Err() == context.DeadlineExceeded {
		return "Error: grep search timed out (10s limit). Try narrowing the search scope.", nil
	}
	if err != nil && stdout.Len() == 0 {
		// rg returns 1 for no matches; Python treats empty stdout as no matches.
		if stdout.Len() == 0 {
			return "No matches found.", nil
		}
	}
	output := strings.TrimSpace(strings.ToValidUTF8(stdout.String(), "\uFFFD"))
	if output == "" {
		return "No matches found.", nil
	}
	lines := strings.Split(output, "\n")
	if headLimit != 0 && len(lines) > headLimit {
		keep := headLimit
		if keep < 0 {
			keep = len(lines) + keep
		}
		if keep < 0 {
			keep = 0
		}
		if keep > len(lines) {
			keep = len(lines)
		}
		return strings.Join(lines[:keep], "\n") + fmt.Sprintf("\n\n... (%d more results truncated)", len(lines)-headLimit), nil
	}
	return output, nil
}

type GlobMatchInput struct {
	Pattern string `json:"pattern" jsonschema_description:"The glob pattern to match files against (e.g. '**/*.py')"`
	Path    string `json:"path,omitempty" jsonschema:"default=." jsonschema_description:"The directory to search in"`
}

type GlobMatchTool struct{ BaseTool }

func NewGlobMatchTool() *GlobMatchTool {
	return &GlobMatchTool{BaseTool{Spec: ToolSpec{
		Name:            "glob_match",
		Description:     "Find files matching a glob pattern. Automatically ignores .git, node_modules, __pycache__, .venv, and similar noise directories.",
		InputPrototype:  GlobMatchInput{},
		ReadOnly:        BoolPtr(true),
		ConcurrencySafe: BoolPtr(true),
		Destructive:     BoolPtr(false),
	}}}
}

func (t *GlobMatchTool) ValidateInput(execCtx ExecutionContext, input any) (bool, string) {
	in := deref[GlobMatchInput](input)
	if in.Path == "" {
		in.Path = "."
	}
	searchDir := resolveToolPath(execCtx, in.Path)
	if isBroadSearchRoot(searchDir) {
		return false, "Cannot glob from a filesystem root or home directory directly. Specify a more specific path within the workspace."
	}
	if ok, msg := checkAllowedReadRoots(searchDir, execCtx["allowed_read_roots"]); !ok {
		return false, msg
	}
	if _, err := os.Stat(searchDir); err != nil {
		return false, fmt.Sprintf("Directory not found: %s", in.Path)
	}
	return true, ""
}

func (t *GlobMatchTool) Execute(_ context.Context, execCtx ExecutionContext, input any) (string, error) {
	in := deref[GlobMatchInput](input)
	if in.Path == "" {
		in.Path = "."
	}
	root := resolveToolPath(execCtx, in.Path)
	pattern := filepath.ToSlash(filepath.Join(root, in.Pattern))
	matches, err := doublestar.FilepathGlob(pattern)
	if err != nil {
		return fmt.Sprintf("Error running glob: %s", err), nil
	}
	filtered := make([]string, 0, len(matches))
	for _, match := range matches {
		if hasIgnoredDir(match) {
			continue
		}
		filtered = append(filtered, match)
	}
	matches = filtered
	if len(matches) == 0 {
		return "No files found matching the pattern.", nil
	}
	sort.SliceStable(matches, func(i, j int) bool {
		mi := fileMTime(matches[i])
		mj := fileMTime(matches[j])
		return mi > mj
	})
	if len(matches) > 100 {
		return strings.Join(matches[:100], "\n") + fmt.Sprintf("\n\n... (%d more files truncated)", len(matches)-100), nil
	}
	return strings.Join(matches, "\n"), nil
}

type BashInput struct {
	Command                   string `json:"command" jsonschema_description:"The shell command to execute"`
	Timeout                   *int   `json:"timeout,omitempty" jsonschema:"nullable,default=null" jsonschema_description:"Optional timeout in milliseconds. Numeric strings are accepted by the runtime as seconds for benchmark compatibility. Maximum 120 seconds."`
	TimeoutSeconds            any    `json:"timeout_seconds,omitempty" jsonschema:"nullable,default=null" jsonschema_description:"Optional timeout in seconds. Maximum 120 seconds."`
	Description               string `json:"description,omitempty" jsonschema:"default=" jsonschema_description:"Brief activity description for UI display"`
	RunInBackground           bool   `json:"run_in_background,omitempty" jsonschema:"default=false" jsonschema_description:"If True, execute asynchronously and return immediately"`
	DangerouslyDisableSandbox bool   `json:"dangerouslyDisableSandbox,omitempty" jsonschema:"default=false" jsonschema_description:"If True, skip sandbox isolation (requires policy approval)"`
}

type RunShellInput = BashInput

type BashTool struct {
	BaseTool
	sandboxManager    *bashpkg.SandboxManager
	backgroundManager *bashpkg.BackgroundManager
}

type RunShellTool = BashTool

func NewBashTool() *BashTool {
	return &BashTool{BaseTool: BaseTool{Spec: ToolSpec{
		Name:              "run_shell",
		Description:       "Execute a shell command in the project environment. Output is limited to 5MB. Use run_in_background for long-running commands and dangerouslyDisableSandbox only when sandbox isolation prevents legitimate operations.",
		InputPrototype:    BashInput{},
		ReadOnly:          BoolPtr(false),
		ConcurrencySafe:   BoolPtr(false),
		Destructive:       BoolPtr(true),
		CommandClassifier: true,
		SiblingAbort:      true,
		MaxOutputChars:    100_000,
	}}, sandboxManager: bashpkg.NewSandboxManager()}
}

func (t *BashTool) DecodeInput(raw map[string]any) (any, error) {
	normalized := copyAnyMap(raw)
	if value, ok := normalized["timeout"]; ok {
		if text, isString := value.(string); isString {
			seconds, errMsg := parseBashTimeoutString(text, true)
			if errMsg != "" {
				return nil, fmt.Errorf("%s", errMsg)
			}
			millis := int(seconds * 1000)
			normalized["timeout"] = millis
		}
	}
	return t.BaseTool.DecodeInput(normalized)
}

func (t *BashTool) TimeoutForInput(input any) time.Duration {
	in := deref[BashInput](input)
	var timeoutValue any
	if in.Timeout != nil {
		timeoutValue = in.Timeout
	}
	seconds, errMsg := parseBashTimeoutSeconds(timeoutValue, in.TimeoutSeconds, config.GetConfig().ShellTimeoutSeconds)
	if errMsg != "" || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds*float64(time.Second)) + time.Second
}

func (t *BashTool) IsReadOnly(input any) bool {
	in := deref[BashInput](input)
	return bashpkg.ClassifyCommand(in.Command).CommandClass == bashpkg.CommandClassSafe
}

func (t *BashTool) IsDestructive(input any) bool {
	in := deref[BashInput](input)
	category := bashpkg.ClassifyBashCommand(in.Command)
	return category == "write" || category == "unknown"
}

func (t *BashTool) Execute(ctx context.Context, execCtx ExecutionContext, input any) (string, error) {
	in := deref[BashInput](input)
	cwd := stringFromExecCtx(execCtx, "cwd")
	if cwd == "" {
		cwd = config.GetConfig().CWD
	}
	var timeoutValue any
	if in.Timeout != nil {
		timeoutValue = in.Timeout
	}
	timeoutSeconds, timeoutErr := parseBashTimeoutSeconds(timeoutValue, in.TimeoutSeconds, config.GetConfig().ShellTimeoutSeconds)
	if timeoutErr != "" {
		return "<tool_use_error>\nInvalid timeout: " + timeoutErr + "\nUse timeout_seconds for seconds, or timeout as milliseconds/seconds string. Maximum 120 seconds.\n</tool_use_error>", nil
	}
	timeout := time.Duration(timeoutSeconds * float64(time.Second))

	secResult := bashpkg.RunAllSecurityChecks(in.Command)
	if bashpkg.IsBlocking(secResult) {
		return "<tool_use_error>\nCommand blocked by security checks:\n  " +
			firstFindingDescriptions(secResult.Findings, 3) +
			"\nThe command contains patterns that are unsafe. Use an alternative approach.\n</tool_use_error>", nil
	}

	permResult := bashpkg.AnalyzeCommandPermissions(in.Command)
	_ = permResult

	stripped := strings.TrimSpace(in.Command)
	if stripped != "" && strings.EqualFold(strings.Fields(stripped)[0], "sed") {
		sedResult := bashpkg.ValidateSedCommand(in.Command)
		if !sedResult.Safe {
			return "<tool_use_error>\nsed command not auto-approved: " + sedResult.Reason +
				"\nThe sed expression uses features that require user review. Use read_file + write_file/edit_file instead, or simplify the sed expression to use only safe patterns.\n</tool_use_error>", nil
		}
	}

	if !bashpkg.IsAbsoluteDestroy(in.Command) {
		valid, invalid := bashpkg.ValidatePaths(in.Command, cwd)
		if !valid {
			invalid = pathsOutsideAllowedRoots(invalid, execCtx)
		}
		if len(invalid) > 0 {
			return "<tool_use_error>\nPath validation failed: the command references files outside the workspace.\n  Blocked paths: " +
				strings.Join(limitStrings(invalid, 5), ", ") +
				"\n  Workspace: " + cwd +
				"\nUse paths within the project workspace, or explicitly acknowledge the out-of-workspace access.\n</tool_use_error>", nil
		}
	}

	if in.RunInBackground {
		return t.executeBackground(execCtx, in.Command, in.Description, cwd, timeout)
	}

	var exitCode int
	var output string
	if bashpkg.ShouldUseSandbox(in.Command, t.sandboxManager, in.DangerouslyDisableSandbox, nil) {
		argv := t.sandboxManager.GetSandboxCommand(in.Command, bashpkg.SandboxConfig{
			Enabled: true, AllowWrite: []string{cwd}, AllowRead: []string{cwd}, AllowNetwork: false,
		}, cwd)
		exitCode, output = runArgvCommand(ctx, argv, cwd, timeout, in.Command)
	} else {
		exitCode, output = runShellCommand(ctx, in.Command, cwd, timeout)
	}
	output = truncateBashOutput(output)
	exitInfo := bashpkg.FormatExitCode(in.Command, exitCode)
	if strings.TrimSpace(output) == "" {
		return exitInfo, nil
	}
	return exitInfo + "\n\n" + output, nil
}

type ToolSearchInput struct {
	Query string `json:"query" jsonschema:"description=Search query. Use 'select:Name1,Name2' to load specific tools, '+prefix keyword' to filter by name prefix, or just keywords to search all available deferred tools."`
}

type ToolSearchTool struct{ BaseTool }

func NewToolSearchTool() *ToolSearchTool {
	return &ToolSearchTool{BaseTool{Spec: ToolSpec{
		Name:            "tool_search",
		Description:     "Search for and load tools that are not in the default tool list. Use this when you need a specific tool that isn't available.\n\nQuery syntax:\n  \"select:Name1,Name2\" — directly load specific tools by exact name\n  \"+prefix keyword\" — filter tools whose name starts with prefix, ranked by keyword relevance\n  \"keyword1 keyword2\" — search all deferred tools by name and description\n\nAfter loading with select:, the tool becomes available for immediate use.",
		InputPrototype:  ToolSearchInput{},
		Aliases:         []string{"search_tools"},
		ReadOnly:        BoolPtr(true),
		ConcurrencySafe: BoolPtr(true),
		Destructive:     BoolPtr(false),
	}}}
}

func (t *ToolSearchTool) Execute(_ context.Context, execCtx ExecutionContext, input any) (string, error) {
	in := deref[ToolSearchInput](input)
	registry, _ := execCtx["_registry"].(*ToolRegistry)
	if registry == nil {
		return "<tool_use_error>\nTool registry not available in context.\n</tool_use_error>", nil
	}
	query := strings.TrimSpace(in.Query)
	if strings.HasPrefix(query, "select:") {
		return handleToolSearchSelect(query, registry), nil
	}
	return handleToolSearchQuery(query, registry), nil
}

func handleToolSearchSelect(query string, registry *ToolRegistry) string {
	rawNames := strings.Split(query[7:], ",")
	var names []string
	for _, name := range rawNames {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	if len(names) == 0 {
		return "No tool names specified. Use 'select:Name1,Name2' to load specific tools."
	}
	var lines, activated, notFound, alreadyActive []string
	for _, name := range names {
		if registry.Get(name) != nil {
			alreadyActive = append(alreadyActive, name)
			continue
		}
		tool := registry.ActivateTool(name)
		if tool != nil {
			activated = append(activated, name)
			schema, _ := jsonMarshalCompact(tool.ToAPISchema()["input_schema"])
			lines = append(lines, "## "+name)
			lines = append(lines, "Description: "+tool.Description())
			lines = append(lines, "Arguments: "+schema)
			lines = append(lines, "")
		} else {
			notFound = append(notFound, name)
		}
	}
	var parts []string
	if len(activated) > 0 {
		parts = append(parts, fmt.Sprintf("Activated %d tool(s): %s. They are now available for use.\n", len(activated), strings.Join(activated, ", ")))
		parts = append(parts, lines...)
	}
	if len(alreadyActive) > 0 {
		parts = append(parts, "Already active: "+strings.Join(alreadyActive, ", "))
	}
	if len(notFound) > 0 {
		var suggestions string
		candidates := registry.GetDeferredToolNames()
		if len(candidates) > 0 {
			sort.SliceStable(candidates, func(i, j int) bool {
				return toolSearchSimilarity(candidates[i], notFound[0]) > toolSearchSimilarity(candidates[j], notFound[0])
			})
			if len(candidates) > 5 {
				candidates = candidates[:5]
			}
			if len(candidates) > 0 {
				suggestions = "Available deferred tools (use select: to load): " + strings.Join(candidates, ", ")
			}
		}
		if suggestions != "" {
			parts = append(parts, fmt.Sprintf("Not found: %s. %s", strings.Join(notFound, ", "), suggestions))
		} else {
			parts = append(parts, fmt.Sprintf("Not found: %s. No deferred tools available.", strings.Join(notFound, ", ")))
		}
	}
	if len(parts) == 0 {
		return "No tools were activated."
	}
	return strings.Join(parts, "\n")
}

func handleToolSearchQuery(query string, registry *ToolRegistry) string {
	results := registry.SearchDeferred(query)
	if len(results) == 0 {
		deferred := registry.GetDeferredTools()
		if len(deferred) == 0 {
			return "No deferred tools are available. All tools are already loaded."
		}
		names := make([]string, 0, len(deferred))
		for name := range deferred {
			names = append(names, name)
		}
		sort.Strings(names)
		return fmt.Sprintf("No tools match '%s'.\n\nAvailable deferred tools: %s\n\nUse 'select:Name' to load one or more of them.", query, strings.Join(names, ", "))
	}
	lines := []string{fmt.Sprintf("Found %d tool(s) matching '%s':\n", len(results), query)}
	for _, tool := range results {
		hint := tool.SearchHint()
		hintStr := ""
		if hint != "" {
			hintStr = " [hint: " + hint + "]"
		}
		desc := strings.Split(tool.Description(), "\n")[0]
		lines = append(lines, "- **"+tool.Name()+"**"+hintStr)
		lines = append(lines, "  "+truncateForMsg(desc, 120))
	}
	lines = append(lines, "")
	lines = append(lines, "Use 'select:Name' to load the tool(s) you need.")
	return strings.Join(lines, "\n")
}

func toolSearchSimilarity(a, b string) float64 {
	a = strings.ToLower(a)
	b = strings.ToLower(b)
	if a == b {
		return 1
	}
	aRunes := []rune(a)
	bRunes := []rune(b)
	i := 0
	for i < len(aRunes) && i < len(bRunes) && aRunes[i] == bRunes[i] {
		i++
	}
	prefixScore := float64(i) / float64(max(len(aRunes), len(bRunes)))
	aSet := map[rune]struct{}{}
	bSet := map[rune]struct{}{}
	for _, r := range aRunes {
		aSet[r] = struct{}{}
	}
	for _, r := range bRunes {
		bSet[r] = struct{}{}
	}
	common := 0
	for r := range aSet {
		if _, ok := bSet[r]; ok {
			common++
		}
	}
	overlap := float64(common) / float64(max(len(aSet), len(bSet), 1))
	return prefixScore*0.6 + overlap*0.4
}

func jsonMarshalCompact(v any) (string, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(v)
	if err != nil {
		return "{}", err
	}
	return string(spacePythonJSONSeparators(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))), nil
}

func spacePythonJSONSeparators(data []byte) []byte {
	out := make([]byte, 0, len(data)+8)
	inString := false
	escaped := false
	for _, b := range data {
		out = append(out, b)
		if escaped {
			escaped = false
			continue
		}
		if inString {
			if b == '\\' {
				escaped = true
			} else if b == '"' {
				inString = false
			}
			continue
		}
		if b == '"' {
			inString = true
			continue
		}
		if b == ':' || b == ',' {
			out = append(out, ' ')
		}
	}
	return out
}

func resolveToolPath(execCtx ExecutionContext, path string) string {
	var resolved string
	if filepath.IsAbs(path) {
		resolved = filepath.Clean(path)
	} else {
		cwd := stringFromExecCtx(execCtx, "cwd")
		if cwd == "" {
			cwd = config.GetConfig().CWD
		}
		resolved = filepath.Clean(filepath.Join(cwd, path))
	}
	return resolvePathBestEffort(resolved)
}

func stringFromExecCtx(execCtx ExecutionContext, key string) string {
	if execCtx == nil {
		return ""
	}
	if s, ok := execCtx[key].(string); ok {
		return s
	}
	return ""
}

func deref[T any](input any) T {
	switch v := input.(type) {
	case T:
		return v
	case *T:
		if v != nil {
			return *v
		}
	}
	var zero T
	return zero
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func timeSeconds(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}

func decodeToolBytes(data []byte) string {
	if len(data) >= 2 && data[0] == 0xff && data[1] == 0xfe {
		u16 := make([]uint16, 0, len(data)/2)
		for i := 0; i+1 < len(data); i += 2 {
			u16 = append(u16, uint16(data[i])|uint16(data[i+1])<<8)
		}
		runes := utf16.Decode(u16)
		if len(data)%2 == 1 {
			runes = append(runes, '\ufffd')
		}
		return string(runes)
	}
	return string(bytes.ToValidUTF8(data, []byte("\ufffd")))
}

func detectLineEndings(content string) string {
	sample := content
	if len(sample) > 4096 {
		sample = sample[:4096]
	}
	crlf := strings.Count(sample, "\r\n")
	lf := strings.Count(sample, "\n") - crlf
	if crlf > lf {
		return "\r\n"
	}
	return "\n"
}

func updateReadState(execCtx ExecutionContext, path, content, lineEndings string, isPartial bool) {
	owner, _ := execCtx["parent_state"].(readFileStateOwner)
	if owner == nil {
		return
	}
	storeContent := content
	if lineEndings == "\r\n" {
		storeContent = strings.ReplaceAll(storeContent, "\r\n", "\n")
	}
	owner.SetReadFileState(path, filetool.FileStateEntry{
		Content:       storeContent,
		TimeStamp:     fileMTime(path),
		IsPartialView: isPartial,
		LineEndings:   lineEndings,
	})
}

func fileMTime(path string) float64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return float64(info.ModTime().UnixNano()) / 1e9
}

func checkAllowedReadRoots(path string, raw any) (bool, string) {
	return checkAllowedRoots("read", path, raw)
}

func checkAllowedWriteRoots(path string, raw any) (bool, string) {
	return checkAllowedRoots("write", path, raw)
}

func pathsOutsideAllowedRoots(paths []string, execCtx ExecutionContext) []string {
	if len(paths) == 0 || execCtx == nil {
		return paths
	}
	readRoots := execCtx["allowed_read_roots"]
	writeRoots := execCtx["allowed_write_roots"]
	if len(rootsFromAny(readRoots)) == 0 && len(rootsFromAny(writeRoots)) == 0 {
		return paths
	}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if ok, _ := checkAllowedReadRoots(path, readRoots); ok {
			continue
		}
		if ok, _ := checkAllowedWriteRoots(path, writeRoots); ok {
			continue
		}
		out = append(out, path)
	}
	return out
}

func checkAllowedRoots(action, path string, raw any) (bool, string) {
	roots := rootsFromAny(raw)
	if len(roots) == 0 {
		return true, ""
	}
	resolved := resolvePathBestEffort(path)
	for _, root := range roots {
		absRoot := resolvePathBestEffort(root)
		rel, err := filepath.Rel(absRoot, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true, ""
		}
	}
	if action == "read" {
		return false, fmt.Sprintf("Cannot read %s: outside allowed read roots (%s).", path, strings.Join(roots, ", "))
	}
	return false, fmt.Sprintf("Cannot write to %s: outside allowed roots (%s).", path, strings.Join(roots, ", "))
}

func rootsFromAny(raw any) []string {
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func resolvePathBestEffort(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	cleaned := filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return filepath.Clean(resolved)
	}
	current := cleaned
	var suffix []string
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			parts := append([]string{resolved}, suffix...)
			return filepath.Clean(filepath.Join(parts...))
		}
		parent := filepath.Dir(current)
		if parent == current {
			return cleaned
		}
		suffix = append([]string{filepath.Base(current)}, suffix...)
		current = parent
	}
}

func isBroadSearchRoot(path string) bool {
	resolved := resolvePathBestEffort(path)
	home, _ := os.UserHomeDir()
	volume := filepath.VolumeName(resolved)
	root := string(filepath.Separator)
	if volume != "" {
		root = volume + string(filepath.Separator)
	}
	parent := filepath.Dir(resolved)
	return filepath.Clean(resolved) == filepath.Clean(home) || filepath.Clean(resolved) == filepath.Clean(root) || parent == resolved
}

var ignoredGlobDirs = map[string]struct{}{
	".git": {}, "node_modules": {}, "__pycache__": {}, ".venv": {}, "venv": {}, ".tox": {}, ".mypy_cache": {}, ".pytest_cache": {},
}

func hasIgnoredDir(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if _, ok := ignoredGlobDirs[part]; ok {
			return true
		}
	}
	return false
}

type editErrorCode struct {
	value int
	name  string
}

type EditErrorCode = editErrorCode

var (
	EditOK                 = editErrorCode{0, "OK"}
	EditNoOp               = editErrorCode{1, "NO_OP"}
	EditPermissionDenied   = editErrorCode{2, "PERMISSION_DENIED"}
	EditEmptyOldOnExisting = editErrorCode{3, "EMPTY_OLD_ON_EXISTING"}
	EditFileNotFound       = editErrorCode{4, "FILE_NOT_FOUND"}
	EditNotebookRedirect   = editErrorCode{5, "NOTEBOOK_REDIRECT"}
	EditUnreadFile         = editErrorCode{6, "UNREAD_FILE"}
	EditStaleRead          = editErrorCode{7, "STALE_READ"}
	EditStringNotFound     = editErrorCode{8, "STRING_NOT_FOUND"}
	EditMultipleMatches    = editErrorCode{9, "MULTIPLE_MATCHES"}
	EditFileTooLarge       = editErrorCode{10, "FILE_TOO_LARGE"}
	EditWriteFailed        = editErrorCode{11, "WRITE_FAILED"}
	EditReadFailed         = editErrorCode{12, "READ_FAILED"}
	EditCascadingEdit      = editErrorCode{13, "CASCADING_EDIT"}

	editNoOp             = editErrorCode{1, "NO_OP"}
	editFileNotFound     = editErrorCode{4, "FILE_NOT_FOUND"}
	editNotebookRedirect = editErrorCode{5, "NOTEBOOK_REDIRECT"}
	editUnreadFile       = editErrorCode{6, "UNREAD_FILE"}
	editStaleRead        = editErrorCode{7, "STALE_READ"}
	editStringNotFound   = editErrorCode{8, "STRING_NOT_FOUND"}
	editMultipleMatches  = editErrorCode{9, "MULTIPLE_MATCHES"}
	editWriteFailed      = editErrorCode{11, "WRITE_FAILED"}
	editReadFailed       = editErrorCode{12, "READ_FAILED"}
	editCascadingEdit    = editErrorCode{13, "CASCADING_EDIT"}
)

func (c editErrorCode) Value() int { return c.value }

func (c editErrorCode) Name() string { return c.name }

func formatEditError(code editErrorCode, detail string) string {
	if detail != "" {
		return fmt.Sprintf("[ErrCode %d] %s: %s", code.value, code.name, detail)
	}
	return fmt.Sprintf("[ErrCode %d] %s", code.value, code.name)
}

var curlyToStraight = map[rune]rune{'“': '"', '”': '"', '‘': '\'', '’': '\''}

func normalizeQuotes(text string) string {
	return strings.Map(func(r rune) rune {
		if repl, ok := curlyToStraight[r]; ok {
			return repl
		}
		return r
	}, text)
}

func stripTrailingWhitespace(text, filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	if ext == ".md" || ext == ".mdx" {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRightFunc(line, unicode.IsSpace)
	}
	return strings.Join(lines, "\n")
}

func findActualString(fileContent, searchString string) string {
	if searchString == "" {
		return ""
	}
	if strings.Contains(fileContent, searchString) {
		return searchString
	}
	normalizedSearch := normalizeQuotes(searchString)
	normalizedFile := normalizeQuotes(fileContent)
	idx := strings.Index(normalizedFile, normalizedSearch)
	if idx == -1 {
		return ""
	}
	runes := []rune(fileContent)
	searchRunes := []rune(searchString)
	runeIdx := len([]rune(normalizedFile[:idx]))
	if runeIdx+len(searchRunes) > len(runes) {
		return ""
	}
	return string(runes[runeIdx : runeIdx+len(searchRunes)])
}

func countOccurrences(fileContent, searchString string) int {
	if searchString == "" {
		return strings.Count(fileContent, searchString)
	}
	actual := findActualString(fileContent, searchString)
	if actual == "" {
		return 0
	}
	return strings.Count(normalizeQuotes(fileContent), normalizeQuotes(searchString))
}

func matchFailureSnippet(fileContent, oldString string) string {
	oldFirst := strings.TrimSpace(strings.Split(oldString, "\n")[0])
	if oldFirst == "" {
		return ""
	}
	lines := strings.Split(fileContent, "\n")
	bestIdx, bestScore := -1, 0.0
	for i, line := range lines {
		score := lineSimilarity(oldFirst, strings.TrimSpace(line))
		if score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	if bestScore < 0.3 {
		return fmt.Sprintf("  No similar lines found. Searched for: '%s'", truncateForMsg(oldFirst, 100))
	}
	start := max(0, bestIdx-2)
	end := min(len(lines), bestIdx+2)
	out := []string{fmt.Sprintf("  Closest match at line %d (%.0f%% similarity):", bestIdx+1, bestScore*100)}
	for i := start; i < end; i++ {
		marker := "   "
		if i == bestIdx {
			marker = ">>>"
		}
		out = append(out, fmt.Sprintf("  %s L%4d  %s", marker, i+1, truncateForMsg(lines[i], 120)))
	}
	return strings.Join(out, "\n")
}

func lineSimilarity(a, b string) float64 {
	if a == b {
		return 1
	}
	aTokens := strings.Fields(a)
	if len(aTokens) == 0 {
		return 0
	}
	bSet := map[string]struct{}{}
	for _, tok := range strings.Fields(b) {
		bSet[tok] = struct{}{}
	}
	intersect := 0
	for _, tok := range aTokens {
		if _, ok := bSet[tok]; ok {
			intersect++
		}
	}
	denom := max(len(aTokens), len(bSet))
	if denom == 0 {
		return 0
	}
	return float64(intersect) / float64(denom)
}

func preserveQuoteStyle(originalSearch, actualFromFile, newString string) string {
	quoteMap := map[rune]rune{}
	orig := []rune(originalSearch)
	actual := []rune(actualFromFile)
	for i := 0; i < len(orig) && i < len(actual); i++ {
		if orig[i] != actual[i] && (orig[i] == '"' || orig[i] == '\'') {
			if actual[i] == '“' || actual[i] == '”' || actual[i] == '‘' || actual[i] == '’' {
				quoteMap[orig[i]] = actual[i]
			}
		}
	}
	if len(quoteMap) == 0 {
		return newString
	}
	return strings.Map(func(r rune) rune {
		if repl, ok := quoteMap[r]; ok {
			return repl
		}
		return r
	}, newString)
}

func checkCascadingEdit(execCtx ExecutionContext, filePath, oldString string) string {
	raw, ok := execCtx["_applied_edits"].(map[string][]map[string]string)
	if !ok {
		return ""
	}
	oldTrimmed := strings.TrimRight(oldString, "\n")
	if oldTrimmed == "" {
		return ""
	}
	for _, prev := range raw[filePath] {
		if strings.Contains(prev["new_string"], oldTrimmed) {
			return formatEditError(editCascadingEdit, "Cascading edit detected: old_string is a substring of a new_string from a previous edit on this file. Re-read the file and adjust the edit to target the original content.")
		}
	}
	return ""
}

func recordAppliedEdit(execCtx ExecutionContext, filePath, oldString, newString string) {
	raw, ok := execCtx["_applied_edits"].(map[string][]map[string]string)
	if !ok {
		raw = map[string][]map[string]string{}
		execCtx["_applied_edits"] = raw
	}
	raw[filePath] = append(raw[filePath], map[string]string{"old_string": oldString, "new_string": newString})
}

func generateUnifiedDiff(path, oldContent, newContent string, contextLines int) string {
	if oldContent == newContent {
		return ""
	}
	diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(oldContent),
		B:        difflib.SplitLines(newContent),
		FromFile: "a/" + path,
		ToFile:   "b/" + path,
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
	if len(lines) > 34 {
		lines = append(lines[:34], "  ... (diff truncated)")
	}
	return strings.Join(lines, "\n")
}

func ComputeEditDiffStat(oldContent, newContent string) (int, int) {
	oldCount := editLineCount(oldContent)
	newCount := editLineCount(newContent)
	if oldCount != newCount {
		return max(0, oldCount-newCount), max(0, newCount-oldCount)
	}
	return oldCount, newCount
}

func editLineCount(content string) int {
	if content == "" {
		return 0
	}
	count := strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") {
		count++
	}
	return count
}

func (t *BashTool) executeBackground(execCtx ExecutionContext, command, description, cwd string, timeout time.Duration) (string, error) {
	if t.backgroundManager == nil {
		if cwd == "" {
			cwd = "."
		}
		runtimeDir := projectRuntimeDirFromContext(execCtx, cwd)
		manager, err := bashpkg.NewBackgroundManager(filepath.Join(runtimeDir, "background"))
		if err != nil {
			return "Could not start background task: " + err.Error(), nil
		}
		t.backgroundManager = manager
	}
	bgTimeout := timeout
	var timeoutPtr *time.Duration
	if bgTimeout >= 120*time.Second {
		timeoutPtr = nil
	} else {
		timeoutPtr = &bgTimeout
	}
	task, err := t.backgroundManager.StartBackgroundWithOptionalTimeout(command, description, cwd, timeoutPtr)
	if err != nil {
		return fmt.Sprintf("Could not start background task: %s\nCurrent active tasks: %d", err, t.backgroundManager.ActiveCount()), nil
	}
	display := description
	if display == "" {
		display = command
		display = firstChars(display, 80)
	}
	return fmt.Sprintf(
		"Command launched in background.\nTask ID: %s\nDescription: %s\n\nOutput file: %s\nError file:  %s\n\nUse read_file to check the output file for results.",
		task.TaskID, display, task.OutputPath, task.ErrorPath,
	), nil
}

func projectRuntimeDirFromContext(execCtx ExecutionContext, cwd string) string {
	if execCtx != nil {
		if runtimeDir, ok := execCtx["runtime_dir"].(string); ok && strings.TrimSpace(runtimeDir) != "" {
			return runtimeDir
		}
	}
	return config.ProjectRuntimeDir(cwd)
}

func runShellCommand(ctx context.Context, command, cwd string, timeout time.Duration) (int, string) {
	return runArgvCommand(ctx, bashpkg.ShellArgv(command, ""), cwd, timeout, command)
}

func runArgvCommand(ctx context.Context, argv []string, cwd string, timeout time.Duration, originalCommand string) (int, string) {
	if len(argv) == 0 {
		return -1, "Error executing command: empty argv"
	}
	if timeout <= 0 {
		return -1, fmt.Sprintf("Error: Command timed out after %ss.\n\nCommand: %s", formatPythonFloatSeconds(timeout), originalCommand)
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	bashpkg.PrepareProcessGroup(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Start()
	if err != nil {
		return -1, "Error executing command: " + err.Error()
	}
	err = cmd.Wait()
	if cmdCtx.Err() == context.DeadlineExceeded {
		bashpkg.TerminateProcessTree(cmd)
		return -1, fmt.Sprintf("Error: Command timed out after %ss.\n\nCommand: %s", formatPythonFloatSeconds(timeout), originalCommand)
	}
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	} else if err != nil {
		exitCode = -1
	}
	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += "[stderr]\n" + stderr.String()
	}
	return exitCode, output
}

func truncateBashOutput(output string) string {
	const maxOutputBytes = 5 * 1024 * 1024
	data := []byte(output)
	if len(data) <= maxOutputBytes {
		return output
	}
	half := maxOutputBytes / 2
	start := string(bytes.ToValidUTF8(data[:half], []byte("\ufffd")))
	end := string(bytes.ToValidUTF8(data[len(data)-half:], []byte("\ufffd")))
	removed := len(data) - maxOutputBytes
	return fmt.Sprintf("%s\n\n... [OUTPUT TRUNCATED: %d bytes removed] ...\n\n%s", start, removed, end)
}

func ParseBashTimeoutSecondsForTest(timeout any, timeoutSeconds any, defaultSeconds float64) (float64, string) {
	return parseBashTimeoutSeconds(timeout, timeoutSeconds, defaultSeconds)
}

func parseBashTimeoutSeconds(timeout any, timeoutSeconds any, defaultSeconds float64) (float64, string) {
	if defaultSeconds <= 0 {
		defaultSeconds = 30.0
	}
	seconds := defaultSeconds
	var errMsg string
	if timeoutSeconds != nil {
		seconds, errMsg = parseBashTimeoutValue(timeoutSeconds, false)
	} else if timeout != nil {
		seconds, errMsg = parseBashTimeoutValue(timeout, true)
	}
	if errMsg != "" {
		return 0, errMsg
	}
	if seconds > 120 {
		seconds = 120
	}
	return seconds, ""
}

func copyAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func parseBashTimeoutValue(value any, legacyTimeoutField bool) (float64, string) {
	switch v := value.(type) {
	case nil:
		return 0, ""
	case *int:
		if v == nil {
			return 0, ""
		}
		return numericBashTimeout(float64(*v), legacyTimeoutField), validatePositiveTimeout(float64(*v))
	case int:
		return numericBashTimeout(float64(v), legacyTimeoutField), validatePositiveTimeout(float64(v))
	case int64:
		return numericBashTimeout(float64(v), legacyTimeoutField), validatePositiveTimeout(float64(v))
	case float64:
		return numericBashTimeout(v, legacyTimeoutField), validatePositiveTimeout(v)
	case float32:
		f := float64(v)
		return numericBashTimeout(f, legacyTimeoutField), validatePositiveTimeout(f)
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, "numeric timeout could not be parsed"
		}
		return numericBashTimeout(f, legacyTimeoutField), validatePositiveTimeout(f)
	case string:
		return parseBashTimeoutString(v, legacyTimeoutField)
	default:
		return 0, fmt.Sprintf("timeout must be numeric or a numeric string, got %T", value)
	}
}

func numericBashTimeout(value float64, legacyTimeoutField bool) float64 {
	if legacyTimeoutField {
		return value / 1000.0
	}
	return value
}

func validatePositiveTimeout(value float64) string {
	if value < 0 {
		return "timeout must be non-negative"
	}
	return ""
}

func parseBashTimeoutString(raw string, legacyTimeoutField bool) (float64, string) {
	text := strings.ToLower(strings.TrimSpace(raw))
	if text == "" {
		return 0, "timeout string is empty"
	}
	unit := ""
	switch {
	case strings.HasSuffix(text, "ms"):
		unit = "ms"
		text = strings.TrimSpace(strings.TrimSuffix(text, "ms"))
	case strings.HasSuffix(text, "s"):
		unit = "s"
		text = strings.TrimSpace(strings.TrimSuffix(text, "s"))
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, "timeout string must be numeric, optionally ending in s or ms"
	}
	if msg := validatePositiveTimeout(value); msg != "" {
		return 0, msg
	}
	if unit == "ms" {
		return value / 1000.0, ""
	}
	if unit == "s" {
		return value, ""
	}
	if legacyTimeoutField {
		return value, ""
	}
	return value, ""
}

func firstFindingDescriptions(findings []bashpkg.SecurityFinding, limit int) string {
	if limit > len(findings) {
		limit = len(findings)
	}
	parts := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		parts = append(parts, findings[i].Description)
	}
	return strings.Join(parts, "; ")
}

func limitStrings(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	return values[:limit]
}

func formatPythonFloatSeconds(timeout time.Duration) string {
	text := strconv.FormatFloat(timeout.Seconds(), 'f', -1, 64)
	if !strings.ContainsAny(text, ".eE") {
		text += ".0"
	}
	return text
}

func truncateForMsg(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	if limit <= 0 {
		return ""
	}
	return string(runes[:limit])
}

func truthy(v any) bool {
	b, ok := v.(bool)
	return ok && b
}
