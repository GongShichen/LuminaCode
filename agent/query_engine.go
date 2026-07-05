package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"LuminaCode/agentContext"
	"LuminaCode/config"
	"LuminaCode/skills"
	coretools "LuminaCode/tools"

	"github.com/google/uuid"
)

type QueryEngine struct {
	Config      config.Config
	CoreEngine  *CoreExecutionEngine
	SessionCost float64

	skillPermissionMu sync.Mutex
	skillPermissionCh chan bool
}

func NewQueryEngine(cfg *config.Config) *QueryEngine {
	if cfg == nil {
		c := config.GetConfig()
		cfg = &c
	}
	return &QueryEngine{
		Config:     *cfg,
		CoreEngine: NewCoreExecutionEngine(cfg),
	}
}

func CreateQueryEngine(cfg *config.Config) *QueryEngine {
	return NewQueryEngine(cfg)
}

func (q *QueryEngine) Abort() { q.CoreEngine.Abort() }

func (q *QueryEngine) Reset() {
	q.CoreEngine.Reset()
	q.SessionCost = 0
}

func (q *QueryEngine) Shutdown() {
	if q.CoreEngine != nil {
		q.CoreEngine.Shutdown()
	}
	q.SessionCost = 0
}

func (q *QueryEngine) ClearMCP() {
	if q.CoreEngine != nil {
		q.CoreEngine.ClearMCP()
	}
}

func (q *QueryEngine) ResolvePermission(decision string, toolName string) {
	if q.resolvePendingSkillPermission(decision == PermissionOnce || decision == PermissionAlways || decision == "true") {
		return
	}
	q.CoreEngine.ResolvePermission(decision, toolName)
}

func (q *QueryEngine) ResolveSkillPermission(granted bool) {
	if q.resolvePendingSkillPermission(granted) {
		return
	}
	if q.CoreEngine != nil {
		q.CoreEngine.ResolveSkillPermission(granted)
	}
}

func (q *QueryEngine) ResolveMCPTrust(granted bool) {
	if q.CoreEngine != nil {
		q.CoreEngine.ResolveMCPTrust(granted)
	}
}

func (q *QueryEngine) SkillRegistry() *skills.SkillRegistry {
	if q == nil || q.CoreEngine == nil {
		return nil
	}
	return q.CoreEngine.skillRegistry
}

func (q *QueryEngine) Compact(state *AgentState) (AgentState, agentContext.CompressionStats) {
	if state == nil {
		empty := NewAgentState()
		return empty, *agentContext.DefaultCompressionStats()
	}
	state.Messages = skills.StripSkillContextMessages(state.Messages, nil)
	currentTokens := agentContext.TokenCountWithEstimation(state.Messages)
	pipeline := agentContext.DefaultContextPipeline()
	pipeline.Config = q.Config
	compressed, stats := pipeline.Compress(
		state.Messages,
		currentTokens,
		state.SystemPrompt,
		q.Config.CompressionContextLimit(),
		q.Config.CompressionThreshold(),
		state,
		nil,
		true,
	)
	if stats.LevelReached >= 1 {
		state.Messages = compressed
		if state.CacheBreakPoints != nil {
			state.CacheBreakPoints.Clear()
		}
		if q.CoreEngine != nil {
			q.CoreEngine.MarkSkillHistoryCompacted("main")
		}
	}
	return *state, stats
}

