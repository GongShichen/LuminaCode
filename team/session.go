package team

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"LuminaCode/agent"
	"LuminaCode/apppaths"
	"LuminaCode/config"
	"LuminaCode/security"
	coretools "LuminaCode/tools"

	"github.com/google/uuid"
)

type PushFunc func(parentSessionID string, eventType string, payload any)
type PermissionFunc func(parentSessionID string, payload map[string]any) string

type Manager struct {
	Config        config.Config
	emit          PushFunc
	askPermission PermissionFunc

	mu       sync.Mutex
	sessions map[string]*Session
}

func NewManager(cfg config.Config, emit PushFunc, ask PermissionFunc) *Manager {
	return &Manager{Config: cfg, emit: emit, askPermission: ask, sessions: map[string]*Session{}}
}

func (m *Manager) List() []TeamListItem {
	return NewLoader(m.Config).ListTeams()
}

func (m *Manager) CreateTemplate(name string) (TeamTemplateResult, error) {
	return NewLoader(m.Config).CreateTemplate(name)
}

func (m *Manager) Start(parentSessionID, teamName, cwd string) (*Session, error) {
	return m.StartWithConfig(parentSessionID, teamName, cwd, m.Config)
}

func (m *Manager) StartWithConfig(parentSessionID, teamName, cwd string, base config.Config) (*Session, error) {
	spec, err := NewLoader(m.Config).Load(teamName)
	if err != nil {
		return nil, err
	}
	cfg := base
	if strings.TrimSpace(cwd) != "" && cwd != cfg.CWD {
		cfg = config.NewConfigForCWD(cwd)
		cfg.TeamDir = base.TeamDir
		if strings.TrimSpace(cfg.TeamDir) == "" {
			cfg.TeamDir = m.Config.TeamDir
		}
		applyPinnedTeamConfig(&cfg, base)
	}
	session := NewSession(parentSessionID, cfg, spec, m.emit, m.askPermission)
	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()
	session.persist()
	session.emit("team.started", session.Snapshot())
	return session, nil
}

func (m *Manager) ApplyParentRuntimeConfig(parentSessionID string, cfg config.Config) {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		if session.ParentSessionID == parentSessionID {
			sessions = append(sessions, session)
		}
	}
	m.mu.Unlock()
	for _, session := range sessions {
		session.ApplyRuntimeConfig(cfg)
	}
}

func (m *Manager) Get(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[id]
	if session == nil {
		return nil, fmt.Errorf("team session %s not found", id)
	}
	return session, nil
}

func (m *Manager) Abort(id string) bool {
	session, err := m.Get(id)
	if err != nil {
		return false
	}
	session.Abort()
	return true
}

func (m *Manager) Shutdown() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.Unlock()
	for _, session := range sessions {
		session.Shutdown()
	}
}

func (m *Manager) ResolvePermission(requestID, decision string) bool {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.Unlock()
	for _, session := range sessions {
		if session.ResolvePermission(requestID, decision) {
			return true
		}
	}
	return false
}

func (m *Manager) HandleA2A(ctx context.Context, teamSessionID, agentID, method string, params json.RawMessage) (any, error) {
	session, err := m.Get(teamSessionID)
	if err != nil {
		return nil, err
	}
	return session.HandleA2A(ctx, agentID, method, params)
}

func applyPinnedTeamConfig(target *config.Config, source config.Config) {
	for field, pinned := range source.PinnedFields {
		if !pinned {
			continue
		}
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
		}
	}
}

type Session struct {
	ID              string
	ParentSessionID string
	Config          config.Config
	Spec            TeamSpec

	emitFn        PushFunc
	askPermission PermissionFunc
	rootDir       string

	mu             sync.Mutex
	agents         map[string]*AgentRuntime
	dialogue       []DialogueEntry
	activity       map[string]ActivityRow
	artifacts      []Artifact
	timeline       []TimelineEvent
	gate           GateStatus
	contract       *AcceptanceContract
	gateVerdicts   map[string]GateVerdict
	a2aTasks       map[string]TeamTask
	agentActiveA2A map[string]string
	loopIteration  int
	waitingForUser bool
	completed      bool
	finalAnswer    string

	busy   atomic.Bool
	cancel context.CancelFunc
	runCtx context.Context

	workspaceMu  sync.Mutex
	permissionMu sync.Mutex
	permissions  map[string]chan string
}

type AgentRuntime struct {
	Spec   TeamAgentSpec
	Engine *agent.QueryEngine
	State  *agent.AgentState
	mu     sync.Mutex
	busy   bool
}

type TeamTask struct {
	ID                string   `json:"id"`
	From              string   `json:"from"`
	To                string   `json:"to"`
	Status            string   `json:"status"`
	Summary           string   `json:"summary"`
	ExpectedArtifacts []string `json:"expected_artifacts,omitempty"`
	Result            string   `json:"result"`
	Err               string   `json:"error,omitempty"`
	done              chan TeamTask
}

func NewSession(parentSessionID string, cfg config.Config, spec TeamSpec, emit PushFunc, ask PermissionFunc) *Session {
	id := "team-" + uuid.NewString()
	root := ""
	useProjectData := cfg.ProjectPaths.TeamsDir != "" && cfg.Paths.ActiveSessionsDir != "" &&
		filepath.Clean(cfg.SessionDir) == filepath.Clean(cfg.Paths.ActiveSessionsDir)
	if useProjectData {
		root = filepath.Join(cfg.ProjectPaths.TeamsDir, spec.Name, id)
	} else {
		root = filepath.Join(cfg.SessionDir, parentSessionID, "teams", id)
	}
	session := &Session{
		ID:              id,
		ParentSessionID: parentSessionID,
		Config:          cfg,
		Spec:            spec,
		emitFn:          emit,
		askPermission:   ask,
		rootDir:         root,
		agents:          map[string]*AgentRuntime{},
		activity:        map[string]ActivityRow{},
		gate:            GateStatus{},
		gateVerdicts:    map[string]GateVerdict{},
		a2aTasks:        map[string]TeamTask{},
		agentActiveA2A:  map[string]string{},
		permissions:     map[string]chan string{},
	}
	for _, agentSpec := range spec.AgentSpecs {
		session.agents[agentSpec.Name] = session.newAgentRuntime(agentSpec)
		session.activity[agentSpec.Name] = ActivityRow{
			AgentID:     agentSpec.Name,
			DisplayName: agentSpec.DisplayName,
			Status:      "idle",
			Summary:     "ready",
		}
	}
	return session
}

func (s *Session) newAgentRuntime(spec TeamAgentSpec) *AgentRuntime {
	cfg := s.Config
	cfg.SessionDir = filepath.Join(s.rootDir, "agents")
	cfg.SessionMemoryDir = s.Config.SessionDir
	cfg.ProjectRuntimeDir = s.teamRuntimeDir()
	config.PinFields(&cfg, "project_runtime_dir")
	cfg.WebSearchCacheScope = s.ID
	cfg.SystemPromptPath = s.materializeAgentSystemPrompt(spec)
	cfg.UserSkillsDir = spec.SkillsDir
	cfg.SkillsDir = filepath.Join(apppaths.ProjectLocalDirName, "__team_no_project_skills__")
	cfg.BundledSkillsDir = filepath.Join(spec.RootDir, "__no_bundled_skills__")
	cfg.IsolatedSkillsOnly = true
	if spec.Model != "" && spec.Model != "inherit" {
		cfg.APIModel = spec.Model
	}
	if spec.MaxTurnsPerTask <= 0 {
		cfg.MaxParentTurns = 2147483647
	} else {
		cfg.MaxParentTurns = spec.MaxTurnsPerTask
	}
	engine := agent.NewQueryEngine(&cfg)
	engine.CoreEngine.AgentID = spec.Name
	engine.CoreEngine.AgentType = spec.Name
	engine.CoreEngine.TeamName = s.Spec.Name
	engine.CoreEngine.TeamSessionID = s.ID
	engine.CoreEngine.TeamAgentID = spec.Name
	allow := teamToolAllowlist(spec.Tools)
	engine.CoreEngine.Registry = engine.CoreEngine.Registry.FilteredCopy(allow, ordinarySubagentToolDenylist(), false, false)
	engine.CoreEngine.Registry.Register(NewGetTeamContextTool(s, spec.Name))
	engine.CoreEngine.Registry.Register(NewSendA2AMessageTool(s, spec.Name))
	if spec.Name == s.Spec.EntryAgent {
		engine.CoreEngine.Registry.Register(NewRecordTeamContractTool(s, spec.Name))
		engine.CoreEngine.Registry.Register(NewCompleteTeamTaskTool(s, spec.Name))
	}
	if s.isGateAgent(spec.Name) {
		engine.CoreEngine.Registry.Register(NewSubmitGateVerdictTool(s, spec.Name))
	}
	state := agent.NewAgentState()
	if cfg.Yolo {
		if state.PermissionState == nil {
			state.PermissionState = security.DefaultPermissionState()
		}
		state.PermissionState.YoloMode = true
	}
	return &AgentRuntime{Spec: spec, Engine: engine, State: &state}
}

func (s *Session) refreshAgentSystemPrompts() {
	for _, runtime := range s.agents {
		if runtime == nil || runtime.Engine == nil {
			continue
		}
		path := s.materializeAgentSystemPrompt(runtime.Spec)
		runtime.Engine.Config.SystemPromptPath = path
		if runtime.Engine.CoreEngine != nil {
			runtime.Engine.CoreEngine.Config.SystemPromptPath = path
		}
	}
}

func (s *Session) materializeAgentSystemPrompt(spec TeamAgentSpec) string {
	base := strings.TrimSpace(readText(spec.SystemPromptPath))
	shared := strings.TrimSpace(s.sharedPromptText())
	var body strings.Builder
	body.WriteString("[SECTION: Team Agent Instructions]\n")
	if shared != "" {
		body.WriteString("# Shared Team Prompt\n\n")
		body.WriteString(shared)
	}
	if base != "" {
		if shared != "" {
			body.WriteString("\n\n")
		}
		body.WriteString(base)
	}
	text := strings.TrimRight(body.String(), "\n") + "\n"
	path := filepath.Join(s.rootDir, "prompts", spec.Name+".system.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return spec.SystemPromptPath
	}
	if err := apppaths.WriteFileAtomic(path, []byte(text), 0o600); err != nil {
		return spec.SystemPromptPath
	}
	return path
}

func (s *Session) ApplyRuntimeConfig(cfg config.Config) {
	s.mu.Lock()
	s.Config = cfg
	s.mu.Unlock()
	for _, runtime := range s.agents {
		if runtime == nil || runtime.Engine == nil {
			continue
		}
		runtime.Engine.Config.Yolo = cfg.Yolo
		if runtime.Engine.CoreEngine != nil {
			runtime.Engine.CoreEngine.Config.Yolo = cfg.Yolo
		}
		if runtime.State != nil {
			if runtime.State.PermissionState == nil {
				runtime.State.PermissionState = security.DefaultPermissionState()
			}
			runtime.State.PermissionState.YoloMode = cfg.Yolo
		}
	}
	s.appendTimeline("team.runtime_config_updated", map[string]any{"yolo": cfg.Yolo})
}

func ordinarySubagentToolDenylist() map[string]struct{} {
	return map[string]struct{}{
		"Agent":       {},
		"TaskList":    {},
		"TaskGet":     {},
		"TaskWait":    {},
		"TaskStop":    {},
		"SendMessage": {},
	}
}

func teamToolAllowlist(raw string) map[string]struct{} {
	value := strings.TrimSpace(raw)
	if value == "" || strings.EqualFold(value, "inherit") || strings.EqualFold(value, "all") {
		return nil
	}
	allow := map[string]struct{}{}
	for _, token := range strings.FieldsFunc(value, func(r rune) bool {
		switch r {
		case ',', ';', '\n', '\r', '\t', ' ':
			return true
		default:
			return false
		}
	}) {
		token = strings.TrimSpace(token)
		if token != "" {
			allow[token] = struct{}{}
		}
	}
	if len(allow) == 0 {
		return nil
	}
	return allow
}

func (s *Session) AgentToolNames(agentID string) []string {
	runtime := s.agents[agentID]
	if runtime == nil || runtime.Engine == nil || runtime.Engine.CoreEngine == nil || runtime.Engine.CoreEngine.Registry == nil {
		return nil
	}
	tools := runtime.Engine.CoreEngine.Registry.ListTools()
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	return names
}

func (s *Session) AgentSkillNames(agentID string) []string {
	runtime := s.agents[agentID]
	if runtime == nil || runtime.Engine == nil || runtime.Engine.CoreEngine == nil {
		return nil
	}
	return runtime.Engine.CoreEngine.SkillNames(s.Config.CWD)
}

func (s *Session) AgentYoloEnabled(agentID string) bool {
	runtime := s.agents[agentID]
	if runtime == nil || runtime.State == nil {
		return false
	}
	return runtime.State.YoloEnabled()
}

func (s *Session) Submit(ctx context.Context, input string) error {
	if strings.TrimSpace(input) == "" {
		return errors.New("empty input")
	}
	if !s.busy.CompareAndSwap(false, true) {
		return errors.New("team_session_busy")
	}
	s.mu.Lock()
	s.completed = false
	s.finalAnswer = ""
	s.gate = GateStatus{}
	s.contract = nil
	s.gateVerdicts = map[string]GateVerdict{}
	s.mu.Unlock()
	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.runCtx = runCtx
	s.mu.Unlock()
	s.cancel = cancel
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.appendRecovery(fmt.Sprintf("Team Loop panic recovered: %v\n%s", recovered, debug.Stack()))
			}
			s.busy.Store(false)
			cancel()
			s.mu.Lock()
			if s.runCtx == runCtx {
				s.runCtx = nil
			}
			s.mu.Unlock()
			s.mu.Lock()
			completed := s.completed
			s.mu.Unlock()
			s.persist()
			if completed {
				s.emit("team.completed", s.Snapshot())
			} else {
				s.emit("team.interrupted_by_user", s.Snapshot())
			}
		}()
		s.runLoop(runCtx, input)
	}()
	return nil
}

func (s *Session) runLoop(ctx context.Context, input string) {
	s.appendDialogue(DialogueEntry{
		FromAgent: "user", ToAgent: []string{s.Spec.EntryAgent}, Kind: "user",
		Summary: "User request", Content: input,
	})
	s.appendTimeline("loop.started", map[string]any{"input": input})
	for {
		if ctx.Err() != nil {
			s.appendTimeline("loop.interrupted_by_user", map[string]any{"reason": ctx.Err().Error()})
			s.emit("team.interrupted_by_user", s.Snapshot())
			return
		}
		s.mu.Lock()
		if s.completed {
			s.mu.Unlock()
			s.emit("team.frame.snapshot", s.Snapshot())
			return
		}
		s.mu.Unlock()
		if s.waitForPendingA2ABeforeNextIteration(ctx) {
			continue
		}
		s.loopIteration++
		iteration := s.loopIteration
		s.emit("team.loop.iteration", s.Snapshot())
		prompt := s.leaderPrompt(input, iteration)
		result, err := s.runAgentTask(ctx, s.Spec.EntryAgent, "user", prompt, "team-loop", []string{"final-answer"}, true)
		if err != nil {
			s.appendRecovery("Team Leader failed; continuing loop with recovery instruction: " + err.Error())
			continue
		}
		if strings.TrimSpace(result) != "" {
			s.appendDialogue(DialogueEntry{
				FromAgent: s.Spec.EntryAgent, ToAgent: []string{"user"}, Kind: "leader_update",
				Summary: "Team Leader update", Content: result,
			})
		}
		s.mu.Lock()
		done := s.completed
		s.mu.Unlock()
		if done {
			s.emit("team.frame.snapshot", s.Snapshot())
			return
		}
		s.appendRecovery("Task is not complete. Team Leader must continue dispatching work, request clarification, or call CompleteTeamTask when gates pass.")
	}
}

