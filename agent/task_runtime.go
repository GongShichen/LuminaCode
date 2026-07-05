package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"LuminaCode/config"
	coretools "LuminaCode/tools"

	"github.com/google/uuid"
)

const (
	HardMaxWaitSeconds       = 3600
	IdleTTLSeconds           = 900
	TerminalRecordTTLSeconds = 900
)

var IdleTTLDuration = time.Duration(IdleTTLSeconds) * time.Second
var TerminalRecordTTLDuration = time.Duration(TerminalRecordTTLSeconds) * time.Second

var terminalTaskStatuses = stringSet("completed", "failed", "killed", "interrupted")
var reusableStableStatuses = stringSet("idle", "failed", "killed", "interrupted")
var nonReusableStableStatuses = stringSet("completed", "failed", "killed", "interrupted")

type AgentTaskRecord struct {
	TaskID            string  `json:"task_id"`
	ParentTaskID      string  `json:"parent_task_id"`
	ParentScopeID     string  `json:"parent_scope_id"`
	WorkerLabel       string  `json:"worker_label"`
	Description       string  `json:"description"`
	AgentType         string  `json:"agent_type"`
	Reusable          bool    `json:"reusable"`
	Status            string  `json:"status"`
	CreatedAt         float64 `json:"created_at"`
	UpdatedAt         float64 `json:"updated_at"`
	ResultText        string  `json:"result_text"`
	ErrorText         string  `json:"error_text"`
	ResultFile        string  `json:"result_file"`
	ErrorFile         string  `json:"error_file"`
	InputTokens       int     `json:"input_tokens"`
	OutputTokens      int     `json:"output_tokens"`
	ToolUseCount      int     `json:"tool_use_count"`
	DurationMS        int     `json:"duration_ms"`
	TerminationReason string  `json:"termination_reason"`
}

func (r AgentTaskRecord) ToMap() map[string]any {
	b, _ := json.Marshal(r)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	if r.ParentTaskID == "" {
		out["parent_task_id"] = nil
	}
	return out
}

type TaskNotification struct {
	NotificationID string
	TaskID         string
	ParentTaskID   string
	ParentScopeID  string
	Status         string
	Summary        string
	Result         string
	Usage          map[string]int
}

type TaskUIEvent struct {
	Type       string
	TaskID     string
	Record     map[string]any
	Summary    string
	ResultText string
}

type TaskEventSink interface {
	EmitTaskEvent(TaskUIEvent)
}

func (n TaskNotification) ToMessage() map[string]any {
	usage, _ := json.Marshal(n.Usage)
	text := fmt.Sprintf("<task-notification>\nnotification_id: %s\ntask_id: %s\nparent_task_id: %s\nparent_scope_id: %s\nstatus: %s\nsummary: %s\nresult: %s\nusage: %s\n</task-notification>",
		n.NotificationID, n.TaskID, n.ParentTaskID, n.ParentScopeID, n.Status, n.Summary, n.Result, string(usage))
	return map[string]any{
		"role":    "user",
		"content": []map[string]any{{"type": "text", "text": text}},
		"isMeta":  true,
		"metadata": map[string]any{
			"source":          "task_notification",
			"task_id":         n.TaskID,
			"notification_id": n.NotificationID,
			"parent_task_id":  nilIfEmpty(n.ParentTaskID),
			"parent_scope_id": n.ParentScopeID,
			"status":          n.Status,
		},
	}
}

type AgentTaskRuntime struct {
	mu                      sync.Mutex
	records                 map[string]*AgentTaskRecord
	notifications           map[string][]TaskNotification
	waiters                 map[string][]chan struct{}
	workers                 map[string]context.CancelFunc
	workerSpecs             map[string]workerSpec
	scopes                  map[string]struct{}
	childCounts             map[string]int
	taskParentScopes        map[string]string
	expiredTaskParentScopes map[string]string
	recordCleanupCancels    map[string]context.CancelFunc
	idleTTLCancels          map[string]context.CancelFunc
	taskEventSink           TaskEventSink
}

