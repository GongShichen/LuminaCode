package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"LuminaCode/agent"
	"LuminaCode/agentContext"
	"LuminaCode/config"
	"LuminaCode/memory"
	"LuminaCode/security"
	coretools "LuminaCode/tools"
)

type largeReadInput struct {
	Content string `json:"content"`
}

type largeReadTool struct {
	coretools.BaseTool
}

func TestStripTransientContextMessagesRemovesSkillMemoryAndTaskContext(t *testing.T) {
	messages := []map[string]any{
		{"role": "user", "content": "real user"},
		{"role": "user", "content": "skill", "metadata": map[string]any{"source": "skill_listing"}},
		memory.BuildMetaUserMessage("memory index", "memory_index"),
		memory.BuildMetaUserMessage("memory recall", memory.MemoryRecallSource),
		{"role": "user", "content": "task done", "metadata": map[string]any{"source": "task_notification"}},
		{"role": "assistant", "content": "real assistant"},
	}

	got := agent.StripTransientContextMessages(messages)
	if len(got) != 2 {
		t.Fatalf("expected only real conversation messages, got %#v", got)
	}
	if got[0]["content"] != "real user" || got[1]["content"] != "real assistant" {
		t.Fatalf("unexpected kept messages: %#v", got)
	}
}

func newLargeReadTool() *largeReadTool {
	return &largeReadTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:            "large_read",
		Description:     "large read",
		InputPrototype:  largeReadInput{},
		ReadOnly:        coretools.BoolPtr(true),
		ConcurrencySafe: coretools.BoolPtr(true),
		Destructive:     coretools.BoolPtr(false),
		MaxOutputChars:  100_000,
	}}}
}

func TestInvalidMaxTokensAPIErrorIsNotPromptTooLong(t *testing.T) {
	msg := `DeepSeek API error 400: {"error":{"message":"Invalid max_tokens value, the valid range of max_tokens is [1, 393216]","type":"invalid_request_error"}}`
	if agent.IsPTLErrorMessage(msg) {
		t.Fatal("invalid max_tokens config error should not be treated as prompt-too-long")
	}
}

func TestAgentErrorEventPreservesAPIStatusMetadata(t *testing.T) {
	cfg := config.NewConfig()
	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	turn := &agent.ModelTurn{}
	action := engine.HandleStreamEvent(map[string]any{
		"type":        "error",
		"message":     `DeepSeek API error 400 400 Bad Request: {"error":"bad"}`,
		"status_code": 400,
		"raw_error":   `{"error":"bad"}`,
	}, turn, nil, &state, 0)
	if action.Event == nil || action.Event.Type != "error" {
		t.Fatalf("expected error event, got %#v", action)
	}
	if action.Event.Metadata["status_code"] != 400 || action.Event.Metadata["raw_error"] != `{"error":"bad"}` {
		t.Fatalf("agent wrapper should preserve API status metadata, got %#v", action.Event.Metadata)
	}
	if action.Event.Content != `DeepSeek API error 400 400 Bad Request: {"error":"bad"}` {
		t.Fatalf("agent wrapper should preserve raw message, got %q", action.Event.Content)
	}
}

func (t *largeReadTool) Execute(_ context.Context, _ coretools.ExecutionContext, input any) (string, error) {
	switch v := input.(type) {
	case *largeReadInput:
		return v.Content, nil
	case largeReadInput:
		return v.Content, nil
	default:
		return "", nil
	}
}

func (t *largeReadTool) FormatLargeResult(_ context.Context, content string, _ int, _, _ string) (string, error) {
	return content, nil
}

type invalidRunShellTool struct {
	coretools.BaseTool
}

func stringFromAnyForTest(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func streamContentsOfType(events []agent.StreamEvent, eventType string) []string {
	var contents []string
	for _, event := range events {
		if event.Type == eventType {
			contents = append(contents, event.Content)
		}
	}
	return contents
}

func newInvalidRunShellTool() *invalidRunShellTool {
	return &invalidRunShellTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:            "run_shell",
		Description:     "invalid run shell",
		InputPrototype:  map[string]any{},
		ReadOnly:        coretools.BoolPtr(false),
		ConcurrencySafe: coretools.BoolPtr(false),
		Destructive:     coretools.BoolPtr(true),
	}}}
}

func (t *invalidRunShellTool) DecodeInput(map[string]any) (any, error) {
	return nil, errors.New("invalid")
}

func TestOutputRecoveryEscalatesThenContinuation(t *testing.T) {
	state := agent.NewAgentState()
	recovery := agent.NewOutputRecoveryState(32)

	first := agent.HandleOutputTruncation(&state, &recovery, nil, nil, "partial", "msg-1", 5, 6)
	if first.Action != "retry" {
		t.Fatalf("expected retry, got %s", first.Action)
	}
	if recovery.CurrentMaxTokens != agent.EscalatedMaxTokens {
		t.Fatalf("expected escalated max tokens, got %d", recovery.CurrentMaxTokens)
	}

	second := agent.HandleOutputTruncation(&state, &recovery, nil, nil, "partial", "msg-2", 7, 8)
	if second.Action != "continue" {
		t.Fatalf("expected continue, got %s", second.Action)
	}
	if len(state.Messages) != 2 {
		t.Fatalf("expected assistant plus continuation prompt, got %d messages", len(state.Messages))
	}
}

