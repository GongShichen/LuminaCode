package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"LuminaCode/agentContext"
	"LuminaCode/config"
	"LuminaCode/harness"
	"LuminaCode/security"
	"LuminaCode/tools/file"
	"github.com/google/uuid"
)

var ErrAgentBusy = errors.New("agent is already running")
var ErrStaleState = errors.New("agent state belongs to an obsolete conversation generation")

const AgentSnapshotVersion = 3

type AgentIdentity struct {
	SessionID     string `json:"session_id"`
	AgentID       string `json:"agent_id"`
	AgentType     string `json:"agent_type"`
	TeamName      string `json:"team_name,omitempty"`
	TeamSessionID string `json:"team_session_id,omitempty"`
	TeamAgentID   string `json:"team_agent_id,omitempty"`
}

type ProgressState struct {
	TurnCount           int    `json:"turn_count"`
	UserTurnCount       int    `json:"user_turn_count"`
	LastContinueReason  string `json:"last_continue_reason,omitempty"`
	LastQuery           string `json:"last_query,omitempty"`
	TotalTaskBudget     *int   `json:"total_task_budget,omitempty"`
	TaskBudgetRemaining *int   `json:"task_budget_remaining,omitempty"`
}

type UsageState struct {
	InputTokens       int             `json:"input_tokens"`
	OutputTokens      int             `json:"output_tokens"`
	CacheReadTokens   int             `json:"cache_read_tokens"`
	CacheCreateTokens int             `json:"cache_create_tokens"`
	ServerToolTokens  int             `json:"server_tool_tokens"`
	Requests          map[string]bool `json:"requests,omitempty"`
}

type PermissionSnapshot struct {
	Policy          *security.PermissionState `json:"policy"`
	DeniedToolCalls map[string]int            `json:"denied_tool_calls"`
	ToolErrors      map[string]int            `json:"tool_errors"`
}

type MemoryState struct {
	LastExtractionTurn     int       `json:"last_extraction_turn"`
	LastExtractionUserTurn int       `json:"last_extraction_user_turn"`
	WritesSinceExtraction  bool      `json:"writes_since_extraction"`
	ExtractionSeq          int64     `json:"extraction_seq"`
	QueryText              string    `json:"query_text,omitempty"`
	QueryTime              time.Time `json:"query_time,omitempty"`
	QueryTimeExplicit      bool      `json:"query_time_explicit,omitempty"`
}

type AgentSnapshot struct {
	Version           int                          `json:"version"`
	Revision          uint64                       `json:"revision"`
	Generation        uint64                       `json:"generation"`
	Identity          AgentIdentity                `json:"identity"`
	Progress          ProgressState                `json:"progress"`
	Usage             UsageState                   `json:"usage"`
	Permissions       PermissionSnapshot           `json:"permissions"`
	Memory            MemoryState                  `json:"memory"`
	Context           agentContext.ContextSnapshot `json:"context"`
	Transcript        []agentContext.Message       `json:"transcript"`
	HistoryIncomplete bool                         `json:"history_incomplete,omitempty"`
}

func cloneSnapshot(s AgentSnapshot) AgentSnapshot {
	raw, _ := json.Marshal(s)
	var out AgentSnapshot
	_ = json.Unmarshal(raw, &out)
	return out
}