type workerSpec struct {
	cfg            config.Config
	registry       *coretools.ToolRegistry
	definition     AgentDef
	parentState    *AgentState
	agentType      string
	modelOverride  string
	extraContext   coretools.ExecutionContext
	sessionState   *SubAgentSessionState
	firstCompleted bool
}

func (rt *AgentTaskRuntime) SetTaskEventSink(sink TaskEventSink) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.taskEventSink = sink
}

func NewAgentTaskRuntime() *AgentTaskRuntime {
	return &AgentTaskRuntime{
		records:                 map[string]*AgentTaskRecord{},
		notifications:           map[string][]TaskNotification{},
		waiters:                 map[string][]chan struct{}{},
		workers:                 map[string]context.CancelFunc{},
		workerSpecs:             map[string]workerSpec{},
		scopes:                  map[string]struct{}{"main": {}},
		childCounts:             map[string]int{},
		taskParentScopes:        map[string]string{},
		expiredTaskParentScopes: map[string]string{},
		recordCleanupCancels:    map[string]context.CancelFunc{},
		idleTTLCancels:          map[string]context.CancelFunc{},
	}
}

func (rt *AgentTaskRuntime) ExportSnapshot() []map[string]any {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	records := make([]*AgentTaskRecord, 0, len(rt.records))
	for _, record := range rt.records {
		records = append(records, record)
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].CreatedAt == records[j].CreatedAt {
			return records[i].TaskID < records[j].TaskID
		}
		return records[i].CreatedAt < records[j].CreatedAt
	})
	out := make([]map[string]any, 0, len(records))
	for _, record := range records {
		out = append(out, record.ToMap())
	}
	return out
}

func (rt *AgentTaskRuntime) ImportSnapshot(snapshot []map[string]any) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.cancelAllRecordCleanupLocked()
	rt.cancelAllIdleTTLLocked()
	rt.records = map[string]*AgentTaskRecord{}
	rt.notifications = map[string][]TaskNotification{}
	rt.waiters = map[string][]chan struct{}{}
	rt.workers = map[string]context.CancelFunc{}
	rt.workerSpecs = map[string]workerSpec{}
	rt.scopes = map[string]struct{}{"main": {}}
	rt.childCounts = map[string]int{}
	rt.taskParentScopes = map[string]string{}
	rt.expiredTaskParentScopes = map[string]string{}
	rt.idleTTLCancels = map[string]context.CancelFunc{}
	for _, raw := range snapshot {
		record := taskRecordFromMap(raw)
		resumedLive := record.Status == "queued" || record.Status == "running" || record.Status == "idle"
		if resumedLive {
			record.Status = "interrupted"
			record.UpdatedAt = nowSeconds()
			if record.TerminationReason == "" {
				record.TerminationReason = "session_resumed"
			}
		}
		rt.registerRecordLocked(record, false)
		if resumedLive {
			rt.enqueueLocked(record, "Worker was interrupted when the session resumed.", firstNonEmptyString(record.ErrorText, record.ResultText))
		}
	}
}