func TestCoreQueryLoopOutputTokenEscalationOmitsRequestMaxTokens(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("invalid request JSON: %v body=%s", err, raw)
		}
		if _, ok := body["max_tokens"]; ok {
			t.Fatalf("request should omit max_tokens, got %#v", body)
		}
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			fmt.Fprintln(w, `data: {"id":"msg-1","choices":[{"delta":{"content":"partial"},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":6}}`)
			fmt.Fprintln(w)
			fmt.Fprintln(w, `data: [DONE]`)
			return
		}
		fmt.Fprintln(w, `data: {"id":"msg-2","choices":[{"delta":{"content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":8}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "custom-router-model"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 32
	cfg.MCPEnabled = false
	cfg.SkillsEnabled = false
	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	state.SystemPrompt = "system"
	state.Messages = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "hello"}}}}

	var events []agent.StreamEvent
	for event := range engine.QueryLoop(context.Background(), &state) {
		events = append(events, event)
	}
	if len(events) == 0 || events[len(events)-1].Type != "done" {
		t.Fatalf("expected final done event, got %#v", events)
	}
	if calls.Load() < 2 {
		t.Fatalf("expected truncation recovery to issue another request, got %d calls", calls.Load())
	}
	if state.LastContinueReason != string(agent.ContinueReasonMaxOutputTokensEscalate) {
		t.Fatalf("expected escalation continue reason, got %q", state.LastContinueReason)
	}
}

func TestCoreQueryLoopTruncatedToolUseExecutesBeforeNextModelTurnLikePython(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(target, []byte("hello from test"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolInput, err := json.Marshal(map[string]string{"file_path": target})
	if err != nil {
		t.Fatal(err)
	}

	var secondBody map[string]any
	var streamCalls atomic.Int32
	writeSSE := func(w http.ResponseWriter, event map[string]any) {
		payload, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(w, "data: %s\n\n", payload)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("invalid request JSON: %v body=%s", err, raw)
		}
		if stream, _ := body["stream"].(bool); !stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"content":[{"type":"text","text":"[]"}]}`)
			return
		}
		if _, ok := body["max_tokens"]; ok {
			t.Fatalf("stream request should omit max_tokens, got %#v", body)
		}

		call := streamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			writeSSE(w, map[string]any{
				"type":    "message_start",
				"message": map[string]any{"id": "msg-read", "usage": map[string]any{"input_tokens": 7}},
			})
			writeSSE(w, map[string]any{
				"type":          "content_block_start",
				"index":         0,
				"content_block": map[string]any{"type": "tool_use", "id": "tool-read-1", "name": "read_file"},
			})
			writeSSE(w, map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": string(toolInput)},
			})
			writeSSE(w, map[string]any{"type": "content_block_stop", "index": 0})
			writeSSE(w, map[string]any{
				"type":  "message_delta",
				"usage": map[string]any{"output_tokens": 4},
				"delta": map[string]any{"stop_reason": "max_tokens"},
			})
			return
		}

		secondBody = body
		writeSSE(w, map[string]any{
			"type":    "message_start",
			"message": map[string]any{"id": "msg-final", "usage": map[string]any{"input_tokens": 3}},
		})
		writeSSE(w, map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "text_delta", "text": "read complete"},
		})
		writeSSE(w, map[string]any{
			"type":  "message_delta",
			"usage": map[string]any{"output_tokens": 2},
			"delta": map[string]any{"stop_reason": "end_turn"},
		})
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.CWD = dir
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "claude-sonnet-4"
	cfg.APIType = "anthropic"
	cfg.APIMaxTokens = 256
	cfg.MCPEnabled = false
	cfg.SkillsEnabled = false
	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	state.SystemPrompt = "system"
	state.Messages = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "read file"}}}}

	var events []agent.StreamEvent
	for event := range engine.QueryLoop(context.Background(), &state) {
		events = append(events, event)
	}

	if streamCalls.Load() != 2 {
		t.Fatalf("expected two main model stream calls, got %d events=%#v", streamCalls.Load(), events)
	}
	foundResult := false
	for _, event := range events {
		if event.Type == "tool_result" &&
			event.Metadata["tool_name"] == "read_file" &&
			event.Metadata["is_error"] != true {
			foundResult = true
		}
	}
	if !foundResult {
		t.Fatalf("expected read_file tool_result before second model turn, events=%#v", events)
	}
	secondMessages, _ := json.Marshal(secondBody["messages"])
	if !strings.Contains(string(secondMessages), `"tool_result"`) || !strings.Contains(string(secondMessages), `"tool-read-1"`) {
		t.Fatalf("second model call should include tool_result for tool-read-1, messages=%s", secondMessages)
	}
	if len(events) == 0 || events[len(events)-1].Type != "done" {
		t.Fatalf("expected final done event, got %#v", events)
	}
}

func TestContinueReasonsMatchPythonEnum(t *testing.T) {
	got := []agent.ContinueReason{
		agent.ContinueReasonNextTurn,
		agent.ContinueReasonCollapseDrainRetry,
		agent.ContinueReasonReactiveCompactRetry,
		agent.ContinueReasonMaxOutputTokensEscalate,
		agent.ContinueReasonMaxOutputTokensRecovery,
	}
	want := []string{
		"next_turn",
		"collapse_drain_retry",
		"reactive_compact_retry",
		"max_output_tokens_escalate",
		"max_output_tokens_recovery",
	}
	if len(got) != len(want) {
		t.Fatalf("reason count mismatch: got %d want %d", len(got), len(want))
	}
	for i := range got {
		if string(got[i]) != want[i] {
			t.Fatalf("reason[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func TestTruncateResultMatchesPythonHeadTailAndCharacterCount(t *testing.T) {
	content := "你好abcdef再见"
	got := agent.TruncateResult(content, 6)
	want := "你好a\n\n... [4 characters truncated] ...\n\nf再见"
	if got != want {
		t.Fatalf("unexpected truncation:\nwant %q\n got %q", want, got)
	}
	if agent.TruncateResult("short", 6) != "short" {
		t.Fatal("content below limit should be unchanged")
	}
	zero := agent.TruncateResult("abcd", 0)
	if zero != "\n\n... [4 characters truncated] ...\n\nabcd" {
		t.Fatalf("max_chars=0 should mirror Python slicing, got %q", zero)
	}
}

func TestFatalAPIErrorKeywordsMatchPythonLoop(t *testing.T) {
	fatalMessages := []string{
		"404",
		"model_not_found",
		"model not found",
		"invalid_api_key",
		"invalid api key",
		"authentication failed",
		"authorization failed",
		"not_found",
		"not found",
	}
	for _, msg := range fatalMessages {
		if !agent.IsFatalAPIError(msg) {
			t.Fatalf("%q should be fatal like Python loop.py", msg)
		}
	}

	nonFatalMessages := []string{
		"temporary forbidden by upstream",
		"billing quota temporarily unavailable",
		"insufficient_quota",
	}
	for _, msg := range nonFatalMessages {
		if agent.IsFatalAPIError(msg) {
			t.Fatalf("%q should not be fatal in the agent loop Python keyword set", msg)
		}
	}
}

func TestCoreQueryLoopFatalStreamErrorEventDoesNotRetryLikePython(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.APIType = "openai_compatible"
	cfg.APIBaseURL = server.URL
	cfg.APIKey = "test-key"
	cfg.APIModel = "custom-router-model"
	cfg.APIMaxTokens = 256
	cfg.MCPEnabled = false
	cfg.SkillsEnabled = false
	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	state.SystemPrompt = "system"
	state.Messages = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "hello"}}}}

	var events []agent.StreamEvent
	for event := range engine.QueryLoop(context.Background(), &state) {
		events = append(events, event)
	}
	if calls.Load() != 1 {
		t.Fatalf("fatal stream error should not consume retry budget like Python, calls=%d events=%#v", calls.Load(), events)
	}
	errorText := strings.Join(streamContentsOfType(events, "error"), "\n")
	if !strings.Contains(errorText, "model not found") || strings.Contains(errorText, "recovered") {
		t.Fatalf("fatal error should be visible without retry label, got %q events=%#v", errorText, events)
	}
}

