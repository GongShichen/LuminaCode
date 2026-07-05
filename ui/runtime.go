package ui

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"LuminaCode/agent"
	luminacli "LuminaCode/cli"
	"LuminaCode/config"
	"LuminaCode/skills"
	coretools "LuminaCode/tools"
)

type UiEvent struct {
	Type     string         `json:"type"`
	Content  string         `json:"content"`
	Metadata map[string]any `json:"metadata"`
}

type TaskUiEvent struct {
	Type       string         `json:"type"`
	TaskID     string         `json:"task_id"`
	Record     map[string]any `json:"record"`
	Summary    string         `json:"summary"`
	ResultText string         `json:"result_text"`
}

type RenderFrame struct {
	TranscriptEntries   []map[string]any          `json:"transcript_entries"`
	Tasks               map[string]map[string]any `json:"tasks"`
	TaskActivityEntries []map[string]any          `json:"task_activity_entries"`
	AnimationFrame      int                       `json:"animation_frame"`
	InputEnabled        bool                      `json:"input_enabled"`
	InputMode           string                    `json:"input_mode"`
	InputPlaceholder    string                    `json:"input_placeholder"`
	ActiveModal         string                    `json:"active_modal"`
	ModalState          map[string]any            `json:"modal_state"`
	Warnings            []string                  `json:"warnings"`
	Errors              []string                  `json:"errors"`
	PermissionAudit     []map[string]any          `json:"permission_audit"`
	SessionCostText     string                    `json:"session_cost_text"`
	SessionCostValue    float64                   `json:"session_cost_value"`
	ModelName           string                    `json:"model_name"`
	ContextUsedTokens   int                       `json:"context_used_tokens"`
	ContextLimitTokens  int                       `json:"context_limit_tokens"`
}

func NewRenderFrame() RenderFrame {
	return RenderFrame{
		TranscriptEntries:   []map[string]any{},
		Tasks:               map[string]map[string]any{},
		TaskActivityEntries: []map[string]any{},
		InputEnabled:        true,
		InputMode:           "normal",
		InputPlaceholder:    "",
		ActiveModal:         "none",
		ModalState:          map[string]any{},
		Warnings:            []string{},
		Errors:              []string{},
		PermissionAudit:     []map[string]any{},
		SessionCostText:     "",
		SessionCostValue:    0,
	}
}

type RendererBackend interface {
	Mount(RenderFrame)
	Update(RenderFrame)
	ShowModal(map[string]any)
	ClearModal()
	Shutdown(RenderFrame)
	AskPermission(any, bool) string
	PickFromList(string, [][2]string) *string
}

type EventRenderer interface {
	RenderEvent(agent.StreamEvent)
}

type FrameTranscriptBackend interface {
	UsesFrameTranscript() bool
}

type RuntimePreparer interface {
	PrepareRuntime()
}

const uiAnimationInterval = 120 * time.Millisecond

type TaskEventSink struct {
	queue chan TaskUiEvent
}

func NewTaskEventSink() *TaskEventSink {
	return &TaskEventSink{queue: make(chan TaskUiEvent, 128)}
}

func (s *TaskEventSink) Emit(event TaskUiEvent) {
	select {
	case s.queue <- event:
	default:
	}
}

func (s *TaskEventSink) EmitTaskEvent(event agent.TaskUIEvent) {
	s.Emit(TaskUiEvent{
		Type:       event.Type,
		TaskID:     event.TaskID,
		Record:     event.Record,
		Summary:    event.Summary,
		ResultText: event.ResultText,
	})
}

func (s *TaskEventSink) Get() TaskUiEvent {
	return <-s.queue
}

type UiRuntime struct {
	Engine                 any
	UI                     RendererBackend
	Frame                  RenderFrame
	TaskSink               *TaskEventSink
	Dirty                  bool
	LastFlushAt            time.Time
	Mounted                bool
	FrameInterval          time.Duration
	MaxTaskActivityEntries int
	CurrentState           *agent.AgentState
}

