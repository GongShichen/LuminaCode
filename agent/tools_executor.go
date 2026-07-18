package agent

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"

	"LuminaCode/config"
	coretools "LuminaCode/tools"

	"golang.org/x/sync/semaphore"
)

const DefaultMaxConcurrentSafeTools = 10

type ToolState int

const (
	ToolStateQueued ToolState = iota
	ToolStateExecuting
	ToolStateCompleted
	ToolStateAborted
	ToolStateYielded
)

type ToolSlot struct {
	TC                coretools.ToolCall
	State             ToolState
	Result            map[string]any
	IsError           bool
	Truncated         string
	PermissionGranted bool
	cancel            context.CancelFunc
	abort             chan struct{}
	done              chan struct{}
}

func (s *ToolSlot) IsErrorSlot() bool { return s != nil && s.IsError }

func (s *ToolSlot) ToolName() string {
	if s == nil {
		return ""
	}
	return s.TC.Name
}

type StreamingToolExecutor struct {
	Registry *coretools.ToolRegistry
	Config   config.Config
	State    *AgentState
	Context  coretools.ExecutionContext

	mu             sync.Mutex
	slots          map[string]*ToolSlot
	toolOrder      []string
	cancelCtx      context.Context
	cancelAll      context.CancelFunc
	nonSafeRunning int
	nonSafeFailed  bool
	safeSemaphore  *semaphore.Weighted
	activity       chan struct{}
	progress       chan map[string]any
}

func NewStreamingToolExecutor(registry *coretools.ToolRegistry, cfg config.Config, state *AgentState, execCtx coretools.ExecutionContext, maxConcurrentSafe ...int) *StreamingToolExecutor {
	limit := int64(DefaultMaxConcurrentSafeTools)
	if len(maxConcurrentSafe) > 0 && maxConcurrentSafe[0] > 0 {
		limit = int64(maxConcurrentSafe[0])
	}
	ctx, cancel := context.WithCancel(context.Background())
	if execCtx == nil {
		execCtx = coretools.ExecutionContext{}
	}
	progress := make(chan map[string]any, 128)
	execCtx["progress_queue"] = progress
	return &StreamingToolExecutor{
		Registry:      registry,
		Config:        cfg,
		State:         state,
		Context:       execCtx,
		slots:         map[string]*ToolSlot{},
		cancelCtx:     ctx,
		cancelAll:     cancel,
		safeSemaphore: semaphore.NewWeighted(limit),
		activity:      make(chan struct{}, 1),
		progress:      progress,
	}
}

func (e *StreamingToolExecutor) AddTool(tc coretools.ToolCall) bool {
	e.mu.Lock()
	slot := &ToolSlot{TC: tc, State: ToolStateQueued, abort: make(chan struct{}), done: make(chan struct{})}
	e.slots[tc.ID] = slot
	e.toolOrder = append(e.toolOrder, tc.ID)
	canStart := e.canStartImmediatelyLocked(tc, false)
	e.mu.Unlock()
	if canStart {
		e.launch(tc.ID)
		return true
	}
	return false
}

func (e *StreamingToolExecutor) TryStartQueued(tcID string) bool {
	e.mu.Lock()
	slot := e.slots[tcID]
	if slot == nil || slot.State != ToolStateQueued {
		e.mu.Unlock()
		return false
	}
	slot.PermissionGranted = true
	canStart := e.canStartImmediatelyLocked(slot.TC, true)
	e.mu.Unlock()
	if !canStart {
		return false
	}
	e.launch(tcID)
	return true
}

func (e *StreamingToolExecutor) DenyTool(tcID string) {
	msg := "User denied this action."
	e.mu.Lock()
	defer e.mu.Unlock()
	slot := e.slots[tcID]
	if slot == nil {
		return
	}
	slot.State = ToolStateCompleted
	slot.Result = map[string]any{"type": "tool_result", "tool_use_id": tcID, "content": msg}
	slot.IsError = false
	e.signalActivity()
}

func (e *StreamingToolExecutor) IsQueued(tcID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	slot := e.slots[tcID]
	return slot != nil && slot.State == ToolStateQueued
}

func (e *StreamingToolExecutor) IsRunning(tcID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	slot := e.slots[tcID]
	return slot != nil && slot.State == ToolStateExecuting
}

func (e *StreamingToolExecutor) GetCompletedResults() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	var results []map[string]any
	for _, tid := range e.toolOrder {
		slot := e.slots[tid]
		if slot == nil {
			continue
		}
		if slot.State == ToolStateCompleted || slot.State == ToolStateAborted {
			slot.State = ToolStateYielded
			if slot.Result != nil {
				results = append(results, slot.Result)
			}
		}
	}
	return results
}

func (e *StreamingToolExecutor) HasPendingWork() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, slot := range e.slots {
		if slot.State == ToolStateQueued || slot.State == ToolStateExecuting {
			return true
		}
	}
	return false
}

