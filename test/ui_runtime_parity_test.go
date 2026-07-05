package test

import (
	"context"
	"strings"
	"testing"
	"time"

	"LuminaCode/agent"
	luminacli "LuminaCode/cli"
	"LuminaCode/config"
	"LuminaCode/security"
	"LuminaCode/skills"
	coretools "LuminaCode/tools"
	"LuminaCode/ui"
)

type toolbarTestState struct {
	turns  int
	input  int
	output int
	yolo   bool
}

func (s toolbarTestState) TurnCountValue() int     { return s.turns }
func (s toolbarTestState) TokenTotals() (int, int) { return s.input, s.output }
func (s toolbarTestState) YoloEnabled() bool       { return s.yolo }

func TestUISharedHelpersMatchPython(t *testing.T) {
	if luminacli.DetectDisplayMode("utf_8") != luminacli.RichUnicode {
		t.Fatal("utf_8 should select rich_unicode")
	}
	if luminacli.DetectDisplayMode("latin1") != luminacli.ASCIISafe {
		t.Fatal("non-utf encoding should select ascii_safe")
	}
	symbols := luminacli.GetDisplaySymbols(luminacli.ASCIISafe)
	symbols["prompt.normal"] = "mutated"
	if luminacli.GetDisplaySymbols(luminacli.ASCIISafe)["prompt.normal"] != ">" {
		t.Fatal("GetDisplaySymbols should return a copy")
	}
	cases := map[string]string{
		"":       "deny",
		"yes":    "once",
		"always": "always",
		"never":  "deny",
		"yep":    "once",
		"again":  "always",
		"wat":    "deny",
	}
	for input, want := range cases {
		if got := luminacli.NormalizePermissionAnswer(input); got != want {
			t.Fatalf("NormalizePermissionAnswer(%q)=%q want %q", input, got, want)
		}
	}
	if luminacli.TranslateBackendRiskLevel("normal") != "medium" || luminacli.TranslateBackendRiskLevel("weird") != "medium" {
		t.Fatal("risk translation should mirror Python")
	}
	if cost := luminacli.CalculateSessionCost(1000, 2000, 0.003, 0.015); cost != 0.033 {
		t.Fatalf("unexpected cost: %v", cost)
	}
	if got := luminacli.FormatCWDForDisplay("/abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ", 20); !strings.Contains(got, "...") || len(got) > 20 {
		t.Fatalf("expected middle-truncated cwd, got %q", got)
	}
	if got := luminacli.FormatCWDForDisplay("/项目/路径/abcdefghijklmnopqrstuvwxyz", 12); got != "/项目/路径...xyz" {
		t.Fatalf("cwd truncation should use Python character slicing for Unicode paths, got %q", got)
	}
	if got := luminacli.FormatCWDForDisplay("abcdefghij", 4); got != "ab...bcdefghij" {
		t.Fatalf("small-width cwd truncation should mirror Python slice semantics, got %q", got)
	}
	toolbar := luminacli.BuildSessionToolbar(toolbarTestState{turns: 3, input: 1200, output: 900, yolo: true}, 0.003, 0.015, " | ")
	for _, want := range []string{"T3", "2K tok", "$0.0171", luminacli.YoloLabel} {
		if !strings.Contains(toolbar, want) {
			t.Fatalf("toolbar missing %q: %q", want, toolbar)
		}
	}
}

