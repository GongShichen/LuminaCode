package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/config"
	coretools "LuminaCode/tools"
)

func TestTaskToolsSerializePreviewAndRawErrors(t *testing.T) {
	runtime := agent.NewAgentTaskRuntime()
	record := runtime.RegisterForegroundTask("task-1", "", "main", "worker", "desc", "general-purpose")
	record.Status = "completed"
	record.ResultText = strings.Repeat("x", 4100)
	record.TerminationReason = "completed"

	registry := coretools.NewToolRegistry(agent.NewTaskGetTool(), agent.NewTaskStopTool())
	execCtx := coretools.ExecutionContext{"task_runtime": runtime, "scope_id": "main"}
	got := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "get-1", Name: "TaskGet", Input: map[string]any{"task_id": "task-1"},
	}, execCtx)
	if got.IsError {
		t.Fatalf("TaskGet failed: %s", got.Content)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(got.Content), &payload); err != nil {
		t.Fatalf("TaskGet returned invalid JSON: %v\n%s", err, got.Content)
	}
	if payload["result_text_truncated"] != true || int(payload["result_text_chars_total"].(float64)) != 4100 {
		t.Fatalf("expected truncated result payload, got %#v", payload)
	}
	if _, ok := payload["parent_task_id"]; !ok || payload["parent_task_id"] != nil {
		t.Fatalf("task payload should include Python parent_task_id=null, got %#v", payload)
	}
	if len(payload["result_text"].(string)) > 4020 {
		t.Fatalf("result preview too long: %d", len(payload["result_text"].(string)))
	}

	unicodeRecord := runtime.RegisterForegroundTask("task-2", "", "main", "worker", "desc", "general-purpose")
	unicodeRecord.Status = "completed"
	unicodeRecord.ResultText = strings.Repeat("界", 4001)
	unicodeRecord.TerminationReason = "completed"
	unicodeGot := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "get-2", Name: "TaskGet", Input: map[string]any{"task_id": "task-2"},
	}, execCtx)
	if unicodeGot.IsError {
		t.Fatalf("TaskGet unicode failed: %s", unicodeGot.Content)
	}
	var unicodePayload map[string]any
	if err := json.Unmarshal([]byte(unicodeGot.Content), &unicodePayload); err != nil {
		t.Fatalf("TaskGet unicode returned invalid JSON: %v\n%s", err, unicodeGot.Content)
	}
	if unicodePayload["result_text_truncated"] != true || int(unicodePayload["result_text_chars_total"].(float64)) != 4001 {
		t.Fatalf("unicode result payload should use Python character counts, got %#v", unicodePayload)
	}
	if !strings.HasPrefix(unicodePayload["result_text"].(string), strings.Repeat("界", 4000)+"\n...[truncated]") {
		t.Fatalf("unicode result preview should use Python character slicing, got %q", unicodePayload["result_text"])
	}

	missing := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "stop-1", Name: "TaskStop", Input: map[string]any{"task_id": "missing"},
	}, execCtx)
	if missing.IsError || strings.HasPrefix(strings.TrimSpace(missing.Content), "\"") || missing.Content != "Error: Task not found." {
		t.Fatalf("expected raw TaskStop error string, got error=%v content=%q", missing.IsError, missing.Content)
	}
}

func TestTaskListReturnsCreatedAtOrderLikePython(t *testing.T) {
	runtime := agent.NewAgentTaskRuntime()
	second := runtime.RegisterForegroundTask("task-second", "", "main", "worker", "second", "general-purpose")
	time.Sleep(10 * time.Millisecond)
	first := runtime.RegisterForegroundTask("task-first", "", "main", "worker", "first", "general-purpose")
	second.CreatedAt = first.CreatedAt + 1
	second.UpdatedAt = second.CreatedAt

	registry := coretools.NewToolRegistry(agent.NewTaskListTool())
	got := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "list-1", Name: "TaskList", Input: map[string]any{},
	}, coretools.ExecutionContext{"task_runtime": runtime, "scope_id": "main"})
	if got.IsError {
		t.Fatalf("TaskList failed: %s", got.Content)
	}
	var payload struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(got.Content), &payload); err != nil {
		t.Fatalf("TaskList returned invalid JSON: %v\n%s", err, got.Content)
	}
	if len(payload.Tasks) != 2 || payload.Tasks[0]["task_id"] != "task-first" || payload.Tasks[1]["task_id"] != "task-second" {
		t.Fatalf("TaskList should be sorted by created_at like Python, got %s", got.Content)
	}
	snapshot := runtime.ExportSnapshot()
	if len(snapshot) != 2 || snapshot[0]["task_id"] != "task-first" || snapshot[1]["task_id"] != "task-second" {
		t.Fatalf("task snapshot should be stable by created_at, got %#v", snapshot)
	}
}

