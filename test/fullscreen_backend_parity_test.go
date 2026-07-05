package test

import (
	"bytes"
	"strings"
	"testing"

	"LuminaCode/ui"
)

func TestFullscreenRendererBackendFactoryAndFrameStateMatchPython(t *testing.T) {
	backend := ui.NewRendererBackend("prompt_toolkit_fullscreen", strings.NewReader(""), nil, nil)
	fullscreen, ok := backend.(*ui.FullscreenRendererBackend)
	if !ok {
		t.Fatalf("prompt_toolkit_fullscreen should select fullscreen backend, got %T", backend)
	}
	legacy := ui.NewRendererBackend("legacy_terminal", strings.NewReader(""), nil, nil)
	if _, ok := legacy.(*ui.FullscreenRendererBackend); !ok {
		t.Fatalf("legacy terminal backend name should still resolve to fullscreen, got %T", legacy)
	}

	frame := ui.NewRenderFrame()
	frame.TranscriptEntries = []map[string]any{
		{"kind": "user", "text": "分析一下项目"},
		{"kind": "assistant", "text": "hello"},
		{"kind": "assistant", "text": " world"},
		{"kind": "thinking", "text": "plan"},
		{"kind": "thinking", "text": " more"},
		{"kind": "tool_result", "text": "[tool result] ok"},
	}
	frame.Tasks["task-1"] = map[string]any{
		"task_id": "task-1", "worker_label": "worker-1", "status": "running",
		"input_tokens": 3, "output_tokens": 2, "tool_use_count": 1, "duration_ms": 120,
	}
	frame.TaskActivityEntries = []map[string]any{{
		"task_id": "task-1", "worker_label": "worker-1", "status": "completed",
		"summary": "Worker completed request.", "result_text": "full result text",
	}}
	frame.InputEnabled = false
	frame.InputPlaceholder = "Agent is responding..."
	frame.Warnings = []string{"careful"}
	frame.ModelName = "gpt-test"
	frame.ContextUsedTokens = 250
	frame.ContextLimitTokens = 1000

	fullscreen.Mount(frame)

	transcript := strings.Join(fullscreen.TranscriptChunks, "|")
	if !strings.Contains(transcript, "你") ||
		!strings.Contains(transcript, "  分析一下项目") ||
		!strings.Contains(transcript, "Lumina") ||
		!strings.Contains(transcript, "  hello world") ||
		!strings.Contains(transcript, "\n\n|") {
		t.Fatalf("unexpected transcript chunks: %#v", fullscreen.TranscriptChunks)
	}
	if !strings.Contains(fullscreen.TranscriptControlText, "对话记录") ||
		!strings.Contains(fullscreen.TranscriptControlText, "hello world") ||
		!strings.Contains(fullscreen.TranscriptControlText, "分析一下项目") ||
		strings.Contains(fullscreen.TranscriptControlText, "plan more") ||
		strings.Contains(fullscreen.TranscriptControlText, "工具结果") {
		t.Fatalf("unexpected transcript control:\n%s", fullscreen.TranscriptControlText)
	}
	if !strings.Contains(fullscreen.TasksText, "Task Snapshots:") ||
		!strings.Contains(fullscreen.TasksText, "- worker-1 [running] tools=1 usage=3/2 duration=120ms") ||
		!strings.Contains(fullscreen.TasksText, "Recent Task Activity:") {
		t.Fatalf("unexpected tasks text:\n%s", fullscreen.TasksText)
	}
	if !fullscreen.InputReadOnly || !strings.Contains(fullscreen.InputMetaText, "输入已锁定：Agent is responding...") {
		t.Fatalf("input state should mirror Python frame sync, readOnly=%v meta=%q", fullscreen.InputReadOnly, fullscreen.InputMetaText)
	}
	if !strings.Contains(fullscreen.StatusText, "Model: gpt-test") ||
		!strings.Contains(fullscreen.StatusText, "[#####---------------]") ||
		!strings.Contains(fullscreen.StatusText, "25%") ||
		!strings.Contains(fullscreen.StatusText, "careful") {
		t.Fatalf("status should show model, context progress, and warning: %q", fullscreen.StatusText)
	}

	fullscreen.ShowModal(map[string]any{
		"kind": "tool_permission", "tool_name": "write_file", "target_summary": "/tmp/demo.txt",
		"display_risk_level": "high", "summary_lines": []string{"demo content"},
		"action_labels": []string{"允许一次", "本会话总是允许", "拒绝"},
	})
	if fullscreen.FocusedPane != "modal" || !strings.Contains(fullscreen.ModalText, "write_file") ||
		!strings.Contains(fullscreen.StatusText, "权限请求待处理") {
		t.Fatalf("modal state mismatch: focus=%s modal=%q status=%q", fullscreen.FocusedPane, fullscreen.ModalText, fullscreen.StatusText)
	}
	fullscreen.ClearModal()
	if fullscreen.ModalText != "" || fullscreen.FocusedPane != "transcript" || !strings.Contains(fullscreen.StatusText, "careful") {
		t.Fatalf("clear modal should restore non-modal focus/status, focus=%s modal=%q status=%q", fullscreen.FocusedPane, fullscreen.ModalText, fullscreen.StatusText)
	}
}