func (q *QueryEngine) SubmitMessage(ctx context.Context, userPrompt string, state *AgentState, sessionID ...string) <-chan StreamEvent {
	out := make(chan StreamEvent)
	go func() {
		defer close(out)
		if strings.HasPrefix(userPrompt, "/") {
			cmd := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(userPrompt, "/")))
			switch cmd {
			case "help", "cost", "tokens", "skill", "mcp", "compact", "compress":
				sendStream(ctx, out, NewStreamEvent("text", fmt.Sprintf("[system] The '%s' command is handled by the REPL.\n", userPrompt), nil))
				sendStream(ctx, out, NewStreamEvent("done", "", nil))
				return
			}
		}
		if state == nil {
			s := NewAgentState()
			state = &s
			if q.Config.Yolo && state.PermissionState != nil {
				state.PermissionState.YoloMode = true
			}
		}
		state.Messages = skills.StripSkillContextMessages(state.Messages, nil)
		q.buildOrRefreshSystemPrompt(state)
		normalizedPrompt := userPrompt
		if strings.HasPrefix(userPrompt, "/") {
			skill, args := q.parseSkillInvocation(userPrompt)
			if skill == nil {
				sendStream(ctx, out, NewStreamEvent("error", "Unknown command: "+userPrompt, nil))
				sendStream(ctx, out, NewStreamEvent("done", "", nil))
				return
			}
			execution, err := q.executeSlashSkill(ctx, out, *skill, args, state, sessionID)
			if err != nil {
				sendStream(ctx, out, NewStreamEvent("error", err.Error(), nil))
				sendStream(ctx, out, NewStreamEvent("done", "", nil))
				return
			}
			normalizedPrompt = q.normalizeSkillUserPrompt(skill.CanonicalName, args)
			if execution.Mode == "fork" {
				resultText := "Skill '" + skill.CanonicalName + "' completed."
				if execution.ResultText != nil && *execution.ResultText != "" {
					resultText = *execution.ResultText
				}
				AddUsage(state, execution.InputTokens, execution.OutputTokens)
				q.commitUserTurn(state, normalizedPrompt)
				AppendAssistantMessage(state, nil, resultText, nil, "")
				state.TurnCount++
				cost := q.CoreEngine.CalculateCost(state)
				q.SessionCost = cost
				q.CoreEngine.LastState = state
				sendStream(ctx, out, NewStreamEvent("text", resultText, nil))
				sendStream(ctx, out, NewStreamEvent("cost", fmt.Sprintf("$%.4f", cost), map[string]any{"cost": cost}))
				sendStream(ctx, out, NewStreamEvent("done", "", nil))
				return
			}
			if execution.Prompt != nil {
				state.Messages = append(state.Messages, q.CoreEngine.skillRegistryMessage(*skill, *execution.Prompt))
			}
			q.recordSkillInvocation(*skill, execution, state.TurnCount)
		}
		q.commitUserTurn(state, normalizedPrompt)
		for event := range q.CoreEngine.QueryLoop(ctx, state) {
			sendStream(ctx, out, event)
			if event.Type == "cost" {
				if cost, ok := event.Metadata["cost"].(float64); ok {
					q.SessionCost = cost
				}
			}
		}
	}()
	return out
}

func (q *QueryEngine) buildOrRefreshSystemPrompt(state *AgentState) {
	if state == nil {
		return
	}
	cwd := skills.ResolveSkillContextCWD(q.Config.CWD, nil)
	if q.Config.AutoMemoryEnabled && q.Config.AutoMemoryDirectory != nil && *q.Config.AutoMemoryDirectory != "" {
		prompt, err := agentContext.BuildSystemPrompt(cwd, agentContext.BuildMemorySection(&q.Config))
		if err == nil && strings.TrimSpace(prompt) != "" {
			state.SystemPrompt = prompt
			return
		}
	}
	if state.SystemPrompt == "" {
		state.SystemPrompt = BuildSystemPrompt(cwd)
	}
}

func (q *QueryEngine) parseSkillInvocation(userPrompt string) (*skills.SkillSpec, string) {
	if !q.Config.SkillsEnabled || !strings.HasPrefix(userPrompt, "/") || q.CoreEngine.skillRegistry == nil {
		return nil, ""
	}
	stripped := strings.TrimSpace(strings.TrimPrefix(userPrompt, "/"))
	if stripped == "" {
		return nil, ""
	}
	cmdName, args, _ := strings.Cut(stripped, " ")
	cmdName = strings.ToLower(strings.TrimSpace(cmdName))
	cwd := skills.ResolveSkillContextCWD(q.Config.CWD, nil)
	skill := q.CoreEngine.skillRegistry.FindVisible(cmdName, cwd)
	if skill == nil || !skill.Frontmatter.UserInvocable {
		return nil, ""
	}
	return skill, strings.TrimSpace(args)
}