func TestUIRuntimeEventMappingAndFrameUpdates(t *testing.T) {
	rt := ui.NewUiRuntime(nil, nil)
	rt.ApplyUIEvent(rt.ToUIEvent(agent.NewStreamEvent("text", "hello", nil)))
	rt.ApplyUIEvent(rt.ToUIEvent(agent.NewStreamEvent("text", " world", nil)))
	rt.ApplyUIEvent(rt.ToUIEvent(agent.NewStreamEvent("thinking", "hmm", nil)))
	rt.ApplyUIEvent(rt.ToUIEvent(agent.NewStreamEvent("tool_call", "read_file", nil)))
	rt.ApplyUIEvent(rt.ToUIEvent(agent.NewStreamEvent("tool_result", "first line\nsecond line", map[string]any{"result": "first line\nsecond line"})))
	rt.ApplyUIEvent(rt.ToUIEvent(agent.NewStreamEvent("cost", "$0.0100", map[string]any{"cost": 0.01})))
	rt.ApplyUIEvent(rt.ToUIEvent(agent.NewStreamEvent("error", "boom", nil)))

	entries := rt.Frame.TranscriptEntries
	if len(entries) != 1 {
		t.Fatalf("expected only assistant text in transcript entries, got %#v", entries)
	}
	if entries[0]["kind"] != "assistant" || entries[0]["text"] != "hello world" {
		t.Fatalf("assistant deltas should merge, got %#v", entries[0])
	}
	if len(rt.Frame.TaskActivityEntries) != 2 ||
		rt.Frame.TaskActivityEntries[0]["summary"] != "[tool] read_file" ||
		rt.Frame.TaskActivityEntries[1]["summary"] != "[tool result] first line" {
		t.Fatalf("tool activity should stay out of transcript and land in task activity, got %#v", rt.Frame.TaskActivityEntries)
	}
	if rt.Frame.SessionCostText != "$0.0100" || rt.Frame.SessionCostValue != 0.01 {
		t.Fatalf("cost event not applied: %#v", rt.Frame)
	}
	if len(rt.Frame.Errors) != 1 || rt.Frame.Errors[0] != "boom" {
		t.Fatalf("fatal event should append errors, got %#v", rt.Frame.Errors)
	}
}

func TestUIContextWindowStatusShowsModelAndProgress(t *testing.T) {
	cfg := config.NewConfig()
	cfg.APIModel = "deepseek-v4-pro[1M]"
	cfg.APIMaxTokens = 1000
	state := agent.NewAgentState()
	state.SystemPrompt = strings.Repeat("s", 40)
	state.Messages = []map[string]any{{"role": "user", "content": strings.Repeat("x", 390)}}

	snapshot := ui.BuildContextWindowSnapshot(cfg, &state)
	if snapshot.ModelName != "deepseek-v4-pro[1M]" || snapshot.LimitTokens != 1000 || snapshot.UsedTokens <= 0 {
		t.Fatalf("unexpected context snapshot: %#v", snapshot)
	}
	status := ui.FormatContextWindowStatus(snapshot.ModelName, 250, snapshot.LimitTokens, 10)
	for _, want := range []string{"Model: deepseek-v4-pro[1M]", "Context", "[###-------]", "25%", "250/1K"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status missing %q: %q", want, status)
		}
	}
}

func TestUIRuntimeMountStateSnapshotBackfillsFullscreenHistory(t *testing.T) {
	state := agent.NewAgentState()
	state.Messages = []map[string]any{
		{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": "分析一下当前项目"}},
		},
		{
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": "这是一个 Go 工程。"},
				{"type": "tool_use", "id": "tool-1", "name": "list_files", "input": map[string]any{}},
			},
		},
		{
			"role": "user",
			"content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "tool-1", "content": "main.go\nui/runtime.go"},
			},
		},
		{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": "<system-reminder>hidden</system-reminder>"}},
			"isMeta":  true,
		},
	}
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader(""), nil, nil)
	rt := ui.NewUiRuntime(nil, fullscreen)

	rt.MountStateSnapshot(&state)

	transcript := fullscreen.TranscriptTextCache
	for _, want := range []string{"你", "  分析一下当前项目", "Lumina", "  这是一个 Go 工程。", "\n\n"} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("resumed transcript missing %q:\n%s", want, transcript)
		}
	}
	for _, hidden := range []string{"hidden", "[tool] list_files", "工具结果: main.go", "main.go\nui/runtime.go"} {
		if strings.Contains(transcript, hidden) {
			t.Fatalf("resume transcript should hide non-dialogue content %q:\n%s", hidden, transcript)
		}
	}
}