func TestCoreQueryLoopNonFatalStreamErrorWithoutToolCallsRetriesUntilSuccessLikePython(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			http.Error(w, "API overloaded - please retry", http.StatusBadRequest)
			return
		}
		writeOpenAIStream(w, "recovered successfully", 2, 3)
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.APIType = "openai_compatible"
	cfg.APIBaseURL = server.URL
	cfg.APIKey = "test-key"
	cfg.APIModel = "custom-router-model"
	cfg.APIMaxTokens = 256
	cfg.MCPEnabled = false
	cfg.SkillsEnabled = false
	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	state.SystemPrompt = "system"
	state.Messages = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "hello"}}}}

	var events []agent.StreamEvent
	for event := range engine.QueryLoop(context.Background(), &state) {
		events = append(events, event)
	}
	if calls.Load() != 2 {
		t.Fatalf("non-fatal stream error should retry once then succeed like Python, calls=%d events=%#v", calls.Load(), events)
	}
	if !strings.Contains(strings.Join(streamContentsOfType(events, "text"), ""), "recovered successfully") {
		t.Fatalf("retry response should appear in output, events=%#v", events)
	}
	if len(streamContentsOfType(events, "error")) == 0 || events[len(events)-1].Type != "done" {
		t.Fatalf("expected visible error followed by done, events=%#v", events)
	}
}

func TestCoreQueryLoopParentTurnLimitUsesConfigValueLikePython(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tool-read\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"file_path\\\":\\\"missing.txt\\\"}\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.APIType = "openai_compatible"
	cfg.APIBaseURL = server.URL
	cfg.APIKey = "test-key"
	cfg.APIModel = "custom-router-model"
	cfg.APIMaxTokens = 256
	cfg.MaxParentTurns = 1
	cfg.MCPEnabled = false
	cfg.SkillsEnabled = false
	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	state.SystemPrompt = "system"
	state.Messages = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "loop"}}}}

	var events []agent.StreamEvent
	for event := range engine.QueryLoop(context.Background(), &state) {
		events = append(events, event)
	}
	errorText := strings.Join(streamContentsOfType(events, "error"), "\n")
	if calls.Load() != 1 || !strings.Contains(errorText, "Reached maximum turns (1)") {
		t.Fatalf("parent loop limit should use config value like Python, calls=%d error=%q events=%#v", calls.Load(), errorText, events)
	}
}

func TestPTLCollapseDrainUsesContextCollapsePipeline(t *testing.T) {
	state := agent.NewAgentState()
	for i := 0; i < 8; i++ {
		state.Messages = append(state.Messages,
			map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "user message with enough detail to summarize."}}},
			map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "assistant answer with enough detail to summarize."}}},
		)
	}
	state.CacheBreakPoints.Add(1)
	manager := agent.PTLRecoveryManager{Config: config.NewConfig(), Regions: []any{"old"}}
	if !manager.TryCollapseDrain(&state) {
		t.Fatal("expected collapse drain to shorten messages")
	}
	if state.LastContinueReason != string(agent.ContinueReasonCollapseDrainRetry) {
		t.Fatalf("unexpected continue reason: %q", state.LastContinueReason)
	}
	if state.CacheBreakPoints.Cardinality() != 0 || manager.Regions != nil {
		t.Fatalf("expected PTL collapse to clear runtime state")
	}
	firstText := state.Messages[0]["content"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(firstText, "[Earlier conversation") || strings.Contains(firstText, "[Collapsed earlier conversation]") {
		t.Fatalf("expected real context collapse projection, got %q", firstText)
	}
}

func TestPTLReactiveCompactUsesContextPipeline(t *testing.T) {
	cfg := config.NewConfig()
	cfg.APIMaxTokens = 1000
	state := agent.NewAgentState()
	state.CacheBreakPoints.Add(2)
	for i := 0; i < 18; i++ {
		state.Messages = append(state.Messages,
			map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": strings.Repeat("user context sentence. ", 20)}}},
			map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": strings.Repeat("assistant context sentence. ", 20)}}},
		)
	}
	originalLen := len(state.Messages)
	manager := agent.PTLRecoveryManager{Config: cfg, Regions: []any{"old"}}
	if !manager.ReactiveCompact(&state) {
		t.Fatal("expected reactive compact to reach L3 or above")
	}
	if state.LastContinueReason != string(agent.ContinueReasonReactiveCompactRetry) {
		t.Fatalf("unexpected continue reason: %q", state.LastContinueReason)
	}
	if state.CacheBreakPoints.Cardinality() != 0 || manager.Regions != nil {
		t.Fatalf("expected reactive compact to clear runtime state")
	}
	firstText := state.Messages[0]["content"].([]map[string]any)[0]["text"].(string)
	if strings.Contains(firstText, "[Earlier conversation") || len(state.Messages) != originalLen {
		t.Fatalf("reactive compact should persist Python pipeline output without projecting L3 regions, len=%d text=%q", len(state.Messages), firstText)
	}
}

