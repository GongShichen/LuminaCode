package harness

import (
	"encoding/json"
	"time"
)

type Audience string

const (
	AudienceModel      Audience = "model"
	AudienceUser       Audience = "user"
	AudienceInternal   Audience = "internal"
	AudienceDiagnostic Audience = "diagnostic"
)

type Event struct {
	Seq           int64           `json:"seq"`
	StreamSeq     int64           `json:"stream_seq"`
	ID            string          `json:"event_id"`
	SessionID     string          `json:"session_id"`
	StreamID      string          `json:"stream_id"`
	Type          string          `json:"type"`
	SchemaVersion int             `json:"schema_version"`
	OccurredAt    time.Time       `json:"occurred_at"`
	CausationID   string          `json:"causation_id,omitempty"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	Audience      []Audience      `json:"audience"`
	Payload       json.RawMessage `json:"payload"`
}

type PendingEvent struct {
	ID            string
	StreamID      string
	Type          string
	SchemaVersion int
	OccurredAt    time.Time
	CausationID   string
	CorrelationID string
	Audience      []Audience
	Payload       any
}

type StreamDescriptor struct {
	ID       string `json:"stream_id"`
	Kind     string `json:"kind"`
	ParentID string `json:"parent_stream_id,omitempty"`
	Status   string `json:"status"`
}

type Checkpoint struct {
	StreamID         string          `json:"stream_id"`
	Projector        string          `json:"projector"`
	ProjectorVersion int             `json:"projector_version"`
	UpToSeq          int64           `json:"upto_seq"`
	State            json.RawMessage `json:"state"`
	CreatedAt        time.Time       `json:"created_at"`
}

type CommandResult struct {
	CommandID   string          `json:"command_id"`
	SessionID   string          `json:"session_id"`
	AcceptedSeq int64           `json:"accepted_seq"`
	Result      json.RawMessage `json:"result"`
	CreatedAt   time.Time       `json:"created_at"`
}

const AnyStreamSeq int64 = -1

// Durable event names are centralized so runtime code cannot silently invent
// incompatible spellings across producers and projectors.
const (
	EventSessionCreated            = "session.created"
	EventSessionMetadataUpdated    = "session.metadata.updated"
	EventSessionStatusChanged      = "session.status.changed"
	EventRunStarted                = "run.started"
	EventRunCompleted              = "run.completed"
	EventRunFailed                 = "run.failed"
	EventRunInterrupted            = "run.interrupted"
	EventStepStarted               = "step.started"
	EventStepCompleted             = "step.completed"
	EventStepFailed                = "step.failed"
	EventStepInterrupted           = "step.interrupted"
	EventMessageUserAppended       = "message.user.appended"
	EventMessageAssistantAppended  = "message.assistant.appended"
	EventMessageContextAppended    = "message.context.appended"
	EventContextReplaced           = "context.replaced"
	EventContextCompiled           = "context.compiled"
	EventModelRequestPrepared      = "model.request.prepared"
	EventModelResponseCompleted    = "model.response.completed"
	EventModelRequestFailed        = "model.request.failed"
	EventModelFallbackSelected     = "model.fallback.selected"
	EventUsageRecorded             = "usage.recorded"
	EventToolCallPlanned           = "tool.call.planned"
	EventToolPolicyEvaluated       = "tool.policy.evaluated"
	EventToolPermissionRequested   = "tool.permission.requested"
	EventToolPermissionResolved    = "tool.permission.resolved"
	EventToolExecutionStarted      = "tool.execution.started"
	EventToolExecutionCompleted    = "tool.execution.completed"
	EventToolExecutionFailed       = "tool.execution.failed"
	EventToolExecutionDenied       = "tool.execution.denied"
	EventToolExecutionCancelled    = "tool.execution.cancelled"
	EventToolExecutionInterrupted  = "tool.execution.interrupted"
	EventSkillInvoked              = "skill.invoked"
	EventSkillContextAttached      = "skill.context.attached"
	EventSkillRecoveryUpdated      = "skill.recovery.updated"
	EventTaskCreated               = "task.created"
	EventTaskStatusChanged         = "task.status.changed"
	EventTaskNotificationAppended  = "task.notification.appended"
	EventTeamCreated               = "team.created"
	EventTeamStatusChanged         = "team.status.changed"
	EventTeamDialogueAppended      = "team.dialogue.appended"
	EventTeamSnapshotUpdated       = "team.snapshot.updated"
	EventTeamLoopIteration         = "team.loop.iteration"
	EventTeamLoopRecovery          = "team.loop.recovery"
	EventTeamAgentMessage          = "team.agent.message"
	EventTeamAgentStatusChanged    = "team.agent.status.changed"
	EventTeamContractUpdated       = "team.contract.updated"
	EventTeamGateUpdated           = "team.gate.updated"
	EventTeamArtifactRegistered    = "team.artifact.registered"
	EventTeamRuntimeCheckpointed   = "team.runtime.checkpointed"
	EventMemoryContextInjected     = "memory.context.injected"
	EventMemoryExtractionScheduled = "memory.extraction.scheduled"
	EventMemoryExtractionCompleted = "memory.extraction.completed"
	EventRuntimeWarning            = "runtime.warning"
	EventRuntimeInvariantFailed    = "runtime.invariant.failed"
	EventRuntimeStateCheckpointed  = "runtime.state.checkpointed"
	EventLegacySnapshotImported    = "legacy.snapshot.imported"
)
