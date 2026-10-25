package session

import (
	"LuminaCode/agent"
	"LuminaCode/config"
	"LuminaCode/harness"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

func checkpointFixture(t *testing.T) (RuntimeStore, *agent.AgentRuntime) {
	t.Helper()
	ctx := context.Background()
	load, err := NewStore(t.TempDir()).OpenRuntime(ctx, "checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = load.Journal.Close() })
	return checkpointOwner(t, load.Journal)
}

func checkpointOwner(t *testing.T, journal RuntimeStore) (RuntimeStore, *agent.AgentRuntime) {
	t.Helper()
	ctx := context.Background()
	owner := agent.NewAgentRuntime(config.Config{ContextInputTokenLimit: 16000}, nil)
	owner.BindStore(journal, journal.SessionID())
	run, draft, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	draft.Messages = append(draft.Messages, map[string]any{"role": "user", "content": "old conversation"})
	if err = owner.Commit(run, draft); err != nil {
		t.Fatal(err)
	}
	owner.EndRun(draft.RunToken())
	return journal, owner
}

func TestStaleCheckpointCannotResurrectClear(t *testing.T) {
	runStaleCheckpointCannotResurrectClear(t, checkpointFixture)
}

func runStaleCheckpointCannotResurrectClear(t *testing.T, fixture func(*testing.T) (RuntimeStore, *agent.AgentRuntime)) {
	journal, owner := fixture(t)
	ctx := context.Background()
	stale := owner.Snapshot().LegacyState()
	if err := owner.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if err := AppendRuntimeState(ctx, journal, &stale, nil, nil); !errors.Is(err, agent.ErrStaleState) {
		t.Fatalf("stale checkpoint accepted: %v", err)
	}
	restored, _, _, err := LoadRuntimeState(ctx, journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Messages) != 0 || len(restored.Transcript) != 0 {
		t.Fatal("clear was undone by an external snapshot")
	}
}

func TestStaleCheckpointCannotRollbackMemoryCursor(t *testing.T) {
	runStaleCheckpointCannotRollbackMemoryCursor(t, checkpointFixture)
}

func runStaleCheckpointCannotRollbackMemoryCursor(t *testing.T, fixture func(*testing.T) (RuntimeStore, *agent.AgentRuntime)) {
	journal, owner := fixture(t)
	ctx := context.Background()
	current := owner.Snapshot()
	stale := current.LegacyState()
	owner.AdvanceMemory(current.Generation, 99)
	if err := AppendRuntimeState(ctx, journal, &stale, nil, nil); !errors.Is(err, agent.ErrStaleState) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	restored, _, _, err := LoadRuntimeState(ctx, journal)
	if err != nil {
		t.Fatal(err)
	}
	if restored.MemoryExtractionSeq != 99 {
		t.Fatal("background memory progress was rolled back")
	}
}

type interleavingCheckpointStore struct {
	RuntimeStore
	once         sync.Once
	beforeAppend func()
}

func (s *interleavingCheckpointStore) Append(ctx context.Context, expected int64, pending ...harness.PendingEvent) ([]harness.Event, error) {
	s.once.Do(s.beforeAppend)
	return s.RuntimeStore.Append(ctx, expected, pending...)
}

func TestResetBetweenCheckpointValidationAndAppendIsRejected(t *testing.T) {
	runResetBetweenCheckpointValidationAndAppendIsRejected(t, checkpointFixture)
}

func runResetBetweenCheckpointValidationAndAppendIsRejected(t *testing.T, fixture func(*testing.T) (RuntimeStore, *agent.AgentRuntime)) {
	journal, owner := fixture(t)
	ctx := context.Background()
	stale := owner.Snapshot().LegacyState()
	interleaved := &interleavingCheckpointStore{RuntimeStore: journal, beforeAppend: func() {
		if err := owner.Reset(ctx); err != nil {
			t.Error(err)
		}
	}}
	if err := AppendRuntimeState(ctx, interleaved, &stale, nil, nil); err == nil {
		t.Fatal("compare-and-append accepted a concurrent reset")
	}
	restored, _, _, err := LoadRuntimeState(ctx, journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Messages) != 0 {
		t.Fatal("reset lost its race with a compatibility checkpoint")
	}
}

func TestOwnedCheckpointSerializesWithReset(t *testing.T) {
	runOwnedCheckpointSerializesWithReset(t, checkpointFixture)
}

func runOwnedCheckpointSerializesWithReset(t *testing.T, fixture func(*testing.T) (RuntimeStore, *agent.AgentRuntime)) {
	journal, owner := fixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			if _, err := owner.Checkpoint(ctx, nil, nil); err != nil {
				t.Error(err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			if err := owner.Reset(ctx); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()
	restored, _, _, err := LoadRuntimeState(ctx, journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Messages) != 0 || restored.ContextSnapshot.Generation != owner.Snapshot().Generation {
		t.Fatal("checkpoint and reset were not serialized")
	}
}

func TestChildRecoveryReplaysDurableTail(t *testing.T) {
	runChildRecoveryReplaysDurableTail(t, checkpointFixture)
}

func runChildRecoveryReplaysDurableTail(t *testing.T, fixture func(*testing.T) (RuntimeStore, *agent.AgentRuntime)) {
	journal, _ := fixture(t)
	ctx := context.Background()
	cfg := config.Config{ContextInputTokenLimit: 16000}
	child := agent.NewAgentRuntime(cfg, nil)
	child.BindStore(journal, "child")
	run, draft, err := child.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	draft.Messages = append(draft.Messages, map[string]any{"role": "user", "content": "child task"})
	if err = child.Commit(run, draft); err != nil {
		t.Fatal(err)
	}
	child.EndRun(draft.RunToken())
	message, _ := json.Marshal(map[string]any{"role": "assistant", "context_id": "durable-tail", "content": "durable result"})
	if _, err = journal.Append(ctx, harness.AnyStreamSeq,
		harness.PendingEvent{StreamID: "child", Type: harness.EventMessageAssistantAppended, Payload: harness.MessageAppendedPayload{Message: message}},
		harness.PendingEvent{StreamID: "child", Type: harness.EventUsageRecorded, Payload: harness.UsageRecordedPayload{RequestID: "tail-request", InputTokens: 7, OutputTokens: 3}},
	); err != nil {
		t.Fatal(err)
	}
	registry := agent.NewSessionRuntime()
	for i := 0; i < 2; i++ {
		if err = registry.RestoreChildren(ctx, journal, cfg, journal.SessionID()); err != nil {
			t.Fatal(err)
		}
		restored := registry.Snapshots()["child"]
		if len(restored.Context.Messages) != 2 || len(restored.Transcript) != 2 || restored.Usage.InputTokens != 7 || restored.Usage.OutputTokens != 3 {
			t.Fatalf("child durable tail lost or duplicated: %+v", restored)
		}
	}
}