func (rt *AgentTaskRuntime) EnsureScope(scopeID string) {
	if scopeID == "" {
		scopeID = "main"
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.scopes[scopeID] = struct{}{}
}

func (rt *AgentTaskRuntime) Shutdown() {
	rt.mu.Lock()
	type taskRef struct {
		id    string
		scope string
	}
	refs := make([]taskRef, 0, len(rt.records))
	for id, record := range rt.records {
		if _, terminal := terminalTaskStatuses[record.Status]; terminal {
			continue
		}
		refs = append(refs, taskRef{id: id, scope: record.ParentScopeID})
	}
	rt.mu.Unlock()
	for _, ref := range refs {
		rt.StopTask(ref.id, ref.scope)
	}
	rt.mu.Lock()
	rt.cancelAllRecordCleanupLocked()
	rt.cancelAllIdleTTLLocked()
	rt.mu.Unlock()
}

func (rt *AgentTaskRuntime) ListTasks(scopeID string) []AgentTaskRecord {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var out []AgentTaskRecord
	for _, record := range rt.records {
		if record.ParentScopeID == scopeID {
			out = append(out, *record)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt == out[j].CreatedAt {
			return out[i].TaskID < out[j].TaskID
		}
		return out[i].CreatedAt < out[j].CreatedAt
	})
	return out
}

func (rt *AgentTaskRuntime) GetTask(taskID, scopeID string) *AgentTaskRecord {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	record := rt.records[taskID]
	if record == nil || record.ParentScopeID != scopeID {
		return nil
	}
	cp := *record
	return &cp
}

func (rt *AgentTaskRuntime) RegisterForegroundTask(taskID, parentTaskID, parentScopeID, workerLabel, description, agentType string) *AgentTaskRecord {
	now := nowSeconds()
	record := &AgentTaskRecord{
		TaskID: taskID, ParentTaskID: parentTaskID, ParentScopeID: parentScopeID,
		WorkerLabel: workerLabel, Description: description, AgentType: agentType,
		Status: "running", CreatedAt: now, UpdatedAt: now,
	}
	rt.mu.Lock()
	rt.registerRecordLocked(record, false)
	rt.mu.Unlock()
	return record
}

func (rt *AgentTaskRuntime) CompleteForegroundTask(record *AgentTaskRecord, terminationReason string) {
	rt.setForegroundTaskTerminal(record, "completed", terminationReason)
}

func (rt *AgentTaskRuntime) FailForegroundTask(record *AgentTaskRecord, terminationReason string) {
	rt.setForegroundTaskTerminal(record, "failed", terminationReason)
}

func (rt *AgentTaskRuntime) DiscardForegroundTask(taskID string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.unregisterRecordLocked(taskID)
	rt.forgetTaskIdentityLocked(taskID)
}

func (rt *AgentTaskRuntime) SpawnWorker(ctx context.Context, cfg config.Config, registry *coretools.ToolRegistry, definition AgentDef, parentState *AgentState, description, prompt, agentType, modelOverride, workerLabel string, reusable bool, parentScopeID, parentTaskID string, extraContext coretools.ExecutionContext) *AgentTaskRecord {
	now := nowSeconds()
	taskID := "subagent-" + agentType + "-" + uuid.NewString()[:8]
	record := &AgentTaskRecord{
		TaskID: taskID, ParentTaskID: parentTaskID, ParentScopeID: parentScopeID,
		WorkerLabel: workerLabel, Description: description, AgentType: agentType,
		Reusable: reusable, Status: "queued", CreatedAt: now, UpdatedAt: now,
	}
	childCtx, cancel := context.WithCancel(context.Background())
	workerState := BuildSubagentState(parentState, definition.PermissionMode)
	workerExtraContext := coretools.ExecutionContext{}
	for key, value := range extraContext {
		workerExtraContext[key] = value
	}
	workerExtraContext["allowed_read_roots"] = []string{cfg.CWD}
	workerExtraContext["task_runtime"] = rt
	workerExtraContext["scope_id"] = taskID
	workerExtraContext["current_task_id"] = taskID
	workerExtraContext["parent_task_id"] = parentTaskID
	workerExtraContext["parent_scope_id"] = parentScopeID
	workerExtraContext["current_agent_type"] = agentType
	workerExtraContext["_drain_pending_notifications"] = rt.DrainPendingNotifications
	workerExtraContext["abort_check"] = func() bool { return childCtx.Err() != nil }
	rt.mu.Lock()
	rt.registerRecordLocked(record, true)
	rt.workers[taskID] = cancel
	rt.workerSpecs[taskID] = workerSpec{
		cfg: cfg, registry: registry, definition: definition, parentState: &workerState,
		agentType: agentType, modelOverride: modelOverride, extraContext: workerExtraContext,
	}
	rt.mu.Unlock()
	go rt.runWorker(childCtx, taskID, prompt)
	return record
}

func (rt *AgentTaskRuntime) SendMessage(taskID, scopeID, prompt string) any {
	rt.mu.Lock()
	record := rt.records[taskID]
	spec, ok := rt.workerSpecs[taskID]
	if record == nil || record.ParentScopeID != scopeID || !ok {
		rt.mu.Unlock()
		return "Error: Worker is not reusable or has already terminated."
	}
	if !record.Reusable {
		rt.mu.Unlock()
		return "Error: Worker is not reusable or has already terminated."
	}
	if record.Status != "idle" {
		rt.mu.Unlock()
		return "Error: Worker is currently busy processing previous request."
	}
	rt.cancelIdleTTLLocked(taskID)
	ctx, cancel := context.WithCancel(context.Background())
	rt.workers[taskID] = cancel
	record.Status = "queued"
	record.UpdatedAt = nowSeconds()
	rt.workerSpecs[taskID] = spec
	cp := *record
	rt.mu.Unlock()
	go rt.runWorker(ctx, taskID, prompt)
	return &cp
}

func (rt *AgentTaskRuntime) StopTask(taskID, scopeID string) any {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	record := rt.getTaskLocked(taskID, scopeID, scopeID != "")
	if record == nil {
		return "Error: Task not found."
	}
	if _, terminal := terminalTaskStatuses[record.Status]; terminal {
		cp := *record
		return &cp
	}
	rt.cancelIdleTTLLocked(taskID)
	summary := "Worker was stopped after its live session disappeared."
	resultText := firstNonEmptyString(record.ResultText, record.ErrorText)
	switch record.Status {
	case "queued":
		summary = "Queued worker was stopped."
		resultText = ""
	case "idle":
		summary = "Idle worker was stopped."
		resultText = record.ResultText
	case "running":
		summary = "Running worker was stopped."
		resultText = firstNonEmptyString(record.ResultText, record.ErrorText)
	}
	if cancel := rt.workers[taskID]; cancel != nil {
		cancel()
	}
	record.Status = "killed"
	record.TerminationReason = "stopped"
	record.UpdatedAt = nowSeconds()
	rt.enqueueLocked(record, summary, resultText)
	delete(rt.workerSpecs, taskID)
	rt.scheduleTerminalRecordCleanupLocked(taskID)
	cp := *record
	return &cp
}

func (rt *AgentTaskRuntime) runWorker(ctx context.Context, taskID, prompt string) {
	rt.setStatus(taskID, "running")
	rt.mu.Lock()
	spec := rt.workerSpecs[taskID]
	rt.mu.Unlock()
	start := time.Now()
	sub := NewSubAgent(spec.cfg, spec.registry, spec.definition, spec.parentState, spec.modelOverride, spec.agentType, spec.extraContext)
	sessionState := spec.sessionState
	if sessionState == nil {
		sessionState = sub.createSessionState(ctx, prompt)
	} else {
		sessionState.Messages = append(sessionState.Messages, map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": prompt}},
		})
	}
	startInputTokens := sessionState.TotalInputTokens
	startOutputTokens := sessionState.TotalOutputTokens
	startToolUseCount := sessionState.TotalToolUseCount
	result := sub.ExecuteOneRequest(ctx, prompt, sessionState)
	inputDelta := result.TotalInputTokens - startInputTokens
	outputDelta := result.TotalOutputTokens - startOutputTokens
	toolUseDelta := sessionState.TotalToolUseCount - startToolUseCount
	rt.mu.Lock()
	current := rt.records[taskID]
	if current == nil {
		rt.mu.Unlock()
		return
	}
	if current.Status == "killed" {
		extra := spec.extraContext
		reusable := current.Reusable
		delete(rt.workers, taskID)
		delete(rt.workerSpecs, taskID)
		rt.mu.Unlock()
		if wt, _ := extra["worktree_path"].(string); wt != "" && !reusable {
			_ = RemoveWorktree(context.Background(), wt)
		}
		return
	}
	current.DurationMS += int(time.Since(start).Milliseconds())
	current.InputTokens += inputDelta
	current.OutputTokens += outputDelta
	current.ToolUseCount += toolUseDelta
	reusable := current.Reusable
	extra := spec.extraContext
	spec.sessionState = sessionState
	spec.firstCompleted = true
	rt.workerSpecs[taskID] = spec
	delete(rt.workers, taskID)
	if !reusable {
		delete(rt.workerSpecs, taskID)
	}
	rt.mu.Unlock()
	if reusable {
		rt.finishReusableIdle(taskID, result.FinalText)
		return
	}
	rt.finish(taskID, "completed", result.FinalText, "", "completed")
	if wt, _ := extra["worktree_path"].(string); wt != "" {
		_ = RemoveWorktree(context.Background(), wt)
	}
}

