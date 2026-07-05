package ui

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"LuminaCode/agent"
	luminacli "LuminaCode/cli"
	"LuminaCode/config"
	"LuminaCode/skills"
	coretools "LuminaCode/tools"

	"golang.org/x/term"
)

const (
	FullscreenTranscriptViewportLines = 18
	FullscreenTasksViewportLines      = 8
	FullscreenModalViewportLines      = 8
)

type FullscreenRendererBackend struct {
	terminal  *TerminalRendererBackend
	out       io.Writer
	inputFile *os.File

	LastFrame        RenderFrame
	ActiveModalState map[string]any

	TranscriptChunks         []string
	TranscriptTextCache      string
	renderedTranscriptBlocks [][2]string
	TaskChunks               []string
	renderedTaskBlocks       [][3]string
	TasksText                string
	ModalText                string
	StatusText               string
	InputMetaText            string
	TranscriptControlText    string
	TasksControlText         string
	ModalControlText         string
	InputReadOnly            bool
	InputEnabled             bool
	InputMode                string
	InputPlaceholder         string
	FocusedPane              string
	PaneScrollOffsets        map[string]int
	TranscriptSearchQuery    string
	TranscriptSearchMatches  []int
	TranscriptSearchIndex    int
	ThinkingExpanded         bool
	TaskActivityExpanded     bool
	PendingInput             bool
	PendingPermission        bool
	PendingSelection         bool
	SelectionValues          [][2]string
	renderEnabled            bool
	screenActive             bool
}

func NewFullscreenRendererBackend(in io.Reader, out io.Writer, errOut io.Writer) *FullscreenRendererBackend {
	renderEnabled := out != nil
	inputFile, _ := in.(*os.File)
	b := &FullscreenRendererBackend{
		terminal:              NewTerminalRendererBackend(in, out, errOut),
		out:                   out,
		inputFile:             inputFile,
		LastFrame:             NewRenderFrame(),
		InputEnabled:          true,
		InputMode:             "normal",
		FocusedPane:           "input",
		PaneScrollOffsets:     map[string]int{"transcript": 0, "tasks": 0, "modal": 0},
		TranscriptSearchIndex: -1,
		renderEnabled:         renderEnabled,
	}
	b.refreshControls()
	return b
}

func (b *FullscreenRendererBackend) UsesFrameTranscript() bool { return true }
func (b *FullscreenRendererBackend) PrepareRuntime() {
	if !b.renderEnabled || b.screenActive || b.out == nil {
		return
	}
	b.screenActive = true
	fmt.Fprint(b.out, "\x1b[?1049h\x1b[?25l")
	b.renderScreen()
}

func (b *FullscreenRendererBackend) RenderWelcome(sessionID string, skillRegistry *skills.SkillRegistry) {
	b.AppendTranscript("LuminaCode - experimental prompt_toolkit full-screen UI\nUse Ctrl+C to interrupt, Ctrl+D to exit.\n\n")
}

func (b *FullscreenRendererBackend) GetInput(state any) (string, bool) {
	b.InputEnabled = true
	b.InputMode = "normal"
	b.InputPlaceholder = "请输入消息并回车。"
	b.PendingInput = true
	b.StatusText = b.formatIdleStatus(state)
	b.refreshControls()
	defer func() {
		b.PendingInput = false
		b.refreshControls()
	}()
	return b.readFullscreenText()
}

func (b *FullscreenRendererBackend) ResetForNewSession() {
	b.TranscriptChunks = nil
	b.TranscriptTextCache = ""
	b.renderedTranscriptBlocks = nil
	b.TaskChunks = nil
	b.renderedTaskBlocks = nil
	b.TasksText = ""
	b.ModalText = ""
	b.StatusText = ""
	b.InputEnabled = true
	b.InputPlaceholder = ""
	b.FocusedPane = "input"
	b.PaneScrollOffsets = map[string]int{"transcript": 0, "tasks": 0, "modal": 0}
	b.TranscriptSearchQuery = ""
	b.TranscriptSearchMatches = nil
	b.TranscriptSearchIndex = -1
	b.ThinkingExpanded = false
	b.TaskActivityExpanded = false
	b.LastFrame = NewRenderFrame()
	b.terminal.ResetForNewSession()
	b.refreshControls()
}