func (s *Session) leaderPrompt(userInput string, iteration int) string {
	teamSystem := readText(s.Spec.TeamSystemPath)
	sharedPrompt := strings.TrimSpace(s.sharedPromptText())
	sharedSection := leaderSharedPromptSection(sharedPrompt)
	policy := readText(s.Spec.CompletionPolicy)
	return fmt.Sprintf(`Team Loop iteration #%d.

Current working directory:
%s

User request:
%s

Team system instructions:
%s
%s

Completion policy:
%s

Available agents:
%s

Current readable dialogue:
%s

Current A2A task status:
%s

Hard runtime rules:
- A2A is traceable. Never say A2A messages cannot be tracked.
- Follow this team's configured task policies and gates. If a dispatch requires a recorded contract, call RecordTeamContract first.
- Verbal assignment is invalid. To assign work, call SendA2AMessage and wait for the tool result.
- If an A2A task is pending or running for a target agent, do not send the same target duplicate work. Continue with other unblocked work or wait for the existing task status to become completed.
- Before starting a new Team Leader iteration, the runtime waits for pending A2A tasks when this team enables wait_for_pending_a2a_before_next_iteration.
- If a specialist exists, do not replace that specialist with a generic background agent or ordinary sub-agent.
- Every specialist's visible progress is projected into the Team transcript; keep member messages useful for the user.
- Preserve user-specified artifact paths exactly.
- If the user asks to create a new named project or directory, infer the project root as Current working directory + "/" + that name unless the user gives an absolute path. All dispatches, files, README paths, tests, and final artifact checks must use that project root. Do not flatten named projects into the current working directory.
- If a configured gate verdict rejects or fails the work, immediately dispatch concrete repair tasks for the cited findings, then re-run the relevant gates.
- If the user asks for multiple components, treat them as integrated by default. User-facing components must consume the agreed component contract unless the user explicitly asks for independent, direct-file, or mock-only implementations.
- Configured gate agents must use SubmitGateVerdict. CompleteTeamTask only succeeds when runtime gate verdicts satisfy the recorded contract.

Continue the Team Loop. Use SendA2AMessage for member-to-member work. Do not stop because of timeout, error, rejection, or long iteration count. Only call CompleteTeamTask when the completion policy is fully satisfied.`, iteration, s.Config.CWD, userInput, teamSystem, sharedSection, policy, s.agentCardsText(), s.dialogueText(), s.a2aTaskStatusText())
}

func leaderSharedPromptSection(sharedPrompt string) string {
	sharedPrompt = strings.TrimSpace(sharedPrompt)
	if sharedPrompt == "" {
		return ""
	}
	const maxLoopPromptSharedPromptRunes = 1800
	visible := truncateTeamVisible(sharedPrompt, maxLoopPromptSharedPromptRunes)
	if visible != sharedPrompt {
		visible += "\n\n[Shared prompt shortened here; the full shared prompt is already prepended to each agent system prompt.]"
	}
	return "\nShared team prompt:\n" + visible + "\n"
}

func (s *Session) sharedPromptText() string {
	if s == nil || strings.TrimSpace(s.Spec.SharedPromptPath) == "" {
		return ""
	}
	return readText(s.Spec.SharedPromptPath)
}

func (s *Session) SendA2AMessage(ctx context.Context, from string, input A2AMessageInput) map[string]any {
	if err := s.ensureContractBeforeDispatch(from, input); err != nil {
		s.appendRecovery(err.Error())
		return map[string]any{"status": "error", "error": err.Error()}
	}
	input.TimeoutSeconds = s.resolveA2AWaitSeconds(input.TimeoutSeconds)
	targets := normalizeTargets(input.To)
	allowed, msg := s.validateTargets(from, targets)
	if !allowed {
		return map[string]any{"status": "error", "error": msg}
	}
	results := make([]map[string]any, 0, len(targets))
	notifyTargets := make([]string, 0, len(targets))
	startTargets := make([]string, 0, len(targets))
	for _, target := range targets {
		if target == s.Spec.EntryAgent && from != s.Spec.EntryAgent {
			notifyTargets = append(notifyTargets, target)
			continue
		}
		if task, active := s.activeA2ATask(target); active {
			results = append(results, map[string]any{
				"to":               target,
				"status":           "task_pending",
				"task_id":          task.ID,
				"existing_task_id": task.ID,
				"message":          fmt.Sprintf("%s already has an active A2A task from %s: %s", target, task.From, firstNonEmpty(task.Summary, "working")),
			})
			continue
		}
		startTargets = append(startTargets, target)
	}
	if len(notifyTargets) > 0 {
		notifyID := "a2a-" + uuid.NewString()[:8]
		s.appendDialogue(DialogueEntry{
			FromAgent: from, ToAgent: notifyTargets, Kind: "message",
			Summary: input.TaskType, Content: input.Message, TaskID: notifyID,
			ArtifactRefs: input.ExpectedArtifacts,
		})
		s.appendTimeline("message/deliver", map[string]any{"task_id": notifyID, "from": from, "to": notifyTargets, "message": input.Message})
		for _, target := range notifyTargets {
			results = append(results, map[string]any{"to": target, "status": "delivered", "task_id": notifyID})
		}
	}
	if len(startTargets) == 0 {
		return map[string]any{"status": "ok", "results": results}
	}
	targets = startTargets
	taskID := "a2a-" + uuid.NewString()[:8]
	s.appendDialogue(DialogueEntry{
		FromAgent: from, ToAgent: targets, Kind: "message",
		Summary: input.TaskType, Content: input.Message, TaskID: taskID,
		ArtifactRefs: input.ExpectedArtifacts,
	})
	s.appendTimeline("message/send", map[string]any{"task_id": taskID, "from": from, "to": targets, "message": input.Message})
	var wg sync.WaitGroup
	resultCh := make(chan map[string]any, len(targets))
	parallelism := s.Spec.Loop.MaxParallelAgents
	if parallelism <= 0 {
		parallelism = len(targets)
	}
	sem := make(chan struct{}, parallelism)
	for _, target := range targets {
		target := target
		a2aTask, started := s.beginA2ATask(taskID, from, target, input.TaskType, input.ExpectedArtifacts)
		if !started {
			results = append(results, map[string]any{
				"to":               target,
				"status":           "task_pending",
				"task_id":          a2aTask.ID,
				"existing_task_id": a2aTask.ID,
				"message":          fmt.Sprintf("%s already has an active A2A task from %s: %s", target, a2aTask.From, firstNonEmpty(a2aTask.Summary, "working")),
			})
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					msg := fmt.Sprintf("agent dispatch panic: %v\n%s", recovered, debug.Stack())
					s.appendRecovery(msg)
					s.finishA2ATask(a2aTask.ID, target, "error", "", msg)
					resultCh <- map[string]any{"to": target, "status": "error", "error": msg}
				}
			}()
			sem <- struct{}{}
			defer func() { <-sem }()
			done := make(chan map[string]any, 1)
			go func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						msg := fmt.Sprintf("agent task panic: %v\n%s", recovered, debug.Stack())
						s.appendRecovery(msg)
						done <- map[string]any{"to": target, "status": "error", "error": msg}
					}
				}()
				result, err := s.runAgentTask(s.activeRunContext(), target, from, input.Message, input.TaskType, input.ExpectedArtifacts, false)
				if err != nil {
					s.finishA2ATask(a2aTask.ID, target, "error", "", err.Error())
					done <- map[string]any{"to": target, "status": "error", "error": err.Error()}
					return
				}
				s.finishA2ATask(a2aTask.ID, target, "completed", result, "")
				done <- map[string]any{"to": target, "status": "completed", "task_id": a2aTask.ID, "result": result}
			}()
			awaitResponse := true
			if input.AwaitResponse != nil {
				awaitResponse = *input.AwaitResponse
			}
			if !awaitResponse {
				resultCh <- map[string]any{"to": target, "status": "queued", "task_id": a2aTask.ID}
				return
			}
			select {
			case result := <-done:
				resultCh <- result
			case task := <-a2aTask.done:
				resultCh <- a2aResultFromTask(task)
			case <-time.After(time.Duration(input.TimeoutSeconds) * time.Second):
				s.markA2ATaskPending(a2aTask.ID, target)
				s.appendRecovery(fmt.Sprintf("%s did not finish within wait window; Team Loop continues with task pending.", target))
				s.setActivity(target, "running", "continuing after A2A wait timeout", a2aTask.ID)
				s.appendDialogue(DialogueEntry{
					FromAgent: target,
					ToAgent:   []string{from},
					Kind:      "timeout",
					Summary:   "A2A wait window elapsed",
					Content: fmt.Sprintf(
						"%s did not finish within the %d second A2A wait window. This is not a user interrupt; the agent keeps working in the Team run context and the Team Loop may continue, retry, or collect a later result.",
						s.agentDisplayName(target), input.TimeoutSeconds,
					),
					TaskID: a2aTask.ID,
				})
				resultCh <- map[string]any{"to": target, "status": "task_pending", "task_id": a2aTask.ID}
			case <-ctx.Done():
				s.finishA2ATask(a2aTask.ID, target, "interrupted", "", ctx.Err().Error())
				resultCh <- map[string]any{"to": target, "status": "interrupted_by_user", "task_id": a2aTask.ID}
			}
		}()
	}
	wg.Wait()
	close(resultCh)
	for result := range resultCh {
		results = append(results, result)
	}
	return map[string]any{"status": "ok", "task_id": taskID, "results": results}
}

func (s *Session) defaultA2ATimeoutSeconds() int {
	if s == nil || s.Spec.Loop.A2ADefaultTimeoutSeconds <= 0 {
		return 300
	}
	return s.Spec.Loop.A2ADefaultTimeoutSeconds
}

func (s *Session) resolveA2AWaitSeconds(requested int) int {
	wait := requested
	if wait <= 0 {
		wait = s.defaultA2ATimeoutSeconds()
	}
	if s != nil && s.Spec.Loop.MinA2ATimeoutSeconds > 0 && wait < s.Spec.Loop.MinA2ATimeoutSeconds {
		wait = s.Spec.Loop.MinA2ATimeoutSeconds
	}
	return wait
}

func (s *Session) activeRunContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runCtx != nil {
		return s.runCtx
	}
	return context.Background()
}

func (s *Session) shouldWaitForPendingA2ABeforeNextIteration() bool {
	if s == nil || s.Spec.Loop.WaitForPendingA2ABeforeNextIteration == nil {
		return true
	}
	return *s.Spec.Loop.WaitForPendingA2ABeforeNextIteration
}

func (s *Session) waitForPendingA2ABeforeNextIteration(ctx context.Context) bool {
	if !s.shouldWaitForPendingA2ABeforeNextIteration() {
		return false
	}
	tasks := s.activeA2ATasks()
	if len(tasks) == 0 {
		return false
	}
	s.appendTimeline("loop.waiting_for_a2a", map[string]any{"tasks": tasks})
	s.appendRecovery("Waiting for pending A2A tasks before the next Team Leader iteration to avoid overlapping workspace edits.")
	s.emit("team.frame.snapshot", s.Snapshot())
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return true
		case <-ticker.C:
			if len(s.activeA2ATasks()) == 0 {
				s.appendTimeline("loop.a2a_wait_complete", map[string]any{"previous_tasks": tasks})
				s.emit("team.frame.snapshot", s.Snapshot())
				return true
			}
		}
	}
}

func a2aTaskKey(taskID, target string) string {
	return taskID + "\x00" + target
}

func isTerminalA2AStatus(status string) bool {
	switch status {
	case "completed", "error", "failed", "interrupted":
		return true
	default:
		return false
	}
}

func (s *Session) activeA2ATasks() []TeamTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := make([]TeamTask, 0, len(s.agentActiveA2A))
	for target, key := range s.agentActiveA2A {
		task, ok := s.a2aTasks[key]
		if !ok || isTerminalA2AStatus(task.Status) {
			delete(s.agentActiveA2A, target)
			continue
		}
		tasks = append(tasks, task)
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].To == tasks[j].To {
			return tasks[i].ID < tasks[j].ID
		}
		return tasks[i].To < tasks[j].To
	})
	return tasks
}

func (s *Session) activeA2ATask(target string) (TeamTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	activeKey := s.agentActiveA2A[target]
	if activeKey == "" {
		return TeamTask{}, false
	}
	task, ok := s.a2aTasks[activeKey]
	if !ok || isTerminalA2AStatus(task.Status) {
		delete(s.agentActiveA2A, target)
		return TeamTask{}, false
	}
	return task, true
}

func (s *Session) beginA2ATask(taskID, from, target, summary string, expectedArtifacts []string) (TeamTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if activeKey := s.agentActiveA2A[target]; activeKey != "" {
		if task, ok := s.a2aTasks[activeKey]; ok && !isTerminalA2AStatus(task.Status) {
			return task, false
		}
		delete(s.agentActiveA2A, target)
	}
	task := TeamTask{
		ID:                taskID,
		From:              from,
		To:                target,
		Status:            "running",
		Summary:           summary,
		ExpectedArtifacts: nonEmptyStrings(expectedArtifacts),
		done:              make(chan TeamTask, 1),
	}
	key := a2aTaskKey(taskID, target)
	s.a2aTasks[key] = task
	s.agentActiveA2A[target] = key
	return task, true
}

func (s *Session) markA2ATaskPending(taskID, target string) {
	key := a2aTaskKey(taskID, target)
	s.mu.Lock()
	task, ok := s.a2aTasks[key]
	if ok && !isTerminalA2AStatus(task.Status) {
		task.Status = "pending"
		s.a2aTasks[key] = task
	}
	s.mu.Unlock()
	s.persist()
}

func (s *Session) finishA2ATask(taskID, target, status, result, errText string) {
	key := a2aTaskKey(taskID, target)
	var task TeamTask
	var previousStatus string
	var ok bool
	var notify chan TeamTask
	s.mu.Lock()
	task, ok = s.a2aTasks[key]
	if !ok {
		task = TeamTask{ID: taskID, To: target}
	}
	previousStatus = task.Status
	if isTerminalA2AStatus(previousStatus) {
		s.mu.Unlock()
		return
	}
	notify = task.done
	task.Status = status
	task.Result = result
	task.Err = errText
	s.a2aTasks[key] = task
	if s.agentActiveA2A[target] == key {
		delete(s.agentActiveA2A, target)
	}
	s.mu.Unlock()
	if notify != nil {
		select {
		case notify <- task:
		default:
		}
	}
	s.persist()
	if previousStatus == "pending" {
		content := fmt.Sprintf("A2A task %s for %s is now %s.", taskID, target, status)
		if errText != "" {
			content += " Error: " + errText
		}
		if result != "" {
			content += "\n\nResult preview:\n" + truncateTeamVisible(result, 1200)
		}
		s.appendDialogue(DialogueEntry{
			FromAgent: "team-loop",
			ToAgent:   []string{task.From},
			Kind:      "task_status",
			Summary:   "A2A task completed",
			Content:   content,
			TaskID:    taskID,
		})
	}
}