func (rt *AgentTaskRuntime) WaitForTasks(ctx context.Context, taskIDs []string, scopeID string, timeoutSeconds int) map[string]any {
	if len(taskIDs) == 0 {
		return map[string]any{"tasks": []map[string]any{}, "timeout": false, "pending_task_ids": []string{}}
	}
	softDeadline := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	hardDeadline := time.Now().Add(HardMaxWaitSeconds * time.Second)
	for {
		rt.consumeMatchingNotifications(scopeID, taskIDs)
		snapshot := rt.snapshot(taskIDs, scopeID)
		inaccessible := rt.findInaccessibleTaskIDs(taskIDs, scopeID)
		expired := rt.findExpiredTaskIDs(taskIDs, scopeID)
		if len(inaccessible) > 0 {
			snapshot["error"] = "Error: Unknown or inaccessible task ids: " + strings.Join(inaccessible, ", ") + "."
			snapshot["inaccessible_task_ids"] = inaccessible
			snapshot["expired_task_ids"] = expired
			return snapshot
		}
		if len(expired) > 0 {
			snapshot["error"] = "Error: Task records expired or were already cleaned up: " + strings.Join(expired, ", ") + "."
			snapshot["inaccessible_task_ids"] = []string{}
			snapshot["expired_task_ids"] = expired
			return snapshot
		}
		missing := stringSliceFromAny(snapshot["missing_task_ids"])
		if len(missing) > 0 {
			snapshot["error"] = "Error: Unknown or inaccessible task ids: " + strings.Join(missing, ", ") + "."
			snapshot["inaccessible_task_ids"] = missing
			snapshot["expired_task_ids"] = []string{}
			return snapshot
		}
		allStable := len(snapshot["pending_task_ids"].([]string)) == 0
		now := time.Now()
		if allStable || !now.Before(softDeadline) || !now.Before(hardDeadline) {
			if !now.Before(hardDeadline) {
				snapshot["error"] = fmt.Sprintf("Error: TaskWait timed out waiting for workers after %d seconds.", HardMaxWaitSeconds)
			}
			snapshot["timeout"] = !now.Before(softDeadline) && !allStable
			snapshot["expired_task_ids"] = expired
			snapshot["inaccessible_task_ids"] = []string{}
			return snapshot
		}
		wait := make(chan struct{}, 1)
		rt.mu.Lock()
		for _, id := range taskIDs {
			rt.waiters[id] = append(rt.waiters[id], wait)
		}
		rt.mu.Unlock()
		maxWait := time.Until(hardDeadline)
		if time.Until(softDeadline) < maxWait {
			maxWait = time.Until(softDeadline)
		}
		if maxWait < 0 {
			maxWait = 0
		}
		timer := time.NewTimer(maxWait)
		select {
		case <-ctx.Done():
			timer.Stop()
			rt.consumeMatchingNotifications(scopeID, taskIDs)
			snapshot["timeout"] = true
			return snapshot
		case <-timer.C:
		case <-wait:
			timer.Stop()
		}
	}
}

