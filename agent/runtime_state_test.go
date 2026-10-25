package agent_test

import (
	"LuminaCode/agent"
	"LuminaCode/agentContext"
	"LuminaCode/config"
	"LuminaCode/harness"
	filetool "LuminaCode/tools/file"
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestAgentRuntimeDraftIsolationAndResetFence(t *testing.T) {
	r := agent.NewAgentRuntime(config.Config{ContextInputTokenLimit: 16000}, nil)
	ctx, draft, err := r.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	draft.Messages = append(draft.Messages, map[string]any{"role": "user", "content": "original"})
	if len(r.Snapshot().Context.Messages) != 0 {
		t.Fatal("draft leaked before commit")
	}
	if err = r.Commit(ctx, draft); err != nil {
		t.Fatal(err)
	}
	draft.ContextSnapshot.SystemPrompt = "escaped draft"
	if r.Snapshot().Context.SystemPrompt == "escaped draft" {
		t.Fatal("context draft shares published state")
	}
	s := r.Snapshot()
	s.Context.Messages[0].Role = "changed"
	if r.Snapshot().Context.Messages[0].Role != "user" {
		t.Fatal("snapshot shares storage")
	}
	services := r.Services(s.Generation)
	if err = r.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("reset did not cancel the active request")
	}
	services.SetReadFileState("old.go", filetool.FileStateEntry{Content: "stale"})
	if _, ok := r.GetReadFileState("old.go"); ok {
		t.Fatal("late tool changed reset runtime")
	}
	if err = r.Commit(context.Background(), draft); !errors.Is(err, agent.ErrStaleState) {
		t.Fatalf("obsolete draft accepted: %v", err)
	}
}

func TestReplayRecoversPostCheckpointFactsAndDeduplicatesUsage(t *testing.T) {
	base := agent.SnapshotFromState(nil)
	message, _ := json.Marshal(map[string]any{"role": "assistant", "content": "durable response", "context_id": "response"})
	payload, _ := json.Marshal(harness.MessageAppendedPayload{Message: message})
	usage, _ := json.Marshal(harness.UsageRecordedPayload{RequestID: "request", InputTokens: 12, OutputTokens: 3})
	events := []harness.Event{{Seq: 2, StreamID: "session", Type: harness.EventMessageAssistantAppended, Payload: payload}, {Seq: 3, StreamID: "session", Type: harness.EventUsageRecorded, Payload: usage}, {Seq: 4, StreamID: "session", Type: harness.EventUsageRecorded, Payload: usage}}
	next := agent.ReplayAgentState(base, events, 1, "session")
	if next.Usage.InputTokens != 12 || next.Usage.OutputTokens != 3 || len(next.Context.Messages) != 1 || len(next.Transcript) != 1 {
		t.Fatalf("lost or duplicated post-checkpoint facts: %+v", next)
	}
}

func TestRuntimeKeepsOriginalToolResultsWhenWorkingViewChanges(t *testing.T) {
	initial := agent.NewAgentState()
	initial.Messages = []map[string]any{{"role": "user", "content": "original request"}}
	r := agent.NewAgentRuntime(config.Config{ContextInputTokenLimit: 16000}, &initial)
	ctx, draft, err := r.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.End(r.Snapshot().Generation)
	draft.Messages[0]["content"] = "compressed working content"
	if err = r.Commit(ctx, draft); err != nil {
		t.Fatal(err)
	}
	original := r.Snapshot().Transcript[0].Map()["content"]
	if original != "original request" {
		t.Fatalf("working edit changed original: %v", original)
	}
}

type failingRuntimeStore struct{ recordingEventStore }

func (*failingRuntimeStore) Append(context.Context, int64, ...harness.PendingEvent) ([]harness.Event, error) {
	return nil, errors.New("disk unavailable")
}
func TestRuntimePersistenceFailureDoesNotPublish(t *testing.T) {
	r := agent.NewAgentRuntime(config.Config{}, nil)
	r.BindStore(&failingRuntimeStore{}, "session")
	ctx, draft, _ := r.Begin(context.Background())
	draft.Messages = append(draft.Messages, map[string]any{"role": "user", "content": "must be durable"})
	if err := r.Commit(ctx, draft); err == nil {
		t.Fatal("write failure ignored")
	}
	if len(r.Snapshot().Context.Messages) != 0 {
		t.Fatal("undurable state was published")
	}
}

func TestRuntimeAccountsForCompactorUsageExactlyOnce(t *testing.T) {
	cfg := config.Config{ContextInputTokenLimit: 10000, ContextCompressThreshold: 0.8}
	initial := agent.NewAgentState()
	for i := 0; i < 8; i++ {
		role := "assistant"
		if i%2 == 0 {
			role = "user"
		}
		initial.Messages = append(initial.Messages, map[string]any{"role": role, "content": "some original evidence for the handoff summary"})
	}
	r := agent.NewAgentRuntime(cfg, &initial)
	manager := agentContext.NewContextManager(cfg, func(context.Context, string, string) (agentContext.SummaryResult, error) {
		return agentContext.SummaryResult{Text: "handoff", Usage: agentContext.UsageObservation{RequestID: "summary-request", InputTokens: 123, OutputTokens: 7}}, nil
	})
	_ = manager.Restore(r.Snapshot().Context)
	r.Context = manager
	ctx, draft, err := r.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.End(draft.ContextSnapshot.Generation)
	result, err := manager.Compact(ctx, agentContext.CompactManual)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.CommitContext(ctx, result, draft); err != nil {
		t.Fatal(err)
	}
	if r.Snapshot().Usage.InputTokens != 123 || r.Snapshot().Usage.OutputTokens != 7 {
		t.Fatal("compactor usage missing")
	}
	draft.UsageRequests["unpublished"] = true
	if r.Snapshot().Usage.Requests["unpublished"] {
		t.Fatal("usage draft shares published map")
	}
	if err = r.CommitContext(ctx, result, draft); !errors.Is(err, agentContext.ErrStaleContext) {
		t.Fatal("candidate committed twice")
	}
	if r.Snapshot().Usage.InputTokens != 123 {
		t.Fatal("usage counted twice")
	}
}

func TestReplayRestoresPlannedCallBeforeItsDurableResult(t *testing.T) {
	plan, _ := json.Marshal(harness.ToolLifecyclePayload{ToolCallID: "call", Name: "read_file", Input: json.RawMessage(`{"file_path":"actual.go","opaque_id":9007199254740993}`)})
	result, _ := json.Marshal(harness.ToolLifecyclePayload{ToolCallID: "call", Result: json.RawMessage(`{"type":"tool_result","tool_use_id":"call","content":"observed output"}`)})
	events := []harness.Event{{Seq: 2, StreamID: "session", Type: harness.EventToolCallPlanned, Payload: plan}, {Seq: 3, StreamID: "session", Type: harness.EventToolExecutionCompleted, Payload: result}}
	recovered := agent.ReplayAgentState(agent.SnapshotFromState(nil), events, 1, "session")
	if len(recovered.Context.Messages) != 2 {
		t.Fatal("durable tool exchange was not reconstructed")
	}
	call := recovered.Context.Messages[0].Map()["content"].([]map[string]any)[0]
	if call["input"].(map[string]any)["opaque_id"] != json.Number("9007199254740993") {
		t.Fatal("replay rounded a tool argument")
	}
	if call["name"] != "read_file" || call["input"].(map[string]any)["file_path"] != "actual.go" {
		t.Fatalf("tool identity or arguments lost: %+v", call)
	}
}