func TestFullscreenRendererBackendStartsScreenBeforeFirstInput(t *testing.T) {
	var out bytes.Buffer
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader("\x03"), &out, &out)

	_, ok := fullscreen.GetInput(nil)

	if ok {
		t.Fatal("Ctrl-C should cancel first input")
	}
	if !strings.Contains(out.String(), "\x1b[?1049h") || !strings.Contains(out.String(), "输入就绪") {
		t.Fatalf("fullscreen should render before waiting for first input, got %q", out.String())
	}
	if !strings.Contains(out.String(), "\x1b[?1000h") || !strings.Contains(out.String(), "\x1b[?1006h") {
		t.Fatalf("fullscreen should enable mouse wheel reporting, got %q", out.String())
	}
	if !strings.Contains(out.String(), "\r\n") {
		t.Fatalf("fullscreen should render CRLF line endings in raw terminal mode, got %q", out.String())
	}
}

func TestFullscreenRendererBackendAdaptsViewportHeightToTerminalLines(t *testing.T) {
	t.Setenv("LINES", "24")
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader(""), nil, nil)
	frame := ui.NewRenderFrame()
	frame.TranscriptEntries = []map[string]any{{"kind": "assistant", "text": strings.Join([]string{
		"one", "two", "three", "four", "five", "six", "seven", "eight",
	}, "\n")}}
	frame.ModelName = "gpt-test"
	frame.ContextUsedTokens = 250
	frame.ContextLimitTokens = 1000

	fullscreen.Mount(frame)

	if got := strings.Count(fullscreen.TranscriptControlText, "\n") + 1; got > 8 {
		t.Fatalf("small terminal should shrink transcript panel, got %d lines:\n%s", got, fullscreen.TranscriptControlText)
	}
}

func TestFullscreenRendererBackendShowsTaskAnimationWhileResponding(t *testing.T) {
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader(""), nil, nil)
	frame := ui.NewRenderFrame()
	frame.InputEnabled = false
	frame.InputPlaceholder = "Agent is responding..."
	frame.AnimationFrame = 2

	fullscreen.Mount(frame)

	if !strings.Contains(fullscreen.TasksText, "正在执行任务..") {
		t.Fatalf("task overview should show animated working line, got %q", fullscreen.TasksText)
	}
	if !strings.Contains(fullscreen.StatusText, "Working..") {
		t.Fatalf("status should show animated working indicator, got %q", fullscreen.StatusText)
	}
}

