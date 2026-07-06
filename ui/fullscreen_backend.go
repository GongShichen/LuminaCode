package ui

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"LuminaCode/agent"
	luminacli "LuminaCode/cli"
	"LuminaCode/config"
	"LuminaCode/skills"
	coretools "LuminaCode/tools"

	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

const (
	FullscreenTranscriptViewportLines = 12
	FullscreenTasksViewportLines      = 5
	FullscreenModalViewportLines      = 8
	fullscreenMinPanelWidth           = 64
	fullscreenMaxPanelWidth           = 118
	fullscreenDefaultHeight           = 30
)

const (
	fullscreenReset  = "\x1b[0m"
	fullscreenBold   = "\x1b[1m"
	fullscreenDim    = "\x1b[2m"
	fullscreenCyan   = "\x1b[36m"
	fullscreenGreen  = "\x1b[32m"
	fullscreenYellow = "\x1b[33m"
	fullscreenRed    = "\x1b[31m"
	fullscreenBlue   = "\x1b[34m"
	fullscreenWhite  = "\x1b[37m"
	fullscreenInvert = "\x1b[7m"
)

const (
	fullscreenEnterScreen = "\x1b[?1049h\x1b[?25l\x1b[?1000h\x1b[?1006h"
	fullscreenExitScreen  = "\x1b[?1006l\x1b[?1000l\x1b[?25h\x1b[?1049l"
)

var fullscreenSpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

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
	InputText                string
	InputCursor              int
	InputDraft               string
	CompletionItems          []luminacli.Completion
	CompletionSelected       int
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
	SelectionTitle           string
	SelectionValues          [][2]string
	SelectionSelected        int
	renderEnabled            bool
	screenActive             bool
}

type fullscreenViewportLayout struct {
	TranscriptLines int
	TasksLines      int
	ModalLines      int
	CompletionLines int
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
	fmt.Fprint(b.out, fullscreenEnterScreen)
	b.renderScreen()
}

func (b *FullscreenRendererBackend) RenderWelcome(sessionID string, skillRegistry *skills.SkillRegistry) {
	b.AppendTranscript("LuminaCode\n")
}

func (b *FullscreenRendererBackend) GetInput(state any) (string, bool) {
	b.PrepareRuntime()
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
	b.InputText = ""
	b.InputCursor = 0
	b.InputDraft = ""
	b.CompletionItems = nil
	b.CompletionSelected = 0
	b.SelectionTitle = ""
	b.SelectionValues = nil
	b.SelectionSelected = 0
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
		fmt.Fprint(b.out, fullscreenExitScreen)
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
	b.InputEnabled = true
	b.InputMode = "selection"
	b.InputPlaceholder = "上下选择，回车确认，Esc 取消。也可输入编号。"
	b.InputText = ""
	b.InputCursor = 0
	b.CompletionItems = nil
	b.CompletionSelected = 0
	b.SelectionTitle = title
	b.SelectionValues = values
	b.SelectionSelected = 0
	b.PendingSelection = true
	b.refreshControls()
	defer func() {
		b.PendingSelection = false
		b.SelectionTitle = ""
		b.SelectionValues = nil
		b.SelectionSelected = 0
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
	case "tool_call":
		b.appendTaskActivityLine("[tool] " + event.Content)
	case "tool_result":
		result := event.Content
		if metadataResult := stringFromAny(event.Metadata["result"]); metadataResult != "" {
			result = metadataResult
		}
		if truthy(event.Metadata["denied"]) {
			b.appendTaskActivityLine("[tool denied]")
		} else if result != "" {
			b.appendTaskActivityLine("[tool result] " + truncateString(result, 200))
		}
	case "error":
		b.StatusText = strings.TrimSpace(event.Content)
		b.refreshControls()
	}
}

func (b *FullscreenRendererBackend) SetInputDraft(draft string) {
	b.InputDraft = draft
	b.setInputBuffer(draft, len([]rune(draft)), 0)
}

func (b *FullscreenRendererBackend) consumeInputDraft() string {
	draft := b.InputDraft
	b.InputDraft = ""
	return draft
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
	layout := b.viewportLayout()
	switch b.activeScrollPane() {
	case "transcript":
		b.ScrollPane("transcript", b.wrappedTextForPane(b.TranscriptTextCache), delta, layout.TranscriptLines)
	case "tasks":
		b.ScrollPane("tasks", b.wrappedTextForPane(b.TasksText), delta, layout.TasksLines)
	case "modal":
		b.ScrollPane("modal", b.wrappedTextForPane(b.ModalText), delta, layout.ModalLines)
	}
}

