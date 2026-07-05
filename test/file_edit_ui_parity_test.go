package test

import (
	"strings"
	"testing"

	coretools "LuminaCode/tools"
)

func TestEditFileRenderToolUseMatchesPython(t *testing.T) {
	rendered := coretools.RenderEditFileToolUse("/tmp/demo.txt", "alpha\nbeta\n", "alpha\nBETA\n", true)
	for _, want := range []string{
		"edit_file (all): demo.txt",
		"--- a/demo.txt",
		"+++ b/demo.txt",
		"@@",
		"-beta",
		"+BETA",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered tool use missing %q:\n%s", want, rendered)
		}
	}

	fallback := coretools.RenderEditFileToolUse("/tmp/demo.txt", "same", "same", false)
	if fallback != "edit_file: demo.txt\n  [-]same\n  [+]same" {
		t.Fatalf("expected Python fallback render, got %q", fallback)
	}
}

func TestEditFileRenderGroupedToolUseMatchesPython(t *testing.T) {
	rendered := coretools.RenderGroupedEditFileToolUse([]coretools.EditFileInput{
		{FilePath: "/tmp/a.go", OldString: "one\n", NewString: "ONE\n"},
		{FilePath: "/tmp/a.go", OldString: "two\nthree", NewString: "TWO"},
		{FilePath: "/tmp/b.go", OldString: "x", NewString: "y"},
	})
	lines := strings.Split(rendered, "\n")
	if lines[0] != "edit_file: a.go (2 edits, [-]4 lines, [+]3 lines)" {
		t.Fatalf("unexpected grouped first line: %q\n%s", lines[0], rendered)
	}
	if !strings.Contains(rendered, "edit_file: b.go") || !strings.Contains(rendered, "-x") || !strings.Contains(rendered, "+y") {
		t.Fatalf("single-edit group should render full preview:\n%s", rendered)
	}
}

func TestEditFileRenderToolResultMatchesPython(t *testing.T) {
	success := "Edit applied successfully: 1 occurrence(s) replaced in demo.txt\n---DIFF---\n--- a/demo.txt\n+++ b/demo.txt\n@@\n-old\n+new\n"
	rendered := coretools.RenderEditFileToolResult(success, false)
	if strings.Contains(rendered, "---DIFF---") || !strings.Contains(rendered, "Edit applied successfully") || !strings.Contains(rendered, "+new") {
		t.Fatalf("success render should remove diff sentinel and keep diff:\n%s", rendered)
	}
	if got := coretools.RenderEditFileToolResult("short custom output", false); got != "Edit: short custom output" {
		t.Fatalf("unexpected generic success render: %q", got)
	}
	longUnicode := strings.Repeat("界", 121)
	if got := coretools.RenderEditFileToolResult(longUnicode, false); got != "Edit: "+strings.Repeat("界", 120) {
		t.Fatalf("generic success render should hard-slice like Python, got %q", got)
	}

	multiple := coretools.RenderEditFileToolResult("[ErrCode 9] MULTIPLE_MATCHES: Found 2 matches", true)
	if multiple != "Edit failed (multiple matches found).\n  Include more surrounding context to make the match unique,\n  or set replace_all=true to replace all occurrences." {
		t.Fatalf("unexpected multiple-match render:\n%s", multiple)
	}
	notebook := coretools.RenderEditFileToolResult("[ErrCode 5] NOTEBOOK_REDIRECT: use notebook", true)
	if !strings.Contains(notebook, "Edit blocked (use notebook_edit for .ipynb files).") ||
		!strings.Contains(notebook, "with 'select:notebook_edit' to load it first.") {
		t.Fatalf("unexpected notebook redirect render:\n%s", notebook)
	}
	noCode := coretools.RenderEditFileToolResult("plain failure", true)
	if noCode != "Edit failed: plain failure" {
		t.Fatalf("unexpected plain failure render: %q", noCode)
	}
}

func TestEditFileRenderErrorContextMatchesPython(t *testing.T) {
	context := coretools.RenderEditErrorContext("/tmp/demo.txt", "beta target", "alpha\nbeta value\ngamma\n")
	for _, want := range []string{
		"Closest match at line 2 (50% similarity):",
		"  >>>    2  beta value",
		"  Expected: [-]beta target",
	} {
		if !strings.Contains(context, want) {
			t.Fatalf("context render missing %q:\n%s", want, context)
		}
	}
	none := coretools.RenderEditErrorContext("/tmp/demo.txt", "missing words", "alpha\nbeta\n")
	if !strings.Contains(none, "No similar lines found in demo.txt.") || !strings.Contains(none, "The file may have been completely rewritten.") {
		t.Fatalf("unexpected no-match context:\n%s", none)
	}
	if got := coretools.RenderEditErrorContext("/tmp/demo.txt", "   ", "alpha"); got != "Error: old_string appears to be empty or whitespace-only." {
		t.Fatalf("unexpected empty old_string context: %q", got)
	}
}

func TestEditFileUITruncationMatchesPython(t *testing.T) {
	longLine := strings.Repeat("a", 101)
	fallback := coretools.RenderEditFileToolUse("/tmp/demo.txt", longLine, longLine, false)
	if !strings.Contains(fallback, "[-]"+strings.Repeat("a", 97)+"...") ||
		!strings.Contains(fallback, "[+]"+strings.Repeat("a", 97)+"...") {
		t.Fatalf("fallback line preview should truncate with Python ellipsis:\n%s", fallback)
	}

	longContextLine := strings.Repeat("x", 121)
	context := coretools.RenderEditErrorContext("/tmp/demo.txt", longContextLine, longContextLine+"\n")
	if !strings.Contains(context, strings.Repeat("x", 117)+"...") {
		t.Fatalf("error context lines should truncate with Python ellipsis:\n%s", context)
	}

	generic := coretools.RenderEditFileToolResult(strings.Repeat("界", 121), false)
	if generic != "Edit: "+strings.Repeat("界", 120) {
		t.Fatalf("generic result should keep Python hard slice without ellipsis, got %q", generic)
	}
}