func NewUiRuntime(engine any, backend RendererBackend) *UiRuntime {
	return &UiRuntime{
		Engine:                 engine,
		UI:                     backend,
		Frame:                  NewRenderFrame(),
		TaskSink:               NewTaskEventSink(),
		FrameInterval:          time.Second / 30,
		MaxTaskActivityEntries: 50,
	}
}

func (r *UiRuntime) MountStateSnapshot(state *agent.AgentState) {
	r.CurrentState = state
	r.Frame = NewRenderFrame()
	r.Frame.TranscriptEntries = TranscriptEntriesFromState(state)
	r.Frame.InputEnabled = true
	r.Frame.InputMode = "normal"
	r.Frame.InputPlaceholder = "请输入消息并回车。"
	r.refreshContextWindowFrame()
	if preparer, ok := r.UI.(RuntimePreparer); ok {
		preparer.PrepareRuntime()
	}
	if r.UI != nil {
		r.UI.Mount(r.Frame)
		r.Mounted = true
	}
	r.Dirty = false
}

func TranscriptEntriesFromState(state *agent.AgentState) []map[string]any {
	if state == nil {
		return []map[string]any{}
	}
	var entries []map[string]any
	for _, message := range state.Messages {
		if shouldHideTranscriptMessage(message) {
			continue
		}
		role := stringFromAny(message["role"])
		switch role {
		case "user":
			entries = appendUserTranscriptMessage(entries, message["content"])
		case "assistant":
			entries = appendAssistantTranscriptMessage(entries, message["content"])
		}
	}
	return entries
}

func (r *UiRuntime) RunSubmitMessage(ctx context.Context, userInput string, state *agent.AgentState, sessionID string) []agent.StreamEvent {
	r.CurrentState = state
	taskRuntime := r.taskRuntime()
	if taskRuntime != nil {
		taskRuntime.SetTaskEventSink(r.TaskSink)
		defer taskRuntime.SetTaskEventSink(nil)
	}
	r.appendTranscriptEntry("user", userInput)
	r.Frame.InputEnabled = false
	r.Frame.InputMode = "normal"
	r.Frame.InputPlaceholder = "Agent is responding..."
	r.refreshContextWindowFrame()
	if preparer, ok := r.UI.(RuntimePreparer); ok {
		preparer.PrepareRuntime()
	}
	if r.UI != nil && !r.Mounted {
		r.UI.Mount(r.Frame)
		r.Mounted = true
	}
	r.markDirty()
	r.Flush(true)
	submitter, ok := r.Engine.(interface {
		SubmitMessage(context.Context, string, *agent.AgentState, ...string) <-chan agent.StreamEvent
	})
	if !ok {
		if r.Dirty {
			r.Flush(true)
		}
		return nil
	}
	var events []agent.StreamEvent
	eventCh := submitter.SubmitMessage(ctx, userInput, state, sessionID)
	ticker := time.NewTicker(uiAnimationInterval)
	defer ticker.Stop()
	for eventCh != nil {
		select {
		case event, open := <-eventCh:
			if !open {
				eventCh = nil
				continue
			}
			uiEvent := r.ToUIEvent(event)
			if uiEvent.Type == "permission_requested" {
				r.HandlePermissionEvent(uiEvent)
			} else {
				r.ApplyUIEvent(uiEvent)
				r.MaybeRenderEvent(event)
				r.FlushIfDue()
			}
			r.DrainTaskEvents()
			events = append(events, event)
		case <-ticker.C:
			r.advanceAnimationFrame()
			r.DrainTaskEvents()
			r.Flush(true)
		case <-ctx.Done():
			if r.Dirty {
				r.Flush(true)
			}
			return events
		}
	}
	r.finishSubmitFrame()
	r.Flush(true)
	return events
}

