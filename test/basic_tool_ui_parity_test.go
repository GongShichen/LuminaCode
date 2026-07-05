package test

import (
	"path/filepath"
	"strings"
	"testing"

	coretools "LuminaCode/tools"
)

func TestReadFileRenderAndBackfillMatchPython(t *testing.T) {
	dir := t.TempDir()
	limit := 10
	input := coretools.ReadFileInput{FilePath: "src/main.go", Offset: 5, Limit: &limit}
	if got := coretools.RenderReadFileToolUse(input); got != "📖 Read main.go (L5-15)" {
		t.Fatalf("unexpected read render: %q", got)
	}
	if got := coretools.RenderReadFileToolResult("1\ta\n2\tb", false); got != "Read 2 lines" {
		t.Fatalf("unexpected read result render: %q", got)
	}
	errText := strings.Repeat("e", 170)
	if got := coretools.RenderReadFileToolResult(errText, true); got != "Read failed: "+strings.Repeat("e", 150) {
		t.Fatalf("unexpected read error render: %q", got)
	}
	grouped := coretools.RenderGroupedReadFileToolUse([]coretools.ReadFileInput{
		{FilePath: "/tmp/a.go"}, {FilePath: "/tmp/b.go"}, {FilePath: "/tmp/c.go"}, {FilePath: "/tmp/d.go"}, {FilePath: "/tmp/e.go"}, {FilePath: "/tmp/f.go"},
	})
	if grouped != "📖 Read 6 files: a.go, b.go, c.go, d.go, e.go" {
		t.Fatalf("unexpected grouped read render: %q", grouped)
	}
	backfilled := coretools.BackfillReadFileInput(input, coretools.ExecutionContext{"cwd": dir})
	if backfilled.FilePath != filepath.Join(dir, "src", "main.go") || backfilled.Offset != 5 || backfilled.Limit == nil || *backfilled.Limit != 10 {
		t.Fatalf("unexpected read backfill: %#v", backfilled)
	}
}

func TestWriteFileRenderAndBackfillMatchPython(t *testing.T) {
	dir := t.TempDir()
	if got := coretools.RenderWriteFileToolUse(coretools.WriteFileInput{FilePath: "/tmp/small.txt", Content: "abc"}); got != "Write small.txt (3B)" {
		t.Fatalf("unexpected small write render: %q", got)
	}
	if got := coretools.RenderWriteFileToolUse(coretools.WriteFileInput{FilePath: "/tmp/unicode.txt", Content: "你好"}); got != "Write unicode.txt (2B)" {
		t.Fatalf("write render should count Python characters, got %q", got)
	}
	if got := coretools.RenderWriteFileToolUse(coretools.WriteFileInput{FilePath: "/tmp/big.txt", Content: strings.Repeat("x", 2048)}); got != "Write big.txt (2KB)" {
		t.Fatalf("unexpected large write render: %q", got)
	}
	if got := coretools.RenderWriteFileToolResult("ok", false); got != "Write OK" {
		t.Fatalf("unexpected write result render: %q", got)
	}
	errText := strings.Repeat("e", 170)
	if got := coretools.RenderWriteFileToolResult(errText, true); got != "Write failed: "+strings.Repeat("e", 150) {
		t.Fatalf("unexpected write error render: %q", got)
	}
	grouped := coretools.RenderGroupedWriteFileToolUse([]coretools.WriteFileInput{
		{FilePath: "/tmp/a"}, {FilePath: "/tmp/b"}, {FilePath: "/tmp/c"}, {FilePath: "/tmp/d"}, {FilePath: "/tmp/e"}, {FilePath: "/tmp/f"},
	})
	if grouped != "Write 6 files: a, b, c, d, e" {
		t.Fatalf("unexpected grouped write render: %q", grouped)
	}
	backfilled := coretools.BackfillWriteFileInput(coretools.WriteFileInput{FilePath: "out/file.txt", Content: "x"}, coretools.ExecutionContext{"cwd": dir})
	if backfilled.FilePath != filepath.Join(dir, "out", "file.txt") || backfilled.Content != "x" {
		t.Fatalf("unexpected write backfill: %#v", backfilled)
	}
}

func TestBashRenderAndBackfillMatchPython(t *testing.T) {
	longCommand := strings.Repeat("x", 130)
	if got := coretools.RenderBashToolUse(coretools.BashInput{Command: longCommand}); got != "Bash "+strings.Repeat("x", 117)+"..." {
		t.Fatalf("unexpected bash use render: %q", got)
	}
	if got := coretools.RenderBashToolResult("[Exit code: 0]\n\noutput", false); got != "[Exit code: 0]" {
		t.Fatalf("unexpected bash exit-code result render: %q", got)
	}
	if got := coretools.RenderBashToolResult("\nfirst line\nsecond line", false); got != "first line" {
		t.Fatalf("unexpected bash first-line result render: %q", got)
	}
	if got := coretools.RenderBashToolResult("   ", false); got != "(empty output)" {
		t.Fatalf("unexpected empty bash result render: %q", got)
	}
	errText := strings.Repeat("e", 170)
	if got := coretools.RenderBashToolResult(errText, true); got != "Bash failed: "+strings.Repeat("e", 150) {
		t.Fatalf("unexpected bash error render: %q", got)
	}
	grouped := coretools.RenderGroupedBashToolUse([]coretools.BashInput{
		{Command: strings.Repeat("a", 70)}, {Command: "second"}, {Command: "third"}, {Command: "fourth"},
	})
	if grouped != "Bash (4 commands): "+strings.Repeat("a", 60)+"; second; third" {
		t.Fatalf("unexpected grouped bash render: %q", grouped)
	}
	timeout := 1000
	backfilled := coretools.BackfillBashInput(coretools.BashInput{Command: "ls -la", Timeout: &timeout})
	if backfilled.Description == "" || backfilled.Command != "ls -la" || backfilled.Timeout == nil || *backfilled.Timeout != 1000 {
		t.Fatalf("unexpected bash backfill: %#v", backfilled)
	}
	described := coretools.BackfillBashInput(coretools.BashInput{Command: "ls -la", Description: "custom"})
	if described.Description != "custom" {
		t.Fatalf("existing description should be preserved, got %#v", described)
	}
}
