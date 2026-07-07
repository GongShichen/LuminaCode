package team

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"LuminaCode/agent"
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
	loopIteration  int
	waitingForUser bool
	completed      bool
	finalAnswer    string

	busy   atomic.Bool
	cancel context.CancelFunc
	runCtx context.Context

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
	ID      string `json:"id"`
	From    string `json:"from"`
	To      string `json:"to"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Result  string `json:"result"`
	Err     string `json:"error,omitempty"`
	done    chan TeamTask
}

func NewSession(parentSessionID string, cfg config.Config, spec TeamSpec, emit PushFunc, ask PermissionFunc) *Session {
	id := "team-" + uuid.NewString()
	root := filepath.Join(cfg.SessionDir, parentSessionID, "teams", id)
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
		gateVerdicts:    map[string]GateVerdict{},
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
	cfg.SystemPromptPath = spec.SystemPromptPath
	cfg.UserSkillsDir = spec.SkillsDir
	cfg.SkillsDir = ".Lumina/__team_no_project_skills__"
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
	engine.CoreEngine.Registry = engine.CoreEngine.Registry.FilteredCopy(nil, ordinarySubagentToolDenylist(), false, false)
	engine.CoreEngine.Registry.Register(NewSendA2AMessageTool(s, spec.Name))
	if spec.Name == s.Spec.EntryAgent {
		engine.CoreEngine.Registry.Register(NewRecordTeamContractTool(s, spec.Name))
		engine.CoreEngine.Registry.Register(NewCompleteTeamTaskTool(s, spec.Name))
	}
	if spec.Name == "qa" || spec.Name == "reviewer" {
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
		s.loopIteration++
		iteration := s.loopIteration
		s.mu.Unlock()
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
	policy := readText(s.Spec.CompletionPolicy)
	return fmt.Sprintf(`Team Loop iteration #%d.

Current working directory:
%s

User request:
%s

Team system instructions:
%s

Completion policy:
%s

Available agents:
%s

Current readable dialogue:
%s

Hard runtime rules:
- A2A is traceable. Never say A2A messages cannot be tracked.
- Before dispatching implementation, QA, or review work, call RecordTeamContract with the acceptance contract.
- Verbal assignment is invalid. To assign work, call SendA2AMessage and wait for the tool result.
- If a specialist exists, do not replace that specialist with a generic background agent or ordinary sub-agent.
- Every specialist's visible progress is projected into the Team transcript; keep member messages useful for the user.
- Preserve user-specified artifact paths exactly.
- If the user asks to create a new named project or directory, infer the project root as Current working directory + "/" + that name unless the user gives an absolute path. All dispatches, files, README paths, tests, and final artifact checks must use that project root. Do not flatten named projects into the current working directory.
- If a Reviewer verdict is "reject" or QA verdict is "fail", immediately dispatch concrete repair tasks for the cited findings, then re-run QA and Reviewer gates.
- If the user asks for multiple components such as "Go backend + TS CLI", treat those components as integrated by default. The frontend/CLI must consume the backend/API unless the user explicitly asks for independent or direct-file implementations.
- QA and Reviewer must use SubmitGateVerdict. CompleteTeamTask only succeeds when runtime gate verdicts satisfy the recorded contract.