func (b *FullscreenRendererBackend) OutputWriter() io.Writer {
	return fullscreenTranscriptWriter{backend: b}
}

func (b *FullscreenRendererBackend) SetRegistry(registry *coretools.ToolRegistry) {
	b.terminal.SetRegistry(registry)
}

func (b *FullscreenRendererBackend) SetExecContext(execCtx coretools.ExecutionContext) {
	b.terminal.SetExecContext(execCtx)
}

func (b *FullscreenRendererBackend) Mount(initialFrame RenderFrame) {
	b.LastFrame = initialFrame
	b.refreshFromFrame(initialFrame)
}

func (b *FullscreenRendererBackend) Update(frame RenderFrame) {
	b.LastFrame = frame
	b.refreshFromFrame(frame)
}

func (b *FullscreenRendererBackend) ShowModal(modalState map[string]any) {
	b.ActiveModalState = cloneMap(modalState)
	b.ModalText = FormatFullscreenModal(modalState)
	b.FocusedPane = "modal"
	b.StatusText = b.formatStatus(b.LastFrame)
	b.refreshControls()
}

func (b *FullscreenRendererBackend) ClearModal() {
	b.ActiveModalState = nil
	b.ModalText = ""
	if b.InputEnabled {
		b.FocusedPane = "input"
	} else {
		b.FocusedPane = "transcript"
	}
	b.StatusText = b.formatStatus(b.LastFrame)
	b.refreshControls()
}

func (b *FullscreenRendererBackend) Shutdown(finalSnapshot RenderFrame) {
	b.LastFrame = finalSnapshot
	b.refreshFromFrame(finalSnapshot)
	if b.renderEnabled && b.screenActive && b.out != nil {
		fmt.Fprint(b.out, "\x1b[?25h\x1b[?1049l")
		b.screenActive = false
	}
}

func (b *FullscreenRendererBackend) AskPermission(prompt any, dangerous bool) string {
	if b.ActiveModalState == nil {
		b.ShowModal(b.permissionModalState(prompt, dangerous))
	}
	b.InputEnabled = true
	b.InputMode = "permission_modal"
	b.InputPlaceholder = "请输入 y / a / n。"
	b.PendingPermission = true
	b.refreshControls()
	defer func() {
		b.PendingPermission = false
		b.InputEnabled = false
		b.InputMode = "normal"
		b.InputPlaceholder = "Agent is responding..."
		b.ClearModal()
	}()
	text, ok := b.readFullscreenText()
	if !ok {
		return "deny"
	}
	return luminacli.NormalizePermissionAnswer(text)
}

func (b *FullscreenRendererBackend) PickFromList(title string, values [][2]string) *string {
	if len(values) == 0 {
		return nil
	}
	b.AppendTranscript(strings.Join(selectionLines(title, values), "\n"))
	b.InputEnabled = true
	b.InputMode = "selection"
	b.InputPlaceholder = "请输入编号并回车。"
	b.SelectionValues = values
	b.PendingSelection = true
	b.refreshControls()
	defer func() {
		b.PendingSelection = false
		b.SelectionValues = nil
		b.InputMode = "normal"
		b.refreshControls()
	}()
	text, ok := b.readFullscreenText()
	if !ok {
		return nil
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	for idx, value := range values {
		if text == fmt.Sprint(idx+1) {
			selected := value[0]
			return &selected
		}
	}
	return nil
}

func (b *FullscreenRendererBackend) RenderEvent(event agent.StreamEvent) {
	switch event.Type {
	case "text":
		b.AppendTranscript(event.Content)
	case "thinking":
		b.AppendTranscript("[thinking] " + event.Content)
	case "tool_call":
		b.AppendTranscript("\n[tool] " + event.Content + "\n")
	case "tool_result":
		result := event.Content
		if metadataResult := stringFromAny(event.Metadata["result"]); metadataResult != "" {
			result = metadataResult
		}
		if truthy(event.Metadata["denied"]) {
			b.AppendTranscript("[tool denied]\n")
		} else if result != "" {
			b.AppendTranscript("[tool result] " + truncateString(result, 200) + "\n")
		}
	case "error":
		b.AppendTranscript("\n[error] " + event.Content + "\n")
	case "cost":
		b.StatusText = event.Content
		b.refreshControls()
	case "done":
		b.AppendTranscript("\n")
	}
}

func (b *FullscreenRendererBackend) VisiblePanes() []string {
	panes := []string{"transcript", "tasks"}
	if b.ModalText != "" {
		panes = append(panes, "modal")
	}
	return append(panes, "input")
}

func (b *FullscreenRendererBackend) CycleFocus(direction int) {
	panes := b.VisiblePanes()
	if len(panes) == 0 {
		return
	}
	index := -1
	for i, pane := range panes {
		if pane == b.FocusedPane {
			index = i
			break
		}
	}
	if index < 0 {
		b.FocusedPane = panes[len(panes)-1]
		return
	}
	next := (index + direction) % len(panes)
	if next < 0 {
		next += len(panes)
	}
	b.FocusedPane = panes[next]
}

func SliceViewportText(text string, offset, height int) string {
	lines := strings.Split(text, "\n")
	if text == "" {
		return ""
	}
	start := maxInt(0, offset)
	end := maxInt(start, start+maxInt(1, height))
	if start > len(lines) {
		start = len(lines)
	}
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start:end], "\n")
}