func (s *Session) finishActiveA2ATask(target, status, result, errText string) bool {
	s.mu.Lock()
	activeKey := s.agentActiveA2A[target]
	if activeKey == "" {
		s.mu.Unlock()
		return false
	}
	task, ok := s.a2aTasks[activeKey]
	s.mu.Unlock()
	if !ok || isTerminalA2AStatus(task.Status) {
		return false
	}
	s.finishA2ATask(task.ID, target, status, result, errText)
	return true
}

func a2aResultFromTask(task TeamTask) map[string]any {
	out := map[string]any{
		"to":      task.To,
		"status":  task.Status,
		"task_id": task.ID,
	}
	if task.Result != "" {
		out["result"] = task.Result
	}
	if task.Err != "" {
		out["error"] = task.Err
	}
	return out
}

func (s *Session) ensureContractBeforeDispatch(from string, input A2AMessageInput) error {
	if from != s.Spec.EntryAgent {
		return nil
	}
	targets := normalizeTargets(input.To)
	s.mu.Lock()
	hasContract := s.contract != nil
	s.mu.Unlock()
	matched := false
	for _, policy := range s.matchTaskPolicies(targets, input.TaskType, !hasContract) {
		if policy.RequiresContract {
			matched = true
			break
		}
	}
	if !matched {
		return nil
	}
	if hasContract {
		return nil
	}
	return fmt.Errorf("Cannot dispatch %q before recording the team contract. Team Leader must call RecordTeamContract first.", input.TaskType)
}

func (s *Session) runAgentTask(ctx context.Context, agentID, from, prompt, taskType string, expectedArtifacts []string, isLeader bool) (string, error) {
	runtime, ok := s.agents[agentID]
	if !ok {
		return "", fmt.Errorf("agent %s not found", agentID)
	}
	runtime.mu.Lock()
	if runtime.busy {
		s.setActivity(agentID, "queued", "waiting for current task", "")
	}
	for runtime.busy {
		runtime.mu.Unlock()
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		runtime.mu.Lock()
	}
	runtime.busy = true
	runtime.mu.Unlock()
	defer func() {
		runtime.mu.Lock()
		runtime.busy = false
		runtime.mu.Unlock()
	}()
	taskID := "task-" + uuid.NewString()[:8]
	s.setActivity(agentID, "running", firstNonEmpty(taskType, "working"), taskID)
	s.appendTimeline("team.agent.started", map[string]any{
		"agent_id": agentID,
		"from":     from,
		"task_id":  taskID,
		"summary":  firstNonEmpty(taskType, "started"),
	})
	s.emit("team.agent.started", s.Snapshot())
	policies := s.matchTaskPolicies([]string{agentID}, taskType, !s.hasRecordedContract())
	stageGuard := teamTaskStageGuard(agentID, taskType, expectedArtifacts, policies)
	if requiresExclusiveWorkspace(policies) {
		s.setActivity(agentID, "running", "waiting for exclusive workspace", taskID)
		s.workspaceMu.Lock()
		defer s.workspaceMu.Unlock()
		s.setActivity(agentID, "running", firstNonEmpty(taskType, "working"), taskID)
	}
	workspaceBefore := map[string]workspaceFileStamp(nil)
	if shouldAuditTaskWrites(policies) {
		workspaceBefore = snapshotWorkspaceFiles(s.Config.CWD)
	}
	agentPrompt := fmt.Sprintf("Message from %s.\nTask type: %s\nExpected artifacts: %s\n\n%s\n\n%s", from, taskType, strings.Join(expectedArtifacts, ", "), stageGuard, prompt)
	var text strings.Builder
	var eventErrors []string
	var writeAttemptPaths []string
	agentSessionID := s.ParentSessionID
	if strings.TrimSpace(agentSessionID) == "" {
		agentSessionID = agentID
	}
	for event := range runtime.Engine.SubmitMessage(ctx, agentPrompt, runtime.State, agentSessionID, agentID) {
		switch event.Type {
		case "text":
			text.WriteString(event.Content)
			s.emit("team.agent.message", map[string]any{"agent_id": agentID, "delta": event.Content})
		case "tool_call":
			s.appendTimeline("team.agent.tool_call", map[string]any{
				"agent_id": agentID,
				"task_id":  taskID,
				"summary":  formatTeamToolCall(event, true),
			})
			s.setActivity(agentID, "running", "using "+firstNonEmpty(event.Content, stringFromMetadata(event.Metadata, "tool_name"), "tool"), taskID)
		case "tool_result":
			s.appendTimeline("team.agent.tool_result", map[string]any{
				"agent_id": agentID,
				"task_id":  taskID,
				"summary":  formatTeamToolResult(event, true),
			})
			if isTruthy(event.Metadata["is_error"]) {
				s.setActivity(agentID, "running", "tool error; continuing", taskID)
			} else {
				if path, ok := writeFilePathFromToolResult(fmt.Sprint(event.Metadata["result"])); ok && s.pathInsideWorkspace(path) {
					writeAttemptPaths = append(writeAttemptPaths, path)
				}
				s.recordWriteFileArtifactFromEvent(agentID, event)
			}
		case "error":
			msg := formatAgentError(agentID, event)
			eventErrors = append(eventErrors, msg)
			s.appendRecovery(msg)
		case "permission_needed":
			decision := s.requestPermission(agentID, event)
			if isTruthy(event.Metadata["sandbox_unavailable"]) &&
				(decision == agent.PermissionOnce || decision == agent.PermissionAlways || decision == "true") {
				cfg := s.Config
				cfg.Yolo = true
				s.ApplyRuntimeConfig(cfg)
			}
			runtime.Engine.ResolvePermission(decision, toolNameFromPermissionEvent(event))
		}
	}
	if runtime.Engine.CoreEngine != nil && runtime.Engine.CoreEngine.LastState != nil {
		runtime.State = runtime.Engine.CoreEngine.LastState
	}
	if ctx.Err() != nil {
		s.setActivity(agentID, "interrupted", "interrupted by user", taskID)
		s.persist()
		s.emit("team.frame.snapshot", s.Snapshot())
		return "", ctx.Err()
	}
	if len(workspaceBefore) > 0 {
		auditExpectedArtifacts := s.expectedArtifactsForWorkspaceAudit(expectedArtifacts)
		violations := append(
			taskWriteViolations(s.Config.CWD, workspaceBefore, snapshotWorkspaceFiles(s.Config.CWD), auditExpectedArtifacts, policies),
			taskWriteAttemptViolations(s.Config.CWD, writeAttemptPaths, auditExpectedArtifacts, policies)...,
		)
		violations = compactSortedStrings(violations)
		if len(violations) > 12 {
			violations = append(violations[:12], fmt.Sprintf("... %d more", len(violations)-12))
		}
		if len(violations) > 0 {
			errMsg := fmt.Sprintf("agent %s modified implementation/runtime files during %s stage: %s", agentID, firstNonEmpty(taskType, "planning"), strings.Join(violations, ", "))
			if invalidated := s.invalidateGateVerdictsForAgent(agentID, errMsg); len(invalidated) > 0 {
				errMsg += fmt.Sprintf(" (invalidated gate verdicts: %s)", strings.Join(invalidated, ", "))
			}
			s.appendTimeline("team.agent.stage_violation", map[string]any{
				"agent_id":   agentID,
				"task_id":    taskID,
				"task_type":  taskType,
				"violations": violations,
			})
			s.appendRecovery(errMsg)
			s.setActivity(agentID, "failed", "stage violation", taskID)
			s.persist()
			s.emit("team.frame.snapshot", s.Snapshot())
			return "", fmt.Errorf("%s", errMsg)
		}
	}
	result := strings.TrimSpace(text.String())
	if result != "" && !isLeader {
		artifactRefs := s.extractArtifacts(agentID, taskType, result, expectedArtifacts)
		s.appendDialogue(DialogueEntry{
			FromAgent: agentID, ToAgent: []string{from}, Kind: "response",
			Summary: firstNonEmpty(taskType, "response"), Content: result,
			ArtifactRefs: artifactRefs, TaskID: taskID,
		})
	} else if result == "" {
		errMsg := fmt.Sprintf("agent %s completed without visible output", agentID)
		if len(eventErrors) > 0 {
			errMsg += ": " + strings.Join(eventErrors, " | ")
		}
		s.appendTimeline("team.agent.completed_without_text", map[string]any{
			"agent_id":  agentID,
			"task_id":   taskID,
			"task_type": taskType,
			"error":     errMsg,
		})
		s.appendRecovery(errMsg)
		s.setActivity(agentID, "failed", "empty response", taskID)
		s.persist()
		s.emit("team.frame.snapshot", s.Snapshot())
		return "", fmt.Errorf("%s", errMsg)
	}
	s.setActivity(agentID, "completed", firstNonEmpty(taskType, "completed"), taskID)
	s.persist()
	s.emit("team.frame.snapshot", s.Snapshot())
	return result, nil
}

type workspaceFileStamp struct {
	size    int64
	modTime int64
	isDir   bool
}

func (s *Session) hasRecordedContract() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.contract != nil
}

func (s *Session) TeamContext(from string, input GetTeamContextInput) TeamContextView {
	limit := input.RecentDialogueLimit
	if limit <= 0 {
		limit = 8
	}
	if limit > 20 {
		limit = 20
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	artifacts := append([]Artifact(nil), s.artifacts...)
	verdicts := cloneGateVerdicts(s.gateVerdicts)
	rows := make([]ActivityRow, 0, len(s.activity))
	for _, row := range s.activity {
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].AgentID < rows[j].AgentID
	})
	tasks := make([]TeamTask, 0, len(s.a2aTasks))
	for _, task := range s.a2aTasks {
		task.done = nil
		if len(task.Result) > 1200 {
			task.Result = truncateTeamVisible(task.Result, 1200)
		}
		tasks = append(tasks, task)
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i].ID == tasks[j].ID {
			return tasks[i].To < tasks[j].To
		}
		return tasks[i].ID < tasks[j].ID
	})
	var recent []DialogueEntry
	if input.IncludeRecentDialogue && len(s.dialogue) > 0 {
		start := len(s.dialogue) - limit
		if start < 0 {
			start = 0
		}
		recent = append([]DialogueEntry(nil), s.dialogue[start:]...)
	}
	return TeamContextView{
		TeamSessionID:   s.ID,
		ParentSessionID: s.ParentSessionID,
		TeamName:        s.Spec.Name,
		CurrentAgent:    from,
		CWD:             s.Config.CWD,
		RuntimeDir:      s.Config.ProjectRuntimeDir,
		TeamRuntimeDir:  s.teamRuntimeDir(),
		Contract:        cloneContract(s.contract),
		Artifacts:       artifacts,
		GateVerdicts:    verdicts,
		ActivityRows:    rows,
		A2ATasks:        tasks,
		RecentDialogue:  recent,
	}
}

func (s *Session) teamRuntimeDir() string {
	if s == nil {
		return ""
	}
	root := s.Config.ProjectPaths.TeamsDir
	if root == "" {
		root = filepath.Join(s.Config.ProjectRuntimeDir, "teams")
	}
	return filepath.Join(root, s.Spec.Name, s.ID)
}

func (s *Session) matchTaskPolicies(targets []string, taskType string, beforeContract bool) []TeamTaskPolicySpec {
	taskType = strings.ToLower(strings.TrimSpace(taskType))
	targets = normalizeTargets(targets)
	var matched []TeamTaskPolicySpec
	for _, policy := range s.Spec.TaskPolicies {
		if policy.BeforeContract && !beforeContract {
			continue
		}
		if !policyMatchesTargets(policy, targets) {
			continue
		}
		if !policyMatchesTaskType(policy, taskType) {
			continue
		}
		matched = append(matched, policy)
	}
	return matched
}

func policyMatchesTargets(policy TeamTaskPolicySpec, targets []string) bool {
	if len(policy.Targets) == 0 || len(targets) == 0 {
		return true
	}
	for _, target := range targets {
		for _, pattern := range policy.Targets {
			if matchPolicyPattern(strings.ToLower(strings.TrimSpace(pattern)), strings.ToLower(strings.TrimSpace(target))) {
				return true
			}
		}
	}
	return false
}

func policyMatchesTaskType(policy TeamTaskPolicySpec, taskType string) bool {
	if len(policy.TaskTypes) == 0 {
		return true
	}
	for _, pattern := range policy.TaskTypes {
		if matchPolicyPattern(strings.ToLower(strings.TrimSpace(pattern)), taskType) {
			return true
		}
	}
	return false
}

func matchPolicyPattern(pattern, value string) bool {
	if pattern == "" {
		return value == ""
	}
	if pattern == "*" || pattern == "all" {
		return true
	}
	if ok := matchSlashGlob(pattern, value); ok {
		return true
	}
	return pattern == value
}

