package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"LuminaCode/tools/bash"
)

func TestBackgroundManagerPathsAndNonzeroExitMatchPython(t *testing.T) {
	manager, err := bash.NewBackgroundManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewBackgroundManager error: %v", err)
	}
	task, err := manager.StartBackground("exit 7", "nonzero", "", 2*time.Second)
	if err != nil {
		t.Fatalf("StartBackground error: %v", err)
	}
	if filepath.Base(filepath.Dir(task.OutputPath)) != "tool-results" {
		t.Fatalf("output dir=%q want tool-results", filepath.Base(filepath.Dir(task.OutputPath)))
	}
	if !strings.HasPrefix(task.TaskID, "bg-") || len(task.TaskID) != len("bg-")+8 {
		t.Fatalf("task id=%q want bg- plus 8 hex chars", task.TaskID)
	}
	if !strings.HasSuffix(task.ErrorPath, ".error.txt") {
		t.Fatalf("error path=%q want .error.txt suffix", task.ErrorPath)
	}
	task = manager.WaitFor(task.TaskID, 3*time.Second)
	if task == nil {
		t.Fatalf("task did not complete")
	}
	if task.Status != "completed" {
		t.Fatalf("status=%q want completed", task.Status)
	}
	if task.ExitCode == nil || *task.ExitCode != 7 {
		t.Fatalf("exit code=%v want 7", task.ExitCode)
	}
	if len(manager.ListActive()) != 0 || len(manager.ListActivate()) != 0 {
		t.Fatalf("completed task should not be active")
	}
}

func TestBackgroundTimeoutStatusAndFilesMatchPython(t *testing.T) {
	manager, err := bash.NewBackgroundManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewBackgroundManager error: %v", err)
	}
	task, err := manager.StartBackground("sleep 2", "timeout", "", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("StartBackground error: %v", err)
	}
	task = manager.WaitFor(task.TaskID, 8*time.Second)
	if task == nil {
		t.Fatalf("task did not finish after timeout")
	}
	if task.Status != "failed" {
		t.Fatalf("timeout status=%q want failed", task.Status)
	}
	output, err := os.ReadFile(task.OutputPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.Contains(string(output), "[Command timed out after 0.01s]") {
		t.Fatalf("timeout output=%q", string(output))
	}
	errorText, err := os.ReadFile(task.ErrorPath)
	if err != nil {
		t.Fatalf("read error: %v", err)
	}
	if string(errorText) != "Timeout after 0.01s" {
		t.Fatalf("timeout error=%q want Python format", string(errorText))
	}
}

func TestBackgroundZeroTimeoutAndNoTimeoutAreDistinctLikePython(t *testing.T) {
	manager, err := bash.NewBackgroundManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewBackgroundManager error: %v", err)
	}
	zeroTask, err := manager.StartBackground("sleep 1", "zero", "", 0)
	if err != nil {
		t.Fatalf("StartBackground zero timeout error: %v", err)
	}
	zeroTask = manager.WaitFor(zeroTask.TaskID, 3*time.Second)
	if zeroTask == nil {
		t.Fatalf("zero-timeout task did not finish")
	}
	if zeroTask.Status != "failed" {
		t.Fatalf("zero-timeout status=%q want failed", zeroTask.Status)
	}
	zeroOutput, err := os.ReadFile(zeroTask.OutputPath)
	if err != nil {
		t.Fatalf("read zero-timeout output: %v", err)
	}
	if !strings.Contains(string(zeroOutput), "[Command timed out after 0.0s]") {
		t.Fatalf("zero-timeout output=%q", string(zeroOutput))
	}

	noTimeoutTask, err := manager.StartBackgroundWithOptionalTimeout("echo no-timeout", "none", "", nil)
	if err != nil {
		t.Fatalf("StartBackground nil timeout error: %v", err)
	}
	noTimeoutTask = manager.WaitFor(noTimeoutTask.TaskID, 3*time.Second)
	if noTimeoutTask == nil {
		t.Fatalf("no-timeout task did not finish")
	}
	if noTimeoutTask.Status != "completed" {
		t.Fatalf("no-timeout status=%q want completed", noTimeoutTask.Status)
	}
	noTimeoutOutput, err := os.ReadFile(noTimeoutTask.OutputPath)
	if err != nil {
		t.Fatalf("read no-timeout output: %v", err)
	}
	if !strings.Contains(string(noTimeoutOutput), "no-timeout") {
		t.Fatalf("no-timeout output=%q", string(noTimeoutOutput))
	}
}