func TestFullscreenRendererBackendFocusScrollSearchAndTogglesMatchPython(t *testing.T) {
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader(""), nil, nil)

	if got := strings.Join(fullscreen.VisiblePanes(), ","); got != "transcript,tasks,input" {
		t.Fatalf("unexpected panes without modal: %s", got)
	}
	fullscreen.CycleFocus(1)
	if fullscreen.FocusedPane != "transcript" {
		t.Fatalf("focus should cycle to transcript, got %s", fullscreen.FocusedPane)
	}
	fullscreen.CycleFocus(1)
	if fullscreen.FocusedPane != "tasks" {
		t.Fatalf("focus should cycle to tasks, got %s", fullscreen.FocusedPane)
	}
	fullscreen.ShowModal(map[string]any{"tool_name": "write_file", "target_summary": "/tmp/x", "display_risk_level": "high"})
	if got := strings.Join(fullscreen.VisiblePanes(), ","); got != "transcript,tasks,modal,input" {
		t.Fatalf("unexpected panes with modal: %s", got)
	}
	fullscreen.CycleFocus(1)
	if fullscreen.FocusedPane != "input" {
		t.Fatalf("modal focus should cycle to input, got %s", fullscreen.FocusedPane)
	}
	fullscreen.CycleFocus(-1)
	if fullscreen.FocusedPane != "modal" {
		t.Fatalf("reverse focus should return to modal, got %s", fullscreen.FocusedPane)
	}

	if got := ui.SliceViewportText("l1\nl2\nl3\nl4\nl5", 1, 3); got != "l2\nl3\nl4" {
		t.Fatalf("unexpected viewport slice: %q", got)
	}
	fullscreen.ScrollPane("transcript", "a\nb\nc", 10, 2)
	if fullscreen.PaneScrollOffsets["transcript"] != 1 {
		t.Fatalf("scroll should clamp to max offset, got %d", fullscreen.PaneScrollOffsets["transcript"])
	}
	fullscreen.ScrollPane("transcript", "a\nb\nc", -10, 2)
	if fullscreen.PaneScrollOffsets["transcript"] != 0 {
		t.Fatalf("scroll should clamp to zero, got %d", fullscreen.PaneScrollOffsets["transcript"])
	}

	fullscreen.TranscriptTextCache = "alpha\nbeta match\ngamma match\ndelta"
	fullscreen.SetTranscriptSearchQuery("match")
	if fullscreen.TranscriptSearchIndex != 0 || len(fullscreen.TranscriptSearchMatches) != 2 {
		t.Fatalf("unexpected search state: matches=%#v index=%d", fullscreen.TranscriptSearchMatches, fullscreen.TranscriptSearchIndex)
	}
	fullscreen.StepTranscriptSearch(1)
	fullscreen.StepTranscriptSearch(1)
	if fullscreen.TranscriptSearchIndex != 0 {
		t.Fatalf("search navigation should wrap, got %d", fullscreen.TranscriptSearchIndex)
	}
	fullscreen.SetTranscriptSearchQuery("")
	if fullscreen.TranscriptSearchIndex != -1 || len(fullscreen.TranscriptSearchMatches) != 0 {
		t.Fatalf("empty query should clear search, matches=%#v index=%d", fullscreen.TranscriptSearchMatches, fullscreen.TranscriptSearchIndex)
	}

	fullscreen.InputMode = "search_query"
	fullscreen.InputEnabled = true
	fullscreen.CancelSearchMode()
	if fullscreen.InputMode != "normal" || fullscreen.InputPlaceholder != "请输入消息并回车。" {
		t.Fatalf("cancel search mismatch: mode=%s placeholder=%s", fullscreen.InputMode, fullscreen.InputPlaceholder)
	}
	fullscreen.PendingPermission = true
	fullscreen.EnterSearchMode()
	if fullscreen.InputMode == "search_query" {
		t.Fatal("enter search should be ignored during permission flow")
	}
}

