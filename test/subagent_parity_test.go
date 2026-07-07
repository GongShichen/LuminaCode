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
	coretools "LuminaCode/tools"
)

type searchInput struct {
	Query string `json:"query"`
}

type searchTool struct {
	coretools.BaseTool
}

func newSearchTool() *searchTool {
	return &searchTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:             "search",
		Description:      "search test tool",
		InputPrototype:   searchInput{},
		ReadOnly:         coretools.BoolPtr(true),
		ConcurrencySafe:  coretools.BoolPtr(true),
		Destructive:      coretools.BoolPtr(false),
		ConfirmFilePaths: false,
	}}}
}

func (t *searchTool) Execute(_ context.Context, _ coretools.ExecutionContext, input any) (string, error) {
	switch v := input.(type) {
	case *searchInput:
		return "search output for " + v.Query, nil
	case searchInput:
		return "search output for " + v.Query, nil
	default:
		return "search output", nil
	}
}

func TestSubAgentExecutesToolAndContinuesConversation(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		call := atomic.AddInt32(&calls, 1)
		if call == 1 {
			fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tool-1\",\"function\":{\"name\":\"large_read\",\"arguments\":\"{\\\"content\\\":\\\"hello from tool\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"id\":\"msg-2\",\"choices\":[{\"delta\":{\"content\":\"final answer\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"msg-2\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":6}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 256
	cfg.SessionDir = t.TempDir()
	registry := coretools.NewToolRegistry(newLargeReadTool())
	def := agent.AgentDef{Name: "general-purpose", Description: "test", MaxTurns: 5}
	state := agent.NewAgentState()
	sub := agent.NewSubAgent(cfg, registry, def, &state, "", "general-purpose", coretools.ExecutionContext{})
	result, err := sub.Run(context.Background(), "do the thing")
	if err != nil {
		t.Fatal(err)
	}
	if result != "final answer" {
		t.Fatalf("unexpected result: %q", result)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected two API calls, got %d", calls)
	}
	if sub.TotalInputTokens != 8 || sub.TotalOutputTokens != 10 {
		t.Fatalf("unexpected token accounting: %d/%d", sub.TotalInputTokens, sub.TotalOutputTokens)
	}
}

func TestSubAgentAbortCheckReturnsAbortText(t *testing.T) {
	cfg := config.NewConfig()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = "http://127.0.0.1:1"
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	registry := coretools.NewToolRegistry(newLargeReadTool())
	def := agent.AgentDef{Name: "general-purpose", Description: "test", MaxTurns: 5}
	sub := agent.NewSubAgent(cfg, registry, def, nil, "", "general-purpose", coretools.ExecutionContext{
		"abort_check": func() bool { return true },
	})
	result, err := sub.Run(context.Background(), "stop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Sub-agent aborted by user") {
		t.Fatalf("expected abort text, got %q", result)
	}
}

func TestSubAgentDeadlineReturnsPartialProgressInsteadOfToolError(t *testing.T) {
	cfg := config.NewConfig()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = "http://127.0.0.1:1"
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	def := agent.AgentDef{Name: "general-purpose", Description: "test", MaxTurns: 5}
	sub := agent.NewSubAgent(cfg, coretools.NewToolRegistry(newSearchTool()), def, nil, "", "general-purpose", coretools.ExecutionContext{})
	session := &agent.SubAgentSessionState{
		Messages: []map[string]any{
			{
				"role":    "assistant",
				"content": []map[string]any{{"type": "text", "text": "partial project findings"}},
			},
		},
		RecentToolObservations: []map[string]any{
			{
				"call":    coretools.ToolCall{Name: "search"},
				"content": "found package manifest\nsecond line",
			},
		},
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	result := sub.ExecuteOneRequest(ctx, "inspect", session)

	for _, want := range []string{"300s timeout", "partial project findings", "search: found package manifest"} {
		if !strings.Contains(result.FinalText, want) {
			t.Fatalf("partial timeout result missing %q:\n%s", want, result.FinalText)
		}
	}
	if strings.Contains(result.FinalText, "<tool_use_error>") {
		t.Fatalf("subagent timeout should not be surfaced as a tool_use_error:\n%s", result.FinalText)
	}
}

func TestSubAgentRunAsksModelToFinalizeAfterTimeout(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			time.Sleep(1200 * time.Millisecond)
			fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{\"content\":\"late text\"}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "Stop using tools") ||
			!strings.Contains(string(body), "time limit") {
			t.Fatalf("finalize request should instruct subagent to stop and summarize known context:\n%s", string(body))
		}
		fmt.Fprint(w, "data: {\"id\":\"msg-2\",\"choices\":[{\"delta\":{\"content\":\"finalized from known facts\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"msg-2\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":8}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 256
	cfg.SessionDir = t.TempDir()
	def := agent.AgentDef{Name: "general-purpose", Description: "test", MaxTurns: 5}
	sub := agent.NewSubAgent(cfg, coretools.NewToolRegistry(), def, nil, "", "general-purpose", coretools.ExecutionContext{
		"subagent_timeout_seconds": 1,
	})

	result, err := sub.Run(context.Background(), "inspect slowly")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[Sub-agent timeout]", "1 second timeout", "main agent may ask", "finalized from known facts"} {
		if !strings.Contains(result, want) {
			t.Fatalf("timeout finalized result missing %q:\n%s", want, result)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("expected timed-out request plus finalize request, got %d", calls.Load())
	}
}

func TestSubAgentSessionStateDefaultsMatchPython(t *testing.T) {
	cfg := config.NewConfig()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = "http://127.0.0.1:1"
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	def := agent.AgentDef{Name: "general-purpose", Description: "test", MaxTurns: 1}
	sub := agent.NewSubAgent(cfg, coretools.NewToolRegistry(), def, nil, "", "general-purpose", coretools.ExecutionContext{})
	state := &agent.SubAgentSessionState{}

	result := sub.ExecuteOneRequest(context.Background(), "hello", state)

	if !strings.Contains(result.FinalText, "Sub-agent API call failed") && !strings.Contains(result.FinalText, "Sub-agent error") {
		t.Fatalf("empty session state should default abort_check=false and reach API path, got %#v", result)
	}
	if state.AbortCheck == nil || state.AbortCheck() {
		t.Fatalf("abort_check default should be an isolated false-returning function")
	}
	if state.SurfacedMemoryIDs == nil {
		t.Fatalf("surfaced memory ids should default to an empty map like Python")
	}
}

func TestSubAgentContinuationSkipsNotificationDrainLikePython(t *testing.T) {
	var streamCalls atomic.Int32
	var drainCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		call := streamCalls.Add(1)
		if call == 1 {
			fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{\"content\":\"part 1\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"id\":\"msg-2\",\"choices\":[{\"delta\":{\"content\":\"part 2\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"msg-2\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 256
	cfg.SessionDir = t.TempDir()
	def := agent.AgentDef{Name: "general-purpose", Description: "test", MaxTurns: 3}
	sub := agent.NewSubAgent(cfg, coretools.NewToolRegistry(), def, nil, "", "general-purpose", coretools.ExecutionContext{
		"scope_id":        "scope-1",
		"current_task_id": "task-1",
		"_drain_pending_notifications": func(scopeID, taskID string) []map[string]any {
			if scopeID != "scope-1" || taskID != "task-1" {
				t.Fatalf("unexpected drain identifiers: %q %q", scopeID, taskID)
			}
			drainCalls.Add(1)
			return nil
		},
	})

	result, err := sub.Run(context.Background(), "start")
	if err != nil {
		t.Fatal(err)
	}
	if result != "part 2" {
		t.Fatalf("unexpected continuation result: %q", result)
	}
	if streamCalls.Load() != 2 {
		t.Fatalf("expected two stream calls, got %d", streamCalls.Load())
	}
	if drainCalls.Load() != 1 {
		t.Fatalf("continuation turn should skip notification drain like Python, got %d calls", drainCalls.Load())
	}
	if sub.TotalInputTokens != 7 || sub.TotalOutputTokens != 10 {
		t.Fatalf("unexpected token accounting: %d/%d", sub.TotalInputTokens, sub.TotalOutputTokens)
	}
}

func TestSubAgentSystemPromptUsesPythonSectionBuilder(t *testing.T) {
	dir := t.TempDir()
	scopeDir := filepath.Join(config.ProjectRuntimeDir(dir), "agent-memory", "explore")
	if err := os.MkdirAll(scopeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeDir, "MEMORY.md"), []byte("# Index\n\n- useful memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfig()
	cfg.CWD = dir
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	cfg.AutoMemoryEnabled = true
	registry := coretools.NewToolRegistry(newLargeReadTool())
	def := agent.AgentDef{Name: "Explore", Description: "Read-only search agent.", MaxTurns: 7}
	sub := agent.NewSubAgent(cfg, registry, def, nil, "", "Explore", coretools.ExecutionContext{})
	session := sub.CreateSessionState("inspect")
	prompt := session.SystemPrompt
	for _, expected := range []string{
		"You are a Explore sub-agent. Read-only search agent.",
		"You have 7 turns to complete the task.",
		"Return your final answer as plain text",
		"Agent-type memory:",
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("subagent system prompt missing %q:\n%s", expected, prompt)
		}
	}
	if strings.Contains(prompt, "\n\nSub-agent: Explore\n") {
		t.Fatalf("subagent prompt should use section builder, got legacy prompt:\n%s", prompt)
	}
}

func TestSubAgentPassesForkThinkingBudgetToAnthropicClientLikePython(t *testing.T) {
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
	budget := 1024
	def := agent.AgentDef{Name: "SkillFork", Description: "Forked skill.", MaxTurns: 1}
	sub := agent.NewSubAgent(cfg, coretools.NewToolRegistry(), def, nil, "", "skill-fork", coretools.ExecutionContext{}, &budget)

	result, err := sub.Run(context.Background(), "inspect")
	if err != nil {
		t.Fatal(err)
	}
	if result != "done" {
		t.Fatalf("unexpected result: %q", result)
	}
	thinking, _ := requestBody["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || int(thinking["budget_tokens"].(float64)) != budget {
		t.Fatalf("expected Anthropic thinking budget in request body, got %#v", requestBody["thinking"])
	}
}

func TestSubAgentSessionRecoversInitialSurfacedAgentMemoryIDsLikePython(t *testing.T) {
	dir := t.TempDir()
	scopeDir := filepath.Join(config.ProjectRuntimeDir(dir), "agent-memory", "explore")
	if err := os.MkdirAll(scopeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeDir, "MEMORY.md"), []byte("- [Repo Note](repo-note.md) - Useful repo note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeDir, "repo-note.md"), []byte("---\nname: Repo Note\ndescription: Useful repo note\nmetadata:\n  type: reference\n---\n\nUse this repo note.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"[\"project--repo-note.md\"]"}}]}`)
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.CWD = dir
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	cfg.AutoMemoryEnabled = true
	def := agent.AgentDef{Name: "Explore", Description: "Read-only search agent.", MaxTurns: 7}
	sub := agent.NewSubAgent(cfg, coretools.NewToolRegistry(), def, nil, "", "Explore", coretools.ExecutionContext{})

	session := sub.CreateSessionState("inspect repo")
	if _, ok := session.SurfacedMemoryIDs["project--repo-note.md"]; !ok {
		t.Fatalf("expected initial agent-memory recall id to be surfaced like Python, got %#v", session.SurfacedMemoryIDs)
	}
}

func TestSubAgentSearchToolTriggersAgentMemoryRecallLikePython(t *testing.T) {
	dir := t.TempDir()
	scopeDir := filepath.Join(config.ProjectRuntimeDir(dir), "agent-memory", "explore")
	if err := os.MkdirAll(scopeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeDir, "MEMORY.md"), []byte("- [Search Note](search-note.md) - Search followup note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeDir, "search-note.md"), []byte("---\nname: Search Note\ndescription: Search followup note\nmetadata:\n  type: reference\n---\n\nUse this search-triggered memory.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var completeCalls atomic.Int32
	var streamCalls atomic.Int32
	var secondStreamSawMemory atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("invalid request JSON: %v body=%s", err, bodyBytes)
		}
		if body["stream"] == false {
			call := completeCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if call == 1 {
				fmt.Fprint(w, `{"choices":[{"message":{"content":"[]"}}]}`)
				return
			}
			fmt.Fprint(w, `{"choices":[{"message":{"content":"[\"project--search-note.md\"]"}}]}`)
			return
		}

		call := streamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tool-1\",\"function\":{\"name\":\"search\",\"arguments\":\"{\\\"query\\\":\\\"repo\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		if strings.Contains(string(bodyBytes), "Use this search-triggered memory.") {
			secondStreamSawMemory.Store(true)
		}
		fmt.Fprint(w, "data: {\"id\":\"msg-2\",\"choices\":[{\"delta\":{\"content\":\"final after search memory\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"msg-2\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":6}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.CWD = dir
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "gpt-5"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 256
	cfg.SessionDir = t.TempDir()
	cfg.AutoMemoryEnabled = true
	def := agent.AgentDef{Name: "Explore", Description: "Search agent.", MaxTurns: 5}
	sub := agent.NewSubAgent(cfg, coretools.NewToolRegistry(newSearchTool()), def, nil, "", "Explore", coretools.ExecutionContext{})

	result, err := sub.Run(context.Background(), "inspect with search")
	if err != nil {
		t.Fatal(err)
	}
	if result != "final after search memory" {
		t.Fatalf("unexpected result: %q", result)
	}
	if completeCalls.Load() < 2 {
		t.Fatalf("expected followup agent-memory selector call after search, got %d", completeCalls.Load())
	}
	if !secondStreamSawMemory.Load() {
		t.Fatalf("second stream request did not include recalled agent memory")
	}
}