func (r *UiRuntime) taskRuntime() *agent.AgentTaskRuntime {
	if provider, ok := r.Engine.(interface {
		TaskRuntimeForUI() *agent.AgentTaskRuntime
	}); ok {
		return provider.TaskRuntimeForUI()
	}
	if queryEngine, ok := r.Engine.(*agent.QueryEngine); ok && queryEngine != nil && queryEngine.CoreEngine != nil {
		return queryEngine.CoreEngine.TaskRuntime
	}
	if coreEngine, ok := r.Engine.(*agent.CoreExecutionEngine); ok && coreEngine != nil {
		return coreEngine.TaskRuntime
	}
	return nil
}

func (r *UiRuntime) resolvePermissionEvent(event UiEvent, answer string) {
	granted := answer == "once" || answer == "always"
	if _, _, ok := skillShellRequestParts(event.Metadata["skill_shell_request"]); ok {
		if resolver, ok := r.Engine.(interface{ ResolveSkillPermission(bool) }); ok {
			resolver.ResolveSkillPermission(granted)
		}
		return
	}
	if _, ok := normalizeMCPTrustRequest(event.Metadata["mcp_trust_request"]); ok {
		if resolver, ok := r.Engine.(interface{ ResolveMCPTrust(bool) }); ok {
			resolver.ResolveMCPTrust(granted)
		}
		return
	}
	toolName := ""
	switch call := event.Metadata["tool_call"].(type) {
	case coretools.ToolCall:
		toolName = call.Name
	case *coretools.ToolCall:
		if call != nil {
			toolName = call.Name
		}
	}
	if resolver, ok := r.Engine.(interface{ ResolvePermission(string, string) }); ok {
		if granted {
			resolver.ResolvePermission(answer, toolName)
		} else {
			resolver.ResolvePermission("deny", "")
		}
	}
}

func (r *UiRuntime) ToUIEvent(event agent.StreamEvent) UiEvent {
	mapping := map[string]string{
		"text":              "assistant_delta",
		"done":              "assistant_done",
		"thinking":          "thinking_delta",
		"tool_call":         "tool_call_started",
		"tool_result":       "tool_call_finished",
		"permission_needed": "permission_requested",
		"error":             "ui_fatal",
		"cost":              "session_cost_updated",
	}
	eventType := mapping[event.Type]
	if eventType == "" {
		eventType = event.Type
	}
	metadata := map[string]any{}
	for key, value := range event.Metadata {
		metadata[key] = value
	}
	return UiEvent{Type: eventType, Content: event.Content, Metadata: metadata}
}

func (r *UiRuntime) ApplyUIEvent(event UiEvent) {
	switch event.Type {
	case "assistant_delta":
		r.appendTranscriptEntry("assistant", event.Content)
		r.markDirty()
	case "thinking_delta":
		if event.Content != "" {
			r.Frame.InputPlaceholder = "Agent is thinking..."
			r.markDirty()
		}
	case "tool_call_started":
		r.recordInlineActivity("tool_call", FormatToolCallEntry(event))
	case "tool_call_finished":
		r.recordInlineActivity("tool_result", FormatToolResultEntry(event))
	case "session_cost_updated":
		r.Frame.SessionCostText = event.Content
		r.Frame.SessionCostValue = floatFromAny(event.Metadata["cost"])
		r.markDirty()
	case "ui_warning":
		if event.Content != "" {
			r.Frame.Warnings = append(r.Frame.Warnings, event.Content)
			r.markDirty()
		}
	case "ui_fatal":
		if event.Content != "" {
			r.Frame.Errors = append(r.Frame.Errors, event.Content)
			r.markDirty()
		}
	}
}

func (r *UiRuntime) DrainTaskEvents() {
	updated := false
	for {
		select {
		case taskEvent := <-r.TaskSink.queue:
			r.Frame.Tasks[taskEvent.TaskID] = taskEvent.Record
			if taskEvent.Type == "task_summary_available" {
				r.recordTaskActivity(taskEvent)
			}
			updated = true
		default:
			if updated {
				r.markDirty()
				r.FlushIfDue()
			}
			return
		}
	}
}

