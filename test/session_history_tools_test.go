package test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"LuminaCode/agent"
	"LuminaCode/config"
	"LuminaCode/sessionmemory"
	coretools "LuminaCode/tools"
)

func TestSessionHistoryToolsAreRegisteredWhenEnabled(t *testing.T) {
	cfg := config.NewConfigForCWD(t.TempDir())
	cfg.SessionMemoryEnabled = true
	engine := agent.NewCoreExecutionEngine(&cfg)

	if engine.Registry.Get("session_history_list") == nil {
		t.Fatal("session_history_list should be registered when session memory is enabled")
	}
	if engine.Registry.Get("session_history_get") == nil {
		t.Fatal("session_history_get should be registered when session memory is enabled")
	}
}

func stringify(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSessionHistoryToolsAreNotRegisteredWhenDisabled(t *testing.T) {
	cfg := config.NewConfigForCWD(t.TempDir())
	cfg.SessionMemoryEnabled = false
	engine := agent.NewCoreExecutionEngine(&cfg)

	if engine.Registry.Get("session_history_list") != nil {
		t.Fatal("session_history_list should not be registered when session memory is disabled")
	}
	if engine.Registry.Get("session_history_get") != nil {
		t.Fatal("session_history_get should not be registered when session memory is disabled")
	}
}

func TestSessionHistoryToolsDoNotExposeSessionOrAgentIDInputs(t *testing.T) {
	for _, tool := range []coretools.Tool{agent.NewSessionHistoryListTool(), agent.NewSessionHistoryGetTool()} {
		schema := tool.ToAPISchema()
		data := stringify(t, schema)
		if strings.Contains(data, "session_id") || strings.Contains(data, "agent_id") {
			t.Fatalf("%s schema should not expose session_id/agent_id to the model: %s", tool.Name(), data)
		}
	}
}

func TestSessionHistoryListUsesCurrentExecutionSessionAndAgentID(t *testing.T) {
	cfg := config.NewConfigForCWD(t.TempDir())
	cfg.SessionDir = t.TempDir()
	cfg.SessionMemoryEnabled = true
	cfg.SessionMemoryTurnInterval = 1
	cfg.SessionMemorySummaryModel = "fake"
	cfg.SessionMemorySummaryMaxTokens = 128
	cfg.SessionMemoryAgentID = "backend"
	store, err := sessionmemory.Open(context.Background(), cfg, "parent-session", func(context.Context, string, []map[string]any, int) (string, error) {
		return `{"title":"backend title","summary":"backend summary","tags":["backend"]}`, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IngestMessages(context.Background(), []map[string]any{{
		"role": "user",
		"content": []map[string]any{{
			"type": "text",
			"text": "backend user turn",
		}},
		"metadata": map[string]any{"session_user_turn": 1},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MaybeCommit(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	tool := agent.NewSessionHistoryListTool()
	out, err := tool.Execute(context.Background(), coretools.ExecutionContext{
		"config":      cfg,
		"_session_id": "parent-session",
		"_agent_id":   "backend",
	}, agent.SessionHistoryListInput{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"session_id": "parent-session"`) ||
		!strings.Contains(out, `"agent_id": "backend"`) ||
		!strings.Contains(out, "backend summary") {
		t.Fatalf("session_history_list should query requested session+agent memory, got %s", out)
	}
}
