package test

import (
	"path/filepath"
	"strings"
	"testing"

	coretools "LuminaCode/tools"
)

func TestNotebookEditRenderToolUseMatchesPython(t *testing.T) {
	replace := coretools.RenderNotebookEditToolUse(coretools.NotebookEditInput{
		NotebookPath: "/tmp/demo.ipynb",
		CellID:       "abcdef123456",
		NewSource:    "x",
	})
	if replace != "Edit notebook: demo.ipynb cell[abcdef12]" {
		t.Fatalf("unexpected replace render: %q", replace)
	}
	insert := coretools.RenderNotebookEditToolUse(coretools.NotebookEditInput{
		NotebookPath: "/tmp/demo.ipynb",
		CellID:       "abcdef123456",
		NewSource:    "x",
		EditMode:     "insert",
	})
	if insert != "Edit notebook (insert): demo.ipynb cell[abcdef12]" {
		t.Fatalf("unexpected insert render: %q", insert)
	}
}

func TestNotebookEditRenderResultAndGroupedUseMatchPython(t *testing.T) {
	if got := coretools.RenderNotebookEditToolResult("anything", false); got != "Notebook edit OK" {
		t.Fatalf("unexpected success render: %q", got)
	}
	errText := strings.Repeat("x", 200)
	errRendered := coretools.RenderNotebookEditToolResult(errText, true)
	if !strings.HasPrefix(errRendered, "Notebook edit failed: ") || len(strings.TrimPrefix(errRendered, "Notebook edit failed: ")) != 150 {
		t.Fatalf("unexpected error render: %q", errRendered)
	}
	grouped := coretools.RenderGroupedNotebookEditToolUse([]coretools.NotebookEditInput{
		{NotebookPath: "/tmp/a.ipynb", CellID: "1"},
		{NotebookPath: "/tmp/b.ipynb", CellID: "2"},
		{NotebookPath: "/tmp/a.ipynb", CellID: "3"},
	})
	if grouped != "Edit notebook: 3 edits in a.ipynb, b.ipynb" {
		t.Fatalf("unexpected grouped render: %q", grouped)
	}
}

func TestNotebookEditBackfillInputMatchesPython(t *testing.T) {
	dir := t.TempDir()
	input := coretools.NotebookEditInput{
		NotebookPath: "demo.ipynb",
		CellID:       "cell-1",
		NewSource:    "x",
	}
	got := coretools.BackfillNotebookEditInput(input, coretools.ExecutionContext{"cwd": dir})
	want := filepath.Join(dir, "demo.ipynb")
	if got.NotebookPath != want || got.CellID != "cell-1" || got.NewSource != "x" || got.EditMode != "replace" {
		t.Fatalf("unexpected backfilled input: %#v want path %q", got, want)
	}
}