func TestUIRuntimePermissionModalAndTaskActivity(t *testing.T) {
	rt := ui.NewUiRuntime(nil, nil)
	command := strings.Repeat("echo x", 50)
	call := coretools.ToolCall{
		ID: "tool-1", Name: "run_shell",
		Input: map[string]any{"command": command},
	}
	event := ui.UiEvent{
		Type: "permission_requested",
		Metadata: map[string]any{
			"tool_call": call,
			"risk":      "normal",
			"dangerous": true,
		},
	}
	modal := rt.BuildPermissionModalState(event)
	if modal["kind"] != "tool_permission" || modal["display_risk_level"] != "medium" || modal["tool_name"] != "run_shell" {
		t.Fatalf("unexpected modal: %#v", modal)
	}
	lines := modal["summary_lines"].([]string)
	if len(lines) != 1 || len(lines[0]) != 200 {
		t.Fatalf("expected command summary truncated to 200 chars, got %#v", lines)
	}
	rt.Frame.ModalState = modal
	rt.RecordPermissionAudit(event, "deny")
	if len(rt.Frame.PermissionAudit) != 1 || rt.Frame.PermissionAudit[0]["decision"] != "deny" {
		t.Fatalf("permission audit not recorded: %#v", rt.Frame.PermissionAudit)
	}

	for i := 0; i < 52; i++ {
		taskID := "task"
		if i > 0 {
			taskID = taskID + string(rune('a'+i%26))
		}
		rt.TaskSink.Emit(ui.TaskUiEvent{
			Type:   "task_summary_available",
			TaskID: taskID,
			Record: map[string]any{
				"worker_label":   "worker",
				"status":         "completed",
				"input_tokens":   i,
				"output_tokens":  i + 1,
				"tool_use_count": i + 2,
				"duration_ms":    i + 3,
			},
			Summary:    "summary",
			ResultText: "result",
		})
	}
	rt.DrainTaskEvents()
	if len(rt.Frame.TaskActivityEntries) != 50 {
		t.Fatalf("task activity should retain latest 50 entries, got %d", len(rt.Frame.TaskActivityEntries))
	}
	last := rt.Frame.TaskActivityEntries[len(rt.Frame.TaskActivityEntries)-1]
	if last["status"] != "completed" || last["result_text"] != "result" {
		t.Fatalf("unexpected task activity entry: %#v", last)
	}
}

type permissionBackend struct {
	prompt    any
	dangerous bool
	updates   int
}

func (b *permissionBackend) Mount(ui.RenderFrame)                     {}
func (b *permissionBackend) Update(ui.RenderFrame)                    { b.updates++ }
func (b *permissionBackend) ShowModal(map[string]any)                 {}
func (b *permissionBackend) ClearModal()                              {}
func (b *permissionBackend) Shutdown(ui.RenderFrame)                  {}
func (b *permissionBackend) PickFromList(string, [][2]string) *string { return nil }
func (b *permissionBackend) AskPermission(prompt any, dangerous bool) string {
	b.prompt = prompt
	b.dangerous = dangerous
	return "once"
}

type prepareBackend struct {
	permissionBackend
	prepared int
	mounted  int
}

func (b *prepareBackend) PrepareRuntime()       { b.prepared++ }
func (b *prepareBackend) Mount(ui.RenderFrame)  { b.mounted++ }
func (b *prepareBackend) Update(ui.RenderFrame) { b.updates++ }

type mcpTrustResolveEngine struct {
	granted  *bool
	resolved chan bool
}

func (e *mcpTrustResolveEngine) ResolveMCPTrust(granted bool) {
	e.granted = &granted
	if e.resolved != nil {
		e.resolved <- granted
	}
}

func (e *mcpTrustResolveEngine) SubmitMessage(ctx context.Context, userInput string, state *agent.AgentState, sessionID ...string) <-chan agent.StreamEvent {
	out := make(chan agent.StreamEvent)
	go func() {
		defer close(out)
		out <- agent.NewStreamEvent("permission_needed", "mcp_project_trust", map[string]any{
			"mcp_trust_request": []map[string]any{{"name": "docs", "command": "node", "args": []string{"server.js"}}},
			"dangerous":         true,
			"risk":              "high",
		})
		select {
		case <-ctx.Done():
			return
		case <-e.resolved:
		}
		out <- agent.NewStreamEvent("done", "", nil)
	}()
	return out
}

