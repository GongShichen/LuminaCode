package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"LuminaCode/tools/file"
	"github.com/google/uuid"
)

// AgentPhase is the live execution state, not a durable conversation fact.
// Begin: idle/completed/failed/cancelled -> preparing.
// Activity: preparing <-> calling_model <-> executing_tools; any activity may
// enter recovering and return to preparation. Permission waits temporarily
// overlay that activity. Cancellation dominates all activities and waits;
// cancelling can finish only as cancelled. End publishes exactly one terminal.
type AgentPhase string

const (
	AgentPhaseIdle              AgentPhase = "idle"
	AgentPhasePreparing         AgentPhase = "preparing"
	AgentPhaseCallingModel      AgentPhase = "calling_model"
	AgentPhaseExecutingTools    AgentPhase = "executing_tools"
	AgentPhaseWaitingPermission AgentPhase = "waiting_permission"
	AgentPhaseRecovering        AgentPhase = "recovering"
	AgentPhaseCancelling        AgentPhase = "cancelling"
	AgentPhaseCompleted         AgentPhase = "completed"
	AgentPhaseFailed            AgentPhase = "failed"
	AgentPhaseCancelled         AgentPhase = "cancelled"
)

func (p AgentPhase) Active() bool {
	switch p {
	case AgentPhasePreparing, AgentPhaseCallingModel, AgentPhaseExecutingTools, AgentPhaseWaitingPermission, AgentPhaseRecovering, AgentPhaseCancelling:
		return true
	}
	return false
}
func (p AgentPhase) Terminal() bool {
	return p == AgentPhaseCompleted || p == AgentPhaseFailed || p == AgentPhaseCancelled
}
func activityPhase(p AgentPhase) bool {
	return p == AgentPhasePreparing || p == AgentPhaseCallingModel || p == AgentPhaseExecutingTools || p == AgentPhaseRecovering
}
func CanTransitionAgentPhase(from, to AgentPhase) bool {
	if from == to {
		return from.Active()
	}
	if !from.Active() {
		return (from == AgentPhaseIdle || from.Terminal()) && to == AgentPhasePreparing
	}
	if from == AgentPhaseCancelling {
		return to == AgentPhaseCancelled
	}
	if from == AgentPhaseWaitingPermission && to == AgentPhaseCompleted {
		return false
	}
	if to == AgentPhaseCancelling || to == AgentPhaseFailed || to == AgentPhaseCompleted {
		return true
	}
	if from == AgentPhaseWaitingPermission {
		return activityPhase(to)
	}
	return activityPhase(from) && (activityPhase(to) || to == AgentPhaseWaitingPermission)
}

var ErrInvalidAgentPhase = errors.New("invalid agent phase transition")
var ErrPermissionPending = errors.New("permission request is no longer pending")

const cancelReasonUser = "aborted by user"

type RunToken struct {
	Generation uint64 `json:"generation"`
	ID         string `json:"run_id"`
}

func (s *AgentState) RunToken() RunToken { return RunToken{Generation: s.generation, ID: s.runID} }

type PermissionKind string

const (
	PermissionTool  PermissionKind = "tool"
	PermissionSkill PermissionKind = "skill"
	PermissionMCP   PermissionKind = "mcp"
)

type PermissionWait struct {
	ID       string         `json:"request_id"`
	Kind     PermissionKind `json:"kind"`
	ToolName string         `json:"tool_name,omitempty"`
}
type PermissionAnswer struct {
	Decision string
	ToolName string
}
type permissionWaiter struct {
	PermissionWait
	ctx    context.Context
	event  StreamEvent
	answer chan PermissionAnswer
}
type PermissionTicket struct {
	Run RunToken
	PermissionWait
	answer <-chan PermissionAnswer
}

type RecoveryKind string

const (
	RecoveryAPI     RecoveryKind = "api"
	RecoveryContext RecoveryKind = "context"
	RecoveryOutput  RecoveryKind = "output"
)

type RecoveryState struct {
	Kind           RecoveryKind        `json:"kind,omitempty"`
	LastError      string              `json:"last_error,omitempty"`
	APIErrors      int                 `json:"api_errors"`
	ContextRetries int                 `json:"context_retries"`
	Continuations  int                 `json:"continuations"`
	Output         OutputRecoveryState `json:"output"`
}

type RunSnapshot struct {
	Token              RunToken         `json:"token"`
	Phase              AgentPhase       `json:"phase"`
	ResumePhase        AgentPhase       `json:"resume_phase,omitempty"`
	StartedAt          time.Time        `json:"started_at,omitempty"`
	EndedAt            time.Time        `json:"ended_at,omitempty"`
	CancelReason       string           `json:"cancel_reason,omitempty"`
	Error              string           `json:"error,omitempty"`
	PendingPermissions []PermissionWait `json:"pending_permissions,omitempty"`
	Recovery           RecoveryState    `json:"recovery"`
}

