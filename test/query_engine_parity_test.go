package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/config"
	"LuminaCode/memory"
	"LuminaCode/skills"
)

func TestQueryEngineSlashSkillInvocationInjectsInlineContext(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, ".Lumina", "PROJECT_SKILLS", "reader")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `---
name: Reader
description: Read things
user-invocable: true
---
Read carefully: $ARGUMENTS
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfig()
	cfg.CWD = dir
	cfg.SkillsDir = ".Lumina/PROJECT_SKILLS"
	cfg.SkillsEnabled = true
	engine := agent.NewQueryEngine(&cfg)
	state := agent.NewAgentState()

	var events []agent.StreamEvent
	for event := range engine.SubmitMessage(context.Background(), "/reader target", &state, "session-1") {
		events = append(events, event)
	}
	for _, event := range events {
		if event.Type == "error" && strings.Contains(event.Content, "Unknown command") {
			t.Fatalf("slash skill should not be treated as unknown: %#v", events)
		}
	}
	if len(state.Messages) < 2 {
		t.Fatalf("expected skill context and normalized user turn, got %#v", state.Messages)
	}
	metadata, _ := state.Messages[0]["metadata"].(map[string]any)
	if metadata["source"] != skills.SkillInlineSource {
		t.Fatalf("expected inline skill context to survive commit, got %#v", state.Messages)
	}
	content := state.Messages[0]["content"].([]map[string]any)
	if !strings.Contains(content[0]["text"].(string), "Read carefully: target") {
		t.Fatalf("expected rendered skill prompt, got %#v", content)
	}
	foundUserPrompt := false
	for _, message := range state.Messages {
		metadata, _ := message["metadata"].(map[string]any)
		if metadata["source"] != nil {
			continue
		}
		content, _ := message["content"].([]map[string]any)
		if len(content) > 0 && content[0]["text"] == "Use skill 'reader' with arguments: target" {
			foundUserPrompt = true
		}
	}
	if !foundUserPrompt {
		t.Fatalf("expected normalized skill user prompt, got %#v", state.Messages)
	}
}

func TestCoreQueryLoopInlineSkillIntegerLikeEffortSetsThinkingBudgetLikePython(t *testing.T) {
	dir := t.TempDir()
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
			t.Fatalf("invalid request JSON: %v body=%s", err, bodyBytes)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"usage\":{\"input_tokens\":2}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3},\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.CWD = dir
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "claude-test"
	cfg.APIType = "anthropic"
	cfg.APIMaxTokens = 256
	cfg.AutoMemoryEnabled = false
	cfg.MCPEnabled = false
	cfg.SkillsEnabled = false
	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	state.SystemPrompt = "system"
	state.LastQuery = "hello"
	state.Messages = []map[string]any{
		{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": "inline"}},
			"isMeta":  true,
			"metadata": map[string]any{
				"source":              skills.SkillInlineSource,
				"lumina_skill_effort": true,
			},
		},
		{"role": "user", "content": []map[string]any{{"type": "text", "text": "hello"}}},
	}

	var sawDone bool
	for event := range engine.QueryLoop(context.Background(), &state) {
		if event.Type == "done" {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("expected query loop to finish")
	}
	thinking, _ := requestBody["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || int(thinking["budget_tokens"].(float64)) != 1 {
		t.Fatalf("inline bool effort should map to Python thinking budget 1, got %#v", requestBody["thinking"])
	}
}

func TestQueryEngineSlashSkillShellPermissionWaitsForUserDecisionLikePython(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, ".Lumina", "PROJECT_SKILLS", "sheller")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `---
name: Sheller
description: Needs shell approval
user-invocable: true
---
Inline output: !` + "`printf shell-ok`" + `
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfig()
	cfg.CWD = dir
	cfg.SkillsDir = ".Lumina/PROJECT_SKILLS"
	cfg.SkillsEnabled = true
	engine := agent.NewQueryEngine(&cfg)
	state := agent.NewAgentState()

	ctx, cancel := context.WithTimeout(context.Background(), 2_000_000_000)
	defer cancel()
	events := engine.SubmitMessage(ctx, "/sheller", &state, "session-shell")
	first, ok := <-events
	if !ok {
		t.Fatal("expected permission event before skill execution finished")
	}
	if first.Type != "permission_needed" {
		t.Fatalf("expected Python-style skill shell permission event, got %#v", first)
	}
	if _, ok := first.Metadata["skill_shell_request"]; !ok {
		t.Fatalf("permission event should carry skill_shell_request metadata, got %#v", first.Metadata)
	}
	engine.ResolveSkillPermission(false)

	var rest []agent.StreamEvent
	for event := range events {
		rest = append(rest, event)
	}
	foundDenied := false
	for _, event := range rest {
		if event.Type == "error" && strings.Contains(event.Content, "shell command was denied") {
			foundDenied = true
		}
	}
	if !foundDenied {
		t.Fatalf("expected denied skill shell error after user decision, got %#v", rest)
	}
}