func (rt *AgentTaskRuntime) CleanupScope(scopeID string) map[string]int {
	report := map[string]int{"active_tasks_stopped": 0, "tasks_removed": 0}
	for {
		rt.mu.Lock()
		var children []*AgentTaskRecord
		for _, record := range rt.records {
			if record.ParentScopeID == scopeID {
				children = append(children, record)
			}
		}
		rt.mu.Unlock()
		if len(children) == 0 {
			break
		}
		for _, record := range children {
			if _, terminal := terminalTaskStatuses[record.Status]; !terminal {
				report["active_tasks_stopped"]++
				rt.StopTask(record.TaskID, record.ParentScopeID)
			}
			child := rt.CleanupScope(record.TaskID)
			report["active_tasks_stopped"] += child["active_tasks_stopped"]
			report["tasks_removed"] += child["tasks_removed"]
			rt.mu.Lock()
			if cancel := rt.workers[record.TaskID]; cancel != nil {
				cancel()
			}
			delete(rt.workers, record.TaskID)
			delete(rt.workerSpecs, record.TaskID)
			removed := rt.unregisterRecordLocked(record.TaskID)
			if removed != nil {
				rt.cancelRecordCleanupLocked(record.TaskID)
				rt.forgetTaskIdentityLocked(record.TaskID)
				report["tasks_removed"]++
			}
			delete(rt.scopes, record.TaskID)
			rt.mu.Unlock()
		}
	}
	rt.mu.Lock()
	delete(rt.scopes, scopeID)
	delete(rt.notifications, scopeID)
	rt.mu.Unlock()
	return report
}

