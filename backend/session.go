package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"LuminaCode/agent"
	luminacli "LuminaCode/cli"
	"LuminaCode/config"
	"LuminaCode/harness"
	"LuminaCode/maintenance"
	"LuminaCode/security"
	"LuminaCode/session"
	luminateam "LuminaCode/team"
	luminaui "LuminaCode/ui"

	"github.com/google/uuid"
)

type SessionManager struct {
	baseConfig config.Config
	store      *session.Store
	factory    *SessionRuntimeFactory

	mu       sync.Mutex
	sessions map[string]*SessionController
}

func NewSessionManager(cfg config.Config, store *session.Store, factory *SessionRuntimeFactory) *SessionManager {
	manager := &SessionManager{
		baseConfig: cfg,
		store:      store,
		factory:    factory,
		sessions:   map[string]*SessionController{},
	}
	// Upgrade every discoverable legacy session eagerly. Failures are isolated
	// per session so a damaged archive cannot prevent the daemon from starting.
	for _, meta := range manager.store.ListSessions() {
		loaded, err := manager.store.OpenRuntime(context.Background(), meta.SessionID)
		if err != nil {
			slog.Warn("runtime journal migration failed", "session_id", meta.SessionID, "error", err)
			continue
		}
		if err := loaded.Journal.Close(); err != nil {
			slog.Warn("runtime journal close after migration failed", "session_id", meta.SessionID, "error", err)
		}
	}
	return manager
}

func (m *SessionManager) Create(cwd string) (*SessionController, error) {
	id := uuid.NewString()
	return m.createWithState(id, cwd, nil)
}

func (m *SessionManager) Resume(sessionID, cwd string) (*SessionController, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("session_id is required")
	}
	m.mu.Lock()
	if existing := m.sessions[sessionID]; existing != nil {
		m.mu.Unlock()
		return existing, nil
	}
	m.mu.Unlock()
	if _, err := os.Stat(session.RuntimeJournalPath(m.baseConfig.SessionDir, sessionID)); errors.Is(err, os.ErrNotExist) {
		if m.store.LoadState(sessionID) == nil && len(m.store.Load(sessionID)) == 0 {
			return nil, fmt.Errorf("session %s not found", sessionID)
		}
	}
	return m.createWithState(sessionID, cwd, nil)
}

func (m *SessionManager) Get(sessionID string) (*SessionController, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[sessionID]
	if session == nil {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}
	return session, nil
}

func (m *SessionManager) List() []session.Meta {
	return m.store.ListSessions()
}

func (m *SessionManager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

func (m *SessionManager) ActiveSessionIDs() map[string]struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]struct{}{}
	for id := range m.sessions {
		out[id] = struct{}{}
	}
	return out
}

func (m *SessionManager) StorageStatus() (maintenance.Report, error) {
	return maintenance.Status(m.baseConfig, maintenance.Options{CurrentSessions: m.ActiveSessionIDs()})
}

func (m *SessionManager) CleanupStorage(enforce bool) (maintenance.Report, error) {
	return maintenance.Cleanup(m.baseConfig, maintenance.Options{Enforce: enforce, CurrentSessions: m.ActiveSessionIDs()})
}

func (m *SessionManager) Pin(sessionID string, pinned bool) (*session.Meta, error) {
	return m.store.Pin(sessionID, pinned)
}

func (m *SessionManager) Shutdown() {
	m.mu.Lock()
	sessions := make([]*SessionController, 0, len(m.sessions))
	for _, controller := range m.sessions {
		sessions = append(sessions, controller)
	}
	m.mu.Unlock()
	for _, controller := range sessions {
		controller.Shutdown()
	}
}

func (m *SessionManager) createWithState(sessionID, cwd string, state *agent.AgentState) (*SessionController, error) {
	controller, err := m.factory.Create(context.Background(), sessionID, cwd, state)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.sessions[sessionID] = controller
	m.mu.Unlock()
	return controller, nil
}