// RunState is private to AgentRuntime.mu. Channels, callbacks and cached files
// never escape via a snapshot or get serialized into conversation checkpoints.
type RunState struct {
	Generation       uint64
	ID               string
	Phase            AgentPhase
	Activity         AgentPhase
	Context          context.Context
	Cancel           context.CancelFunc
	stopCancellation func() bool
	StartedAt        time.Time
	EndedAt          time.Time
	CancelReason     string
	Error            string
	Pending          map[string]*permissionWaiter
	PermissionEvents []StreamEvent
	Recovery         RecoveryState
	Files            map[string]file.FileStateEntry
	Replacements     map[string]string
}

func idleRun(generation uint64) RunState {
	return RunState{Generation: generation, Phase: AgentPhaseIdle, Files: map[string]file.FileStateEntry{}, Replacements: map[string]string{}}
}
func (r *AgentRuntime) runMatchesLocked(token RunToken) bool {
	return token.ID != "" && token.ID == r.run.ID && token.Generation == r.run.Generation && token.Generation == r.snapshot.Generation
}
func (r *AgentRuntime) runActiveLocked(token RunToken) bool {
	return r.runMatchesLocked(token) && r.run.Phase.Active()
}
func (r *AgentRuntime) RunSnapshot() RunSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.runSnapshotLocked()
}
func (r *AgentRuntime) runSnapshotLocked() RunSnapshot {
	s := RunSnapshot{Token: RunToken{r.run.Generation, r.run.ID}, Phase: r.run.Phase, StartedAt: r.run.StartedAt, EndedAt: r.run.EndedAt, CancelReason: r.run.CancelReason, Error: r.run.Error, Recovery: r.run.Recovery}
	if s.Phase == AgentPhaseWaitingPermission {
		s.ResumePhase = r.run.Activity
	}
	for _, wait := range r.run.Pending {
		s.PendingPermissions = append(s.PendingPermissions, wait.PermissionWait)
	}
	sort.Slice(s.PendingPermissions, func(i, j int) bool { return s.PendingPermissions[i].ID < s.PendingPermissions[j].ID })
	return s
}
func (r *AgentRuntime) Transition(token RunToken, to AgentPhase) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.runActiveLocked(token) {
		return ErrStaleState
	}
	from := r.run.Activity
	if r.run.Phase == AgentPhaseCancelling {
		return context.Canceled
	}
	if !activityPhase(to) || !CanTransitionAgentPhase(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidAgentPhase, from, to)
	}
	r.run.Activity = to
	r.refreshRunPhaseLocked()
	return nil
}
func (r *AgentRuntime) refreshRunPhaseLocked() {
	if r.run.Phase == AgentPhaseCancelling || r.run.Phase.Terminal() {
		return
	}
	r.run.Phase = r.run.Activity
	if len(r.run.Pending) > 0 {
		r.run.Phase = AgentPhaseWaitingPermission
	}
}
func (r *AgentRuntime) cancelWaitersLocked() {
	for id, wait := range r.run.Pending {
		select {
		case wait.answer <- PermissionAnswer{Decision: PermissionDeny}:
		default:
		}
		delete(r.run.Pending, id)
	}
	r.run.PermissionEvents = nil
}
func (r *AgentRuntime) cancelRun(token RunToken, reason string) {
	r.mu.Lock()
	cancel := r.cancelRunLocked(token, reason)
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (r *AgentRuntime) cancelRunLocked(token RunToken, reason string) context.CancelFunc {
	if r.runMatchesLocked(token) && (r.run.Phase == AgentPhaseCancelled || r.run.Phase == AgentPhaseCancelling) && reason == cancelReasonUser {
		r.run.CancelReason = reason
	}
	if !r.runActiveLocked(token) || r.run.Phase == AgentPhaseCancelling {
		return nil
	}
	r.run.Phase = AgentPhaseCancelling
	r.run.CancelReason = reason
	r.cancelWaitersLocked()
	return r.run.Cancel
}
func (r *AgentRuntime) ObserveRunEvent(token RunToken, event StreamEvent) {
	if event.Type != "error" || event.Metadata["recovered"] == true {
		return
	}
	r.FailRun(token, event.Content)
}
func (r *AgentRuntime) FailRun(token RunToken, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runActiveLocked(token) && r.run.Phase != AgentPhaseCancelling {
		r.run.Error = message
	}
}
func (r *AgentRuntime) EndRun(token RunToken) error {
	r.mu.Lock()
	if !r.runActiveLocked(token) {
		r.mu.Unlock()
		return ErrStaleState
	}
	phase := AgentPhaseCompleted
	if r.run.Phase == AgentPhaseCancelling || (r.run.Context != nil && r.run.Context.Err() != nil) {
		phase = AgentPhaseCancelled
		if r.run.CancelReason == "" && r.run.Context != nil && r.run.Context.Err() != nil {
			r.run.CancelReason = r.run.Context.Err().Error()
		}
	} else if r.run.Error != "" {
		phase = AgentPhaseFailed
	} else if len(r.run.Pending) > 0 {
		phase = AgentPhaseFailed
		r.run.Error = "run ended with unresolved permission requests"
	}
	r.cancelWaitersLocked()
	r.run.Phase = phase
	r.run.EndedAt = time.Now().UTC()
	stop, cancel := r.run.stopCancellation, r.run.Cancel
	r.run.stopCancellation = nil
	r.run.Cancel = nil
	r.mu.Unlock()
	if stop != nil {
		stop()
	}
	if cancel != nil {
		cancel()
	}
	return nil
}

func (r *AgentRuntime) BeginPermission(ctx context.Context, token RunToken, kind PermissionKind, tool string, event StreamEvent, queued bool) (PermissionTicket, StreamEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.runActiveLocked(token) {
		return PermissionTicket{}, event, ErrStaleState
	}
	if err := ctx.Err(); err != nil {
		return PermissionTicket{}, event, err
	}
	if r.run.Phase == AgentPhaseCancelling {
		return PermissionTicket{}, event, context.Canceled
	}
	if kind != PermissionTool && kind != PermissionSkill && kind != PermissionMCP {
		return PermissionTicket{}, event, ErrPermissionPending
	}
	request := PermissionWait{ID: uuid.NewString(), Kind: kind, ToolName: tool}
	waiter := &permissionWaiter{PermissionWait: request, ctx: ctx, answer: make(chan PermissionAnswer, 1)}
	r.run.Pending[request.ID] = waiter
	r.refreshRunPhaseLocked()
	metadata := map[string]any{}
	for k, v := range event.Metadata {
		metadata[k] = v
	}
	metadata["permission_request_id"] = request.ID
	metadata["run_id"] = token.ID
	metadata["generation"] = token.Generation
	event.Metadata = metadata
	waiter.event = event
	if queued {
		r.run.PermissionEvents = append(r.run.PermissionEvents, event)
	}
	return PermissionTicket{Run: token, PermissionWait: request, answer: waiter.answer}, event, nil
}
func (r *AgentRuntime) AwaitPermission(ctx context.Context, ticket PermissionTicket) (PermissionAnswer, error) {
	defer r.cancelPermission(ticket)
	select {
	case answer := <-ticket.answer:
		r.mu.RLock()
		valid := r.runActiveLocked(ticket.Run) && r.run.Phase != AgentPhaseCancelling && (r.run.Context == nil || r.run.Context.Err() == nil)
		r.mu.RUnlock()
		if !valid {
			return PermissionAnswer{Decision: PermissionDeny}, context.Canceled
		}
		if err := ctx.Err(); err != nil {
			return PermissionAnswer{Decision: PermissionDeny}, err
		}
		return answer, nil
	case <-ctx.Done():
		return PermissionAnswer{Decision: PermissionDeny}, ctx.Err()
	}
}
func (r *AgentRuntime) cancelPermission(ticket PermissionTicket) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runMatchesLocked(ticket.Run) {
		delete(r.run.Pending, ticket.ID)
		kept := r.run.PermissionEvents[:0]
		for _, event := range r.run.PermissionEvents {
			if PermissionRequestID(event) != ticket.ID {
				kept = append(kept, event)
			}
		}
		r.run.PermissionEvents = kept
		r.refreshRunPhaseLocked()
	}
}
func (r *AgentRuntime) ResolveRunPermission(kind PermissionKind, answer PermissionAnswer, ids ...string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.run.Phase.Active() || r.run.Phase == AgentPhaseCancelling || (r.run.Context != nil && r.run.Context.Err() != nil) {
		return false
	}
	id := ""
	if len(ids) > 0 {
		id = ids[0]
	}
	var selected *permissionWaiter
	for requestID, wait := range r.run.Pending {
		if id != "" && id != requestID {
			continue
		}
		if kind != "" && kind != wait.Kind {
			continue
		}
		if id == "" && answer.ToolName != "" && wait.Kind == PermissionTool && wait.ToolName != answer.ToolName {
			continue
		}
		if selected != nil {
			return false
		}
		selected = wait
	}
	if selected == nil {
		return false
	}
	if answer.Decision == "" {
		answer.Decision = PermissionDeny
	}
	selected.answer <- answer
	delete(r.run.Pending, selected.ID)
	r.refreshRunPhaseLocked()
	return true
}
func (r *AgentRuntime) DrainPermissionEvents(token RunToken) []StreamEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.runActiveLocked(token) {
		return nil
	}
	events := r.run.PermissionEvents
	r.run.PermissionEvents = nil
	return events
}
func (r *AgentRuntime) StartRecovery(token RunToken, kind RecoveryKind, message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.runActiveLocked(token) {
		return ErrStaleState
	}
	if r.run.Phase == AgentPhaseCancelling {
		return context.Canceled
	}
	r.run.Recovery.Kind = kind
	r.run.Recovery.LastError = message
	r.run.Activity = AgentPhaseRecovering
	r.refreshRunPhaseLocked()
	return nil
}
func (r *AgentRuntime) APIErrors(token RunToken) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.runMatchesLocked(token) {
		return 0
	}
	return r.run.Recovery.APIErrors
}
func (r *AgentRuntime) SetAPIErrors(token RunToken, count int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runActiveLocked(token) {
		r.run.Recovery.APIErrors = count
	}
}
func (r *AgentRuntime) OutputRecovery(token RunToken) OutputRecoveryState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.runMatchesLocked(token) {
		return OutputRecoveryState{}
	}
	return r.run.Recovery.Output
}
func (r *AgentRuntime) ResetOutputRecovery(token RunToken, limit int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runActiveLocked(token) {
		r.run.Recovery.Output.ResetAfterHistoryReplace(limit)
	}
}
func (r *AgentRuntime) ResetOutputRetries(token RunToken) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runActiveLocked(token) {
		r.run.Recovery.Output.ResetRetries()
	}
}
func (r *AgentRuntime) HandleOutputTruncation(token RunToken, state *AgentState, turn *ModelTurn) OutputRecoveryResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.runActiveLocked(token) || r.run.Phase == AgentPhaseCancelling {
		return OutputRecoveryResult{Action: "fail"}
	}
	result := HandleOutputTruncation(state, &r.run.Recovery.Output, turn.ToolCalls, turn.ThinkingContent, turn.FullText, turn.MessageID, turn.InputTokens, turn.OutputTokens)
	if result.ShouldContinue() {
		r.run.Recovery.Kind = RecoveryOutput
		r.run.Activity = AgentPhaseRecovering
		r.refreshRunPhaseLocked()
	}
	return result
}
func (r *AgentRuntime) TryRecovery(token RunToken, kind RecoveryKind, maximum int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.runActiveLocked(token) || r.run.Phase == AgentPhaseCancelling {
		return false
	}
	count := &r.run.Recovery.ContextRetries
	if kind == RecoveryOutput {
		count = &r.run.Recovery.Continuations
	}
	if *count >= maximum {
		return false
	}
	*count++
	r.run.Recovery.Kind = kind
	r.run.Activity = AgentPhaseRecovering
	r.refreshRunPhaseLocked()
	return true
}

