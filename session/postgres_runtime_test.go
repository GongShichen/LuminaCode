package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/harness"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func postgresTestRepository(t *testing.T) (*PostgresRepository, string, string) {
	t.Helper()
	url := os.Getenv("LUMINA_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set LUMINA_TEST_POSTGRES_URL to run PostgreSQL runtime contracts")
	}
	cluster := "runtime-contract-" + uuid.NewString()
	repository, err := NewPostgresRepository(context.Background(), url, cluster)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	return repository, url, cluster
}

func postgresCheckpointFixture(t *testing.T) (RuntimeStore, *agent.AgentRuntime) {
	t.Helper()
	repository, _, _ := postgresTestRepository(t)
	load, err := repository.OpenRuntime(context.Background(), "tenant-a", "session-"+uuid.NewString(), RuntimeOpenOptions{CreateIfMissing: true, FenceToken: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = load.Journal.Close() })
	return checkpointOwner(t, load.Journal)
}

// Run the same recovery and checkpoint contracts against the real SQL backend.
func TestPostgresSessionRecoveryAndConcurrency(t *testing.T) {
	if os.Getenv("LUMINA_TEST_POSTGRES_URL") == "" {
		t.Skip("set LUMINA_TEST_POSTGRES_URL to run PostgreSQL runtime contracts")
	}
	cases := []struct {
		name string
		run  func(*testing.T, func(*testing.T) (RuntimeStore, *agent.AgentRuntime))
	}{
		{"stale_generation_after_clear", runStaleCheckpointCannotResurrectClear},
		{"stale_revision_after_memory_progress", runStaleCheckpointCannotRollbackMemoryCursor},
		{"reset_between_validation_and_append", runResetBetweenCheckpointValidationAndAppendIsRejected},
		{"checkpoint_reset_serialization", runOwnedCheckpointSerializesWithReset},
		{"child_stream_durable_tail", runChildRecoveryReplaysDurableTail},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) { test.run(t, postgresCheckpointFixture) })
	}
}