func teamTaskStageGuard(agentID, taskType string, expectedArtifacts []string, policies []TeamTaskPolicySpec) string {
	expected := strings.Join(nonEmptyStrings(expectedArtifacts), ", ")
	if expected == "" {
		expected = "(none declared)"
	}
	if len(policies) == 0 {
		return "Stage guard:\n- Follow the Team shared process and stay within this task type, role boundary, and expected artifact list."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Stage guard:\n- Agent: %s\n- Task type: %s\n- Expected artifacts: %s\n", agentID, firstNonEmpty(taskType, "(unspecified)"), expected)
	for _, policy := range policies {
		fmt.Fprintf(&b, "- Active policy: %s", firstNonEmpty(policy.Name, "(unnamed)"))
		if strings.TrimSpace(policy.Description) != "" {
			fmt.Fprintf(&b, " - %s", strings.TrimSpace(policy.Description))
		}
		b.WriteString("\n")
		if policy.RequiresContract {
			b.WriteString("  - This task requires a recorded team contract before dispatch.\n")
		}
		if policy.ExclusiveWorkspace {
			b.WriteString("  - This task has exclusive workspace access while it runs; do not assume other agents can concurrently mutate project files.\n")
		}
		if policy.RestrictWritesToExpectedArtifacts {
			b.WriteString("  - Writes are restricted to the expected artifacts plus explicitly allowed write globs.\n")
		}
		if len(policy.AllowedWriteGlobs) > 0 {
			fmt.Fprintf(&b, "  - Allowed write globs: %s\n", strings.Join(policy.AllowedWriteGlobs, ", "))
		}
		if len(policy.DeniedWriteGlobs) > 0 {
			fmt.Fprintf(&b, "  - Denied write globs: %s\n", strings.Join(policy.DeniedWriteGlobs, ", "))
		}
	}
	b.WriteString("- Stay within the active policy. If the requested work conflicts with the policy, report the conflict instead of bypassing it.")
	return b.String()
}

func shouldAuditTaskWrites(policies []TeamTaskPolicySpec) bool {
	for _, policy := range policies {
		if policy.AuditWrites || policy.RestrictWritesToExpectedArtifacts || len(policy.AllowedWriteGlobs) > 0 || len(policy.DeniedWriteGlobs) > 0 {
			return true
		}
	}
	return false
}

func requiresExclusiveWorkspace(policies []TeamTaskPolicySpec) bool {
	for _, policy := range policies {
		if policy.ExclusiveWorkspace {
			return true
		}
	}
	return false
}

func (s *Session) invalidateGateVerdictsForAgent(agentID, reason string) []string {
	agentID = strings.TrimSpace(agentID)
	if s == nil || agentID == "" {
		return nil
	}
	var invalidated []string
	s.mu.Lock()
	for name, verdict := range s.gateVerdicts {
		if verdict.AgentID != agentID {
			continue
		}
		delete(s.gateVerdicts, name)
		invalidated = append(invalidated, name)
		delete(s.gate, name)
	}
	s.mu.Unlock()
	invalidated = compactSortedStrings(invalidated)
	if len(invalidated) > 0 {
		s.appendTimeline("team.gate.invalidated", map[string]any{
			"agent_id": agentID,
			"gates":    invalidated,
			"reason":   reason,
		})
	}
	return invalidated
}

func snapshotWorkspaceFiles(root string) map[string]workspaceFileStamp {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}
	out := map[string]workspaceFileStamp{}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			switch name {
			case ".git", apppaths.ProjectLocalDirNameLower, apppaths.ProjectLocalDirName, "node_modules", ".venv", "venv", "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache", "dist", "build":
				if path != root {
					return filepath.SkipDir
				}
			}
			if path != root {
				if info, err := d.Info(); err == nil {
					if rel, err := filepath.Rel(root, path); err == nil {
						out[filepath.ToSlash(rel)+"/"] = workspaceFileStamp{modTime: info.ModTime().UnixNano(), isDir: true}
					}
				}
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		out[filepath.ToSlash(rel)] = workspaceFileStamp{size: info.Size(), modTime: info.ModTime().UnixNano()}
		return nil
	})
	return out
}

func taskWriteViolations(root string, before, after map[string]workspaceFileStamp, expectedArtifacts []string, policies []TeamTaskPolicySpec) []string {
	if len(after) == 0 {
		return nil
	}
	allowedExact := allowedExpectedArtifactSet(root, expectedArtifacts)
	allowGlobs := collectPolicyGlobs(policies, true)
	denyGlobs := collectPolicyGlobs(policies, false)
	restrictToAllowList := len(allowGlobs) > 0 || restrictWritesToExpectedArtifacts(policies)
	var violations []string
	for rel, stamp := range after {
		prev, existed := before[rel]
		if existed && prev == stamp {
			continue
		}
		if matchesAnySlashGlob(denyGlobs, rel) {
			violations = append(violations, rel)
			continue
		}
		if isAllowedExpectedArtifact(rel, allowedExact) || matchesAnySlashGlob(allowGlobs, rel) {
			continue
		}
		if stamp.isDir {
			continue
		}
		if restrictToAllowList {
			violations = append(violations, rel)
		}
	}
	sort.Strings(violations)
	if len(violations) > 12 {
		violations = append(violations[:12], fmt.Sprintf("... %d more", len(violations)-12))
	}
	return violations
}

func taskWriteAttemptViolations(root string, writtenPaths []string, expectedArtifacts []string, policies []TeamTaskPolicySpec) []string {
	if len(writtenPaths) == 0 {
		return nil
	}
	allowedExact := allowedExpectedArtifactSet(root, expectedArtifacts)
	allowGlobs := collectPolicyGlobs(policies, true)
	denyGlobs := collectPolicyGlobs(policies, false)
	restrictToAllowList := len(allowGlobs) > 0 || restrictWritesToExpectedArtifacts(policies)
	var violations []string
	for _, path := range writtenPaths {
		rel, ok := workspaceRelPath(root, path)
		if !ok {
			continue
		}
		if matchesAnySlashGlob(denyGlobs, rel) {
			violations = append(violations, rel)
			continue
		}
		if isAllowedExpectedArtifact(rel, allowedExact) || matchesAnySlashGlob(allowGlobs, rel) {
			continue
		}
		if restrictToAllowList {
			violations = append(violations, rel)
		}
	}
	violations = compactSortedStrings(violations)
	if len(violations) > 12 {
		violations = append(violations[:12], fmt.Sprintf("... %d more", len(violations)-12))
	}
	return violations
}

