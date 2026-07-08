package test

import (
	"context"
	"strings"
	"testing"

	coretools "LuminaCode/tools"
)

func TestBashToolDirectExecutionDoesNotCancelSimpleCommands(t *testing.T) {
	dir := t.TempDir()
	tool := coretools.NewBashTool()
	execCtx := coretools.ExecutionContext{"cwd": dir}

	echoInput, err := tool.DecodeInput(map[string]any{
		"command":     "echo hello",
		"description": "smoke echo",
	})
	if err != nil {
		t.Fatalf("decode echo input: %v", err)
	}
	echoOut, err := tool.Execute(context.Background(), execCtx, echoInput)
	if err != nil {
		t.Fatalf("execute echo: %v", err)
	}
	if !strings.Contains(echoOut, "[Exit code: 0") || !strings.Contains(echoOut, "hello") {
		t.Fatalf("echo output = %q", echoOut)
	}

	mkdirInput, err := tool.DecodeInput(map[string]any{
		"command":     "mkdir -p todolite/backend",
		"description": "smoke mkdir",
	})
	if err != nil {
		t.Fatalf("decode mkdir input: %v", err)
	}
	mkdirOut, err := tool.Execute(context.Background(), execCtx, mkdirInput)
	if err != nil {
		t.Fatalf("execute mkdir: %v", err)
	}
	if !strings.Contains(mkdirOut, "[Exit code: 0") {
		t.Fatalf("mkdir output = %q", mkdirOut)
	}
}

func TestBashToolDirectExecutionUsesPipefail(t *testing.T) {
	dir := t.TempDir()
	tool := coretools.NewBashTool()
	execCtx := coretools.ExecutionContext{"cwd": dir}

	input, err := tool.DecodeInput(map[string]any{
		"command":     "false | cat",
		"description": "pipefail smoke",
	})
	if err != nil {
		t.Fatalf("decode pipefail input: %v", err)
	}
	out, err := tool.Execute(context.Background(), execCtx, input)
	if err != nil {
		t.Fatalf("execute pipefail: %v", err)
	}
	if strings.Contains(out, "[Exit code: 0") {
		t.Fatalf("pipeline failure should not be masked by the last command: %q", out)
	}
}
