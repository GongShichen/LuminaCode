package test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/config"
	"LuminaCode/memory"
	coretools "LuminaCode/tools"
)

func TestExtractionPromptAndResultFormatting(t *testing.T) {
	messages := []map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "text", "text": "remember my preference"}}},
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "name": "read_file", "id": "t1"}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "content": strings.Repeat("x", 220)}}},
	}
	prompt := agent.BuildExtractionPrompt(messages, "- existing", 5)
	if !strings.Contains(prompt, "## Existing MEMORY.md index") ||
		!strings.Contains(prompt, "remember my preference") ||
		!strings.Contains(prompt, "[tool: read_file id=t1]") ||
		!strings.Contains(prompt, "...(truncated)") ||
		!strings.Contains(prompt, "Be selective — only extract insights") ||
		!strings.Contains(prompt, "You have 5 turns maximum") {
		t.Fatalf("unexpected extraction prompt: %q", prompt)
	}
	formatted := agent.FormatExtractionResult(" saved preference ")
	if formatted != "<system-reminder note=\"auto-memory\">\nBackground memory extraction completed:\nsaved preference\n</system-reminder>" {
		t.Fatalf("unexpected formatted result: %q", formatted)
	}
	if empty := agent.FormatExtractionResult("  "); empty != "" {
		t.Fatalf("expected empty result for blank summary, got %q", empty)
	}

	unicodePrompt := agent.BuildExtractionPrompt([]map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "content": strings.Repeat("界", 201)}}},
	}, "- existing", 5)
	if !strings.Contains(unicodePrompt, "[tool_result: "+strings.Repeat("界", 200)+"...(truncated)]") {
		t.Fatalf("tool_result preview should truncate by Python characters, got %q", unicodePrompt)
	}
	longSummary := strings.Repeat("记", 501)
	formattedLong := agent.FormatExtractionResult(longSummary)
	if !strings.Contains(formattedLong, strings.Repeat("记", 500)+"\n</system-reminder>") || strings.Contains(formattedLong, strings.Repeat("记", 501)) {
		t.Fatalf("formatted extraction result should truncate by Python characters, got %q", formattedLong)
	}
}

func TestBuildExtractionRegistryUsesPythonSafeAllowlist(t *testing.T) {
	base := coretools.NewToolRegistry(
		coretools.NewReadFileTool(),
		coretools.NewWriteFileTool(),
		coretools.NewEditFileTool(),
		coretools.NewGrepSearchTool(),
		coretools.NewGlobMatchTool(),
		coretools.NewBashTool(),
		coretools.NewToolSearchTool(),
	)
	filtered := agent.BuildExtractionRegistry(base)
	for _, name := range []string{"read_file", "write_file", "edit_file", "grep_search", "glob_match", "run_shell"} {
		if filtered.Get(name) == nil {
			t.Fatalf("extraction registry missing allowed tool %s", name)
		}
	}
	if filtered.Get("tool_search") != nil {
		t.Fatalf("extraction registry should exclude tools outside Python allowlist")
	}
}

func TestExtractionControllerThrottleAndRunner(t *testing.T) {
	dir := t.TempDir()
	store := memory.NewMemoryStore(dir)
	if _, err := store.SaveEntry(&memory.MemoryEntry{Name: "Existing", Description: "old", Metadata: map[string]any{"type": "user"}, Content: "Old memory"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfig()
	cfg.AutoMemoryDirectory = &dir
	cfg.MemoryExtractionPromptPath = filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(cfg.MemoryExtractionPromptPath, []byte("system prompt"), 0o644); err != nil {
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
	state.Messages = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "save this"}}}}
	var called atomic.Bool
	controller.Runner = func(ctx context.Context, prompt, systemPrompt string, filteredRegistry *coretools.ToolRegistry, extraContext coretools.ExecutionContext) (string, error) {
		called.Store(true)
		if systemPrompt != "system prompt" {
			t.Fatalf("unexpected system prompt: %q", systemPrompt)
		}
		if !strings.Contains(prompt, "Existing") || !strings.Contains(prompt, "save this") {
			t.Fatalf("unexpected prompt: %q", prompt)
		}
		roots, _ := extraContext["allowed_write_roots"].([]string)
		if len(roots) != 1 || roots[0] == "" {
			t.Fatalf("expected allowed memory root, got %#v", extraContext)
		}
		return "saved one memory", nil
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
	if !strings.Contains(result, "saved one memory") {
		t.Fatalf("unexpected extraction result: %q", result)
	}
	if state.LastExtractionUserTurn != 5 || state.MemoryWritesSinceExtraction {
		t.Fatalf("state was not updated after extraction: %#v", state)
	}
}

func TestExtractionControllerPreservesExplicitZeroConfigLikePython(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig()
	state := agent.NewAgentState()
	state.UserTurnCount = 0
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
		t.Fatalf("explicit zero turn threshold should allow extraction like Python")
	}
	done := make(chan struct{})
	controller.Runner = func(ctx context.Context, prompt, systemPrompt string, filteredRegistry *coretools.ToolRegistry, extraContext coretools.ExecutionContext) (string, error) {
		defer close(done)
		if !strings.Contains(prompt, "first") || !strings.Contains(prompt, "second") {
			t.Fatalf("context_message_count=0 should keep all messages like Python slicing, got %q", prompt)
		}
		if !strings.Contains(prompt, "You have 0 turns maximum") {
			t.Fatalf("max_extraction_turns=0 should be preserved in prompt, got %q", prompt)
		}
		return "zero config run", nil
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
	if result := controller.ConsumeResult(); result != "" {
		t.Fatalf("blank extraction summary should consume as empty string, got %q", result)
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
	if got := controller.ConsumeResult(); !strings.Contains(got, "detached extraction complete") {
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

func TestPostToolUseMarksMemoryWritesAndRefreshesIndex(t *testing.T) {
	dir := t.TempDir()
	memoryDir := filepath.Join(dir, "memory")
	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entryPath := filepath.Join(memoryDir, "project-note.md")
	if err := os.WriteFile(entryPath, []byte("---\nname: Project Note\ndescription: Useful detail\nmetadata:\n  type: project\n---\n\nBody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfig()
	cfg.CWD = dir
	cfg.AutoMemoryEnabled = true
	cfg.AutoMemoryDirectory = &memoryDir
	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()

	engine.PostToolUse(coretools.ToolCall{
		ID: "write-memory", Name: "write_file", Input: map[string]any{"file_path": entryPath},
	}, &state)
	if !state.MemoryWritesSinceExtraction {
		t.Fatal("expected memory write flag after writing inside auto-memory directory")
	}
	index, err := os.ReadFile(filepath.Join(memoryDir, "MEMORY.md"))
	if err != nil {
		t.Fatalf("expected MEMORY.md refresh: %v", err)
	}
	if !strings.Contains(string(index), "Project note") {
		t.Fatalf("expected refreshed index to mention memory entry, got %q", string(index))
	}

	state.MemoryWritesSinceExtraction = false
	regularPath := filepath.Join(dir, "regular.txt")
	engine.PostToolUse(coretools.ToolCall{
		ID: "write-regular", Name: "write_file", Input: map[string]any{"file_path": regularPath},
	}, &state)
	if state.MemoryWritesSinceExtraction {
		t.Fatal("regular project write should not mark memory writes since extraction")
	}
}