func TestTaskWaitConsumesMatchingNotificationsLikePython(t *testing.T) {
	runtime := agent.NewAgentTaskRuntime()
	record := runtime.RegisterForegroundTask("task-1", "", "main", "worker", "desc", "general-purpose")
	stopped := runtime.StopTask(record.TaskID, "main")
	stoppedRecord, ok := stopped.(*agent.AgentTaskRecord)
	if !ok || stoppedRecord.Status != "killed" {
		t.Fatalf("expected stopped foreground-like record, got %#v", stopped)
	}

	registry := coretools.NewToolRegistry(agent.NewTaskWaitTool())
	got := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "wait-1", Name: "TaskWait", Input: map[string]any{"task_ids": []string{"task-1"}, "timeout_seconds": 1},
	}, coretools.ExecutionContext{"task_runtime": runtime, "scope_id": "main"})
	if got.IsError {
		t.Fatalf("TaskWait failed: %s", got.Content)
	}
	if notifications := runtime.DrainPendingNotifications("main"); len(notifications) != 0 {
		t.Fatalf("TaskWait should consume matching task notifications like Python, got %#v", notifications)
	}
}

func TestTaskWaitZeroTimeoutReturnsImmediateSoftTimeoutLikePython(t *testing.T) {
	runtime := agent.NewAgentTaskRuntime()
	record := runtime.RegisterForegroundTask("task-1", "", "main", "worker", "desc", "general-purpose")

	result := runtime.WaitForTasks(context.Background(), []string{record.TaskID}, "main", 0)

	if result["timeout"] != true {
		t.Fatalf("timeout_seconds=0 should immediately soft-timeout like Python, got %#v", result)
	}
	pending, _ := result["pending_task_ids"].([]string)
	if len(pending) != 1 || pending[0] != record.TaskID {
		t.Fatalf("expected pending task to be reported, got %#v", result)
	}
	if _, ok := result["error"]; ok {
		t.Fatalf("soft timeout should not be a hard error, got %#v", result)
	}
}

type capturingTaskSink struct {
	events []agent.TaskUIEvent
}

func (s *capturingTaskSink) EmitTaskEvent(event agent.TaskUIEvent) {
	s.events = append(s.events, event)
}

func TestTaskRuntimeEmitsTaskUIEventsLikePython(t *testing.T) {
	runtime := agent.NewAgentTaskRuntime()
	sink := &capturingTaskSink{}
	runtime.SetTaskEventSink(sink)

	record := runtime.RegisterForegroundTask("task-1", "", "main", "worker", "desc", "general-purpose")
	stopped := runtime.StopTask(record.TaskID, "main")
	stoppedRecord, ok := stopped.(*agent.AgentTaskRecord)
	if !ok || stoppedRecord.Status != "killed" {
		t.Fatalf("expected stopped record, got %#v", stopped)
	}
	if len(sink.events) != 1 {
		t.Fatalf("expected one task UI event, got %#v", sink.events)
	}
	event := sink.events[0]
	if event.Type != "task_summary_available" || event.TaskID != "task-1" || event.Summary != "Running worker was stopped." {
		t.Fatalf("unexpected task UI event: %#v", event)
	}
	if event.Record["status"] != "killed" || event.Record["parent_task_id"] != nil || event.ResultText != "" {
		t.Fatalf("task UI event should carry Python-style record and result, got %#v", event)
	}

	runtime.SetTaskEventSink(nil)
	runtime.StopTask(record.TaskID, "main")
	if len(sink.events) != 1 {
		t.Fatalf("nil task sink should stop UI event emission, got %#v", sink.events)
	}
}