func applyPinnedDaemonConfig(target *config.Config, source config.Config) {
	if target == nil || len(source.PinnedFields) == 0 {
		return
	}
	target.PinnedFields = map[string]bool{}
	for field, pinned := range source.PinnedFields {
		if !pinned {
			continue
		}
		target.PinnedFields[field] = true
		switch field {
		case "api_key":
			target.APIKey = source.APIKey
		case "api_base_url":
			target.APIBaseURL = source.APIBaseURL
		case "api_model":
			target.APIModel = source.APIModel
		case "api_type":
			target.APIType = source.APIType
		case "api_max_tokens":
			target.APIMaxTokens = source.APIMaxTokens
		case "harness_mode":
			target.HarnessMode = source.HarnessMode
		}
	}
}

type SessionController struct {
	id      string
	cfg     config.Config
	engine  *agent.QueryEngine
	store   *session.Store
	journal *session.RuntimeJournal
	ui      *luminaui.UiRuntime
	bridge  *WSRendererBridge

	stateMu sync.Mutex
	state   *agent.AgentState

	commandMu    sync.Mutex
	submitMu     sync.Mutex
	submitCancel context.CancelFunc

	busy atomic.Bool
	seq  atomic.Int64
}

func NewSessionController(sessionID string, cfg config.Config, engine *agent.QueryEngine, state *agent.AgentState, store *session.Store, journal *session.RuntimeJournal, emit EventEmitter) *SessionController {
	controller := &SessionController{
		id:      sessionID,
		cfg:     cfg,
		engine:  engine,
		store:   store,
		journal: journal,
		state:   state,
	}
	controller.bridge = NewWSRendererBridge(sessionID, emit, controller.nextSeq)
	controller.ui = luminaui.NewUiRuntime(engine, controller.bridge)
	if engine != nil && engine.CoreEngine != nil {
		engine.CoreEngine.StateObserver = controller.observeState
		engine.CoreEngine.RuntimeEvents = agent.NewRuntimeEventRecorder(journal, sessionID)
		engine.CoreEngine.RuntimeEvents.SetPublisher(controller.bridge.emitRuntimeEvents)
		engine.CoreEngine.TaskRuntime.SetTaskEventObserver(engine.CoreEngine.RuntimeEvents)
	}
	return controller
}

func (c *SessionController) ID() string {
	return c.id
}

func (c *SessionController) RuntimeConfig() config.Config {
	c.stateMu.Lock()
	state := c.state
	c.stateMu.Unlock()
	cfg := c.cfg
	if state != nil && state.YoloEnabled() {
		cfg.Yolo = true
	}
	return cfg
}

func (c *SessionController) Mount() {
	c.stateMu.Lock()
	state := c.state
	c.stateMu.Unlock()
	c.ui.MountStateSnapshot(state)
}

func (c *SessionController) Snapshot() SessionSnapshot {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	lastSeq := int64(0)
	if c.journal != nil {
		lastSeq, _ = c.journal.Head(context.Background())
	}
	return SessionSnapshot{
		SessionID:         c.id,
		Frame:             c.ui.Frame,
		Busy:              c.busy.Load(),
		Model:             c.cfg.APIModel,
		CWD:               c.cfg.CWD,
		LastSeq:           lastSeq,
		ProjectionVersion: 1,
	}
}

func (c *SessionController) Events(ctx context.Context, afterSeq int64, limit int) (EventPage, error) {
	if c.journal == nil {
		return EventPage{}, errors.New("runtime journal is unavailable")
	}
	events, err := c.journal.Load(ctx, afterSeq, limit)
	if err != nil {
		return EventPage{}, err
	}
	next := afterSeq
	if len(events) > 0 {
		next = events[len(events)-1].Seq
	}
	head, err := c.journal.Head(ctx)
	if err != nil {
		return EventPage{}, err
	}
	return EventPage{Events: events, NextAfterSeq: next, HasMore: next < head}, nil
}

