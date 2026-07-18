package bash

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	assistantBlockingBudgetMs = 15000
	maxBackgroundTasks        = 10
	isWindows                 = runtime.GOOS == "windows"
)

type BackgroundTask struct {
	TaskID      string   `json:"task_id"`
	Command     string   `json:"command"`
	Description string   `json:"description"`
	OutputPath  string   `json:"output_path"`
	ErrorPath   string   `json:"error_path"`
	Status      string   `json:"status"`
	ExitCode    *int     `json:"exit_code"`
	StartTime   float64  `json:"start_time"`
	EndTime     *float64 `json:"end_time"`
}

type RunningTask struct {
	Cmd    *exec.Cmd
	Cancel context.CancelFunc
	Done   chan struct{}
}

type BackgroundManager struct {
	tasks      map[string]*BackgroundTask
	running    map[string]*RunningTask
	sessionDir string
	resultDir  string
	mu         sync.Mutex
}

func NewBackgroundManager(sessionDir string) (*BackgroundManager, error) {
	return NewBackgroundManagerForResults(filepath.Join(sessionDir, "tool-results"))
}

func NewBackgroundManagerForResults(resultDir string) (*BackgroundManager, error) {
	if err := os.MkdirAll(resultDir, 0o700); err != nil {
		return nil, err
	}
	return &BackgroundManager{
		tasks:      make(map[string]*BackgroundTask),
		running:    make(map[string]*RunningTask),
		sessionDir: filepath.Dir(resultDir),
		resultDir:  resultDir,
	}, nil
}

func (m *BackgroundManager) activeCountLocked() int {
	count := 0
	for _, task := range m.tasks {
		if task.Status == "running" {
			count++
		}
	}
	return count
}

func (m *BackgroundManager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeCountLocked()
}

func (m *BackgroundManager) CanAccept() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeCountLocked() < maxBackgroundTasks
}

func (m *BackgroundManager) Get(taskID string) *BackgroundTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks[taskID]
}

func (m *BackgroundManager) ListActivate() []*BackgroundTask {
	return m.ListActive()
}

func (m *BackgroundManager) ListActive() []*BackgroundTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*BackgroundTask
	for _, task := range m.tasks {
		if task.Status == "running" {
			result = append(result, task)
		}
	}
	return result
}

func (m *BackgroundManager) ListAll() []*BackgroundTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*BackgroundTask, 0, len(m.tasks))
	for _, task := range m.tasks {
		result = append(result, task)
	}
	return result
}

func (m *BackgroundManager) StartBackground(
	command, description, cwd string,
	timeout time.Duration) (*BackgroundTask, error) {
	return m.StartBackgroundWithOptionalTimeout(command, description, cwd, &timeout)
}

func (m *BackgroundManager) StartBackgroundArgv(
	command, description string, argv []string, cwd string,
	timeout time.Duration) (*BackgroundTask, error) {
	return m.StartBackgroundWithOptionalTimeoutArgv(command, description, argv, cwd, &timeout)
}

func (m *BackgroundManager) StartBackgroundWithOptionalTimeout(
	command, description, cwd string,
	timeout *time.Duration) (*BackgroundTask, error) {
	return m.StartBackgroundWithOptionalTimeoutArgv(command, description, ShellArgv(command, ""), cwd, timeout)
}

