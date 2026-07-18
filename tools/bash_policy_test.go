package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"LuminaCode/config"
	bashpkg "LuminaCode/tools/bash"
)

func TestBashToolFailsClosedWithoutSandboxAndAllowsUserYolo(t *testing.T) {
	tool := NewBashTool()
	tool.sandboxManager = &bashpkg.SandboxManager{}
	registry := NewToolRegistry(tool)
	dir := t.TempDir()
	call := ToolCall{ID: "shell", Name: "run_shell", Input: map[string]any{"command": "echo yolo-ok"}}

	blocked := registry.Execute(context.Background(), call, ExecutionContext{"cwd": dir})
	if !strings.Contains(blocked.Content, "sandbox backend is unavailable") || strings.Contains(blocked.Content, "yolo-ok\n") {
		t.Fatalf("non-YOLO execution should fail closed, got %q", blocked.Content)
	}

	allowed := registry.Execute(context.Background(), call, ExecutionContext{
		"cwd":    dir,
		"config": config.Config{Yolo: true},
	})
	if !strings.Contains(allowed.Content, "yolo-ok") {
		t.Fatalf("explicit YOLO should execute without sandbox, got %q", allowed.Content)
	}
}

func TestToolResultsDirUsesInjectedRuntimeForZeroConfig(t *testing.T) {
	runtimeDir := t.TempDir()
	got := toolResultsDirFromContext(ExecutionContext{
		"config":      config.Config{},
		"runtime_dir": runtimeDir,
	}, t.TempDir())
	want := filepath.Join(runtimeDir, "tool-results", "_legacy")
	if got != want {
		t.Fatalf("tool results dir=%q want %q", got, want)
	}
}

func TestBashBackgroundUsesSandboxArgv(t *testing.T) {
	tool := NewBashTool()
	available, _, _ := tool.SandboxStatus()
	if !available {
		t.Skip("OS sandbox backend unavailable")
	}
	registry := NewToolRegistry(tool)
	dir := t.TempDir()
	runtimeDir := t.TempDir()
	result := registry.Execute(context.Background(), ToolCall{
		ID: "background-shell", Name: "run_shell",
		Input: map[string]any{"command": "env", "run_in_background": true},
	}, ExecutionContext{
		"cwd":                 dir,
		"runtime_dir":         runtimeDir,
		"allowed_read_roots":  []string{dir, runtimeDir},
		"allowed_write_roots": []string{dir, runtimeDir},
	})
	if !strings.Contains(result.Content, "Command launched in background") {
		t.Fatalf("background sandbox launch failed: %q", result.Content)
	}
	var outputPath string
	for _, line := range strings.Split(result.Content, "\n") {
		if strings.HasPrefix(line, "Output file: ") {
			outputPath = strings.TrimSpace(strings.TrimPrefix(line, "Output file: "))
		}
	}
	if outputPath == "" || !strings.HasPrefix(outputPath, filepath.Join(runtimeDir, "tool-results", "_legacy")) {
		t.Fatalf("unexpected background output path: %q", outputPath)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(outputPath)
		if err == nil && strings.Contains(string(data), "HOME="+dir) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	data, err := os.ReadFile(outputPath)
	t.Fatalf("background command did not use sanitized sandbox environment: err=%v output=%q", err, data)
}