func compactSortedStrings(values []string) []string {
	values = nonEmptyStrings(values)
	sort.Strings(values)
	if len(values) < 2 {
		return values
	}
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func restrictWritesToExpectedArtifacts(policies []TeamTaskPolicySpec) bool {
	for _, policy := range policies {
		if policy.RestrictWritesToExpectedArtifacts {
			return true
		}
	}
	return false
}

func workspaceRelPath(root, path string) (string, bool) {
	root = strings.TrimSpace(root)
	path = strings.TrimSpace(path)
	if root == "" || path == "" {
		return "", false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return filepath.ToSlash(filepath.Clean(rel)), true
}

func allowedExpectedArtifactSet(root string, expectedArtifacts []string) map[string]bool {
	allowed := map[string]bool{}
	for _, artifact := range expectedArtifacts {
		artifact = strings.TrimSpace(artifact)
		if artifact == "" {
			continue
		}
		artifact = strings.Trim(artifact, "`\"'")
		if filepath.IsAbs(artifact) {
			if rel, err := filepath.Rel(root, artifact); err == nil {
				artifact = rel
			}
		}
		allowed[filepath.ToSlash(filepath.Clean(artifact))] = true
	}
	return allowed
}

func (s *Session) expectedArtifactsForWorkspaceAudit(expectedArtifacts []string) []string {
	out := append([]string(nil), expectedArtifacts...)
	if s == nil {
		return out
	}
	cwd := strings.TrimSpace(s.Config.CWD)
	if cwd == "" {
		return out
	}
	s.mu.Lock()
	projectRoot := ""
	if s.contract != nil {
		projectRoot = strings.TrimSpace(s.contract.ProjectRoot)
	}
	s.mu.Unlock()
	if projectRoot == "" {
		return out
	}
	if !filepath.IsAbs(projectRoot) {
		projectRoot = filepath.Join(cwd, projectRoot)
	}
	projectRoot = filepath.Clean(projectRoot)
	cwd = filepath.Clean(cwd)
	projectRel, err := filepath.Rel(cwd, projectRoot)
	if err != nil || projectRel == "." || strings.HasPrefix(projectRel, "..") {
		return out
	}
	projectRel = filepath.ToSlash(projectRel)
	seen := map[string]bool{}
	for _, artifact := range out {
		seen[filepath.ToSlash(filepath.Clean(strings.TrimSpace(artifact)))] = true
	}
	for _, artifact := range expectedArtifacts {
		artifact = strings.TrimSpace(artifact)
		if artifact == "" || filepath.IsAbs(artifact) {
			continue
		}
		clean := filepath.ToSlash(filepath.Clean(strings.Trim(artifact, "`\"'")))
		if clean == "." || strings.HasPrefix(clean, projectRel+"/") {
			continue
		}
		expanded := filepath.ToSlash(filepath.Join(projectRel, clean))
		if !seen[expanded] {
			out = append(out, expanded)
			seen[expanded] = true
		}
	}
	return out
}

func isAllowedExpectedArtifact(rel string, allowed map[string]bool) bool {
	rel = filepath.ToSlash(filepath.Clean(rel))
	if allowed[rel] {
		return true
	}
	base := filepath.Base(rel)
	return allowed[base]
}

func collectPolicyGlobs(policies []TeamTaskPolicySpec, allowed bool) []string {
	var globs []string
	for _, policy := range policies {
		if allowed {
			globs = append(globs, policy.AllowedWriteGlobs...)
		} else {
			globs = append(globs, policy.DeniedWriteGlobs...)
		}
	}
	return nonEmptyStrings(globs)
}

func matchesAnySlashGlob(patterns []string, rel string) bool {
	for _, pattern := range patterns {
		if matchSlashGlob(pattern, rel) {
			return true
		}
	}
	return false
}

func matchSlashGlob(pattern, rel string) bool {
	pattern = filepath.ToSlash(filepath.Clean(strings.TrimSpace(pattern)))
	rel = filepath.ToSlash(filepath.Clean(strings.TrimSpace(rel)))
	if pattern == "." || pattern == "" {
		return rel == "." || rel == ""
	}
	if pattern == "*" || pattern == "**" {
		return true
	}
	if pattern == rel {
		return true
	}
	ok, err := filepath.Match(pattern, rel)
	if err == nil && ok {
		return true
	}
	regex := slashGlobRegex(pattern)
	ok, err = regexp.MatchString(regex, rel)
	return err == nil && ok
}

func slashGlobRegex(pattern string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		ch := pattern[i]
		switch ch {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	b.WriteString("$")
	return b.String()
}

func (s *Session) RecordContract(from string, input AcceptanceContract) string {
	if from != s.Spec.EntryAgent {
		return "Only the Team Leader can record the team acceptance contract."
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	input.UserRequirements = nonEmptyStrings(input.UserRequirements)
	input.ComponentBoundaries = nonEmptyStrings(input.ComponentBoundaries)
	input.RequiredArtifacts = nonEmptyStrings(input.RequiredArtifacts)
	input.CompletionCriteria = nonEmptyStrings(input.CompletionCriteria)
	if input.CreatedAt == "" {
		input.CreatedAt = now
	}
	input.UpdatedAt = now
	s.mu.Lock()
	if s.contract != nil {
		input = mergeAcceptanceContract(*s.contract, input)
		input.UpdatedAt = now
	}
	s.contract = cloneContract(&input)
	s.mu.Unlock()
	s.registerExistingArtifacts(from, input.RequiredArtifacts)
	s.appendTimeline("team.contract.recorded", input)
	s.appendDialogue(DialogueEntry{
		FromAgent: from,
		ToAgent:   []string{"team"},
		Kind:      "contract",
		Summary:   "Acceptance contract recorded",
		Content:   contractDialogueSummary(input),
	})
	s.persist()
	s.emit("team.frame.snapshot", s.Snapshot())
	return "Team acceptance contract recorded."
}

func (s *Session) registerExistingArtifacts(owner string, artifacts []string) {
	var changed bool
	for _, artifact := range nonEmptyStrings(artifacts) {
		if path, ok := s.resolveExpectedArtifactPath(artifact); ok {
			before := len(s.Artifacts())
			s.recordArtifact(owner, s.artifactDisplayName(artifact, path), firstLineFromFile(path), path)
			if len(s.Artifacts()) != before {
				changed = true
			}
		}
	}
	if changed {
		s.persist()
	}
}

func (s *Session) isGateAgent(agentID string) bool {
	for _, check := range s.Spec.Gates.Checks {
		if strings.TrimSpace(check.Agent) == agentID {
			return true
		}
	}
	return false
}

func (s *Session) gateCheckForRoleOrAgent(role, from string) (TeamGateCheckSpec, bool) {
	role = strings.TrimSpace(role)
	from = strings.TrimSpace(from)
	for _, check := range s.Spec.Gates.Checks {
		if check.Name == role || check.Agent == role || check.Agent == from {
			if check.Agent == from {
				return check, true
			}
		}
	}
	return TeamGateCheckSpec{}, false
}

func (s *Session) validateGateVerdictInput(from, role string, verdict GateVerdict) (bool, string) {
	check, ok := s.gateCheckForRoleOrAgent(role, from)
	if !ok {
		return false, fmt.Sprintf("%s is not a configured gate for agent %s.", role, from)
	}
	if check.Agent != from {
		return false, fmt.Sprintf("gate %s must be submitted by agent %s, not %s.", check.Name, check.Agent, from)
	}
	status := strings.TrimSpace(verdict.Status)
	if status == "" {
		return false, "status must not be empty."
	}
	if !statusIn(status, check.AllowedStatuses) {
		return false, fmt.Sprintf("status must be one of: %s.", strings.Join(check.AllowedStatuses, ", "))
	}
	if statusIn(status, check.EvidenceRequiredStatuses) && len(verdict.Evidence) == 0 {
		return false, fmt.Sprintf("gate %s status %s requires evidence.", check.Name, status)
	}
	if statusIn(status, check.FindingsRequiredStatuses) && len(verdict.Findings) == 0 {
		return false, fmt.Sprintf("gate %s status %s requires findings.", check.Name, status)
	}
	return true, ""
}

func statusIn(status string, allowed []string) bool {
	status = strings.TrimSpace(status)
	for _, item := range allowed {
		if status == strings.TrimSpace(item) {
			return true
		}
	}
	return false
}

func (s *Session) SubmitGateVerdict(from string, input GateVerdict) string {
	role := strings.TrimSpace(input.Role)
	if role == "" {
		role = from
	}
	check, ok := s.gateCheckForRoleOrAgent(role, from)
	if !ok {
		return fmt.Sprintf("Cannot submit gate verdict: %s is not configured as a gate for agent %s.", role, from)
	}
	if ok, msg := s.validateGateVerdictInput(from, check.Name, input); !ok {
		return "Cannot submit gate verdict: " + msg
	}
	if missing := s.missingActiveA2AExpectedArtifacts(from); len(missing) > 0 {
		return "Cannot submit gate verdict: missing expected artifacts for current A2A task: " + strings.Join(missing, ", ")
	}
	input.Role = check.Name
	input.AgentID = from
	input.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	s.mu.Lock()
	if s.gateVerdicts == nil {
		s.gateVerdicts = map[string]GateVerdict{}
	}
	if previous, ok := s.gateVerdicts[check.Name]; ok {
		input = mergeGateVerdict(previous, input)
	}
	s.gateVerdicts[check.Name] = input
	if s.gate == nil {
		s.gate = GateStatus{}
	}
	s.gate[check.Name] = input.Status
	s.mu.Unlock()
	s.appendTimeline("team.gate.verdict", input)
	s.appendDialogue(DialogueEntry{
		FromAgent: from,
		ToAgent:   []string{s.Spec.EntryAgent},
		Kind:      "gate_verdict",
		Summary:   fmt.Sprintf("%s verdict: %s", strings.ToUpper(check.Name), input.Status),
		Content:   gateVerdictDialogueSummary(input),
	})
	s.persist()
	s.emit("team.frame.snapshot", s.Snapshot())
	s.finishActiveA2ATask(from, "completed", gateVerdictDialogueSummary(input), "")
	return "Gate verdict recorded."
}

func (s *Session) missingActiveA2AExpectedArtifacts(agentID string) []string {
	s.mu.Lock()
	activeKey := s.agentActiveA2A[agentID]
	task, ok := s.a2aTasks[activeKey]
	var expected []string
	if ok && !isTerminalA2AStatus(task.Status) {
		expected = append(expected, task.ExpectedArtifacts...)
	}
	if len(expected) == 0 {
		s.mu.Unlock()
		return nil
	}
	missing := s.missingRequiredArtifactsLocked(uniqueStrings(expected))
	s.mu.Unlock()
	return missing
}

func (s *Session) CompleteTask(from string, input CompleteTeamTaskInput) string {
	if from != s.Spec.EntryAgent {
		return "Only the Team Leader can complete the team task."
	}
	if err := s.validateCompletion(input); err != nil {
		return "Cannot complete: " + err.Error()
	}
	if s.Spec.Loop.RequireFinalArtifact && len(input.RequiredArtifacts) == 0 {
		return "Cannot complete: this team requires at least one final artifact."
	}
	s.mu.Lock()
	missing := s.missingRequiredArtifactsLocked(input.RequiredArtifacts)
	s.mu.Unlock()
	if len(missing) > 0 {
		return "Cannot complete: missing required artifacts: " + strings.Join(missing, ", ")
	}
	exported, err := s.exportCompletionArtifacts(input.RequiredArtifacts)
	if err != nil {
		return "Cannot complete: export final artifacts: " + err.Error()
	}
	s.mu.Lock()
	s.completed = true
	finalAnswer := appendExportSummary(input.FinalAnswer, exported)
	s.finalAnswer = finalAnswer
	if statuses := completionGateStatuses(input); len(statuses) > 0 {
		s.gate = cloneGateStatus(statuses)
	}
	if row, ok := s.activity[from]; ok {
		row.Status = "completed"
		row.Summary = firstNonEmpty(input.Summary, "Team task complete")
		s.activity[from] = row
	}
	s.mu.Unlock()
	s.appendDialogue(DialogueEntry{
		FromAgent: from, ToAgent: []string{"user"}, Kind: "final",
		Summary: firstNonEmpty(input.Summary, "Team task complete"),
		Content: finalAnswer, ArtifactRefs: append(input.RequiredArtifacts, exported...),
	})
	s.appendTimeline("loop.completed", map[string]any{"summary": input.Summary, "gate_statuses": completionGateStatuses(input), "deferral_reasons": input.DeferralReasons, "exported_artifacts": exported})
	s.persist()
	return "Team task marked complete."
}

func (s *Session) exportCompletionArtifacts(required []string) ([]string, error) {
	if !s.Spec.Output.ExportToWorkdir {
		return nil, nil
	}
	artifacts := nonEmptyStrings(s.Spec.Output.Artifacts)
	if len(artifacts) == 0 {
		artifacts = nonEmptyStrings(required)
	}
	if len(artifacts) == 0 {
		return nil, nil
	}
	dirName := strings.TrimSpace(s.Spec.Output.Directory)
	if dirName == "" {
		dirName = s.Spec.Name + "-" + s.ID
	}
	dirName = strings.ReplaceAll(dirName, "{team_session_id}", s.ID)
	dirName = strings.ReplaceAll(dirName, "{team_name}", s.Spec.Name)
	outDir := filepath.Clean(filepath.Join(s.Config.CWD, dirName))
	if !pathInsideRoot(outDir, s.Config.CWD) {
		return nil, fmt.Errorf("output directory escapes working directory: %s", outDir)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	var exported []string
	for _, name := range artifacts {
		src, ok := s.resolveRuntimeArtifactPath(name)
		if !ok {
			return nil, fmt.Errorf("missing export artifact %s", name)
		}
		dst := filepath.Join(outDir, filepath.ToSlash(filepath.Clean(strings.Trim(strings.TrimSpace(name), "`\"'"))))
		if !pathInsideRoot(dst, outDir) {
			return nil, fmt.Errorf("export artifact escapes output directory: %s", name)
		}
		if err := copyArtifactPath(src, dst); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		display := filepath.ToSlash(mustRelOrBase(s.Config.CWD, dst))
		exported = append(exported, display)
		s.recordArtifact(s.Spec.EntryAgent, display, firstLineFromExport(dst), dst)
	}
	if len(exported) > 0 {
		s.appendTimeline("team.artifacts.exported", map[string]any{"output_dir": outDir, "artifacts": exported})
		s.persist()
	}
	return exported, nil
}

func (s *Session) resolveRuntimeArtifactPath(name string) (string, bool) {
	name = strings.Trim(strings.TrimSpace(name), "`\"'")
	if name == "" {
		return "", false
	}
	if filepath.IsAbs(name) {
		if pathExists(name) {
			return filepath.Clean(name), true
		}
		return "", false
	}
	for _, root := range []string{s.teamRuntimeDir(), s.Config.CWD} {
		if root == "" {
			continue
		}
		candidate := filepath.Join(root, name)
		if pathExists(candidate) {
			return filepath.Clean(candidate), true
		}
	}
	for _, artifact := range s.artifacts {
		if artifact.Name == name || artifact.ID == name || filepath.ToSlash(artifact.Name) == filepath.ToSlash(name) {
			if pathExists(artifact.Path) {
				return filepath.Clean(artifact.Path), true
			}
		}
	}
	return "", false
}

func copyArtifactPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return copyDir(src, dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, info.Mode().Perm())
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			info, statErr := d.Info()
			if statErr != nil {
				return statErr
			}
			return os.MkdirAll(target, info.Mode().Perm())
		}
		info, statErr := d.Info()
		if statErr != nil {
			return statErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if mkdirErr := os.MkdirAll(filepath.Dir(target), 0o755); mkdirErr != nil {
			return mkdirErr
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func firstLineFromExport(path string) string {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return ""
	}
	return firstLineFromFile(path)
}

func appendExportSummary(answer string, exported []string) string {
	if len(exported) == 0 {
		return answer
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(answer))
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString("已导出到工作目录：\n")
	for _, path := range exported {
		b.WriteString("- ")
		b.WriteString(path)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func (s *Session) validateCompletion(input CompleteTeamTaskInput) error {
	s.mu.Lock()
	contract := cloneContract(s.contract)
	verdicts := cloneGateVerdicts(s.gateVerdicts)
	gates := s.Spec.Gates
	s.mu.Unlock()
	if gates.RequireContract && contract == nil {
		return fmt.Errorf("missing Team Acceptance Contract; call RecordTeamContract first")
	}
	var required []string
	if contract != nil {
		required = append(required, contract.RequiredArtifacts...)
	}
	for _, check := range gates.Checks {
		verdict, ok := verdicts[check.Name]
		if !ok {
			return fmt.Errorf("missing structured gate verdict for %s from %s; that agent must call SubmitGateVerdict", check.Name, check.Agent)
		}
		wantStatus := completionGateStatus(input, check.Name)
		if strings.TrimSpace(wantStatus) == "" {
			return fmt.Errorf("gate_statuses[%s] is required for configured gate agent %s", check.Name, check.Agent)
		}
		if wantStatus != verdict.Status {
			return fmt.Errorf("gate_statuses[%s] %q does not match latest gate verdict %q", check.Name, wantStatus, verdict.Status)
		}
		if !statusIn(verdict.Status, check.PassStatuses) {
			return fmt.Errorf("gate %s is not in pass statuses (%s): %s", check.Name, strings.Join(check.PassStatuses, ", "), verdict.Status)
		}
		if err := validateGateFindings(check.Name, verdict, gates, input.DeferralReasons); err != nil {
			return err
		}
		if statusIn(verdict.Status, check.EvidenceRequiredStatuses) && contract != nil {
			if missing := missingRequiredEvidence(contract, verdict.Evidence, gateNames(gates.Checks)); len(missing) > 0 {
				return fmt.Errorf("gate %s evidence missing required contract checks: %s", check.Name, strings.Join(missing, ", "))
			}
		}
	}
	required = append(required, input.RequiredArtifacts...)
	s.mu.Lock()
	missingArtifacts := s.missingRequiredArtifactsLocked(uniqueStrings(required))
	s.mu.Unlock()
	if len(missingArtifacts) > 0 {
		return fmt.Errorf("missing required contract artifacts: %s", strings.Join(missingArtifacts, ", "))
	}
	return nil
}

func completionGateStatuses(input CompleteTeamTaskInput) GateStatus {
	statuses := GateStatus{}
	for k, v := range input.GateStatuses {
		if strings.TrimSpace(k) != "" && strings.TrimSpace(v) != "" {
			statuses[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return statuses
}

func completionGateStatus(input CompleteTeamTaskInput, name string) string {
	return completionGateStatuses(input)[name]
}

func validateGateFindings(label string, verdict GateVerdict, gates TeamGateSpec, deferrals map[string]string) error {
	for _, finding := range verdict.Findings {
		if finding.Blocking {
			return fmt.Errorf("%s has blocking finding: %s", label, finding.Summary)
		}
		if isResolvedGateFinding(finding) {
			continue
		}
		if gates.NonblockingFindings == "require_followup_or_deferral" {
			reason := gateFindingDeferralReason(label, finding, deferrals)
			if reason == "" && gates.DeferralRequiresReason {
				return fmt.Errorf("%s has nonblocking finding without follow-up or deferral reason: %s", label, finding.Summary)
			}
		}
	}
	return nil
}

func isResolvedGateFinding(finding GateFinding) bool {
	text := strings.ToLower(strings.TrimSpace(finding.Summary + " " + finding.Details))
	if text == "" {
		return false
	}
	resolvedMarkers := []string{
		"fixed",
		"verified",
		"resolved",
		"confirmed",
		"validated",
		"no regression",
		"well-supported",
		"appropriately hedged",
		"no overextension",
		"not misleading",
		"credible",
		"meets threshold",
		"已修复",
		"已验证",
		"验证通过",
		"已解决",
		"修复确认",
		"无回归",
		"证据充分",
		"充分支撑",
		"适当保守",
		"未超出证据",
		"可信",
	}
	for _, marker := range resolvedMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func gateFindingDeferralReason(label string, finding GateFinding, deferrals map[string]string) string {
	if len(deferrals) == 0 {
		return ""
	}
	keys := []string{
		label + ":" + finding.Category + ":" + finding.Summary,
		finding.Category + ":" + finding.Summary,
		label + ":" + finding.Category,
		finding.Category,
		finding.Summary,
	}
	normalized := map[string]string{}
	for key, value := range deferrals {
		normalized[normalizeDeferralKey(key)] = strings.TrimSpace(value)
	}
	for _, key := range keys {
		want := normalizeDeferralKey(key)
		if reason := normalized[want]; reason != "" {
			return reason
		}
		for actual, reason := range normalized {
			if reason != "" && deferralKeysMatch(want, actual) {
				return reason
			}
		}
	}
	return ""
}

func normalizeDeferralKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.Join(strings.Fields(key), " ")
	return key
}

func deferralKeysMatch(want, actual string) bool {
	want = normalizeDeferralKey(want)
	actual = normalizeDeferralKey(actual)
	if want == "" || actual == "" {
		return false
	}
	if want == actual || strings.Contains(want, actual) || strings.Contains(actual, want) {
		return true
	}
	wantTokens := significantDeferralTokens(want)
	actualTokens := significantDeferralTokens(actual)
	if len(wantTokens) < 4 || len(actualTokens) < 4 {
		return false
	}
	actualSet := map[string]struct{}{}
	for _, token := range actualTokens {
		actualSet[token] = struct{}{}
	}
	matched := 0
	for _, token := range wantTokens {
		if _, ok := actualSet[token]; ok {
			matched++
		}
	}
	required := int(math.Ceil(float64(len(wantTokens)) * 0.75))
	if required < 4 {
		required = 4
	}
	return matched >= required
}

func significantDeferralTokens(value string) []string {
	replacer := strings.NewReplacer(
		":", " ", ";", " ", ",", " ", ".", " ", "(", " ", ")", " ",
		"`", " ", "'", " ", "\"", " ", "—", " ", "-", " ", "_", " ",
		"/", " ", "\\", " ",
	)
	value = replacer.Replace(strings.ToLower(value))
	var out []string
	stop := map[string]struct{}{
		"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "but": {}, "for": {}, "has": {}, "in": {}, "is": {}, "it": {}, "of": {}, "or": {}, "the": {}, "to": {}, "use": {}, "uses": {}, "with": {},
	}
	for _, token := range strings.Fields(value) {
		if len([]rune(token)) < 2 {
			continue
		}
		if _, ok := stop[token]; ok {
			continue
		}
		out = append(out, token)
	}
	return out
}

func (s *Session) missingRequiredArtifactsLocked(required []string) []string {
	have := map[string]struct{}{}
	for _, artifact := range s.artifacts {
		have[artifact.Name] = struct{}{}
		have[artifact.ID] = struct{}{}
		if artifact.Path != "" {
			have[artifact.Path] = struct{}{}
		}
	}
	var missing []string
	for _, name := range required {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := have[name]; ok {
			continue
		}
		if s.requiredArtifactFileExists(name) {
			continue
		}
		missing = append(missing, name)
	}
	return missing
}

func (s *Session) requiredArtifactFileExists(name string) bool {
	if filepath.IsAbs(name) {
		return pathExists(name)
	}
	if dir := s.teamRuntimeDir(); dir != "" && pathExists(filepath.Join(dir, name)) {
		return true
	}
	if pathExists(filepath.Join(s.Config.CWD, name)) {
		return true
	}
	if s.requiredArtifactExistsInNamedProjectRoot(name) {
		return true
	}
	for _, artifact := range s.artifacts {
		if artifact.Path == "" {
			continue
		}
		base := filepath.Dir(artifact.Path)
		if fileExists(filepath.Join(base, name)) {
			return true
		}
	}
	return false
}

func pathExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func (s *Session) requiredArtifactExistsInNamedProjectRoot(name string) bool {
	cwd := strings.TrimSpace(s.Config.CWD)
	if cwd == "" {
		return false
	}
	matches, err := filepath.Glob(filepath.Join(cwd, "*", name))
	if err != nil {
		return false
	}
	for _, match := range matches {
		rel, err := filepath.Rel(cwd, match)
		if err != nil || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
			continue
		}
		if fileExists(match) {
			return true
		}
	}
	return false
}

func contractDialogueSummary(contract AcceptanceContract) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Project root: %s\n", contract.ProjectRoot)
	if contract.IntegrationContract != "" {
		fmt.Fprintf(&b, "Integration: %s\n", contract.IntegrationContract)
	}
	if len(contract.RequiredArtifacts) > 0 {
		fmt.Fprintf(&b, "Required artifacts: %s\n", strings.Join(contract.RequiredArtifacts, ", "))
	}
	var checks []string
	for _, check := range append(append([]ContractCheck(nil), contract.RequiredCommands...), contract.IntegrationSmokes...) {
		name := firstNonEmpty(check.Name, check.Command)
		if name != "" {
			checks = append(checks, name)
		}
	}
	if len(checks) > 0 {
		fmt.Fprintf(&b, "Required checks: %s\n", strings.Join(checks, ", "))
	}
	return strings.TrimSpace(b.String())
}

func gateVerdictDialogueSummary(verdict GateVerdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Status: %s\n", verdict.Status)
	if verdict.Summary != "" {
		fmt.Fprintf(&b, "%s\n", verdict.Summary)
	}
	if len(verdict.Evidence) > 0 {
		fmt.Fprintf(&b, "Evidence:\n")
		for _, item := range verdict.Evidence {
			state := "fail"
			if item.Passed {
				state = "pass"
			}
			fmt.Fprintf(&b, "- %s: %s", firstNonEmpty(item.Name, item.Command), state)
			if item.OutputSummary != "" {
				fmt.Fprintf(&b, " — %s", item.OutputSummary)
			}
			fmt.Fprintln(&b)
		}
	}
	if len(verdict.Findings) > 0 {
		fmt.Fprintf(&b, "Findings:\n")
		for _, finding := range verdict.Findings {
			level := "nonblocking"
			if finding.Blocking {
				level = "blocking"
			}
			fmt.Fprintf(&b, "- [%s] %s", level, finding.Summary)
			if finding.Details != "" {
				fmt.Fprintf(&b, " — %s", finding.Details)
			}
			fmt.Fprintln(&b)
		}
	}
	return strings.TrimSpace(b.String())
}

func gateNames(checks []TeamGateCheckSpec) map[string]struct{} {
	names := map[string]struct{}{}
	for _, check := range checks {
		for _, value := range []string{check.Name, check.Agent} {
			if key := normalizeEvidenceKey(value); key != "" {
				names[key] = struct{}{}
			}
		}
	}
	return names
}

func missingRequiredEvidence(contract *AcceptanceContract, evidence []GateEvidence, gateChecks ...map[string]struct{}) []string {
	seen := map[string]bool{}
	var passed []GateEvidence
	for _, item := range evidence {
		if !item.Passed {
			continue
		}
		passed = append(passed, item)
		for _, key := range []string{item.Name, item.Command} {
			key = normalizeEvidenceKey(key)
			if key != "" {
				seen[key] = true
			}
		}
	}
	var missing []string
	for _, check := range append(append([]ContractCheck(nil), contract.RequiredCommands...), contract.IntegrationSmokes...) {
		if !check.Required {
			continue
		}
		nameKey := normalizeEvidenceKey(check.Name)
		commandKey := normalizeEvidenceKey(check.Command)
		if contractCheckCoveredByGate(nameKey, commandKey, gateChecks...) {
			continue
		}
		if (nameKey != "" && seen[nameKey]) || (commandKey != "" && seen[commandKey]) {
			continue
		}
		if requiredEvidenceCovered(check, passed) {
			continue
		}
		missing = append(missing, firstNonEmpty(check.Name, check.Command))
	}
	return missing
}

func contractCheckCoveredByGate(nameKey, commandKey string, gateChecks ...map[string]struct{}) bool {
	for _, gates := range gateChecks {
		if len(gates) == 0 {
			continue
		}
		if nameKey != "" {
			if _, ok := gates[nameKey]; ok {
				return true
			}
		}
		if commandKey != "" {
			if _, ok := gates[commandKey]; ok {
				return true
			}
		}
	}
	return false
}

func normalizeEvidenceKey(value string) string {
	value = strings.NewReplacer("-", " ", ":", " ", "/", " ", "_", " ").Replace(value)
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func requiredEvidenceCovered(check ContractCheck, evidence []GateEvidence) bool {
	for _, item := range evidence {
		if commandEvidenceCovers(check.Command, item.Command) {
			return true
		}
		if namedEvidenceCovers(check.Name, item) {
			return true
		}
	}
	return false
}

func namedEvidenceCovers(requiredName string, item GateEvidence) bool {
	requiredName = normalizeEvidenceKey(requiredName)
	if requiredName == "" {
		return false
	}
	haystack := normalizeEvidenceKey(item.Name + " " + item.OutputSummary)
	if haystack == "" {
		return false
	}
	if strings.Contains(haystack, requiredName) {
		return true
	}
	parts := significantEvidenceNameTokens(requiredName)
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if !evidenceTokenCovered(part, haystack) {
			return false
		}
	}
	return true
}

func evidenceTokenCovered(token, haystack string) bool {
	token = normalizeEvidenceKey(token)
	if token == "" {
		return true
	}
	if strings.Contains(haystack, token) {
		return true
	}
	for _, alias := range evidenceTokenAliases(token) {
		if alias = normalizeEvidenceKey(alias); alias != "" && strings.Contains(haystack, alias) {
			return true
		}
	}
	return false
}

func evidenceTokenAliases(token string) []string {
	switch token {
	case "source", "sources":
		return []string{"source", "sources", "source id", "source ids", "source_id", "来源", "记录"}
	case "paper", "papers":
		return []string{"paper", "papers", "preprint", "preprints", "report", "reports", "论文", "预印本"}
	case "official":
		return []string{"official", "official doc", "official_doc", "regulatory", "authority", "官方", "监管"}
	case "count", "counts":
		return []string{"count", "counts", "total", "数量", "记录", "条"}
	case "consistency", "consistent":
		return []string{"consistency", "consistent", "一致", "完全一致", "无孤儿"}
	case "citation", "citations":
		return []string{"citation", "citations", "引用", "覆盖"}
	case "final":
		return []string{"final", "final report", "final-report", "最终", "报告"}
	case "report":
		return []string{"report", "reports", "报告"}
	case "evidence":
		return []string{"evidence", "证据"}
	case "matrix":
		return []string{"matrix", "矩阵"}
	case "exists", "existence", "present", "presence":
		return []string{"exists", "exist", "present", "存在", "已创建", "完整"}
	case "id", "ids":
		return []string{"id", "ids", "source id", "source ids", "source_id"}
	}
	return nil
}

func significantEvidenceNameTokens(name string) []string {
	name = strings.NewReplacer("-", " ", ":", " ", "/", " ", "_", " ").Replace(name)
	var parts []string
	for _, field := range strings.Fields(name) {
		for _, part := range splitAlphaNumericAndCJK(field) {
			part = strings.TrimSpace(part)
			part = stripGenericEvidenceWords(part)
			if part == "" || isGenericEvidenceNameToken(part) {
				continue
			}
			parts = append(parts, part)
		}
	}
	return parts
}

func stripGenericEvidenceWords(token string) string {
	for _, word := range []string{"命令", "验证", "场景", "测试", "检查", "功能", "路径"} {
		token = strings.ReplaceAll(token, word, "")
	}
	return token
}

func splitAlphaNumericAndCJK(value string) []string {
	var parts []string
	var b strings.Builder
	var lastClass int
	flush := func() {
		if b.Len() > 0 {
			parts = append(parts, b.String())
			b.Reset()
		}
	}
	for _, r := range value {
		class := evidenceRuneClass(r)
		if class == 0 {
			flush()
			lastClass = 0
			continue
		}
		if lastClass != 0 && class != lastClass {
			flush()
		}
		b.WriteRune(r)
		lastClass = class
	}
	flush()
	return parts
}

func evidenceRuneClass(r rune) int {
	switch {
	case r >= 'a' && r <= 'z':
		return 1
	case r >= '0' && r <= '9':
		return 1
	case r >= '\u4e00' && r <= '\u9fff':
		return 2
	default:
		return 0
	}
}

func isGenericEvidenceNameToken(token string) bool {
	switch token {
	case "命令", "验证", "场景", "测试", "检查", "功能", "路径", "check", "required", "exists", "existence", "present", "presence":
		return true
	}
	return false
}

func commandEvidenceCovers(requiredCommand, evidenceCommand string) bool {
	required := commandEvidenceFragments(requiredCommand)
	if len(required) == 0 {
		return false
	}
	actual := commandEvidenceFragments(evidenceCommand)
	if len(actual) == 0 {
		return false
	}
	for _, want := range required {
		found := false
		for _, got := range actual {
			if commandFragmentMatches(want, got) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func commandEvidenceFragments(command string) []string {
	command = normalizeQuotedShellArgs(strings.ToLower(strings.TrimSpace(command)))
	command = strings.NewReplacer("&&", ";", "||", ";").Replace(command)
	parts := strings.Split(command, ";")
	var out []string
	for _, part := range parts {
		fields := commandEvidenceFields(part)
		if len(fields) == 0 || isStatusCheckFragment(fields) {
			continue
		}
		out = append(out, strings.Join(fields, " "))
	}
	return out
}

func commandEvidenceFields(fragment string) []string {
	var fields []string
	for _, field := range strings.Fields(fragment) {
		field = strings.TrimSpace(field)
		if field == "" || isShellRedirectionToken(field) {
			continue
		}
		fields = append(fields, field)
	}
	return fields
}

func normalizeQuotedShellArgs(value string) string {
	var b strings.Builder
	inSingle := false
	inDouble := false
	wroteWildcard := false
	for _, r := range value {
		switch {
		case inSingle:
			if r == '\'' {
				inSingle = false
				if !wroteWildcard {
					b.WriteString(" * ")
					wroteWildcard = true
				}
			}
		case inDouble:
			if r == '"' {
				inDouble = false
				if !wroteWildcard {
					b.WriteString(" * ")
					wroteWildcard = true
				}
			}
		case r == '\'':
			inSingle = true
			wroteWildcard = false
		case r == '"':
			inDouble = true
			wroteWildcard = false
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isShellRedirectionToken(token string) bool {
	if token == ">" || token == ">>" || token == "<" {
		return true
	}
	if strings.Contains(token, ">&") || strings.Contains(token, "<&") {
		return true
	}
	if strings.HasPrefix(token, ">") || strings.HasPrefix(token, "2>") || strings.HasPrefix(token, "1>") {
		return true
	}
	return false
}

func isStatusCheckFragment(fields []string) bool {
	if len(fields) == 0 {
		return true
	}
	switch fields[0] {
	case "echo":
		for _, field := range fields[1:] {
			if strings.Contains(field, "$?") {
				return true
			}
		}
	case "test", "[":
		for _, field := range fields[1:] {
			if strings.Contains(field, "$?") {
				return true
			}
		}
	}
	return false
}

func commandFragmentMatches(required, actual string) bool {
	if required == actual {
		return true
	}
	requiredFields := strings.Fields(required)
	actualFields := strings.Fields(actual)
	if len(requiredFields) == 0 || len(actualFields) < len(requiredFields) {
		return false
	}
	for i, want := range requiredFields {
		if want == "*" || actualFields[i] == "*" {
			continue
		}
		if want != actualFields[i] {
			if literalInputArgumentMatches(requiredFields, actualFields, i) {
				continue
			}
			return false
		}
	}
	return true
}

func literalInputArgumentMatches(requiredFields, actualFields []string, index int) bool {
	if index != len(requiredFields)-1 || index != len(actualFields)-1 {
		return false
	}
	if len(requiredFields) < 2 || requiredFields[index] == "" || actualFields[index] == "" {
		return false
	}
	action := requiredFields[index-1]
	switch action {
	case "add", "create", "new":
		return !looksLikeControlArgument(requiredFields[index]) && !looksLikeControlArgument(actualFields[index])
	default:
		return false
	}
}

func looksLikeControlArgument(value string) bool {
	if value == "" || value == "*" {
		return false
	}
	if strings.HasPrefix(value, "-") {
		return true
	}
	if _, err := strconv.Atoi(value); err == nil {
		return true
	}
	lower := strings.ToLower(value)
	switch lower {
	case "true", "false", "null", "none", "abc", "nan":
		return true
	default:
		return false
	}
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (s *Session) Abort() {
	if s.cancel != nil {
		s.cancel()
	}
	for _, runtime := range s.agents {
		runtime.Engine.Abort()
	}
	s.busy.Store(false)
	s.mu.Lock()
	for agentID, row := range s.activity {
		if row.Status == "running" || row.Status == "queued" {
			row.Status = "interrupted"
			row.Summary = "interrupted by user"
			s.activity[agentID] = row
		}
	}
	s.mu.Unlock()
	s.appendTimeline("loop.interrupted_by_user", map[string]any{"source": "abort"})
	s.persist()
	s.emit("team.interrupted_by_user", s.Snapshot())
}

func (s *Session) Shutdown() {
	s.Abort()
	s.mu.Lock()
	agents := make([]*AgentRuntime, 0, len(s.agents))
	for _, runtime := range s.agents {
		agents = append(agents, runtime)
	}
	s.mu.Unlock()
	for _, runtime := range agents {
		if runtime != nil && runtime.Engine != nil {
			runtime.Engine.Shutdown()
		}
	}
}

func (s *Session) ResolvePermission(requestID, decision string) bool {
	s.permissionMu.Lock()
	ch := s.permissions[requestID]
	if ch != nil {
		delete(s.permissions, requestID)
	}
	s.permissionMu.Unlock()
	if ch == nil {
		return false
	}
	ch <- decision
	return true
}

func (s *Session) requestPermission(agentID string, event agent.StreamEvent) string {
	if s.AgentYoloEnabled(agentID) {
		return "always"
	}
	requestID := uuid.NewString()
	ch := make(chan string, 1)
	s.permissionMu.Lock()
	s.permissions[requestID] = ch
	s.permissionMu.Unlock()
	payload := map[string]any{
		"request_id":      requestID,
		"team_session_id": s.ID,
		"agent_id":        agentID,
		"agent_display":   s.agentDisplayName(agentID),
		"prompt":          permissionPromptPayload(agentID, s.agentDisplayName(agentID), event),
		"dangerous":       isTruthy(event.Metadata["dangerous"]),
	}
	if s.askPermission != nil {
		return s.askPermission(s.ParentSessionID, payload)
	}
	s.emit("permission_requested", payload)
	select {
	case decision := <-ch:
		return decision
	case <-time.After(24 * time.Hour):
		return "deny"
	}
}

func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	agents := make([]TeamAgentSpec, 0, len(s.Spec.AgentSpecs))
	agents = append(agents, s.Spec.AgentSpecs...)
	activity := make([]ActivityRow, 0, len(s.activity))
	for _, spec := range s.Spec.AgentSpecs {
		if row, ok := s.activity[spec.Name]; ok {
			activity = append(activity, row)
		}
	}
	return Snapshot{
		TeamMode:         true,
		TeamSessionID:    s.ID,
		ActiveTeamID:     s.Spec.Name,
		ActiveTeamName:   s.Spec.DisplayName,
		LoopIteration:    s.loopIteration,
		Running:          s.busy.Load(),
		WaitingForUser:   s.waitingForUser,
		Agents:           agents,
		Dialogue:         append([]DialogueEntry(nil), s.dialogue...),
		ActivityRows:     activity,
		Artifacts:        append([]Artifact(nil), s.artifacts...),
		GateStatus:       cloneGateStatus(s.gate),
		TeamContract:     cloneContract(s.contract),
		GateVerdicts:     cloneGateVerdicts(s.gateVerdicts),
		InputEnabled:     !s.busy.Load(),
		InputPlaceholder: "请输入 Team 消息并回车。",
	}
}

func cloneContract(in *AcceptanceContract) *AcceptanceContract {
	if in == nil {
		return nil
	}
	out := *in
	out.UserRequirements = append([]string(nil), in.UserRequirements...)
	out.ComponentBoundaries = append([]string(nil), in.ComponentBoundaries...)
	out.RequiredArtifacts = append([]string(nil), in.RequiredArtifacts...)
	out.RequiredCommands = append([]ContractCheck(nil), in.RequiredCommands...)
	out.IntegrationSmokes = append([]ContractCheck(nil), in.IntegrationSmokes...)
	out.CompletionCriteria = append([]string(nil), in.CompletionCriteria...)
	return &out
}

func cloneGateVerdicts(in map[string]GateVerdict) map[string]GateVerdict {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]GateVerdict, len(in))
	for key, verdict := range in {
		verdict.Evidence = append([]GateEvidence(nil), verdict.Evidence...)
		verdict.Findings = append([]GateFinding(nil), verdict.Findings...)
		out[key] = verdict
	}
	return out
}

func cloneGateStatus(in GateStatus) GateStatus {
	if len(in) == 0 {
		return GateStatus{}
	}
	out := make(GateStatus, len(in))
	for key, status := range in {
		key = strings.TrimSpace(key)
		status = strings.TrimSpace(status)
		if key != "" && status != "" {
			out[key] = status
		}
	}
	return out
}

func mergeAcceptanceContract(existing AcceptanceContract, next AcceptanceContract) AcceptanceContract {
	merged := next
	if strings.TrimSpace(merged.ProjectRoot) == "" {
		merged.ProjectRoot = existing.ProjectRoot
	}
	if strings.TrimSpace(merged.IntegrationContract) == "" {
		merged.IntegrationContract = existing.IntegrationContract
	} else if strings.TrimSpace(existing.IntegrationContract) != "" && !strings.Contains(merged.IntegrationContract, existing.IntegrationContract) {
		merged.IntegrationContract = strings.TrimSpace(existing.IntegrationContract + "\n" + merged.IntegrationContract)
	}
	if strings.TrimSpace(merged.CreatedAt) == "" {
		merged.CreatedAt = existing.CreatedAt
	}
	merged.UserRequirements = uniqueStrings(append(existing.UserRequirements, merged.UserRequirements...))
	merged.ComponentBoundaries = uniqueStrings(append(existing.ComponentBoundaries, merged.ComponentBoundaries...))
	merged.RequiredArtifacts = uniqueStrings(append(existing.RequiredArtifacts, merged.RequiredArtifacts...))
	merged.CompletionCriteria = uniqueStrings(append(existing.CompletionCriteria, merged.CompletionCriteria...))
	merged.RequiredCommands = mergeContractChecks(existing.RequiredCommands, merged.RequiredCommands)
	merged.IntegrationSmokes = mergeContractChecks(existing.IntegrationSmokes, merged.IntegrationSmokes)
	return merged
}

func mergeContractChecks(existing []ContractCheck, next []ContractCheck) []ContractCheck {
	merged := append([]ContractCheck(nil), existing...)
	index := map[string]int{}
	for i, check := range merged {
		index[contractCheckKey(check)] = i
	}
	for _, check := range next {
		key := contractCheckKey(check)
		if key == "" {
			continue
		}
		if i, ok := index[key]; ok {
			merged[i].Required = merged[i].Required || check.Required
			if merged[i].Name == "" {
				merged[i].Name = check.Name
			}
			if merged[i].Command == "" {
				merged[i].Command = check.Command
			}
			if merged[i].CWD == "" {
				merged[i].CWD = check.CWD
			}
			continue
		}
		index[key] = len(merged)
		merged = append(merged, check)
	}
	return merged
}

func contractCheckKey(check ContractCheck) string {
	return strings.Join([]string{
		normalizeEvidenceKey(check.Name),
		normalizeEvidenceKey(check.Command),
		normalizeEvidenceKey(check.CWD),
	}, "\x00")
}

func mergeGateVerdict(existing GateVerdict, next GateVerdict) GateVerdict {
	if strings.TrimSpace(existing.Status) != "" &&
		strings.TrimSpace(next.Status) != "" &&
		strings.TrimSpace(existing.Status) != strings.TrimSpace(next.Status) {
		return next
	}
	merged := next
	if strings.TrimSpace(merged.Status) == "" {
		merged.Status = existing.Status
	}
	if strings.TrimSpace(existing.Summary) != "" && strings.TrimSpace(next.Summary) != "" && existing.Summary != next.Summary {
		merged.Summary = strings.TrimSpace(existing.Summary + "\n" + next.Summary)
	} else if strings.TrimSpace(merged.Summary) == "" {
		merged.Summary = existing.Summary
	}
	merged.Evidence = mergeGateEvidence(existing.Evidence, next.Evidence)
	// Agents may submit several verdicts while issues are being fixed. Evidence is
	// cumulative, but findings describe the latest verdict so resolved blockers do
	// not poison later passes.
	merged.Findings = append([]GateFinding(nil), next.Findings...)
	if merged.CreatedAt == "" {
		merged.CreatedAt = existing.CreatedAt
	}
	return merged
}

func mergeGateEvidence(existing []GateEvidence, next []GateEvidence) []GateEvidence {
	merged := append([]GateEvidence(nil), existing...)
	index := map[string]int{}
	for i, item := range merged {
		index[gateEvidenceKey(item)] = i
	}
	for _, item := range next {
		key := gateEvidenceKey(item)
		if key == "" {
			continue
		}
		if i, ok := index[key]; ok {
			merged[i] = item
			continue
		}
		index[key] = len(merged)
		merged = append(merged, item)
	}
	return merged
}

func gateEvidenceKey(item GateEvidence) string {
	return strings.Join([]string{
		normalizeEvidenceKey(item.Name),
		normalizeEvidenceKey(item.Command),
		normalizeEvidenceKey(item.CWD),
	}, "\x00")
}

func mergeGateFindings(existing []GateFinding, next []GateFinding) []GateFinding {
	merged := append([]GateFinding(nil), existing...)
	seen := map[string]struct{}{}
	for _, item := range merged {
		seen[gateFindingKey(item)] = struct{}{}
	}
	for _, item := range next {
		key := gateFindingKey(item)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, item)
	}
	return merged
}

func gateFindingKey(item GateFinding) string {
	return strings.Join([]string{
		normalizeEvidenceKey(item.Category),
		normalizeEvidenceKey(item.Summary),
		normalizeEvidenceKey(item.Details),
	}, "\x00")
}

func (s *Session) Dialogue() []DialogueEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]DialogueEntry(nil), s.dialogue...)
}

func (s *Session) Timeline() []TimelineEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TimelineEvent(nil), s.timeline...)
}

func (s *Session) Artifacts() []Artifact {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Artifact(nil), s.artifacts...)
}

// SummaryData holds a compact summary of a team session's current state.
type SummaryData struct {
	DialogueCount  int                    `json:"dialogue_count"`
	ArtifactCount  int                    `json:"artifact_count"`
	ActivityCount  int                    `json:"activity_count"`
	LoopIteration  int                    `json:"loop_iteration"`
	GateStatus     GateStatus             `json:"gate_status"`
	GateVerdicts   map[string]GateVerdict `json:"gate_verdicts,omitempty"`
	Running        bool                   `json:"running"`
	ActiveTeamName string                 `json:"active_team_name"`
}

func (s *Session) Summary() SummaryData {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SummaryData{
		DialogueCount:  len(s.dialogue),
		ArtifactCount:  len(s.artifacts),
		ActivityCount:  len(s.activity),
		LoopIteration:  s.loopIteration,
		GateStatus:     cloneGateStatus(s.gate),
		GateVerdicts:   cloneGateVerdicts(s.gateVerdicts),
		Running:        s.busy.Load(),
		ActiveTeamName: s.Spec.DisplayName,
	}
}

func (s *Session) Detail(kind, id, name string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case "dialogue":
		for _, entry := range s.dialogue {
			if entry.ID == id || entry.TaskID == id {
				return entry, nil
			}
		}
	case "artifact":
		for _, artifact := range s.artifacts {
			if artifact.ID == id || artifact.Name == name || artifact.Name == id {
				return artifact, nil
			}
		}
	case "agent":
		if runtime := s.agents[id]; runtime != nil {
			return map[string]any{
				"agent":    runtime.Spec,
				"activity": s.activity[id],
			}, nil
		}
	case "activity", "task":
		for _, row := range s.activity {
			if row.AgentID == id || row.TaskID == id {
				return row, nil
			}
		}
	default:
		return map[string]any{
			"dialogue":  append([]DialogueEntry(nil), s.dialogue...),
			"artifacts": append([]Artifact(nil), s.artifacts...),
			"activity":  append([]ActivityRow(nil), activityRowsLocked(s)...),
		}, nil
	}
	return nil, fmt.Errorf("team detail not found")
}

func (s *Session) HandleA2A(ctx context.Context, agentID, method string, params json.RawMessage) (any, error) {
	switch method {
	case "agent.card.get":
		agent, ok := s.agents[agentID]
		if !ok {
			return nil, fmt.Errorf("agent %s not found", agentID)
		}
		return map[string]any{
			"name": agent.Spec.Name, "display_name": agent.Spec.DisplayName,
			"description": agent.Spec.Description, "communicates_with": agent.Spec.AllowedAgents,
		}, nil
	case "message/send", "message/stream":
		var input A2AMessageInput
		if err := json.Unmarshal(params, &input); err != nil {
			return nil, err
		}
		return s.SendA2AMessage(ctx, agentID, input), nil
	case "artifact/list":
		s.mu.Lock()
		defer s.mu.Unlock()
		return append([]Artifact(nil), s.artifacts...), nil
	case "artifact/get":
		var p struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, artifact := range s.artifacts {
			if artifact.ID == p.ID || artifact.Name == p.Name {
				return artifact, nil
			}
		}
		return nil, fmt.Errorf("artifact not found")
	case "tasks/get":
		return s.Snapshot(), nil
	case "tasks/cancel":
		s.Abort()
		return map[string]any{"cancelled": true}, nil
	default:
		return nil, fmt.Errorf("unknown A2A method: %s", method)
	}
}

func (s *Session) appendDialogue(entry DialogueEntry) {
	if entry.ID == "" {
		entry.ID = "dlg-" + uuid.NewString()[:10]
	}
	if entry.CreatedAt == "" {
		entry.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	s.mu.Lock()
	s.dialogue = append(s.dialogue, entry)
	s.mu.Unlock()
	s.appendTimeline("team_dialogue", entry)
	s.emit("team.dialogue.appended", entry)
	s.emit("team.frame.snapshot", s.Snapshot())
}

func (s *Session) appendRecovery(message string) {
	s.appendTimeline("loop.recovery", map[string]any{"message": message})
	s.appendDialogue(DialogueEntry{FromAgent: "team-loop", ToAgent: []string{s.Spec.EntryAgent}, Kind: "recovery", Summary: "Recovery", Content: message})
	s.emit("team.loop.recovery", s.Snapshot())
}

func formatAgentError(agentID string, event agent.StreamEvent) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("%s error: %s", agentID, event.Content))
	for _, key := range []string{"error_type", "provider", "status_code", "status", "request_url", "operation", "raw_error"} {
		if value, ok := event.Metadata[key]; ok && value != nil && fmt.Sprint(value) != "" {
			parts = append(parts, fmt.Sprintf("%s: %v", key, value))
		}
	}
	return strings.Join(parts, "\n")
}

func permissionPromptPayload(agentID, agentDisplay string, event agent.StreamEvent) map[string]any {
	payload := map[string]any{
		"agent_id":      agentID,
		"agent_display": agentDisplay,
		"tool_name":     firstNonEmpty(event.Content, toolNameFromPermissionEvent(event), "tool"),
		"risk":          stringFromMetadata(event.Metadata, "risk"),
	}
	if reason := stringFromMetadata(event.Metadata, "reason"); reason != "" {
		payload["reason"] = truncateTeamVisible(reason, 800)
	}
	if backend := stringFromMetadata(event.Metadata, "sandbox_backend"); backend != "" {
		payload["sandbox_backend"] = backend
	}
	if tc, ok := event.Metadata["tool_call"].(coretools.ToolCall); ok {
		payload["tool_call_id"] = tc.ID
		payload["tool_name"] = firstNonEmpty(tc.Name, fmt.Sprint(payload["tool_name"]))
		addPermissionInputSummary(payload, tc.Input)
	} else if raw, ok := event.Metadata["tool_call"].(map[string]any); ok {
		payload["tool_call_id"] = raw["id"]
		payload["tool_name"] = firstNonEmpty(fmt.Sprint(raw["name"]), fmt.Sprint(payload["tool_name"]))
		if input, ok := raw["input"].(map[string]any); ok {
			addPermissionInputSummary(payload, input)
		}
	}
	return payload
}

func addPermissionInputSummary(payload map[string]any, input map[string]any) {
	if input == nil {
		return
	}
	for _, key := range []string{"command", "cmd", "file_path", "path", "cwd", "workdir", "reason", "description"} {
		if value, ok := input[key]; ok && value != nil && strings.TrimSpace(fmt.Sprint(value)) != "" {
			payload[key] = truncateTeamVisible(fmt.Sprint(value), 800)
		}
	}
	if len(payload) <= 5 {
		payload["summary"] = truncateTeamVisible(fmt.Sprint(input), 800)
	}
}

func formatTeamToolCall(event agent.StreamEvent, showDetails bool) string {
	toolName := firstNonEmpty(event.Content, stringFromMetadata(event.Metadata, "tool_name"), "tool")
	if !showDetails {
		return "正在使用工具：" + toolName
	}
	return "正在使用工具：" + toolName + "\n" + truncateTeamVisible(fmt.Sprintf("input: %v", event.Metadata["input"]), 300)
}

func formatTeamToolResult(event agent.StreamEvent, showDetails bool) string {
	toolName := firstNonEmpty(stringFromMetadata(event.Metadata, "tool_name"), event.Content, "tool")
	if isTruthy(event.Metadata["is_error"]) {
		return "工具返回错误：" + toolName + "\n" + truncateTeamVisible(fmt.Sprint(event.Metadata["result"]), 500)
	}
	if !showDetails {
		return "工具执行完成：" + toolName
	}
	result := strings.TrimSpace(fmt.Sprint(event.Metadata["result"]))
	if result == "" {
		return "工具执行完成：" + toolName
	}
	return "工具执行完成：" + toolName + "\n" + truncateTeamVisible(result, 500)
}

func stringFromMetadata(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func isTruthy(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return v == "true"
	default:
		return false
	}
}

func truncateTeamVisible(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 || len([]rune(text)) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit]) + "..."
}

func (s *Session) appendTimeline(eventType string, payload any) {
	s.mu.Lock()
	s.timeline = append(s.timeline, TimelineEvent{Type: eventType, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Payload: payload})
	s.mu.Unlock()
	s.persist()
}

func (s *Session) setActivity(agentID, status, summary, taskID string) {
	s.mu.Lock()
	s.activity[agentID] = ActivityRow{AgentID: agentID, DisplayName: s.agentDisplayNameLocked(agentID), Status: status, Summary: summary, TaskID: taskID}
	s.mu.Unlock()
	s.persist()
	s.emit("team.agent.status", s.Snapshot())
}

func (s *Session) extractArtifacts(owner, taskType, result string, expected []string) []string {
	var refs []string
	if len(expected) == 0 && strings.TrimSpace(result) == "" {
		return nil
	}
	for _, artifact := range nonEmptyStrings(expected) {
		if path, ok := s.resolveExpectedArtifactPath(artifact); ok {
			refs = append(refs, s.recordArtifact(owner, s.artifactDisplayName(artifact, path), firstLineFromFile(path), path).Name)
		}
	}
	if len(refs) > 0 {
		s.persist()
		return refs
	}
	name := firstNonEmpty(firstString(expected), taskType, "artifact") + "-" + owner
	id := "art-" + uuid.NewString()[:8]
	path := filepath.Join(s.rootDir, "artifacts", id+".md")
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = apppaths.WriteFileAtomic(path, []byte(result), 0o600)
	artifact := s.recordArtifact(owner, name, firstLine(result), path)
	refs = append(refs, artifact.Name)
	s.persist()
	return refs
}

func (s *Session) recordWriteFileArtifactFromEvent(owner string, event agent.StreamEvent) {
	if stringFromMetadata(event.Metadata, "tool_name") != "write_file" {
		return
	}
	path, ok := writeFilePathFromToolResult(fmt.Sprint(event.Metadata["result"]))
	if !ok || !fileExists(path) || (!s.pathInsideWorkspace(path) && !s.pathInsideTeamRuntime(path)) {
		return
	}
	s.recordArtifact(owner, s.artifactNameForPath(path), firstLineFromFile(path), path)
	s.persist()
}

func writeFilePathFromToolResult(result string) (string, bool) {
	const prefix = "File written successfully: "
	result = strings.TrimSpace(result)
	if !strings.HasPrefix(result, prefix) {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(result, prefix))
	idx := strings.LastIndex(rest, " (")
	if idx <= 0 {
		return "", false
	}
	path := strings.TrimSpace(rest[:idx])
	if path == "" {
		return "", false
	}
	return filepath.Clean(path), true
}

func (s *Session) pathInsideWorkspace(path string) bool {
	return pathInsideRoot(path, s.Config.CWD)
}

func (s *Session) pathInsideTeamRuntime(path string) bool {
	return pathInsideRoot(path, s.teamRuntimeDir())
}

func pathInsideRoot(path, root string) bool {
	root = strings.TrimSpace(root)
	if root == "" || strings.TrimSpace(path) == "" {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != "..")
}

func (s *Session) artifactNameForPath(path string) string {
	if s.pathInsideWorkspace(path) {
		return filepath.ToSlash(mustRelOrBase(s.Config.CWD, path))
	}
	if s.pathInsideTeamRuntime(path) {
		return filepath.ToSlash(mustRelOrBase(s.teamRuntimeDir(), path))
	}
	return filepath.Base(path)
}

func mustRelOrBase(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != ".." {
		return rel
	}
	return filepath.Base(path)
}

func (s *Session) recordArtifact(owner, name, summary, path string) Artifact {
	name = firstNonEmpty(strings.TrimSpace(name), filepath.Base(path), "artifact")
	path = filepath.Clean(path)
	s.mu.Lock()
	for _, existing := range s.artifacts {
		if filepath.Clean(existing.Path) == path {
			s.mu.Unlock()
			return existing
		}
	}
	artifact := Artifact{ID: "art-" + uuid.NewString()[:8], Name: name, Owner: owner, Summary: summary, Path: path, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	s.artifacts = append(s.artifacts, artifact)
	s.mu.Unlock()
	s.emit("team.artifact.created", artifact)
	return artifact
}

func (s *Session) resolveExpectedArtifactPath(name string) (string, bool) {
	name = strings.Trim(strings.TrimSpace(name), "`\"'")
	if name == "" {
		return "", false
	}
	if filepath.IsAbs(name) {
		if fileExists(name) {
			return filepath.Clean(name), true
		}
		return "", false
	}
	candidate := filepath.Join(s.Config.CWD, name)
	if fileExists(candidate) {
		return filepath.Clean(candidate), true
	}
	matches, err := filepath.Glob(filepath.Join(s.Config.CWD, "*", name))
	if err == nil {
		for _, match := range matches {
			if fileExists(match) {
				return filepath.Clean(match), true
			}
		}
	}
	return "", false
}

func (s *Session) artifactDisplayName(name, path string) string {
	name = strings.Trim(strings.TrimSpace(name), "`\"'")
	if filepath.IsAbs(name) {
		if rel, err := filepath.Rel(s.Config.CWD, name); err == nil && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != ".." {
			name = rel
		} else {
			name = filepath.Base(name)
		}
	}
	if name == "" {
		name = path
		if filepath.IsAbs(name) {
			if rel, err := filepath.Rel(s.Config.CWD, name); err == nil && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != ".." {
				name = rel
			} else {
				name = filepath.Base(name)
			}
		}
	}
	if name == "" {
		name = "artifact"
	}
	return filepath.ToSlash(filepath.Clean(name))
}

func firstLineFromFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text())
	}
	return ""
}