Continue the Team Loop. Use SendA2AMessage for member-to-member work. Do not stop because of timeout, error, rejection, or long iteration count. Only call CompleteTeamTask when the completion policy is fully satisfied.`, iteration, s.Config.CWD, userInput, teamSystem, policy, s.agentCardsText(), s.dialogueText())
}

func (s *Session) SendA2AMessage(ctx context.Context, from string, input A2AMessageInput) map[string]any {
	if err := s.ensureContractBeforeDispatch(from, input); err != nil {
		s.appendRecovery(err.Error())
		return map[string]any{"status": "error", "error": err.Error()}
	}
	if input.TimeoutSeconds <= 0 {
		input.TimeoutSeconds = 300
	}
	targets := normalizeTargets(input.To)
	allowed, msg := s.validateTargets(from, targets)
	if !allowed {
		return map[string]any{"status": "error", "error": msg}
	}
	taskID := "a2a-" + uuid.NewString()[:8]
	s.appendDialogue(DialogueEntry{
		FromAgent: from, ToAgent: targets, Kind: "message",
		Summary: input.TaskType, Content: input.Message, TaskID: taskID,
		ArtifactRefs: input.ExpectedArtifacts,
	})
	s.appendTimeline("message/send", map[string]any{"task_id": taskID, "from": from, "to": targets, "message": input.Message})
	results := make([]map[string]any, 0, len(targets))
	var wg sync.WaitGroup
	resultCh := make(chan map[string]any, len(targets))
	parallelism := s.Spec.Loop.MaxParallelAgents
	if parallelism <= 0 {
		parallelism = len(targets)
	}
	sem := make(chan struct{}, parallelism)
	for _, target := range targets {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					msg := fmt.Sprintf("agent dispatch panic: %v\n%s", recovered, debug.Stack())
					s.appendRecovery(msg)
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
					done <- map[string]any{"to": target, "status": "error", "error": err.Error()}
					return
				}
				done <- map[string]any{"to": target, "status": "completed", "result": result}
			}()
			awaitResponse := true
			if input.AwaitResponse != nil {
				awaitResponse = *input.AwaitResponse
			}
			if !awaitResponse {
				resultCh <- map[string]any{"to": target, "status": "queued", "task_id": taskID}
				return
			}
			select {
			case result := <-done:
				resultCh <- result
			case <-time.After(time.Duration(input.TimeoutSeconds) * time.Second):
				s.appendRecovery(fmt.Sprintf("%s did not finish within wait window; Team Loop continues with task pending.", target))
				s.setActivity(target, "running", "continuing after A2A wait timeout", taskID)
				s.appendDialogue(DialogueEntry{
					FromAgent: target,
					ToAgent:   []string{from},
					Kind:      "timeout",
					Summary:   "A2A wait window elapsed",
					Content: fmt.Sprintf(
						"%s did not finish within the %d second A2A wait window. This is not a user interrupt; the agent keeps working in the Team run context and the Team Loop may continue, retry, or collect a later result.",
						s.agentDisplayName(target), input.TimeoutSeconds,
					),
					TaskID: taskID,
				})
				resultCh <- map[string]any{"to": target, "status": "task_pending", "task_id": taskID}
			case <-ctx.Done():
				resultCh <- map[string]any{"to": target, "status": "interrupted_by_user", "task_id": taskID}
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

func (s *Session) activeRunContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runCtx != nil {
		return s.runCtx
	}
	return context.Background()
}

func (s *Session) ensureContractBeforeDispatch(from string, input A2AMessageInput) error {
	if from != s.Spec.EntryAgent {
		return nil
	}
	if !s.Spec.Gates.RequireContract {
		return nil
	}
	taskType := strings.ToLower(strings.TrimSpace(input.TaskType))
	if taskType == "" {
		return nil
	}
	requires := []string{"implementation", "implement", "backend", "frontend", "qa", "verification", "review"}
	matched := false
	for _, marker := range requires {
		if strings.Contains(taskType, marker) {
			matched = true
			break
		}
	}
	if !matched {
		return nil
	}
	s.mu.Lock()
	hasContract := s.contract != nil
	s.mu.Unlock()
	if hasContract {
		return nil
	}
	return fmt.Errorf("Cannot dispatch %q before recording Team Acceptance Contract. Team Leader must call RecordTeamContract first.", input.TaskType)
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
	agentPrompt := fmt.Sprintf("Message from %s.\nTask type: %s\nExpected artifacts: %s\n\n%s", from, taskType, strings.Join(expectedArtifacts, ", "), prompt)
	var text strings.Builder
	var eventErrors []string
	for event := range runtime.Engine.SubmitMessage(ctx, agentPrompt, runtime.State, agentID) {
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
			}
		case "error":
			msg := formatAgentError(agentID, event)
			eventErrors = append(eventErrors, msg)
			s.appendRecovery(msg)
		case "permission_needed":
			decision := s.requestPermission(agentID, event)
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

func (s *Session) SubmitGateVerdict(from string, input GateVerdict) string {
	role := strings.TrimSpace(input.Role)
	if role == "" {
		role = from
	}
	if role != "qa" && role != "reviewer" {
		return "Cannot submit gate verdict: role must be qa or reviewer."
	}
	if from != role {
		return fmt.Sprintf("Cannot submit %s verdict from agent %s.", role, from)
	}
	if role == "reviewer" && strings.TrimSpace(input.Status) == "reject" {
		// Rejections are stored, but completion will continue to fail until repair and re-review.
	} else if role == "qa" && !validQAStatus(input.Status) {
		return "Cannot submit QA verdict: status must be pass or not_applicable."
	} else if role == "reviewer" && !validReviewerStatus(input.Status) {
		return "Cannot submit Reviewer verdict: status must be pass, accepted_with_notes, or reject."
	}
	input.Role = role
	input.AgentID = from
	input.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	s.mu.Lock()
	if s.gateVerdicts == nil {
		s.gateVerdicts = map[string]GateVerdict{}
	}
	if role == "qa" {
		if previous, ok := s.gateVerdicts[role]; ok {
			input = mergeQAGateVerdict(previous, input)
		}
	}
	s.gateVerdicts[role] = input
	if role == "qa" {
		s.gate.QA = input.Status
	} else {
		s.gate.Reviewer = input.Status
	}
	s.mu.Unlock()
	s.appendTimeline("team.gate.verdict", input)
	s.appendDialogue(DialogueEntry{
		FromAgent: from,
		ToAgent:   []string{s.Spec.EntryAgent},
		Kind:      "gate_verdict",
		Summary:   fmt.Sprintf("%s verdict: %s", strings.ToUpper(role), input.Status),
		Content:   gateVerdictDialogueSummary(input),
	})
	s.persist()
	s.emit("team.frame.snapshot", s.Snapshot())
	return "Gate verdict recorded."
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
	s.mu.Lock()
	s.completed = true
	s.finalAnswer = input.FinalAnswer
	s.gate = GateStatus{QA: input.QAStatus, Reviewer: input.ReviewerStatus}
	if row, ok := s.activity[from]; ok {
		row.Status = "completed"
		row.Summary = firstNonEmpty(input.Summary, "Team task complete")
		s.activity[from] = row
	}
	s.mu.Unlock()
	s.appendDialogue(DialogueEntry{
		FromAgent: from, ToAgent: []string{"user"}, Kind: "final",
		Summary: firstNonEmpty(input.Summary, "Team task complete"),
		Content: input.FinalAnswer, ArtifactRefs: input.RequiredArtifacts,
	})
	s.appendTimeline("loop.completed", map[string]any{"summary": input.Summary, "qa_status": input.QAStatus, "reviewer_status": input.ReviewerStatus, "deferral_reasons": input.DeferralReasons})
	s.persist()
	return "Team task marked complete."
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
	if gates.QAAgent != "" {
		qa, ok := verdicts[gates.QAAgent]
		if !ok {
			return fmt.Errorf("missing structured QA gate verdict from %s; that agent must call SubmitGateVerdict", gates.QAAgent)
		}
		if strings.TrimSpace(input.QAStatus) == "" {
			return fmt.Errorf("qa_status is required for team QA gate %s", gates.QAAgent)
		}
		if input.QAStatus != qa.Status {
			return fmt.Errorf("qa_status %q does not match latest QA verdict %q", input.QAStatus, qa.Status)
		}
		if !validQAStatus(qa.Status) || qa.Status != "pass" {
			if qa.Status != "not_applicable" {
				return fmt.Errorf("QA gate is not pass/not_applicable: %s", qa.Status)
			}
		}
		if err := validateGateFindings("QA", qa, gates, input.DeferralReasons); err != nil {
			return err
		}
		if qa.Status == "pass" && contract != nil {
			if missing := missingRequiredEvidence(contract, qa.Evidence); len(missing) > 0 {
				return fmt.Errorf("QA evidence missing required contract checks: %s", strings.Join(missing, ", "))
			}
		}
	}
	if gates.ReviewerAgent != "" {
		reviewer, ok := verdicts[gates.ReviewerAgent]
		if !ok {
			return fmt.Errorf("missing structured Reviewer gate verdict from %s; that agent must call SubmitGateVerdict", gates.ReviewerAgent)
		}
		if strings.TrimSpace(input.ReviewerStatus) == "" {
			return fmt.Errorf("reviewer_status is required for team Reviewer gate %s", gates.ReviewerAgent)
		}
		if input.ReviewerStatus != reviewer.Status {
			return fmt.Errorf("reviewer_status %q does not match latest Reviewer verdict %q", input.ReviewerStatus, reviewer.Status)
		}
		if !validReviewerStatus(reviewer.Status) {
			return fmt.Errorf("Reviewer gate is not pass/accepted_with_notes: %s", reviewer.Status)
		}
		if strings.EqualFold(reviewer.Status, "accepted_with_notes") && containsBlockingLanguage(reviewer) {
			return fmt.Errorf("Reviewer accepted_with_notes contains blocking language")
		}
		if err := validateGateFindings("Reviewer", reviewer, gates, input.DeferralReasons); err != nil {
			return err
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

func validateGateFindings(label string, verdict GateVerdict, gates TeamGateSpec, deferrals map[string]string) error {
	for _, finding := range verdict.Findings {
		if finding.Blocking {
			return fmt.Errorf("%s has blocking finding: %s", label, finding.Summary)
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

func gateFindingDeferralReason(label string, finding GateFinding, deferrals map[string]string) string {
	if len(deferrals) == 0 {
		return ""
	}
	keys := []string{
		label + ":" + finding.Category + ":" + finding.Summary,
		finding.Category + ":" + finding.Summary,
		finding.Summary,
	}
	normalized := map[string]string{}
	for key, value := range deferrals {
		normalized[normalizeDeferralKey(key)] = strings.TrimSpace(value)
	}
	for _, key := range keys {
		if reason := normalized[normalizeDeferralKey(key)]; reason != "" {
			return reason
		}
	}
	return ""
}

func normalizeDeferralKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.Join(strings.Fields(key), " ")
	return key
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
		return fileExists(name)
	}
	if fileExists(filepath.Join(s.Config.CWD, name)) {
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

func missingRequiredEvidence(contract *AcceptanceContract, evidence []GateEvidence) []string {
	seen := map[string]bool{}
	for _, item := range evidence {
		if !item.Passed {
			continue
		}
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
		if (nameKey != "" && seen[nameKey]) || (commandKey != "" && seen[commandKey]) {
			continue
		}
		missing = append(missing, firstNonEmpty(check.Name, check.Command))
	}
	return missing
}

func normalizeEvidenceKey(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func containsBlockingLanguage(verdict GateVerdict) bool {
	haystack := strings.ToLower(verdict.Summary)
	for _, finding := range verdict.Findings {
		haystack += "\n" + strings.ToLower(finding.Category+" "+finding.Summary+" "+finding.Details)
	}
	haystack = strings.ReplaceAll(haystack, "nonblocking", "")
	haystack = strings.ReplaceAll(haystack, "non-blocking", "")
	haystack = strings.ReplaceAll(haystack, "no blocking", "")
	blocking := []string{"critical", "must fix", "must be fixed", "blocking", "build-breaking", "security", "data-loss", "correctness"}
	for _, marker := range blocking {
		if strings.Contains(haystack, marker) {
			return true
		}
	}
	return false
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
		GateStatus:       s.gate,
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

func mergeQAGateVerdict(existing GateVerdict, next GateVerdict) GateVerdict {
	merged := next
	if existing.Status == "pass" || next.Status == "pass" {
		merged.Status = "pass"
	}
	if strings.TrimSpace(existing.Summary) != "" && strings.TrimSpace(next.Summary) != "" && existing.Summary != next.Summary {
		merged.Summary = strings.TrimSpace(existing.Summary + "\n" + next.Summary)
	} else if strings.TrimSpace(merged.Summary) == "" {
		merged.Summary = existing.Summary
	}
	merged.Evidence = mergeGateEvidence(existing.Evidence, next.Evidence)
	// QA may submit several verdicts while fixing issues. Evidence is cumulative,
	// but findings describe the latest QA verdict so resolved blockers do not
	// poison later passes.
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
	DialogueCount  int        `json:"dialogue_count"`
	ArtifactCount  int        `json:"artifact_count"`
	ActivityCount  int        `json:"activity_count"`
	LoopIteration  int        `json:"loop_iteration"`
	GateStatus     GateStatus `json:"gate_status"`
	Running        bool       `json:"running"`
	ActiveTeamName string     `json:"active_team_name"`
}

func (s *Session) Summary() SummaryData {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SummaryData{
		DialogueCount:  len(s.dialogue),
		ArtifactCount:  len(s.artifacts),
		ActivityCount:  len(s.activity),
		LoopIteration:  s.loopIteration,
		GateStatus:     s.gate,
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
	name := firstNonEmpty(firstString(expected), taskType, "artifact") + "-" + owner
	id := "art-" + uuid.NewString()[:8]
	path := filepath.Join(s.rootDir, "artifacts", id+".md")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(result), 0o644)
	artifact := Artifact{ID: id, Name: name, Owner: owner, Summary: firstLine(result), Path: path, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	s.mu.Lock()
	s.artifacts = append(s.artifacts, artifact)
	s.mu.Unlock()
	refs = append(refs, artifact.Name)
	s.persist()
	s.emit("team.artifact.created", artifact)
	return refs
}

func (s *Session) persist() {
	_ = os.MkdirAll(s.rootDir, 0o755)
	snapshot := s.Snapshot()
	s.mu.Lock()
	teamData := map[string]any{"id": s.ID, "parent_session_id": s.ParentSessionID, "team": s.Spec.Name, "snapshot": snapshot}
	dialogue := append([]DialogueEntry(nil), s.dialogue...)
	timeline := append([]TimelineEvent(nil), s.timeline...)
	artifacts := append([]Artifact(nil), s.artifacts...)
	agents := make(map[string]*AgentRuntime, len(s.agents))
	for id, runtime := range s.agents {
		agents[id] = runtime
	}
	s.mu.Unlock()
	writeJSON(filepath.Join(s.rootDir, "team.json"), teamData)
	writeJSONL(filepath.Join(s.rootDir, "dialogue.jsonl"), dialogue)
	writeJSONL(filepath.Join(s.rootDir, "timeline.jsonl"), timeline)
	writeJSON(filepath.Join(s.rootDir, "artifacts", "index.json"), artifacts)
	for id, runtime := range agents {
		dir := filepath.Join(s.rootDir, "agents", id)
		_ = os.MkdirAll(dir, 0o755)
		state := persistableAgentState(runtime.State)
		writeJSON(filepath.Join(dir, "state.json"), state)
		writeJSONL(filepath.Join(dir, "transcript.jsonl"), state.Messages)
	}
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
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "lumina team persist: failed to marshal %s: %v\n", path, err)
		return
	}
	_ = os.WriteFile(path, append(data, '\n'), 0o644)
}

func writeJSONL[T any](path string, values []T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			fmt.Fprintf(os.Stderr, "lumina team persist: failed to write %s: %v\n", path, recovered)
		}
	}()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, value := range values {
		if err := enc.Encode(value); err != nil {
			fmt.Fprintf(os.Stderr, "lumina team persist: failed to encode %s: %v\n", path, err)
			return
		}
	}
	_ = w.Flush()
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
