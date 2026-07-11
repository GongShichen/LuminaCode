package test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/api"
	"LuminaCode/config"
	"LuminaCode/longmemory"
)

type expansionStreamClient struct {
	mu        sync.Mutex
	input     map[string]any
	request   string
	streamErr error
}

func (m *expansionStreamClient) StreamChat(_ context.Context, _ string, messages []map[string]any, _ []map[string]any, _ *api.LLMRequestOptions) <-chan api.EventResult {
	out := make(chan api.EventResult, 2)
	m.mu.Lock()
	if len(messages) > 0 {
		m.request, _ = messages[0]["content"].(string)
	}
	input := m.input
	err := m.streamErr
	m.mu.Unlock()
	if err != nil {
		out <- api.EventResult{Err: err}
	} else {
		out <- api.EventResult{Event: map[string]any{"type": "tool_use", "id": "expand-1",
			"name": "ExpandMemoryQuery", "input": input}}
	}
	close(out)
	return out
}

func (m *expansionStreamClient) Complete(context.Context, string, []map[string]any, api.CompleteOptions) (string, error) {
	return "", nil
}

func TestMemoryExpansionForbiddenFieldFallsBackToAllChannels(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	storePath := filepath.Join(root, "memory.sqlite")
	store, err := longmemory.Open(ctx, storePath)
	if err != nil {
		t.Fatal(err)
	}
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}
	if _, err := store.Upsert(ctx, longmemory.Candidate{ScopeType: scope.Type, ScopeKey: scope.Key,
		MemoryType: longmemory.TypeProject, Title: "Release command", Content: "Run go test ./... before release.",
		Entities: []string{"go test"}, SourceSessionID: "session-a", Confidence: 1}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	cfg := config.NewConfigForCWD(root)
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = storePath
	cfg.MemoryEmbeddingEnabled = false
	cfg.MemoryQueryExpansionEnabled = true
	state := agent.NewAgentState()
	state.MemorySessionID = "query-session"
	client := &expansionStreamClient{input: map[string]any{
		"queries": []any{"release test command"}, "channel": "bm25",
	}}
	recalls := agent.RunMemoryRecallWithRuntime(ctx, cfg, &state, "What command runs before release?",
		func(context.Context, string) (api.LLMClient, error) { return client, nil })
	if len(recalls) == 0 {
		t.Fatal("forbidden expansion field must not prevent original-query retrieval")
	}
	store, err = longmemory.Open(ctx, storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	traces, err := store.ListRetrievalTraces(ctx, 1)
	if err != nil || len(traces) != 1 || traces[0].Run == nil {
		t.Fatalf("missing retrieval trace: traces=%#v err=%v", traces, err)
	}
	run := traces[0].Run
	if run.ExpansionError != "" {
		t.Fatalf("unknown routing fields should be ignored without discarding valid expansion: %q", run.ExpansionError)
	}
	if len(run.Expansion.Queries) != 1 || run.Expansion.Queries[0] != "release test command" {
		t.Fatalf("valid expansion fields must be retained, got %#v", run.Expansion)
	}
	if len(run.ChannelResults) != 6 {
		t.Fatalf("all six channels must run, got %d", len(run.ChannelResults))
	}
}

func TestMemoryExpansionReceivesOnlyVisibleRecentContext(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	storePath := filepath.Join(root, "memory.sqlite")
	store, err := longmemory.Open(ctx, storePath)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = store.Upsert(ctx, longmemory.Candidate{ScopeType: longmemory.ScopeProject,
		ScopeKey: longmemory.ProjectScopeKey(root), MemoryType: longmemory.TypeSemantic,
		Title: "Visible preference", Content: "Use pnpm for frontend tasks."})
	_ = store.Close()
	cfg := config.NewConfigForCWD(root)
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = storePath
	cfg.MemoryEmbeddingEnabled = false
	state := agent.NewAgentState()
	state.Messages = []map[string]any{
		{"role": "system", "content": "SECRET_SYSTEM_TEXT"},
		{"role": "user", "content": "visible user context"},
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "name": "read_file", "input": map[string]any{"path": "SECRET_TOOL_PATH"}}, {"type": "text", "text": "visible assistant context"}}},
		{"role": "user", "content": "SECRET_TRANSIENT_MEMORY", "metadata": map[string]any{"source": "memory"}},
	}
	client := &expansionStreamClient{input: map[string]any{"queries": []any{"frontend package manager"}}}
	_ = agent.RunMemoryRecallWithRuntime(ctx, cfg, &state, "Which package manager should I use?",
		func(context.Context, string) (api.LLMClient, error) { return client, nil })
	client.mu.Lock()
	request := client.request
	client.mu.Unlock()
	if strings.Contains(request, "SECRET_SYSTEM_TEXT") || strings.Contains(request, "SECRET_TOOL_PATH") || strings.Contains(request, "SECRET_TRANSIENT_MEMORY") {
		t.Fatalf("hidden context leaked into expansion request: %s", request)
	}
	if !strings.Contains(request, "visible user context") || !strings.Contains(request, "visible assistant context") {
		t.Fatalf("visible recent context missing from expansion request: %s", request)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(request), &decoded); err != nil || decoded["query"] != "Which package manager should I use?" {
		t.Fatalf("raw query was not preserved: decoded=%#v err=%v", decoded, err)
	}
}

func TestMemoryExpansionKeepsValidFieldsWhenTemporalInputIsPartiallyInvalid(t *testing.T) {
	cfg := config.NewConfig()
	cfg.MemoryQueryExpansionEnabled = true
	cfg.MemoryQueryExpansionMaxQueries = 5
	client := &expansionStreamClient{input: map[string]any{
		"queries":  []any{"Aurora persistence decision"},
		"entities": []any{"Project Aurora"},
		"temporal_constraints": []any{map[string]any{
			"from": "May 4, 2026", "to": "not a date", "order": "sideways", "ignored": "value",
		}},
		"channel": "bm25",
	}}
	expansion, _, expansionErr := agent.ExpandMemoryQuery(context.Background(), cfg,
		longmemory.MemoryQuery{Text: "What did Aurora decide?", Timestamp: time.Now().UTC()}, longmemory.MemoryCatalog{},
		func(context.Context, string) (api.LLMClient, error) { return client, nil })
	if expansionErr != "" {
		t.Fatalf("partial temporal input discarded the whole expansion: %s", expansionErr)
	}
	if len(expansion.Queries) != 1 || len(expansion.Entities) != 1 || len(expansion.TemporalConstraints) != 1 {
		t.Fatalf("valid expansion fields were not retained: %#v", expansion)
	}
	constraint := expansion.TemporalConstraints[0]
	if constraint.From.IsZero() || !constraint.To.IsZero() || constraint.Order != "none" {
		t.Fatalf("temporal fields were not normalized independently: %#v", constraint)
	}
}