func TestFullscreenRendererBackendHandleKeysMatchPromptToolkitBindings(t *testing.T) {
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader(""), nil, nil)
	fullscreen.TranscriptTextCache = strings.Join([]string{
		"line 1", "line 2", "line 3", "line 4", "line 5", "line 6",
		"line 7", "line 8", "line 9", "line 10", "line 11", "line 12",
		"line 13", "line 14", "line 15", "line 16", "line 17", "line 18",
		"line 19", "line 20", "match here", "line 22",
	}, "\n")
	fullscreen.TasksText = "a\nb\nc\nd\ne\nf\ng\nh\ni"

	fullscreen.HandleKey("f6")
	if fullscreen.FocusedPane != "transcript" {
		t.Fatalf("F6 should focus transcript, got %s", fullscreen.FocusedPane)
	}
	fullscreen.HandleKey("pagedown")
	if fullscreen.PaneScrollOffsets["transcript"] != 10 {
		t.Fatalf("PageDown should scroll transcript by viewport and clamp, got %d", fullscreen.PaneScrollOffsets["transcript"])
	}
	fullscreen.HandleKey("pageup")
	if fullscreen.PaneScrollOffsets["transcript"] != 0 {
		t.Fatalf("PageUp should scroll transcript back to top, got %d", fullscreen.PaneScrollOffsets["transcript"])
	}
	fullscreen.HandleKey("c-f")
	if fullscreen.InputMode != "search_query" || fullscreen.InputPlaceholder != "输入搜索词并回车。" {
		t.Fatalf("Ctrl-F should enter search mode, mode=%s placeholder=%q", fullscreen.InputMode, fullscreen.InputPlaceholder)
	}
	fullscreen.SetTranscriptSearchQuery("match")
	fullscreen.HandleKey("f3")
	if fullscreen.TranscriptSearchIndex != 0 || fullscreen.PaneScrollOffsets["transcript"] != 14 {
		t.Fatalf("F3 should jump to search result, index=%d offset=%d", fullscreen.TranscriptSearchIndex, fullscreen.PaneScrollOffsets["transcript"])
	}
	fullscreen.HandleKey("s-f3")
	if fullscreen.TranscriptSearchIndex != 0 {
		t.Fatalf("Shift-F3 should wrap single search result, got %d", fullscreen.TranscriptSearchIndex)
	}

	frame := ui.NewRenderFrame()
	frame.TranscriptEntries = []map[string]any{{"kind": "thinking", "text": "hidden thought"}}
	frame.TaskActivityEntries = []map[string]any{{
		"task_id": "task-1", "summary": "done", "result_text": "expanded detail",
	}}
	fullscreen.Mount(frame)
	fullscreen.HandleKey("f4")
	fullscreen.HandleKey("f5")
	if !fullscreen.ThinkingExpanded || strings.Contains(fullscreen.TranscriptTextCache, "hidden thought") {
		t.Fatalf("F4 should not expose thinking in dialogue transcript, expanded=%v cache=%s", fullscreen.ThinkingExpanded, fullscreen.TranscriptTextCache)
	}
	if !fullscreen.TaskActivityExpanded || !strings.Contains(fullscreen.TasksControlText, "expanded detail") {
		t.Fatalf("F5 should toggle task details, text=%s", fullscreen.TasksControlText)
	}
	fullscreen.HandleKey("s-f6")
	if fullscreen.FocusedPane != "input" {
		t.Fatalf("Shift-F6 should reverse focus to input, got %s", fullscreen.FocusedPane)
	}
}

func TestFullscreenRendererBackendScrollsTranscriptHistoryWhileInputFocused(t *testing.T) {
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader(""), nil, nil)
	fullscreen.FocusedPane = "input"
	fullscreen.TranscriptTextCache = strings.Join([]string{
		"line 1", "line 2", "line 3", "line 4", "line 5", "line 6",
		"line 7", "line 8", "line 9", "line 10", "line 11", "line 12",
		"line 13", "line 14", "line 15", "line 16", "line 17", "line 18",
	}, "\n")
	fullscreen.HandleKey("end")

	if !strings.Contains(fullscreen.TranscriptControlText, "对话记录") ||
		!strings.Contains(fullscreen.TranscriptControlText, "/18") {
		t.Fatalf("scrollable transcript title should show visible range, got:\n%s", fullscreen.TranscriptControlText)
	}
	fullscreen.HandleKey("pageup")
	if fullscreen.PaneScrollOffsets["transcript"] != 0 {
		t.Fatalf("PageUp with input focus should scroll transcript history, got offset %d", fullscreen.PaneScrollOffsets["transcript"])
	}
	fullscreen.HandleKey("end")
	if fullscreen.PaneScrollOffsets["transcript"] == 0 {
		t.Fatalf("End with input focus should return transcript to the latest lines")
	}
	fullscreen.HandleKey("home")
	if fullscreen.PaneScrollOffsets["transcript"] != 0 {
		t.Fatalf("Home with input focus should jump transcript to top, got offset %d", fullscreen.PaneScrollOffsets["transcript"])
	}
}