func TestCheckPermissionChainValidatesBeforeCommandRule(t *testing.T) {
	cfg := config.NewConfig()
	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	state.PermissionState.ConfirmCommandPrefix("go test")
	needsPermission := engine.CheckPermissionChain(coretools.ToolCall{
		ID: "shell-1", Name: "run_shell", Input: map[string]any{"command": "go test ./..."},
	}, newInvalidRunShellTool(), &state)
	if !needsPermission {
		t.Fatal("invalid input should require permission instead of being auto-approved by command prefix")
	}
}

func TestPermissionResolverUsesBashClassifierForSensitiveReads(t *testing.T) {
	dir := t.TempDir()
	registry := coretools.NewToolRegistry(coretools.NewBashTool())
	cfg := config.NewConfig()
	cfg.CWD = dir
	state := agent.NewAgentState()
	executor := agent.NewStreamingToolExecutor(registry, cfg, &state, coretools.ExecutionContext{"cwd": dir})
	call := coretools.ToolCall{
		ID:    "bash-sensitive",
		Name:  "run_shell",
		Input: map[string]any{"command": "cat /etc/shadow"},
	}
	executor.AddTool(call)

	prompted := false
	resolver := agent.PermissionResolver{
		Registry: registry,
		CheckPermission: func(coretools.ToolCall, coretools.Tool, *agent.AgentState) bool {
			return true
		},
		RequestDecision: func(context.Context, agent.StreamEvent) (string, string) {
			prompted = true
			return agent.PermissionDeny, ""
		},
	}
	events := resolver.Resolve(context.Background(), []coretools.ToolCall{call}, executor, &state, func() bool { return false })
	if !prompted {
		t.Fatal("expected sensitive read to require permission instead of being auto-approved")
	}
	if len(events) != 1 || events[0].Type != "tool_result" || !strings.Contains(events[0].Content, "User denied this action") {
		t.Fatalf("expected denied tool_result event, got %#v", events)
	}
}

func TestPermissionResolverDangerousEventExposesRiskFlagsLikePython(t *testing.T) {
	registry := coretools.NewToolRegistry(coretools.NewBashTool())
	cfg := config.NewConfig()
	state := agent.NewAgentState()
	executor := agent.NewStreamingToolExecutor(registry, cfg, &state, coretools.ExecutionContext{})
	call := coretools.ToolCall{ID: "bash-danger", Name: "run_shell", Input: map[string]any{"command": "sudo rm -rf /tmp/lumina-risk-test"}}
	executor.AddTool(call)

	var permissionEvent agent.StreamEvent
	resolver := agent.PermissionResolver{
		Registry:        registry,
		CheckPermission: func(coretools.ToolCall, coretools.Tool, *agent.AgentState) bool { return true },
		RequestDecision: func(_ context.Context, event agent.StreamEvent) (string, string) {
			permissionEvent = event
			return agent.PermissionDeny, ""
		},
	}
	events := resolver.Resolve(context.Background(), []coretools.ToolCall{call}, executor, &state, func() bool { return false })
	if permissionEvent.Type != "permission_needed" ||
		permissionEvent.Metadata["risk"] != "high" ||
		permissionEvent.Metadata["dangerous"] != true {
		t.Fatalf("dangerous permission event should expose Python risk flags, got %#v", permissionEvent)
	}
	if len(events) != 1 || events[0].Type != "tool_result" || events[0].Metadata["denied"] != true {
		t.Fatalf("denied dangerous command should emit denied tool_result like Python, got %#v", events)
	}
}

func TestPermissionResolverAlwaysGrantSkipsEmptyShellPrefixLikePython(t *testing.T) {
	registry := coretools.NewToolRegistry(coretools.NewBashTool())
	cfg := config.NewConfig()
	state := agent.NewAgentState()
	executor := agent.NewStreamingToolExecutor(registry, cfg, &state, coretools.ExecutionContext{})
	call := coretools.ToolCall{ID: "bash-empty-prefix", Name: "run_shell", Input: map[string]any{"command": "rm"}}
	executor.AddTool(call)

	resolver := agent.PermissionResolver{
		Registry:        registry,
		CheckPermission: func(coretools.ToolCall, coretools.Tool, *agent.AgentState) bool { return true },
		RequestDecision: func(context.Context, agent.StreamEvent) (string, string) {
			return agent.PermissionAlways, ""
		},
	}
	events := resolver.Resolve(context.Background(), []coretools.ToolCall{call}, executor, &state, func() bool { return false })
	if len(events) != 0 {
		t.Fatalf("always grant should start the tool without denial events, got %#v", events)
	}
	if state.PermissionState.ConfirmedCommandRules.Cardinality() != 0 {
		t.Fatalf("empty shell prefix should not persist a command rule, got %#v", state.PermissionState.ConfirmedCommandRules.ToSlice())
	}
}

func TestRepairOrphanToolsPairsAssistantUses(t *testing.T) {
	messages := []map[string]any{
		{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "id": "tool-1", "name": "read_file", "input": map[string]any{"file_path": "a.go"}},
		}},
	}
	repaired := agent.RepairOrphanTools(messages)
	if len(repaired) != 2 {
		t.Fatalf("expected synthetic tool result message, got %d messages", len(repaired))
	}
	blocks, ok := repaired[1]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("unexpected synthetic result content type: %#v", repaired[1]["content"])
	}
	if len(blocks) != 1 || blocks[0]["tool_use_id"] != "tool-1" {
		t.Fatalf("unexpected synthetic result: %#v", blocks)
	}
	if blocks[0]["content"] != "[System: tool execution interrupted - no result available]" {
		t.Fatalf("synthetic result content differs from Python: %#v", blocks[0])
	}
}