func SnapshotFromState(s *AgentState) AgentSnapshot {
	if s == nil {
		fresh := NewAgentState()
		s = &fresh
	}
	s.captureTranscript()
	ctx := agentContext.ContextSnapshot{Version: 3, Generation: 1, SystemPrompt: s.SystemPrompt, ConsecutiveFailures: s.ConsecutiveAutoCompactFailures}
	if s.ContextSnapshot != nil {
		ctx = *s.ContextSnapshot
	}
	ctx.SystemPrompt = s.SystemPrompt
	ctx.ConsecutiveFailures = s.ConsecutiveAutoCompactFailures
	ctx.Messages = nil
	attachments := map[string]agentContext.Attachment{}
	for _, a := range ctx.Attachments {
		attachments[a.ID] = a
	}
	order := []string{}
	for _, a := range ctx.Attachments {
		order = append(order, a.ID)
	}
	for i, msg := range s.Messages {
		typed := agentContext.MessageFromMap("working", i, msg)
		if transientRuntimeMessage(msg) {
			source := typed.Source
			if source == "" {
				source = "runtime"
			}
			id := source
			if source == "skill_inline" || source == "task_notification" {
				id = typed.ID
			}
			if _, ok := attachments[id]; !ok {
				order = append(order, id)
			}
			lifetime := "run"
			if source == "task_notification" {
				lifetime = "request"
			}
			attachments[id] = agentContext.Attachment{ID: id, Source: source, Lifetime: lifetime, Message: typed}
		} else {
			ctx.Messages = append(ctx.Messages, typed)
		}
	}
	ctx.Attachments = nil
	for _, id := range order {
		ctx.Attachments = append(ctx.Attachments, attachments[id])
	}
	result := AgentSnapshot{
		Version: 3, Revision: s.snapshotRevision, Generation: ctx.Generation, Context: ctx,
		Identity:    AgentIdentity{s.MemorySessionID, s.MemoryAgentID, s.MemoryAgentType, s.MemoryTeamName, s.MemoryTeamSessionID, s.MemoryTeamAgentID},
		Progress:    ProgressState{s.TurnCount, s.UserTurnCount, s.LastContinueReason, s.LastQuery, s.TotalTaskBudget, s.TaskBudgetRemaining},
		Usage:       UsageState{InputTokens: s.TotalInputTokens, OutputTokens: s.TotalOutputTokens, CacheReadTokens: s.CacheReadInputTokens, CacheCreateTokens: s.CacheCreateInputTokens, ServerToolTokens: s.ServerToolUseInputTokens, Requests: s.UsageRequests},
		Permissions: PermissionSnapshot{s.PermissionState, s.DeniedToolCalls, s.ToolErrors},
		Memory:      MemoryState{LastExtractionTurn: s.LastExtractionTurn, LastExtractionUserTurn: s.LastExtractionUserTurn, WritesSinceExtraction: s.MemoryWritesSinceExtraction, ExtractionSeq: s.MemoryExtractionSeq, QueryText: s.MemoryQueryText, QueryTime: s.MemoryQueryTime, QueryTimeExplicit: s.MemoryQueryTimeExplicit},
		Transcript:  agentContext.MessagesFromMaps("transcript", s.Transcript), HistoryIncomplete: s.HistoryIncomplete,
	}
	return cloneSnapshot(result)
}

func (s AgentSnapshot) LegacyState() AgentState {
	s = cloneSnapshot(s)
	result := NewAgentState()
	result.snapshotRevision = s.Revision
	result.Messages = agentContext.MessageMaps(s.Context.Messages)
	result.Transcript = agentContext.MessageMaps(s.Transcript)
	result.SystemPrompt = s.Context.SystemPrompt
	result.ContextSnapshot = &s.Context
	result.ConsecutiveAutoCompactFailures = s.Context.ConsecutiveFailures
	result.TurnCount = s.Progress.TurnCount
	result.UserTurnCount = s.Progress.UserTurnCount
	result.LastContinueReason = s.Progress.LastContinueReason
	result.LastQuery = s.Progress.LastQuery
	result.TotalTaskBudget = s.Progress.TotalTaskBudget
	result.TaskBudgetRemaining = s.Progress.TaskBudgetRemaining
	result.TotalInputTokens = s.Usage.InputTokens
	result.TotalOutputTokens = s.Usage.OutputTokens
	result.CacheReadInputTokens = s.Usage.CacheReadTokens
	result.CacheCreateInputTokens = s.Usage.CacheCreateTokens
	result.ServerToolUseInputTokens = s.Usage.ServerToolTokens
	result.UsageRequests = s.Usage.Requests
	result.PermissionState = s.Permissions.Policy
	if result.PermissionState == nil {
		result.PermissionState = security.DefaultPermissionState()
	}
	if s.Permissions.DeniedToolCalls != nil {
		result.DeniedToolCalls = s.Permissions.DeniedToolCalls
	}
	if s.Permissions.ToolErrors != nil {
		result.ToolErrors = s.Permissions.ToolErrors
	}
	result.LastExtractionTurn = s.Memory.LastExtractionTurn
	result.LastExtractionUserTurn = s.Memory.LastExtractionUserTurn
	result.MemoryWritesSinceExtraction = s.Memory.WritesSinceExtraction
	result.MemoryExtractionSeq = s.Memory.ExtractionSeq
	result.MemoryQueryText = s.Memory.QueryText
	result.MemoryQueryTime = s.Memory.QueryTime
	result.MemoryQueryTimeExplicit = s.Memory.QueryTimeExplicit
	result.MemorySessionID = s.Identity.SessionID
	result.MemoryAgentID = s.Identity.AgentID
	result.MemoryAgentType = s.Identity.AgentType
	result.MemoryTeamName = s.Identity.TeamName
	result.MemoryTeamSessionID = s.Identity.TeamSessionID
	result.MemoryTeamAgentID = s.Identity.TeamAgentID
	result.HistoryIncomplete = s.HistoryIncomplete
	return result
}