func TestUIRuntimeSpecialPermissionRequestsMatchPythonObjectSemantics(t *testing.T) {
	backend := &permissionBackend{}
	rt := ui.NewUiRuntime(nil, backend)
	event := ui.UiEvent{
		Type: "permission_requested",
		Metadata: map[string]any{
			"skill_shell_request": skills.SkillShellPermissionRequest{
				SkillName: "build-ios",
				Command:   strings.Repeat("make test ", 30),
			},
		},
	}
	modal := rt.BuildPermissionModalState(event)
	if modal["kind"] != "skill_shell_permission" || modal["tool_name"] != "skill-shell" {
		t.Fatalf("expected skill shell modal, got %#v", modal)
	}
	if modal["target_summary"] != strings.Repeat("make test ", 30) {
		t.Fatalf("unexpected skill shell target summary: %#v", modal)
	}
	if got := rt.HandlePermissionEvent(event); got != "once" || !backend.dangerous {
		t.Fatalf("skill shell permission should ask dangerously and return answer, got %q dangerous=%v", got, backend.dangerous)
	}
	prompt, ok := backend.prompt.(map[string]any)
	if !ok || prompt["name"] != "skill-shell:build-ios" {
		t.Fatalf("skill shell prompt should match Python synthetic prompt, got %#v", backend.prompt)
	}
	input, _ := prompt["input"].(map[string]any)
	if input["command"] != strings.Repeat("make test ", 30) || input["file_path"] != "" {
		t.Fatalf("unexpected skill shell prompt input: %#v", input)
	}

	unicodeEvent := ui.UiEvent{
		Type: "permission_requested",
		Metadata: map[string]any{
			"tool_call": coretools.ToolCall{
				Name:  "run_shell",
				Input: map[string]any{"command": strings.Repeat("测", 201)},
			},
		},
	}
	unicodeModal := rt.BuildPermissionModalState(unicodeEvent)
	lines, _ := unicodeModal["summary_lines"].([]string)
	if len(lines) != 1 || lines[0] != strings.Repeat("测", 200) {
		t.Fatalf("permission summary should truncate by Python characters, got %#v", unicodeModal["summary_lines"])
	}

	mcpEvent := ui.UiEvent{
		Type: "permission_requested",
		Metadata: map[string]any{
			"mcp_trust_request": []any{
				map[string]any{"name": "local", "command": "node", "args": []string{"server.js", "--stdio"}},
				map[string]string{"name": "remote", "url": "https://example.invalid/mcp"},
			},
		},
	}
	mcpModal := rt.BuildPermissionModalState(mcpEvent)
	if mcpModal["kind"] != "mcp_trust_permission" || mcpModal["tool_name"] != "mcp-project-trust" {
		t.Fatalf("expected MCP trust modal, got %#v", mcpModal)
	}
	wantTarget := "local: node server.js --stdio; remote: https://example.invalid/mcp"
	if mcpModal["target_summary"] != wantTarget {
		t.Fatalf("unexpected MCP target summary: %#v", mcpModal)
	}
	rt.HandlePermissionEvent(mcpEvent)
	prompt, ok = backend.prompt.(map[string]any)
	if !ok || prompt["name"] != "mcp-project-trust" {
		t.Fatalf("MCP trust prompt should match Python synthetic prompt, got %#v", backend.prompt)
	}
	input, _ = prompt["input"].(map[string]any)
	if input["command"] != wantTarget || input["file_path"] != "" {
		t.Fatalf("unexpected MCP trust prompt input: %#v", input)
	}
}