func (q *QueryEngine) executeSlashSkill(ctx context.Context, out chan<- StreamEvent, skill skills.SkillSpec, args string, state *AgentState, sessionID []string) (skills.SkillExecutionResult, error) {
	if q.CoreEngine.skillRegistry == nil {
		return skills.SkillExecutionResult{}, fmt.Errorf("skills are not enabled")
	}
	loader := skills.NewSkillLoader(q.Config)
	executor := skills.NewSkillExecutor(loader, skills.NewPromptProcessor(q.Config))
	executor.ForkRunner = q.CoreEngine.runForkSkill
	sid := ""
	if len(sessionID) > 0 {
		sid = sessionID[0]
	}
	if sid == "" {
		sid = uuid.NewString()[:12]
	}
	extraContext := coretools.ExecutionContext{
		"_session_id":         sid,
		"_skill_persistence":  q.CoreEngine.skillPersistence,
		"_skill_agent_scope":  "main",
		"_turn_count":         state.TurnCount,
		"_permission_runtime": q.CoreEngine,
	}
	return executor.Execute(ctx, skill, args, sid, func(req skills.SkillShellPermissionRequest) bool {
		return q.requestSkillShellPermission(ctx, out, req)
	}, q.CoreEngine.Registry, state, extraContext)
}

func (q *QueryEngine) requestSkillShellPermission(ctx context.Context, out chan<- StreamEvent, req skills.SkillShellPermissionRequest) bool {
	ch := make(chan bool, 1)
	q.skillPermissionMu.Lock()
	if q.skillPermissionCh != nil {
		q.skillPermissionMu.Unlock()
		return false
	}
	q.skillPermissionCh = ch
	q.skillPermissionMu.Unlock()
	defer func() {
		q.skillPermissionMu.Lock()
		if q.skillPermissionCh == ch {
			q.skillPermissionCh = nil
		}
		q.skillPermissionMu.Unlock()
	}()

	event := NewStreamEvent("permission_needed", "", map[string]any{
		"skill_shell_request": req,
		"dangerous":           true,
		"risk":                "high",
	})
	if !sendStream(ctx, out, event) {
		return false
	}
	select {
	case granted := <-ch:
		return granted
	case <-ctx.Done():
		return false
	}
}

func (q *QueryEngine) resolvePendingSkillPermission(granted bool) bool {
	q.skillPermissionMu.Lock()
	ch := q.skillPermissionCh
	q.skillPermissionMu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- granted:
	default:
	}
	return true
}

func (q *QueryEngine) normalizeSkillUserPrompt(skillName, skillArgs string) string {
	if strings.TrimSpace(skillArgs) != "" {
		return fmt.Sprintf("Use skill '%s' with arguments: %s", skillName, skillArgs)
	}
	return fmt.Sprintf("Use skill '%s'.", skillName)
}

func (q *QueryEngine) recordSkillInvocation(skill skills.SkillSpec, execution skills.SkillExecutionResult, turnCount int) {
	if q.CoreEngine.skillPersistence == nil || execution.Prompt == nil || *execution.Prompt == "" {
		return
	}
	path := skill.SkillFile
	if path == "" {
		path = skill.Directory
	}
	q.CoreEngine.skillPersistence.RecordInvocation("main", skill.CanonicalName, path, *execution.Prompt, turnCount)
}

func (e *CoreExecutionEngine) skillRegistryMessage(skill skills.SkillSpec, prompt string) map[string]any {
	loader := skills.NewSkillLoader(e.Config)
	executor := skills.NewSkillExecutor(loader, skills.NewPromptProcessor(e.Config))
	return executor.BuildInlineSkillMessage(skill, prompt, true)
}

func (q *QueryEngine) commitUserTurn(state *AgentState, normalizedPrompt string) {
	state.LastQuery = normalizedPrompt
	state.UserTurnCount++
	state.MemoryWritesSinceExtraction = false
	state.Messages = append(state.Messages, map[string]any{
		"role":    "user",
		"content": []map[string]any{{"type": "text", "text": normalizedPrompt}},
	})
	if q.CoreEngine != nil {
		q.CoreEngine.LastState = state
	}
}
