package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"LuminaCode/agent"
	luminacli "LuminaCode/cli"
	"LuminaCode/config"
	"LuminaCode/maintenance"
	"LuminaCode/security"
	"LuminaCode/session"
	luminaui "LuminaCode/ui"

	"github.com/google/uuid"
)

type SessionManager struct {
	baseConfig config.Config
	store      *session.Store
	emit       func(PushEvent)

	mu       sync.Mutex
	sessions map[string]*SessionController
}

func NewSessionManager(cfg config.Config, emit func(PushEvent)) *SessionManager {
	return &SessionManager{
		baseConfig: cfg,
		store:      session.NewStore(cfg.SessionDir),
		emit:       emit,
		sessions:   map[string]*SessionController{},
	}
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
	state := m.store.LoadState(sessionID)
	if state == nil {
		messages := m.store.Load(sessionID)
		if len(messages) > 0 {
			s := agent.NewAgentState()
			s.Messages = messages
			state = &s
		}
	}
	if state == nil {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}
	return m.createWithState(sessionID, cwd, state)
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
	cfg := m.baseConfig
	if strings.TrimSpace(cwd) != "" && cwd != cfg.CWD {
		cfg = config.NewConfigForCWD(cwd)
		applyPinnedDaemonConfig(&cfg, m.baseConfig)
	}
	engine := agent.NewQueryEngine(&cfg)
	if recovery := m.store.LoadSkillRecovery(sessionID); recovery != nil && engine.CoreEngine != nil {
		engine.CoreEngine.ImportSkillRecoverySnapshot(recovery)
		engine.CoreEngine.MarkSkillHistoryCompacted("main")
	}
	if tasks := m.store.LoadTaskRuntimeSnapshot(sessionID); len(tasks) > 0 && engine.CoreEngine != nil && engine.CoreEngine.TaskRuntime != nil {
		engine.CoreEngine.TaskRuntime.ImportSnapshot(tasks)
	}
	controller := NewSessionController(sessionID, cfg, engine, state, m.store, m.emit)
	m.mu.Lock()
	m.sessions[sessionID] = controller
	m.mu.Unlock()
	controller.Mount()
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
	id     string
	cfg    config.Config
	engine *agent.QueryEngine
	store  *session.Store
	ui     *luminaui.UiRuntime
	bridge *WSRendererBridge

	stateMu sync.Mutex
	state   *agent.AgentState

	submitMu     sync.Mutex
	submitCancel context.CancelFunc

	busy atomic.Bool
	seq  atomic.Int64
}

func NewSessionController(sessionID string, cfg config.Config, engine *agent.QueryEngine, state *agent.AgentState, store *session.Store, emit func(PushEvent)) *SessionController {
	controller := &SessionController{
		id:     sessionID,
		cfg:    cfg,
		engine: engine,
		store:  store,
		state:  state,
	}
	controller.bridge = NewWSRendererBridge(sessionID, emit, controller.nextSeq)
	controller.ui = luminaui.NewUiRuntime(engine, controller.bridge)
	if engine != nil && engine.CoreEngine != nil {
		engine.CoreEngine.StateObserver = controller.observeState
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
	return SessionSnapshot{
		SessionID: c.id,
		Frame:     c.ui.Frame,
		Busy:      c.busy.Load(),
		Model:     c.cfg.APIModel,
		CWD:       c.cfg.CWD,
	}
}

func (c *SessionController) Submit(ctx context.Context, input string) error {
	if strings.TrimSpace(input) == "" {
		return errors.New("empty input")
	}
	if !c.busy.CompareAndSwap(false, true) {
		return errors.New("session_busy")
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
		_ = c.Save()
		c.emitStatus("idle", nil)
		c.bridge.emitEvent("session.done", c.Snapshot())
	}()
	return nil
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
		c.engine.Shutdown()
	}
}

func (c *SessionController) Clear() {
	c.Abort()
	c.engine.Reset()
	c.stateMu.Lock()
	c.state = nil
	c.stateMu.Unlock()
	c.ui.MountStateSnapshot(nil)
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
	return c.store.SaveSnapshotWithRecovery(c.id, state, recovery, tasks)
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