func TestRepairOrphanToolsDropsOrphanResultsLikePython(t *testing.T) {
	messages := []map[string]any{
		{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "missing-use", "content": "orphan"},
			{"type": "text", "text": "keep me"},
		}},
	}
	repaired := agent.RepairOrphanTools(messages)
	if len(repaired) != 1 {
		t.Fatalf("expected one repaired message, got %#v", repaired)
	}
	blocks := repaired[0]["content"].([]map[string]any)
	if len(blocks) != 1 || blocks[0]["type"] != "text" || blocks[0]["text"] != "keep me" {
		t.Fatalf("expected orphan result to be dropped without synthetic unknown tool_use, got %#v", blocks)
	}
}

func TestStreamingToolExecutorFIFOAggregateBudget(t *testing.T) {
	registry := coretools.NewToolRegistry(newLargeReadTool())
	cfg := config.NewConfig()
	cfg.MaxMessageToolResultsChars = 120
	cfg.SessionDir = t.TempDir()
	state := agent.NewAgentState()
	executor := agent.NewStreamingToolExecutor(registry, cfg, &state, coretools.ExecutionContext{})
	executor.AddTool(coretools.ToolCall{ID: "tool-1", Name: "large_read", Input: map[string]any{"content": strings.Repeat("a", 100)}})
	executor.AddTool(coretools.ToolCall{ID: "tool-2", Name: "large_read", Input: map[string]any{"content": strings.Repeat("b", 100)}})

	results := executor.GetRemainingResults(context.Background())
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0]["tool_use_id"] != "tool-1" || results[1]["tool_use_id"] != "tool-2" {
		t.Fatalf("results not FIFO: %#v", results)
	}
	total := 0
	for _, result := range results {
		total += len(result["content"].(string))
	}
	if total <= 120 {
		return
	}
	if !strings.Contains(results[0]["content"].(string), "aggregate tool-result budget") {
		t.Fatalf("expected aggregate truncation, total=%d results=%#v", total, results)
	}
}

func TestDeniedToolResultContentEscalates(t *testing.T) {
	state := agent.NewAgentState()
	call := coretools.ToolCall{ID: "tool-1", Name: "write_file", Input: map[string]any{}}
	agent.DeniedToolResultContent(&state, call)
	agent.DeniedToolResultContent(&state, call)
	third := agent.DeniedToolResultContent(&state, call)
	if !strings.Contains(third, "DO NOT request it again") {
		t.Fatalf("expected anti-loop hint, got %q", third)
	}
}

func TestAgentToolValidateInputTrimsWhitespaceLikePython(t *testing.T) {
	tool := agent.NewAgentTool()
	if ok, msg := tool.ValidateInput(coretools.ExecutionContext{}, agent.AgentInput{Description: "   ", Prompt: "do it"}); ok || msg != "description must not be empty." {
		t.Fatalf("expected trimmed description validation, ok=%v msg=%q", ok, msg)
	}
	if ok, msg := tool.ValidateInput(coretools.ExecutionContext{}, agent.AgentInput{Description: "desc", Prompt: "\t\n"}); ok || msg != "prompt must not be empty." {
		t.Fatalf("expected trimmed prompt validation, ok=%v msg=%q", ok, msg)
	}
}

func TestTaskRuntimeStopUnknown(t *testing.T) {
	rt := agent.NewAgentTaskRuntime()
	result := rt.StopTask("missing", "main")
	if result != "Error: Task not found." {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestCoreQueryLoopAutoCompressionUsesAPIMaxTokensEightyPercentTrigger(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
			t.Fatalf("invalid request body: %v body=%s", err, bodyBytes)
		}
		writeOpenAIStream(w, "ok", 1, 1)
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.APIType = "openai_compatible"
	cfg.APIBaseURL = server.URL
	cfg.APIKey = "test-key"
	cfg.APIModel = "custom-router-model"
	cfg.APIMaxTokens = 1000
	cfg.MCPEnabled = false
	cfg.SkillsEnabled = false
	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	state.SystemPrompt = "system"
	state.Messages = []map[string]any{
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "tool-old", "name": "run_shell", "input": map[string]any{"command": "old"}}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "tool-old", "content": strings.Repeat("old output ", 2000)}}},
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "tool-recent", "name": "run_shell", "input": map[string]any{"command": "recent"}}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "tool-recent", "content": strings.Repeat("recent output ", 5)}}},
		{"role": "user", "content": []map[string]any{{"type": "text", "text": "continue"}}},
	}

	for range engine.QueryLoop(context.Background(), &state) {
	}
	if requestBody == nil {
		t.Fatal("server did not receive request")
	}
	messages, _ := requestBody["messages"].([]any)
	if len(messages) == 0 {
		t.Fatalf("request missing messages: %#v", requestBody)
	}
	foundClearedOld := false
	for _, raw := range messages {
		msg, _ := raw.(map[string]any)
		if msg["role"] == "tool" && msg["tool_call_id"] == "tool-old" {
			foundClearedOld = msg["content"] == "[Old tool result content cleared]"
		}
	}
	if !foundClearedOld {
		t.Fatalf("old tool result should be compressed before API request when APIMaxTokens*0.8 is exceeded, messages=%#v", messages)
	}
	if state.ConsecutiveAutoCompactFailures != 0 {
		t.Fatalf("test should verify pre-request L1/L2 compression without L4 failure, state=%#v", state)
	}
}