type runTokenContextKey struct{}

func runTokenFromContext(ctx context.Context) (RunToken, bool) {
	token, ok := ctx.Value(runTokenContextKey{}).(RunToken)
	return token, ok
}
func permissionGranted(answer PermissionAnswer) bool {
	return answer.Decision == PermissionOnce || answer.Decision == PermissionAlways || answer.Decision == "true"
}
func PermissionRequestID(event StreamEvent) string {
	return stringFromAny(event.Metadata["permission_request_id"])
}

func (r *AgentRuntime) ResetContinuationRetries(token RunToken) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runActiveLocked(token) {
		r.run.Recovery.Continuations = 0
	}
}

func (r *AgentRuntime) contextForRun(token RunToken) context.Context {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.runMatchesLocked(token) {
		return r.run.Context
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
func (r *AgentRuntime) withRun(token RunToken, fn func()) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.runActiveLocked(token) {
		return ErrStaleState
	}
	if r.run.Phase == AgentPhaseCancelling || (r.run.Context != nil && r.run.Context.Err() != nil) {
		return context.Canceled
	}
	fn()
	return nil
}

// Replies may target a child while the parent is executing its Agent tool.
// The globally unique ticket ID identifies the owning runtime, not the UI tab.
func (s *SessionRuntime) ResolvePermissionRequest(id string, answer PermissionAnswer) bool {
	if id == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, runtime := range s.agents {
		if runtime.ResolveRunPermission("", answer, id) {
			return true
		}
	}
	return false
}
func (s *SessionRuntime) ResolvePermissionKind(kind PermissionKind, answer PermissionAnswer) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var selected *AgentRuntime
	var id string
	for _, runtime := range s.agents {
		for _, pending := range runtime.RunSnapshot().PendingPermissions {
			if pending.Kind == kind {
				if selected != nil {
					return false
				}
				selected = runtime
				id = pending.ID
			}
		}
	}
	return selected != nil && selected.ResolveRunPermission(kind, answer, id)
}