func (s *Session) persist() {
	if strings.TrimSpace(s.rootDir) == "" {
		return
	}
	_ = os.MkdirAll(s.rootDir, 0o700)
	s.trimRuntimeLogs()
	snapshot := s.Snapshot()
	s.mu.Lock()
	dialogue := append([]DialogueEntry(nil), s.dialogue...)
	timeline := append([]TimelineEvent(nil), s.timeline...)
	artifacts := append([]Artifact(nil), s.artifacts...)
	a2aTasks := make([]TeamTask, 0, len(s.a2aTasks))
	for _, task := range s.a2aTasks {
		task.done = nil
		a2aTasks = append(a2aTasks, task)
	}
	agents := make(map[string]*AgentRuntime, len(s.agents))
	for id, runtime := range s.agents {
		agents[id] = runtime
	}
	s.mu.Unlock()
	teamData := map[string]any{"id": s.ID, "parent_session_id": s.ParentSessionID, "team": s.Spec.Name, "snapshot": snapshot, "a2a_tasks": a2aTasks}
	writeJSON(filepath.Join(s.rootDir, "team.json"), teamData)
	writeJSONL(filepath.Join(s.rootDir, "dialogue.jsonl"), dialogue)
	writeJSONL(filepath.Join(s.rootDir, "timeline.jsonl"), timeline)
	writeJSON(filepath.Join(s.rootDir, "artifacts", "index.json"), artifacts)
	for id, runtime := range agents {
		dir := filepath.Join(s.rootDir, "agents", id)
		_ = os.MkdirAll(dir, 0o700)
		state := persistableAgentState(runtime.State)
		writeJSON(filepath.Join(dir, "state.json"), state)
		writeJSONL(filepath.Join(dir, "transcript.jsonl"), state.Messages)
	}
}

