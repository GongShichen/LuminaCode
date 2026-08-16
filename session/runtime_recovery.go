package session

import (
	"context"
	"encoding/json"
	"sort"

	"LuminaCode/harness"
)

type openTool struct {
	payload       harness.ToolLifecyclePayload
	streamID      string
	correlationID string
}

type openRun struct {
	payload  harness.RunLifecyclePayload
	streamID string
}

type openStep struct {
	runID    string
	stepID   string
	stepNo   int
	streamID string
}

// RecoverInterruptedRuntime closes lifecycles that were durable before a
// process stopped but never reached a terminal event. It only appends facts;
// existing history is never rewritten.
func RecoverInterruptedRuntime(ctx context.Context, journal *RuntimeJournal) error {
	tools := map[string]openTool{}
	runs := map[string]openRun{}
	steps := map[string]openStep{}
	after := int64(0)
	for {
		events, err := journal.Load(ctx, after, 500)
		if err != nil {
			return err
		}
		for _, event := range events {
			switch event.Type {
			case harness.EventRunStarted:
				var payload harness.RunLifecyclePayload
				if json.Unmarshal(event.Payload, &payload) == nil && payload.RunID != "" {
					runs[payload.RunID] = openRun{payload: payload, streamID: event.StreamID}
				}
			case harness.EventRunCompleted, harness.EventRunFailed, harness.EventRunInterrupted:
				var payload harness.RunLifecyclePayload
				if json.Unmarshal(event.Payload, &payload) == nil {
					delete(runs, payload.RunID)
				}
			case harness.EventStepStarted:
				var payload struct {
					RunID  string `json:"run_id"`
					StepID string `json:"step_id"`
					StepNo int    `json:"step_no"`
				}
				if json.Unmarshal(event.Payload, &payload) == nil && payload.StepID != "" {
					steps[payload.StepID] = openStep{runID: payload.RunID, stepID: payload.StepID, stepNo: payload.StepNo, streamID: event.StreamID}
				}
			case harness.EventStepCompleted, harness.EventStepFailed, harness.EventStepInterrupted:
				var payload struct {
					StepID string `json:"step_id"`
				}
				if json.Unmarshal(event.Payload, &payload) == nil {
					delete(steps, payload.StepID)
				}
			case harness.EventToolCallPlanned:
				var payload harness.ToolLifecyclePayload
				if json.Unmarshal(event.Payload, &payload) == nil && payload.ToolCallID != "" {
					tools[payload.ToolCallID] = openTool{payload: payload, streamID: event.StreamID, correlationID: event.CorrelationID}
				}
			case harness.EventToolExecutionCompleted, harness.EventToolExecutionFailed, harness.EventToolExecutionDenied,
				harness.EventToolExecutionCancelled, harness.EventToolExecutionInterrupted:
				var payload harness.ToolLifecyclePayload
				if json.Unmarshal(event.Payload, &payload) == nil {
					delete(tools, payload.ToolCallID)
				}
			}
			after = event.Seq
		}
		if len(events) < 500 {
			break
		}
	}

	toolIDs := make([]string, 0, len(tools))
	for id := range tools {
		toolIDs = append(toolIDs, id)
	}
	sort.Strings(toolIDs)
	for _, id := range toolIDs {
		entry := tools[id]
		entry.payload.Error = "backend stopped before the tool produced a durable terminal result"
		if _, err := journal.Append(ctx, harness.AnyStreamSeq, harness.PendingEvent{
			StreamID: entry.streamID, Type: harness.EventToolExecutionInterrupted,
			CorrelationID: entry.correlationID, Audience: []harness.Audience{harness.AudienceModel, harness.AudienceUser}, Payload: entry.payload,
		}); err != nil {
			return err
		}
	}
	stepIDs := make([]string, 0, len(steps))
	for id := range steps {
		stepIDs = append(stepIDs, id)
	}
	sort.Strings(stepIDs)
	for _, id := range stepIDs {
		entry := steps[id]
		if _, err := journal.Append(ctx, harness.AnyStreamSeq, harness.PendingEvent{
			StreamID: entry.streamID, Type: harness.EventStepInterrupted, CorrelationID: entry.runID,
			Audience: []harness.Audience{harness.AudienceInternal},
			Payload: map[string]any{"run_id": entry.runID, "step_id": entry.stepID, "step_no": entry.stepNo,
				"reason": "backend restarted before the step reached a terminal event"},
		}); err != nil {
			return err
		}
	}
	runIDs := make([]string, 0, len(runs))
	for id := range runs {
		runIDs = append(runIDs, id)
	}
	sort.Strings(runIDs)
	for _, id := range runIDs {
		entry := runs[id]
		entry.payload.Reason = "backend restarted before the run reached a terminal event"
		if _, err := journal.Append(ctx, harness.AnyStreamSeq, harness.PendingEvent{
			StreamID: entry.streamID, Type: harness.EventRunInterrupted, CorrelationID: id,
			Audience: []harness.Audience{harness.AudienceUser, harness.AudienceInternal}, Payload: entry.payload,
		}); err != nil {
			return err
		}
	}
	return nil
}