func TestForegroundTaskCompletionDoesNotEnqueueNotificationLikePython(t *testing.T) {
	runtime := agent.NewAgentTaskRuntime()
	record := runtime.RegisterForegroundTask("task-1", "", "main", "worker", "desc", "general-purpose")
	runtime.CompleteForegroundTask(record, "completed")
	if got := runtime.GetTask("task-1", "main"); got == nil || got.Status != "completed" || got.TerminationReason != "completed" {
		t.Fatalf("expected foreground task terminal status, got %#v", got)
	}
	if notifications := runtime.DrainPendingNotifications("main"); len(notifications) != 0 {
		t.Fatalf("foreground completion should not enqueue task notifications like Python, got %#v", notifications)
	}
	runtime.DiscardForegroundTask("task-1")
	if got := runtime.GetTask("task-1", "main"); got != nil {
		t.Fatalf("foreground task should be discarded, got %#v", got)
	}
}

func TestReusableWorkerPreservesSessionAcrossMessages(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("invalid request body: %v", err)
		}
		messages, _ := body["messages"].([]any)
		call := atomic.AddInt32(&calls, 1)
		if call == 1 {
			if !requestContainsText(messages, "first prompt") || requestContainsText(messages, "second prompt") {
				t.Fatalf("unexpected first reusable-worker request: %s", mustJSON(body))
			}
			writeOpenAIStream(w, "first done", 3, 4)
			return
		}
		if call == 2 {
			for _, expected := range []string{"first prompt", "first done", "second prompt"} {
				if !requestContainsText(messages, expected) {
					t.Fatalf("second reusable-worker request missing %q: %s", expected, mustJSON(body))
				}
			}
			writeOpenAIStream(w, "second done", 5, 6)
			return
		}
		t.Fatalf("unexpected extra API call %d: %s", call, mustJSON(body))
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 256
	cfg.SessionDir = t.TempDir()
	rt := agent.NewAgentTaskRuntime()
	def := agent.AgentDef{Name: "Explore", Description: "search", MaxTurns: 3}
	state := agent.NewAgentState()
	record := rt.SpawnWorker(context.Background(), cfg, coretools.NewToolRegistry(), def, &state, "desc", "first prompt", "Explore", "", "worker", true, "main", "", coretools.ExecutionContext{})
	waitUntilStatus(t, rt, record.TaskID, "main", "idle")
	if got := rt.SendMessage(record.TaskID, "main", "second prompt"); fmt.Sprint(got) == "Error: Worker is not reusable or has already terminated." {
		t.Fatalf("expected reusable worker to accept follow-up, got %#v", got)
	}
	waitUntilStatus(t, rt, record.TaskID, "main", "idle")
	final := rt.GetTask(record.TaskID, "main")
	if final == nil || final.ResultText != "second done" {
		t.Fatalf("unexpected final record: %#v", final)
	}
	if final.InputTokens != 8 || final.OutputTokens != 10 {
		t.Fatalf("expected token deltas accumulated once, got %d/%d", final.InputTokens, final.OutputTokens)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected exactly two API calls, got %d", calls)
	}
}

type contextCaptureTool struct {
	coretools.BaseTool
	captured chan map[string]any
}

func newContextCaptureTool(captured chan map[string]any) *contextCaptureTool {
	return &contextCaptureTool{
		BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
			Name:            "context_capture",
			Description:     "capture execution context",
			InputPrototype:  struct{}{},
			ReadOnly:        coretools.BoolPtr(true),
			ConcurrencySafe: coretools.BoolPtr(true),
			Destructive:     coretools.BoolPtr(false),
		}},
		captured: captured,
	}
}

func (t *contextCaptureTool) Execute(_ context.Context, execCtx coretools.ExecutionContext, _ any) (string, error) {
	parentState, _ := execCtx["parent_state"].(*agent.AgentState)
	cfg, _ := execCtx["config"].(config.Config)
	yolo := false
	if parentState != nil && parentState.PermissionState != nil {
		yolo = parentState.PermissionState.YoloMode
	}
	payload := map[string]any{
		"scope_id":           execCtx["scope_id"],
		"current_task_id":    execCtx["current_task_id"],
		"parent_scope_id":    execCtx["parent_scope_id"],
		"parent_task_id":     execCtx["parent_task_id"],
		"current_agent_type": execCtx["current_agent_type"],
		"has_task_runtime":   execCtx["task_runtime"] != nil,
		"parent_yolo":        yolo,
		"cwd":                execCtx["cwd"],
		"allowed_read_roots": execCtx["allowed_read_roots"],
		"config_cwd":         cfg.CWD,
	}
	if abortCheck, ok := execCtx["abort_check"].(func() bool); ok {
		payload["abort_check"] = abortCheck()
	}
	t.captured <- payload
	b, _ := json.Marshal(payload)
	return string(b), nil
}