type RunState struct {
	Generation   uint64
	Cancel       context.CancelFunc
	Running      bool
	Files        map[string]file.FileStateEntry
	Replacements map[string]string
}

// AgentRuntime is the only publisher of an agent's state. Execution routines
// edit detached drafts; readers never observe those in-flight mutations.
type AgentRuntime struct {
	mu              sync.RWMutex
	snapshot        AgentSnapshot
	Context         *agentContext.ContextManager
	run             RunState
	store           harness.EventStore
	streamID        string
	policyRevision  uint64
	durableMessages map[string]int64
}

func NewAgentRuntime(cfg config.Config, initial *AgentState) *AgentRuntime {
	if initial == nil {
		fresh := NewAgentState()
		fresh.PermissionState.YoloMode = cfg.Yolo
		initial = &fresh
	}
	snapshot := SnapshotFromState(initial)
	r := &AgentRuntime{snapshot: snapshot, Context: agentContext.NewContextManager(cfg, nil)}
	_ = r.Context.Restore(snapshot.Context)
	r.run = RunState{Generation: snapshot.Generation, Files: map[string]file.FileStateEntry{}, Replacements: map[string]string{}}
	return r
}

func (r *AgentRuntime) Snapshot() AgentSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneSnapshot(r.snapshot)
}
func (r *AgentRuntime) BindStore(store harness.EventStore, stream string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store = store
	r.streamID = stream
	if store != nil && stream != "" {
		_ = store.CreateStream(context.Background(), harness.StreamDescriptor{ID: stream, Kind: "agent", Status: "idle"})
	}
}
func (r *AgentRuntime) Restore(s AgentSnapshot) error {
	if s.Version != 3 {
		return errors.New("unsupported agent snapshot version")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run.Running {
		return ErrAgentBusy
	}
	s.Generation = max(s.Generation, r.snapshot.Generation+1)
	s.Context.Generation = s.Generation
	r.snapshot = cloneSnapshot(s)
	r.run = RunState{Generation: s.Generation, Files: map[string]file.FileStateEntry{}, Replacements: map[string]string{}}
	return r.Context.Restore(s.Context)
}

func (r *AgentRuntime) Begin(ctx context.Context) (context.Context, *AgentState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run.Running {
		return ctx, nil, ErrAgentBusy
	}
	runCtx, cancel := context.WithCancel(ctx)
	r.run.Cancel = cancel
	r.run.Running = true
	s := r.snapshot.LegacyState()
	s.owner = r
	s.generation = r.snapshot.Generation
	s.permissionRevision = r.policyRevision
	return runCtx, &s, nil
}

func (r *AgentRuntime) End(generation uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.run.Generation {
		return
	}
	if r.run.Cancel != nil {
		r.run.Cancel()
	}
	r.run.Cancel = nil
	r.run.Running = false
}

func (r *AgentRuntime) Abort() {
	r.mu.RLock()
	cancel := r.run.Cancel
	r.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
}

func (r *AgentRuntime) persistLocked(ctx context.Context, s AgentSnapshot, extra ...harness.PendingEvent) error {
	return r.persistSnapshotLocked(ctx, s, nil, extra...)
}

func (r *AgentRuntime) persistSnapshotLocked(ctx context.Context, s AgentSnapshot, metadata *harness.RuntimeStateCheckpointedPayload, extra ...harness.PendingEvent) error {
	if r.store == nil {
		return nil
	}
	known := map[string]bool{}
	for _, message := range r.snapshot.Transcript {
		known[message.ID] = true
	}
	var added []harness.PendingEvent
	var indices []int
	for i, message := range s.Transcript {
		if sequence, ok := r.durableMessages[message.ID]; ok {
			s.Transcript[i].Seq = sequence
			s.Transcript[i].Fields["event_seq"], _ = json.Marshal(sequence)
			continue
		}
		if known[message.ID] {
			continue
		}
		raw, _ := json.Marshal(message.Map())
		kind := harness.EventMessageContextAppended
		if message.Role == "assistant" {
			kind = harness.EventMessageAssistantAppended
		} else if message.Role == "user" && !isToolResultCarrierMessage(message.Map()) {
			kind = harness.EventMessageUserAppended
		}
		added = append(added, harness.PendingEvent{StreamID: r.streamID, Type: kind, SchemaVersion: 3, Audience: []harness.Audience{harness.AudienceModel, harness.AudienceUser}, Payload: harness.MessageAppendedPayload{Message: raw}})
		indices = append(indices, i)
	}
	if len(added) > 0 {
		events, err := r.store.Append(ctx, harness.AnyStreamSeq, added...)
		if err != nil {
			return err
		}
		for i, event := range events {
			message := &s.Transcript[indices[i]]
			message.Seq = event.Seq
			if r.durableMessages == nil {
				r.durableMessages = map[string]int64{}
			}
			r.durableMessages[message.ID] = event.Seq
			message.Fields["event_seq"], _ = json.Marshal(event.Seq)
		}
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	payload := harness.RuntimeStateCheckpointedPayload{State: raw}
	if metadata != nil {
		payload.SkillRecovery = metadata.SkillRecovery
		payload.Tasks = metadata.Tasks
	}
	extra = append(extra, harness.PendingEvent{StreamID: r.streamID, Type: harness.EventRuntimeStateCheckpointed, SchemaVersion: 3, Payload: payload})
	_, err = r.store.Append(ctx, harness.AnyStreamSeq, extra...)
	return err
}

func (r *AgentRuntime) Commit(ctx context.Context, draft *AgentState) error {
	next := SnapshotFromState(draft)
	r.mu.Lock()
	defer r.mu.Unlock()
	if draft.generation != r.snapshot.Generation {
		return ErrStaleState
	}
	if draft.permissionRevision < r.policyRevision {
		next.Permissions.Policy.YoloMode = r.snapshot.Permissions.Policy.YoloMode
		draft.PermissionState.YoloMode = next.Permissions.Policy.YoloMode
		draft.permissionRevision = r.policyRevision
	}
	next.Generation = r.snapshot.Generation
	next.Revision = r.snapshot.Revision + 1
	next.Context.Generation = next.Generation
	next.Context.Revision = r.snapshot.Context.Revision + 1
	// An asynchronous memory completion can only advance the durable cursor.
	next.Memory.ExtractionSeq = max(next.Memory.ExtractionSeq, r.snapshot.Memory.ExtractionSeq)
	if err := r.persistLocked(ctx, next); err != nil {
		return err
	}
	r.snapshot = next
	draft.snapshotRevision = next.Revision
	draft.Transcript = agentContext.MessageMaps(next.Transcript)
	draft.MemoryExtractionSeq = next.Memory.ExtractionSeq
	contextCopy := next.Context.Clone()
	draft.ContextSnapshot = &contextCopy
	return r.Context.Restore(next.Context)
}

func (r *AgentRuntime) CommitContext(ctx context.Context, result agentContext.CompactionResult, draft *AgentState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if result.Generation != r.snapshot.Generation || result.BaseRevision != r.snapshot.Context.Revision {
		return agentContext.ErrStaleContext
	}
	next := cloneSnapshot(r.snapshot)
	next.Revision++
	result.Next.Revision = result.BaseRevision + 1
	next.Context = result.Next
	var events []harness.PendingEvent
	if next.Usage.Requests == nil {
		next.Usage.Requests = map[string]bool{}
	}
	for _, usage := range result.Usage {
		if usage.RequestID == "" || next.Usage.Requests[usage.RequestID] {
			continue
		}
		next.Usage.Requests[usage.RequestID] = true
		next.Usage.InputTokens += usage.InputTokens
		next.Usage.OutputTokens += usage.OutputTokens
		next.Usage.CacheReadTokens += usage.CacheReadTokens
		next.Usage.CacheCreateTokens += usage.CacheCreateTokens
		events = append(events, harness.PendingEvent{StreamID: r.streamID, Type: harness.EventUsageRecorded, Payload: harness.UsageRecordedPayload{RequestID: usage.RequestID, InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CacheReadTokens: usage.CacheReadTokens, CacheCreateTokens: usage.CacheCreateTokens}})
	}
	if result.HistoryReplaced {
		r.run.Replacements = map[string]string{}
		events = append(events, harness.PendingEvent{StreamID: r.streamID, Type: harness.EventContextReplaced, SchemaVersion: 3, Payload: result.Next})
	}
	if err := r.persistLocked(ctx, next, events...); err != nil {
		return err
	}
	if err := r.Context.Commit(result); err != nil {
		return err
	}
	r.snapshot = next
	draft.snapshotRevision = next.Revision
	draft.Messages = agentContext.MessageMaps(next.Context.Messages)
	contextCopy := next.Context.Clone()
	draft.ContextSnapshot = &contextCopy
	draft.SystemPrompt = next.Context.SystemPrompt
	draft.ConsecutiveAutoCompactFailures = next.Context.ConsecutiveFailures
	draft.TotalInputTokens = next.Usage.InputTokens
	draft.TotalOutputTokens = next.Usage.OutputTokens
	draft.CacheReadInputTokens = next.Usage.CacheReadTokens
	draft.CacheCreateInputTokens = next.Usage.CacheCreateTokens
	draft.UsageRequests = map[string]bool{}
	for id, seen := range next.Usage.Requests {
		draft.UsageRequests[id] = seen
	}
	return nil
}

func (r *AgentRuntime) Reset(ctx context.Context) error {
	r.Abort()
	r.mu.Lock()
	defer r.mu.Unlock()
	fresh := SnapshotFromState(nil)
	fresh.Generation = r.snapshot.Generation + 1
	fresh.Context.Generation = fresh.Generation
	if err := r.persistLocked(ctx, fresh, harness.PendingEvent{StreamID: r.streamID, Type: harness.EventSessionReset, SchemaVersion: 3, Payload: map[string]any{"generation": fresh.Generation}}); err != nil {
		return err
	}
	r.snapshot = fresh
	r.durableMessages = map[string]int64{}
	r.run = RunState{Generation: fresh.Generation, Files: map[string]file.FileStateEntry{}, Replacements: map[string]string{}}
	return r.Context.Restore(fresh.Context)
}

func (r *AgentRuntime) AdvanceMemory(generation uint64, seq int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.snapshot.Generation || seq <= r.snapshot.Memory.ExtractionSeq {
		return
	}
	next := cloneSnapshot(r.snapshot)
	next.Memory.ExtractionSeq = seq
	next.Revision++
	if r.persistLocked(context.Background(), next) == nil {
		r.snapshot = next
	}
}

func (r *AgentRuntime) SetReadFileState(path string, entry file.FileStateEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.run.Files[path] = entry
}
func (r *AgentRuntime) GetReadFileState(path string) (file.FileStateEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.run.Files[path]
	return v, ok
}
func (r *AgentRuntime) YoloEnabled() bool { return r.Snapshot().Permissions.Policy.YoloMode }

// SessionRuntime is a registry, not a second mutable copy of agent state.
type SessionRuntime struct {
	mu     sync.RWMutex
	agents map[string]*AgentRuntime
}

func NewSessionRuntime() *SessionRuntime { return &SessionRuntime{agents: map[string]*AgentRuntime{}} }
func (s *SessionRuntime) Register(id string, r *AgentRuntime) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agents[id] = r
}
func (s *SessionRuntime) Snapshots() map[string]AgentSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]AgentSnapshot{}
	for id, r := range s.agents {
		out[id] = r.Snapshot()
	}
	return out
}

