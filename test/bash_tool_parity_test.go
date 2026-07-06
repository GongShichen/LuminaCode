package test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coretools "LuminaCode/tools"
	bashpkg "LuminaCode/tools/bash"
)

func TestBashBlocksCommandSubstitutionLikePython(t *testing.T) {
	registry := coretools.NewToolRegistry(coretools.NewBashTool())
	dir := t.TempDir()
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "bash-1", Name: "run_shell",
		Input: map[string]any{"command": "echo $(pwd)"},
	}, coretools.ExecutionContext{"cwd": dir})
	if !strings.Contains(result.Content, "Command blocked by security checks") ||
		!strings.Contains(result.Content, "$() command substitution") {
		t.Fatalf("expected command substitution block, got error=%v content=%q", result.IsError, result.Content)
	}
}

func TestBashInputSchemaDefaultsMatchPython(t *testing.T) {
	schema := coretools.NewBashTool().ToAPISchema()["input_schema"].(map[string]any)
	properties := schema["properties"].(map[string]any)

	timeout := properties["timeout"].(map[string]any)
	timeoutDefault, hasTimeoutDefault := timeout["default"]
	if timeout["oneOf"] != nil || timeout["anyOf"] == nil || !hasTimeoutDefault || timeoutDefault != nil ||
		!strings.Contains(timeout["description"].(string), "Optional timeout in milliseconds") {
		t.Fatalf("timeout schema should match Python nullable/default shape, got %#v", timeout)
	}
	timeoutSeconds := properties["timeout_seconds"].(map[string]any)
	if !strings.Contains(timeoutSeconds["description"].(string), "Optional timeout in seconds") {
		t.Fatalf("timeout_seconds schema mismatch: %#v", timeoutSeconds)
	}

	description := properties["description"].(map[string]any)
	if description["default"] != "" || description["description"] != "Brief activity description for UI display" {
		t.Fatalf("description schema mismatch: %#v", description)
	}

	background := properties["run_in_background"].(map[string]any)
	if background["default"] != false || background["description"] != "If True, execute asynchronously and return immediately" {
		t.Fatalf("run_in_background schema mismatch: %#v", background)
	}

	disableSandbox := properties["dangerouslyDisableSandbox"].(map[string]any)
	if disableSandbox["default"] != false || disableSandbox["description"] != "If True, skip sandbox isolation (requires policy approval)" {
		t.Fatalf("dangerouslyDisableSandbox schema mismatch: %#v", disableSandbox)
	}
}

func TestRunShellAliasesMatchPythonBackwardCompatibility(t *testing.T) {
	var input coretools.RunShellInput = coretools.BashInput{Command: "pwd"}
	if input.Command != "pwd" {
		t.Fatalf("RunShellInput should alias BashInput, got %#v", input)
	}
	var tool *coretools.RunShellTool = coretools.NewBashTool()
	if tool.Name() != "run_shell" {
		t.Fatalf("RunShellTool should alias BashTool, got %q", tool.Name())
	}
}

func TestEditErrorCodePublicValuesMatchPythonEnum(t *testing.T) {
	codes := []struct {
		code  coretools.EditErrorCode
		name  string
		value int
	}{
		{coretools.EditOK, "OK", 0},
		{coretools.EditNoOp, "NO_OP", 1},
		{coretools.EditPermissionDenied, "PERMISSION_DENIED", 2},
		{coretools.EditEmptyOldOnExisting, "EMPTY_OLD_ON_EXISTING", 3},
		{coretools.EditFileNotFound, "FILE_NOT_FOUND", 4},
		{coretools.EditNotebookRedirect, "NOTEBOOK_REDIRECT", 5},
		{coretools.EditUnreadFile, "UNREAD_FILE", 6},
		{coretools.EditStaleRead, "STALE_READ", 7},
		{coretools.EditStringNotFound, "STRING_NOT_FOUND", 8},
		{coretools.EditMultipleMatches, "MULTIPLE_MATCHES", 9},
		{coretools.EditFileTooLarge, "FILE_TOO_LARGE", 10},
		{coretools.EditWriteFailed, "WRITE_FAILED", 11},
		{coretools.EditReadFailed, "READ_FAILED", 12},
		{coretools.EditCascadingEdit, "CASCADING_EDIT", 13},
	}
	for _, tc := range codes {
		if tc.code.Name() != tc.name || tc.code.Value() != tc.value {
			t.Fatalf("unexpected edit error code: got %s=%d want %s=%d", tc.code.Name(), tc.code.Value(), tc.name, tc.value)
		}
	}
}