func (b *FullscreenRendererBackend) ScrollPane(pane, text string, delta, height int) {
	lines := strings.Split(text, "\n")
	if text == "" {
		lines = nil
	}
	maxOffset := maxInt(0, len(lines)-maxInt(1, height))
	current := b.PaneScrollOffsets[pane]
	b.PaneScrollOffsets[pane] = maxInt(0, minInt(maxOffset, current+delta))
}

func (b *FullscreenRendererBackend) ScrollActivePane(delta int) {
	switch b.FocusedPane {
	case "transcript":
		b.ScrollPane("transcript", b.TranscriptTextCache, delta, FullscreenTranscriptViewportLines)
	case "tasks":
		b.ScrollPane("tasks", b.TasksText, delta, FullscreenTasksViewportLines)
	case "modal":
		b.ScrollPane("modal", b.ModalText, delta, FullscreenModalViewportLines)
	}
}

func (b *FullscreenRendererBackend) SetTranscriptSearchQuery(query string) {
	normalized := strings.TrimSpace(query)
	b.TranscriptSearchQuery = normalized
	if normalized == "" {
		b.TranscriptSearchMatches = nil
		b.TranscriptSearchIndex = -1
		return
	}
	var matches []int
	for idx, line := range strings.Split(b.TranscriptTextCache, "\n") {
		if strings.Contains(strings.ToLower(line), strings.ToLower(normalized)) {
			matches = append(matches, idx)
		}
	}
	b.TranscriptSearchMatches = matches
	if len(matches) == 0 {
		b.TranscriptSearchIndex = -1
	} else {
		b.TranscriptSearchIndex = 0
	}
}

func (b *FullscreenRendererBackend) StepTranscriptSearch(direction int) {
	if len(b.TranscriptSearchMatches) == 0 {
		b.TranscriptSearchIndex = -1
		return
	}
	current := b.TranscriptSearchIndex
	if current < 0 {
		current = 0
	}
	count := len(b.TranscriptSearchMatches)
	b.TranscriptSearchIndex = (current + direction) % count
	if b.TranscriptSearchIndex < 0 {
		b.TranscriptSearchIndex += count
	}
	matchLine := b.TranscriptSearchMatches[b.TranscriptSearchIndex]
	b.PaneScrollOffsets["transcript"] = maxInt(0, matchLine-FullscreenTranscriptViewportLines/2)
}

func (b *FullscreenRendererBackend) PageScrollDelta() int {
	switch b.FocusedPane {
	case "transcript":
		return FullscreenTranscriptViewportLines
	case "tasks":
		return FullscreenTasksViewportLines
	case "modal":
		return FullscreenModalViewportLines
	default:
		return FullscreenTranscriptViewportLines
	}
}