func transientRuntimeMessage(msg map[string]any) bool {
	if meta, _ := msg["isMeta"].(bool); meta {
		return true
	}
	metadata, _ := msg["metadata"].(map[string]any)
	switch metadata["source"] {
	case "skill_inline", "skill_listing", "skill_recovery", "memory_recall", "task_notification", "post_compact_files":
		return true
	}
	return metadata["lumina_memory_context"] == true
}

func (s *AgentState) captureTranscript() {
	seen := map[string]bool{}
	for _, msg := range s.Transcript {
		seen[stringFromAny(msg["context_id"])] = true
	}
	for i, msg := range s.Messages {
		if stringFromAny(msg["context_id"]) == "" && s.owner != nil {
			msg["context_id"] = uuid.NewString()
		}
		typed := agentContext.MessageFromMap("history", i, msg)
		msg["context_id"] = typed.ID
		metadata, _ := msg["metadata"].(map[string]any)
		if transientRuntimeMessage(msg) || metadata["source"] == "compaction" {
			continue
		}
		text := visibleMessageText(msg["content"])
		if stringsContainsCompaction(text) {
			s.HistoryIncomplete = true
			continue
		}
		if !seen[typed.ID] {
			s.Transcript = append(s.Transcript, typed.Map())
			seen[typed.ID] = true
		}
	}
	var sequence int64
	for _, msg := range s.Transcript {
		if n := messageSequence(msg); n > sequence {
			sequence = n
		}
		if _, ok := msg["event_seq"]; !ok {
			sequence++
			msg["event_seq"] = sequence
		}
	}

}

