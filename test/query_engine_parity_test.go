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

	"LuminaCode/agent"
	"LuminaCode/longmemory"
	"LuminaCode/skills"
)

func TestQueryEngineSlashSkillInvocationInjectsInlineContext(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skills", "reader")
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
	cfg := isolatedConfigForCWD(t, dir)
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

	cfg := isolatedConfigForCWD(t, dir)
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "claude-test"
	cfg.APIType = "anthropic"
	cfg.APIMaxTokens = 256
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
	cfg := isolatedConfigForCWD(t, dir)
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
	cfg := isolatedConfig(t)
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
	cfg := isolatedConfigForCWD(t, dir)
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = filepath.Join(dir, "memory.sqlite")
	cfg.SkillsEnabled = false
	engine := agent.NewQueryEngine(&cfg)
	state := agent.NewAgentState()
	state.SystemPrompt = "stale prompt"

	for range engine.SubmitMessage(context.Background(), "hello", &state, "session-memory-prompt") {
	}

	if !strings.Contains(state.SystemPrompt, "Lumina identity.") ||
		!strings.Contains(state.SystemPrompt, "## Long-Term Memory") ||
		!strings.Contains(state.SystemPrompt, "local SQLite store") ||
		strings.Contains(state.SystemPrompt, "stale prompt") {
		t.Fatalf("system prompt was not refreshed with long-term memory section:\n%s", state.SystemPrompt)
	}
}

func TestCoreQueryLoopPrefetchesRecalledMemoriesBeforeFirstRequestLikePython(t *testing.T) {
	dir := t.TempDir()
	store, err := longmemory.Open(context.Background(), filepath.Join(dir, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Upsert(context.Background(), longmemory.Candidate{
		ScopeType:  longmemory.ScopeProject,
		ScopeKey:   longmemory.CanonicalProjectScopeKey(dir),
		MemoryType: longmemory.TypeProject,
		Title:      "Repo Note",
		Summary:    "Important repo note",
		Content:    "Remember the blue switch.",
		Importance: 0.9,
		Confidence: 0.9,
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
		if count != 1 {
			t.Fatalf("long-term memory recall should be local; unexpected extra model request %#v", body)
		}
		mainBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-main\",\"usage\":{\"input_tokens\":2}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3},\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
	}))
	defer server.Close()

	cfg := isolatedConfigForCWD(t, dir)
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "claude-test"
	cfg.APIType = "anthropic"
	cfg.APIMaxTokens = 256
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = store.Path()
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

	if requestCount.Load() != 1 {
		t.Fatalf("expected one main model request, got %d", requestCount.Load())
	}
	mainJSON, _ := json.Marshal(mainBody["messages"])
	if !strings.Contains(string(mainJSON), "Remember the blue switch.") {
		t.Fatalf("main request should include pre-fetched recalled memory before first model call, body=%s", mainJSON)
	}
}

func TestCoreQueryLoopFollowupRecallAppendsAfterToolResultsLikePython(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(target, []byte("hello from test"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := longmemory.Open(context.Background(), filepath.Join(dir, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Upsert(context.Background(), longmemory.Candidate{
		ScopeType:  longmemory.ScopeProject,
		ScopeKey:   longmemory.CanonicalProjectScopeKey(dir),
		MemoryType: longmemory.TypeFeedback,
		Title:      "initial",
		Summary:    "Initial preference memory.",
		Content:    "Initial memory body. continue",
		Importance: 0.9,
		Confidence: 0.9,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(context.Background(), longmemory.Candidate{
		ScopeType:  longmemory.ScopeProject,
		ScopeKey:   longmemory.CanonicalProjectScopeKey(dir),
		MemoryType: longmemory.TypeReference,
		Title:      "followup",
		Summary:    "Follow-up memory after reading source.txt.",
		Content:    "Follow-up memory body for source.txt.",
		Importance: 0.9,
		Confidence: 0.9,
	}); err != nil {
		t.Fatal(err)
	}

	var streamCalls atomic.Int32
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
			t.Fatalf("long-term memory recall should be local, got unexpected completion request %#v", body)
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

	cfg := isolatedConfigForCWD(t, dir)
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "custom-router-model"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 5000
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = store.Path()
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

	if streamCalls.Load() != 2 {
		t.Fatalf("expected two stream calls, got %d", streamCalls.Load())
	}
	bodyJSON, _ := json.Marshal(secondStreamBody["messages"])
	bodyText := string(bodyJSON)
	if strings.Count(bodyText, "Initial memory body.") != 1 || strings.Count(bodyText, "Follow-up memory body") != 1 {
		t.Fatalf("second stream should contain initial and followup memories exactly once, body=%s", bodyText)
	}
	if !strings.Contains(bodyText, `"role":"tool"`) || !strings.Contains(bodyText, `"tool_call_id":"tool-read-1"`) {
		t.Fatalf("second stream should preserve tool result alongside recalled memories, body=%s", bodyText)
	}
}

func TestQueryEngineShutdownTearsDownRuntimeLikePython(t *testing.T) {
	cfg := isolatedConfig(t)
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = "http://127.0.0.1"
	engine := agent.NewQueryEngine(&cfg)
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
}