func (b *FullscreenRendererBackend) HandleKey(key string) bool {
	switch key {
	case "f6":
		b.CycleFocus(1)
	case "s-f6":
		b.CycleFocus(-1)
	case "pageup":
		b.ScrollActivePane(-b.PageScrollDelta())
	case "pagedown":
		b.ScrollActivePane(b.PageScrollDelta())
	case "c-f":
		b.EnterSearchMode()
	case "f3":
		b.StepTranscriptSearch(1)
	case "s-f3":
		b.StepTranscriptSearch(-1)
	case "f4":
		b.ToggleThinkingVisibility()
	case "f5":
		b.ToggleTaskActivityDetail()
	case "escape":
		if b.ActiveModalState != nil {
			b.FocusedPane = "input"
		} else if b.InputMode == "search_query" {
			b.CancelSearchMode()
		} else {
			return false
		}
	default:
		return false
	}
	b.refreshControls()
	return true
}

func (b *FullscreenRendererBackend) CancelSearchMode() {
	b.InputMode = "normal"
	if b.InputEnabled {
		b.InputPlaceholder = "请输入消息并回车。"
	} else {
		b.InputPlaceholder = "正在等待代理继续执行。"
	}
}

func (b *FullscreenRendererBackend) EnterSearchMode() {
	if b.PendingPermission || b.PendingSelection || !b.InputEnabled {
		return
	}
	b.InputMode = "search_query"
	b.InputEnabled = true
	b.InputPlaceholder = "输入搜索词并回车。"
}

func (b *FullscreenRendererBackend) ToggleThinkingVisibility() {
	b.ThinkingExpanded = !b.ThinkingExpanded
	b.renderedTranscriptBlocks = nil
	b.refreshFromFrame(b.LastFrame)
}

func (b *FullscreenRendererBackend) ToggleTaskActivityDetail() {
	b.TaskActivityExpanded = !b.TaskActivityExpanded
	b.renderedTaskBlocks = nil
	b.refreshFromFrame(b.LastFrame)
}

func (b *FullscreenRendererBackend) AppendTranscript(text string) {
	safe := strings.ToValidUTF8(text, "\uFFFD")
	b.TranscriptChunks = append(b.TranscriptChunks, safe)
	b.TranscriptTextCache = strings.Join(b.TranscriptChunks, "")
	b.renderedTranscriptBlocks = nil
	b.refreshControls()
}

func (b *FullscreenRendererBackend) refreshFromFrame(frame RenderFrame) {
	b.syncTranscriptFromFrame(frame)
	b.syncTasksFromFrame(frame)
	b.StatusText = b.formatStatus(frame)
	b.syncInputFromFrame(frame)
	b.refreshControls()
}

func (b *FullscreenRendererBackend) refreshControls() {
	b.TranscriptControlText = b.renderTranscriptRegion()
	b.TasksControlText = b.renderTasksRegion()
	b.ModalControlText = b.renderModalRegion()
	b.InputMetaText = b.formatInputMeta()
	b.InputReadOnly = !b.isInputEditable()
	b.renderScreen()
}

func (b *FullscreenRendererBackend) syncTranscriptFromFrame(frame RenderFrame) {
	if len(frame.TranscriptEntries) == 0 {
		return
	}
	blocks := BuildFullscreenTranscriptBlocks(frame.TranscriptEntries)
	newBlocks := make([][2]string, 0, len(blocks))
	for _, block := range blocks {
		newBlocks = append(newBlocks, fullscreenBlockSignature(block))
	}
	previous := b.renderedTranscriptBlocks
	if len(previous) > 0 && len(previous) != len(b.TranscriptChunks) {
		previous = nil
	}
	prefixLen := sharedTranscriptPrefix(previous, newBlocks)
	if len(previous) == 0 {
		b.TranscriptChunks = make([]string, 0, len(blocks))
		for _, block := range blocks {
			b.TranscriptChunks = append(b.TranscriptChunks, b.formatTranscriptBlock(block))
		}
	} else {
		chunks := append([]string{}, b.TranscriptChunks[:prefixLen]...)
		for _, block := range blocks[prefixLen:] {
			chunks = append(chunks, b.formatTranscriptBlock(block))
		}
		b.TranscriptChunks = chunks
	}
	b.renderedTranscriptBlocks = newBlocks
	b.TranscriptTextCache = strings.Join(b.TranscriptChunks, "")
}