func (r *UiRuntime) HandlePermissionEvent(event UiEvent) string {
	r.Frame.ActiveModal = "permission_request"
	r.Frame.InputEnabled = true
	r.Frame.InputMode = "permission_modal"
	r.Frame.InputPlaceholder = "Answer with y/n/a/d and press Enter."
	r.Frame.ModalState = r.BuildPermissionModalState(event)
	r.markDirty()
	if r.UI != nil {
		r.UI.ShowModal(r.Frame.ModalState)
	}
	answer := "deny"
	if r.UI != nil {
		if _, _, ok := skillShellRequestParts(event.Metadata["skill_shell_request"]); ok {
			answer = r.UI.AskPermission(permissionPromptFromEvent(event), true)
		} else if _, ok := normalizeMCPTrustRequest(event.Metadata["mcp_trust_request"]); ok {
			answer = r.UI.AskPermission(permissionPromptFromEvent(event), true)
		} else if tc := event.Metadata["tool_call"]; tc != nil {
			answer = r.UI.AskPermission(tc, truthy(event.Metadata["dangerous"]) || stringFromAny(event.Metadata["risk"]) == "high")
		} else {
			answer = r.UI.AskPermission(permissionPromptFromEvent(event), true)
		}
	}
	r.resolvePermissionEvent(event, answer)
	r.RecordPermissionAudit(event, answer)
	r.Frame.ActiveModal = "none"
	r.Frame.InputEnabled = false
	r.Frame.InputMode = "normal"
	r.Frame.InputPlaceholder = "Agent is responding..."
	r.Frame.ModalState = map[string]any{}
	r.markDirty()
	if r.UI != nil {
		r.UI.ClearModal()
	}
	r.Flush(true)
	return answer
}

func (r *UiRuntime) BuildPermissionModalState(event UiEvent) map[string]any {
	backendRisk := stringFromAny(event.Metadata["risk"])
	if backendRisk == "" {
		if truthy(event.Metadata["dangerous"]) {
			backendRisk = "high"
		} else {
			backendRisk = "normal"
		}
	}
	displayRisk := luminacli.TranslateBackendRiskLevel(backendRisk)
	toolName := ""
	targetSummary := ""
	summaryLines := []string{}
	kind := "permission_request"
	if tc, ok := event.Metadata["tool_call"].(coretools.ToolCall); ok {
		toolName = tc.Name
		targetSummary = firstNonEmpty(stringFromAny(tc.Input["file_path"]), stringFromAny(tc.Input["command"]), stringFromAny(tc.Input["pattern"]))
		switch toolName {
		case "run_shell":
			summaryLines = []string{truncateString(stringFromAny(tc.Input["command"]), 200)}
		case "write_file", "edit_file":
			summaryLines = []string{truncateString(stringFromAny(tc.Input["content"]), 200)}
		}
		kind = "tool_permission"
	} else if tcPtr, ok := event.Metadata["tool_call"].(*coretools.ToolCall); ok && tcPtr != nil {
		copied := *tcPtr
		event.Metadata["tool_call"] = copied
		return r.BuildPermissionModalState(event)
	} else if command, ok := skillShellCommand(event.Metadata["skill_shell_request"]); ok {
		toolName = "skill-shell"
		targetSummary = command
		summaryLines = []string{truncateString(targetSummary, 200)}
		kind = "skill_shell_permission"
	} else if req, ok := normalizeMCPTrustRequest(event.Metadata["mcp_trust_request"]); ok {
		toolName = "mcp-project-trust"
		targetSummary = FormatMCPTrustTarget(req)
		summaryLines = []string{truncateString(targetSummary, 200)}
		kind = "mcp_trust_permission"
	}
	return map[string]any{
		"kind":               kind,
		"tool_name":          toolName,
		"target_summary":     targetSummary,
		"backend_risk_level": backendRisk,
		"display_risk_level": displayRisk,
		"dangerous":          truthy(event.Metadata["dangerous"]),
		"summary_lines":      nonEmptyStrings(summaryLines),
		"action_labels":      append([]string{}, luminacli.Phase1PermissionActionLabels...),
	}
}