func stringsContainsCompaction(text string) bool {
	return strings.Contains(text, "[Compaction handoff summary]") || strings.Contains(text, "[Earlier conversation")
}

type AgentServices struct {
	Runtime    *AgentRuntime
	Generation uint64
}

func (s AgentServices) SetReadFileState(path string, entry file.FileStateEntry) {
	r := s.Runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.Generation == r.snapshot.Generation {
		r.run.Files[path] = entry
	}
}
func (s AgentServices) GetReadFileState(path string) (file.FileStateEntry, bool) {
	r := s.Runtime
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s.Generation != r.snapshot.Generation {
		return file.FileStateEntry{}, false
	}
	v, ok := r.run.Files[path]
	return v, ok
}
func (s AgentServices) YoloEnabled() bool {
	snapshot := s.Runtime.Snapshot()
	return snapshot.Generation == s.Generation && snapshot.Permissions.Policy != nil && snapshot.Permissions.Policy.YoloMode
}
func (r *AgentRuntime) Services(generation uint64) AgentServices { return AgentServices{r, generation} }
func (r *AgentRuntime) RecentFiles() []map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	state := NewAgentState()
	state.ReadFileState = r.run.Files
	return recentReadFilesForPostCompact(&state, 5)
}
func contextSkillMessages(state *AgentState) []map[string]any {
	messages := append([]map[string]any{}, state.Messages...)
	if state.ContextSnapshot != nil {
		for _, a := range state.ContextSnapshot.Attachments {
			if a.Source == "skill_inline" {
				messages = append(messages, a.Message.Map())
			}
		}
	}
	return messages
}