func BuildFullscreenTranscriptBlocks(entries []map[string]any) []map[string]any {
	var blocks []map[string]any
	for _, entry := range entries {
		kind := stringFromAny(entry["kind"])
		text := stringFromAny(entry["text"])
		if text == "" {
			continue
		}
		if len(blocks) > 0 && (kind == "assistant" || kind == "thinking") && stringFromAny(blocks[len(blocks)-1]["kind"]) == kind {
			blocks[len(blocks)-1]["text"] = stringFromAny(blocks[len(blocks)-1]["text"]) + text
		} else {
			blocks = append(blocks, map[string]any{"kind": kind, "text": text})
		}
	}
	return blocks
}

func (b *FullscreenRendererBackend) formatTranscriptBlock(block map[string]any) string {
	kind := stringFromAny(block["kind"])
	text := stringFromAny(block["text"])
	switch kind {
	case "assistant":
		return text
	case "thinking":
		if !b.ThinkingExpanded {
			return "[thinking collapsed]"
		}
		return "[thinking] " + text
	case "tool_call":
		return text + "\n"
	case "tool_result":
		normalized := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "[tool result] "), "[tool result]"))
		if normalized == "" {
			return "工具结果:\n"
		}
		return "工具结果: " + normalized + "\n"
	case "warning":
		return "[warning] " + text + "\n"
	case "error":
		return "[error] " + text + "\n"
	default:
		return text
	}
}

func (b *FullscreenRendererBackend) syncTasksFromFrame(frame RenderFrame) {
	blocks := b.buildTaskBlocks(frame)
	newBlocks := make([][3]string, 0, len(blocks))
	for _, block := range blocks {
		newBlocks = append(newBlocks, fullscreenTaskBlockSignature(block))
	}
	previous := b.renderedTaskBlocks
	if len(previous) > 0 && len(previous) != len(b.TaskChunks) {
		previous = nil
	}
	prefixLen := sharedTaskPrefix(previous, newBlocks)
	if len(previous) == 0 {
		b.TaskChunks = make([]string, 0, len(blocks))
		for _, block := range blocks {
			b.TaskChunks = append(b.TaskChunks, stringFromAny(block["text"]))
		}
	} else {
		chunks := append([]string{}, b.TaskChunks[:prefixLen]...)
		for _, block := range blocks[prefixLen:] {
			chunks = append(chunks, stringFromAny(block["text"]))
		}
		b.TaskChunks = chunks
	}
	b.renderedTaskBlocks = newBlocks
	b.TasksText = strings.Join(b.TaskChunks, "\n")
}

func (b *FullscreenRendererBackend) buildTaskBlocks(frame RenderFrame) []map[string]any {
	hasSnapshots := len(frame.Tasks) > 0
	hasActivity := len(frame.TaskActivityEntries) > 0
	if !hasSnapshots && !hasActivity {
		return []map[string]any{{"kind": "empty", "key": "empty", "text": "Tasks: none"}}
	}
	var blocks []map[string]any
	if hasSnapshots {
		blocks = append(blocks, map[string]any{"kind": "snapshot_header", "key": "snapshot_header", "text": "Task Snapshots:"})
		for _, taskID := range sortedStringMapKeys(frame.Tasks) {
			record := frame.Tasks[taskID]
			label := firstNonEmpty(stringFromAny(record["worker_label"]), taskID)
			status := firstNonEmpty(stringFromAny(record["status"]), "unknown")
			blocks = append(blocks, map[string]any{
				"kind": "snapshot_line",
				"key":  taskID,
				"text": fmt.Sprintf("- %s [%s] tools=%d usage=%d/%d duration=%dms", label, status, intFromAny(record["tool_use_count"]), intFromAny(record["input_tokens"]), intFromAny(record["output_tokens"]), intFromAny(record["duration_ms"])),
			})
		}
	}
	if hasActivity {
		if hasSnapshots {
			blocks = append(blocks, map[string]any{"kind": "separator", "key": "snapshot_activity", "text": ""})
		}
		blocks = append(blocks, map[string]any{"kind": "activity_header", "key": "activity_header", "text": "Recent Task Activity:"})
		start := maxInt(0, len(frame.TaskActivityEntries)-5)
		for idx, entry := range frame.TaskActivityEntries[start:] {
			label := firstNonEmpty(stringFromAny(entry["worker_label"]), stringFromAny(entry["task_id"]))
			status := firstNonEmpty(stringFromAny(entry["status"]), "unknown")
			taskID := stringFromAny(entry["task_id"])
			blocks = append(blocks, map[string]any{
				"kind": "activity_line",
				"key":  fmt.Sprintf("%s:%d", taskID, idx),
				"text": fmt.Sprintf("- %s [%s] %s", label, status, stringFromAny(entry["summary"])),
			})
			if b.TaskActivityExpanded && stringFromAny(entry["result_text"]) != "" {
				blocks = append(blocks, map[string]any{
					"kind": "activity_detail",
					"key":  fmt.Sprintf("%s:%d:detail", taskID, idx),
					"text": "  result: " + truncateString(stringFromAny(entry["result_text"]), 200),
				})
			}
		}
	}
	return blocks
}