func FormatMCPTrustTarget(request []map[string]any) string {
	var parts []string
	for _, item := range request {
		name := stringFromAny(item["name"])
		command := firstNonEmpty(stringFromAny(item["command"]), stringFromAny(item["url"]))
		argText := joinAnySlice(item["args"])
		parts = append(parts, strings.TrimSpace(fmt.Sprintf("%s: %s %s", name, command, argText)))
	}
	return strings.Join(parts, "; ")
}

func (r *UiRuntime) RecordPermissionAudit(event UiEvent, answer string) {
	state := r.Frame.ModalState
	r.Frame.PermissionAudit = append(r.Frame.PermissionAudit, map[string]any{
		"tool_name":      state["tool_name"],
		"risk_level":     firstNonEmpty(stringFromAny(state["backend_risk_level"]), "low"),
		"dangerous":      truthy(state["dangerous"]),
		"target_summary": state["target_summary"],
		"decision":       answer,
	})
}

func (r *UiRuntime) Flush(force bool) {
	if !r.Dirty && !force {
		return
	}
	r.refreshContextWindowFrame()
	if r.UI != nil {
		r.UI.Update(r.Frame)
	}
	r.Dirty = false
	r.LastFlushAt = time.Now()
}

func (r *UiRuntime) FlushIfDue() {
	if !r.Dirty {
		return
	}
	if r.LastFlushAt.IsZero() || time.Since(r.LastFlushAt) >= r.FrameInterval {
		r.Flush(true)
	}
}

func (r *UiRuntime) MaybeRenderEvent(event agent.StreamEvent) {
	if frameBackend, ok := r.UI.(FrameTranscriptBackend); ok && frameBackend.UsesFrameTranscript() {
		return
	}
	if renderer, ok := r.UI.(EventRenderer); ok {
		renderer.RenderEvent(event)
	}
}

func (r *UiRuntime) Shutdown() {
	if r.Dirty {
		r.Flush(true)
	}
	if r.UI != nil {
		r.UI.Shutdown(r.Frame)
	}
}

func (r *UiRuntime) markDirty() {
	r.Dirty = true
}

func (r *UiRuntime) advanceAnimationFrame() {
	if r.Frame.InputEnabled || r.Frame.ActiveModal != "none" {
		return
	}
	r.Frame.AnimationFrame++
	r.markDirty()
}

func (r *UiRuntime) finishSubmitFrame() {
	r.Frame.InputEnabled = true
	r.Frame.InputMode = "normal"
	r.Frame.InputPlaceholder = "请输入消息并回车。"
	r.markDirty()
}

func (r *UiRuntime) refreshContextWindowFrame() {
	cfg, ok := r.engineConfig()
	if !ok {
		return
	}
	state := r.latestState()
	snapshot := BuildContextWindowSnapshot(cfg, state)
	r.Frame.ModelName = snapshot.ModelName
	r.Frame.ContextUsedTokens = snapshot.UsedTokens
	r.Frame.ContextLimitTokens = snapshot.LimitTokens
}

func (r *UiRuntime) engineConfig() (config.Config, bool) {
	switch engine := r.Engine.(type) {
	case *agent.QueryEngine:
		if engine == nil {
			return config.Config{}, false
		}
		return engine.Config, true
	case *agent.CoreExecutionEngine:
		if engine == nil {
			return config.Config{}, false
		}
		return engine.Config, true
	default:
		return config.Config{}, false
	}
}