func (m *BackgroundManager) StartBackgroundWithOptionalTimeoutArgv(
	command, description string, argv []string, cwd string,
	timeout *time.Duration) (*BackgroundTask, error) {
	if len(argv) == 0 {
		return nil, errors.New("cannot start background task with empty argv")
	}
	m.mu.Lock()
	if m.activeCountLocked() >= maxBackgroundTasks {
		count := m.activeCountLocked()
		m.mu.Unlock()
		return nil, fmt.Errorf("Too many background tasks (%d). Wait for some to complete.", count)
	}
	taskID := "bg-" + randomHex(4)
	for m.tasks[taskID] != nil {
		taskID = "bg-" + randomHex(4)
	}

	outputPath := filepath.Join(m.resultDir, taskID+".txt")
	errPath := filepath.Join(m.resultDir, taskID+".error.txt")
	task := &BackgroundTask{
		TaskID:      taskID,
		Command:     command,
		Description: description,
		OutputPath:  outputPath,
		ErrorPath:   errPath,
		Status:      "running",
		StartTime:   nowSeconds(),
	}
	m.tasks[taskID] = task
	m.mu.Unlock()
	ctx := context.Background()
	var cancel context.CancelFunc
	timeoutValue := time.Duration(0)
	hasTimeout := timeout != nil
	if hasTimeout {
		timeoutValue = *timeout
		ctx, cancel = context.WithTimeout(ctx, timeoutValue)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	done := make(chan struct{})
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if cwd == "" {
		cwd = "."
	}
	cmd.Dir = cwd
	prepareProcessGroup(cmd)
	m.mu.Lock()
	m.running[taskID] = &RunningTask{
		Cmd:    cmd,
		Cancel: cancel,
		Done:   done,
	}
	m.mu.Unlock()
	go m.runTask(ctx, task, cmd, cancel, done, timeoutValue, hasTimeout)
	return task, nil
}

func (m *BackgroundManager) Cancel(taskID string) bool {
	m.mu.Lock()
	task := m.tasks[taskID]
	running := m.running[taskID]
	if task == nil || task.Status != "running" || running == nil {
		m.mu.Unlock()
		return false
	}
	task.Status = "cancelled"
	task.EndTime = float64Ptr(nowSeconds())
	m.mu.Unlock()
	terminateProcessTree(running.Cmd)
	running.Cancel()
	select {
	case <-running.Done:
	case <-time.After(5 * time.Second):
		if running.Cmd.Process != nil {
			_ = running.Cmd.Process.Kill()
		}
	}

	m.mu.Lock()
	delete(m.running, taskID)
	m.mu.Unlock()
	return true
}

func (m *BackgroundManager) WaitFor(taskID string, timeout time.Duration) *BackgroundTask {
	m.mu.Lock()
	task := m.tasks[taskID]
	if task == nil {
		m.mu.Unlock()
		return nil
	}
	if task.Status != "running" {
		m.mu.Unlock()
		return task
	}
	running := m.running[taskID]
	m.mu.Unlock()
	if running == nil {
		return task
	}

	if timeout <= 0 {
		<-running.Done
	} else {
		select {
		case <-running.Done:
		case <-time.After(timeout):
			return nil
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks[taskID]
}

func (m *BackgroundManager) GetOutput(taskID string) *string {
	m.mu.Lock()
	task := m.tasks[taskID]
	m.mu.Unlock()

	if task == nil {
		return nil
	}

	data, err := os.ReadFile(task.OutputPath)
	if err != nil {
		return nil
	}
	return new(string(data))
}

func (m *BackgroundManager) GetError(taskID string) *string {
	m.mu.Lock()
	task := m.tasks[taskID]
	m.mu.Unlock()
	if task == nil {
		return nil
	}
	data, err := os.ReadFile(task.ErrorPath)
	if err != nil {
		return nil
	}

	return new(string(data))
}

func (m *BackgroundManager) runTask(
	ctx context.Context,
	task *BackgroundTask,
	cmd *exec.Cmd,
	cancel context.CancelFunc,
	done chan struct{},
	timeout time.Duration,
	hasTimeout bool) {
	defer close(done)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if hasTimeout && timeout <= 0 {
		timeoutSeconds := formatDurationSeconds(timeout)
		output := fmt.Sprintf("[Command timed out after %ss]\nCommand: %s\n", timeoutSeconds, task.Command)
		_ = os.WriteFile(task.OutputPath, []byte(output), 0o600)
		_ = os.WriteFile(task.ErrorPath, []byte("Timeout after "+timeoutSeconds+"s"), 0o600)
		m.finishTask(task.TaskID, "failed", nil)
		return
	}
	err := cmd.Start()
	if err != nil {
		if hasTimeout && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			timeoutSeconds := formatDurationSeconds(timeout)
			output := fmt.Sprintf("[Command timed out after %ss]\nCommand: %s\n", timeoutSeconds, task.Command)
			_ = os.WriteFile(task.OutputPath, []byte(output), 0o600)
			_ = os.WriteFile(task.ErrorPath, []byte("Timeout after "+timeoutSeconds+"s"), 0o600)
			m.finishTask(task.TaskID, "failed", nil)
			return
		}
		_ = os.WriteFile(task.ErrorPath, []byte("Background task crashed: "+err.Error()), 0o600)
		m.finishTask(task.TaskID, "failed", nil)
		return
	}
	err = cmd.Wait()
	if hasTimeout && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		terminateProcessTree(cmd)
		timeoutSeconds := formatDurationSeconds(timeout)
		output := fmt.Sprintf("[Command timed out after %ss]\nCommand: %s\n", timeoutSeconds, task.Command)
		_ = os.WriteFile(task.OutputPath, []byte(output), 0o600)
		_ = os.WriteFile(task.ErrorPath, []byte("Timeout after "+timeoutSeconds+"s"), 0o600)
		m.finishTask(task.TaskID, "failed", nil)
		return
	}

	if errors.Is(ctx.Err(), context.Canceled) {
		terminateProcessTree(cmd)
		m.finishTask(task.TaskID, "cancelled", nil)
		return
	}
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\n[stderr]\n" + stderr.String()
	}
	output = FormatExitCode(task.Command, exitCode) + "\n\n" + output
	_ = os.WriteFile(task.OutputPath, []byte(output), 0o600)

	m.finishTask(task.TaskID, "completed", &exitCode)
}

func (m *BackgroundManager) finishTask(taskID, status string, exitCode *int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	task := m.tasks[taskID]
	if task == nil {
		return
	}
	if task.Status != "cancelled" {
		task.Status = status
		task.ExitCode = exitCode
		task.EndTime = float64Ptr(nowSeconds())
	}
	delete(m.running, taskID)
}

func AutoBackgroundIfBlocking(startTime time.Time, manager *BackgroundManager, command, description, cmd string) (*BackgroundTask, error) {
	if time.Since(startTime) >= time.Duration(assistantBlockingBudgetMs)*time.Millisecond && manager.CanAccept() {
		return manager.StartBackgroundWithOptionalTimeout(command, description, cmd, nil)
	}
	return nil, nil
}

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	argv := ShellArgv(command, "")
	return exec.CommandContext(ctx, argv[0], argv[1:]...)
}

func prepareProcessGroup(cmd *exec.Cmd) {
	prepareProcessGroupForPlatform(cmd)
}

func PrepareProcessGroup(cmd *exec.Cmd) {
	prepareProcessGroup(cmd)
}

func TerminateProcessTree(cmd *exec.Cmd) {
	terminateProcessTreeForPlatform(cmd)
}

func terminateProcessTree(cmd *exec.Cmd) {
	TerminateProcessTree(cmd)
}

func nowSeconds() float64 {
	return float64(time.Now().UnixNano()) / float64(time.Second)
}

func float64Ptr(value float64) *float64 {
	return &value
}

func formatDurationSeconds(duration time.Duration) string {
	text := strconv.FormatFloat(duration.Seconds(), 'f', -1, 64)
	if !strings.ContainsAny(text, ".eE") {
		text += ".0"
	}
	return text
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func formatBackgroundExitCode(command string, exitCode int) string {
	return FormatExitCode(command, exitCode)
}