func TestQueryEngineManualCompactUsesPythonPipeline(t *testing.T) {
	cfg := config.NewConfig()
	cfg.APIMaxTokens = 1000
	engine := agent.NewQueryEngine(&cfg)
	state := agent.NewAgentState()
	state.CacheBreakPoints.Add(1)
	noisyOutput := strings.Repeat("Collecting package\nDownloading file\nRequirement already satisfied: dep\n", 200)
	state.Messages = []map[string]any{
		{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "id": "tool-1", "name": "run_shell"},
		}},
		{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "tool-1", "content": noisyOutput},
		}},
	}

	compacted, stats := engine.Compact(&state)
	if stats.LevelReached < 1 {
		t.Fatalf("expected manual compact to reach at least L1, got %#v", stats)
	}
	if compacted.CacheBreakPoints.Cardinality() != 0 {
		t.Fatalf("manual compact should clear cache breakpoints, got %#v", compacted.CacheBreakPoints)
	}
	resultContent := compacted.Messages[1]["content"].([]map[string]any)[0]["content"].(string)
	if len(resultContent) >= len(noisyOutput) {
		t.Fatalf("expected noisy tool result to shrink, got len=%d original=%d", len(resultContent), len(noisyOutput))
	}
}

func TestQueryEngineRefreshesSystemPromptWithMemorySectionLikePython(t *testing.T) {
	dir := t.TempDir()
	systemDir := filepath.Join(dir, ".Lumina", "SYSTEM")
	if err := os.MkdirAll(systemDir, 0o755); err != nil {
		t.Fatal(err)
	}
	template := `[SECTION: identity]
Lumina identity.
`
	if err := os.WriteFile(filepath.Join(systemDir, "system-prompt.md"), []byte(template), 0o644); err != nil {
		t.Fatal(err)
	}
	memDir := filepath.Join(dir, ".Lumina", "memory")
	cfg := config.NewConfig()
	cfg.CWD = dir
	cfg.AutoMemoryEnabled = true
	cfg.AutoMemoryDirectory = &memDir
	cfg.SkillsEnabled = false
	engine := agent.NewQueryEngine(&cfg)
	state := agent.NewAgentState()
	state.SystemPrompt = "stale prompt"

	for range engine.SubmitMessage(context.Background(), "hello", &state, "session-memory-prompt") {
	}

	if !strings.Contains(state.SystemPrompt, "Lumina identity.") ||
		!strings.Contains(state.SystemPrompt, "## Persistent Memory") ||
		!strings.Contains(state.SystemPrompt, memDir) ||
		strings.Contains(state.SystemPrompt, "stale prompt") {
		t.Fatalf("system prompt was not refreshed like Python:\n%s", state.SystemPrompt)
	}
}

