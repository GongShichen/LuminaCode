package harness

import (
	"encoding/json"
	"fmt"
)

type MessageAppendedPayload struct {
	Message  json.RawMessage `json:"message"`
	RunID    string          `json:"run_id,omitempty"`
	StepID   string          `json:"step_id,omitempty"`
	UserTurn int             `json:"user_turn,omitempty"`
}

type RunLifecyclePayload struct {
	RunID  string `json:"run_id"`
	Reason string `json:"reason,omitempty"`
}

type UsageRecordedPayload struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	CacheReadTokens   int `json:"cache_read_tokens,omitempty"`
	CacheCreateTokens int `json:"cache_create_tokens,omitempty"`
	ServerToolTokens  int `json:"server_tool_tokens,omitempty"`
}

type ContextCompiledPayload struct {
	RunID        string          `json:"run_id"`
	StepID       string          `json:"step_id"`
	Digest       string          `json:"digest"`
	BlobID       string          `json:"blob_id,omitempty"`
	MessageCount int             `json:"message_count"`
	ToolCount    int             `json:"tool_count"`
	Inline       json.RawMessage `json:"inline,omitempty"`
}

type ToolLifecyclePayload struct {
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name,omitempty"`
	CallIndex  int             `json:"call_index,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// RuntimeStateCheckpointedPayload is the lossless bridge used while legacy
// AgentState fields are being split into dedicated projectors. New behavior
// must still emit its typed domain event; this payload guarantees that a v2
// session never needs to fall back to the old sidecar files during migration.
type RuntimeStateCheckpointedPayload struct {
	State         json.RawMessage `json:"state"`
	SkillRecovery json.RawMessage `json:"skill_recovery,omitempty"`
	Tasks         json.RawMessage `json:"tasks,omitempty"`
}

type ToolCallView struct {
	Name      string `json:"name"`
	CallIndex int    `json:"call_index"`
	State     string `json:"state"`
}

type SessionProjection struct {
	Status            string                  `json:"status"`
	LastSeq           int64                   `json:"last_seq"`
	Messages          []json.RawMessage       `json:"messages"`
	RunStates         map[string]string       `json:"run_states"`
	ToolCalls         map[string]ToolCallView `json:"tool_calls"`
	InputTokens       int                     `json:"input_tokens"`
	OutputTokens      int                     `json:"output_tokens"`
	CacheReadTokens   int                     `json:"cache_read_tokens"`
	CacheCreateTokens int                     `json:"cache_create_tokens"`
	ServerToolTokens  int                     `json:"server_tool_tokens"`
}

func NewSessionProjection() *SessionProjection {
	projection := &SessionProjection{}
	projection.Reset()
	return projection
}

func (p *SessionProjection) Name() string { return "session" }
func (p *SessionProjection) Version() int { return 1 }

func (p *SessionProjection) Reset() {
	p.Status = "idle"
	p.LastSeq = 0
	p.Messages = []json.RawMessage{}
	p.RunStates = map[string]string{}
	p.ToolCalls = map[string]ToolCallView{}
	p.InputTokens = 0
	p.OutputTokens = 0
	p.CacheReadTokens = 0
	p.CacheCreateTokens = 0
	p.ServerToolTokens = 0
}

func (p *SessionProjection) Apply(event Event) error {
	if event.Seq <= p.LastSeq {
		return nil
	}
	switch event.Type {
	case EventSessionCreated:
		p.Status = "idle"
	case EventSessionStatusChanged:
		var payload struct {
			Status string `json:"status"`
		}
		if err := decodePayload(event, &payload); err != nil {
			return err
		}
		p.Status = payload.Status
	case EventRunStarted, EventRunCompleted, EventRunFailed, EventRunInterrupted:
		var payload RunLifecyclePayload
		if err := decodePayload(event, &payload); err != nil {
			return err
		}
		state := "running"
		switch event.Type {
		case EventRunCompleted:
			state = "completed"
		case EventRunFailed:
			state = "failed"
		case EventRunInterrupted:
			state = "interrupted"
		}
		p.RunStates[payload.RunID] = state
	case EventMessageUserAppended, EventMessageAssistantAppended, EventMessageContextAppended:
		var payload MessageAppendedPayload
		if err := decodePayload(event, &payload); err != nil {
			return err
		}
		p.Messages = append(p.Messages, append(json.RawMessage(nil), payload.Message...))
	case EventUsageRecorded:
		var payload UsageRecordedPayload
		if err := decodePayload(event, &payload); err != nil {
			return err
		}
		p.InputTokens += payload.InputTokens
		p.OutputTokens += payload.OutputTokens
		p.CacheReadTokens += payload.CacheReadTokens
		p.CacheCreateTokens += payload.CacheCreateTokens
		p.ServerToolTokens += payload.ServerToolTokens
	case EventToolCallPlanned, EventToolExecutionStarted, EventToolExecutionCompleted,
		EventToolExecutionFailed, EventToolExecutionDenied, EventToolExecutionCancelled, EventToolExecutionInterrupted:
		var payload ToolLifecyclePayload
		if err := decodePayload(event, &payload); err != nil {
			return err
		}
		view := p.ToolCalls[payload.ToolCallID]
		if payload.Name != "" {
			view.Name = payload.Name
		}
		if event.Type == EventToolCallPlanned {
			view.CallIndex = payload.CallIndex
		}
		view.State = toolStateForEvent(event.Type)
		p.ToolCalls[payload.ToolCallID] = view
	}
	p.LastSeq = event.Seq
	return nil
}

func (p *SessionProjection) MarshalState() ([]byte, error) { return json.Marshal(p) }

func (p *SessionProjection) UnmarshalState(data []byte) error {
	if err := json.Unmarshal(data, p); err != nil {
		return err
	}
	if p.RunStates == nil {
		p.RunStates = map[string]string{}
	}
	if p.ToolCalls == nil {
		p.ToolCalls = map[string]ToolCallView{}
	}
	return nil
}

func decodePayload(event Event, target any) error {
	if err := json.Unmarshal(event.Payload, target); err != nil {
		return fmt.Errorf("decode %s v%d: %w", event.Type, event.SchemaVersion, err)
	}
	return nil
}

func toolStateForEvent(eventType string) string {
	switch eventType {
	case EventToolCallPlanned:
		return "planned"
	case EventToolExecutionStarted:
		return "running"
	case EventToolExecutionCompleted:
		return "completed"
	case EventToolExecutionFailed:
		return "failed"
	case EventToolExecutionDenied:
		return "denied"
	case EventToolExecutionCancelled:
		return "cancelled"
	case EventToolExecutionInterrupted:
		return "interrupted"
	default:
		return "unknown"
	}
}

type InvariantViolation struct {
	Seq     int64  `json:"seq"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func ValidateInvariants(events []Event) []InvariantViolation {
	runs := map[string]string{}
	tools := map[string]string{}
	var violations []InvariantViolation
	for _, event := range events {
		switch event.Type {
		case EventRunStarted:
			var payload RunLifecyclePayload
			if decodePayload(event, &payload) != nil || payload.RunID == "" {
				violations = append(violations, InvariantViolation{Seq: event.Seq, Code: "invalid_run", Message: "run.started requires run_id"})
				continue
			}
			if runs[payload.RunID] != "" {
				violations = append(violations, InvariantViolation{Seq: event.Seq, Code: "duplicate_run_start", Message: payload.RunID})
			}
			runs[payload.RunID] = "running"
		case EventRunCompleted, EventRunFailed, EventRunInterrupted:
			var payload RunLifecyclePayload
			_ = decodePayload(event, &payload)
			if runs[payload.RunID] != "running" {
				violations = append(violations, InvariantViolation{Seq: event.Seq, Code: "orphan_run_terminal", Message: payload.RunID})
			} else {
				runs[payload.RunID] = "terminal"
			}
		case EventToolCallPlanned:
			var payload ToolLifecyclePayload
			_ = decodePayload(event, &payload)
			if tools[payload.ToolCallID] != "" {
				violations = append(violations, InvariantViolation{Seq: event.Seq, Code: "duplicate_tool_call", Message: payload.ToolCallID})
			}
			tools[payload.ToolCallID] = "planned"
		case EventToolExecutionCompleted, EventToolExecutionFailed, EventToolExecutionDenied,
			EventToolExecutionCancelled, EventToolExecutionInterrupted:
			var payload ToolLifecyclePayload
			_ = decodePayload(event, &payload)
			if tools[payload.ToolCallID] == "" {
				violations = append(violations, InvariantViolation{Seq: event.Seq, Code: "orphan_tool_terminal", Message: payload.ToolCallID})
			} else if tools[payload.ToolCallID] == "terminal" {
				violations = append(violations, InvariantViolation{Seq: event.Seq, Code: "duplicate_tool_terminal", Message: payload.ToolCallID})
			} else {
				tools[payload.ToolCallID] = "terminal"
			}
		}
	}
	for runID, state := range runs {
		if state == "running" {
			violations = append(violations, InvariantViolation{Code: "open_run", Message: runID})
		}
	}
	for toolID, state := range tools {
		if state != "terminal" {
			violations = append(violations, InvariantViolation{Code: "open_tool_call", Message: toolID})
		}
	}
	return violations
}
