package test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"LuminaCode/agent"
	coretools "LuminaCode/tools"

	"github.com/bmatcuk/doublestar/v4"
)

func TestEditFileRequiresFullReadAndPreservesCRLF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(path, []byte("hello\r\nworld\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry(coretools.NewReadFileTool(), coretools.NewEditFileTool())
	state := agent.NewAgentState()
	ctx := coretools.ExecutionContext{"cwd": dir, "parent_state": &state}

	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "edit-1", Name: "edit_file",
		Input: map[string]any{"file_path": path, "old_string": "hello", "new_string": "hi"},
	}, ctx)
	if !result.IsError || !strings.Contains(result.Content, "[ErrCode 6] UNREAD_FILE") {
		t.Fatalf("expected unread-file error, got error=%v content=%q", result.IsError, result.Content)
	}

	read := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "read-1", Name: "read_file",
		Input: map[string]any{"file_path": path},
	}, ctx)
	if read.IsError {
		t.Fatalf("read failed: %s", read.Content)
	}
	result = registry.Execute(context.Background(), coretools.ToolCall{
		ID: "edit-2", Name: "edit_file",
		Input: map[string]any{"file_path": path, "old_string": "hello", "new_string": "hi"},
	}, ctx)
	if result.IsError {
		t.Fatalf("edit failed: %s", result.Content)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hi\r\nworld\r\n") {
		t.Fatalf("expected CRLF-preserving edit, got %q", string(data))
	}
}

func TestReadFileLimitZeroAndNegativeMatchPython(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry(coretools.NewReadFileTool())
	zero := 0
	zeroResult := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "read-zero", Name: "read_file",
		Input: map[string]any{"file_path": path, "limit": zero},
	}, coretools.ExecutionContext{"cwd": dir})
	if zeroResult.IsError {
		t.Fatalf("limit=0 read failed: %s", zeroResult.Content)
	}
	if zeroResult.Content != "1\talpha\n2\tbeta\n3\tgamma" {
		t.Fatalf("limit=0 should read the full file like Python, got %q", zeroResult.Content)
	}
	negative := -1
	negativeResult := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "read-negative", Name: "read_file",
		Input: map[string]any{"file_path": path, "limit": negative},
	}, coretools.ExecutionContext{"cwd": dir})
	if negativeResult.IsError {
		t.Fatalf("limit=-1 read failed: %s", negativeResult.Content)
	}
	if negativeResult.Content != "" {
		t.Fatalf("limit=-1 should produce an empty result like Python, got %q", negativeResult.Content)
	}
}

func TestWriteFileCountsUnicodeCharactersLikePython(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unicode.txt")
	registry := coretools.NewToolRegistry(coretools.NewWriteFileTool())
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "write-unicode", Name: "write_file",
		Input: map[string]any{"file_path": path, "content": "你好"},
	}, coretools.ExecutionContext{"cwd": dir})
	if result.IsError {
		t.Fatalf("write failed: %s", result.Content)
	}
	if !strings.Contains(result.Content, "(2 characters)") {
		t.Fatalf("write_file should report Python character count, got %q", result.Content)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "你好" {
		t.Fatalf("unexpected written content: %q", data)
	}
}

func TestReadFileUTF16LEBOMAndOddByteMatchPythonDecode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "utf16.txt")
	data := []byte{0xff, 0xfe, 'A', 0x00, 0xff}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry(coretools.NewReadFileTool())
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "read-utf16", Name: "read_file",
		Input: map[string]any{"file_path": path},
	}, coretools.ExecutionContext{"cwd": dir})
	if result.IsError {
		t.Fatalf("read failed: %s", result.Content)
	}
	if result.Content != "1\t\ufeffA\ufffd" {
		t.Fatalf("expected Python utf-16-le decode with BOM and replacement, got %q", result.Content)
	}
}

func TestEditFileDetectsStaleRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mtime/content stale behavior differs on Windows like Python")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry(coretools.NewReadFileTool(), coretools.NewEditFileTool())
	state := agent.NewAgentState()
	ctx := coretools.ExecutionContext{"cwd": dir, "parent_state": &state}
	read := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "read-1", Name: "read_file",
		Input: map[string]any{"file_path": path},
	}, ctx)
	if read.IsError {
		t.Fatalf("read failed: %s", read.Content)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "edit-1", Name: "edit_file",
		Input: map[string]any{"file_path": path, "old_string": "alpha", "new_string": "omega"},
	}, ctx)
	if !result.IsError || !strings.Contains(result.Content, "[ErrCode 7] STALE_READ") {
		t.Fatalf("expected stale-read error, got error=%v content=%q", result.IsError, result.Content)
	}
}