func TestPostgresConcurrentAppendCompareAndSwap(t *testing.T) {
	repository, _, _ := postgresTestRepository(t)
	ctx := context.Background()
	load, err := repository.OpenRuntime(ctx, "tenant-a", "append-cas", RuntimeOpenOptions{CreateIfMissing: true, FenceToken: 1})
	if err != nil {
		t.Fatal(err)
	}
	head, err := load.Journal.StreamHead(ctx, load.Journal.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	const writers = 24
	start := make(chan struct{})
	var won, conflicted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := load.Journal.Append(ctx, head, harness.PendingEvent{Type: harness.EventRuntimeWarning, Payload: map[string]any{"writer": i}})
			if err == nil {
				won.Add(1)
			} else if errors.Is(err, ErrStreamConflict) {
				conflicted.Add(1)
			} else {
				t.Error(err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if won.Load() != 1 || conflicted.Load() != writers-1 {
		t.Fatalf("CAS winners=%d conflicts=%d", won.Load(), conflicted.Load())
	}
	after, err := load.Journal.StreamHead(ctx, load.Journal.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if after != head+1 {
		t.Fatalf("failed writers changed stream head: before=%d after=%d", head, after)
	}
}

func TestPostgresProjectionLockAcrossConnections(t *testing.T) {
	repository, url, cluster := postgresTestRepository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := repository.OpenRuntime(ctx, "tenant-a", "projection", RuntimeOpenOptions{CreateIfMissing: true, FenceToken: 1})
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewPostgresRepository(ctx, url, cluster)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	second, err := other.OpenRuntime(ctx, "tenant-a", "projection", RuntimeOpenOptions{FenceToken: 1})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 2)
	secondEntered := make(chan struct{})
	go func() {
		finished <- first.Journal.WithProjectionLock(ctx, func() error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	<-entered
	go func() {
		finished <- second.Journal.WithProjectionLock(ctx, func() error { close(secondEntered); return nil })
	}()
	select {
	case <-secondEntered:
		t.Error("second connection bypassed the projection lock")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-secondEntered:
	default:
		t.Fatal("second connection never acquired the released lock")
	}
}

func TestPostgresFailoverFencesOldWriter(t *testing.T) {
	repository, _, _ := postgresTestRepository(t)
	ctx := context.Background()
	first, err := repository.OpenRuntime(ctx, "tenant-a", "failover", RuntimeOpenOptions{CreateIfMissing: true, FenceToken: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, owner := checkpointOwner(t, first.Journal)
	second, err := repository.OpenRuntime(ctx, "tenant-a", "failover", RuntimeOpenOptions{FenceToken: 2})
	if err != nil {
		t.Fatal(err)
	}
	before, err := second.Journal.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = first.Journal.Append(ctx, harness.AnyStreamSeq, harness.PendingEvent{Type: harness.EventRuntimeWarning, Payload: map[string]any{"stale": true}})
	if !errors.Is(err, ErrSessionFenceLost) {
		t.Fatalf("old append was not fenced: %v", err)
	}
	if _, err = owner.Checkpoint(ctx, nil, nil); !errors.Is(err, ErrSessionFenceLost) {
		t.Fatalf("old checkpoint was not fenced: %v", err)
	}
	if err = first.Journal.SaveConsumerOffset(ctx, "stale", 99); !errors.Is(err, ErrSessionFenceLost) {
		t.Fatalf("old cursor update was not fenced: %v", err)
	}
	after, err := second.Journal.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("stale owner changed the durable event log")
	}
	if _, err = second.Journal.Append(ctx, harness.AnyStreamSeq, harness.PendingEvent{Type: harness.EventRuntimeWarning, Payload: map[string]any{"new_owner": true}}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresRecoveryAfterConnectionReopen(t *testing.T) {
	repository, url, cluster := postgresTestRepository(t)
	ctx := context.Background()
	load, err := repository.OpenRuntime(ctx, "tenant-a", "restart", RuntimeOpenOptions{CreateIfMissing: true, FenceToken: 1})
	if err != nil {
		t.Fatal(err)
	}
	initial := agent.NewAgentState()
	initial.PermissionState.YoloMode = true
	initial.TotalInputTokens = 100
	initial.Messages = []map[string]any{{"role": "user", "content": "preserve the original goal"}}
	if err = AppendRuntimeState(ctx, load.Journal, &initial, nil, nil); err != nil {
		t.Fatal(err)
	}
	response, _ := json.Marshal(map[string]any{"role": "assistant", "context_id": "durable-response", "content": "already committed response"})
	_, err = load.Journal.Append(ctx, harness.AnyStreamSeq,
		harness.PendingEvent{Type: harness.EventRunStarted, Payload: harness.RunLifecyclePayload{RunID: "interrupted-run"}},
		harness.PendingEvent{Type: harness.EventStepStarted, Payload: map[string]any{"run_id": "interrupted-run", "step_id": "step-1", "step_no": 1}},
		harness.PendingEvent{Type: harness.EventMessageAssistantAppended, Payload: harness.MessageAppendedPayload{Message: response}},
		harness.PendingEvent{Type: harness.EventUsageRecorded, Payload: harness.UsageRecordedPayload{RequestID: "response-usage", InputTokens: 7, OutputTokens: 3}},
		harness.PendingEvent{Type: harness.EventToolCallPlanned, Payload: harness.ToolLifecyclePayload{ToolCallID: "interrupted-tool", Name: "read_file", Input: json.RawMessage(`{"file_path":"actual.go","opaque_id":9007199254740993}`)}},
		harness.PendingEvent{Type: harness.EventToolExecutionStarted, Payload: harness.ToolLifecyclePayload{ToolCallID: "interrupted-tool", Name: "read_file"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	repository.Close()
	reopened, err := NewPostgresRepository(ctx, url, cluster)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := reopened.OpenRuntime(ctx, "tenant-a", "restart", RuntimeOpenOptions{FenceToken: 1})
	if err != nil {
		t.Fatal(err)
	}
	if restored.State == nil || !restored.State.YoloEnabled() || restored.State.TotalInputTokens != 107 || restored.State.TotalOutputTokens != 3 {
		t.Fatalf("restart lost permissions or usage: %+v", restored.State)
	}
	if len(restored.State.Messages) != 4 || len(restored.State.Transcript) != 4 {
		t.Fatalf("restart lost durable messages/tool exchange: messages=%d original=%d", len(restored.State.Messages), len(restored.State.Transcript))
	}
	call := restored.State.Messages[2]["content"].([]map[string]any)[0]
	input := call["input"].(map[string]any)
	if call["name"] != "read_file" || input["file_path"] != "actual.go" || input["opaque_id"] != json.Number("9007199254740993") {
		t.Fatalf("PostgreSQL recovery changed tool arguments: %+v", call)
	}
	events, err := restored.Journal.Load(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, event := range events {
		counts[event.Type]++
	}
	for _, kind := range []string{harness.EventRunInterrupted, harness.EventStepInterrupted, harness.EventToolExecutionInterrupted} {
		if counts[kind] != 1 {
			t.Fatalf("%s count=%d", kind, counts[kind])
		}
	}
	head, err := restored.Journal.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again, err := reopened.OpenRuntime(ctx, "tenant-a", "restart", RuntimeOpenOptions{FenceToken: 1})
	if err != nil {
		t.Fatal(err)
	}
	after, err := again.Journal.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != head || again.State.TotalInputTokens != 107 {
		t.Fatal("reopening duplicated recovery events or usage")
	}
	if err = again.Journal.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("recovered session %s with %d messages and a complete interrupted lifecycle", again.Journal.SessionID(), len(again.State.Messages))
}

func TestPostgresConcurrentCheckpointsAcrossHandles(t *testing.T) {
	repository, _, _ := postgresTestRepository(t)
	limited := repository.pool.Config()
	limited.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(context.Background(), limited)
	if err != nil {
		t.Fatal(err)
	}
	repository.pool.Close()
	repository.pool = pool
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	load, err := repository.OpenRuntime(ctx, "tenant-a", "multi-checkpoint", RuntimeOpenOptions{CreateIfMissing: true, FenceToken: 1})
	if err != nil {
		t.Fatal(err)
	}
	state := agent.NewAgentState()
	state.Messages = []map[string]any{{"role": "user", "content": "unchanged state"}}
	if err = AppendRuntimeState(ctx, load.Journal, &state, nil, nil); err != nil {
		t.Fatal(err)
	}
	current, _, _, err := LoadRuntimeState(ctx, load.Journal)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	handles := make([]RuntimeStore, 0, workers)
	for i := 0; i < workers; i++ {
		opened, err := repository.OpenRuntime(ctx, "tenant-a", "multi-checkpoint", RuntimeOpenOptions{FenceToken: 1})
		if err != nil {
			t.Fatal(err)
		}
		handles = append(handles, opened.Journal)
	}
	seed := agent.SnapshotFromState(current)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, handle := range handles {
		wg.Add(1)
		go func(i int, handle RuntimeStore) {
			defer wg.Done()
			<-start
			local := seed.LegacyState()
			if err := AppendRuntimeState(ctx, handle, &local, map[string]any{"writer": fmt.Sprint(i)}, nil); err != nil {
				t.Error(err)
			}
		}(i, handle)
	}
	close(start)
	wg.Wait()
	restored, _, _, err := LoadRuntimeState(context.Background(), load.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Messages) != 1 {
		t.Fatal("parallel checkpoints damaged state")
	}
}

func TestPostgresCancelledProjectionLockDoesNotLeak(t *testing.T) {
	repository, url, cluster := postgresTestRepository(t)
	ctx := context.Background()
	first, err := repository.OpenRuntime(ctx, "tenant-a", "cancel-lock", RuntimeOpenOptions{CreateIfMissing: true, FenceToken: 1})
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewPostgresRepository(ctx, url, cluster)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	second, err := other.OpenRuntime(ctx, "tenant-a", "cancel-lock", RuntimeOpenOptions{FenceToken: 1})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- first.Journal.WithProjectionLock(ctx, func() error { close(entered); <-release; return nil })
	}()
	<-entered
	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	err = second.Journal.WithProjectionLock(waitCtx, func() error { t.Error("cancelled waiter entered the lock"); return nil })
	cancel()
	close(release)
	if lockErr := <-finished; lockErr != nil {
		t.Fatal(lockErr)
	}
	if err == nil {
		t.Fatal("blocked lock acquisition ignored cancellation")
	}
	followCtx, followCancel := context.WithTimeout(ctx, 2*time.Second)
	defer followCancel()
	if err = second.Journal.WithProjectionLock(followCtx, func() error { return nil }); err != nil {
		t.Fatalf("cancelled waiter leaked an advisory lock: %v", err)
	}
}
