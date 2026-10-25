package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/harness"
)

func TestTypedRuntimeMigrationReplaysAfterCheckpointAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir())
	load, err := store.OpenRuntime(ctx, "migration")
	if err != nil {
		t.Fatal(err)
	}
	defer load.Journal.Close()
	runTypedMigrationContract(t, load.Journal)
}

func runTypedMigrationContract(t *testing.T, journal RuntimeStore) {
	t.Helper()
	ctx := context.Background()
	var err error
	legacy := agent.NewAgentState()
	legacy.PermissionState.YoloMode = true
	legacy.TotalInputTokens = 100
	legacy.Messages = []map[string]any{{"role": "user", "content": "original request"}}
	raw, _ := json.Marshal(legacy)
	if _, err = journal.Append(ctx, harness.AnyStreamSeq, harness.PendingEvent{Type: harness.EventRuntimeStateCheckpointed, Payload: harness.RuntimeStateCheckpointedPayload{State: raw}}); err != nil {
		t.Fatal(err)
	}
	message, _ := json.Marshal(map[string]any{"role": "assistant", "context_id": "reply", "content": "durable reply"})
	if _, err = journal.Append(ctx, harness.AnyStreamSeq,
		harness.PendingEvent{Type: harness.EventMessageAssistantAppended, Payload: harness.MessageAppendedPayload{Message: message}},
		harness.PendingEvent{Type: harness.EventUsageRecorded, Payload: harness.UsageRecordedPayload{RequestID: "request", InputTokens: 7, OutputTokens: 2}},
	); err != nil {
		t.Fatal(err)
	}
	first, _, _, err := LoadRuntimeState(ctx, journal)
	if err != nil {
		t.Fatal(err)
	}
	if first.TotalInputTokens != 107 || first.TotalOutputTokens != 2 || !first.YoloEnabled() || len(first.Messages) != 2 || len(first.Transcript) != 2 {
		t.Fatalf("migration lost state: %+v", first)
	}
	head, _ := journal.Head(ctx)
	second, _, _, err := LoadRuntimeState(ctx, journal)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := journal.Head(ctx)
	if head != again || second.TotalInputTokens != 107 || len(second.Transcript) != 2 {
		t.Fatal("loading upgraded state duplicated facts or migration")
	}
}

func TestPostgresTypedRuntimeMigrationContract(t *testing.T) {
	url := os.Getenv("LUMINA_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set LUMINA_TEST_POSTGRES_URL to run the PostgreSQL state replay contract")
	}
	ctx := context.Background()
	repository, err := NewPostgresRepository(ctx, url, fmt.Sprintf("context-test-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	load, err := repository.OpenRuntime(ctx, "context-test", fmt.Sprintf("migration-%d", time.Now().UnixNano()), RuntimeOpenOptions{CreateIfMissing: true, FenceToken: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer load.Journal.Close()
	runTypedMigrationContract(t, load.Journal)
}

func TestChildCheckpointCannotReplaceMainAgentState(t *testing.T) {
	ctx := context.Background()
	load, err := NewStore(t.TempDir()).OpenRuntime(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	defer load.Journal.Close()
	main := agent.NewAgentState()
	main.Messages = []map[string]any{{"role": "user", "content": "parent request"}}
	if err = AppendRuntimeState(ctx, load.Journal, &main, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err = load.Journal.CreateStream(ctx, harness.StreamDescriptor{ID: "child", Kind: "agent", ParentID: "parent"}); err != nil {
		t.Fatal(err)
	}
	child := agent.NewAgentState()
	child.Messages = []map[string]any{{"role": "user", "content": "private child task"}}
	raw, _ := json.Marshal(agent.SnapshotFromState(&child))
	if _, err = load.Journal.Append(ctx, harness.AnyStreamSeq, harness.PendingEvent{StreamID: "child", Type: harness.EventRuntimeStateCheckpointed, Payload: harness.RuntimeStateCheckpointedPayload{State: raw}}); err != nil {
		t.Fatal(err)
	}
	restored, _, _, err := LoadRuntimeState(ctx, load.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Messages[0]["content"] != "parent request" {
		t.Fatal("child checkpoint leaked into parent")
	}
}

func TestClearGenerationDoesNotRestorePriorConversation(t *testing.T) {
	ctx := context.Background()
	load, err := NewStore(t.TempDir()).OpenRuntime(ctx, "reset")
	if err != nil {
		t.Fatal(err)
	}
	defer load.Journal.Close()
	initial := agent.NewAgentState()
	initial.Messages = []map[string]any{{"role": "user", "content": "old"}}
	if err = AppendRuntimeState(ctx, load.Journal, &initial, nil, nil); err != nil {
		t.Fatal(err)
	}
	message, _ := json.Marshal(map[string]any{"role": "user", "content": "new"})
	if _, err = load.Journal.Append(ctx, harness.AnyStreamSeq,
		harness.PendingEvent{Type: harness.EventSessionReset, Payload: map[string]any{"generation": 2}},
		harness.PendingEvent{Type: harness.EventMessageUserAppended, Payload: harness.MessageAppendedPayload{Message: message}},
	); err != nil {
		t.Fatal(err)
	}
	restored, _, _, err := LoadRuntimeState(ctx, load.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Messages) != 1 || restored.Messages[0]["content"] != "new" || len(restored.Transcript) != 1 || restored.ContextSnapshot.Generation < 2 {
		t.Fatalf("reset resurrected old state: %+v", restored)
	}
}

func TestLegacyFileMigrationKeepsOneCopyOfOriginalMessages(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir())
	state := agent.NewAgentState()
	state.Messages = []map[string]any{{"role": "user", "content": "request"}, {"role": "assistant", "content": "response"}}
	if err := store.SaveSnapshotWithRecovery("legacy-file", &state, nil, nil); err != nil {
		t.Fatal(err)
	}
	load, err := store.OpenRuntime(ctx, "legacy-file")
	if err != nil {
		t.Fatal(err)
	}
	defer load.Journal.Close()
	if len(load.State.Transcript) != 2 {
		t.Fatalf("legacy migration duplicated original history: %d", len(load.State.Transcript))
	}
	again, _, _, err := LoadRuntimeState(ctx, load.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Transcript) != 2 {
		t.Fatal("reloading duplicated legacy history")
	}
}

func TestTerminalToolEventAndCanonicalMessageAreOneOriginalObservation(t *testing.T) {
	ctx := context.Background()
	load, err := NewStore(t.TempDir()).OpenRuntime(ctx, "tool-observation")
	if err != nil {
		t.Fatal(err)
	}
	defer load.Journal.Close()
	result := map[string]any{"type": "tool_result", "tool_use_id": "call", "content": "output"}
	rawResult, _ := json.Marshal(result)
	message, _ := json.Marshal(map[string]any{"role": "user", "context_id": "canonical", "content": []map[string]any{result}})
	if _, err = load.Journal.Append(ctx, harness.AnyStreamSeq,
		harness.PendingEvent{Type: harness.EventToolExecutionCompleted, Payload: harness.ToolLifecyclePayload{ToolCallID: "call", Result: rawResult}},
		harness.PendingEvent{Type: harness.EventMessageContextAppended, Payload: harness.MessageAppendedPayload{Message: message}},
	); err != nil {
		t.Fatal(err)
	}
	state, _, _, err := LoadRuntimeState(ctx, load.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Transcript) != 1 || len(state.Messages) != 1 {
		t.Fatalf("tool observation duplicated: original=%d working=%d", len(state.Transcript), len(state.Messages))
	}
}