func (c *SessionController) RuntimeDescription() map[string]any {
	if c.engine == nil || c.engine.CoreEngine == nil || c.engine.CoreEngine.Runtime == nil {
		return map[string]any{"available": false, "session_id": c.id}
	}
	description := c.engine.CoreEngine.Runtime.Describe()
	description["session_id"] = c.id
	return description
}

func (c *SessionController) AppendTeamEvent(ctx context.Context, teamSessionID, eventType string, payload any) error {
	if c.journal == nil || strings.TrimSpace(teamSessionID) == "" {
		return errors.New("team runtime journal is unavailable")
	}
	if err := c.journal.CreateStream(ctx, harness.StreamDescriptor{
		ID: teamSessionID, Kind: string(harness.ScopeTeam), ParentID: c.id, Status: "active",
	}); err != nil {
		return err
	}
	events, err := c.journal.Append(ctx, harness.AnyStreamSeq, harness.PendingEvent{
		StreamID: teamSessionID, Type: eventType,
		Audience: []harness.Audience{harness.AudienceUser, harness.AudienceInternal}, Payload: payload,
	})
	if err != nil {
		return err
	}
	c.bridge.emitRuntimeEvents(events)
	return nil
}

// TeamRuntimeCheckpoints returns the latest lossless checkpoint for every Team
// child stream. It deliberately ignores UI snapshot events.
func (c *SessionController) TeamRuntimeCheckpoints(ctx context.Context) ([]luminateam.RuntimeCheckpoint, error) {
	if c.journal == nil {
		return nil, errors.New("team runtime journal is unavailable")
	}
	latest := map[string]luminateam.RuntimeCheckpoint{}
	afterSeq := int64(0)
	for {
		events, err := c.journal.Load(ctx, afterSeq, 500)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			if event.Type != harness.EventTeamRuntimeCheckpointed {
				continue
			}
			var checkpoint luminateam.RuntimeCheckpoint
			if err := json.Unmarshal(event.Payload, &checkpoint); err != nil {
				return nil, fmt.Errorf("decode team checkpoint at seq %d: %w", event.Seq, err)
			}
			latest[event.StreamID] = checkpoint
		}
		if len(events) == 0 {
			break
		}
		afterSeq = events[len(events)-1].Seq
		if len(events) < 500 {
			break
		}
	}
	checkpoints := make([]luminateam.RuntimeCheckpoint, 0, len(latest))
	for _, checkpoint := range latest {
		checkpoints = append(checkpoints, checkpoint)
	}
	sort.SliceStable(checkpoints, func(i, j int) bool {
		return checkpoints[i].Snapshot.TeamSessionID < checkpoints[j].Snapshot.TeamSessionID
	})
	return checkpoints, nil
}

func (c *SessionController) Submit(ctx context.Context, input string) error {
	_, err := c.submit(ctx, input, nil)
	return err
}

func (c *SessionController) SubmitCommand(ctx context.Context, commandID, input string) (map[string]any, error) {
	c.commandMu.Lock()
	defer c.commandMu.Unlock()
	if strings.TrimSpace(commandID) == "" {
		commandID = uuid.NewString()
	}
	if c.journal == nil {
		return nil, errors.New("runtime journal is unavailable")
	}
	if existing, err := c.journal.GetCommandResult(ctx, commandID); err != nil {
		return nil, err
	} else if existing != nil {
		var result map[string]any
		if err := json.Unmarshal(existing.Result, &result); err != nil {
			return nil, err
		}
		result["duplicate"] = true
		return result, nil
	}
	runID := uuid.NewString()
	if c.engine != nil && c.engine.CoreEngine != nil && c.engine.CoreEngine.RuntimeEvents != nil {
		c.engine.CoreEngine.RuntimeEvents.ReserveRunID(runID)
	}
	result := map[string]any{"accepted": true, "command_id": commandID, "run_id": runID}
	_, err := c.submit(ctx, input, func() error {
		head, err := c.journal.Head(ctx)
		if err != nil {
			return err
		}
		result["accepted_seq"] = head
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return c.journal.SaveCommandResult(ctx, harness.CommandResult{CommandID: commandID, SessionID: c.id, AcceptedSeq: head, Result: raw})
	})
	return result, err
}