func (rt *AgentTaskRuntime) DrainPendingNotifications(scopeID string) []map[string]any {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	notifications := rt.notifications[scopeID]
	rt.notifications[scopeID] = nil
	out := make([]map[string]any, 0, len(notifications))
	for _, notification := range notifications {
		out = append(out, notification.ToMessage())
	}
	return out
}

func (rt *AgentTaskRuntime) consumeMatchingNotifications(scopeID string, taskIDs []string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.consumeMatchingNotificationsLocked(scopeID, taskIDs)
}

func (rt *AgentTaskRuntime) consumeMatchingNotificationsLocked(scopeID string, taskIDs []string) {
	if len(taskIDs) == 0 {
		return
	}
	wanted := map[string]struct{}{}
	for _, taskID := range taskIDs {
		wanted[taskID] = struct{}{}
	}
	notifications := rt.notifications[scopeID]
	if len(notifications) == 0 {
		return
	}
	kept := notifications[:0]
	for _, notification := range notifications {
		if _, ok := wanted[notification.TaskID]; ok {
			continue
		}
		kept = append(kept, notification)
	}
	if len(kept) == 0 {
		delete(rt.notifications, scopeID)
		return
	}
	rt.notifications[scopeID] = kept
}

func (rt *AgentTaskRuntime) finish(taskID, status, result, errText, reason string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	record := rt.records[taskID]
	if record == nil {
		return
	}
	record.Status = status
	record.ResultText = result
	record.ErrorText = errText
	record.TerminationReason = reason
	record.UpdatedAt = nowSeconds()
	summary := "Worker completed."
	message := result
	if errText != "" {
		summary = "Worker failed."
		message = errText
	}
	rt.enqueueLocked(record, summary, message)
	if _, terminal := terminalTaskStatuses[record.Status]; terminal {
		rt.scheduleTerminalRecordCleanupLocked(taskID)
	}
}

func (rt *AgentTaskRuntime) setForegroundTaskTerminal(record *AgentTaskRecord, status, terminationReason string) {
	if record == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	current := rt.records[record.TaskID]
	if current == nil {
		return
	}
	current.Status = status
	current.TerminationReason = terminationReason
	current.UpdatedAt = nowSeconds()
}

func (rt *AgentTaskRuntime) finishReusableIdle(taskID, result string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	record := rt.records[taskID]
	if record == nil {
		return
	}
	record.Status = "idle"
	record.ResultText = result
	record.ErrorText = ""
	record.TerminationReason = "completed"
	record.UpdatedAt = nowSeconds()
	rt.enqueueLocked(record, "Worker completed request.", result)
	rt.scheduleIdleTTLLocked(taskID)
}

func (rt *AgentTaskRuntime) setStatus(taskID, status string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if record := rt.records[taskID]; record != nil {
		record.Status = status
		record.UpdatedAt = nowSeconds()
	}
}

func (rt *AgentTaskRuntime) snapshot(taskIDs []string, scopeID string) map[string]any {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var tasks []map[string]any
	var pending []string
	var missing []string
	for _, id := range taskIDs {
		record := rt.getTaskLocked(id, scopeID, true)
		if record == nil {
			missing = append(missing, id)
			continue
		}
		tasks = append(tasks, serializeTaskPayload(record.ToMap()))
		if !isStableTask(record) {
			pending = append(pending, id)
		}
	}
	return map[string]any{
		"tasks":                 tasks,
		"timeout":               false,
		"pending_task_ids":      pending,
		"missing_task_ids":      missing,
		"expired_task_ids":      []string{},
		"inaccessible_task_ids": []string{},
	}
}