func (r *AgentRuntime) SetYolo(ctx context.Context, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := cloneSnapshot(r.snapshot)
	if next.Permissions.Policy == nil {
		next.Permissions.Policy = security.DefaultPermissionState()
	}
	next.Permissions.Policy.YoloMode = enabled
	next.Revision++
	if err := r.persistLocked(ctx, next); err != nil {
		return err
	}
	r.snapshot = next
	r.policyRevision++
	return nil
}

func (r *AgentRuntime) HasDurableMessage(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.durableMessages[id]; ok {
		return true
	}
	for _, m := range r.snapshot.Transcript {
		if m.ID == id {
			return true
		}
	}
	return false
}
func (r *AgentRuntime) MarkDurableMessage(id string, seq int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.durableMessages == nil {
		r.durableMessages = map[string]int64{}
	}
	r.durableMessages[id] = seq
}

func (s *SessionRuntime) RestoreChildren(ctx context.Context, store harness.EventStore, cfg config.Config, mainStream string) error {
	latest := map[string]AgentSnapshot{}
	latestSeq := map[string]int64{}
	var allEvents []harness.Event
	births := map[string]int64{}
	lastReset := int64(0)
	after := int64(0)
	for {
		events, err := store.Load(ctx, after, 500)
		if err != nil {
			return err
		}
		for _, event := range events {
			allEvents = append(allEvents, event)
			after = event.Seq
			if births[event.StreamID] == 0 {
				births[event.StreamID] = event.Seq
			}
			if event.StreamID == mainStream && event.Type == harness.EventSessionReset {
				lastReset = event.Seq
			}
			if event.StreamID == mainStream || event.Type != harness.EventRuntimeStateCheckpointed {
				continue
			}
			var payload harness.RuntimeStateCheckpointedPayload
			if json.Unmarshal(event.Payload, &payload) != nil {
				continue
			}
			var snapshot AgentSnapshot
			if json.Unmarshal(payload.State, &snapshot) == nil && snapshot.Version == 3 {
				latest[event.StreamID] = snapshot
				latestSeq[event.StreamID] = event.Seq
			}
		}
		if len(events) < 500 {
			break
		}
	}
	for id, snapshot := range latest {
		if births[id] < lastReset {
			continue
		}
		snapshot = ReplayAgentState(snapshot, allEvents, latestSeq[id], id)
		r := NewAgentRuntime(cfg, nil)
		if err := r.Restore(snapshot); err != nil {
			return err
		}
		r.BindStore(store, id)
		s.Register(id, r)
	}
	return nil
}