func TestEditFileReturnsUnifiedDiffLikePython(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry(coretools.NewReadFileTool(), coretools.NewEditFileTool())
	state := agent.NewAgentState()
	ctx := coretools.ExecutionContext{"cwd": dir, "parent_state": &state}
	read := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "read-diff", Name: "read_file", Input: map[string]any{"file_path": path},
	}, ctx)
	if read.IsError {
		t.Fatalf("read failed: %s", read.Content)
	}
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "edit-diff", Name: "edit_file", Input: map[string]any{"file_path": path, "old_string": "beta", "new_string": "BETA"},
	}, ctx)
	if result.IsError {
		t.Fatalf("edit failed: %s", result.Content)
	}
	for _, want := range []string{"---DIFF---\n--- a/", "\n+++ b/", "@@", "-beta", "+BETA"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("expected unified diff fragment %q in result:\n%s", want, result.Content)
		}
	}
	if strings.Contains(result.Content, "- alpha") && strings.Contains(result.Content, "+ alpha") {
		t.Fatalf("diff appears to be whole-file non-unified output, got:\n%s", result.Content)
	}
}

func TestComputeEditDiffStatMatchesPython(t *testing.T) {
	cases := []struct {
		oldContent string
		newContent string
		removed    int
		added      int
	}{
		{"", "", 0, 0},
		{"a\n", "a\nb\n", 0, 1},
		{"a\nb\n", "a\n", 1, 0},
		{"a\nb", "x\ny", 2, 2},
		{"a", "", 1, 0},
		{"", "a", 0, 1},
	}
	for _, tc := range cases {
		removed, added := coretools.ComputeEditDiffStat(tc.oldContent, tc.newContent)
		if removed != tc.removed || added != tc.added {
			t.Fatalf("ComputeEditDiffStat(%q,%q)=(%d,%d) want (%d,%d)", tc.oldContent, tc.newContent, removed, added, tc.removed, tc.added)
		}
	}
}

func TestEditFileEmptyOldStringOnExistingContentMatchesPython(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry(coretools.NewEditFileTool())
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID:   "edit-empty-old",
		Name: "edit_file",
		Input: map[string]any{
			"file_path":  path,
			"old_string": "",
			"new_string": "prefix\n",
		},
	}, coretools.ExecutionContext{"cwd": dir})
	if !strings.Contains(result.Content, "[ErrCode 9] MULTIPLE_MATCHES") {
		t.Fatalf("expected empty old_string on existing content to be rejected like Python, got error=%v content=%q", result.IsError, result.Content)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "alpha\n" {
		t.Fatalf("file should not have been modified, got %q", string(data))
	}
}

func TestEditFileQuoteNormalizationAndStylePreservation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quotes.txt")
	if err := os.WriteFile(path, []byte("title = “old value”\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry(coretools.NewReadFileTool(), coretools.NewEditFileTool())
	state := agent.NewAgentState()
	ctx := coretools.ExecutionContext{"cwd": dir, "parent_state": &state}
	read := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "read-quotes", Name: "read_file", Input: map[string]any{"file_path": path},
	}, ctx)
	if read.IsError {
		t.Fatalf("read failed: %s", read.Content)
	}
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "edit-quotes", Name: "edit_file",
		Input: map[string]any{
			"file_path":  path,
			"old_string": `title = "old value"`,
			"new_string": `title = "new value"`,
		},
	}, ctx)
	if result.IsError {
		t.Fatalf("edit failed: %s", result.Content)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "title = ”new value”\n" {
		t.Fatalf("expected quote style to be preserved, got %q", string(data))
	}
}

func TestEditFileQuoteNormalizationAfterUnicodePrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quotes-prefix.txt")
	if err := os.WriteFile(path, []byte("前缀 title = “old value”\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry(coretools.NewReadFileTool(), coretools.NewEditFileTool())
	state := agent.NewAgentState()
	ctx := coretools.ExecutionContext{"cwd": dir, "parent_state": &state}
	read := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "read-quotes-prefix", Name: "read_file", Input: map[string]any{"file_path": path},
	}, ctx)
	if read.IsError {
		t.Fatalf("read failed: %s", read.Content)
	}
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "edit-quotes-prefix", Name: "edit_file",
		Input: map[string]any{
			"file_path":  path,
			"old_string": `title = "old value"`,
			"new_string": `title = "new value"`,
		},
	}, ctx)
	if result.IsError {
		t.Fatalf("edit failed after unicode prefix: %s", result.Content)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "前缀 title = ”new value”\n" {
		t.Fatalf("expected quote-normalized match to use Python character index, got %q", string(data))
	}
}