func TestCoreQueryLoopPrefetchesRecalledMemoriesBeforeFirstRequestLikePython(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	store := memory.NewMemoryStore(memDir)
	if _, err := store.SaveEntry(&memory.MemoryEntry{
		Name:        "Repo Note",
		Description: "Important repo note",
		Content:     "Remember the blue switch.",
		Metadata:    map[string]any{"type": "project"},
	}); err != nil {
		t.Fatal(err)
	}

	var requestCount atomic.Int32
	var mainBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("invalid request JSON: %v body=%s", err, bodyBytes)
		}
		count := requestCount.Add(1)
		if count == 1 {
			if body["stream"] != false {
				t.Fatalf("first request should be Python-style recall Complete call, got %#v", body)
			}
			if _, ok := body["max_tokens"]; ok {
				t.Fatalf("recall Complete request should omit max_tokens, got %#v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"content":[{"type":"text","text":"[\"repo-note.md\"]"}]}`)
			return
		}
		mainBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-main\",\"usage\":{\"input_tokens\":2}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3},\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.CWD = dir
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "claude-test"
	cfg.APIType = "anthropic"
	cfg.APIMaxTokens = 256
	cfg.AutoMemoryEnabled = true
	cfg.AutoMemoryDirectory = &memDir
	cfg.MemoryRecallPrefetchTimeoutSeconds = 1
	cfg.MCPEnabled = false
	cfg.SkillsEnabled = false

	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	state.SystemPrompt = "system"
	state.LastQuery = "what should I remember?"
	state.Messages = []map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "text", "text": state.LastQuery}}},
	}
	for range engine.QueryLoop(context.Background(), &state) {
	}

	if requestCount.Load() < 2 {
		t.Fatalf("expected recall and main model requests, got %d", requestCount.Load())
	}
	mainJSON, _ := json.Marshal(mainBody["messages"])
	if !strings.Contains(string(mainJSON), "Remember the blue switch.") {
		t.Fatalf("main request should include pre-fetched recalled memory before first model call, body=%s", mainJSON)
	}
}

func TestCoreQueryLoopSlowMemoryRecallDoesNotBlockFirstRequestLikePython(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	store := memory.NewMemoryStore(memDir)
	if _, err := store.SaveEntry(&memory.MemoryEntry{
		Name:        "Slow Note",
		Description: "Slow recall note",
		Content:     "This slow memory should not reach the first request.",
		Metadata:    map[string]any{"type": "project"},
	}); err != nil {
		t.Fatal(err)
	}

	mainBodyCh := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("invalid request JSON: %v body=%s", err, bodyBytes)
		}
		if stream, _ := body["stream"].(bool); !stream {
			time.Sleep(150 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"content":"[\"slow-note.md\"]"}}]}`)
			return
		}
		select {
		case mainBodyCh <- body:
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"msg-main\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.CWD = dir
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "custom-router-model"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 256
	cfg.AutoMemoryEnabled = true
	cfg.AutoMemoryDirectory = &memDir
	cfg.MemoryRecallPrefetchTimeoutSeconds = 0.02
	cfg.MCPEnabled = false
	cfg.SkillsEnabled = false

	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	state.SystemPrompt = "system"
	state.LastQuery = "what should I remember?"
	state.Messages = []map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "text", "text": state.LastQuery}}},
	}
	for range engine.QueryLoop(context.Background(), &state) {
	}

	select {
	case mainBody := <-mainBodyCh:
		mainJSON, _ := json.Marshal(mainBody["messages"])
		if strings.Contains(string(mainJSON), "This slow memory should not reach the first request.") {
			t.Fatalf("slow recall should not block and inject before first request, body=%s", mainJSON)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("main request was blocked by slow memory recall")
	}
}