func TestBashReadOnlyClassifierMatchesPythonSafetyRules(t *testing.T) {
	tool := coretools.NewBashTool()
	cases := []struct {
		name     string
		command  string
		readOnly bool
	}{
		{name: "simple read", command: "ls -la", readOnly: true},
		{name: "safe subcommand", command: "git status", readOnly: true},
		{name: "output redirect writes", command: "cat input.txt > output.txt", readOnly: false},
		{name: "sensitive read needs permission", command: "cat /etc/shadow", readOnly: false},
		{name: "secret read dangerous", command: "cat /proc/self/environ", readOnly: false},
		{name: "privilege boundary", command: "sudo ls", readOnly: false},
	}
	for _, tc := range cases {
		got := tool.IsReadOnly(coretools.BashInput{Command: tc.command})
		if got != tc.readOnly {
			t.Fatalf("%s: IsReadOnly(%q)=%v want %v", tc.name, tc.command, got, tc.readOnly)
		}
	}
}

func TestBashExitCodeSemanticsMatchPython(t *testing.T) {
	cases := []struct {
		name    string
		command string
		code    int
		want    string
	}{
		{
			name:    "rsync partial transfer is error",
			command: "rsync src dst",
			code:    23,
			want:    "[Exit code: 23 — ERROR: Partial transfer (some files failed)]",
		},
		{
			name:    "expr syntax error",
			command: "expr 1 +",
			code:    2,
			want:    "[Exit code: 2 — ERROR: Syntax error in expression]",
		},
		{
			name:    "pgrep invalid pattern",
			command: "pgrep [",
			code:    2,
			want:    "[Exit code: 2 — ERROR: Error: invalid pattern or option]",
		},
		{
			name:    "unparsed command generic error",
			command: "",
			code:    7,
			want:    "[Exit code: 7 — ERROR: Exit code 7]",
		},
	}
	for _, tc := range cases {
		if got := bashpkg.FormatExitCode(tc.command, tc.code); got != tc.want {
			t.Fatalf("%s: FormatExitCode(%q, %d)=%q want %q", tc.name, tc.command, tc.code, got, tc.want)
		}
	}
}

func TestBashRejectsUnsafeSedLikePython(t *testing.T) {
	registry := coretools.NewToolRegistry(coretools.NewBashTool())
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "bash-1", Name: "run_shell",
		Input: map[string]any{"command": "sed '1w /tmp/out' " + path},
	}, coretools.ExecutionContext{"cwd": dir})
	if !strings.Contains(result.Content, "sed command not auto-approved") ||
		!strings.Contains(result.Content, "Write command") {
		t.Fatalf("expected sed rejection, got error=%v content=%q", result.IsError, result.Content)
	}
}

func TestBashPathValidationRejectsOutsideWorkspace(t *testing.T) {
	registry := coretools.NewToolRegistry(coretools.NewBashTool())
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "bash-1", Name: "run_shell",
		Input: map[string]any{"command": "cat " + outside},
	}, coretools.ExecutionContext{"cwd": dir})
	if !strings.Contains(result.Content, "Path validation failed") ||
		!strings.Contains(result.Content, outside) {
		t.Fatalf("expected outside-workspace rejection, got error=%v content=%q", result.IsError, result.Content)
	}
}

func TestBashInterpretsGrepNoMatchAsNonError(t *testing.T) {
	registry := coretools.NewToolRegistry(coretools.NewBashTool())
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "bash-1", Name: "run_shell",
		Input: map[string]any{"command": "grep nomatch " + path, "dangerouslyDisableSandbox": true},
	}, coretools.ExecutionContext{"cwd": dir})
	if !strings.Contains(result.Content, "[Exit code: 1") ||
		!strings.Contains(result.Content, "No match found (not an error)") {
		t.Fatalf("expected grep exit-code semantics, got error=%v content=%q", result.IsError, result.Content)
	}
}