func (e *StreamingToolExecutor) WaitForActivity(ctx context.Context) {
	if !e.HasPendingWork() {
		return
	}
	if !e.hasRunningWork() {
		e.maybeDrain()
		if !e.hasRunningWork() {
			return
		}
	}
	select {
	case <-ctx.Done():
	case <-e.activity:
	case <-e.cancelCtx.Done():
	}
}

func (e *StreamingToolExecutor) GetRemainingResults(ctx context.Context) []map[string]any {
	for e.HasPendingWork() {
		if ctx != nil && ctx.Err() != nil {
			break
		}
		if e.Cancelled() && !e.hasRunningWork() {
			break
		}
		e.WaitForActivity(ctx)
		e.maybeDrain()
		if !e.hasRunningWork() {
			break
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var results []map[string]any
	for _, tid := range e.toolOrder {
		slot := e.slots[tid]
		if slot == nil || slot.State == ToolStateYielded {
			continue
		}
		if slot.Result != nil {
			results = append(results, slot.Result)
		}
	}
	if len(results) > 1 && totalToolResultContentChars(results) > e.Config.MaxMessageToolResultsChars {
		return coretools.ApplyAggregateResultBudget(results, e.Config.MaxMessageToolResultsChars)
	}
	return results
}

func (e *StreamingToolExecutor) hasRunningWork() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, slot := range e.slots {
		if slot.State == ToolStateExecuting {
			return true
		}
	}
	return false
}

func (e *StreamingToolExecutor) HasRunningWork() bool {
	return e.hasRunningWork()
}

func (e *StreamingToolExecutor) DrainProgress() []map[string]any {
	var events []map[string]any
	for {
		select {
		case item := <-e.progress:
			events = append(events, item)
		default:
			return events
		}
	}
}

func (e *StreamingToolExecutor) AbortSiblings(failedToolID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	failed := e.slots[failedToolID]
	if failed == nil {
		return
	}
	aborted := 0
	for tid, slot := range e.slots {
		if tid == failedToolID || (slot.State != ToolStateQueued && slot.State != ToolStateExecuting) {
			continue
		}
		tool := e.Registry.Get(slot.TC.Name)
		if tool == nil || !tool.SupportsSiblingAbort() {
			continue
		}
		signalAbort(slot)
		if slot.cancel != nil {
			slot.cancel()
		}
		slot.State = ToolStateAborted
		slot.IsError = true
		slot.Result = map[string]any{
			"type":        "tool_result",
			"tool_use_id": tid,
			"content":     fmt.Sprintf("Execution aborted — sibling bash tool '%s' (%s) failed.", failed.TC.Name, failedToolID),
		}
		aborted++
	}
	if aborted > 0 {
		e.cancelAll()
		e.signalActivity()
	}
}

func (e *StreamingToolExecutor) GetSlot(tcID string) *ToolSlot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.slots[tcID]
}

func (e *StreamingToolExecutor) GetSlotInfo(toolUseID string) SlotInfo {
	return e.GetSlot(toolUseID)
}

func (e *StreamingToolExecutor) Cancelled() bool {
	select {
	case <-e.cancelCtx.Done():
		return true
	default:
		return false
	}
}

func (e *StreamingToolExecutor) maybeDrain() {
	var toLaunch []string
	e.mu.Lock()
	if e.Cancelled() {
		e.mu.Unlock()
		return
	}
	for _, tid := range e.toolOrder {
		slot := e.slots[tid]
		if slot != nil && slot.State == ToolStateQueued && e.isSafeLocked(slot.TC) && e.canStartImmediatelyLocked(slot.TC, false) {
			toLaunch = append(toLaunch, tid)
		}
	}
	for _, tid := range e.toolOrder {
		slot := e.slots[tid]
		if slot == nil || slot.State != ToolStateQueued || e.isSafeLocked(slot.TC) || !slot.PermissionGranted {
			continue
		}
		if e.canStartImmediatelyLocked(slot.TC, true) {
			toLaunch = append(toLaunch, tid)
			break
		}
	}
	e.mu.Unlock()
	for _, tid := range toLaunch {
		e.launch(tid)
	}
}

func (e *StreamingToolExecutor) canStartImmediatelyLocked(tc coretools.ToolCall, allowNonSafe bool) bool {
	if e.Cancelled() || e.nonSafeRunning > 0 {
		return false
	}
	if !e.isSafeLocked(tc) && !allowNonSafe {
		return false
	}
	return true
}

func (e *StreamingToolExecutor) isSafeLocked(tc coretools.ToolCall) bool {
	tool := e.Registry.Get(tc.Name)
	if tool == nil {
		return false
	}
	input, err := tool.DecodeInput(tc.Input)
	if err != nil {
		input = nil
	}
	return tool.IsConcurrencySafe(input)
}