func TestFullscreenRendererBackendMouseWheelScrollsTranscriptHistory(t *testing.T) {
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader("\x1b[<64;20;10M\x03"), nil, nil)
	fullscreen.FocusedPane = "input"
	fullscreen.TranscriptTextCache = strings.Join([]string{
		"line 1", "line 2", "line 3", "line 4", "line 5", "line 6",
		"line 7", "line 8", "line 9", "line 10", "line 11", "line 12",
		"line 13", "line 14", "line 15", "line 16", "line 17", "line 18",
	}, "\n")
	fullscreen.HandleKey("end")
	bottom := fullscreen.PaneScrollOffsets["transcript"]

	_, ok := fullscreen.GetInput(nil)

	if ok {
		t.Fatal("Ctrl-C should stop after mouse wheel event")
	}
	if fullscreen.PaneScrollOffsets["transcript"] >= bottom {
		t.Fatalf("mouse wheel up should scroll transcript history upward, before=%d after=%d", bottom, fullscreen.PaneScrollOffsets["transcript"])
	}
}

func TestFullscreenRendererBackendScrollsTaskOverview(t *testing.T) {
	t.Setenv("LINES", "30")
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader(""), nil, nil)
	fullscreen.TasksText = strings.Join([]string{
		"task 1", "task 2", "task 3", "task 4", "task 5", "task 6",
		"task 7", "task 8", "task 9", "task 10", "task 11", "task 12",
	}, "\n")
	fullscreen.FocusedPane = "tasks"

	fullscreen.HandleKey("pagedown")

	if fullscreen.PaneScrollOffsets["tasks"] == 0 {
		t.Fatalf("PageDown should scroll focused task overview, offsets=%#v", fullscreen.PaneScrollOffsets)
	}
	fullscreen.HandleKey("home")
	if fullscreen.PaneScrollOffsets["tasks"] != 0 {
		t.Fatalf("Home should return task overview to top, offsets=%#v", fullscreen.PaneScrollOffsets)
	}
}

func TestFullscreenRendererBackendMouseWheelScrollsTaskOverviewByRegion(t *testing.T) {
	t.Setenv("LINES", "30")
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader("\x1b[<65;20;20M\x03"), nil, nil)
	fullscreen.FocusedPane = "input"
	fullscreen.TranscriptTextCache = strings.Join([]string{
		"line 1", "line 2", "line 3", "line 4", "line 5", "line 6",
		"line 7", "line 8", "line 9", "line 10", "line 11", "line 12",
	}, "\n")
	fullscreen.TasksText = strings.Join([]string{
		"task 1", "task 2", "task 3", "task 4", "task 5", "task 6",
		"task 7", "task 8", "task 9", "task 10", "task 11", "task 12",
	}, "\n")

	_, ok := fullscreen.GetInput(nil)

	if ok {
		t.Fatal("Ctrl-C should stop after mouse wheel event")
	}
	if fullscreen.PaneScrollOffsets["tasks"] == 0 {
		t.Fatalf("mouse wheel in task panel should scroll task overview, offsets=%#v", fullscreen.PaneScrollOffsets)
	}
	if fullscreen.PaneScrollOffsets["transcript"] != 0 {
		t.Fatalf("mouse wheel in task panel should not scroll transcript, offsets=%#v", fullscreen.PaneScrollOffsets)
	}
}

func TestFullscreenRendererBackendGetInputHandlesControlKeysLikePython(t *testing.T) {
	input := "\x06match\n\x1bOR\x1bOS\x1b[15~\x1b[17~hello\n"
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader(input), nil, nil)
	frame := ui.NewRenderFrame()
	frame.TranscriptEntries = []map[string]any{
		{"kind": "assistant", "text": "alpha\nmatch one\nmatch two\n"},
		{"kind": "thinking", "text": "plan"},
	}
	frame.TaskActivityEntries = []map[string]any{{"task_id": "task-1", "summary": "done", "result_text": "detail"}}
	fullscreen.Mount(frame)

	text, ok := fullscreen.GetInput(nil)
	if !ok || text != "hello" {
		t.Fatalf("expected final submitted input after control keys, ok=%v text=%q", ok, text)
	}
	if fullscreen.TranscriptSearchQuery != "match" || len(fullscreen.TranscriptSearchMatches) != 2 {
		t.Fatalf("Ctrl-F search should update search state, query=%q matches=%#v", fullscreen.TranscriptSearchQuery, fullscreen.TranscriptSearchMatches)
	}
	if !fullscreen.ThinkingExpanded || !fullscreen.TaskActivityExpanded {
		t.Fatalf("F4/F5 from input stream should toggle detail state")
	}
	if fullscreen.FocusedPane != "transcript" {
		t.Fatalf("F6 from input stream should cycle focus, got %s", fullscreen.FocusedPane)
	}
}

