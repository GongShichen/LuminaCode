package test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/config"
	coretools "LuminaCode/tools"
)

type instantAbortFailTool struct {
	coretools.BaseTool
}

func newInstantAbortFailTool() *instantAbortFailTool {
	return &instantAbortFailTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:            "fail_abort",
		Description:     "fails and participates in sibling abort",
		InputPrototype:  map[string]any{},
		ReadOnly:        coretools.BoolPtr(true),
		ConcurrencySafe: coretools.BoolPtr(true),
		Destructive:     coretools.BoolPtr(false),
		SiblingAbort:    true,
	}}}
}

func (t *instantAbortFailTool) Execute(_ context.Context, _ coretools.ExecutionContext, _ any) (string, error) {
	return "", errors.New("sibling failure")
}

type siblingBlockingTool struct {
	coretools.BaseTool
	started       chan<- struct{}
	sawAbortEvent chan<- bool
}

func newSiblingBlockingTool(started chan<- struct{}, sawAbortEvent chan<- bool) *siblingBlockingTool {
	return &siblingBlockingTool{
		BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
			Name:            "blocking_abort",
			Description:     "blocks until sibling abort",
			InputPrototype:  map[string]any{},
			ReadOnly:        coretools.BoolPtr(true),
			ConcurrencySafe: coretools.BoolPtr(true),
			Destructive:     coretools.BoolPtr(false),
			SiblingAbort:    true,
		}},
		started:       started,
		sawAbortEvent: sawAbortEvent,
	}
}

func (t *siblingBlockingTool) Execute(ctx context.Context, execCtx coretools.ExecutionContext, _ any) (string, error) {
	_, ok := execCtx["abort_event"].(<-chan struct{})
	t.sawAbortEvent <- ok
	close(t.started)
	<-ctx.Done()
	return "", ctx.Err()
}

type blockingFailTool struct {
	coretools.BaseTool
	started chan<- struct{}
	release <-chan struct{}
}

func newBlockingFailTool(started chan<- struct{}, release <-chan struct{}) *blockingFailTool {
	return &blockingFailTool{
		BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
			Name:            "blocking_fail",
			Description:     "blocks until released then fails",
			InputPrototype:  map[string]any{},
			ReadOnly:        coretools.BoolPtr(false),
			ConcurrencySafe: coretools.BoolPtr(false),
			Destructive:     coretools.BoolPtr(true),
		}},
		started: started,
		release: release,
	}
}

func (t *blockingFailTool) Execute(_ context.Context, _ coretools.ExecutionContext, _ any) (string, error) {
	close(t.started)
	<-t.release
	return "", errors.New("boom")
}

type queuedSafeTool struct {
	coretools.BaseTool
	ran chan<- struct{}
}

func newQueuedSafeTool(ran chan<- struct{}) *queuedSafeTool {
	return &queuedSafeTool{
		BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
			Name:            "queued_safe",
			Description:     "safe tool that should remain queued after cancellation",
			InputPrototype:  map[string]any{},
			ReadOnly:        coretools.BoolPtr(true),
			ConcurrencySafe: coretools.BoolPtr(true),
			Destructive:     coretools.BoolPtr(false),
		}},
		ran: ran,
	}
}

func (t *queuedSafeTool) Execute(_ context.Context, _ coretools.ExecutionContext, _ any) (string, error) {
	t.ran <- struct{}{}
	return "should not run", nil
}

type longSingleResultTool struct {
	coretools.BaseTool
	content string
}

type orderedProbeTool struct {
	coretools.BaseTool
	started chan<- string
	release <-chan struct{}
	result  string
}

func newOrderedProbeTool(name string, safe bool, started chan<- string, release <-chan struct{}) *orderedProbeTool {
	return &orderedProbeTool{
		BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
			Name:            name,
			Description:     "tool ordering probe",
			InputPrototype:  map[string]any{},
			ReadOnly:        coretools.BoolPtr(safe),
			ConcurrencySafe: coretools.BoolPtr(safe),
			Destructive:     coretools.BoolPtr(!safe),
		}},
		started: started,
		release: release,
		result:  name,
	}
}

func (t *orderedProbeTool) Execute(_ context.Context, _ coretools.ExecutionContext, _ any) (string, error) {
	t.started <- t.Name()
	if t.release != nil {
		<-t.release
	}
	return t.result, nil
}