func TestBashExplicitZeroTimeoutMatchesPython(t *testing.T) {
	registry := coretools.NewToolRegistry(coretools.NewBashTool())
	dir := t.TempDir()
	timeout := 0
	start := time.Now()
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "bash-timeout-zero", Name: "run_shell",
		Input: map[string]any{"command": "sleep 1", "timeout": timeout, "dangerouslyDisableSandbox": true},
	}, coretools.ExecutionContext{"cwd": dir})
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("timeout=0 should timeout immediately like Python, took %s", time.Since(start))
	}
	if result.IsError || !strings.Contains(result.Content, "Error: Command timed out after 0.0s.") {
		t.Fatalf("expected Python-style timeout=0 result as tool data, error=%v content=%q", result.IsError, result.Content)
	}
}

func TestBashBackgroundTaskRunsAfterLaunch(t *testing.T) {
	registry := coretools.NewToolRegistry(coretools.NewBashTool())
	dir := t.TempDir()
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "bash-1", Name: "run_shell",
		Input: map[string]any{
			"command":           "sleep 0.1; echo background-done",
			"description":       "background parity test",
			"run_in_background": true,
		},
	}, coretools.ExecutionContext{"cwd": dir})
	if !strings.Contains(result.Content, "Command launched in background.") {
		t.Fatalf("expected background launch, got error=%v content=%q", result.IsError, result.Content)
	}
	outputPath := extractBackgroundOutputPath(result.Content)
	if outputPath == "" {
		t.Fatalf("could not parse output path from %q", result.Content)
	}
	var data []byte
	var err error
	for i := 0; i < 20; i++ {
		data, err = os.ReadFile(outputPath)
		if err == nil && strings.Contains(string(data), "background-done") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("background task did not produce expected output, err=%v content=%q", err, string(data))
}

func TestBashBackgroundDescriptionUsesPythonCharacterSlice(t *testing.T) {
	registry := coretools.NewToolRegistry(coretools.NewBashTool())
	dir := t.TempDir()
	command := strings.Repeat("测", 90)
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "bash-bg-unicode", Name: "run_shell",
		Input: map[string]any{
			"command":           command,
			"run_in_background": true,
		},
	}, coretools.ExecutionContext{"cwd": dir})
	want := "Description: " + strings.Repeat("测", 80)
	if result.IsError || !strings.Contains(result.Content, want) {
		t.Fatalf("background description should truncate by Python characters, got error=%v content=%q", result.IsError, result.Content)
	}
}

func TestBashLargeUnicodeOutputTruncatesWithPythonDecodeReplacement(t *testing.T) {
	registry := coretools.NewToolRegistry(coretools.NewBashTool())
	dir := t.TempDir()
	command := `python3 -c "import sys; sys.stdout.write('测' * ((5*1024*1024)//3+2))"`
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "bash-large-unicode", Name: "run_shell",
		Input: map[string]any{"command": command, "dangerouslyDisableSandbox": true},
	}, coretools.ExecutionContext{"cwd": dir})
	if result.IsError {
		t.Fatalf("large unicode bash output failed: %s", result.Content)
	}
	if !strings.Contains(result.Content, "... [OUTPUT TRUNCATED: 4 bytes removed] ...") {
		t.Fatalf("expected Python byte-count truncation marker, got %q", result.Content)
	}
	if !strings.Contains(result.Content, strings.Repeat("测", 2)+"�\n\n... [OUTPUT TRUNCATED") ||
		!strings.Contains(result.Content, "... [OUTPUT TRUNCATED: 4 bytes removed] ...\n\n�"+strings.Repeat("测", 2)) {
		t.Fatalf("truncated UTF-8 halves should decode with replacement like Python, got %q", result.Content)
	}
}

func extractBackgroundOutputPath(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "Output file: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Output file: "))
		}
	}
	return ""
}