func TestFullscreenRendererBackendSlashCompletionMenuMatchesInteractiveInput(t *testing.T) {
	var out bytes.Buffer
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader("/\x1b[B\t\n"), &out, &out)

	text, ok := fullscreen.GetInput(nil)

	if !ok || text != "/clear" {
		t.Fatalf("expected Down+Tab to select the second slash command, ok=%v text=%q", ok, text)
	}
	rendered := out.String()
	if !strings.Contains(rendered, "/help") || !strings.Contains(rendered, "/clear") {
		t.Fatalf("slash completion menu should render command candidates, got %q", rendered)
	}
}

func TestFullscreenRendererBackendSlashCompletionMenuScrollsWithSelection(t *testing.T) {
	t.Setenv("LINES", "24")
	var out bytes.Buffer
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader("/\x1b[B\x1b[B\x1b[B\x1b[B\x1b[B\x1b[B\x03"), &out, &out)

	_, ok := fullscreen.GetInput(nil)

	if ok {
		t.Fatal("Ctrl-C should stop input after exercising completion menu")
	}
	frames := strings.Split(out.String(), "\x1b[H\x1b[2J")
	foundScrolledMenu := false
	for _, frame := range frames {
		if strings.Contains(frame, "above") && strings.Contains(frame, "more") && strings.Contains(frame, "\x1b[7m  /") {
			foundScrolledMenu = true
			break
		}
	}
	if !foundScrolledMenu {
		t.Fatalf("selected completion should remain visible while menu scrolls, output:\n%s", out.String())
	}
}

func TestFullscreenRendererBackendEditsInputAtCursor(t *testing.T) {
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader("abc\x1b[D\x1b[DX\x1b[CZ\x1b[D\x1b[3~\n"), nil, nil)

	text, ok := fullscreen.GetInput(nil)

	if !ok || text != "aXbc" {
		t.Fatalf("expected cursor-aware input editing, ok=%v text=%q", ok, text)
	}
}

func TestFullscreenRendererBackendMovesInputCursorVertically(t *testing.T) {
	t.Setenv("COLUMNS", "64")
	longInput := strings.Repeat("a", 70)
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader(longInput+"\x1b[AX\n"), nil, nil)

	text, ok := fullscreen.GetInput(nil)

	want := strings.Repeat("a", 14) + "X" + strings.Repeat("a", 56)
	if !ok || text != want {
		t.Fatalf("expected Up to move cursor to previous visual row, ok=%v text length=%d want length=%d text=%q", ok, len(text), len(want), text)
	}
}

func TestFullscreenRendererBackendReturnsUserInputWithoutMutatingTranscript(t *testing.T) {
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader("你好 lumina\n"), nil, nil)

	text, ok := fullscreen.GetInput(nil)

	if !ok || text != "你好 lumina" {
		t.Fatalf("expected submitted user input, ok=%v text=%q", ok, text)
	}
	if strings.Contains(fullscreen.TranscriptTextCache, "你好 lumina") {
		t.Fatalf("input reader should not mutate dialogue transcript before runtime submit, cache=%q", fullscreen.TranscriptTextCache)
	}
}

func TestFullscreenRendererBackendWrapsLongTranscriptLines(t *testing.T) {
	t.Setenv("COLUMNS", "72")
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader(""), nil, nil)
	longError := "[error] DeepSeek API error 400 400 Bad Request: " + strings.Repeat("supported API model message ", 8)

	fullscreen.AppendTranscript(longError)

	if strings.Contains(fullscreen.TranscriptControlText, "…") {
		t.Fatalf("transcript should wrap long lines instead of truncating them:\n%s", fullscreen.TranscriptControlText)
	}
	if got := strings.Count(fullscreen.TranscriptControlText, "supported API model message"); got < 2 {
		t.Fatalf("wrapped transcript should show more long-line content, got %d occurrences:\n%s", got, fullscreen.TranscriptControlText)
	}
}

