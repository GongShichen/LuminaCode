package agent_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/config"
	"LuminaCode/harness"
)

type recordingEventStore struct {
	events []harness.Event
}

func TestRuntimeAssemblyAttachesIntegrationHooksAndCapabilities(t *testing.T) {
	cfg := config.NewConfigForCWD(t.TempDir())
	engine := newTestQueryEngine(cfg)
	defer engine.Shutdown()
	assembly, err := agent.NewRuntimeAssembly("session-1", nil, engine.CoreEngine.Registry)
	if err != nil {
		t.Fatal(err)
	}
	defer assembly.Close()
	if err := engine.CoreEngine.AttachRuntime(assembly); err != nil {
		t.Fatal(err)
	}
	description := assembly.Describe()
	hooks := description["hooks"].(map[string]int)
	for _, name := range []string{"memory_preparing", "skill_preparing", "mcp_preparing"} {
		if hooks[name] != 1 {
			t.Fatalf("hook %s was not installed: %#v", name, hooks)
		}
	}
	for _, capability := range []harness.CapabilityKey{agent.CapabilityMemory, agent.CapabilityMCP, agent.CapabilitySubagent, agent.CapabilitySandbox} {
		if _, ok := assembly.Session.Resolve(capability); !ok {
			t.Fatalf("capability %s was not attached", capability)
		}
	}
}

func (s *recordingEventStore) CreateStream(context.Context, harness.StreamDescriptor) error {
	return nil
}

func (s *recordingEventStore) Append(_ context.Context, _ int64, pending ...harness.PendingEvent) ([]harness.Event, error) {
	created := make([]harness.Event, 0, len(pending))
	for _, item := range pending {
		payload, err := json.Marshal(item.Payload)
		if err != nil {
			return nil, err
		}
		seq := int64(len(s.events) + 1)
		event := harness.Event{Seq: seq, StreamSeq: seq, ID: item.ID, StreamID: item.StreamID,
			Type: item.Type, SchemaVersion: 1, OccurredAt: time.Unix(seq, 0), CorrelationID: item.CorrelationID,
			Audience: item.Audience, Payload: payload}
		s.events = append(s.events, event)
		created = append(created, event)
	}
	return created, nil
}

func (s *recordingEventStore) Load(_ context.Context, afterSeq int64, limit int) ([]harness.Event, error) {
	var out []harness.Event
	for _, event := range s.events {
		if event.Seq > afterSeq && (limit <= 0 || len(out) < limit) {
			out = append(out, event)
		}
	}
	return out, nil
}

func (s *recordingEventStore) LoadStream(ctx context.Context, _ string, afterSeq int64, limit int) ([]harness.Event, error) {
	return s.Load(ctx, afterSeq, limit)
}

func (s *recordingEventStore) Head(context.Context) (int64, error) {
	return int64(len(s.events)), nil
}

func (s *recordingEventStore) SaveCheckpoint(context.Context, harness.Checkpoint) error { return nil }
func (s *recordingEventStore) LoadCheckpoint(context.Context, string, string) (*harness.Checkpoint, error) {
	return nil, nil
}
func (s *recordingEventStore) SaveCommandResult(context.Context, harness.CommandResult) error {
	return nil
}
func (s *recordingEventStore) GetCommandResult(context.Context, string) (*harness.CommandResult, error) {
	return nil, nil
}
func (s *recordingEventStore) Close() error { return nil }

func TestRuntimeEventRecorderClosesEveryStepBeforeRun(t *testing.T) {
	store := &recordingEventStore{}
	recorder := agent.NewRuntimeEventRecorder(store, "session-1")
	ctx := context.Background()
	if err := recorder.BeginRun(ctx, map[string]any{"role": "user", "content": "go"}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.BeginStep(ctx); err != nil {
		t.Fatal(err)
	}
	if err := recorder.BeginStep(ctx); err != nil {
		t.Fatal(err)
	}
	if err := recorder.EndRun(ctx, "completed", ""); err != nil {
		t.Fatal(err)
	}
	want := []string{
		harness.EventMessageUserAppended, harness.EventRunStarted,
		harness.EventStepStarted, harness.EventStepCompleted,
		harness.EventStepStarted, harness.EventStepCompleted,
		harness.EventRunCompleted,
	}
	if len(store.events) != len(want) {
		t.Fatalf("events = %d, want %d: %#v", len(store.events), len(want), store.events)
	}
	for index, eventType := range want {
		if store.events[index].Type != eventType {
			t.Fatalf("event[%d] = %s, want %s", index, store.events[index].Type, eventType)
		}
	}
}

func TestTaskRuntimeEmitsDurableLifecycleOutsideUIRenderCycle(t *testing.T) {
	store := &recordingEventStore{}
	recorder := agent.NewRuntimeEventRecorder(store, "session-1")
	runtime := agent.NewAgentTaskRuntime()
	runtime.SetTaskEventObserver(recorder)
	record := runtime.RegisterForegroundTask("task-1", "", "main", "worker", "verify", "general-purpose")
	runtime.CompleteForegroundTask(record, "completed")
	if len(store.events) != 2 {
		t.Fatalf("lifecycle events = %d, want 2: %#v", len(store.events), store.events)
	}
	if store.events[0].Type != harness.EventTaskCreated || store.events[1].Type != harness.EventTaskStatusChanged {
		t.Fatalf("unexpected task lifecycle: %s, %s", store.events[0].Type, store.events[1].Type)
	}
}