func (r *UiRuntime) latestState() *agent.AgentState {
	if queryEngine, ok := r.Engine.(*agent.QueryEngine); ok && queryEngine != nil && queryEngine.CoreEngine != nil && queryEngine.CoreEngine.LastState != nil {
		return queryEngine.CoreEngine.LastState
	}
	if coreEngine, ok := r.Engine.(*agent.CoreExecutionEngine); ok && coreEngine != nil && coreEngine.LastState != nil {
		return coreEngine.LastState
	}
	return r.CurrentState
}

func (r *UiRuntime) appendTranscriptEntry(kind, text string) {
	if kind == "user" {
		text = strings.TrimSpace(text)
	}
	if text == "" || strings.TrimSpace(text) == "" {
		return
	}
	entries := r.Frame.TranscriptEntries
	if len(entries) > 0 && entries[len(entries)-1]["kind"] == kind && kind == "assistant" {
		entries[len(entries)-1]["text"] = stringFromAny(entries[len(entries)-1]["text"]) + text
		r.Frame.TranscriptEntries = entries
		return
	}
	r.Frame.TranscriptEntries = append(entries, map[string]any{"kind": kind, "text": text})
}

func (r *UiRuntime) recordTaskActivity(taskEvent TaskUiEvent) {
	record := taskEvent.Record
	activity := map[string]any{
		"task_id":        taskEvent.TaskID,
		"worker_label":   firstNonEmpty(stringFromAny(record["worker_label"]), taskEvent.TaskID),
		"status":         firstNonEmpty(stringFromAny(record["status"]), "unknown"),
		"summary":        taskEvent.Summary,
		"result_text":    taskEvent.ResultText,
		"input_tokens":   intFromAny(record["input_tokens"]),
		"output_tokens":  intFromAny(record["output_tokens"]),
		"tool_use_count": intFromAny(record["tool_use_count"]),
		"duration_ms":    intFromAny(record["duration_ms"]),
	}
	entries := r.Frame.TaskActivityEntries
	if len(entries) > 0 && mapsEqual(entries[len(entries)-1], activity) {
		return
	}
	entries = append(entries, activity)
	if len(entries) > r.MaxTaskActivityEntries {
		entries = entries[len(entries)-r.MaxTaskActivityEntries:]
	}
	r.Frame.TaskActivityEntries = entries
}

func (r *UiRuntime) recordInlineActivity(kind, summary string) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return
	}
	entry := map[string]any{
		"task_id":        "agent:" + kind,
		"worker_label":   "agent",
		"status":         kind,
		"summary":        summary,
		"result_text":    "",
		"input_tokens":   0,
		"output_tokens":  0,
		"tool_use_count": 0,
		"duration_ms":    0,
	}
	entries := r.Frame.TaskActivityEntries
	if len(entries) > 0 && mapsEqual(entries[len(entries)-1], entry) {
		return
	}
	entries = append(entries, entry)
	if len(entries) > r.MaxTaskActivityEntries {
		entries = entries[len(entries)-r.MaxTaskActivityEntries:]
	}
	r.Frame.TaskActivityEntries = entries
	r.markDirty()
}

func FormatToolCallEntry(event UiEvent) string {
	toolName := firstNonEmpty(event.Content, stringFromAny(event.Metadata["tool_name"]), "tool")
	return fmt.Sprintf("[tool] %s", toolName)
}

func FormatToolResultEntry(event UiEvent) string {
	if truthy(event.Metadata["denied"]) {
		return "[tool denied]"
	}
	result := event.Content
	if metadataResult := stringFromAny(event.Metadata["result"]); metadataResult != "" {
		result = metadataResult
	}
	if result == "" {
		return "[tool result]"
	}
	preview := truncateString(strings.Split(strings.TrimSpace(result), "\n")[0], 200)
	return fmt.Sprintf("[tool result] %s", preview)
}