func (s *Session) trimRuntimeLogs() {
	if s == nil {
		return
	}
	dialogueLimit := s.Config.TeamDialogueMaxEntries
	if dialogueLimit <= 0 {
		dialogueLimit = 1000
	}
	timelineLimit := s.Config.TeamTimelineMaxEntries
	if timelineLimit <= 0 {
		timelineLimit = 2000
	}
	var oldDialogue []DialogueEntry
	var oldTimeline []TimelineEvent
	s.mu.Lock()
	if len(s.dialogue) > dialogueLimit {
		cut := len(s.dialogue) - dialogueLimit
		oldDialogue = append([]DialogueEntry(nil), s.dialogue[:cut]...)
		s.dialogue = append([]DialogueEntry(nil), s.dialogue[cut:]...)
	}
	if len(s.timeline) > timelineLimit {
		cut := len(s.timeline) - timelineLimit
		oldTimeline = append([]TimelineEvent(nil), s.timeline[:cut]...)
		s.timeline = append([]TimelineEvent(nil), s.timeline[cut:]...)
	}
	s.mu.Unlock()
	if len(oldDialogue) > 0 {
		appendDialogueSummary(filepath.Join(s.rootDir, "dialogue.summary.md"), oldDialogue)
	}
	if len(oldTimeline) > 0 {
		appendTimelineSummary(filepath.Join(s.rootDir, "timeline.summary.md"), oldTimeline)
	}
}