func (b *FullscreenRendererBackend) syncInputFromFrame(frame RenderFrame) {
	if b.PendingInput || b.PendingPermission {
		return
	}
	b.InputEnabled = frame.InputEnabled
	b.InputMode = firstNonEmpty(frame.InputMode, "normal")
	b.InputPlaceholder = frame.InputPlaceholder
}

func (b *FullscreenRendererBackend) isInputEditable() bool {
	return b.PendingPermission || b.PendingInput || b.InputEnabled
}

func (b *FullscreenRendererBackend) formatInputMeta() string {
	if b.PendingSelection {
		return "选择输入：" + firstNonEmpty(b.InputPlaceholder, "请输入编号并回车。")
	}
	if b.PendingPermission {
		return "权限输入：" + firstNonEmpty(b.InputPlaceholder, "请输入 y / a / n。")
	}
	if b.PendingInput {
		return "输入就绪：" + firstNonEmpty(b.InputPlaceholder, "请输入消息并回车。")
	}
	if !b.InputEnabled {
		return "输入已锁定：" + firstNonEmpty(b.InputPlaceholder, "正在等待代理继续执行。")
	}
	return "输入就绪：" + firstNonEmpty(b.InputPlaceholder, "请输入消息并回车。")
}

func (b *FullscreenRendererBackend) formatStatus(frame RenderFrame) string {
	if b.ActiveModalState != nil {
		return "权限请求待处理"
	}
	var parts []string
	if status := FormatContextWindowStatus(frame.ModelName, frame.ContextUsedTokens, frame.ContextLimitTokens, 20); status != "" {
		parts = append(parts, status)
	}
	if len(frame.Errors) > 0 {
		parts = append(parts, frame.Errors[len(frame.Errors)-1])
	}
	if len(frame.Warnings) > 0 {
		parts = append(parts, frame.Warnings[len(frame.Warnings)-1])
	}
	if len(parts) > 0 {
		return strings.Join(parts, luminacli.ToolbarSeparator)
	}
	return b.StatusText
}

func (b *FullscreenRendererBackend) formatIdleStatus(state any) string {
	var parts []string
	if cfg, ok := b.terminal.execCtx["config"].(config.Config); ok {
		var agentState *agent.AgentState
		if typed, ok := state.(*agent.AgentState); ok {
			agentState = typed
		} else if typed, ok := state.(agent.AgentState); ok {
			agentState = &typed
		}
		snapshot := BuildContextWindowSnapshot(cfg, agentState)
		if status := FormatContextWindowStatus(snapshot.ModelName, snapshot.UsedTokens, snapshot.LimitTokens, 20); status != "" {
			parts = append(parts, status)
		}
	}
	if toolbarState, ok := state.(luminacli.ToolbarState); ok {
		if toolbar := luminacli.BuildSessionToolbar(toolbarState, 0.003, 0.015, luminacli.ToolbarSeparator); toolbar != "" {
			parts = append(parts, toolbar)
		}
	}
	return strings.Join(parts, luminacli.ToolbarSeparator)
}