func shouldHideTranscriptMessage(message map[string]any) bool {
	if truthy(message["isMeta"]) {
		return true
	}
	metadata, _ := message["metadata"].(map[string]any)
	return truthy(metadata["lumina_skill_context"]) || truthy(metadata["lumina_memory_context"])
}

func appendUserTranscriptMessage(entries []map[string]any, content any) []map[string]any {
	if text, ok := content.(string); ok {
		return appendTranscriptSnapshotEntry(entries, "user", formatUserTranscriptText(text))
	}
	blocks := transcriptContentBlocks(content)
	if len(blocks) == 0 {
		return entries
	}
	var textParts []string
	for _, block := range blocks {
		switch stringFromAny(block["type"]) {
		case "text":
			text := strings.TrimSpace(stringFromAny(block["text"]))
			if text != "" && !strings.Contains(text, "<system_hint>") {
				textParts = append(textParts, text)
			}
		}
	}
	if len(textParts) > 0 {
		entries = appendTranscriptSnapshotEntry(entries, "user", formatUserTranscriptText(strings.Join(textParts, "\n\n")))
	}
	return entries
}

func appendAssistantTranscriptMessage(entries []map[string]any, content any) []map[string]any {
	if text, ok := content.(string); ok {
		return appendTranscriptSnapshotEntry(entries, "assistant", formatAssistantTranscriptText(text))
	}
	for _, block := range transcriptContentBlocks(content) {
		switch stringFromAny(block["type"]) {
		case "text":
			entries = appendTranscriptSnapshotEntry(entries, "assistant", formatAssistantTranscriptText(stringFromAny(block["text"])))
		}
	}
	return entries
}

func appendTranscriptSnapshotEntry(entries []map[string]any, kind, text string) []map[string]any {
	if strings.TrimSpace(text) == "" {
		return entries
	}
	return append(entries, map[string]any{"kind": kind, "text": text})
}

func formatUserTranscriptText(text string) string {
	return strings.TrimSpace(text)
}

func formatAssistantTranscriptText(text string) string {
	return strings.TrimSpace(text)
}

func formatHistoricalToolResult(block map[string]any) string {
	content := transcriptBlockContentText(block["content"])
	if content == "" {
		content = stringFromAny(block["text"])
	}
	if content == "" {
		return "[tool result]"
	}
	return "[tool result] " + truncateString(strings.Split(strings.TrimSpace(content), "\n")[0], 200)
}

func transcriptBlockContentText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []map[string]any:
		parts := make([]string, 0, len(typed))
		for _, block := range typed {
			if stringFromAny(block["type"]) == "text" {
				parts = append(parts, stringFromAny(block["text"]))
			}
		}
		return strings.Join(nonEmptyStrings(parts), "\n")
	case []any:
		parts := make([]string, 0, len(typed))
		for _, raw := range typed {
			if block, ok := raw.(map[string]any); ok && stringFromAny(block["type"]) == "text" {
				parts = append(parts, stringFromAny(block["text"]))
			}
		}
		return strings.Join(nonEmptyStrings(parts), "\n")
	default:
		return ""
	}
}

func transcriptContentBlocks(content any) []map[string]any {
	switch typed := content.(type) {
	case []map[string]any:
		return typed
	case []any:
		blocks := make([]map[string]any, 0, len(typed))
		for _, raw := range typed {
			if block, ok := raw.(map[string]any); ok {
				blocks = append(blocks, block)
			}
		}
		return blocks
	default:
		return nil
	}
}