func TestCoreQueryLoopFollowupRecallAppendsAfterToolResultsLikePython(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	target := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(target, []byte("hello from test"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := memory.NewMemoryStore(memDir)
	if _, err := store.SaveEntry(&memory.MemoryEntry{
		Name:        "initial",
		Description: "Initial preference memory.",
		Content:     "Initial memory body.",
		Metadata:    map[string]any{"type": "feedback"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveEntry(&memory.MemoryEntry{
		Name:        "followup",
		Description: "Follow-up memory after reading files.",
		Content:     "Follow-up memory body.",
		Metadata:    map[string]any{"type": "reference"},
	}); err != nil {
		t.Fatal(err)
	}

	var completeCalls atomic.Int32
	var streamCalls atomic.Int32
	var secondRecallPrompt string
	var secondStreamBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("invalid request JSON: %v body=%s", err, bodyBytes)
		}
		if stream, _ := body["stream"].(bool); !stream {
			call := completeCalls.Add(1)
			messages, _ := body["messages"].([]any)
			if call == 2 && len(messages) > 0 {
				user, _ := messages[len(messages)-1].(map[string]any)
				secondRecallPrompt = fmt.Sprint(user["content"])
			}
			w.Header().Set("Content-Type", "application/json")
			if call == 1 {
				fmt.Fprint(w, `{"choices":[{"message":{"content":"[\"initial.md\"]"}}]}`)
				return
			}
			fmt.Fprint(w, `{"choices":[{"message":{"content":"[\"initial.md\", \"followup.md\"]"}}]}`)
			return
		}

		call := streamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tool-read-1\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"file_path\\\":\\\"source.txt\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":4}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		secondStreamBody = body
		fmt.Fprint(w, "data: {\"id\":\"msg-2\",\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"msg-2\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.CWD = dir
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "custom-router-model"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 5000
	cfg.AutoMemoryEnabled = true
	cfg.AutoMemoryDirectory = &memDir
	cfg.MemoryRecallPrefetchTimeoutSeconds = 1
	cfg.MCPEnabled = false
	cfg.SkillsEnabled = false

	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	state.SystemPrompt = "system"
	state.LastQuery = "read file and continue"
	state.Messages = []map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "text", "text": state.LastQuery}}},
	}
	for range engine.QueryLoop(context.Background(), &state) {
	}

	if streamCalls.Load() != 2 || completeCalls.Load() != 2 {
		t.Fatalf("expected two stream calls and two recall calls like Python, stream=%d complete=%d", streamCalls.Load(), completeCalls.Load())
	}
	if !strings.Contains(secondRecallPrompt, "Already shown") || !strings.Contains(secondRecallPrompt, "initial.md") {
		t.Fatalf("followup recall prompt should exclude already surfaced memory like Python, prompt=%q", secondRecallPrompt)
	}
	bodyJSON, _ := json.Marshal(secondStreamBody["messages"])
	bodyText := string(bodyJSON)
	if strings.Count(bodyText, "Initial memory body.") != 1 || strings.Count(bodyText, "Follow-up memory body.") != 1 {
		t.Fatalf("second stream should contain initial and followup memories exactly once, body=%s", bodyText)
	}
	if !strings.Contains(bodyText, `"role":"tool"`) || !strings.Contains(bodyText, `"tool_call_id":"tool-read-1"`) {
		t.Fatalf("second stream should preserve tool result alongside recalled memories, body=%s", bodyText)
	}
}

func TestQueryEngineShutdownTearsDownRuntimeLikePython(t *testing.T) {
	cfg := config.NewConfig()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = "http://127.0.0.1"
	engine := agent.NewQueryEngine(&cfg)
	engine.SessionCost = 12.34
	oldRuntime := engine.CoreEngine.TaskRuntime
	record := oldRuntime.RegisterForegroundTask("task-1", "", "main", "worker", "desc", "general-purpose")
	if record == nil {
		t.Fatal("expected task record")
	}

	engine.Shutdown()

	stopped := oldRuntime.GetTask("task-1", "main")
	if stopped == nil || stopped.Status != "killed" || stopped.TerminationReason != "stopped" {
		t.Fatalf("shutdown should stop existing task runtime records, got %#v", stopped)
	}
	if engine.CoreEngine.TaskRuntime == nil || engine.CoreEngine.TaskRuntime == oldRuntime {
		t.Fatalf("shutdown should replace task runtime like Python, old=%p new=%p", oldRuntime, engine.CoreEngine.TaskRuntime)
	}
	if engine.SessionCost != 0 {
		t.Fatalf("shutdown should reset session cost, got %v", engine.SessionCost)
	}
}