func TestBackgroundWorkerUsesPythonScopedExecutionContext(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := atomic.AddInt32(&calls, 1)
		if call == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tool-1\",\"function\":{\"name\":\"context_capture\",\"arguments\":\"{}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		writeOpenAIStream(w, "done", 2, 3)
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 256
	cfg.SessionDir = t.TempDir()
	rt := agent.NewAgentTaskRuntime()
	parent := agent.NewAgentState()
	captured := make(chan map[string]any, 1)
	def := agent.AgentDef{Name: "Explore", Description: "search", MaxTurns: 3, PermissionMode: "bypass"}
	worktreeCWD := cfg.CWD + "/.Lumina/worktrees/Explore-test"
	record := rt.SpawnWorker(context.Background(), cfg, coretools.NewToolRegistry(newContextCaptureTool(captured)), def, &parent, "desc", "prompt", "Explore", "", "worker", false, "parent-scope", "parent-task", coretools.ExecutionContext{"scope_id": "wrong-parent", "worktree_cwd": worktreeCWD})
	waitUntilStatus(t, rt, record.TaskID, "parent-scope", "completed")

	var payload map[string]any
	select {
	case payload = <-captured:
	default:
		t.Fatal("context_capture tool did not run")
	}
	if payload["scope_id"] != record.TaskID || payload["current_task_id"] != record.TaskID {
		t.Fatalf("worker should execute in its own scope/task, got %#v for record %#v", payload, record)
	}
	if payload["parent_scope_id"] != "parent-scope" || payload["parent_task_id"] != "parent-task" {
		t.Fatalf("worker parent identifiers not propagated like Python: %#v", payload)
	}
	if payload["current_agent_type"] != "Explore" || payload["has_task_runtime"] != true {
		t.Fatalf("worker runtime context missing Python fields: %#v", payload)
	}
	if payload["parent_yolo"] != false {
		t.Fatalf("permission bypass must not manufacture user YOLO state, got %#v", payload)
	}
	if payload["cwd"] != worktreeCWD {
		t.Fatalf("worker execution cwd should come from worktree_cwd, got %#v", payload)
	}
	allowed, _ := payload["allowed_read_roots"].([]string)
	if len(allowed) != 1 || allowed[0] != cfg.CWD || payload["config_cwd"] != cfg.CWD {
		t.Fatalf("worker should preserve original config cwd and read roots, got %#v", payload)
	}
	if parent.PermissionState == nil || parent.PermissionState.YoloMode {
		t.Fatalf("worker permission state should not mutate parent, parent=%#v", parent.PermissionState)
	}
}

func TestBackgroundWorkerOutlivesParentContextLikePython(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tool-1\",\"function\":{\"name\":\"context_capture\",\"arguments\":\"{}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		writeOpenAIStream(w, "done", 1, 1)
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 256
	cfg.SessionDir = t.TempDir()
	rt := agent.NewAgentTaskRuntime()
	parent := agent.NewAgentState()
	captured := make(chan map[string]any, 1)
	def := agent.AgentDef{Name: "Explore", Description: "search", MaxTurns: 3, PermissionMode: "inherit"}
	parentCtx, cancel := context.WithCancel(context.Background())
	record := rt.SpawnWorker(parentCtx, cfg, coretools.NewToolRegistry(newContextCaptureTool(captured)), def, &parent, "desc", "prompt", "Explore", "", "worker", false, "main", "", coretools.ExecutionContext{})
	cancel()
	waitUntilStatus(t, rt, record.TaskID, "main", "completed")

	var payload map[string]any
	select {
	case payload = <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("context_capture tool did not run")
	}
	if payload["abort_check"] != false {
		t.Fatalf("background worker should not inherit parent context cancellation, got %#v", payload)
	}
}