func TestFullscreenRendererBackendThinkingAndTaskDetailTogglesMatchPython(t *testing.T) {
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader(""), nil, nil)
	frame := ui.NewRenderFrame()
	frame.TranscriptEntries = []map[string]any{
		{"kind": "assistant", "text": "hello"},
		{"kind": "thinking", "text": "plan one"},
		{"kind": "thinking", "text": " plan two"},
	}
	frame.TaskActivityEntries = []map[string]any{{
		"task_id": "task-1", "worker_label": "worker-1", "status": "completed",
		"summary": "Worker completed request.", "result_text": "full result text",
	}}

	fullscreen.Mount(frame)
	if strings.Contains(fullscreen.TranscriptControlText, "[thinking collapsed]") ||
		strings.Contains(fullscreen.TranscriptControlText, "plan one") ||
		strings.Contains(fullscreen.TasksControlText, "full result text") {
		t.Fatalf("collapsed/default detail mismatch:\ntranscript=%s\ntasks=%s", fullscreen.TranscriptControlText, fullscreen.TasksControlText)
	}

	fullscreen.ToggleThinkingVisibility()
	fullscreen.ToggleTaskActivityDetail()
	if !fullscreen.ThinkingExpanded || strings.Contains(fullscreen.TranscriptControlText, "plan one plan two") {
		t.Fatalf("thinking toggle mismatch: expanded=%v text=%s", fullscreen.ThinkingExpanded, fullscreen.TranscriptControlText)
	}
	if !fullscreen.TaskActivityExpanded || !strings.Contains(fullscreen.TasksControlText, "full result text") {
		t.Fatalf("task detail toggle mismatch: expanded=%v text=%s", fullscreen.TaskActivityExpanded, fullscreen.TasksControlText)
	}
}

func TestFullscreenRendererBackendRendersAlternateScreenFrames(t *testing.T) {
	var out bytes.Buffer
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader(""), &out, &out)
	fullscreen.PrepareRuntime()
	frame := ui.NewRenderFrame()
	frame.TranscriptEntries = []map[string]any{{"kind": "assistant", "text": "hello fullscreen"}}
	frame.Tasks["task-1"] = map[string]any{
		"task_id": "task-1", "worker_label": "worker", "status": "running",
	}
	frame.InputEnabled = false
	frame.InputPlaceholder = "Agent is responding..."
	fullscreen.Mount(frame)

	rendered := out.String()
	if !strings.Contains(rendered, "\x1b[?1049h") || !strings.Contains(rendered, "\x1b[?25l") {
		t.Fatalf("fullscreen prepare should enter alternate screen and hide cursor, got %q", rendered)
	}
	if !strings.Contains(rendered, "hello fullscreen") ||
		!strings.Contains(rendered, "任务概览") ||
		!strings.Contains(rendered, "输入已锁定：Agent is responding...") {
		t.Fatalf("fullscreen frame should render transcript/tasks/input state, got %q", rendered)
	}

	writer := fullscreen.OutputWriter()
	if _, err := writer.Write([]byte("slash output\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "slash output") {
		t.Fatalf("fullscreen output writer should append into transcript and refresh screen, got %q", out.String())
	}

	fullscreen.Shutdown(frame)
	if !strings.Contains(out.String(), "\x1b[?25h\x1b[?1049l") {
		t.Fatalf("fullscreen shutdown should restore cursor and leave alternate screen, got %q", out.String())
	}
	if !strings.Contains(out.String(), "\x1b[?1006l") || !strings.Contains(out.String(), "\x1b[?1000l") {
		t.Fatalf("fullscreen shutdown should disable mouse reporting, got %q", out.String())
	}
}

func TestFullscreenRendererBackendAskPermissionStateMatchesPython(t *testing.T) {
	var out bytes.Buffer
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader("a\n"), &out, &out)
	fullscreen.PrepareRuntime()
	answer := fullscreen.AskPermission(map[string]any{
		"name":  "run_shell",
		"input": map[string]any{"command": "go test ./..."},
	}, true)
	if answer != "always" {
		t.Fatalf("expected normalized always answer, got %q", answer)
	}
	if fullscreen.PendingPermission || fullscreen.ActiveModalState != nil || fullscreen.ModalText != "" {
		t.Fatalf("permission state should be cleared after answer, pending=%v modal=%#v text=%q", fullscreen.PendingPermission, fullscreen.ActiveModalState, fullscreen.ModalText)
	}
	if fullscreen.InputMode != "normal" || fullscreen.InputPlaceholder != "Agent is responding..." || fullscreen.InputEnabled {
		t.Fatalf("permission input state should reset to responding, mode=%s placeholder=%q enabled=%v", fullscreen.InputMode, fullscreen.InputPlaceholder, fullscreen.InputEnabled)
	}
	rendered := out.String()
	if !strings.Contains(rendered, "权限请求") || !strings.Contains(rendered, "go test ./...") {
		t.Fatalf("permission modal should render into fullscreen output, got %q", rendered)
	}
}