func TestEditFileTrailingWhitespacePolicyMatchesPython(t *testing.T) {
	dir := t.TempDir()
	txtPath := filepath.Join(dir, "plain.txt")
	mdPath := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(txtPath, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mdPath, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry(coretools.NewReadFileTool(), coretools.NewEditFileTool())
	state := agent.NewAgentState()
	ctx := coretools.ExecutionContext{"cwd": dir, "parent_state": &state}
	for _, path := range []string{txtPath, mdPath} {
		read := registry.Execute(context.Background(), coretools.ToolCall{
			ID: "read-" + filepath.Base(path), Name: "read_file", Input: map[string]any{"file_path": path},
		}, ctx)
		if read.IsError {
			t.Fatalf("read failed: %s", read.Content)
		}
	}
	txt := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "edit-txt", Name: "edit_file",
		Input: map[string]any{"file_path": txtPath, "old_string": "old", "new_string": "new  \nline\t\n"},
	}, ctx)
	if txt.IsError {
		t.Fatalf("txt edit failed: %s", txt.Content)
	}
	md := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "edit-md", Name: "edit_file",
		Input: map[string]any{"file_path": mdPath, "old_string": "old", "new_string": "new  \nline\t\n"},
	}, ctx)
	if md.IsError {
		t.Fatalf("md edit failed: %s", md.Content)
	}
	txtData, err := os.ReadFile(txtPath)
	if err != nil {
		t.Fatal(err)
	}
	mdData, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(txtData) != "new\nline\n\n" {
		t.Fatalf("non-Markdown trailing whitespace should be stripped, got %q", string(txtData))
	}
	if string(mdData) != "new  \nline\t\n\n" {
		t.Fatalf("Markdown trailing whitespace should be preserved, got %q", string(mdData))
	}
}

func TestEditFileStripsUnicodeTrailingWhitespaceLikePython(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unicode-space.txt")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry(coretools.NewReadFileTool(), coretools.NewEditFileTool())
	state := agent.NewAgentState()
	ctx := coretools.ExecutionContext{"cwd": dir, "parent_state": &state}
	read := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "read-unicode-space", Name: "read_file", Input: map[string]any{"file_path": path},
	}, ctx)
	if read.IsError {
		t.Fatalf("read failed: %s", read.Content)
	}
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "edit-unicode-space", Name: "edit_file",
		Input: map[string]any{"file_path": path, "old_string": "old", "new_string": "new\u00a0\n"},
	}, ctx)
	if result.IsError {
		t.Fatalf("edit failed: %s", result.Content)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new\n\n" {
		t.Fatalf("non-Markdown Python rstrip should remove Unicode trailing whitespace, got %q", string(data))
	}
}

func TestEditFileDeleteLineHeuristicAndFailureSnippetMatchPython(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lines.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry(coretools.NewReadFileTool(), coretools.NewEditFileTool())
	state := agent.NewAgentState()
	ctx := coretools.ExecutionContext{"cwd": dir, "parent_state": &state}
	read := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "read-lines", Name: "read_file", Input: map[string]any{"file_path": path},
	}, ctx)
	if read.IsError {
		t.Fatalf("read failed: %s", read.Content)
	}
	remove := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "remove-line", Name: "edit_file",
		Input: map[string]any{"file_path": path, "old_string": "beta", "new_string": ""},
	}, ctx)
	if remove.IsError {
		t.Fatalf("delete-line edit failed: %s", remove.Content)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "alpha\ngamma\n" {
		t.Fatalf("delete-line heuristic should consume the following newline, got %q", string(data))
	}
	miss := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "missing-line", Name: "edit_file",
		Input: map[string]any{"file_path": path, "old_string": "alpha gamma", "new_string": "x"},
	}, ctx)
	if miss.IsError || !strings.Contains(miss.Content, "[ErrCode 8] STRING_NOT_FOUND") ||
		!strings.Contains(miss.Content, "Closest match at line 1") ||
		!strings.Contains(miss.Content, ">>> L   1  alpha") {
		t.Fatalf("expected Python-style failure snippet, error=%v content=%q", miss.IsError, miss.Content)
	}
	unicodeOld := strings.Repeat("界", 121)
	unicodeMiss := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "missing-unicode", Name: "edit_file",
		Input: map[string]any{"file_path": path, "old_string": unicodeOld, "new_string": "x"},
	}, ctx)
	if unicodeMiss.IsError ||
		!strings.Contains(unicodeMiss.Content, "Searched for: "+strings.Repeat("界", 120)) ||
		strings.Contains(unicodeMiss.Content, strings.Repeat("界", 121)) {
		t.Fatalf("edit miss should truncate old_string by Python characters, error=%v content=%q", unicodeMiss.IsError, unicodeMiss.Content)
	}
}

func TestReadFileLimitZeroReturnsFullContentLikePython(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry(coretools.NewReadFileTool())
	state := agent.NewAgentState()
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "read-zero", Name: "read_file", Input: map[string]any{"file_path": path, "limit": 0},
	}, coretools.ExecutionContext{"cwd": dir, "parent_state": &state})
	if result.IsError {
		t.Fatalf("read failed: %s", result.Content)
	}
	if !strings.Contains(result.Content, "1\ta") || !strings.Contains(result.Content, "4\t") {
		t.Fatalf("limit=0 should behave as unlimited like Python, got %q", result.Content)
	}
	if len(state.ReadFileState) != 1 {
		t.Fatalf("expected one resolved read-file state entry, got %#v", state.ReadFileState)
	}
	for _, entry := range state.ReadFileState {
		if !entry.IsPartialView {
			t.Fatalf("limit=0 still records a partial-view read in Python, entry=%#v", entry)
		}
	}
}