func (c *SessionController) submit(ctx context.Context, input string, beforeStart func() error) (map[string]any, error) {
	if strings.TrimSpace(input) == "" {
		return nil, errors.New("empty input")
	}
	if !c.busy.CompareAndSwap(false, true) {
		return nil, errors.New("session_busy")
	}
	if beforeStart != nil {
		if err := beforeStart(); err != nil {
			c.busy.Store(false)
			return nil, err
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	c.submitMu.Lock()
	c.submitCancel = cancel
	c.submitMu.Unlock()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				c.emitStatus("error", map[string]any{"error": fmt.Sprintf("session panic: %v", recovered)})
			}
			cancel()
			c.submitMu.Lock()
			c.submitCancel = nil
			c.submitMu.Unlock()
			c.busy.Store(false)
		}()
		c.stateMu.Lock()
		state := c.state
		c.stateMu.Unlock()
		c.emitStatus("running", nil)
		c.ui.RunSubmitMessage(runCtx, input, state, c.id)
		if c.engine.CoreEngine != nil {
			c.stateMu.Lock()
			c.state = c.engine.CoreEngine.LastState
			c.stateMu.Unlock()
		}
		if err := c.Save(); err != nil {
			c.emitStatus("error", map[string]any{"error": err.Error(), "faulted": true})
			return
		}
		c.emitStatus("idle", nil)
		c.bridge.emitEvent("session.done", c.Snapshot())
	}()
	return map[string]any{"accepted": true}, nil
}

func (c *SessionController) Abort() {
	c.engine.Abort()
	c.submitMu.Lock()
	cancel := c.submitCancel
	c.submitMu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.emitStatus("aborted", nil)
}

func (c *SessionController) Shutdown() {
	c.Abort()
	if c.engine != nil {
		if c.engine.CoreEngine != nil && c.engine.CoreEngine.Runtime != nil {
			_ = c.engine.CoreEngine.Runtime.Close()
		}
		c.engine.Shutdown()
	}
	if c.journal != nil {
		_ = c.journal.Close()
	}
}

func (c *SessionController) Clear() {
	c.Abort()
	c.engine.Reset()
	fresh := agent.NewAgentState()
	c.stateMu.Lock()
	c.state = &fresh
	c.stateMu.Unlock()
	if c.engine != nil && c.engine.CoreEngine != nil {
		c.engine.CoreEngine.LastState = &fresh
	}
	c.ui.MountStateSnapshot(nil)
	_ = c.Save()
}

func (c *SessionController) Save() error {
	c.stateMu.Lock()
	state := c.state
	c.stateMu.Unlock()
	if state == nil {
		return nil
	}
	return c.persistStateSnapshot(state)
}

func (c *SessionController) observeState(state *agent.AgentState) {
	if state == nil {
		return
	}
	c.stateMu.Lock()
	c.state = state
	c.stateMu.Unlock()
	_ = c.persistStateSnapshot(state)
}

func (c *SessionController) persistStateSnapshot(state *agent.AgentState) error {
	var recovery map[string]any
	var tasks []map[string]any
	if c.engine.CoreEngine != nil {
		recovery = c.engine.CoreEngine.ExportSkillRecoverySnapshot()
		if c.engine.CoreEngine.TaskRuntime != nil {
			tasks = c.engine.CoreEngine.TaskRuntime.ExportSnapshot()
		}
	}
	if c.journal == nil {
		return errors.New("runtime journal is unavailable")
	}
	if err := session.AppendRuntimeState(context.Background(), c.journal, state, recovery, tasks); err != nil {
		return err
	}
	return c.store.UpdateMetaProjection(c.id, len(state.Messages), state.TurnCount)
}