func (e *StreamingToolExecutor) launch(tcID string) {
	e.mu.Lock()
	slot := e.slots[tcID]
	if slot == nil || slot.State != ToolStateQueued {
		e.mu.Unlock()
		return
	}
	isSafe := e.isSafeLocked(slot.TC)
	slot.State = ToolStateExecuting
	ctx, cancel := context.WithCancel(e.cancelCtx)
	slot.cancel = cancel
	if !isSafe {
		e.nonSafeRunning++
	}
	e.mu.Unlock()
	go e.executeOne(ctx, tcID, isSafe)
}

func (e *StreamingToolExecutor) executeOne(ctx context.Context, tcID string, isSafe bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			e.finishResult(
				tcID,
				fmt.Sprintf("<tool_use_error>\nTool execution panic: %v\n</tool_use_error>", recovered),
				fmt.Sprintf("<tool_use_error>\nTool execution panic: %v\n%s\n</tool_use_error>", recovered, string(debug.Stack())),
				true,
			)
		}
		e.mu.Lock()
		slot := e.slots[tcID]
		if slot != nil {
			if slot.State != ToolStateAborted {
				slot.State = ToolStateCompleted
			}
			close(slot.done)
		}
		if !isSafe {
			e.nonSafeRunning--
		}
		e.mu.Unlock()
		e.maybeDrain()
		e.signalActivity()
	}()

	e.mu.Lock()
	slot := e.slots[tcID]
	if slot == nil {
		e.mu.Unlock()
		return
	}
	tc := slot.TC
	abort := slot.abort
	e.mu.Unlock()

	if e.Cancelled() || abortSignalled(abort) {
		e.finishCancelled(tcID, "Execution cancelled — a prior write tool failed.")
		return
	}
	execCtx := e.executionContextForSlot(abort)

	if e.State != nil {
		e.mu.Lock()
		if cached, ok := e.State.ContentReplacements[tcID]; ok {
			e.mu.Unlock()
			e.finishResult(tcID, cached, cached, false)
			return
		}
		e.mu.Unlock()
	}

	var result coretools.ToolResult
	if isSafe {
		if err := e.safeSemaphore.Acquire(ctx, 1); err != nil {
			e.finishCancelled(tcID, "Execution cancelled before tool start.")
			return
		}
		defer e.safeSemaphore.Release(1)
		result = e.Registry.Execute(ctx, tc, execCtx)
	} else {
		result = e.Registry.Execute(ctx, tc, execCtx)
	}

	if abortSignalled(abort) {
		if e.preserveAbortResult(tcID) {
			return
		}
		e.finishCancelled(tcID, "Execution aborted — sibling tool failed.")
		return
	}

	truncated := result.Content
	if tool := e.Registry.Get(tc.Name); tool != nil && !result.IsError {
		sessionID := fmt.Sprint(e.Context["_session_id"])
		if formatted, err := tool.FormatLargeResult(ctx, result.Content, tool.MaxOutputChars(), tcID, e.Config.ToolResultsDir(sessionID)); err == nil {
			truncated = formatted
		}
	}
	if e.State != nil {
		e.mu.Lock()
		e.State.ContentReplacements[tcID] = truncated
		e.mu.Unlock()
	}
	e.finishResult(tcID, truncated, truncated, result.IsError)

	if result.IsError && !isSafe {
		e.mu.Lock()
		e.nonSafeFailed = true
		for tid, other := range e.slots {
			if tid != tcID && other.State == ToolStateExecuting {
				signalAbort(other)
				if other.cancel != nil {
					other.cancel()
				}
			}
		}
		e.mu.Unlock()
		e.cancelAll()
	}
}

func (e *StreamingToolExecutor) executionContextForSlot(abort <-chan struct{}) coretools.ExecutionContext {
	execCtx := coretools.ExecutionContext{}
	for k, v := range e.Context {
		execCtx[k] = v
	}
	execCtx["abort_event"] = abort
	return execCtx
}

func (e *StreamingToolExecutor) preserveAbortResult(tcID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	slot := e.slots[tcID]
	return slot != nil && slot.Result != nil
}

func (e *StreamingToolExecutor) finishCancelled(tcID, content string) {
	e.finishResult(tcID, content, content, true)
}

func (e *StreamingToolExecutor) finishResult(tcID, content, truncated string, isError bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	slot := e.slots[tcID]
	if slot == nil {
		return
	}
	slot.Truncated = truncated
	slot.IsError = isError
	slot.Result = map[string]any{"type": "tool_result", "tool_use_id": tcID, "content": content}
}

func (e *StreamingToolExecutor) signalActivity() {
	select {
	case e.activity <- struct{}{}:
	default:
	}
}

func signalAbort(slot *ToolSlot) {
	if slot == nil || slot.abort == nil {
		return
	}
	select {
	case <-slot.abort:
	default:
		close(slot.abort)
	}
}

func abortSignalled(abort <-chan struct{}) bool {
	if abort == nil {
		return false
	}
	select {
	case <-abort:
		return true
	default:
		return false
	}
}

func totalToolResultContentChars(results []map[string]any) int {
	total := 0
	for _, result := range results {
		if content, ok := result["content"].(string); ok {
			total += len([]rune(content))
		}
	}
	return total
}