func TestAllowedRootsResolveSymlinksLikePython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	dir := t.TempDir()
	allowed := filepath.Join(dir, "allowed")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(allowed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(allowed, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	registry := coretools.NewToolRegistry(coretools.NewReadFileTool(), coretools.NewWriteFileTool())
	readResult := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "read-link", Name: "read_file", Input: map[string]any{"file_path": filepath.Join(link, "secret.txt")},
	}, coretools.ExecutionContext{"cwd": dir, "allowed_read_roots": []string{allowed}})
	if !readResult.IsError || !strings.Contains(readResult.Content, "outside allowed read roots") {
		t.Fatalf("expected symlink read escape to be blocked, error=%v content=%q", readResult.IsError, readResult.Content)
	}
	writeResult := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "write-link", Name: "write_file", Input: map[string]any{"file_path": filepath.Join(link, "escape.txt"), "content": "x"},
	}, coretools.ExecutionContext{"cwd": dir, "allowed_write_roots": []string{allowed}})
	if !writeResult.IsError || !strings.Contains(writeResult.Content, "outside allowed roots") {
		t.Fatalf("expected symlink write escape to be blocked, error=%v content=%q", writeResult.IsError, writeResult.Content)
	}
}

func TestNotebookEditReplaceInsertDelete(t *testing.T) {
	dir := t.TempDir()
	notebook := filepath.Join(dir, "demo.ipynb")
	raw := `{
 "cells": [
  {
   "cell_type": "code",
   "id": "cell-1",
   "metadata": {},
   "source": "print('old')"
  },
  {
   "cell_type": "markdown",
   "id": "cell-2",
   "metadata": {},
   "source": "old text"
  }
 ],
 "metadata": {},
 "nbformat": 4,
 "nbformat_minor": 5
}
`
	if err := os.WriteFile(notebook, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry(coretools.NewEditFileTool(), coretools.NewNotebookEditTool())
	ctx := coretools.ExecutionContext{"cwd": dir, "allowed_write_roots": []string{dir}}
	redirect := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "edit-ipynb", Name: "edit_file",
		Input: map[string]any{"file_path": notebook, "old_string": "old", "new_string": "new"},
	}, ctx)
	if !redirect.IsError || !strings.Contains(redirect.Content, "NOTEBOOK_REDIRECT") {
		t.Fatalf("expected notebook redirect, got error=%v content=%q", redirect.IsError, redirect.Content)
	}
	replace := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "nb-1", Name: "notebook_edit",
		Input: map[string]any{"notebook_path": notebook, "cell_id": "cell-1", "new_source": "print('new')"},
	}, ctx)
	if replace.IsError || !strings.Contains(replace.Content, "replace cell") {
		t.Fatalf("replace failed: error=%v content=%q", replace.IsError, replace.Content)
	}
	insert := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "nb-2", Name: "notebook_edit",
		Input: map[string]any{"notebook_path": notebook, "cell_id": "cell-1", "new_source": "inserted", "cell_type": "markdown", "edit_mode": "insert"},
	}, ctx)
	if insert.IsError || !strings.Contains(insert.Content, "insert cell") {
		t.Fatalf("insert failed: error=%v content=%q", insert.IsError, insert.Content)
	}
	del := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "nb-3", Name: "notebook_edit",
		Input: map[string]any{"notebook_path": notebook, "cell_id": "cell-2", "new_source": "", "edit_mode": "delete"},
	}, ctx)
	if del.IsError || !strings.Contains(del.Content, "delete cell") {
		t.Fatalf("delete failed: error=%v content=%q", del.IsError, del.Content)
	}
	data, err := os.ReadFile(notebook)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("notebook was not valid JSON: %v\n%s", err, data)
	}
	cells := parsed["cells"].([]any)
	if len(cells) != 2 {
		t.Fatalf("expected two cells after insert+delete, got %#v", cells)
	}
	first := cells[0].(map[string]any)
	second := cells[1].(map[string]any)
	if first["source"] != "print('new')" || second["source"] != "inserted" || second["cell_type"] != "markdown" {
		t.Fatalf("unexpected cells after notebook edits: %#v", cells)
	}
}