func (c *SessionController) Compact() map[string]any {
	c.stateMu.Lock()
	state := c.state
	c.stateMu.Unlock()
	compressed, stats := c.engine.Compact(state)
	c.stateMu.Lock()
	c.state = &compressed
	c.stateMu.Unlock()
	c.ui.MountStateSnapshot(&compressed)
	_ = c.Save()
	payload, _ := json.Marshal(stats)
	out := map[string]any{}
	_ = json.Unmarshal(payload, &out)
	return out
}

func (c *SessionController) Tokens() map[string]any {
	c.stateMu.Lock()
	state := c.state
	c.stateMu.Unlock()
	input, output := 0, 0
	turns := 0
	if state != nil {
		input, output = state.TokenTotals()
		turns = state.TurnCountValue()
	}
	return map[string]any{
		"input_tokens":  input,
		"output_tokens": output,
		"total_tokens":  input + output,
		"turn_count":    turns,
	}
}

func (c *SessionController) ToggleYolo() map[string]any {
	return c.setYolo(!c.RuntimeConfig().Yolo)
}

func (c *SessionController) SetYolo(enabled bool) map[string]any {
	return c.setYolo(enabled)
}

func (c *SessionController) setYolo(enabled bool) map[string]any {
	c.stateMu.Lock()
	state := c.state
	if state == nil {
		s := agent.NewAgentState()
		state = &s
		c.state = state
	}
	if state.PermissionState == nil {
		state.PermissionState = security.DefaultPermissionState()
	}
	state.PermissionState.YoloMode = enabled
	c.cfg.Yolo = enabled
	if c.engine != nil {
		c.engine.Config.Yolo = enabled
		if c.engine.CoreEngine != nil {
			c.engine.CoreEngine.Config.Yolo = enabled
		}
	}
	c.stateMu.Unlock()
	_ = c.Save()
	c.ui.MountStateSnapshot(state)
	return map[string]any{"yolo": enabled}
}

func (c *SessionController) SlashRows() []luminacli.CommandHelpRow {
	return luminacli.IterCommandHelpRows(c.engine.SkillRegistry(), c.cfg.CWD)
}

func (c *SessionController) SlashItems() []luminacli.CommandCompletionItem {
	return luminacli.IterCommandCompletionItems(c.engine.SkillRegistry(), c.cfg.CWD)
}

func (c *SessionController) Skills() []map[string]any {
	registry := c.engine.SkillRegistry()
	if registry == nil {
		return []map[string]any{}
	}
	skills := registry.ListUserInvocable(c.cfg.CWD)
	out := make([]map[string]any, 0, len(skills))
	for _, skill := range skills {
		out = append(out, map[string]any{
			"name":        skill.CanonicalName,
			"description": skill.Frontmatter.Description,
			"context":     skill.Frontmatter.Context,
			"source":      string(skill.Source),
			"directory":   skill.Directory,
		})
	}
	return out
}

func (c *SessionController) MCPTools() []map[string]any {
	if c.engine == nil || c.engine.CoreEngine == nil || c.engine.CoreEngine.Registry == nil {
		return []map[string]any{}
	}
	tools := c.engine.CoreEngine.Registry.ListTools()
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		out = append(out, map[string]any{
			"name":        tool.Name(),
			"description": tool.Description(),
		})
	}
	return out
}

func (c *SessionController) ResolvePermission(requestID, decision string) bool {
	return c.bridge.ResolvePermission(requestID, decision)
}

func (c *SessionController) nextSeq() int64 {
	return c.seq.Add(1)
}

func (c *SessionController) emitStatus(status string, extra map[string]any) {
	payload := map[string]any{"status": status}
	for key, value := range extra {
		payload[key] = value
	}
	c.bridge.emitEvent("session.status", payload)
}