func TestCoreQueryLoopProjectsL3RegionsForRequestWithoutRewritingStateLikePython(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
			t.Fatalf("invalid request body: %v body=%s", err, bodyBytes)
		}
		writeOpenAIStream(w, "ok", 1, 1)
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.APIType = "openai_compatible"
	cfg.APIBaseURL = server.URL
	cfg.APIKey = "test-key"
	cfg.APIModel = "custom-router-model"
	cfg.APIMaxTokens = 1000
	cfg.MCPEnabled = false
	cfg.SkillsEnabled = false
	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	state.SystemPrompt = "system"

	var original []map[string]any
	for idx := 0; idx < 8; idx++ {
		original = append(original,
			map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": fmt.Sprintf("user-%d %s", idx, strings.Repeat("x", 4000))}}},
			map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": fmt.Sprintf("assistant-%d %s", idx, strings.Repeat("y", 4000))}}},
		)
	}
	state.Messages = append([]map[string]any(nil), original...)
	originalJSON, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}

	for range engine.QueryLoop(context.Background(), &state) {
	}
	if requestBody == nil {
		t.Fatal("server did not receive request")
	}
	messages, _ := requestBody["messages"].([]any)
	if len(messages) >= len(original)+1 {
		t.Fatalf("L3 projection should shorten provider request, got %d request messages for %d history messages", len(messages), len(original))
	}
	foundProjection := false
	for _, raw := range messages {
		msg, _ := raw.(map[string]any)
		if msg["role"] == "user" && strings.HasPrefix(stringFromAnyForTest(msg["content"]), "[Earlier conversation") {
			foundProjection = true
		}
	}
	if !foundProjection {
		t.Fatalf("provider request should include projected L3 summary like Python, messages=%#v", messages)
	}
	statePrefixJSON, err := json.Marshal(state.Messages[:len(original)])
	if err != nil {
		t.Fatal(err)
	}
	if string(statePrefixJSON) != string(originalJSON) {
		t.Fatalf("L3 projection must not rewrite state history before assistant commit\nwant %s\n got %s", originalJSON, statePrefixJSON)
	}
}

func TestCoreQueryLoopInjectsPendingTaskNotificationsLikePython(t *testing.T) {
	var requestCount atomic.Int32
	var mainBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("invalid request JSON: %v body=%s", err, raw)
		}
		if strings.Contains(string(raw), "main query") {
			mainBody = body
		}
		count := requestCount.Add(1)
		writeOpenAIStream(w, fmt.Sprintf("result-%d", count), 1, 2)
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 256
	cfg.MCPEnabled = false
	cfg.SkillsEnabled = false
	cfg.LongTermMemoryEnabled = false

	engine := agent.NewCoreExecutionEngine(&cfg)
	workerState := agent.NewAgentState()
	def := agent.AgentDef{Name: "Explore", Description: "search", MaxTurns: 3}
	record := engine.TaskRuntime.SpawnWorker(context.Background(), cfg, coretools.NewToolRegistry(), def, &workerState, "desc", "worker prompt", "Explore", "", "worker", true, "main", "", coretools.ExecutionContext{})
	waitUntilStatus(t, engine.TaskRuntime, record.TaskID, "main", "idle")
	engine.TaskRuntime.DrainPendingNotifications("main")
	engine.TaskRuntime.StopTask(record.TaskID, "main")

	state := agent.NewAgentState()
	state.SystemPrompt = "system"
	state.LastQuery = "main query"
	state.Messages = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "main query"}}}}
	for range engine.QueryLoop(context.Background(), &state) {
	}

	if mainBody == nil {
		t.Fatalf("expected a main query request after worker setup, request count=%d", requestCount.Load())
	}
	mainJSON, _ := json.Marshal(mainBody["messages"])
	if !strings.Contains(string(mainJSON), "task-notification") || !strings.Contains(string(mainJSON), "Idle worker was stopped.") {
		t.Fatalf("main request should include pending task notification like Python, messages=%s", mainJSON)
	}
}

func TestTaskRuntimeImportSnapshotInterruptsLiveTasks(t *testing.T) {
	rt := agent.NewAgentTaskRuntime()
	rt.ImportSnapshot([]map[string]any{{
		"task_id":         "subagent-Explore-12345678",
		"parent_scope_id": "main",
		"worker_label":    "reader",
		"description":     "read things",
		"agent_type":      "Explore",
		"reusable":        true,
		"status":          "idle",
	}})
	record := rt.GetTask("subagent-Explore-12345678", "main")
	if record == nil {
		t.Fatal("expected imported record")
	}
	if record.Status != "interrupted" || record.TerminationReason != "session_resumed" {
		t.Fatalf("expected live task to resume as interrupted, got %#v", record)
	}
	notifications := rt.DrainPendingNotifications("main")
	if len(notifications) != 1 {
		t.Fatalf("expected resume interruption notification, got %d", len(notifications))
	}
}