func appendDialogueSummary(path string, entries []DialogueEntry) {
	if len(entries) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n## Trimmed dialogue (%s, %d entries)\n\n", time.Now().UTC().Format(time.RFC3339), len(entries))
	for _, entry := range entries {
		fmt.Fprintf(&b, "- %s %s -> %s [%s] %s: %s\n",
			entry.CreatedAt,
			firstNonEmpty(entry.FromAgent, "unknown"),
			strings.Join(entry.ToAgent, ","),
			firstNonEmpty(entry.Kind, "message"),
			firstNonEmpty(entry.Summary, "(no summary)"),
			truncateTeamVisible(strings.ReplaceAll(entry.Content, "\n", " "), 500),
		)
	}
	appendTextFile(path, b.String())
}

func appendTimelineSummary(path string, entries []TimelineEvent) {
	if len(entries) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n## Trimmed timeline (%s, %d events)\n\n", time.Now().UTC().Format(time.RFC3339), len(entries))
	for _, entry := range entries {
		payload, _ := json.Marshal(entry.Payload)
		fmt.Fprintf(&b, "- %s [%s] %s\n", entry.CreatedAt, entry.Type, truncateTeamVisible(string(payload), 500))
	}
	appendTextFile(path, b.String())
}

func appendTextFile(path, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(text)
}

func persistableAgentState(state *agent.AgentState) agent.AgentState {
	if state == nil {
		empty := agent.NewAgentState()
		return empty
	}
	clone := *state
	clone.CacheBreakPoints = nil
	clone.Messages = persistableMessages(state.Messages)
	return clone
}

func persistableMessages(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		out = append(out, persistableMap(message))
	}
	return out
}

func persistableMap(message map[string]any) map[string]any {
	data, err := marshalJSONRecover(message)
	if err == nil {
		var clean map[string]any
		if json.Unmarshal(data, &clean) == nil {
			return clean
		}
	}
	clean := make(map[string]any, len(message))
	for key, value := range message {
		if data, err := marshalJSONRecover(value); err == nil {
			var decoded any
			if json.Unmarshal(data, &decoded) == nil {
				clean[key] = decoded
				continue
			}
		}
		clean[key] = fmt.Sprint(value)
	}
	return clean
}

func marshalJSONRecover(value any) (data []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			data = nil
			err = fmt.Errorf("json marshal panic: %v", recovered)
		}
	}()
	data, err = json.Marshal(value)
	return data, err
}

func (s *Session) agentCardsText() string {
	var lines []string
	for _, spec := range s.Spec.AgentSpecs {
		lines = append(lines, fmt.Sprintf("- %s (%s): %s", spec.Name, spec.DisplayName, spec.Description))
	}
	return strings.Join(lines, "\n")
}

func activityRowsLocked(s *Session) []ActivityRow {
	rows := make([]ActivityRow, 0, len(s.activity))
	for _, spec := range s.Spec.AgentSpecs {
		if row, ok := s.activity[spec.Name]; ok {
			rows = append(rows, row)
		}
	}
	return rows
}

func (s *Session) dialogueText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lines []string
	start := 0
	if len(s.dialogue) > 40 {
		start = len(s.dialogue) - 40
	}
	for _, entry := range s.dialogue[start:] {
		lines = append(lines, fmt.Sprintf("%s -> %s: %s", entry.FromAgent, strings.Join(entry.ToAgent, ","), firstNonEmpty(entry.Content, entry.Summary)))
	}
	return strings.Join(lines, "\n")
}

func (s *Session) a2aTaskStatusText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.a2aTasks) == 0 {
		return "No A2A tasks have been dispatched yet."
	}
	tasks := make([]TeamTask, 0, len(s.a2aTasks))
	for _, task := range s.a2aTasks {
		tasks = append(tasks, task)
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i].ID == tasks[j].ID {
			return tasks[i].To < tasks[j].To
		}
		return tasks[i].ID < tasks[j].ID
	})
	if len(tasks) > 20 {
		tasks = tasks[len(tasks)-20:]
	}
	lines := make([]string, 0, len(tasks))
	for _, task := range tasks {
		detail := firstNonEmpty(task.Err, task.Result)
		if detail != "" {
			detail = " - " + truncateTeamVisible(detail, 220)
		}
		lines = append(lines, fmt.Sprintf("%s %s -> %s: %s (%s)%s", task.ID, task.From, task.To, firstNonEmpty(task.Summary, "task"), firstNonEmpty(task.Status, "unknown"), detail))
	}
	return strings.Join(lines, "\n")
}

func (s *Session) validateTargets(from string, targets []string) (bool, string) {
	sender, ok := s.agents[from]
	if !ok {
		return false, "sender agent not found: " + from
	}
	allowed := map[string]struct{}{}
	for _, value := range sender.Spec.AllowedAgents {
		if value == "all" {
			for id := range s.agents {
				allowed[id] = struct{}{}
			}
			break
		}
		allowed[value] = struct{}{}
	}
	for _, target := range targets {
		if _, ok := s.agents[target]; !ok {
			return false, "target agent not found: " + target
		}
		if _, ok := allowed[target]; !ok {
			return false, fmt.Sprintf("%s is not allowed to communicate with %s", from, target)
		}
	}
	return true, ""
}

func (s *Session) agentDisplayName(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentDisplayNameLocked(id)
}

func (s *Session) agentDisplayNameLocked(id string) string {
	if runtime := s.agents[id]; runtime != nil && runtime.Spec.DisplayName != "" {
		return runtime.Spec.DisplayName
	}
	return id
}

func (s *Session) emit(eventType string, payload any) {
	if s.emitFn != nil {
		s.emitFn(s.ParentSessionID, eventType, payload)
	}
}

func normalizeTargets(values []string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if _, ok := seen[part]; ok {
				continue
			}
			seen[part] = struct{}{}
			out = append(out, part)
		}
	}
	return out
}

func toolNameFromPermissionEvent(event agent.StreamEvent) string {
	if tc, ok := event.Metadata["tool_call"].(coretools.ToolCall); ok {
		return tc.Name
	}
	if raw, ok := event.Metadata["tool_call"].(map[string]any); ok {
		if name, _ := raw["name"].(string); name != "" {
			return name
		}
	}
	return ""
}

func writeJSON(path string, value any) {
	defer func() {
		if recovered := recover(); recovered != nil {
			fmt.Fprintf(os.Stderr, "lumina team persist: failed to write %s: %v\n", path, recovered)
		}
	}()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "lumina team persist: failed to marshal %s: %v\n", path, err)
		return
	}
	if err := apppaths.WriteFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "lumina team persist: failed to write %s: %v\n", path, err)
	}
}

func writeJSONL[T any](path string, values []T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			fmt.Fprintf(os.Stderr, "lumina team persist: failed to write %s: %v\n", path, recovered)
		}
	}()
	var data bytes.Buffer
	enc := json.NewEncoder(&data)
	for _, value := range values {
		if err := enc.Encode(value); err != nil {
			fmt.Fprintf(os.Stderr, "lumina team persist: failed to encode %s: %v\n", path, err)
			return
		}
	}
	if err := apppaths.WriteFileAtomic(path, data.Bytes(), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "lumina team persist: failed to write %s: %v\n", path, err)
	}
}

func readText(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	line, _, _ := strings.Cut(text, "\n")
	if len([]rune(line)) > 160 {
		runes := []rune(line)
		return string(runes[:160])
	}
	return line
}
