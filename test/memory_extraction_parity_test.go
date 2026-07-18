package test

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/config"
	"LuminaCode/longmemory"
	coretools "LuminaCode/tools"
)

func TestLongTermExtractionPromptParserAndResultFormatting(t *testing.T) {
	messages := []map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "text", "text": "remember my preference"}}},
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "name": "read_file", "id": "t1"}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "content": strings.Repeat("x", 220)}}},
	}
	prompt := agent.BuildLongTermExtractionPrompt(messages, nil)
	if !strings.Contains(prompt, "Call ExtractMemoryBatch exactly once") ||
		!strings.Contains(prompt, "remember my preference") ||
		!strings.Contains(prompt, "[tool: read_file id=t1]") ||
		!strings.Contains(prompt, "...(truncated)") ||
		!strings.Contains(prompt, "scope_type") {
		t.Fatalf("unexpected long-term extraction prompt: %q", prompt)
	}
	parsed := agent.ParseLongTermMemoryCandidates(`{"memories":[{"scope_type":"project","memory_type":"feedback","title":"Preference","content":"remember my preference"}]}`)
	if len(parsed) != 1 || parsed[0].Title != "Preference" {
		t.Fatalf("unexpected parsed candidates: %#v", parsed)
	}
	formatted := agent.FormatExtractionResult(" saved preference ")
	if formatted != "<system-reminder note=\"auto-memory\">\nBackground memory extraction completed:\nsaved preference\n</system-reminder>" {
		t.Fatalf("unexpected formatted result: %q", formatted)
	}
	if empty := agent.FormatExtractionResult("  "); empty != "" {
		t.Fatalf("expected empty result for blank summary, got %q", empty)
	}

	unicodePrompt := agent.BuildLongTermExtractionPrompt([]map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "content": strings.Repeat("界", 201)}}},
	}, nil)
	if !strings.Contains(unicodePrompt, "[tool_result: "+strings.Repeat("界", 200)+"...(truncated)]") {
		t.Fatalf("tool_result preview should truncate by Python characters, got %q", unicodePrompt)
	}
	longSummary := strings.Repeat("记", 501)
	formattedLong := agent.FormatExtractionResult(longSummary)
	if !strings.Contains(formattedLong, strings.Repeat("记", 500)+"\n</system-reminder>") || strings.Contains(formattedLong, strings.Repeat("记", 501)) {
		t.Fatalf("formatted extraction result should truncate by Python characters, got %q", formattedLong)
	}
}