func isStableTask(record *AgentTaskRecord) bool {
	if record == nil {
		return true
	}
	statuses := nonReusableStableStatuses
	if record.Reusable {
		statuses = reusableStableStatuses
	}
	_, ok := statuses[record.Status]
	return ok
}

func (rt *AgentTaskRuntime) enqueueLocked(record *AgentTaskRecord, summary, result string) {
	notification := TaskNotification{
		NotificationID: uuid.NewString()[:12],
		TaskID:         record.TaskID, ParentTaskID: record.ParentTaskID, ParentScopeID: record.ParentScopeID,
		Status: record.Status, Summary: summary, Result: result,
		Usage: map[string]int{"input_tokens": record.InputTokens, "output_tokens": record.OutputTokens},
	}
	rt.notifications[record.ParentScopeID] = append(rt.notifications[record.ParentScopeID], notification)
	if rt.taskEventSink != nil {
		eventType := "task_snapshot_updated"
		if _, terminal := terminalTaskStatuses[record.Status]; terminal {
			eventType = "task_summary_available"
		}
		rt.taskEventSink.EmitTaskEvent(TaskUIEvent{
			Type:       eventType,
			TaskID:     record.TaskID,
			Record:     record.ToMap(),
			Summary:    summary,
			ResultText: result,
		})
	}
	for _, waiter := range rt.waiters[record.TaskID] {
		select {
		case waiter <- struct{}{}:
		default:
		}
	}
	rt.waiters[record.TaskID] = nil
}

func (rt *AgentTaskRuntime) getTaskLocked(taskID, scopeID string, forceScopeCheck bool) *AgentTaskRecord {
	record := rt.records[taskID]
	if record == nil {
		return nil
	}
	if forceScopeCheck && record.ParentScopeID != scopeID {
		return nil
	}
	return record
}

func (rt *AgentTaskRuntime) registerRecordLocked(record *AgentTaskRecord, createTaskScope bool) {
	rt.records[record.TaskID] = record
	rt.taskParentScopes[record.TaskID] = record.ParentScopeID
	rt.childCounts[record.ParentScopeID]++
	rt.scopes[record.ParentScopeID] = struct{}{}
	if createTaskScope {
		rt.scopes[record.TaskID] = struct{}{}
	}
}

func (rt *AgentTaskRuntime) scheduleIdleTTLLocked(taskID string) {
	rt.cancelIdleTTLLocked(taskID)
	record := rt.records[taskID]
	if record == nil || record.Status != "idle" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.idleTTLCancels[taskID] = cancel
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(IdleTTLDuration):
		}
		rt.mu.Lock()
		defer rt.mu.Unlock()
		delete(rt.idleTTLCancels, taskID)
		record := rt.records[taskID]
		if record == nil || record.Status != "idle" {
			return
		}
		record.Status = "completed"
		record.TerminationReason = "idle_ttl_expired"
		record.UpdatedAt = nowSeconds()
		rt.enqueueLocked(record, "Reusable worker expired after idling.", record.ResultText)
		delete(rt.workers, taskID)
		delete(rt.workerSpecs, taskID)
		rt.scheduleTerminalRecordCleanupLocked(taskID)
	}()
}

func (rt *AgentTaskRuntime) cancelIdleTTLLocked(taskID string) {
	if cancel := rt.idleTTLCancels[taskID]; cancel != nil {
		cancel()
	}
	delete(rt.idleTTLCancels, taskID)
}

func (rt *AgentTaskRuntime) cancelAllIdleTTLLocked() {
	for taskID, cancel := range rt.idleTTLCancels {
		cancel()
		delete(rt.idleTTLCancels, taskID)
	}
}

func (rt *AgentTaskRuntime) unregisterRecordLocked(taskID string) *AgentTaskRecord {
	rt.cancelIdleTTLLocked(taskID)
	record := rt.records[taskID]
	if record == nil {
		return nil
	}
	delete(rt.records, taskID)
	delete(rt.taskParentScopes, taskID)
	rt.expiredTaskParentScopes[taskID] = record.ParentScopeID
	remaining := rt.childCounts[record.ParentScopeID] - 1
	if remaining > 0 {
		rt.childCounts[record.ParentScopeID] = remaining
	} else {
		delete(rt.childCounts, record.ParentScopeID)
	}
	return record
}

