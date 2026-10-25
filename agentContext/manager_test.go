package agentContext

import (
	"LuminaCode/config"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func managerFixture(t *testing.T, limit int, summary SummaryFunc, messages []map[string]any) *ContextManager {
	t.Helper()
	cfg := config.Config{ContextInputTokenLimit: limit, ContextCompressThreshold: 0.8}
	m := NewContextManager(cfg, summary)
	if err := m.Restore(ContextSnapshot{Version: 3, Generation: 1, Messages: MessagesFromMaps("test", messages)}); err != nil {
		t.Fatal(err)
	}
	return m
}

func conversation(n int, size int) []map[string]any {
	var messages []map[string]any
	for i := 0; i < n; i++ {
		role := "assistant"
		if i%2 == 0 {
			role = "user"
		}
		messages = append(messages, map[string]any{"role": role, "content": []map[string]any{{"type": "text", "text": strings.Repeat("evidence. ", size)}}})
	}
	return messages
}

func TestMessagePreservesOpaqueFieldsAndStableIdentity(t *testing.T) {
	input := map[string]any{"role": "assistant", "vendor_flag": json.RawMessage(`{"large":9007199254740993}`), "content": []map[string]any{{"type": "vendor_thinking", "opaque": []any{"a", true}}, {"type": "tool_use", "id": "tool", "name": "read_file", "input": map[string]any{"limit": json.Number("9007199254740993")}}}}
	m := MessageFromMap("session", 2, input)
	got := m.Map()
	again := MessageFromMap("different", 99, got)
	if again.ID != m.ID {
		t.Fatal("identity changed on round trip")
	}
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), "9007199254740993") || !strings.Contains(string(raw), "vendor_thinking") {
		t.Fatalf("opaque fields lost: %s", raw)
	}
}

func TestManagerCountsEntireRequestAndRejectsMandatoryOverflow(t *testing.T) {
	m := managerFixture(t, 200, nil, []map[string]any{{"role": "user", "content": "keep my request"}})
	result, err := m.Build(context.Background(), BuildInput{SystemPrompt: strings.Repeat("system", 200), ToolSchemas: []map[string]any{{"name": "tool", "description": strings.Repeat("schema", 100)}}})
	if !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatalf("expected budget error, got %v", err)
	}
	if result.Compiled.Budget.System == 0 || result.Compiled.Budget.Tools == 0 {
		t.Fatal("fixed request components were not counted")
	}
	if m.Snapshot().Revision != 0 {
		t.Fatal("preparation published state")
	}
}

func TestManagerManualSummaryBelowTriggerPreservesToolEvidence(t *testing.T) {
	messages := conversation(8, 3)
	messages[1] = map[string]any{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "call", "name": "read_file", "input": map[string]any{"path": "important.go"}}}}
	messages[2] = map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "call", "content": "unique tool finding"}}}
	m := managerFixture(t, 10000, func(_ context.Context, system, prompt string) (SummaryResult, error) {
		if !strings.Contains(prompt, "important.go") || !strings.Contains(prompt, "unique tool finding") {
			t.Fatal("tool evidence omitted")
		}
		return SummaryResult{Text: "concise handoff"}, nil
	}, messages)
	result, err := m.Compact(context.Background(), CompactManual, BuildInput{Evidence: MessagesFromMaps("source", messages)})
	if err != nil {
		t.Fatal(err)
	}
	if !result.HistoryReplaced {
		t.Fatal("manual mode should summarize history below the automatic threshold")
	}
	if m.Snapshot().Revision != 0 {
		t.Fatal("uncommitted state leaked")
	}
	if err = m.Commit(result); err != nil {
		t.Fatal(err)
	}
	if err = m.Commit(result); !errors.Is(err, ErrStaleContext) {
		t.Fatalf("stale candidate accepted: %v", err)
	}
}

func TestManagerEmptySummaryAndStreamFailureDoNotReplaceHistory(t *testing.T) {
	for _, failure := range []error{nil, errors.New("stream failed")} {
		m := managerFixture(t, 10000, func(context.Context, string, string) (SummaryResult, error) { return SummaryResult{Text: ""}, failure }, conversation(8, 4))
		result, err := m.Compact(context.Background(), CompactManual)
		if err == nil || result.HistoryReplaced || result.Next.ConsecutiveFailures != 1 {
			t.Fatalf("incorrect failure result: %v %+v", err, result)
		}
		if len(result.Next.Messages) != 8 {
			t.Fatal("failed summary discarded history")
		}
	}
}

func TestManagerL3ContinuesToSummaryWhenTailStillExceedsBudget(t *testing.T) {
	calls := 0
	m := managerFixture(t, 2200, func(context.Context, string, string) (SummaryResult, error) {
		calls++
		return SummaryResult{Text: "handoff"}, nil
	}, conversation(16, 100))
	result, _ := m.Build(context.Background(), BuildInput{})
	if calls != 1 || result.Stats.LevelReached != 4 {
		t.Fatalf("L3 incorrectly deferred L4: calls=%d level=%d", calls, result.Stats.LevelReached)
	}
}