func TestUIRuntimeSpecialPermissionPromptPriorityAndFlushMatchPython(t *testing.T) {
	backend := &permissionBackend{}
	rt := ui.NewUiRuntime(nil, backend)
	event := ui.UiEvent{
		Type: "permission_requested",
		Metadata: map[string]any{
			"tool_call": coretools.ToolCall{
				ID: "tool-1", Name: "run_shell",
				Input: map[string]any{"command": "rm -rf demo"},
			},
			"skill_shell_request": skills.SkillShellPermissionRequest{
				SkillName: "audit",
				Command:   "npm test",
			},
			"dangerous": true,
			"risk":      "high",
		},
	}

	answer := rt.HandlePermissionEvent(event)

	if answer != "once" {
		t.Fatalf("unexpected permission answer: %q", answer)
	}
	prompt, ok := backend.prompt.(map[string]any)
	if !ok || prompt["name"] != "skill-shell:audit" {
		t.Fatalf("skill shell permission should take prompt priority over legacy tool_call like Python, got %#v", backend.prompt)
	}
	if backend.updates != 1 {
		t.Fatalf("permission handler should force-flush after clearing modal like Python, got %d updates", backend.updates)
	}
	if rt.Frame.ActiveModal != "none" || rt.Frame.InputEnabled {
		t.Fatalf("permission handler should restore responding frame state, got %#v", rt.Frame)
	}
}

func TestUIRuntimeResolvesMCPTrustDecisionLikePython(t *testing.T) {
	backend := &permissionBackend{}
	engine := &mcpTrustResolveEngine{resolved: make(chan bool, 1)}
	rt := ui.NewUiRuntime(engine, backend)

	events := rt.RunSubmitMessage(context.Background(), "hello", nil, "session-1")

	if engine.granted == nil || !*engine.granted {
		t.Fatalf("MCP trust answer should call ResolveMCPTrust(true), got %#v", engine.granted)
	}
	if len(events) != 2 || events[0].Type != "permission_needed" || events[1].Type != "done" {
		t.Fatalf("unexpected MCP trust submit events: %#v", events)
	}
}

type submitRuntimeFakeEngine struct {
	rt       *agent.AgentTaskRuntime
	resolved chan string
}

func (e *submitRuntimeFakeEngine) TaskRuntimeForUI() *agent.AgentTaskRuntime {
	return e.rt
}

func (e *submitRuntimeFakeEngine) SubmitMessage(ctx context.Context, userInput string, state *agent.AgentState, sessionID ...string) <-chan agent.StreamEvent {
	out := make(chan agent.StreamEvent)
	go func() {
		defer close(out)
		record := e.rt.RegisterForegroundTask("task-1", "", "main", "worker", "desc", "general-purpose")
		e.rt.StopTask(record.TaskID, "main")
		out <- agent.NewStreamEvent("text", "hello", nil)
		out <- agent.NewStreamEvent("permission_needed", "", map[string]any{
			"tool_call": coretools.ToolCall{ID: "tool-1", Name: "run_shell", Input: map[string]any{"command": "echo ok"}},
			"dangerous": true,
		})
		select {
		case <-ctx.Done():
			return
		case <-e.resolved:
		}
		out <- agent.NewStreamEvent("done", "", nil)
	}()
	return out
}

func (e *submitRuntimeFakeEngine) ResolvePermission(decision string, toolName string) {
	e.resolved <- decision + ":" + toolName
}