func TestFullscreenRendererBackendPickFromListSelectionMatchesPython(t *testing.T) {
	var out bytes.Buffer
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader("2\n"), &out, &out)
	fullscreen.PrepareRuntime()

	selected := fullscreen.PickFromList("Resume session", [][2]string{
		{"sess-1", "Session one"},
		{"sess-2", "Session two"},
	})

	if selected == nil || *selected != "sess-2" {
		t.Fatalf("fullscreen picklist should select the typed number, got %#v", selected)
	}
	if fullscreen.PendingSelection || fullscreen.InputMode != "normal" || fullscreen.SelectionValues != nil {
		t.Fatalf("selection state should be cleared after picklist, pending=%v mode=%s values=%#v", fullscreen.PendingSelection, fullscreen.InputMode, fullscreen.SelectionValues)
	}
	rendered := out.String()
	if !strings.Contains(rendered, "Resume session") ||
		!strings.Contains(rendered, "2. Session two") ||
		!strings.Contains(rendered, "上下选择，回车确认") {
		t.Fatalf("fullscreen picklist should render selection prompt, got:\n%s", rendered)
	}
	if strings.Contains(fullscreen.TranscriptTextCache, "Resume session") || strings.Contains(fullscreen.TranscriptTextCache, "Session two") {
		t.Fatalf("fullscreen picklist should render in input panel, not transcript: %q", fullscreen.TranscriptTextCache)
	}

	out.Reset()
	fullscreen = ui.NewFullscreenRendererBackend(strings.NewReader("\n"), &out, &out)
	if got := fullscreen.PickFromList("Empty", nil); got != nil || out.Len() != 0 {
		t.Fatalf("empty fullscreen picklist should return before rendering, got=%#v out=%q", got, out.String())
	}
}

func TestFullscreenRendererBackendPickFromListArrowSelection(t *testing.T) {
	var out bytes.Buffer
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader("\x1b[B\n"), &out, &out)
	fullscreen.PrepareRuntime()

	selected := fullscreen.PickFromList("Select Skill", [][2]string{
		{"writer", "/writer                  Write docs"},
		{"reader", "/reader                  Read docs"},
	})

	if selected == nil || *selected != "reader" {
		t.Fatalf("Down+Enter should select second item, got %#v", selected)
	}
	rendered := out.String()
	if !strings.Contains(rendered, "Select Skill") || !strings.Contains(rendered, "\x1b[7m  2. /reader") {
		t.Fatalf("fullscreen picklist should highlight selected skill, got:\n%s", rendered)
	}
}

func TestFullscreenRendererBackendInputDraftLetsUserFinishSkillPrompt(t *testing.T) {
	var out bytes.Buffer
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader("inspect src\n"), &out, &out)
	fullscreen.PrepareRuntime()
	fullscreen.SetInputDraft("/reader ")

	text, ok := fullscreen.GetInput(nil)

	if !ok || text != "/reader inspect src" {
		t.Fatalf("drafted skill invocation should wait for user input, ok=%v text=%q", ok, text)
	}
	if !strings.Contains(out.String(), "/reader") {
		t.Fatalf("drafted skill invocation should render in input panel, got:\n%s", out.String())
	}
}

func TestFullscreenRendererBackendPickFromListEscCancels(t *testing.T) {
	var out bytes.Buffer
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader("\x1b"), &out, &out)
	fullscreen.PrepareRuntime()

	selected := fullscreen.PickFromList("Resume session", [][2]string{
		{"sess-1", "Session one"},
		{"sess-2", "Session two"},
	})

	if selected != nil {
		t.Fatalf("Esc should cancel fullscreen picklist, got %#v", selected)
	}
	if fullscreen.PendingSelection || fullscreen.InputMode != "normal" || fullscreen.SelectionValues != nil {
		t.Fatalf("selection state should be cleared after Esc, pending=%v mode=%s values=%#v", fullscreen.PendingSelection, fullscreen.InputMode, fullscreen.SelectionValues)
	}
}