func permissionPromptFromEvent(event UiEvent) map[string]any {
	if skillName, command, ok := skillShellRequestParts(event.Metadata["skill_shell_request"]); ok {
		return map[string]any{"name": "skill-shell:" + skillName, "input": map[string]any{"command": command, "file_path": ""}}
	}
	if req, ok := normalizeMCPTrustRequest(event.Metadata["mcp_trust_request"]); ok {
		return map[string]any{"name": "mcp-project-trust", "input": map[string]any{"command": FormatMCPTrustTarget(req), "file_path": ""}}
	}
	return map[string]any{"name": event.Type, "input": event.Metadata}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func stringFromAny(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func skillShellCommand(value any) (string, bool) {
	_, command, ok := skillShellRequestParts(value)
	return command, ok
}

func skillShellRequestParts(value any) (string, string, bool) {
	switch req := value.(type) {
	case nil:
		return "", "", false
	case skills.SkillShellPermissionRequest:
		return req.SkillName, req.Command, true
	case *skills.SkillShellPermissionRequest:
		if req == nil {
			return "", "", false
		}
		return req.SkillName, req.Command, true
	case map[string]any:
		return stringFromAny(req["skill_name"]), stringFromAny(req["command"]), true
	case map[string]string:
		return req["skill_name"], req["command"], true
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return "", "", false
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return "", "", false
	}
	return stringFieldByNames(rv, "SkillName", "skill_name"), stringFieldByNames(rv, "Command", "command"), true
}

func normalizeMCPTrustRequest(value any) ([]map[string]any, bool) {
	switch req := value.(type) {
	case nil:
		return nil, false
	case []map[string]any:
		return req, true
	case []any:
		items := make([]map[string]any, 0, len(req))
		for _, raw := range req {
			if item, ok := mapStringAny(raw); ok {
				items = append(items, item)
			}
		}
		return items, true
	default:
		rv := reflect.ValueOf(value)
		if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
			items := make([]map[string]any, 0, rv.Len())
			for i := 0; i < rv.Len(); i++ {
				if item, ok := mapStringAny(rv.Index(i).Interface()); ok {
					items = append(items, item)
				}
			}
			return items, true
		}
	}
	return nil, false
}

func mapStringAny(value any) (map[string]any, bool) {
	if value == nil {
		return nil, false
	}
	switch item := value.(type) {
	case map[string]any:
		return item, true
	case map[string]string:
		out := make(map[string]any, len(item))
		for key, value := range item {
			out[key] = value
		}
		return out, true
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, false
	}
	out := map[string]any{}
	for _, key := range []string{"name", "command", "url", "args"} {
		if field := fieldByNames(rv, exportedFieldName(key), key); field.IsValid() && field.CanInterface() {
			out[key] = field.Interface()
		}
	}
	return out, true
}

func stringFieldByNames(value reflect.Value, names ...string) string {
	field := fieldByNames(value, names...)
	if !field.IsValid() || !field.CanInterface() {
		return ""
	}
	return stringFromAny(field.Interface())
}

func fieldByNames(value reflect.Value, names ...string) reflect.Value {
	for _, name := range names {
		if field := value.FieldByName(name); field.IsValid() {
			return field
		}
	}
	return reflect.Value{}
}

func exportedFieldName(name string) string {
	if strings.EqualFold(name, "url") {
		return "URL"
	}
	parts := strings.Split(name, "_")
	for idx, part := range parts {
		if part == "" {
			continue
		}
		parts[idx] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, "")
}

func intFromAny(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

func floatFromAny(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return 0
	}
}

func truthy(value any) bool {
	b, _ := value.(bool)
	return b
}

func truncateString(value string, limit int) string {
	runes := []rune(value)
	if limit <= 0 || len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func nonEmptyStrings(values []string) []string {
	var out []string
	for _, value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func joinAnySlice(value any) string {
	if value == nil {
		return ""
	}
	var items []any
	switch typed := value.(type) {
	case []any:
		items = typed
	case []string:
		items = make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, item)
		}
	default:
		rv := reflect.ValueOf(value)
		if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
			return ""
		}
		items = make([]any, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			items = append(items, rv.Index(i).Interface())
		}
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, fmt.Sprint(item))
	}
	return strings.Join(parts, " ")
}

func mapsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for key, av := range a {
		if fmt.Sprint(av) != fmt.Sprint(b[key]) {
			return false
		}
	}
	return true
}