func (rt *AgentTaskRuntime) forgetTaskIdentityLocked(taskID string) {
	delete(rt.taskParentScopes, taskID)
	delete(rt.expiredTaskParentScopes, taskID)
}

func (rt *AgentTaskRuntime) findInaccessibleTaskIDs(taskIDs []string, scopeID string) []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var inaccessible []string
	for _, taskID := range taskIDs {
		if _, ok := rt.records[taskID]; ok {
			continue
		}
		if parent, ok := rt.taskParentScopes[taskID]; ok && parent != scopeID {
			inaccessible = append(inaccessible, taskID)
			continue
		}
		if parent, ok := rt.expiredTaskParentScopes[taskID]; ok && parent != scopeID {
			inaccessible = append(inaccessible, taskID)
		}
	}
	return inaccessible
}

func (rt *AgentTaskRuntime) findExpiredTaskIDs(taskIDs []string, scopeID string) []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var expired []string
	for _, taskID := range taskIDs {
		if _, ok := rt.records[taskID]; ok {
			continue
		}
		if parent, ok := rt.expiredTaskParentScopes[taskID]; ok && parent == scopeID {
			expired = append(expired, taskID)
		}
	}
	return expired
}

func (rt *AgentTaskRuntime) scheduleTerminalRecordCleanupLocked(taskID string) {
	record := rt.records[taskID]
	if record == nil {
		return
	}
	if _, terminal := terminalTaskStatuses[record.Status]; !terminal {
		return
	}
	if _, exists := rt.recordCleanupCancels[taskID]; exists {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.recordCleanupCancels[taskID] = cancel
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(TerminalRecordTTLDuration):
		}
		rt.mu.Lock()
		defer rt.mu.Unlock()
		rt.discardTerminalRecordLocked(taskID)
	}()
}

func (rt *AgentTaskRuntime) discardTerminalRecordLocked(taskID string) bool {
	record := rt.records[taskID]
	if record == nil {
		delete(rt.recordCleanupCancels, taskID)
		rt.forgetTaskIdentityLocked(taskID)
		return true
	}
	if _, terminal := terminalTaskStatuses[record.Status]; !terminal {
		delete(rt.recordCleanupCancels, taskID)
		return true
	}
	if rt.childCounts[taskID] > 0 {
		return false
	}
	delete(rt.workers, taskID)
	delete(rt.workerSpecs, taskID)
	rt.cancelIdleTTLLocked(taskID)
	rt.unregisterRecordLocked(taskID)
	delete(rt.recordCleanupCancels, taskID)
	rt.forgetTaskIdentityLocked(taskID)
	if taskID != "main" {
		delete(rt.scopes, taskID)
	}
	return true
}

func (rt *AgentTaskRuntime) cancelRecordCleanupLocked(taskID string) {
	if cancel := rt.recordCleanupCancels[taskID]; cancel != nil {
		cancel()
	}
	delete(rt.recordCleanupCancels, taskID)
}

func (rt *AgentTaskRuntime) cancelAllRecordCleanupLocked() {
	for taskID, cancel := range rt.recordCleanupCancels {
		cancel()
		delete(rt.recordCleanupCancels, taskID)
	}
}

func taskRecordFromMap(data map[string]any) *AgentTaskRecord {
	b, _ := json.Marshal(data)
	record := &AgentTaskRecord{}
	_ = json.Unmarshal(b, record)
	if record.TaskID == "" {
		record.TaskID = fmt.Sprintf("task-%s", uuid.NewString()[:8])
	}
	if record.ParentScopeID == "" {
		record.ParentScopeID = "main"
	}
	if record.AgentType == "" {
		record.AgentType = "general-purpose"
	}
	if record.Status == "" {
		record.Status = "interrupted"
	}
	if record.CreatedAt == 0 {
		record.CreatedAt = nowSeconds()
	}
	if record.UpdatedAt == 0 {
		record.UpdatedAt = nowSeconds()
	}
	return record
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func stringSliceFromAny(v any) []string {
	switch values := v.(type) {
	case []string:
		return values
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if s, ok := value.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func nowSeconds() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}