func FormatFullscreenModal(modalState map[string]any) string {
	if len(modalState) == 0 {
		return ""
	}
	riskLabel := displayRiskLabel(firstNonEmpty(stringFromAny(modalState["display_risk_level"]), "medium"))
	var summaryLines []string
	for _, line := range anyStringSlice(modalState["summary_lines"]) {
		if line != "" {
			summaryLines = append(summaryLines, "- "+line)
		}
	}
	actionLabels := anyStringSlice(modalState["action_labels"])
	actionText := strings.Join(actionLabels, " / ")
	if actionText == "" {
		actionText = "允许一次 / 本会话总是允许 / 拒绝"
	}
	parts := []string{
		"权限请求",
		"类型: " + stringFromAny(modalState["tool_name"]),
		"目标: " + stringFromAny(modalState["target_summary"]),
		"风险: " + riskLabel,
	}
	if len(summaryLines) > 0 {
		parts = append(parts, "摘要:")
		parts = append(parts, summaryLines...)
	}
	parts = append(parts, "操作: "+actionText, "请输入 y / a / n 并回车。")
	return strings.Join(parts, "\n")
}

func (b *FullscreenRendererBackend) renderTranscriptRegion() string {
	body := SliceViewportText(b.TranscriptTextCache, b.PaneScrollOffsets["transcript"], FullscreenTranscriptViewportLines)
	return b.renderRegion("对话记录", body, "transcript")
}

func (b *FullscreenRendererBackend) renderTasksRegion() string {
	body := SliceViewportText(b.TasksText, b.PaneScrollOffsets["tasks"], FullscreenTasksViewportLines)
	return b.renderRegion("任务概览", body, "tasks")
}

func (b *FullscreenRendererBackend) renderModalRegion() string {
	body := SliceViewportText(b.ModalText, b.PaneScrollOffsets["modal"], FullscreenModalViewportLines)
	return b.renderRegion("权限请求", body, "modal")
}

func (b *FullscreenRendererBackend) renderRegion(title, body, pane string) string {
	marker := " "
	if b.FocusedPane == pane {
		marker = ">"
	}
	if body != "" {
		return marker + " " + title + "\n" + body
	}
	return marker + " " + title
}

func fullscreenBlockSignature(block map[string]any) [2]string {
	return [2]string{stringFromAny(block["kind"]), stringFromAny(block["text"])}
}

func fullscreenTaskBlockSignature(block map[string]any) [3]string {
	return [3]string{stringFromAny(block["kind"]), stringFromAny(block["key"]), stringFromAny(block["text"])}
}

func sharedTranscriptPrefix(a, b [][2]string) int {
	limit := minInt(len(a), len(b))
	for i := 0; i < limit; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return limit
}

func sharedTaskPrefix(a, b [][3]string) int {
	limit := minInt(len(a), len(b))
	for i := 0; i < limit; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return limit
}

func selectionLines(title string, values [][2]string) []string {
	lines := []string{title, ""}
	for idx, value := range values {
		lines = append(lines, fmt.Sprintf("  %d. %s", idx+1, value[1]))
	}
	lines = append(lines, "", "Type a number and press Enter (empty to cancel).")
	return lines
}