func TestNotebookEditPreservesJSONOrderAndUnicodeLikePython(t *testing.T) {
	dir := t.TempDir()
	notebook := filepath.Join(dir, "ordered.ipynb")
	raw := `{
 "cells": [
  {
   "id": "cell-1",
   "cell_type": "markdown",
   "metadata": {
    "z": 1,
    "a": 2
   },
   "source": "old"
  }
 ],
 "metadata": {
  "z": "<tag>",
  "a": "中文"
 },
 "nbformat": 4,
 "nbformat_minor": 5
}
`
	if err := os.WriteFile(notebook, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry(coretools.NewNotebookEditTool())
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "nb-order", Name: "notebook_edit",
		Input: map[string]any{"notebook_path": notebook, "cell_id": "cell-1", "new_source": "新的 <value>"},
	}, coretools.ExecutionContext{"cwd": dir})
	if result.IsError {
		t.Fatalf("notebook edit failed: %s", result.Content)
	}
	data, err := os.ReadFile(notebook)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		`"cells": [`,
		`"id": "cell-1"`,
		`"cell_type": "markdown"`,
		`"metadata": {`,
		`"z": 1`,
		`"a": 2`,
		`"source": "新的 <value>"`,
		`"z": "<tag>"`,
		`"a": "中文"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("notebook output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\\u4e2d") || strings.Contains(got, "\\u003c") {
		t.Fatalf("Python ensure_ascii=False equivalent should not escape unicode/html chars:\n%s", got)
	}
	if strings.Index(got, `"id": "cell-1"`) > strings.Index(got, `"cell_type": "markdown"`) ||
		strings.Index(got, `"cell_type": "markdown"`) > strings.Index(got, `"metadata": {`) ||
		strings.Index(got, `"metadata": {`) > strings.Index(got, `"source": "新的 <value>"`) {
		t.Fatalf("cell key order should follow Python insertion order:\n%s", got)
	}
}

func TestNotebookEditRuntimeFailuresAreDataLikePythonRegistry(t *testing.T) {
	dir := t.TempDir()
	notebook := filepath.Join(dir, "bad.ipynb")
	if err := os.WriteFile(notebook, []byte(`{"cells":[{"id":"cell-1","source":"x"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry(coretools.NewNotebookEditTool())
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "nb-missing", Name: "notebook_edit",
		Input: map[string]any{"notebook_path": notebook, "cell_id": "missing", "new_source": "x"},
	}, coretools.ExecutionContext{"cwd": dir})
	if result.IsError || !strings.Contains(result.Content, "Cell with id 'missing' not found in notebook. The notebook has 1 cells.") {
		t.Fatalf("runtime notebook failure should be returned as data like Python, error=%v content=%q", result.IsError, result.Content)
	}
}

func TestNotebookEditValidation(t *testing.T) {
	dir := t.TempDir()
	notebook := filepath.Join(dir, "demo.ipynb")
	if err := os.WriteFile(notebook, []byte(`{"cells":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry(coretools.NewNotebookEditTool())
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "nb-invalid", Name: "notebook_edit",
		Input: map[string]any{"notebook_path": notebook, "cell_id": "missing", "new_source": "x", "edit_mode": "insert"},
	}, coretools.ExecutionContext{"cwd": dir, "allowed_write_roots": []string{dir}})
	if !result.IsError || !strings.Contains(result.Content, "cell_type is required") {
		t.Fatalf("expected validation error, got error=%v content=%q", result.IsError, result.Content)
	}
}

func TestNotebookEditRelativeMissingFileHintMatchesPython(t *testing.T) {
	dir := t.TempDir()
	registry := coretools.NewToolRegistry(coretools.NewNotebookEditTool(), coretools.NewEditFileTool())
	notebook := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "nb-missing-relative", Name: "notebook_edit",
		Input: map[string]any{"notebook_path": "missing.ipynb", "cell_id": "c1", "new_source": "x"},
	}, coretools.ExecutionContext{"cwd": dir})
	if !notebook.IsError || !strings.Contains(notebook.Content, "File not found: missing.ipynb") ||
		strings.Contains(notebook.Content, "path is relative") {
		t.Fatalf("notebook_edit should match Python's resolved-path hint behavior, error=%v content=%q", notebook.IsError, notebook.Content)
	}

	edit := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "edit-missing-relative", Name: "edit_file",
		Input: map[string]any{"file_path": "missing.txt", "old_string": "x", "new_string": "y"},
	}, coretools.ExecutionContext{"cwd": dir})
	if !edit.IsError || !strings.Contains(edit.Content, "File not found: missing.txt (path is relative") {
		t.Fatalf("edit_file should keep Python's input-path relative hint, error=%v content=%q", edit.IsError, edit.Content)
	}
}

func TestGlobMatchIgnoresNoiseAndSortsRecentFirst(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.py")
	newPath := filepath.Join(dir, "new.py")
	ignored := filepath.Join(dir, ".git", "ignored.py")
	if err := os.MkdirAll(filepath.Dir(ignored), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(oldPath, []byte("old"), 0o644)
	time.Sleep(10 * time.Millisecond)
	_ = os.WriteFile(newPath, []byte("new"), 0o644)
	_ = os.WriteFile(ignored, []byte("ignored"), 0o644)

	registry := coretools.NewToolRegistry(coretools.NewGlobMatchTool())
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "glob-1", Name: "glob_match",
		Input: map[string]any{"path": dir, "pattern": "**/*.py"},
	}, coretools.ExecutionContext{"cwd": dir, "allowed_read_roots": []string{dir}})
	if result.IsError {
		t.Fatalf("glob failed: %s", result.Content)
	}
	if strings.Contains(result.Content, ".git") {
		t.Fatalf("expected ignored .git file to be absent: %s", result.Content)
	}
	lines := strings.Split(result.Content, "\n")
	resolvedNewPath := mustResolvePath(t, newPath)
	if len(lines) < 2 || lines[0] != resolvedNewPath {
		t.Fatalf("expected newest file first, got %q want first %q", result.Content, resolvedNewPath)
	}
}

func TestGlobMatchEqualMTimeKeepsGlobOrderLikePythonStableSort(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"z.py", "a.py", "m.py"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sameTime := time.Unix(1700000000, 0)
	for _, name := range []string{"z.py", "a.py", "m.py"} {
		if err := os.Chtimes(filepath.Join(dir, name), sameTime, sameTime); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := doublestar.FilepathGlob(filepath.ToSlash(filepath.Join(dir, "*.py")))
	if err != nil {
		t.Fatal(err)
	}
	for i, path := range expected {
		expected[i] = mustResolvePath(t, path)
	}
	registry := coretools.NewToolRegistry(coretools.NewGlobMatchTool())
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "glob-stable", Name: "glob_match",
		Input: map[string]any{"path": dir, "pattern": "*.py"},
	}, coretools.ExecutionContext{"cwd": dir, "allowed_read_roots": []string{dir}})
	if result.IsError {
		t.Fatalf("glob failed: %s", result.Content)
	}
	got := strings.Split(result.Content, "\n")
	if strings.Join(got, "\n") != strings.Join(expected, "\n") {
		t.Fatalf("equal mtime should preserve glob order like Python stable sort:\nwant %#v\n got %#v", expected, got)
	}
}

func TestGrepSearchOutputModesAndHeadLimitMatchPython(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("Alpha\nBeta\nalpha two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("alpha three\nother\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry(coretools.NewGrepSearchTool())
	content := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "grep-content", Name: "grep_search",
		Input: map[string]any{"pattern": "alpha", "path": dir, "output_mode": "content", "case_insensitive": true, "head_limit": 2},
	}, coretools.ExecutionContext{"cwd": dir})
	if content.IsError || !strings.Contains(content.Content, "... (1 more results truncated)") {
		t.Fatalf("expected Python-style grep truncation, error=%v content=%q", content.IsError, content.Content)
	}
	if strings.Count(strings.Split(content.Content, "\n\n...")[0], "\n") != 1 {
		t.Fatalf("head_limit=2 should keep two result lines, got %q", content.Content)
	}
	files := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "grep-files", Name: "grep_search",
		Input: map[string]any{"pattern": "alpha", "path": dir, "output_mode": "files_with_matches"},
	}, coretools.ExecutionContext{"cwd": dir})
	if files.IsError || !strings.Contains(files.Content, "a.txt") || !strings.Contains(files.Content, "b.txt") {
		t.Fatalf("files_with_matches mismatch: error=%v content=%q", files.IsError, files.Content)
	}
	count := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "grep-count", Name: "grep_search",
		Input: map[string]any{"pattern": "alpha", "path": dir, "output_mode": "count", "case_insensitive": true},
	}, coretools.ExecutionContext{"cwd": dir})
	if count.IsError || !strings.Contains(count.Content, ":2") || !strings.Contains(count.Content, ":1") {
		t.Fatalf("count mode mismatch: error=%v content=%q", count.IsError, count.Content)
	}
	none := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "grep-none", Name: "grep_search",
		Input: map[string]any{"pattern": "nomatch", "path": dir, "output_mode": "content"},
	}, coretools.ExecutionContext{"cwd": dir})
	if none.IsError || none.Content != "No matches found." {
		t.Fatalf("no-match output mismatch: error=%v content=%q", none.IsError, none.Content)
	}
}

func TestGrepSearchUsesRipgrepDefaultFilesWithMatches(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not installed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.txt")
	_ = os.WriteFile(path, []byte("needle\nneedle again\n"), 0o644)
	registry := coretools.NewToolRegistry(coretools.NewGrepSearchTool())
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "grep-1", Name: "grep_search",
		Input: map[string]any{"path": dir, "pattern": "needle"},
	}, coretools.ExecutionContext{"cwd": dir, "allowed_read_roots": []string{dir}})
	if result.IsError {
		t.Fatalf("grep failed: %s", result.Content)
	}
	resolvedPath := mustResolvePath(t, path)
	if strings.TrimSpace(result.Content) != resolvedPath {
		t.Fatalf("expected files_with_matches output %q, got %q", resolvedPath, result.Content)
	}

	var lines []string
	for i := 0; i < 260; i++ {
		lines = append(lines, "needle")
	}
	_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	contentResult := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "grep-2", Name: "grep_search",
		Input: map[string]any{"path": path, "pattern": "needle", "output_mode": "content"},
	}, coretools.ExecutionContext{"cwd": dir, "allowed_read_roots": []string{dir}})
	if contentResult.IsError {
		t.Fatalf("grep content failed: %s", contentResult.Content)
	}
	if !strings.Contains(contentResult.Content, "... (10 more results truncated)") {
		t.Fatalf("expected default 250-line truncation, got %q", contentResult.Content)
	}
	unlimitedResult := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "grep-3", Name: "grep_search",
		Input: map[string]any{"path": path, "pattern": "needle", "output_mode": "content", "head_limit": 0},
	}, coretools.ExecutionContext{"cwd": dir, "allowed_read_roots": []string{dir}})
	if unlimitedResult.IsError {
		t.Fatalf("grep explicit unlimited failed: %s", unlimitedResult.Content)
	}
	if strings.Contains(unlimitedResult.Content, "more results truncated") || strings.Count(unlimitedResult.Content, "\n")+1 != 260 {
		t.Fatalf("explicit head_limit=0 should disable truncation like Python, got %q", unlimitedResult.Content)
	}
	negativeResult := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "grep-4", Name: "grep_search",
		Input: map[string]any{"path": path, "pattern": "needle", "output_mode": "content", "head_limit": -1},
	}, coretools.ExecutionContext{"cwd": dir, "allowed_read_roots": []string{dir}})
	if negativeResult.IsError {
		t.Fatalf("grep explicit negative head_limit failed: %s", negativeResult.Content)
	}
	if !strings.Contains(negativeResult.Content, "... (261 more results truncated)") {
		t.Fatalf("negative head_limit should use Python truncation math, got %q", negativeResult.Content)
	}
	prefix := strings.Split(negativeResult.Content, "\n\n...")[0]
	if strings.Count(prefix, "\n")+1 != 259 {
		t.Fatalf("head_limit=-1 should keep len(lines)-1 result lines like Python, got %q", negativeResult.Content)
	}
}

func TestGrepSearchSchemaExposesCountOutputMode(t *testing.T) {
	schema := coretools.NewGrepSearchTool().ToAPISchema()
	inputSchema, _ := schema["input_schema"].(map[string]any)
	properties, _ := inputSchema["properties"].(map[string]any)
	outputMode, _ := properties["output_mode"].(map[string]any)
	values, _ := outputMode["enum"].([]any)
	found := false
	for _, value := range values {
		if value == "count" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("grep_search output_mode schema should include Python count mode, output_mode=%#v full_schema=%#v", outputMode, schema)
	}
	if _, hasRef := inputSchema["$ref"]; hasRef || len(properties) == 0 {
		t.Fatalf("tool input schema should inline top-level properties like Python, got %#v", inputSchema)
	}
	if _, hasAdditional := inputSchema["additionalProperties"]; hasAdditional {
		t.Fatalf("built-in tool schema should not add additionalProperties like Python Pydantic schema: %#v", inputSchema)
	}
	if outputMode["default"] != "files_with_matches" ||
		outputMode["description"] != "Output mode: 'content', 'files_with_matches', or 'count'" {
		t.Fatalf("grep_search output_mode schema should preserve Python default/description, got %#v", outputMode)
	}
	globProp, _ := properties["glob"].(map[string]any)
	globDefault, hasGlobDefault := globProp["default"]
	if globProp["oneOf"] != nil || globProp["anyOf"] == nil || !hasGlobDefault || globDefault != nil {
		t.Fatalf("nullable glob schema should use Python-style anyOf/default null, got %#v", globProp)
	}
}

func TestNotebookEditSchemaDescriptionsDoNotTruncateAtCommas(t *testing.T) {
	schema := coretools.NewNotebookEditTool().ToAPISchema()
	inputSchema, _ := schema["input_schema"].(map[string]any)
	properties, _ := inputSchema["properties"].(map[string]any)
	cellID, _ := properties["cell_id"].(map[string]any)
	if !strings.Contains(fmt.Sprint(cellID["description"]), "or at the beginning if not specified") {
		t.Fatalf("cell_id description should match Python without comma truncation, got %#v", cellID)
	}
	cellType, _ := properties["cell_type"].(map[string]any)
	cellTypeDefault, hasCellTypeDefault := cellType["default"]
	if cellType["oneOf"] != nil || cellType["anyOf"] == nil || !hasCellTypeDefault || cellTypeDefault != nil {
		t.Fatalf("cell_type should be nullable with JSON null default like Python, got %#v", cellType)
	}
}

func TestToolSearchPrefixSyntax(t *testing.T) {
	registry := coretools.NewToolRegistry(coretools.NewToolSearchTool())
	registry.Register(&deferredTestTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:           "mcp_alpha",
		Description:    "Alpha remote tool",
		InputPrototype: map[string]any{},
		ShouldDefer:    true,
		ReadOnly:       coretools.BoolPtr(true),
	}}})
	registry.Register(&deferredTestTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:           "notebook_beta",
		Description:    "Beta notebook tool",
		InputPrototype: map[string]any{},
		ShouldDefer:    true,
		ReadOnly:       coretools.BoolPtr(true),
	}}})

	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "search-1", Name: "tool_search", Input: map[string]any{"query": "+mcp"},
	}, coretools.ExecutionContext{"_registry": registry})
	if result.IsError || !strings.Contains(result.Content, "mcp_alpha") || strings.Contains(result.Content, "notebook_beta") {
		t.Fatalf("unexpected prefix search result: error=%v content=%q", result.IsError, result.Content)
	}
}

func TestToolSearchSelectArgumentsUsePythonJSONDumpsFormatting(t *testing.T) {
	registry := coretools.NewToolRegistry(coretools.NewToolSearchTool())
	registry.Register(&deferredSchemaTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:        "schema_tool",
		Description: "Schema tool",
		ShouldDefer: true,
		ReadOnly:    coretools.BoolPtr(true),
	}}})
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "select-1", Name: "tool_search", Input: map[string]any{"query": "select:schema_tool"},
	}, coretools.ExecutionContext{"_registry": registry})
	if result.IsError {
		t.Fatalf("tool_search select failed: %s", result.Content)
	}
	for _, want := range []string{`"type": "object"`, `"description": "<tag>"`, `"value": {"type": "string"}`} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("select arguments should match Python json.dumps default separators, missing %q in:\n%s", want, result.Content)
		}
	}
	if strings.Contains(result.Content, `"type":"object"`) || strings.Contains(result.Content, `\u003c`) {
		t.Fatalf("select arguments should use Python separators and avoid Go HTML escaping, got:\n%s", result.Content)
	}
}

func TestToolSearchMissingSuggestionsKeepDeferredInsertionOrderLikePython(t *testing.T) {
	registry := coretools.NewToolRegistry(coretools.NewToolSearchTool())
	for _, name := range []string{"bbb", "ccc", "ddd"} {
		registry.Register(&deferredTestTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
			Name:        name,
			Description: name,
			ShouldDefer: true,
			ReadOnly:    coretools.BoolPtr(true),
		}}})
	}
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "search-missing", Name: "tool_search", Input: map[string]any{"query": "select:zzz"},
	}, coretools.ExecutionContext{"_registry": registry})
	if result.IsError {
		t.Fatalf("tool_search missing query failed: %s", result.Content)
	}
	want := "Available deferred tools (use select: to load): bbb, ccc, ddd"
	if !strings.Contains(result.Content, want) {
		t.Fatalf("equal-score suggestions should preserve Python insertion order, want %q in:\n%s", want, result.Content)
	}
}

func TestDeferredToolIndexSearchMatchesPython(t *testing.T) {
	index := coretools.NewDeferredToolIndex()
	index.Add(&deferredTestTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:        "name_hit",
		Description: "No useful words here",
		SearchHint:  "misc",
	}}})
	index.Add(&deferredTestTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:        "hint_only",
		Description: "keyword only in description should not count",
		SearchHint:  "keyword",
	}}})
	index.Add(&deferredTestTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:        "desc_only",
		Description: "keyword",
		SearchHint:  "",
	}}})

	results := index.Search("keyword")
	if len(results) != 1 || results[0].Name() != "hint_only" {
		t.Fatalf("description should not participate in Python deferred search, got %#v", toolNames(results))
	}
	index.Add(&deferredTestTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:        "keyword_name",
		Description: "",
		SearchHint:  "",
	}}})
	results = index.Search("keyword")
	if len(results) != 2 || results[0].Name() != "keyword_name" || results[1].Name() != "hint_only" {
		t.Fatalf("name matches should rank above hint matches, got %#v", toolNames(results))
	}
	index.Add(&deferredTestTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:        "another_hint",
		Description: "",
		SearchHint:  "keyword",
	}}})
	results = index.Search("keyword")
	if len(results) != 3 || results[1].Name() != "hint_only" || results[2].Name() != "another_hint" {
		t.Fatalf("equal-score deferred tools should keep Python stable insertion order, got %#v", toolNames(results))
	}
	results = index.Search("+keyword")
	if len(results) != 1 || results[0].Name() != "keyword_name" {
		t.Fatalf("+prefix without keywords should return matching prefix tools, got %#v", toolNames(results))
	}
	results = index.Search("select:keyword_name,hint_only,missing")
	if len(results) != 2 || results[0].Name() != "keyword_name" || results[1].Name() != "hint_only" {
		t.Fatalf("select search should return exact deferred matches in query order, got %#v", toolNames(results))
	}
	if activated := index.Activate("KEYWORD_NAME"); activated != nil {
		t.Fatalf("activate should be case-sensitive like Python, got %#v", activated.Name())
	}
	if activated := index.Activate("keyword_name"); activated == nil || activated.Name() != "keyword_name" {
		t.Fatalf("exact activation failed: %#v", activated)
	}
}

type deferredTestTool struct {
	coretools.BaseTool
}

type deferredSchemaTool struct {
	coretools.BaseTool
}

func (t *deferredSchemaTool) ToAPISchema() map[string]any {
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type":        "object",
			"description": "<tag>",
			"properties": map[string]any{
				"value": map[string]any{"type": "string"},
			},
		},
	}
}

func toolNames(tools []coretools.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	return names
}

func mustResolvePath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		abs, absErr := filepath.Abs(path)
		if absErr != nil {
			t.Fatalf("resolve path %s: %v / %v", path, err, absErr)
		}
		return filepath.Clean(abs)
	}
	return filepath.Clean(resolved)
}