func TestAgentStateCacheBreakpointsUsePythonKey(t *testing.T) {
	state := agent.NewAgentState()
	state.CacheBreakPoints.Add(2)
	state.CacheBreakPoints.Add(5)
	m, err := state.ToMap()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["cache_breakpoints"]; !ok {
		t.Fatalf("expected Python-compatible cache_breakpoints key, got %#v", m)
	}
	if _, ok := m["cache_break_points"]; ok {
		t.Fatalf("unexpected legacy cache_break_points key in serialized state: %#v", m)
	}

	restored, err := agent.GetAgentStateFromMap(map[string]any{
		"cache_breakpoints": []any{float64(3), float64(7)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !restored.CacheBreakPoints.Contains(3) || !restored.CacheBreakPoints.Contains(7) {
		t.Fatalf("expected Python cache_breakpoints to restore, got %#v", restored.CacheBreakPoints)
	}

	legacy, err := agent.GetAgentStateFromMap(map[string]any{
		"cache_break_points": []any{float64(11)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !legacy.CacheBreakPoints.Contains(11) {
		t.Fatalf("expected legacy cache_break_points to restore, got %#v", legacy.CacheBreakPoints)
	}

	defaulted, err := agent.GetAgentStateFromMap(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if defaulted.PermissionState == nil || defaulted.DeniedToolCalls == nil || defaulted.ToolErrors == nil || defaulted.CacheBreakPoints == nil {
		t.Fatalf("expected missing fields to use AgentState defaults, got %#v", defaulted)
	}
}

func TestTaskRuntimeWaitReportsInaccessibleAndForgottenTasks(t *testing.T) {
	rt := agent.NewAgentTaskRuntime()
	rt.RegisterForegroundTask("child-other", "", "other-scope", "other", "desc", "Explore")
	inaccessible := rt.WaitForTasks(context.Background(), []string{"child-other"}, "main", 1)
	if !strings.Contains(inaccessible["error"].(string), "Unknown or inaccessible") {
		t.Fatalf("expected inaccessible error, got %#v", inaccessible)
	}

	rt.DiscardForegroundTask("child-other")
	forgotten := rt.WaitForTasks(context.Background(), []string{"child-other"}, "other-scope", 1)
	if !strings.Contains(forgotten["error"].(string), "Unknown or inaccessible") {
		t.Fatalf("expected unknown/inaccessible error after foreground discard, got %#v", forgotten)
	}
}

func TestTaskRuntimeCleanupScopeRemovesDescendants(t *testing.T) {
	rt := agent.NewAgentTaskRuntime()
	rt.RegisterForegroundTask("child-1", "", "ephemeral", "child", "desc", "Explore")
	rt.RegisterForegroundTask("grandchild-1", "child-1", "child-1", "grand", "desc", "Explore")
	report := rt.CleanupScope("ephemeral")
	if report["tasks_removed"] != 2 {
		t.Fatalf("expected two removed tasks, got %#v", report)
	}
	if record := rt.GetTask("child-1", "ephemeral"); record != nil {
		t.Fatalf("expected child removed, got %#v", record)
	}
}

func TestWorktreeCreateMatchesPythonLifecycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")

	result, err := agent.CreateWorktree(context.Background(), repo, "HEAD", "Explore", ".Lumina/worktrees")
	if err != nil {
		t.Fatal(err)
	}
	if result.RepoRoot != repo || result.BaseRef != "HEAD" {
		t.Fatalf("unexpected worktree metadata: %#v", result)
	}
	if result.WorktreePath == "" || !strings.Contains(filepath.ToSlash(result.WorktreePath), "/.Lumina/worktrees/Explore-") {
		t.Fatalf("unexpected worktree path: %#v", result)
	}
	if result.BranchName != "" {
		t.Fatalf("Python worktree creation does not create a branch, got %q", result.BranchName)
	}
	if _, err := os.Stat(filepath.Join(result.WorktreePath, "README.md")); err != nil {
		t.Fatalf("expected worktree checkout: %v", err)
	}
	if err := agent.RemoveWorktree(context.Background(), result.WorktreePath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(result.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("expected worktree removed, stat err=%v", err)
	}
}

func TestWorktreeCreateGracefullyDegradesOutsideGit(t *testing.T) {
	dir := t.TempDir()
	result, err := agent.CreateWorktree(context.Background(), dir, "HEAD", "Explore", ".Lumina/worktrees")
	if err != nil {
		t.Fatal(err)
	}
	if result.WorktreePath != "" || result.BaseRef != "HEAD" {
		t.Fatalf("expected graceful no-worktree result, got %#v", result)
	}
}

func TestPermissionStateCommandRulesRoundTripAndLegacyKey(t *testing.T) {
	state := security.DefaultPermissionState()
	state.ConfirmCommandPrefix("go test")
	state.ConfirmCommandPrefix("")
	m, err := state.ToMap()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["confirmed_command_rules"]; !ok {
		t.Fatalf("expected Python-compatible confirmed_command_rules key: %#v", m)
	}
	restored, err := security.GetPermissionStateFromMap(m)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.IsCommandPrefixConfirmed("go test") {
		t.Fatalf("expected command prefix to survive round trip: %#v", m)
	}

	legacy, err := security.GetPermissionStateFromMap(map[string]any{
		"confitment_commad_rules": []any{"Bash(npm test:*)"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !legacy.IsCommandPrefixConfirmed("npm test") {
		t.Fatal("expected old misspelled command-rules key to remain readable")
	}
}

func TestShellCommandPrefixConfirmationSkipsOnlySafeCommands(t *testing.T) {
	cfg := config.NewConfig()
	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	state.PermissionState.ConfirmCommandPrefix("go test")
	tool := engine.Registry.Get("run_shell")
	if tool == nil {
		t.Fatal("run_shell tool not registered")
	}

	safeCall := coretools.ToolCall{
		ID:    "bash-safe",
		Name:  "run_shell",
		Input: map[string]any{"command": "go test ./..."},
	}
	if engine.CheckPermissionChain(safeCall, tool, &state) {
		t.Fatal("expected confirmed safe command prefix to skip permission")
	}

	dangerousCall := coretools.ToolCall{
		ID:    "bash-dangerous",
		Name:  "run_shell",
		Input: map[string]any{"command": "go test ./... && rm -rf /tmp/lumina-test"},
	}
	if !engine.CheckPermissionChain(dangerousCall, tool, &state) {
		t.Fatal("expected confirmed prefix not to skip dangerous compound command")
	}
}

func TestVisibleToolResultIDsMatchesPython(t *testing.T) {
	messages := []map[string]any{
		{
			"role": "user",
			"content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "visible-1", "content": "ok"},
				{"type": "text", "text": "ignored"},
			},
		},
		{"role": "user", "content": "not a block list"},
		{
			"role": "user",
			"content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "", "content": "empty id"},
				{"type": "tool_result", "tool_use_id": "visible-2", "content": "ok"},
			},
		},
	}
	ids := agent.VisibleToolResultIDs(messages)
	for _, id := range []string{"visible-1", "visible-2"} {
		if _, ok := ids[id]; !ok {
			t.Fatalf("missing visible tool result id %q in %#v", id, ids)
		}
	}
	if _, ok := ids[""]; ok || len(ids) != 2 {
		t.Fatalf("unexpected visible ids: %#v", ids)
	}
}

func TestQueryLoopStripsLegacyMemoryIndexContextBeforeRequest(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig()
	cfg.CWD = dir
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = filepath.Join(dir, "memory.sqlite")
	cfg.APIKey = ""
	cfg.APIBaseURL = ""
	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	state.Messages = []map[string]any{
		memory.BuildMetaUserMessage("old index", "memory_index"),
		{"role": "user", "content": "current task"},
	}

	for range engine.QueryLoop(context.Background(), &state) {
	}

	for _, message := range state.Messages {
		metadata, _ := message["metadata"].(map[string]any)
		if metadata[memory.MemoryMetaKey] == true && metadata["source"] == "memory_index" {
			t.Fatalf("legacy memory index context should be stripped, got %#v", state.Messages)
		}
	}
	if state.Messages[len(state.Messages)-1]["content"] != "current task" {
		t.Fatalf("current task should remain last: %#v", state.Messages)
	}
}

func TestCoreEngineSkillToolShellPermissionWaitsLikePython(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, ".Lumina", "PROJECT_SKILLS", "sheller")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `---
name: Sheller
description: Needs shell approval
---
Inline output: !` + "`printf shell-ok`" + `
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		call := calls.Add(1)
		if call == 1 {
			fmt.Fprintln(w, `data: {"id":"msg-1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"skill-call-1","type":"function","function":{"name":"Skill","arguments":"{\"skill\":\"sheller\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			fmt.Fprintln(w)
			fmt.Fprintln(w, `data: [DONE]`)
			return
		}
		fmt.Fprintln(w, `data: {"id":"msg-2","choices":[{"delta":{"content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.CWD = dir
	cfg.SkillsDir = ".Lumina/PROJECT_SKILLS"
	cfg.SkillsEnabled = true
	cfg.APIType = "openai_compatible"
	cfg.APIBaseURL = server.URL
	cfg.APIKey = "test-key"
	cfg.APIModel = "custom-router-model"
	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	state.SystemPrompt = "system"
	state.Messages = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "use skill"}}}}

	var events []agent.StreamEvent
	for event := range engine.QueryLoop(context.Background(), &state) {
		events = append(events, event)
		if event.Type == "permission_needed" {
			if _, ok := event.Metadata["skill_shell_request"]; !ok {
				t.Fatalf("skill permission event missing request metadata: %#v", event)
			}
			engine.ResolveSkillPermission(false)
		}
	}

	foundPermission := false
	foundDeniedResult := false
	for _, event := range events {
		if event.Type == "permission_needed" {
			foundPermission = true
		}
		if event.Type == "tool_result" && event.Metadata["tool_name"] == "Skill" {
			result, _ := event.Metadata["result"].(string)
			isError, _ := event.Metadata["is_error"].(bool)
			if isError && strings.Contains(result, "shell command was denied") {
				foundDeniedResult = true
			}
		}
	}
	if !foundPermission || !foundDeniedResult {
		t.Fatalf("expected Python-style Skill shell permission and denied tool result, got %#v", events)
	}
}

func TestRuntimeCacheEditLifecycleMatchesPython(t *testing.T) {
	cfg := config.GetConfig()
	cfg.APIModel = "claude-sonnet-4"
	cfg.AnthropicCacheEditsEnabled = true
	engine := agent.NewCoreExecutionEngine(&cfg)
	engine.QueueCacheEdits([]agentContext.CacheEdit{
		agentContext.NewCacheEdit("stale-1", nil),
		agentContext.NewCacheEdit("missing", nil),
	})

	options := engine.ConsumeCacheEditsForRequest(map[string]struct{}{"stale-1": {}})
	if len(options.AnthropicCacheEdits) != 1 || options.AnthropicCacheEdits[0].ToolUseID != "stale-1" || options.AnthropicCacheEdits[0].Action != "delete" {
		t.Fatalf("expected only visible pending edit to be consumed, got %#v", options)
	}
	snapshot := engine.CacheEditStateSnapshot()
	if len(snapshot.Pending) != 0 || len(snapshot.ConsumedInFlight) != 1 || len(snapshot.Pinned) != 0 {
		t.Fatalf("pending edit should move in flight before stream result, got %#v", snapshot)
	}

	engine.RestoreInFlightCacheEdits()
	snapshot = engine.CacheEditStateSnapshot()
	if len(snapshot.Pending) != 1 || len(snapshot.ConsumedInFlight) != 0 {
		t.Fatalf("restore should prepend consumed edits back to pending, got %#v", snapshot)
	}

	options = engine.ConsumeCacheEditsForRequest(map[string]struct{}{"stale-1": {}})
	if len(options.AnthropicCacheEdits) != 1 {
		t.Fatalf("restored edit should be consumed again, got %#v", options)
	}
	engine.PinConsumedCacheEdits()
	snapshot = engine.CacheEditStateSnapshot()
	if len(snapshot.Pending) != 0 || len(snapshot.ConsumedInFlight) != 0 || len(snapshot.Pinned) != 1 {
		t.Fatalf("successful request should pin consumed edit, got %#v", snapshot)
	}

	options = engine.ConsumeCacheEditsForRequest(map[string]struct{}{"stale-1": {}})
	if len(options.AnthropicCacheEdits) != 1 || options.AnthropicCacheEdits[0].ToolUseID != "stale-1" {
		t.Fatalf("pinned edit should be reused while visible, got %#v", options)
	}
	options = engine.ConsumeCacheEditsForRequest(map[string]struct{}{})
	if len(options.AnthropicCacheEdits) != 0 || len(engine.CacheEditStateSnapshot().Pinned) != 0 {
		t.Fatalf("invisible pinned edits should be pruned, got %#v state=%#v", options, engine.CacheEditStateSnapshot())
	}

	cfg.APIModel = "gpt-5"
	engine = agent.NewCoreExecutionEngine(&cfg)
	engine.QueueCacheEdits([]agentContext.CacheEdit{agentContext.NewCacheEdit("stale-2", nil)})
	options = engine.ConsumeCacheEditsForRequest(map[string]struct{}{"stale-2": {}})
	if len(options.AnthropicCacheEdits) != 0 || len(engine.CacheEditStateSnapshot().Pending) != 0 {
		t.Fatalf("non-Claude models should clear runtime cache edits, got %#v state=%#v", options, engine.CacheEditStateSnapshot())
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}
