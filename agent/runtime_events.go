package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"

	"LuminaCode/harness"
	coretools "LuminaCode/tools"

	"github.com/google/uuid"
)

type RuntimeEventRecorder struct {
	mu           sync.Mutex
	store        harness.EventStore
	streamID     string
	runID        string
	stepID       string
	stepNo       int
	terminal     bool
	publish      func([]harness.Event)
	pendingRunID string
}

func NewRuntimeEventRecorder(store harness.EventStore, sessionID string) *RuntimeEventRecorder {
	return &RuntimeEventRecorder{store: store, streamID: sessionID}
}

func (r *RuntimeEventRecorder) EventStore() harness.EventStore {
	if r == nil {
		return nil
	}
	return r.store
}

func (r *RuntimeEventRecorder) SetPublisher(publish func([]harness.Event)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.publish = publish
	r.mu.Unlock()
}

func (r *RuntimeEventRecorder) ReserveRunID(runID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.pendingRunID = runID
	r.mu.Unlock()
}

func (r *RuntimeEventRecorder) BeginRun(ctx context.Context, message map[string]any) error {
	if r == nil || r.store == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runID != "" && !r.terminal {
		return fmt.Errorf("runtime run %s is already active", r.runID)
	}
	r.runID = r.pendingRunID
	if r.runID == "" {
		r.runID = uuid.NewString()
	}
	r.pendingRunID, r.stepID, r.stepNo, r.terminal = "", "", 0, false
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	events, err := r.store.Append(ctx, harness.AnyStreamSeq,
		harness.PendingEvent{StreamID: r.streamID, Type: harness.EventMessageUserAppended, CorrelationID: r.runID,
			Audience: []harness.Audience{harness.AudienceModel, harness.AudienceUser}, Payload: harness.MessageAppendedPayload{Message: raw, RunID: r.runID}},
		harness.PendingEvent{StreamID: r.streamID, Type: harness.EventRunStarted, CorrelationID: r.runID,
			Payload: harness.RunLifecyclePayload{RunID: r.runID}},
	)
	r.publishLocked(events)
	return err
}

func (r *RuntimeEventRecorder) BeginStep(ctx context.Context) error {
	if r == nil || r.store == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runID == "" || r.terminal {
		return fmt.Errorf("cannot begin step without an active run")
	}
	pending := make([]harness.PendingEvent, 0, 2)
	if r.stepID != "" {
		pending = append(pending, harness.PendingEvent{StreamID: r.streamID,
			Type: harness.EventStepCompleted, CorrelationID: r.runID,
			Payload: map[string]any{"run_id": r.runID, "step_id": r.stepID, "step_no": r.stepNo}})
	}
	nextStepNo := r.stepNo + 1
	nextStepID := uuid.NewString()
	pending = append(pending, harness.PendingEvent{StreamID: r.streamID,
		Type: harness.EventStepStarted, CorrelationID: r.runID,
		Payload: map[string]any{"run_id": r.runID, "step_id": nextStepID, "step_no": nextStepNo}})
	events, err := r.store.Append(ctx, harness.AnyStreamSeq, pending...)
	if err == nil {
		r.stepNo = nextStepNo
		r.stepID = nextStepID
	}
	r.publishLocked(events)
	return err
}

func (r *RuntimeEventRecorder) RecordModelRequest(ctx context.Context, model string, messageCount, toolCount int) error {
	return r.append(ctx, harness.EventModelRequestPrepared, []harness.Audience{harness.AudienceInternal}, map[string]any{
		"run_id": r.runIDValue(), "step_id": r.stepIDValue(), "model": model,
		"message_count": messageCount, "tool_count": toolCount,
	})
}