func TestStopTaskMatchesPythonStatusSpecificNotifications(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeOpenAIStream(w, "previous result", 1, 2)
	}))
	defer server.Close()
	cfg := config.NewConfig()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 256
	rt := agent.NewAgentTaskRuntime()
	def := agent.AgentDef{Name: "Explore", Description: "search", MaxTurns: 3}
	state := agent.NewAgentState()
	record := rt.SpawnWorker(context.Background(), cfg, coretools.NewToolRegistry(), def, &state, "desc", "first prompt", "Explore", "", "worker", true, "main", "", coretools.ExecutionContext{})
	waitUntilStatus(t, rt, record.TaskID, "main", "idle")
	rt.DrainPendingNotifications("main")
	stopped := rt.StopTask(record.TaskID, "main")
	stoppedRecord, ok := stopped.(*agent.AgentTaskRecord)
	if !ok || stoppedRecord.Status != "killed" || stoppedRecord.TerminationReason != "stopped" {
		t.Fatalf("unexpected stopped record: %#v", stopped)
	}
	notifications := rt.DrainPendingNotifications("main")
	if len(notifications) != 1 {
		t.Fatalf("expected one stop notification, got %#v", notifications)
	}
	text := notifications[0]["content"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(text, "summary: Idle worker was stopped.") || !strings.Contains(text, "result: previous result") {
		t.Fatalf("unexpected idle stop notification: %q", text)
	}
	metadata := notifications[0]["metadata"].(map[string]any)
	if _, ok := metadata["parent_task_id"]; !ok || metadata["parent_task_id"] != nil {
		t.Fatalf("task notification metadata should include Python parent_task_id=null, got %#v", metadata)
	}

	again := rt.StopTask(record.TaskID, "main")
	againRecord, ok := again.(*agent.AgentTaskRecord)
	if !ok || againRecord.Status != "killed" || againRecord.TerminationReason != "stopped" {
		t.Fatalf("terminal stop should return unchanged terminal record, got %#v", again)
	}
	if extra := rt.DrainPendingNotifications("main"); len(extra) != 0 {
		t.Fatalf("terminal stop should not enqueue duplicate notifications: %#v", extra)
	}
}

func TestShutdownStopsIdleReusableWorkers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeOpenAIStream(w, "idle result", 1, 2)
	}))
	defer server.Close()
	cfg := config.NewConfig()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 256
	rt := agent.NewAgentTaskRuntime()
	def := agent.AgentDef{Name: "Explore", Description: "search", MaxTurns: 3}
	state := agent.NewAgentState()
	record := rt.SpawnWorker(context.Background(), cfg, coretools.NewToolRegistry(), def, &state, "desc", "prompt", "Explore", "", "worker", true, "main", "", coretools.ExecutionContext{})
	waitUntilStatus(t, rt, record.TaskID, "main", "idle")
	rt.Shutdown()
	stopped := rt.GetTask(record.TaskID, "main")
	if stopped == nil || stopped.Status != "killed" || stopped.TerminationReason != "stopped" {
		t.Fatalf("shutdown should stop idle reusable worker, got %#v", stopped)
	}
}