func newLongSingleResultTool(content string) *longSingleResultTool {
	return &longSingleResultTool{
		BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
			Name:            "long_single",
			Description:     "returns one long result",
			InputPrototype:  map[string]any{},
			ReadOnly:        coretools.BoolPtr(true),
			ConcurrencySafe: coretools.BoolPtr(true),
			Destructive:     coretools.BoolPtr(false),
			MaxOutputChars:  1000,
		}},
		content: content,
	}
}

func (t *longSingleResultTool) Execute(_ context.Context, _ coretools.ExecutionContext, _ any) (string, error) {
	return t.content, nil
}

func TestStreamingToolExecutorReturnsWhenOnlyQueuedToolsCannotStartLikePython(t *testing.T) {
	registry := coretools.NewToolRegistry(coretools.NewWriteFileTool())
	cfg := config.NewConfig()
	cfg.SessionDir = t.TempDir()
	state := agent.NewAgentState()
	executor := agent.NewStreamingToolExecutor(registry, cfg, &state, coretools.ExecutionContext{"cwd": t.TempDir()})

	started := executor.AddTool(coretools.ToolCall{
		ID:   "write-1",
		Name: "write_file",
		Input: map[string]any{
			"file_path": "queued.txt",
			"content":   "not yet",
		},
	})
	if started {
		t.Fatal("write_file should queue until permission is granted")
	}

	done := make(chan []map[string]any, 1)
	go func() {
		done <- executor.GetRemainingResults(context.Background())
	}()

	select {
	case results := <-done:
		if len(results) != 0 {
			t.Fatalf("Python returns no results for stalled queued tools, got %#v", results)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetRemainingResults hung with only queued tools and no running tasks")
	}
}

func TestStreamingToolExecutorQueuedExclusiveIsOrderingBarrier(t *testing.T) {
	started := make(chan string, 2)
	releaseWrite := make(chan struct{})
	write := newOrderedProbeTool("write", false, started, releaseWrite)
	read := newOrderedProbeTool("read", true, started, nil)
	executor := agent.NewStreamingToolExecutor(
		coretools.NewToolRegistry(write, read), config.NewConfig(), nil, coretools.ExecutionContext{},
	)

	if executor.AddTool(coretools.ToolCall{ID: "write-1", Name: "write", Input: map[string]any{}}) {
		t.Fatal("exclusive tool must wait for permission")
	}
	if executor.AddTool(coretools.ToolCall{ID: "read-1", Name: "read", Input: map[string]any{}}) {
		t.Fatal("read must not overtake a queued exclusive tool")
	}
	select {
	case name := <-started:
		t.Fatalf("tool %s started before permission", name)
	default:
	}
	if !executor.TryStartQueued("write-1") {
		t.Fatal("permission should start the exclusive tool")
	}
	if name := <-started; name != "write" {
		t.Fatalf("first start = %s, want write", name)
	}
	select {
	case name := <-started:
		t.Fatalf("tool %s crossed the running exclusive barrier", name)
	default:
	}
	close(releaseWrite)
	if name := <-started; name != "read" {
		t.Fatalf("second start = %s, want read", name)
	}
	results := executor.GetRemainingResults(context.Background())
	if len(results) != 2 || results[0]["tool_use_id"] != "write-1" || results[1]["tool_use_id"] != "read-1" {
		t.Fatalf("results must preserve model order: %#v", results)
	}
}

func TestStreamingToolExecutorBuffersParallelResultsInModelOrder(t *testing.T) {
	started := make(chan string, 2)
	releaseFirst := make(chan struct{})
	first := newOrderedProbeTool("first", true, started, releaseFirst)
	second := newOrderedProbeTool("second", true, started, nil)
	executor := agent.NewStreamingToolExecutor(
		coretools.NewToolRegistry(first, second), config.NewConfig(), nil, coretools.ExecutionContext{},
	)

	executor.AddTool(coretools.ToolCall{ID: "first-1", Name: "first", Input: map[string]any{}})
	executor.AddTool(coretools.ToolCall{ID: "second-1", Name: "second", Input: map[string]any{}})
	<-started
	<-started
	time.Sleep(10 * time.Millisecond)
	if results := executor.GetCompletedResults(); len(results) != 0 {
		t.Fatalf("later result must remain buffered behind the first call: %#v", results)
	}
	close(releaseFirst)
	results := executor.GetRemainingResults(context.Background())
	if len(results) != 2 || results[0]["tool_use_id"] != "first-1" || results[1]["tool_use_id"] != "second-1" {
		t.Fatalf("parallel results must preserve model order: %#v", results)
	}
}

func TestStreamingToolExecutorSkipsAggregateBudgetForSingleResultLikePython(t *testing.T) {
	longContent := strings.Repeat("x", 80)
	registry := coretools.NewToolRegistry(newLongSingleResultTool(longContent))
	cfg := config.NewConfig()
	cfg.SessionDir = t.TempDir()
	cfg.MaxMessageToolResultsChars = 20
	state := agent.NewAgentState()
	executor := agent.NewStreamingToolExecutor(registry, cfg, &state, coretools.ExecutionContext{})

	if !executor.AddTool(coretools.ToolCall{ID: "long-1", Name: "long_single", Input: map[string]any{}}) {
		t.Fatal("expected safe tool to start immediately")
	}
	results := executor.GetRemainingResults(context.Background())
	if len(results) != 1 {
		t.Fatalf("expected one result, got %#v", results)
	}
	if got := results[0]["content"]; got != longContent {
		t.Fatalf("single result should not be aggregate-truncated like Python, got %#v", got)
	}
}

func TestStreamingToolExecutorStoresLargeResultsInProjectRuntime(t *testing.T) {
	longContent := strings.Repeat("x", 2000)
	registry := coretools.NewToolRegistry(newLongSingleResultTool(longContent))
	cfg := config.NewConfig()
	cfg.SessionDir = filepath.Join(t.TempDir(), "sessions")
	cfg.ProjectRuntimeDir = filepath.Join(t.TempDir(), "project-runtime")
	state := agent.NewAgentState()
	executor := agent.NewStreamingToolExecutor(registry, cfg, &state, coretools.ExecutionContext{})

	if !executor.AddTool(coretools.ToolCall{ID: "long-runtime-1", Name: "long_single", Input: map[string]any{}}) {
		t.Fatal("expected safe tool to start immediately")
	}
	results := executor.GetRemainingResults(context.Background())
	if len(results) != 1 {
		t.Fatalf("expected one result, got %#v", results)
	}
	if _, err := os.Stat(filepath.Join(cfg.ProjectRuntimeDir, "tool-results")); err != nil {
		t.Fatalf("expected large tool result under project runtime: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.SessionDir, "tool-results")); !os.IsNotExist(err) {
		t.Fatalf("session dir must not contain tool-results, stat err=%v", err)
	}
}

func TestStreamingToolExecutorBreaksWhenCancelledWithQueuedTools(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	ranSafe := make(chan struct{}, 1)
	registry := coretools.NewToolRegistry(
		newBlockingFailTool(started, release),
		newQueuedSafeTool(ranSafe),
	)
	cfg := config.NewConfig()
	cfg.SessionDir = t.TempDir()
	state := agent.NewAgentState()
	executor := agent.NewStreamingToolExecutor(registry, cfg, &state, coretools.ExecutionContext{})

	executor.AddTool(coretools.ToolCall{ID: "fail-1", Name: "blocking_fail", Input: map[string]any{}})
	if !executor.TryStartQueued("fail-1") {
		t.Fatal("expected non-safe tool to start after explicit permission")
	}
	<-started

	startedImmediately := executor.AddTool(coretools.ToolCall{ID: "safe-1", Name: "queued_safe", Input: map[string]any{}})
	if startedImmediately {
		t.Fatal("expected safe tool to queue while non-safe tool is running")
	}

	close(release)
	done := make(chan []map[string]any, 1)
	go func() {
		done <- executor.GetRemainingResults(context.Background())
	}()

	select {
	case results := <-done:
		if len(results) != 1 {
			t.Fatalf("expected only failed tool result, got %#v", results)
		}
		if results[0]["tool_use_id"] != "fail-1" {
			t.Fatalf("expected failed tool result first, got %#v", results)
		}
		content, _ := results[0]["content"].(string)
		if !strings.Contains(content, "boom") {
			t.Fatalf("expected failure content to include tool error, got %q", content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetRemainingResults hung after cancellation with queued tools")
	}

	select {
	case <-ranSafe:
		t.Fatal("queued safe tool should not run after non-safe failure cancellation")
	default:
	}
}

func TestModelTurnCommitsFinalTextAfterTrailingStreamError(t *testing.T) {
	if agent.ShouldCommitFinalTextAfterStreamError(&agent.ModelTurn{StreamHadError: true}) {
		t.Fatal("empty response after stream error should remain an error")
	}
	if !agent.ShouldCommitFinalTextAfterStreamError(&agent.ModelTurn{
		FullText:       "done",
		StreamHadError: true,
	}) {
		t.Fatal("final text followed by a trailing stream error should be committed")
	}
	if agent.ShouldCommitFinalTextAfterStreamError(&agent.ModelTurn{
		FullText:       "need tool",
		StreamHadError: true,
		ToolCalls: []coretools.ToolCall{{
			ID:   "tool-1",
			Name: "bash",
		}},
	}) {
		t.Fatal("tool calls with a stream error should not be treated as a final answer")
	}
}

func TestStreamingToolExecutorOuterDrainLoopBreaksWhenCancelledWithQueuedTools(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	ranSafe := make(chan struct{}, 1)
	registry := coretools.NewToolRegistry(
		newBlockingFailTool(started, release),
		newQueuedSafeTool(ranSafe),
	)
	cfg := config.NewConfig()
	cfg.SessionDir = t.TempDir()
	state := agent.NewAgentState()
	executor := agent.NewStreamingToolExecutor(registry, cfg, &state, coretools.ExecutionContext{})

	executor.AddTool(coretools.ToolCall{ID: "fail-1", Name: "blocking_fail", Input: map[string]any{}})
	if !executor.TryStartQueued("fail-1") {
		t.Fatal("expected non-safe tool to start after explicit permission")
	}
	<-started
	if executor.AddTool(coretools.ToolCall{ID: "safe-1", Name: "queued_safe", Input: map[string]any{}}) {
		t.Fatal("expected safe tool to queue while non-safe tool is running")
	}

	close(release)
	done := make(chan []map[string]any, 1)
	go func() {
		var results []map[string]any
		for executor.HasPendingWork() {
			results = append(results, executor.GetCompletedResults()...)
			if executor.HasPendingWork() {
				executor.WaitForActivity(context.Background())
				if executor.HasPendingWork() && !executor.HasRunningWork() {
					break
				}
			}
		}
		results = append(results, executor.GetCompletedResults()...)
		results = append(results, executor.GetRemainingResults(context.Background())...)
		done <- results
	}()

	select {
	case results := <-done:
		if len(results) != 1 || results[0]["tool_use_id"] != "fail-1" {
			t.Fatalf("expected only failed tool result, got %#v", results)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("outer tool drain loop hung with cancelled executor and queued tools")
	}

	select {
	case <-ranSafe:
		t.Fatal("queued safe tool should not run after cancellation")
	default:
	}
}

func TestStreamingToolExecutorPreservesSiblingAbortResult(t *testing.T) {
	started := make(chan struct{})
	sawAbortEvent := make(chan bool, 1)
	registry := coretools.NewToolRegistry(
		newSiblingBlockingTool(started, sawAbortEvent),
		newInstantAbortFailTool(),
	)
	cfg := config.NewConfig()
	cfg.SessionDir = t.TempDir()
	state := agent.NewAgentState()
	executor := agent.NewStreamingToolExecutor(registry, cfg, &state, coretools.ExecutionContext{})

	if !executor.AddTool(coretools.ToolCall{ID: "block-1", Name: "blocking_abort", Input: map[string]any{}}) {
		t.Fatal("expected blocking sibling tool to start immediately")
	}
	<-started
	if !<-sawAbortEvent {
		t.Fatal("expected executor to inject per-slot abort_event into tool context")
	}
	if !executor.AddTool(coretools.ToolCall{ID: "fail-1", Name: "fail_abort", Input: map[string]any{}}) {
		t.Fatal("expected failing sibling tool to start immediately")
	}

	results := waitForCompletedTool(t, executor, "fail-1")
	executor.AbortSiblings("fail-1")
	results = append(results, executor.GetRemainingResults(context.Background())...)

	var abortContent string
	for _, result := range results {
		if result["tool_use_id"] == "block-1" {
			abortContent, _ = result["content"].(string)
			break
		}
	}
	expected := "Execution aborted — sibling bash tool 'fail_abort' (fail-1) failed."
	if abortContent != expected {
		t.Fatalf("expected preserved sibling abort result %q, got %q from %#v", expected, abortContent, results)
	}
}

func waitForCompletedTool(t *testing.T, executor *agent.StreamingToolExecutor, toolID string) []map[string]any {
	t.Helper()
	deadline := time.After(2 * time.Second)
	var collected []map[string]any
	for {
		batch := executor.GetCompletedResults()
		collected = append(collected, batch...)
		for _, result := range batch {
			if result["tool_use_id"] == toolID {
				return collected
			}
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s to complete", toolID)
		default:
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			executor.WaitForActivity(ctx)
			cancel()
		}
	}
}