func TestManagerCancellationAndCircuitBreaker(t *testing.T) {
	calls := 0
	m := managerFixture(t, 10000, func(ctx context.Context, _, _ string) (SummaryResult, error) {
		calls++
		return SummaryResult{Text: ""}, ctx.Err()
	}, conversation(8, 4))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Compact(ctx, CompactManual); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s := m.Snapshot()
	s.ConsecutiveFailures = 3
	_ = m.Restore(s)
	_, _ = m.Compact(context.Background(), CompactManual)
	if calls != 0 {
		t.Fatal("circuit breaker called summarizer")
	}
}

func TestSafeTailRetainsCompleteToolPair(t *testing.T) {
	messages := []map[string]any{{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "a"}}}, {"role": "assistant", "content": "between"}, {"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "a"}}}}
	if start := SafeTailStart(messages, 1); start != 0 {
		t.Fatalf("split a tool exchange at %d", start)
	}
}

func TestManagerRestorationFitsRemainingBudget(t *testing.T) {
	m := managerFixture(t, 1500, func(context.Context, string, string) (SummaryResult, error) {
		return SummaryResult{Text: "handoff"}, nil
	}, conversation(8, 10))
	result, err := m.Compact(context.Background(), CompactManual, BuildInput{RestoreFiles: []map[string]string{{"path": "a.go", "content": strings.Repeat("x", 20000)}}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.HistoryReplaced || result.Compiled.Budget.Total > 1500 {
		t.Fatal("file restoration overflowed request")
	}
}

func TestPreparedSummaryCannotCommitAfterReset(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	m := managerFixture(t, 10000, func(context.Context, string, string) (SummaryResult, error) {
		close(entered)
		<-release
		return SummaryResult{Text: "handoff"}, nil
	}, conversation(8, 4))
	finished := make(chan CompactionResult, 1)
	go func() { result, _ := m.Compact(context.Background(), CompactManual); finished <- result }()
	<-entered
	state := m.Snapshot()
	state.Generation++
	state.Revision = 0
	state.Messages = nil
	if err := m.Restore(state); err != nil {
		t.Fatal(err)
	}
	close(release)
	result := <-finished
	if err := m.Commit(result); !errors.Is(err, ErrStaleContext) {
		t.Fatalf("late summary committed: %v", err)
	}
	if len(m.Snapshot().Messages) != 0 {
		t.Fatal("reset history was overwritten")
	}
}

func TestOldCacheCompletionCannotConsumeOrPinNewGeneration(t *testing.T) {
	cfg := config.Config{APIModel: "claude-test", AnthropicCacheEditsEnabled: true}
	m := NewContextManager(cfg, nil)
	original := m.Snapshot().Generation
	next := m.Snapshot()
	next.Generation++
	if err := m.Restore(next); err != nil {
		t.Fatal(err)
	}
	m.QueueCacheEdits([]CacheEdit{NewCacheEdit("current", nil)})
	ids := map[string]struct{}{"current": {}}
	if options := m.ConsumeCacheEditsForGeneration(ids, "claude-test", original); len(options.AnthropicCacheEdits) != 0 {
		t.Fatal("old request consumed new edits")
	}
	m.ConsumeCacheEditsForGeneration(ids, "claude-test", next.Generation)
	m.PinConsumedCacheEdits(original)
	if len(m.CacheEditStateSnapshot().ConsumedInFlight) != 1 {
		t.Fatal("old completion pinned another request's edits")
	}
}

func TestFileRestorationLeavesHeadroomAndDoesNotAccumulate(t *testing.T) {
	m := managerFixture(t, 1500, func(context.Context, string, string) (SummaryResult, error) {
		return SummaryResult{Text: "handoff"}, nil
	}, conversation(8, 10))
	input := BuildInput{RestoreFiles: []map[string]string{{"path": "a.go", "content": strings.Repeat("x", 20000)}}}
	result, err := m.Compact(context.Background(), CompactManual, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Compiled.Budget.Total >= result.Stats.TokensBefore || result.Compiled.Budget.Total > 1200 {
		t.Fatal("restoration erased compression gains or headroom")
	}
	if err = m.Commit(result); err != nil {
		t.Fatal(err)
	}
	auto, err := m.Build(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if auto.Attempted {
		t.Fatal("restoration immediately triggered another compaction")
	}
	second, err := m.Compact(context.Background(), CompactManual, input)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, attachment := range second.Next.Attachments {
		if ids[attachment.ID] {
			t.Fatal("duplicate restoration attachment")
		}
		ids[attachment.ID] = true
	}
}
