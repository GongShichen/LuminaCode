package test

import (
	"path/filepath"
	"strings"
	"testing"

	coretools "LuminaCode/tools"
)

func TestGrepSearchRenderAndBackfillMatchPython(t *testing.T) {
	dir := t.TempDir()
	input := coretools.GrepSearchInput{
		Pattern:         strings.Repeat("x", 70),
		Path:            "src/pkg",
		Glob:            "*.go",
		OutputMode:      "",
		CaseInsensitive: true,
	}
	rendered := coretools.RenderGrepSearchToolUse(input)
	if rendered != "Grep '"+strings.Repeat("x", 60)+"' in pkg -i" {
		t.Fatalf("unexpected grep render: %q", rendered)
	}
	backfilled := coretools.BackfillGrepSearchInput(input, coretools.ExecutionContext{"cwd": dir})
	if backfilled.Path != filepath.Join(dir, "src", "pkg") || backfilled.OutputMode != "files_with_matches" || backfilled.HeadLimit == nil || *backfilled.HeadLimit != 250 {
		t.Fatalf("unexpected grep backfill: %#v", backfilled)
	}
	if got := coretools.RenderGrepSearchToolResult("No matches found.", false); got != "0 matches" {
		t.Fatalf("unexpected no-match render: %q", got)
	}
	if got := coretools.RenderGrepSearchToolResult("a\nb\nc", false); got != "3 match(es)" {
		t.Fatalf("unexpected match count render: %q", got)
	}
	grouped := coretools.RenderGroupedGrepSearchToolUse([]coretools.GrepSearchInput{
		{Pattern: "one"}, {Pattern: "two"}, {Pattern: "three"}, {Pattern: "four"}, {Pattern: "five"}, {Pattern: "six"},
	})
	if grouped != "Grep 6 patterns: one, two, three, four, five" {
		t.Fatalf("unexpected grouped grep render: %q", grouped)
	}
}

func TestGlobMatchRenderAndBackfillMatchPython(t *testing.T) {
	dir := t.TempDir()
	input := coretools.GlobMatchInput{Pattern: "**/*.go", Path: "src/pkg"}
	if got := coretools.RenderGlobMatchToolUse(input); got != "Glob '**/*.go' in pkg" {
		t.Fatalf("unexpected glob render: %q", got)
	}
	backfilled := coretools.BackfillGlobMatchInput(input, coretools.ExecutionContext{"cwd": dir})
	if backfilled.Path != filepath.Join(dir, "src", "pkg") {
		t.Fatalf("unexpected glob backfill: %#v", backfilled)
	}
	if got := coretools.RenderGlobMatchToolResult("No files found matching the pattern.", false); got != "0 files" {
		t.Fatalf("unexpected no-file render: %q", got)
	}
	if got := coretools.RenderGlobMatchToolResult("a\nb", false); got != "2 file(s)" {
		t.Fatalf("unexpected file count render: %q", got)
	}
	grouped := coretools.RenderGroupedGlobMatchToolUse([]coretools.GlobMatchInput{
		{Pattern: "*.go"}, {Pattern: "*.py"}, {Pattern: "*.md"}, {Pattern: "*.json"}, {Pattern: "*.yaml"}, {Pattern: "*.toml"},
	})
	if grouped != "Glob 6 patterns: *.go, *.py, *.md, *.json, *.yaml" {
		t.Fatalf("unexpected grouped glob render: %q", grouped)
	}
}

func TestToolSearchRenderMatchesPython(t *testing.T) {
	input := coretools.ToolSearchInput{Query: strings.Repeat("q", 80)}
	if got := coretools.RenderToolSearchToolUse(input); got != "Tool search: "+strings.Repeat("q", 60) {
		t.Fatalf("unexpected tool_search use render: %q", got)
	}
	if got := coretools.RenderToolSearchToolResult("abcdef", false); got != "Tool search (6 chars)" {
		t.Fatalf("unexpected tool_search result render: %q", got)
	}
	if got := coretools.RenderToolSearchToolResult("工具", false); got != "Tool search (2 chars)" {
		t.Fatalf("tool_search result render should count Python characters, got %q", got)
	}
	errText := strings.Repeat("e", 120)
	if got := coretools.RenderToolSearchToolResult(errText, true); got != "Tool search failed: "+strings.Repeat("e", 100) {
		t.Fatalf("unexpected tool_search error render: %q", got)
	}
}