func (s AgentServices) Replacement(id string) (string, bool) {
	r := s.Runtime
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s.Generation != r.snapshot.Generation {
		return "", false
	}
	value, ok := r.run.Replacements[id]
	return value, ok
}
func (s AgentServices) SetReplacement(id, value string) {
	r := s.Runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.Generation == r.snapshot.Generation {
		r.run.Replacements[id] = value
	}
}
func (s *SessionRuntime) Usage() UsageState {
	snapshots := s.Snapshots()
	total := UsageState{Requests: map[string]bool{}}
	for _, snapshot := range snapshots {
		u := snapshot.Usage
		total.InputTokens += u.InputTokens
		total.OutputTokens += u.OutputTokens
		total.CacheReadTokens += u.CacheReadTokens
		total.CacheCreateTokens += u.CacheCreateTokens
		total.ServerToolTokens += u.ServerToolTokens
		for id := range u.Requests {
			total.Requests[id] = true
		}
	}
	return total
}
func (r *AgentRuntime) RecordCompiled(ctx context.Context, compiled agentContext.CompiledContext, draft *AgentState) error {
	snapshot := r.Snapshot().Context
	snapshot.Budget = compiled.Budget
	return r.CommitContext(ctx, agentContext.CompactionResult{BaseRevision: snapshot.Revision, Generation: snapshot.Generation, Next: snapshot, Compiled: compiled}, draft)
}

func (s *SessionRuntime) Reset(main *AgentRuntime) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agents = map[string]*AgentRuntime{"main": main}
}

// Checkpoint snapshots current state under the same lock as commits and resets.
// Callers supply auxiliary recovery data, never an authoritative state DTO.
func (r *AgentRuntime) Checkpoint(ctx context.Context, recovery map[string]any, tasks []map[string]any) (AgentSnapshot, error) {
	metadata := harness.RuntimeStateCheckpointedPayload{}
	var err error
	if metadata.SkillRecovery, err = json.Marshal(recovery); err != nil {
		return AgentSnapshot{}, err
	}
	if metadata.Tasks, err = json.Marshal(tasks); err != nil {
		return AgentSnapshot{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.store == nil {
		return AgentSnapshot{}, errors.New("agent runtime has no durable store")
	}
	next := cloneSnapshot(r.snapshot)
	if err = r.persistSnapshotLocked(ctx, next, &metadata); err != nil {
		return AgentSnapshot{}, err
	}
	return cloneSnapshot(next), nil
}
func (r *AgentRuntime) HasDurableStore() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store != nil
}