func TestUIRuntimeRunSubmitMessageDrivesTaskSinkAndPermissionLikePython(t *testing.T) {
	backend := &permissionBackend{}
	engine := &submitRuntimeFakeEngine{rt: agent.NewAgentTaskRuntime(), resolved: make(chan string, 1)}
	rt := ui.NewUiRuntime(engine, backend)
	state := agent.NewAgentState()

	events := rt.RunSubmitMessage(context.Background(), "please analyze", &state, "session-1")

	if len(events) != 3 || events[0].Type != "text" || events[1].Type != "permission_needed" || events[2].Type != "done" {
		t.Fatalf("unexpected submit events: %#v", events)
	}
	if len(rt.Frame.TranscriptEntries) != 2 ||
		rt.Frame.TranscriptEntries[0]["kind"] != "user" ||
		rt.Frame.TranscriptEntries[0]["text"] != "please analyze" ||
		rt.Frame.TranscriptEntries[1]["kind"] != "assistant" ||
		rt.Frame.TranscriptEntries[1]["text"] != "hello" {
		t.Fatalf("submit should keep user and assistant as separate transcript cells, got %#v", rt.Frame.TranscriptEntries)
	}
	if backend.prompt == nil || !backend.dangerous {
		t.Fatalf("permission event should ask backend like Python, prompt=%#v dangerous=%v", backend.prompt, backend.dangerous)
	}
	select {
	case got := <-engine.resolved:
		t.Fatalf("permission decision should have been consumed by fake engine, still queued: %q", got)
	default:
	}
	if len(rt.Frame.Tasks) != 1 || rt.Frame.Tasks["task-1"]["status"] != "killed" {
		t.Fatalf("task sink events should update frame tasks, got %#v", rt.Frame.Tasks)
	}
	if len(rt.Frame.TaskActivityEntries) != 1 || rt.Frame.TaskActivityEntries[0]["summary"] != "Running worker was stopped." {
		t.Fatalf("terminal task event should create activity entry, got %#v", rt.Frame.TaskActivityEntries)
	}
	if !rt.Frame.InputEnabled || rt.Frame.InputPlaceholder != "请输入消息并回车。" || backend.updates == 0 {
		t.Fatalf("submit runtime should update frame and backend, frame=%#v updates=%d", rt.Frame, backend.updates)
	}

	record := engine.rt.RegisterForegroundTask("task-2", "", "main", "worker", "desc", "general-purpose")
	engine.rt.StopTask(record.TaskID, "main")
	rt.DrainTaskEvents()
	if _, ok := rt.Frame.Tasks["task-2"]; ok {
		t.Fatalf("task sink should be detached after submit, got tasks %#v", rt.Frame.Tasks)
	}
}

func TestUIRuntimeDrainTaskEventsFlushesWhenDueLikePython(t *testing.T) {
	backend := &permissionBackend{}
	rt := ui.NewUiRuntime(nil, backend)
	rt.TaskSink.Emit(ui.TaskUiEvent{
		Type:       "task_snapshot_updated",
		TaskID:     "task-1",
		Record:     map[string]any{"task_id": "task-1", "status": "running"},
		Summary:    "started",
		ResultText: "",
	})

	rt.DrainTaskEvents()

	if backend.updates != 1 {
		t.Fatalf("draining task events should flush immediately when no previous frame was flushed, got %d updates", backend.updates)
	}
	if rt.Dirty {
		t.Fatal("drain-triggered flush should clear dirty flag")
	}
}

type prepareSubmitEngine struct{}

func (e *prepareSubmitEngine) SubmitMessage(ctx context.Context, userInput string, state *agent.AgentState, sessionID ...string) <-chan agent.StreamEvent {
	out := make(chan agent.StreamEvent)
	go func() {
		defer close(out)
		out <- agent.NewStreamEvent("done", "", nil)
	}()
	return out
}

func TestUIRuntimeRunSubmitMessagePreparesBackendBeforeMountLikePython(t *testing.T) {
	backend := &prepareBackend{}
	rt := ui.NewUiRuntime(&prepareSubmitEngine{}, backend)

	rt.RunSubmitMessage(context.Background(), "hello", nil, "session-1")

	if backend.prepared != 1 || backend.mounted != 1 {
		t.Fatalf("run submit should prepare backend once before mounting, prepared=%d mounted=%d", backend.prepared, backend.mounted)
	}
}

type delayedSubmitEngine struct{}

func (e *delayedSubmitEngine) SubmitMessage(ctx context.Context, userInput string, state *agent.AgentState, sessionID ...string) <-chan agent.StreamEvent {
	out := make(chan agent.StreamEvent)
	go func() {
		defer close(out)
		select {
		case <-time.After(350 * time.Millisecond):
			out <- agent.NewStreamEvent("done", "", nil)
		case <-ctx.Done():
		}
	}()
	return out
}