func TestReusableWorkerExpiresAfterIdleTTLLikePython(t *testing.T) {
	oldTTL := agent.IdleTTLDuration
	agent.IdleTTLDuration = 40 * time.Millisecond
	defer func() { agent.IdleTTLDuration = oldTTL }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeOpenAIStream(w, "idle result", 1, 2)
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 256
	rt := agent.NewAgentTaskRuntime()
	def := agent.AgentDef{Name: "Explore", Description: "search", MaxTurns: 3}
	state := agent.NewAgentState()
	record := rt.SpawnWorker(context.Background(), cfg, coretools.NewToolRegistry(), def, &state, "desc", "prompt", "Explore", "", "worker", true, "main", "", coretools.ExecutionContext{})
	waitUntilStatus(t, rt, record.TaskID, "main", "idle")
	rt.DrainPendingNotifications("main")
	waitUntilStatus(t, rt, record.TaskID, "main", "completed")

	expired := rt.GetTask(record.TaskID, "main")
	if expired == nil || expired.TerminationReason != "idle_ttl_expired" || expired.ResultText != "idle result" {
		t.Fatalf("idle reusable worker should expire like Python, got %#v", expired)
	}
	notifications := rt.DrainPendingNotifications("main")
	if len(notifications) != 1 {
		t.Fatalf("expected one idle ttl notification, got %#v", notifications)
	}
	text := notifications[0]["content"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(text, "summary: Reusable worker expired after idling.") || !strings.Contains(text, "result: idle result") {
		t.Fatalf("unexpected idle ttl notification: %q", text)
	}
	if got := rt.SendMessage(record.TaskID, "main", "after ttl"); fmt.Sprint(got) != "Error: Worker is not reusable or has already terminated." {
		t.Fatalf("expired reusable worker should reject follow-up, got %#v", got)
	}
}

func TestNonReusableWorkerTerminalRecordCleanupTTLMatchesPython(t *testing.T) {
	oldTTL := agent.TerminalRecordTTLDuration
	agent.TerminalRecordTTLDuration = 40 * time.Millisecond
	defer func() { agent.TerminalRecordTTLDuration = oldTTL }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeOpenAIStream(w, "done", 1, 2)
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 256
	rt := agent.NewAgentTaskRuntime()
	def := agent.AgentDef{Name: "Explore", Description: "search", MaxTurns: 3}
	state := agent.NewAgentState()
	record := rt.SpawnWorker(context.Background(), cfg, coretools.NewToolRegistry(), def, &state, "desc", "prompt", "Explore", "", "worker", false, "main", "", coretools.ExecutionContext{})
	waitUntilStatus(t, rt, record.TaskID, "main", "completed")
	waitUntilMissing(t, rt, record.TaskID, "main")

	waitResult := rt.WaitForTasks(context.Background(), []string{record.TaskID}, "main", 1)
	missing, _ := waitResult["missing_task_ids"].([]string)
	if len(missing) != 1 || missing[0] != record.TaskID {
		t.Fatalf("cleaned terminal record should be forgotten like Python, got %#v", waitResult)
	}
}

func TestStopRunningWorkerIsNotOverwrittenByCompletion(t *testing.T) {
	toolStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tool-1\",\"function\":{\"name\":\"blocking_tool\",\"arguments\":\"{}\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	cfg := config.NewConfig()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 256
	rt := agent.NewAgentTaskRuntime()
	def := agent.AgentDef{Name: "Explore", Description: "search", MaxTurns: 3}
	state := agent.NewAgentState()
	record := rt.SpawnWorker(context.Background(), cfg, coretools.NewToolRegistry(newBlockingTool(toolStarted)), def, &state, "desc", "prompt", "Explore", "", "worker", false, "main", "", coretools.ExecutionContext{})
	select {
	case <-toolStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking tool did not start")
	}
	stopped := rt.StopTask(record.TaskID, "main")
	stoppedRecord, ok := stopped.(*agent.AgentTaskRecord)
	if !ok || stoppedRecord.Status != "killed" {
		t.Fatalf("expected running worker stopped as killed, got %#v", stopped)
	}
	time.Sleep(100 * time.Millisecond)
	final := rt.GetTask(record.TaskID, "main")
	if final == nil || final.Status != "killed" || final.TerminationReason != "stopped" {
		t.Fatalf("stopped running worker should not be overwritten by goroutine completion, got %#v", final)
	}
}

type blockingTool struct {
	coretools.BaseTool
	started chan struct{}
}

func newBlockingTool(started chan struct{}) *blockingTool {
	return &blockingTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:            "blocking_tool",
		Description:     "blocks until cancelled",
		InputPrototype:  struct{}{},
		ReadOnly:        coretools.BoolPtr(true),
		ConcurrencySafe: coretools.BoolPtr(true),
		Destructive:     coretools.BoolPtr(false),
	}}, started: started}
}

func (t *blockingTool) Execute(ctx context.Context, _ coretools.ExecutionContext, _ any) (string, error) {
	close(t.started)
	<-ctx.Done()
	return "", ctx.Err()
}

func writeOpenAIStream(w http.ResponseWriter, text string, inputTokens, outputTokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"id\":\"msg\",\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", text)
	fmt.Fprintf(w, "data: {\"id\":\"msg\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d}}\n\n", inputTokens, outputTokens)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func requestContainsText(messages []any, needle string) bool {
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		content := message["content"]
		switch blocks := content.(type) {
		case string:
			if strings.Contains(blocks, needle) {
				return true
			}
		case []any:
			for _, rawBlock := range blocks {
				block, _ := rawBlock.(map[string]any)
				if strings.Contains(fmt.Sprint(block["text"]), needle) || strings.Contains(fmt.Sprint(block["content"]), needle) {
					return true
				}
			}
		}
	}
	return false
}

func waitUntilStatus(t *testing.T, rt *agent.AgentTaskRuntime, taskID, scopeID, status string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		record := rt.GetTask(taskID, scopeID)
		if record != nil && record.Status == status {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach status %s; record=%#v", taskID, status, rt.GetTask(taskID, scopeID))
}

func waitUntilMissing(t *testing.T, rt *agent.AgentTaskRuntime, taskID, scopeID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rt.GetTask(taskID, scopeID) == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s was not cleaned up; record=%#v", taskID, rt.GetTask(taskID, scopeID))
}

func mustJSON(v any) string {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(v)
	return strings.TrimSpace(buf.String())
}