func (b *FullscreenRendererBackend) ScrollActivePaneToEdge(bottom bool) {
	layout := b.viewportLayout()
	pane := b.activeScrollPane()
	text := ""
	height := layout.TranscriptLines
	switch pane {
	case "tasks":
		text = b.wrappedTextForPane(b.TasksText)
		height = layout.TasksLines
	case "modal":
		text = b.wrappedTextForPane(b.ModalText)
		height = layout.ModalLines
	default:
		pane = "transcript"
		text = b.wrappedTextForPane(b.TranscriptTextCache)
		height = layout.TranscriptLines
	}
	if !bottom {
		b.PaneScrollOffsets[pane] = 0
		return
	}
	lines := splitNonEmptyViewportLines(text)
	b.PaneScrollOffsets[pane] = maxInt(0, len(lines)-maxInt(1, height))
}

func (b *FullscreenRendererBackend) activeScrollPane() string {
	switch b.FocusedPane {
	case "transcript", "tasks", "modal":
		return b.FocusedPane
	default:
		return "transcript"
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
	transcriptLines := b.viewportLayout().TranscriptLines
	b.PaneScrollOffsets["transcript"] = maxInt(0, matchLine-transcriptLines/2)
}

func (b *FullscreenRendererBackend) PageScrollDelta() int {
	layout := b.viewportLayout()
	switch b.activeScrollPane() {
	case "transcript":
		return layout.TranscriptLines
	case "tasks":
		return layout.TasksLines
	case "modal":
		return layout.ModalLines
	default:
		return layout.TranscriptLines
	}
}

func (b *FullscreenRendererBackend) HandleKey(key string) bool {
	if strings.HasPrefix(key, "mouse_scroll_") {
		pane := b.activeScrollPane()
		if _, suffix, ok := strings.Cut(key, ":"); ok && suffix != "" {
			pane = suffix
		}
		direction := 1
		if strings.HasPrefix(key, "mouse_scroll_up") {
			direction = -1
		}
		b.ScrollPaneByName(pane, direction*b.MouseScrollDelta(pane))
		b.refreshControls()
		return true
	}
	switch key {
	case "f6":
		b.CycleFocus(1)
	case "s-f6":
		b.CycleFocus(-1)
	case "up":
		if len(b.CompletionItems) == 0 {
			return false
		}
		b.CompletionSelected--
		if b.CompletionSelected < 0 {
			b.CompletionSelected = len(b.CompletionItems) - 1
		}
	case "down":
		if len(b.CompletionItems) == 0 {
			return false
		}
		b.CompletionSelected = (b.CompletionSelected + 1) % len(b.CompletionItems)
	case "pageup":
		b.ScrollActivePane(-b.PageScrollDelta())
	case "pagedown":
		b.ScrollActivePane(b.PageScrollDelta())
	case "home":
		b.ScrollActivePaneToEdge(false)
	case "end":
		b.ScrollActivePaneToEdge(true)
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

func (b *FullscreenRendererBackend) ScrollPaneByName(pane string, delta int) {
	layout := b.viewportLayout()
	switch pane {
	case "tasks":
		b.ScrollPane("tasks", b.wrappedTextForPane(b.TasksText), delta, layout.TasksLines)
	case "modal":
		b.ScrollPane("modal", b.wrappedTextForPane(b.ModalText), delta, layout.ModalLines)
	default:
		b.ScrollPane("transcript", b.wrappedTextForPane(b.TranscriptTextCache), delta, layout.TranscriptLines)
	}
}

func (b *FullscreenRendererBackend) MouseScrollDelta(pane string) int {
	layout := b.viewportLayout()
	switch pane {
	case "tasks":
		return maxInt(1, layout.TasksLines/2)
	case "modal":
		return maxInt(1, layout.ModalLines/2)
	default:
		return maxInt(1, layout.TranscriptLines/2)
	}
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
	b.scrollTranscriptToBottom()
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
	b.scrollTranscriptToBottom()
}

func BuildFullscreenTranscriptBlocks(entries []map[string]any) []map[string]any {
	var blocks []map[string]any
	for _, entry := range entries {
		kind := stringFromAny(entry["kind"])
		text := stringFromAny(entry["text"])
		if text == "" {
			continue
		}
		if kind != "user" && kind != "assistant" {
			continue
		}
		if len(blocks) > 0 && kind == "assistant" && stringFromAny(blocks[len(blocks)-1]["kind"]) == kind {
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
	case "user":
		return formatDialogueTranscriptBlock(fullscreenBold+fullscreenCyan+"你"+fullscreenReset, text)
	case "assistant":
		return formatDialogueTranscriptBlock(fullscreenBold+fullscreenGreen+"Lumina"+fullscreenReset, text)
	default:
		return text
	}
}

func formatDialogueTranscriptBlock(label, text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	lines := []string{label}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, "  "+line)
	}
	return strings.Join(lines, "\n") + "\n\n"
}

func (b *FullscreenRendererBackend) appendTaskActivityLine(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if b.TasksText == "" || b.TasksText == "Tasks: none" {
		b.TasksText = "Agent Activity:"
	}
	b.TaskChunks = append(b.TaskChunks, text)
	if len(b.TaskChunks) > 50 {
		b.TaskChunks = b.TaskChunks[len(b.TaskChunks)-50:]
	}
	b.TasksText = strings.Join(append([]string{"Agent Activity:"}, b.TaskChunks...), "\n")
	b.refreshControls()
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
		if !frame.InputEnabled {
			return []map[string]any{{"kind": "animation", "key": "animation", "text": b.animatedTaskLine(frame)}}
		}
		return []map[string]any{{"kind": "empty", "key": "empty", "text": "Tasks: none"}}
	}
	var blocks []map[string]any
	if !frame.InputEnabled {
		blocks = append(blocks, map[string]any{"kind": "animation", "key": "animation", "text": b.animatedTaskLine(frame)})
	}
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

func (b *FullscreenRendererBackend) animatedTaskLine(frame RenderFrame) string {
	return fmt.Sprintf("%s 正在执行任务%s", fullscreenSpinner(frame.AnimationFrame), animatedDots(frame.AnimationFrame))
}

func fullscreenSpinner(frame int) string {
	if len(fullscreenSpinnerFrames) == 0 {
		return "-"
	}
	if frame < 0 {
		frame = -frame
	}
	return fullscreenCyan + fullscreenSpinnerFrames[frame%len(fullscreenSpinnerFrames)] + fullscreenReset
}

func animatedDots(frame int) string {
	if frame < 0 {
		frame = -frame
	}
	return strings.Repeat(".", frame%4)
}

func (b *FullscreenRendererBackend) syncInputFromFrame(frame RenderFrame) {
	if b.PendingInput || b.PendingPermission || b.PendingSelection {
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
	if !frame.InputEnabled {
		parts = append(parts, fullscreenSpinner(frame.AnimationFrame)+" Working"+animatedDots(frame.AnimationFrame))
	}
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
		if toolbar := luminacli.BuildSessionToolbar(toolbarState, luminacli.ToolbarSeparator); toolbar != "" {
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
	lines := b.viewportLayout().TranscriptLines
	wrapped := b.wrappedTextForPane(b.TranscriptTextCache)
	body := SliceViewportText(wrapped, b.PaneScrollOffsets["transcript"], lines)
	return b.renderPanel(b.scrollTitle("对话记录", "transcript", wrapped, lines), body, "transcript", lines)
}

func (b *FullscreenRendererBackend) renderTasksRegion() string {
	lines := b.viewportLayout().TasksLines
	wrapped := b.wrappedTextForPane(b.TasksText)
	body := SliceViewportText(wrapped, b.PaneScrollOffsets["tasks"], lines)
	return b.renderPanel(b.scrollTitle("任务概览", "tasks", wrapped, lines), body, "tasks", lines)
}

func (b *FullscreenRendererBackend) renderModalRegion() string {
	if strings.TrimSpace(b.ModalText) == "" {
		return ""
	}
	lines := b.viewportLayout().ModalLines
	wrapped := b.wrappedTextForPane(b.ModalText)
	body := SliceViewportText(wrapped, b.PaneScrollOffsets["modal"], lines)
	return b.renderPanel(b.scrollTitle("权限请求", "modal", wrapped, lines), body, "modal", lines)
}

func (b *FullscreenRendererBackend) renderInputRegion() string {
	if b.PendingSelection {
		return b.renderSelectionInputRegion()
	}
	lines := []string{b.formatInputPromptLine()}
	completionLimit := b.viewportLayout().CompletionLines
	start, end := completionWindow(len(b.CompletionItems), b.CompletionSelected, completionLimit)
	if start > 0 {
		lines = append(lines, fullscreenDim+fmt.Sprintf("  ... %d above", start)+fullscreenReset)
	}
	for idx := start; idx < end; idx++ {
		completion := b.CompletionItems[idx]
		label := firstNonEmpty(completion.Display, completion.Text)
		meta := completion.DisplayMeta
		item := "  " + padVisible(truncateVisible(label, 24), 26)
		if meta != "" {
			item += fullscreenDim + truncateVisible(meta, 58) + fullscreenReset
		}
		if idx == b.CompletionSelected {
			item = fullscreenInvert + item + fullscreenReset
		}
		lines = append(lines, item)
	}
	if end < len(b.CompletionItems) {
		lines = append(lines, fullscreenDim+fmt.Sprintf("  ... %d more", len(b.CompletionItems)-end)+fullscreenReset)
	}
	return b.renderPanel("输入", strings.Join(lines, "\n"), "input", maxInt(1, len(lines)))
}

func (b *FullscreenRendererBackend) renderSelectionInputRegion() string {
	lines := []string{fullscreenDim + firstNonEmpty(b.InputPlaceholder, "上下选择，回车确认，Esc 取消。") + fullscreenReset}
	if b.InputText != "" {
		lines = append(lines, b.formatInputPromptLine())
	}
	limit := maxInt(1, b.viewportLayout().CompletionLines)
	start, end := completionWindow(len(b.SelectionValues), b.SelectionSelected, limit)
	if start > 0 {
		lines = append(lines, fullscreenDim+fmt.Sprintf("  ... %d above", start)+fullscreenReset)
	}
	for idx := start; idx < end; idx++ {
		value := b.SelectionValues[idx]
		label := fmt.Sprintf("%d. %s", idx+1, value[1])
		item := "  " + padVisible(truncateVisible(label, b.panelWidth()-8), b.panelWidth()-8)
		if idx == b.SelectionSelected {
			item = fullscreenInvert + item + fullscreenReset
		}
		lines = append(lines, item)
	}
	if end < len(b.SelectionValues) {
		lines = append(lines, fullscreenDim+fmt.Sprintf("  ... %d more", len(b.SelectionValues)-end)+fullscreenReset)
	}
	title := firstNonEmpty(b.SelectionTitle, "选择")
	return b.renderPanel(title, strings.Join(lines, "\n"), "input", maxInt(1, len(lines)))
}

func completionWindow(count, selected, limit int) (int, int) {
	if count <= 0 || limit <= 0 {
		return 0, 0
	}
	if selected < 0 {
		selected = 0
	}
	if selected >= count {
		selected = count - 1
	}
	if limit >= count {
		return 0, count
	}
	start := selected - limit/2
	if start < 0 {
		start = 0
	}
	maxStart := count - limit
	if start > maxStart {
		start = maxStart
	}
	return start, start + limit
}

func (b *FullscreenRendererBackend) formatInputPromptLine() string {
	prompt := fullscreenGreen + "❯" + fullscreenReset + " "
	if b.InputMode == "search_query" {
		prompt = fullscreenYellow + "/" + fullscreenReset + " "
	}
	if b.InputText != "" {
		return prompt + formatFullscreenInputCursor(b.InputText, b.InputCursor)
	}
	return prompt + fullscreenDim + firstNonEmpty(b.InputPlaceholder, "请输入消息并回车。") + fullscreenReset
}

func (b *FullscreenRendererBackend) renderStatusRegion() string {
	body := firstNonEmpty(b.StatusText, "Model: unknown")
	return b.renderPanel("状态", body, "status", 1)
}

func (b *FullscreenRendererBackend) scrollTitle(title, pane, text string, height int) string {
	lines := splitNonEmptyViewportLines(text)
	if len(lines) <= maxInt(1, height) {
		return title
	}
	start := minInt(maxInt(0, b.PaneScrollOffsets[pane]), len(lines))
	end := minInt(len(lines), start+maxInt(1, height))
	return fmt.Sprintf("%s %d-%d/%d", title, start+1, end, len(lines))
}

func (b *FullscreenRendererBackend) renderPanel(title, body, pane string, minBodyLines int) string {
	width := b.panelWidth()
	inner := maxInt(12, width-2)
	active := b.FocusedPane == pane
	borderStyle, titleStyle, marker := b.panelStyles(pane, body, active)
	headerPlain := fmt.Sprintf(" %s %s ", marker, title)
	header := titleStyle + headerPlain + fullscreenReset + borderStyle
	topFill := maxInt(0, inner-runewidth.StringWidth(headerPlain)-1)

	lines := []string{
		borderStyle + "╭─" + header + strings.Repeat("─", topFill) + "╮" + fullscreenReset,
	}
	bodyLines := strings.Split(body, "\n")
	if strings.TrimSpace(stripANSI(body)) == "" {
		bodyLines = []string{fullscreenDim + "暂无内容" + fullscreenReset}
	}
	for len(bodyLines) < maxInt(1, minBodyLines) {
		bodyLines = append(bodyLines, "")
	}
	if len(bodyLines) > maxInt(1, minBodyLines) {
		bodyLines = bodyLines[:maxInt(1, minBodyLines)]
	}
	for _, line := range bodyLines {
		line = truncateVisible(line, inner-2)
		lines = append(lines, borderStyle+"│"+fullscreenReset+" "+padVisible(line, inner-2)+" "+borderStyle+"│"+fullscreenReset)
	}
	lines = append(lines, borderStyle+"╰"+strings.Repeat("─", inner)+"╯"+fullscreenReset)
	return strings.Join(lines, "\n")
}

func (b *FullscreenRendererBackend) panelStyles(pane, body string, active bool) (string, string, string) {
	borderStyle := fullscreenDim
	titleStyle := fullscreenBold + fullscreenWhite
	marker := " "
	if active {
		borderStyle = fullscreenCyan
		titleStyle = fullscreenBold + fullscreenCyan
		marker = "●"
	}
	switch pane {
	case "transcript":
		if !active {
			titleStyle = fullscreenBold + fullscreenWhite
		}
	case "tasks":
		if !active {
			titleStyle = fullscreenBold + fullscreenBlue
		}
	case "modal":
		borderStyle = fullscreenYellow
		titleStyle = fullscreenBold + fullscreenYellow
		if active {
			marker = "●"
		}
	case "input":
		borderStyle = fullscreenGreen
		titleStyle = fullscreenBold + fullscreenGreen
		if active {
			marker = "●"
		}
	case "status":
		if strings.Contains(strings.ToLower(body), "error") {
			borderStyle = fullscreenRed
			titleStyle = fullscreenBold + fullscreenRed
		}
	}
	return borderStyle, titleStyle, marker
}

func (b *FullscreenRendererBackend) wrappedTextForPane(text string) string {
	width := maxInt(8, b.panelWidth()-4)
	return strings.Join(wrapVisibleLines(text, width), "\n")
}

func (b *FullscreenRendererBackend) scrollTranscriptToBottom() {
	layout := b.viewportLayout()
	wrapped := b.wrappedTextForPane(b.TranscriptTextCache)
	lines := splitNonEmptyViewportLines(wrapped)
	b.PaneScrollOffsets["transcript"] = maxInt(0, len(lines)-layout.TranscriptLines)
}

func wrapVisibleLines(text string, width int) []string {
	if text == "" {
		return []string{""}
	}
	var wrapped []string
	for _, raw := range strings.Split(text, "\n") {
		if raw == "" {
			wrapped = append(wrapped, "")
			continue
		}
		wrapped = append(wrapped, wrapVisibleLine(raw, width)...)
	}
	return wrapped
}

func wrapVisibleLine(text string, width int) []string {
	if width <= 0 {
		return []string{text}
	}
	var lines []string
	var current strings.Builder
	currentWidth := 0
	inEscape := false
	inCSI := false
	for _, r := range text {
		if inEscape {
			current.WriteRune(r)
			if r == '[' && !inCSI {
				inCSI = true
				continue
			}
			if !inCSI || (r >= '@' && r <= '~') {
				inEscape = false
				inCSI = false
			}
			continue
		}
		if r == '\x1b' {
			inEscape = true
			current.WriteRune(r)
			continue
		}
		runeWidth := runewidth.RuneWidth(r)
		if currentWidth > 0 && currentWidth+runeWidth > width {
			lines = append(lines, current.String())
			current.Reset()
			currentWidth = 0
		}
		current.WriteRune(r)
		currentWidth += runeWidth
	}
	lines = append(lines, current.String())
	return lines
}

func splitNonEmptyViewportLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func formatFullscreenInputCursor(text string, cursor int) string {
	runes := []rune(text)
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(runes) {
		cursor = len(runes)
	}
	before := string(runes[:cursor])
	if cursor == len(runes) {
		return before + fullscreenInvert + " " + fullscreenReset
	}
	return before + fullscreenInvert + string(runes[cursor]) + fullscreenReset + string(runes[cursor+1:])
}

func inputCursorMoveWidth(b *FullscreenRendererBackend) int {
	return maxInt(8, b.panelWidth()-8)
}

func moveFullscreenInputCursorVertical(line []rune, cursor, width int, down bool) int {
	if len(line) == 0 || width <= 0 {
		return cursor
	}
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(line) {
		cursor = len(line)
	}
	offsets := fullscreenInputCursorOffsets(line)
	currentCell := offsets[cursor]
	currentRow := currentCell / width
	currentCol := currentCell % width
	targetRow := currentRow - 1
	if down {
		targetRow = currentRow + 1
	}
	if targetRow < 0 {
		return cursor
	}
	maxRow := offsets[len(offsets)-1] / width
	if targetRow > maxRow {
		return cursor
	}
	targetCell := targetRow*width + currentCol
	best := cursor
	bestDistance := absInt(offsets[cursor] - targetCell)
	for idx, offset := range offsets {
		row := offset / width
		if row != targetRow {
			continue
		}
		distance := absInt(offset - targetCell)
		if distance < bestDistance || (distance == bestDistance && idx > best) {
			best = idx
			bestDistance = distance
		}
	}
	return best
}

func fullscreenInputCursorOffsets(line []rune) []int {
	offsets := make([]int, len(line)+1)
	cell := 0
	for idx, r := range line {
		offsets[idx] = cell
		cell += maxInt(1, runewidth.RuneWidth(r))
	}
	offsets[len(line)] = cell
	return offsets
}

func (b *FullscreenRendererBackend) panelWidth() int {
	width, _ := b.terminalSize()
	return width
}

func (b *FullscreenRendererBackend) terminalSize() (int, int) {
	width := 96
	height := fullscreenDefaultHeight
	if b.inputFile != nil {
		if termWidth, termHeight, err := term.GetSize(int(b.inputFile.Fd())); err == nil && termWidth > 0 {
			width = termWidth - 2
			if termHeight > 0 {
				height = termHeight
			}
		}
	}
	if columns, ok := positiveEnvInt("COLUMNS"); ok {
		width = columns - 2
	}
	if lines, ok := positiveEnvInt("LINES"); ok {
		height = lines
	}
	width = maxInt(fullscreenMinPanelWidth, minInt(width, fullscreenMaxPanelWidth))
	height = maxInt(12, height)
	return width, height
}

func positiveEnvInt(name string) (int, bool) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

func (b *FullscreenRendererBackend) viewportLayout() fullscreenViewportLayout {
	_, height := b.terminalSize()
	completionLines := 0
	if len(b.CompletionItems) > 0 || b.PendingSelection {
		completionLines = minInt(7, maxInt(1, (height-22)/2))
	}
	modalLines := 0
	if strings.TrimSpace(b.ModalText) != "" {
		modalLines = minInt(FullscreenModalViewportLines, maxInt(3, height/5))
	}

	fixedLines := 1
	if b.StatusText != "" {
		fixedLines += 5
	}
	fixedLines += 5 + completionLines
	if modalLines > 0 {
		fixedLines += 4 + modalLines
	}
	fixedLines += 8

	availableBodyLines := maxInt(2, height-fixedLines)
	tasksLines := minInt(FullscreenTasksViewportLines, maxInt(1, availableBodyLines/4))
	transcriptLines := maxInt(1, availableBodyLines-tasksLines)

	return fullscreenViewportLayout{
		TranscriptLines: transcriptLines,
		TasksLines:      tasksLines,
		ModalLines:      maxInt(1, modalLines),
		CompletionLines: completionLines,
	}
}

func padVisible(text string, width int) string {
	current := runewidth.StringWidth(stripANSI(text))
	if current >= width {
		return text
	}
	return text + strings.Repeat(" ", width-current)
}

func truncateVisible(text string, width int) string {
	if runewidth.StringWidth(stripANSI(text)) <= width {
		return text
	}
	var out strings.Builder
	current := 0
	inEscape := false
	inCSI := false
	for _, r := range text {
		if inEscape {
			out.WriteRune(r)
			if r == '[' && !inCSI {
				inCSI = true
				continue
			}
			if !inCSI || (r >= '@' && r <= '~') {
				inEscape = false
				inCSI = false
			}
			continue
		}
		if r == '\x1b' {
			inEscape = true
			out.WriteRune(r)
			continue
		}
		next := current + runewidth.RuneWidth(r)
		if next > maxInt(0, width-1) {
			break
		}
		out.WriteRune(r)
		current = next
	}
	if strings.Contains(text, "\x1b[") {
		return out.String() + "…" + fullscreenReset
	}
	return out.String() + "…"
}

func stripANSI(text string) string {
	var out strings.Builder
	inEscape := false
	inCSI := false
	for _, r := range text {
		if inEscape {
			if r == '[' && !inCSI {
				inCSI = true
				continue
			}
			if !inCSI || (r >= '@' && r <= '~') {
				inEscape = false
				inCSI = false
			}
			continue
		}
		if r == '\x1b' {
			inEscape = true
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
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
	sections := []string{b.renderHeader(), b.TranscriptControlText, b.TasksControlText}
	if b.ModalControlText != "" {
		sections = append(sections, b.ModalControlText)
	}
	if b.StatusText != "" {
		sections = append(sections, b.renderStatusRegion())
	}
	if b.InputMetaText != "" {
		sections = append(sections, b.renderInputRegion())
	}
	fmt.Fprint(b.out, "\x1b[H\x1b[2J")
	screen := strings.ReplaceAll(strings.Join(sections, "\n")+"\n", "\n", "\r\n")
	fmt.Fprint(b.out, screen)
}

func (b *FullscreenRendererBackend) renderHeader() string {
	width := b.panelWidth()
	title := fullscreenBold + fullscreenCyan + "LuminaCode" + fullscreenReset
	metaPlain := firstNonEmpty(b.InputMetaText, "输入就绪")
	meta := fullscreenDim + metaPlain + fullscreenReset
	contentWidth := width - runewidth.StringWidth("LuminaCode") - runewidth.StringWidth(metaPlain) - 1
	if contentWidth < 1 {
		contentWidth = 1
	}
	return title + strings.Repeat(" ", contentWidth) + meta
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

	line := []rune(b.consumeInputDraft())
	cursor := len(line)
	selected := 0
	b.setInputBuffer(string(line), cursor, 0)
	defer b.setInputBuffer("", 0, 0)
	for {
		key, text, err := b.readFullscreenToken()
		if err != nil {
			if len(line) > 0 {
				return strings.TrimSpace(string(line)), true
			}
			return "", false
		}
		switch key {
		case "enter":
			current := strings.TrimSpace(string(line))
			if b.PendingSelection || b.InputMode == "selection" {
				if current == "" && len(b.SelectionValues) > 0 {
					return fmt.Sprint(b.SelectionSelected + 1), true
				}
				return current, true
			}
			if b.InputMode == "search_query" {
				b.SetTranscriptSearchQuery(current)
				b.CancelSearchMode()
				line = line[:0]
				cursor = 0
				selected = 0
				b.setInputBuffer("", 0, 0)
				b.refreshControls()
				continue
			}
			return current, true
		case "backspace":
			if cursor > 0 {
				line = append(line[:cursor-1], line[cursor:]...)
				cursor--
				selected = 0
				b.setInputBuffer(string(line), cursor, selected)
			}
		case "delete":
			if cursor < len(line) {
				line = append(line[:cursor], line[cursor+1:]...)
				selected = 0
				b.setInputBuffer(string(line), cursor, selected)
			}
		case "left":
			if cursor > 0 {
				cursor--
				b.setInputBuffer(string(line), cursor, selected)
			}
		case "right":
			if cursor < len(line) {
				cursor++
				b.setInputBuffer(string(line), cursor, selected)
			}
		case "home":
			if len(line) > 0 {
				cursor = 0
				b.setInputBuffer(string(line), cursor, selected)
			} else if b.HandleKey(key) {
				continue
			}
		case "end":
			if len(line) > 0 {
				cursor = len(line)
				b.setInputBuffer(string(line), cursor, selected)
			} else if b.HandleKey(key) {
				continue
			}
		case "tab":
			if len(b.CompletionItems) > 0 {
				if selected < 0 || selected >= len(b.CompletionItems) {
					selected = 0
				}
				line, cursor = applyTerminalCompletion(line, cursor, b.CompletionItems[selected])
				selected = 0
				b.setInputBuffer(string(line), cursor, selected)
			}
		case "c-c":
			if b.PendingPermission {
				return "no", true
			}
			return "", false
		case "c-d":
			return "", false
		case "escape":
			if b.PendingSelection || b.InputMode == "selection" {
				return "", false
			}
			if b.PendingPermission || b.InputMode == "permission_modal" {
				return "no", true
			}
			if b.InputMode == "search_query" {
				b.CancelSearchMode()
				line = line[:0]
				cursor = 0
				selected = 0
				b.setInputBuffer("", 0, 0)
				b.refreshControls()
			}
		default:
			if key != "" {
				if key == "up" || key == "down" {
					if b.PendingSelection || b.InputMode == "selection" {
						b.MoveSelection(key == "down")
						b.refreshControls()
						continue
					}
					completions := b.terminal.currentSlashCompletions(line, cursor)
					if len(completions) > 0 {
						if key == "up" {
							selected = (selected - 1 + len(completions)) % len(completions)
						} else {
							selected = (selected + 1) % len(completions)
						}
						b.setInputBuffer(string(line), cursor, selected)
						continue
					}
					cursor = moveFullscreenInputCursorVertical(line, cursor, inputCursorMoveWidth(b), key == "down")
					b.setInputBuffer(string(line), cursor, selected)
					continue
				}
				if b.HandleKey(key) {
					if key == "c-f" {
						line = line[:0]
						cursor = 0
						selected = 0
						b.setInputBuffer("", 0, 0)
					}
					continue
				}
			}
			if text != "" {
				for _, r := range text {
					if r == 0 || unicode.IsControl(r) {
						continue
					}
					line = append(line[:cursor], append([]rune{r}, line[cursor:]...)...)
					cursor++
				}
				selected = 0
				b.setInputBuffer(string(line), cursor, selected)
			}
		}
	}
}

func (b *FullscreenRendererBackend) setInputBuffer(text string, cursor int, selected int) {
	b.InputText = text
	line := []rune(text)
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(line) {
		cursor = len(line)
	}
	b.InputCursor = cursor
	if b.PendingSelection || b.InputMode == "selection" {
		b.CompletionItems = nil
		b.CompletionSelected = 0
		b.refreshControls()
		return
	}
	completions := b.terminal.currentSlashCompletions(line, cursor)
	b.CompletionItems = completions
	if len(completions) == 0 {
		b.CompletionSelected = 0
	} else {
		if selected < 0 {
			selected = 0
		}
		if selected >= len(completions) {
			selected = len(completions) - 1
		}
		b.CompletionSelected = selected
	}
	b.refreshControls()
}

func (b *FullscreenRendererBackend) MoveSelection(down bool) {
	count := len(b.SelectionValues)
	if count == 0 {
		b.SelectionSelected = 0
		return
	}
	if down {
		b.SelectionSelected = (b.SelectionSelected + 1) % count
	} else {
		b.SelectionSelected = (b.SelectionSelected - 1 + count) % count
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
	case '\t':
		return "tab", "", nil
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
	return b.fullscreenEscapeKey(sequence)
}

func isFullscreenEscapeTerminator(b byte) bool {
	return (b >= 'A' && b <= 'Z') || b == '~' || b == 'm'
}

func (b *FullscreenRendererBackend) fullscreenEscapeKey(sequence string) string {
	if key := b.fullscreenMouseKey(sequence); key != "" {
		return key
	}
	switch sequence {
	case "\x1b":
		return "escape"
	case "\x1b[A":
		return "up"
	case "\x1b[B":
		return "down"
	case "\x1b[C":
		return "right"
	case "\x1b[D":
		return "left"
	case "\x1b[H", "\x1bOH", "\x1b[1~", "\x1b[7~":
		return "home"
	case "\x1b[F", "\x1bOF", "\x1b[4~", "\x1b[8~":
		return "end"
	case "\x1b[3~":
		return "delete"
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

func (b *FullscreenRendererBackend) fullscreenMouseKey(sequence string) string {
	if !strings.HasPrefix(sequence, "\x1b[<") {
		return ""
	}
	rest := strings.TrimPrefix(sequence, "\x1b[<")
	parts := strings.Split(rest, ";")
	if len(parts) < 3 {
		return "escape"
	}
	button, err := strconv.Atoi(parts[0])
	if err != nil {
		return "escape"
	}
	y, _ := strconv.Atoi(strings.TrimRight(parts[2], "Mm"))
	if button&64 == 0 {
		return "escape"
	}
	pane := b.paneAtTerminalY(y)
	switch button % 4 {
	case 0:
		return "mouse_scroll_up:" + pane
	case 1:
		return "mouse_scroll_down:" + pane
	default:
		return "escape"
	}
}

func (b *FullscreenRendererBackend) paneAtTerminalY(y int) string {
	if y <= 0 {
		return b.activeScrollPane()
	}
	layout := b.viewportLayout()
	line := 1
	transcriptStart := line + 1
	transcriptEnd := transcriptStart + layout.TranscriptLines + 1
	if y >= transcriptStart && y <= transcriptEnd {
		return "transcript"
	}
	tasksStart := transcriptEnd + 1
	tasksEnd := tasksStart + layout.TasksLines + 1
	if y >= tasksStart && y <= tasksEnd {
		return "tasks"
	}
	nextStart := tasksEnd + 1
	if strings.TrimSpace(b.ModalText) != "" {
		modalEnd := nextStart + layout.ModalLines + 1
		if y >= nextStart && y <= modalEnd {
			return "modal"
		}
	}
	return b.activeScrollPane()
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

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