func displayRiskLabel(risk string) string {
	switch risk {
	case "low":
		return "低"
	case "high":
		return "高"
	case "critical":
		return "严重"
	default:
		return "中"
	}
}

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func anyStringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s := stringFromAny(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func (b *FullscreenRendererBackend) renderScreen() {
	if !b.renderEnabled || !b.screenActive || b.out == nil {
		return
	}
	sections := []string{b.TranscriptControlText, b.TasksControlText}
	if b.ModalControlText != "" {
		sections = append(sections, b.ModalControlText)
	}
	if b.StatusText != "" {
		sections = append(sections, b.StatusText)
	}
	if b.InputMetaText != "" {
		sections = append(sections, b.InputMetaText)
	}
	fmt.Fprint(b.out, "\x1b[H\x1b[2J")
	fmt.Fprint(b.out, strings.Join(sections, "\n\n"))
	fmt.Fprint(b.out, "\n")
}

func (b *FullscreenRendererBackend) permissionModalState(prompt any, dangerous bool) map[string]any {
	name, input := permissionPromptNameAndInput(prompt)
	targetSummary := firstNonEmpty(
		stringFromAny(input["file_path"]),
		stringFromAny(input["command"]),
	)
	summaryLines := []string{}
	switch name {
	case "run_shell":
		if command := stringFromAny(input["command"]); command != "" {
			summaryLines = append(summaryLines, truncateString(command, 200))
		}
	case "write_file", "edit_file":
		if content := stringFromAny(input["content"]); content != "" {
			summaryLines = append(summaryLines, truncateString(content, 200))
		}
	}
	risk := "medium"
	if dangerous {
		risk = "high"
	}
	if name == "" {
		name = "permission"
	}
	return map[string]any{
		"kind":               "tool_permission",
		"tool_name":          name,
		"target_summary":     targetSummary,
		"display_risk_level": risk,
		"dangerous":          dangerous,
		"summary_lines":      summaryLines,
		"action_labels":      []string{"允许一次", "本会话总是允许", "拒绝"},
	}
}

func (b *FullscreenRendererBackend) readFullscreenText() (string, bool) {
	restore := b.enableRawInput()
	defer restore()

	var builder strings.Builder
	for {
		key, text, err := b.readFullscreenToken()
		if err != nil {
			if builder.Len() > 0 {
				return strings.TrimSpace(builder.String()), true
			}
			return "", false
		}
		switch key {
		case "enter":
			current := strings.TrimSpace(builder.String())
			if b.InputMode == "search_query" {
				b.SetTranscriptSearchQuery(current)
				b.CancelSearchMode()
				builder.Reset()
				b.refreshControls()
				continue
			}
			return current, true
		case "backspace":
			value := []rune(builder.String())
			if len(value) > 0 {
				builder.Reset()
				builder.WriteString(string(value[:len(value)-1]))
			}
		case "c-c":
			if b.PendingPermission {
				return "no", true
			}
			return "", false
		case "c-d":
			return "", false
		default:
			if key != "" {
				if b.HandleKey(key) {
					if key == "c-f" {
						builder.Reset()
					}
					continue
				}
			}
			if text != "" {
				builder.WriteString(text)
			}
		}
	}
}

func (b *FullscreenRendererBackend) enableRawInput() func() {
	if b.inputFile == nil {
		return func() {}
	}
	fd := int(b.inputFile.Fd())
	if !term.IsTerminal(fd) {
		return func() {}
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return func() {}
	}
	return func() {
		_ = term.Restore(fd, oldState)
	}
}

func (b *FullscreenRendererBackend) readFullscreenToken() (string, string, error) {
	r, _, err := b.terminal.in.ReadRune()
	if err != nil {
		return "", "", err
	}
	switch r {
	case '\r', '\n':
		return "enter", "", nil
	case '\x03':
		return "c-c", "", nil
	case '\x04':
		return "c-d", "", nil
	case '\x06':
		return "c-f", "", nil
	case '\x7f', '\b':
		return "backspace", "", nil
	case '\x1b':
		return b.readEscapeKey(), "", nil
	default:
		return "", string(r), nil
	}
}

func (b *FullscreenRendererBackend) readEscapeKey() string {
	sequence := "\x1b"
	for b.terminal.in.Buffered() > 0 {
		next, err := b.terminal.in.ReadByte()
		if err != nil {
			break
		}
		sequence += string(next)
		if len(sequence) >= 3 && isFullscreenEscapeTerminator(next) {
			break
		}
	}
	return fullscreenEscapeKey(sequence)
}

func isFullscreenEscapeTerminator(b byte) bool {
	return (b >= 'A' && b <= 'Z') || b == '~'
}

func fullscreenEscapeKey(sequence string) string {
	switch sequence {
	case "\x1b":
		return "escape"
	case "\x1bOR", "\x1b[13~":
		return "f3"
	case "\x1b[1;2R", "\x1b[13;2~":
		return "s-f3"
	case "\x1bOS", "\x1b[14~":
		return "f4"
	case "\x1b[15~":
		return "f5"
	case "\x1b[17~":
		return "f6"
	case "\x1b[17;2~":
		return "s-f6"
	case "\x1b[5~":
		return "pageup"
	case "\x1b[6~":
		return "pagedown"
	default:
		return "escape"
	}
}

type fullscreenTranscriptWriter struct {
	backend *FullscreenRendererBackend
}

func (w fullscreenTranscriptWriter) Write(p []byte) (int, error) {
	if w.backend != nil {
		w.backend.AppendTranscript(string(p))
	}
	return len(p), nil
}

func sortedStringMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
