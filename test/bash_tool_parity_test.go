package test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"LuminaCode/config"
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

	if _, exposed := properties["dangerouslyDisableSandbox"]; exposed {
		t.Fatal("dangerouslyDisableSandbox must not be exposed to the model")
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

func TestBashToolRegistryTimeoutFollowsInput(t *testing.T) {
	tool := coretools.NewBashTool()
	timeoutMillis := 60_000
	if got := tool.TimeoutForInput(coretools.BashInput{Command: "sleep 1", Timeout: &timeoutMillis}); got < 60*time.Second || got > 61*time.Second+100*time.Millisecond {
		t.Fatalf("timeout int should set registry hard timeout around 61s, got %s", got)
	}
	decoded, err := tool.DecodeInput(map[string]any{"command": "sleep 1", "timeout": "60"})
	if err != nil {
		t.Fatal(err)
	}
	if got := tool.TimeoutForInput(decoded); got < 60*time.Second || got > 61*time.Second+100*time.Millisecond {
		t.Fatalf("timeout string should set registry hard timeout around 61s, got %s", got)
	}
	if got := tool.TimeoutForInput(coretools.BashInput{Command: "sleep 1", TimeoutSeconds: "45"}); got < 45*time.Second || got > 46*time.Second+100*time.Millisecond {
		t.Fatalf("timeout_seconds should set registry hard timeout around 46s, got %s", got)
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

func TestBashPathValidationAllowsRuntimeRoot(t *testing.T) {
	registry := coretools.NewToolRegistry(coretools.NewBashTool())
	dir := t.TempDir()
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	target := filepath.Join(runtimeDir, "nested")
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "bash-1", Name: "run_shell",
		Input: map[string]any{
			"command":                   "mkdir -p " + shellPath(target),
			"dangerouslyDisableSandbox": true,
		},
	}, yoloBashContext(dir, map[string]any{
		"cwd":                 dir,
		"allowed_read_roots":  []string{dir, runtimeDir},
		"allowed_write_roots": []string{dir, runtimeDir},
	}))
	if !strings.Contains(result.Content, "Success (exit 0)") {
		t.Fatalf("expected runtime root command to pass, got error=%v content=%q", result.IsError, result.Content)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("runtime target was not created: %v", err)
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
		Input: map[string]any{"command": "grep nomatch " + shellPath(path), "dangerouslyDisableSandbox": true},
	}, yoloBashContext(dir, nil))
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
	}, yoloBashContext(dir, nil))
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
	}, yoloBashContext(dir, nil))
	if !strings.Contains(result.Content, "Command launched in background.") {
		t.Fatalf("expected background launch, got error=%v content=%q", result.IsError, result.Content)
	}
	outputPath := extractBackgroundOutputPath(result.Content)
	if outputPath == "" {
		t.Fatalf("could not parse output path from %q", result.Content)
	}
	wantPrefix := filepath.Join(config.ProjectRuntimeDir(dir), "background", "tool-results") + string(os.PathSeparator)
	if !strings.HasPrefix(outputPath, wantPrefix) {
		t.Fatalf("background output should stay under project runtime tool-results, got %q want prefix %q", outputPath, wantPrefix)
	}
	if _, err := os.Stat(filepath.Join(dir, ".lumina")); !os.IsNotExist(err) {
		t.Fatalf("background output should not create workspace .lumina, stat err=%v", err)
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
	}, yoloBashContext(dir, nil))
	want := "Description: " + strings.Repeat("测", 80)
	if result.IsError || !strings.Contains(result.Content, want) {
		t.Fatalf("background description should truncate by Python characters, got error=%v content=%q", result.IsError, result.Content)
	}
}

func TestBashLargeUnicodeOutputTruncatesWithPythonDecodeReplacement(t *testing.T) {
	registry := coretools.NewToolRegistry(coretools.NewBashTool())
	dir := t.TempDir()
	command := `PYTHONIOENCODING=utf-8 ` + pythonCommandName() + ` -c "import sys; sys.stdout.write('\u6d4b' * ((5*1024*1024)//3+2))"`
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "bash-large-unicode", Name: "run_shell",
		Input: map[string]any{"command": command, "dangerouslyDisableSandbox": true},
	}, yoloBashContext(dir, nil))
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

func yoloBashContext(cwd string, extra map[string]any) coretools.ExecutionContext {
	ctx := coretools.ExecutionContext{"cwd": cwd, "config": config.Config{Yolo: true}}
	for key, value := range extra {
		ctx[key] = value
	}
	return ctx
}