func (r *RuntimeEventRecorder) RecordCompiledContext(ctx context.Context, systemPrompt string, messages []map[string]any, toolSchemas []map[string]any) error {
	if r == nil || r.store == nil {
		return nil
	}
	compiled, err := json.Marshal(struct {
		SystemPrompt string           `json:"system_prompt"`
		Messages     []map[string]any `json:"messages"`
		ToolSchemas  []map[string]any `json:"tool_schemas"`
	}{SystemPrompt: systemPrompt, Messages: messages, ToolSchemas: toolSchemas})
	if err != nil {
		return err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(compiled))
	payload := harness.ContextCompiledPayload{
		RunID: r.runIDValue(), StepID: r.stepIDValue(), Digest: digest,
		MessageCount: len(messages), ToolCount: len(toolSchemas),
	}
	if blobs, ok := r.store.(harness.BlobStore); ok {
		blobID, err := blobs.PutBlob(ctx, "application/vnd.lumina.compiled-context+json", compiled)
		if err != nil {
			return err
		}
		payload.BlobID = blobID
	} else {
		payload.Inline = compiled
	}
	return r.append(ctx, harness.EventContextCompiled, []harness.Audience{harness.AudienceModel, harness.AudienceInternal}, payload)
}

func (r *RuntimeEventRecorder) RecordAssistant(ctx context.Context, message map[string]any, usage harness.UsageRecordedPayload) error {
	if r == nil || r.store == nil {
		return nil
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	events, err := r.store.Append(ctx, harness.AnyStreamSeq,
		harness.PendingEvent{StreamID: r.streamID, Type: harness.EventModelResponseCompleted, CorrelationID: r.runID,
			Payload: map[string]any{"run_id": r.runID, "step_id": r.stepID}},
		harness.PendingEvent{StreamID: r.streamID, Type: harness.EventMessageAssistantAppended, CorrelationID: r.runID,
			Audience: []harness.Audience{harness.AudienceModel, harness.AudienceUser}, Payload: harness.MessageAppendedPayload{Message: raw, RunID: r.runID, StepID: r.stepID}},
		harness.PendingEvent{StreamID: r.streamID, Type: harness.EventUsageRecorded, CorrelationID: r.runID, Payload: usage},
	)
	r.publishLocked(events)
	return err
}

func (r *RuntimeEventRecorder) RecordToolPlanned(ctx context.Context, call coretools.ToolCall, callIndex int) error {
	input, err := json.Marshal(call.Input)
	if err != nil {
		return err
	}
	return r.append(ctx, harness.EventToolCallPlanned, []harness.Audience{harness.AudienceModel, harness.AudienceUser}, harness.ToolLifecyclePayload{
		ToolCallID: call.ID, Name: call.Name, CallIndex: callIndex, Input: input,
	})
}

func (r *RuntimeEventRecorder) RecordPermission(ctx context.Context, call coretools.ToolCall, requested bool, decision string) error {
	eventType := harness.EventToolPermissionRequested
	if !requested {
		eventType = harness.EventToolPermissionResolved
	}
	return r.append(ctx, eventType, []harness.Audience{harness.AudienceUser, harness.AudienceInternal}, map[string]any{
		"tool_call_id": call.ID, "name": call.Name, "decision": decision,
	})
}

func (r *RuntimeEventRecorder) BeforeToolStart(ctx context.Context, call coretools.ToolCall) error {
	return r.append(ctx, harness.EventToolExecutionStarted, []harness.Audience{harness.AudienceInternal}, harness.ToolLifecyclePayload{ToolCallID: call.ID, Name: call.Name})
}

func (r *RuntimeEventRecorder) ToolFinished(ctx context.Context, call coretools.ToolCall, result map[string]any, isError bool, state ToolState) error {
	eventType := harness.EventToolExecutionCompleted
	if state == ToolStateAborted {
		eventType = harness.EventToolExecutionCancelled
	} else if isError {
		eventType = harness.EventToolExecutionFailed
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return r.append(context.WithoutCancel(ctx), eventType, []harness.Audience{harness.AudienceModel, harness.AudienceUser}, harness.ToolLifecyclePayload{ToolCallID: call.ID, Name: call.Name, Result: raw})
}

func (r *RuntimeEventRecorder) ToolDenied(ctx context.Context, call coretools.ToolCall, result map[string]any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return r.append(ctx, harness.EventToolExecutionDenied, []harness.Audience{harness.AudienceModel, harness.AudienceUser}, harness.ToolLifecyclePayload{ToolCallID: call.ID, Name: call.Name, Result: raw})
}

func (r *RuntimeEventRecorder) RecordTask(ctx context.Context, event TaskUIEvent) error {
	eventType := harness.EventTaskStatusChanged
	if event.Type == "task_created" {
		eventType = harness.EventTaskCreated
	} else if event.Type == "task_summary_available" {
		eventType = harness.EventTaskNotificationAppended
	}
	return r.append(context.WithoutCancel(ctx), eventType, []harness.Audience{harness.AudienceUser, harness.AudienceInternal}, map[string]any{
		"task_id": event.TaskID, "record": event.Record, "summary": event.Summary, "result_text": event.ResultText,
	})
}

func (r *RuntimeEventRecorder) EmitTaskEvent(event TaskUIEvent) {
	_ = r.RecordTask(context.Background(), event)
}

func (r *RuntimeEventRecorder) EndRun(ctx context.Context, status, reason string) error {
	if r == nil || r.store == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runID == "" || r.terminal {
		return nil
	}
	eventType := harness.EventRunCompleted
	if status == "failed" {
		eventType = harness.EventRunFailed
	} else if status == "interrupted" {
		eventType = harness.EventRunInterrupted
	}
	pending := make([]harness.PendingEvent, 0, 2)
	if r.stepID != "" {
		stepEventType := harness.EventStepCompleted
		if status == "failed" {
			stepEventType = harness.EventStepFailed
		} else if status == "interrupted" {
			stepEventType = harness.EventStepInterrupted
		}
		pending = append(pending, harness.PendingEvent{StreamID: r.streamID, Type: stepEventType, CorrelationID: r.runID,
			Payload: map[string]any{"run_id": r.runID, "step_id": r.stepID, "step_no": r.stepNo, "reason": reason}})
	}
	pending = append(pending, harness.PendingEvent{StreamID: r.streamID,
		Type: eventType, CorrelationID: r.runID, Payload: harness.RunLifecyclePayload{RunID: r.runID, Reason: reason}})
	events, err := r.store.Append(context.WithoutCancel(ctx), harness.AnyStreamSeq, pending...)
	if err == nil {
		r.terminal = true
	}
	r.publishLocked(events)
	return err
}

func (r *RuntimeEventRecorder) AppendPending(ctx context.Context, events ...harness.PendingEvent) error {
	if r == nil || r.store == nil || len(events) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range events {
		if events[i].StreamID == "" {
			events[i].StreamID = r.streamID
		}
		if events[i].CorrelationID == "" {
			events[i].CorrelationID = r.runID
		}
	}
	committed, err := r.store.Append(ctx, harness.AnyStreamSeq, events...)
	r.publishLocked(committed)
	return err
}

func (r *RuntimeEventRecorder) append(ctx context.Context, eventType string, audience []harness.Audience, payload any) error {
	if r == nil || r.store == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	events, err := r.store.Append(ctx, harness.AnyStreamSeq, harness.PendingEvent{StreamID: r.streamID,
		Type: eventType, CorrelationID: r.runID, Audience: audience, Payload: payload})
	r.publishLocked(events)
	return err
}

func (r *RuntimeEventRecorder) publishLocked(events []harness.Event) {
	if r.publish != nil && len(events) > 0 {
		r.publish(events)
	}
}

func (r *RuntimeEventRecorder) runIDValue() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runID
}

func (r *RuntimeEventRecorder) stepIDValue() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stepID
}

type ToolEventObserver interface {
	BeforeToolStart(context.Context, coretools.ToolCall) error
	ToolFinished(context.Context, coretools.ToolCall, map[string]any, bool, ToolState) error
	ToolDenied(context.Context, coretools.ToolCall, map[string]any) error
}
