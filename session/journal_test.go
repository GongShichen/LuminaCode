package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/harness"
)

func TestRuntimeJournalAppendReplayCheckpointAndConflict(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	journal, err := openRuntimeJournal(ctx, RuntimeJournalPath(t.TempDir(), "session-1"), "session-1", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	message, _ := json.Marshal(map[string]any{"role": "user", "content": "hello"})
	events, err := journal.Append(ctx, 0,
		harness.PendingEvent{Type: harness.EventSessionCreated, Payload: map[string]any{"session_id": "session-1"}},
		harness.PendingEvent{Type: harness.EventMessageUserAppended, Audience: []harness.Audience{harness.AudienceModel, harness.AudienceUser}, Payload: harness.MessageAppendedPayload{Message: message, RunID: "run-1", UserTurn: 1}},
		harness.PendingEvent{Type: harness.EventRunStarted, Payload: harness.RunLifecyclePayload{RunID: "run-1"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Seq != 1 || events[2].StreamSeq != 3 {
		t.Fatalf("unexpected event sequence: %#v", events)
	}
	if _, err := journal.Append(ctx, 0, harness.PendingEvent{Type: harness.EventRuntimeWarning, Payload: map[string]any{}}); !errors.Is(err, ErrStreamConflict) {
		t.Fatalf("expected stream conflict, got %v", err)
	}
	if _, err := journal.Append(ctx, 3, harness.PendingEvent{Type: harness.EventRunCompleted, Payload: harness.RunLifecyclePayload{RunID: "run-1"}}); err != nil {
		t.Fatal(err)
	}
	projection := harness.NewSessionProjection()
	last, err := harness.Replay(ctx, journal, projection, 0)
	if err != nil {
		t.Fatal(err)
	}
	if last != 4 || len(projection.Messages) != 1 || projection.RunStates["run-1"] != "completed" {
		t.Fatalf("unexpected projection: %#v", projection)
	}
	state, _ := projection.MarshalState()
	if err := journal.SaveCheckpoint(ctx, harness.Checkpoint{StreamID: "session-1", Projector: projection.Name(), ProjectorVersion: projection.Version(), UpToSeq: last, State: state}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := journal.LoadCheckpoint(ctx, "session-1", projection.Name())
	if err != nil || checkpoint == nil || checkpoint.UpToSeq != 4 {
		t.Fatalf("unexpected checkpoint: %#v %v", checkpoint, err)
	}
	if err := journal.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestStoreCompatibilityReadsStateFromOpenRuntimeJournal(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := NewStore(dir)
	load, err := store.OpenRuntime(ctx, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer load.Journal.Close()
	state := agent.NewAgentState()
	state.PermissionState.YoloMode = true
	if err := AppendRuntimeState(ctx, load.Journal, &state, nil, nil); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenRuntimeJournal(ctx, dir, "session-1")
	if err != nil {
		t.Fatalf("open concurrent reader: %v", err)
	}
	if direct, _, _, err := LoadRuntimeState(ctx, reader); err != nil || direct == nil {
		_ = reader.Close()
		t.Fatalf("direct runtime load: state=%#v err=%v", direct, err)
	}
	_ = reader.Close()
	loaded := NewStore(dir).LoadState("session-1")
	if loaded == nil || loaded.PermissionState == nil || !loaded.PermissionState.YoloMode {
		t.Fatalf("runtime state was not visible through Store: %#v", loaded)
	}
}

func TestRuntimeJournalCommandAndBlobAreIdempotent(t *testing.T) {
	ctx := context.Background()
	journal, err := OpenRuntimeJournal(ctx, t.TempDir(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	digest1, err := journal.PutBlob(ctx, "text/plain", []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	digest2, err := journal.PutBlob(ctx, "text/plain", []byte("same"))
	if err != nil || digest1 != digest2 {
		t.Fatalf("blob dedup failed: %s %s %v", digest1, digest2, err)
	}
	result := harness.CommandResult{CommandID: "command-1", SessionID: "session-1", AcceptedSeq: 7, Result: json.RawMessage(`{"ok":true}`)}
	if err := journal.SaveCommandResult(ctx, result); err != nil {
		t.Fatal(err)
	}
	result.AcceptedSeq = 9
	if err := journal.SaveCommandResult(ctx, result); err != nil {
		t.Fatal(err)
	}
	loaded, err := journal.GetCommandResult(ctx, "command-1")
	if err != nil || loaded == nil || loaded.AcceptedSeq != 7 {
		t.Fatalf("first command result must win: %#v %v", loaded, err)
	}
}

func TestRecoverInterruptedRuntimeAppendsTerminalEvents(t *testing.T) {
	ctx := context.Background()
	journal, err := OpenRuntimeJournal(ctx, t.TempDir(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if _, err := journal.Append(ctx, 0,
		harness.PendingEvent{Type: harness.EventRunStarted, Payload: harness.RunLifecyclePayload{RunID: "run-1"}},
		harness.PendingEvent{Type: harness.EventStepStarted, CorrelationID: "run-1", Payload: map[string]any{"run_id": "run-1", "step_id": "step-1", "step_no": 1}},
		harness.PendingEvent{Type: harness.EventToolCallPlanned, CorrelationID: "run-1", Payload: harness.ToolLifecyclePayload{ToolCallID: "tool-1", Name: "write"}},
		harness.PendingEvent{Type: harness.EventToolExecutionStarted, CorrelationID: "run-1", Payload: harness.ToolLifecyclePayload{ToolCallID: "tool-1", Name: "write"}},
	); err != nil {
		t.Fatal(err)
	}
	if err := RecoverInterruptedRuntime(ctx, journal); err != nil {
		t.Fatal(err)
	}
	events, err := journal.Load(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-3].Type; got != harness.EventToolExecutionInterrupted {
		t.Fatalf("tool recovery event = %s", got)
	}
	if got := events[len(events)-2].Type; got != harness.EventStepInterrupted {
		t.Fatalf("step recovery event = %s", got)
	}
	if got := events[len(events)-1].Type; got != harness.EventRunInterrupted {
		t.Fatalf("last event = %s", got)
	}
	if err := RecoverInterruptedRuntime(ctx, journal); err != nil {
		t.Fatal(err)
	}
	again, _ := journal.Load(ctx, 0, 100)
	if len(again) != len(events) {
		t.Fatalf("recovery must be idempotent: before=%d after=%d", len(events), len(again))
	}
}

func TestOpenRuntimeMigratesLegacyStateAndNeverFallsBack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := NewStore(dir)
	state := agent.NewAgentState()
	state.Messages = []map[string]any{{"role": "user", "content": "legacy"}, {"role": "assistant", "content": "answer"}}
	state.TurnCount = 3
	state.PermissionState.YoloMode = true
	recovery := map[string]any{"version": 1, "agent_scopes": map[string]any{"main": map[string]any{"active": true}}}
	tasks := []map[string]any{{"id": "task-1", "status": "completed"}}
	if err := store.SaveSnapshotWithRecovery("legacy", &state, recovery, tasks); err != nil {
		t.Fatal(err)
	}
	load, err := store.OpenRuntime(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if !load.Migrated || load.State == nil || load.State.TurnCount != 3 || !load.State.PermissionState.YoloMode {
		t.Fatalf("unexpected migrated state: %#v", load)
	}
	if len(load.Tasks) != 1 || load.Tasks[0]["id"] != "task-1" {
		t.Fatalf("tasks did not migrate: %#v", load.Tasks)
	}
	_ = load.Journal.Close()
	if _, err := os.Stat(filepath.Join(dir, "legacy", ".migration-backup", "v1", "state.json")); err != nil {
		t.Fatalf("migration backup missing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "legacy", "state.json"), []byte(`{"state":{"turn_count":999}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.OpenRuntime(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Journal.Close()
	if reopened.State == nil || reopened.State.TurnCount != 3 {
		t.Fatalf("v2 runtime fell back to modified legacy state: %#v", reopened.State)
	}
}

func TestTaskLifecycleEventsOverrideCompatibilityCheckpoint(t *testing.T) {
	ctx := context.Background()
	journal, err := OpenRuntimeJournal(ctx, t.TempDir(), "session-task-events")
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	state := agent.NewAgentState()
	legacyTask := []map[string]any{{"task_id": "task-1", "status": "queued", "created_at": float64(1)}}
	if err := AppendRuntimeState(ctx, journal, &state, nil, legacyTask); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(ctx, harness.AnyStreamSeq,
		harness.PendingEvent{Type: harness.EventTaskCreated, Payload: map[string]any{"task_id": "task-1", "record": map[string]any{"task_id": "task-1", "status": "running", "created_at": float64(1)}}},
		harness.PendingEvent{Type: harness.EventTaskStatusChanged, Payload: map[string]any{"task_id": "task-1", "record": map[string]any{"task_id": "task-1", "status": "completed", "created_at": float64(1)}}},
	); err != nil {
		t.Fatal(err)
	}
	_, _, tasks, err := LoadRuntimeState(ctx, journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0]["status"] != "completed" {
		t.Fatalf("task projection did not win over compatibility checkpoint: %#v", tasks)
	}
}

func TestCorruptRuntimeJournalIsIsolatedToItsSession(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := NewStore(dir)
	healthy, err := store.OpenRuntime(ctx, "healthy")
	if err != nil {
		t.Fatal(err)
	}
	_ = healthy.Journal.Close()
	badRoot := filepath.Join(dir, "corrupt")
	if err := os.MkdirAll(badRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badRoot, "runtime.sqlite"), []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenRuntime(ctx, "corrupt"); err == nil {
		t.Fatal("expected corrupt session journal to fail")
	}
	reopened, err := store.OpenRuntime(ctx, "healthy")
	if err != nil {
		t.Fatalf("corrupt sibling session affected healthy runtime: %v", err)
	}
	_ = reopened.Journal.Close()
}