func TestExtractionControllerThrottleAndRunner(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig()
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = filepath.Join(dir, "memory.sqlite")
	cfg.MemoryBackgroundExtractionInterval = 5
	store, err := longmemory.Open(context.Background(), cfg.LongTermMemoryStore)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Upsert(context.Background(), longmemory.Candidate{ScopeType: longmemory.ScopeProject, ScopeKey: longmemory.ProjectScopeKey(dir), MemoryType: longmemory.TypeSemantic, Title: "Existing", Content: "Old memory"}); err != nil {
		t.Fatal(err)
	}
	state := agent.NewAgentState()
	state.UserTurnCount = 4
	controller := agent.NewExtractionController(cfg, coretools.NewToolRegistry(), agent.ExtractionConfig{
		TurnsBetweenExtractions: 5,
		MaxExtractionTurns:      5,
		ContextMessageCount:     8,
	})
	if controller.ShouldExtract(&state) {
		t.Fatalf("expected throttle before five user turns")
	}
	state.UserTurnCount = 5
	state.Messages = []map[string]any{{"role": "user", "id": "m1", "content": []map[string]any{{"type": "text", "text": "save this"}}}}
	var called atomic.Bool
	controller.Runner = func(ctx context.Context, prompt, systemPrompt string, filteredRegistry *coretools.ToolRegistry, extraContext coretools.ExecutionContext) (string, error) {
		called.Store(true)
		if !strings.Contains(prompt, "Existing") || !strings.Contains(prompt, "save this") {
			t.Fatalf("unexpected prompt: %q", prompt)
		}
		if filteredRegistry.Get("write_file") != nil || filteredRegistry.Get("edit_file") != nil {
			t.Fatalf("long-term extractor should not get write tools")
		}
		return `{"memories":[{"scope_type":"project","memory_type":"semantic","title":"Saved","content":"Saved memory","importance":0.8,"confidence":0.9}],"evidence_spans":[{"memory_index":0,"message_id":"m1","text":"save this"}]}`, nil
	}
	if !controller.Schedule(context.Background(), &state, dir) {
		t.Fatalf("expected extraction to launch")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !controller.HasPendingResult() {
		time.Sleep(10 * time.Millisecond)
	}
	if !called.Load() {
		t.Fatalf("runner was not called")
	}
	result := controller.ConsumeResult()
	if !strings.Contains(result, "saved long-term memories") {
		t.Fatalf("unexpected extraction result: %q", result)
	}
	if state.LastExtractionUserTurn != 5 || state.MemoryWritesSinceExtraction {
		t.Fatalf("state was not updated after extraction: %#v", state)
	}
}

func TestExtractionControllerAppliesCandidateActions(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfigForCWD(dir)
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = filepath.Join(dir, "memory.sqlite")
	cfg.MemoryBackgroundExtractionInterval = 1
	store, err := longmemory.Open(context.Background(), cfg.LongTermMemoryStore)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := store.Upsert(context.Background(), longmemory.Candidate{
		ScopeType:  longmemory.ScopeProject,
		ScopeKey:   longmemory.ProjectScopeKey(dir),
		MemoryType: longmemory.TypeProject,
		Title:      "Old Build Rule",
		Content:    "Run only go test.",
	})
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	state := agent.NewAgentState()
	state.UserTurnCount = 1
	state.Messages = []map[string]any{{"role": "user", "id": "m2", "content": "replace the build rule"}}
	controller := agent.NewExtractionController(cfg, coretools.NewToolRegistry(), agent.ExtractionConfig{TurnsBetweenExtractions: 1, ContextMessageCount: 8})
	controller.Runner = func(ctx context.Context, prompt, systemPrompt string, filteredRegistry *coretools.ToolRegistry, extraContext coretools.ExecutionContext) (string, error) {
		if !strings.Contains(prompt, "action") || !strings.Contains(prompt, "target_memory_id") {
			t.Fatalf("prompt should expose memory actions, got %q", prompt)
		}
		return `{"memories":[
			{"action":"ignore","scope_type":"project","memory_type":"semantic","title":"Duplicate","content":"duplicate"},
			{"action":"supersede","target_memory_id":"` + existing.MemoryID + `","scope_type":"project","memory_type":"project","title":"Build Rule","content":"Run go test ./... and npm test before commit.","importance":0.9,"confidence":0.9}
		],"evidence_spans":[{"memory_index":1,"message_id":"m2","text":"replace the build rule"}]}`, nil
	}
	if !controller.Schedule(context.Background(), &state, dir) {
		t.Fatalf("expected extraction to launch")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !controller.HasPendingResult() {
		time.Sleep(10 * time.Millisecond)
	}
	_ = controller.ConsumeResult()
	verify, err := longmemory.Open(context.Background(), cfg.LongTermMemoryStore)
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Close()
	old, err := verify.Get(context.Background(), existing.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != longmemory.StatusSuperseded || old.SupersededBy == "" {
		t.Fatalf("old memory should be superseded, got %#v", old)
	}
	found, err := verify.Search(context.Background(), longmemory.SearchOptions{Query: "npm test before commit", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].MemoryID != old.SupersededBy || strings.Contains(found[0].Content, "duplicate") {
		t.Fatalf("expected only replacement memory to be searchable, got %#v", found)
	}
}

func TestExtractionControllerPreservesExplicitZeroConfigLikePython(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig()
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = filepath.Join(dir, "memory.sqlite")
	cfg.MemoryBackgroundExtractionInterval = 1
	state := agent.NewAgentState()
	state.UserTurnCount = 1
	state.Messages = []map[string]any{
		{"role": "user", "content": "first"},
		{"role": "assistant", "content": "second"},
	}
	controller := agent.NewExtractionController(cfg, coretools.NewToolRegistry(), agent.ExtractionConfig{
		TurnsBetweenExtractions: 0,
		MaxExtractionTurns:      0,
		ContextMessageCount:     0,
	})
	if !controller.ShouldExtract(&state) {
		t.Fatalf("memory_background_extraction_turn_interval=1 should allow extraction")
	}
	done := make(chan struct{})
	controller.Runner = func(ctx context.Context, prompt, systemPrompt string, filteredRegistry *coretools.ToolRegistry, extraContext coretools.ExecutionContext) (string, error) {
		defer close(done)
		if !strings.Contains(prompt, "first") || !strings.Contains(prompt, "second") {
			t.Fatalf("context_message_count=0 should keep all messages like Python slicing, got %q", prompt)
		}
		if !strings.Contains(prompt, "Call ExtractMemoryBatch exactly once") {
			t.Fatalf("long-term extraction prompt should request a structured tool call, got %q", prompt)
		}
		matches := regexp.MustCompile(`message_id=([^\]]+)`).FindStringSubmatch(prompt)
		if len(matches) != 2 {
			t.Fatalf("missing source message id in extraction prompt: %q", prompt)
		}
		return `{"memories":[{"scope_type":"project","memory_type":"semantic","title":"Zero","content":"zero config run","source_message_ids":["` + matches[1] + `"]}]}`, nil
	}
	if !controller.Schedule(context.Background(), &state, dir) {
		t.Fatalf("explicit zero config should schedule extraction")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("extraction runner was not called")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !controller.HasPendingResult() {
		time.Sleep(10 * time.Millisecond)
	}
	if !controller.HasPendingResult() {
		t.Fatalf("extraction did not finish controller bookkeeping")
	}
	_ = controller.ConsumeResult()
}

func TestExtractionControllerEmptyResultStillUpdatesStateLikePython(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig()
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = filepath.Join(dir, "memory.sqlite")
	cfg.MemoryBackgroundExtractionInterval = 5
	state := agent.NewAgentState()
	state.TurnCount = 7
	state.UserTurnCount = 5
	state.Messages = []map[string]any{{"role": "user", "content": "nothing durable here"}}
	controller := agent.NewExtractionController(cfg, coretools.NewToolRegistry(), agent.ExtractionConfig{
		TurnsBetweenExtractions: 5,
		MaxExtractionTurns:      5,
		ContextMessageCount:     8,
	})
	controller.Runner = func(ctx context.Context, prompt, systemPrompt string, filteredRegistry *coretools.ToolRegistry, extraContext coretools.ExecutionContext) (string, error) {
		return "   ", nil
	}
	if !controller.Schedule(context.Background(), &state, dir) {
		t.Fatalf("expected extraction to launch")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !controller.HasPendingResult() {
		time.Sleep(10 * time.Millisecond)
	}
	if !controller.HasPendingResult() {
		t.Fatalf("empty formatted result should still be pending like Python")
	}
	if result := controller.ConsumeResult(); !strings.Contains(result, "saved nothing") {
		t.Fatalf("blank long-term extraction should report saved nothing, got %q", result)
	}
	if state.LastExtractionTurn != 7 || state.LastExtractionUserTurn != 5 || state.MemoryWritesSinceExtraction {
		t.Fatalf("empty extraction should still update throttle state like Python, got %#v", state)
	}
}

func TestExtractionControllerStoresPendingWhileRunning(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig()
	state := agent.NewAgentState()
	state.UserTurnCount = 10
	state.Messages = []map[string]any{{"role": "user", "content": "first"}}
	controller := agent.NewExtractionController(cfg, coretools.NewToolRegistry(), agent.ExtractionConfig{
		TurnsBetweenExtractions: 1,
		MaxExtractionTurns:      5,
		ContextMessageCount:     8,
	})
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	controller.Runner = func(ctx context.Context, prompt, systemPrompt string, filteredRegistry *coretools.ToolRegistry, extraContext coretools.ExecutionContext) (string, error) {
		call := calls.Add(1)
		if call == 1 {
			close(started)
			<-release
		}
		return "run complete", nil
	}
	if !controller.Schedule(context.Background(), &state, dir) {
		t.Fatalf("expected first extraction to launch")
	}
	<-started
	state.UserTurnCount = 11
	state.Messages = []map[string]any{{"role": "user", "content": "second"}}
	if controller.Schedule(context.Background(), &state, dir) {
		t.Fatalf("second schedule should store pending context")
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (calls.Load() < 2 || state.LastExtractionUserTurn != 11) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected trailing extraction run, got %d", calls.Load())
	}
	if state.LastExtractionUserTurn != 11 {
		t.Fatalf("expected trailing extraction to finish state update, got %d", state.LastExtractionUserTurn)
	}
}

func TestExtractionControllerRunsDetachedFromScheduleContextLikePython(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig()
	state := agent.NewAgentState()
	state.UserTurnCount = 5
	state.Messages = []map[string]any{{"role": "user", "content": "extract"}}
	controller := agent.NewExtractionController(cfg, coretools.NewToolRegistry(), agent.ExtractionConfig{
		TurnsBetweenExtractions: 1,
		MaxExtractionTurns:      5,
		ContextMessageCount:     8,
	})
	started := make(chan struct{})
	release := make(chan struct{})
	controller.Runner = func(ctx context.Context, prompt, systemPrompt string, filteredRegistry *coretools.ToolRegistry, extraContext coretools.ExecutionContext) (string, error) {
		close(started)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-release:
			return "detached extraction complete", nil
		}
	}
	parentCtx, cancel := context.WithCancel(context.Background())
	if !controller.Schedule(parentCtx, &state, dir) {
		t.Fatalf("expected extraction to launch")
	}
	<-started
	cancel()
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !controller.HasPendingResult() {
		time.Sleep(10 * time.Millisecond)
	}
	if !controller.HasPendingResult() {
		t.Fatalf("background extraction should outlive the scheduling context like Python create_task")
	}
	if got := controller.ConsumeResult(); !strings.Contains(got, "saved nothing") {
		t.Fatalf("unexpected extraction result: %q", got)
	}
}

func TestExtractionControllerCancelCancelsRunningTask(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig()
	state := agent.NewAgentState()
	state.UserTurnCount = 5
	state.Messages = []map[string]any{{"role": "user", "content": "extract"}}
	controller := agent.NewExtractionController(cfg, coretools.NewToolRegistry(), agent.ExtractionConfig{
		TurnsBetweenExtractions: 1,
		MaxExtractionTurns:      5,
		ContextMessageCount:     8,
	})
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var calls atomic.Int32
	controller.Runner = func(ctx context.Context, prompt, systemPrompt string, filteredRegistry *coretools.ToolRegistry, extraContext coretools.ExecutionContext) (string, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-ctx.Done()
		if calls.Load() == 1 {
			close(cancelled)
		}
		return "should not surface", ctx.Err()
	}
	if !controller.Schedule(context.Background(), &state, dir) {
		t.Fatalf("expected extraction to launch")
	}
	<-started
	controller.Cancel()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatalf("running extraction was not cancelled")
	}
	if controller.HasPendingResult() {
		t.Fatalf("cancelled extraction should not leave a result")
	}
	if !controller.Schedule(context.Background(), &state, dir) {
		t.Fatalf("controller should accept new extraction after cancel")
	}
	controller.Cancel()
}

func TestExtractionControllerPersistsFailureAndReplaysUncommittedCursor(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfigForCWD(dir)
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = filepath.Join(dir, "memory.sqlite")
	cfg.MemoryBackgroundExtractionInterval = 1
	state := agent.NewAgentState()
	state.MemorySessionID = "resume-session"
	state.UserTurnCount = 1
	state.Messages = []map[string]any{{"role": "user", "id": "message-1", "content": "Use pnpm for this project."}}

	failing := agent.NewExtractionController(cfg, coretools.NewToolRegistry(), agent.ExtractionConfig{TurnsBetweenExtractions: 1})
	failing.SourceSessionID = "resume-session"
	failureReturned := make(chan struct{})
	failing.Runner = func(context.Context, string, string, *coretools.ToolRegistry, coretools.ExecutionContext) (string, error) {
		close(failureReturned)
		return "", errors.New("temporary extraction failure")
	}
	if !failing.Schedule(context.Background(), &state, "") {
		t.Fatal("expected failed extraction to be scheduled")
	}
	select {
	case <-failureReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("failed extraction runner was not called")
	}
	deadline := time.Now().Add(2 * time.Second)
	var jobs []longmemory.Job
	for time.Now().Before(deadline) {
		store, err := longmemory.Open(context.Background(), cfg.LongTermMemoryStore)
		if err == nil {
			jobs, _ = store.ListJobs(context.Background(), []string{"extraction"}, nil, 10)
			_ = store.Close()
		}
		if len(jobs) > 0 && jobs[0].Status == "retry" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(jobs) != 1 || jobs[0].Status != "retry" || !strings.Contains(jobs[0].LastError, "temporary extraction failure") {
		t.Fatalf("failed extraction was not persisted for retry: %#v", jobs)
	}
	if state.MemoryExtractionCursor != 0 {
		t.Fatalf("failed extraction advanced state cursor to %d", state.MemoryExtractionCursor)
	}

	retry := agent.NewExtractionController(cfg, coretools.NewToolRegistry(), agent.ExtractionConfig{TurnsBetweenExtractions: 1})
	retry.SourceSessionID = "resume-session"
	retry.Runner = func(_ context.Context, prompt, _ string, _ *coretools.ToolRegistry, _ coretools.ExecutionContext) (string, error) {
		if !strings.Contains(prompt, "Use pnpm for this project.") {
			t.Fatalf("retry did not replay the uncommitted message: %s", prompt)
		}
		return `{"memories":[{"scope_type":"project","memory_type":"project","title":"Package manager","content":"Use pnpm for this project."}],"evidence_spans":[{"memory_index":0,"message_id":"message-1","text":"Use pnpm for this project."}]}`, nil
	}
	if !retry.Schedule(context.Background(), &state, "") {
		t.Fatal("expected extraction retry to be scheduled")
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !retry.HasPendingResult() {
		time.Sleep(10 * time.Millisecond)
	}
	if !retry.HasPendingResult() || state.MemoryExtractionCursor != 1 {
		t.Fatalf("retry did not commit and advance cursor: result=%t cursor=%d", retry.HasPendingResult(), state.MemoryExtractionCursor)
	}
}