func TestUIRuntimeRunSubmitMessageTicksAnimationWhileWaiting(t *testing.T) {
	fullscreen := ui.NewFullscreenRendererBackend(strings.NewReader(""), nil, nil)
	rt := ui.NewUiRuntime(&delayedSubmitEngine{}, fullscreen)

	rt.RunSubmitMessage(context.Background(), "wait for work", nil, "session-1")

	if fullscreen.LastFrame.AnimationFrame == 0 {
		t.Fatalf("runtime should advance animation frames while waiting, frame=%#v", fullscreen.LastFrame)
	}
	if fullscreen.LastFrame.InputEnabled != true || fullscreen.InputPlaceholder != "请输入消息并回车。" {
		t.Fatalf("runtime should restore input-ready frame after submit, frame=%#v placeholder=%q", fullscreen.LastFrame, fullscreen.InputPlaceholder)
	}
	if strings.Contains(fullscreen.TasksText, "正在执行任务") {
		t.Fatalf("fullscreen task overview should clear animated working state after submit, got %q", fullscreen.TasksText)
	}
}

func TestTerminalUIBackendMixinMatchesPythonUpdateSemantics(t *testing.T) {
	var rendered []string
	backend := ui.NewTerminalUIBackendMixin(func(record map[string]any) {
		rendered = append(rendered, record["task_id"].(string))
	})
	initial := ui.NewRenderFrame()
	initial.Tasks["z"] = map[string]any{"task_id": "z", "status": "queued"}
	backend.Mount(initial)
	if len(backend.LastRenderedTaskSignatures) != 0 {
		t.Fatalf("mount should reset rendered signatures, got %#v", backend.LastRenderedTaskSignatures)
	}
	modal := map[string]any{"kind": "permission_request"}
	backend.ShowModal(modal)
	if backend.ActiveModalState["kind"] != "permission_request" {
		t.Fatalf("modal state not stored: %#v", backend.ActiveModalState)
	}
	backend.ClearModal()
	if backend.ActiveModalState != nil {
		t.Fatalf("modal state should clear, got %#v", backend.ActiveModalState)
	}

	frame := ui.NewRenderFrame()
	frame.Tasks["b"] = map[string]any{"task_id": "b", "status": "running", "input_tokens": 1, "output_tokens": 2, "tool_use_count": 3, "duration_ms": 4}
	frame.Tasks["a"] = map[string]any{"task_id": "a", "status": "running", "input_tokens": 1, "output_tokens": 2, "tool_use_count": 3, "duration_ms": 4}
	backend.Update(frame)
	if strings.Join(rendered, ",") != "a,b" {
		t.Fatalf("tasks should render in sorted task_id order, got %#v", rendered)
	}
	if backend.LastFrame.Tasks["a"]["status"] != "running" {
		t.Fatalf("last frame not updated: %#v", backend.LastFrame)
	}

	backend.Update(frame)
	if strings.Join(rendered, ",") != "a,b" {
		t.Fatalf("unchanged task signatures should not rerender, got %#v", rendered)
	}
	frame.Tasks["a"]["output_tokens"] = 9
	backend.Update(frame)
	if strings.Join(rendered, ",") != "a,b,a" {
		t.Fatalf("changed signature should rerender only changed task, got %#v", rendered)
	}

	finalFrame := ui.NewRenderFrame()
	finalFrame.InputEnabled = false
	backend.Shutdown(finalFrame)
	if backend.LastFrame.InputEnabled {
		t.Fatalf("shutdown should store final snapshot, got %#v", backend.LastFrame)
	}
}

func TestAgentStateImplementsToolbarState(t *testing.T) {
	state := agent.NewAgentState()
	state.TurnCount = 2
	state.TotalInputTokens = 600
	state.TotalOutputTokens = 500
	state.PermissionState = security.DefaultPermissionState()
	state.PermissionState.YoloMode = true
	toolbar := luminacli.BuildSessionToolbar(&state, 0.003, 0.015, " | ")
	for _, want := range []string{"T2", "1K tok", "$0.0093", luminacli.YoloLabel} {
		if !strings.Contains(toolbar, want) {
			t.Fatalf("agent state toolbar missing %q: %q", want, toolbar)
		}
	}
}
