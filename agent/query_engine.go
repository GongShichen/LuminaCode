package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"LuminaCode/agentContext"
	"LuminaCode/cluster"
	"LuminaCode/config"
	"LuminaCode/harness"
	"LuminaCode/memory"
	"LuminaCode/skills"
	coretools "LuminaCode/tools"

	"github.com/google/uuid"
)

type QueryEngine struct {
	Config     config.Config
	CoreEngine *CoreExecutionEngine
}

func NewQueryEngine(cfg config.Config, core *CoreExecutionEngine) *QueryEngine {
	return &QueryEngine{
		Config:     cfg,
		CoreEngine: core,
	}
}

type QueryEngineFactory interface {
	Create(config.Config, cluster.RuntimeIdentity) *QueryEngine
}

type DefaultQueryEngineFactory struct {
	memoryFactory MemoryFabricFactory
}

func NewQueryEngineFactory(memoryFactory MemoryFabricFactory) *DefaultQueryEngineFactory {
	return &DefaultQueryEngineFactory{memoryFactory: memoryFactory}
}

func (f *DefaultQueryEngineFactory) Create(cfg config.Config, identity cluster.RuntimeIdentity) *QueryEngine {
	if strings.TrimSpace(identity.TenantID) == "" {
		identity.TenantID = "local"
	}
	if strings.TrimSpace(identity.ProjectID) == "" {
		identity.ProjectID = MemoryFabricSpace(cfg)
	}
	core := NewCoreExecutionEngine(cfg, identity, f.memoryFactory)
	return NewQueryEngine(cfg, core)
}

func (q *QueryEngine) Abort() { q.CoreEngine.Abort() }

func (q *QueryEngine) Reset(ctx context.Context) error { return q.CoreEngine.Reset(ctx) }

func (q *QueryEngine) RefreshRuntimeConfig() {
	if q == nil {
		return
	}
	refreshed := config.ReloadDynamicConfig(q.Config)
	q.Config = refreshed
	if q.CoreEngine != nil {
		q.CoreEngine.RefreshRuntimeConfig(refreshed)
	}
	config.SetConfig(refreshed)
}

func (q *QueryEngine) Shutdown() {
	if q.CoreEngine != nil {
		q.CoreEngine.Shutdown()
	}
}

func (q *QueryEngine) ClearMCP() {
	if q.CoreEngine != nil {
		q.CoreEngine.ClearMCP()
	}
}

func (q *QueryEngine) ResolvePermission(decision, toolName string) {
	if q.CoreEngine.SessionRuntime.ResolvePermissionKind(PermissionSkill, PermissionAnswer{Decision: decision}) {
		return
	}
	q.CoreEngine.ResolvePermission(decision, toolName)
}
func (q *QueryEngine) ResolveSkillPermission(granted bool) {
	q.CoreEngine.ResolveSkillPermission(granted)
}
func (q *QueryEngine) ResolvePermissionRequest(id, decision, toolName string) bool {
	return q.CoreEngine.ResolvePermissionRequest(id, decision, toolName)
}

func (q *QueryEngine) PermissionContext(id string) (context.Context, bool) {
	return q.CoreEngine.SessionRuntime.PermissionContext(id)
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

func (q *QueryEngine) Compact(ctx context.Context, mode agentContext.CompactMode) (agentContext.CompactionResult, error) {
	runCtx, state, err := q.CoreEngine.AgentRuntime.Begin(ctx, q.Config.APIMaxTokens)
	if err != nil {
		return agentContext.CompactionResult{}, err
	}
	defer q.CoreEngine.AgentRuntime.EndRun(state.RunToken())
	q.buildOrRefreshSystemPrompt(state)
	result, err := q.CoreEngine.prepareContextResult(runCtx, state, q.CoreEngine.Registry.GetAPISchemas(), mode, q.Config.APIModel)
	if publishErr := q.CoreEngine.publishDraft(context.WithoutCancel(ctx), state); publishErr != nil && err == nil {
		err = publishErr
	}
	if err != nil {
		q.CoreEngine.AgentRuntime.FailRun(state.RunToken(), err.Error())
	}
	return result, err
}

func (q *QueryEngine) Snapshot() AgentSnapshot { return q.CoreEngine.AgentRuntime.Snapshot() }
func (q *QueryEngine) Restore(state *AgentState) error {
	if state == nil {
		fresh := NewAgentState()
		fresh.PermissionState.YoloMode = q.Config.Yolo
		state = &fresh
	}
	return q.CoreEngine.AgentRuntime.Restore(SnapshotFromState(state))
}

func (q *QueryEngine) SubmitMessage(ctx context.Context, userPrompt string) <-chan StreamEvent {
	out := make(chan StreamEvent)
	runCtx, state, beginErr := q.CoreEngine.beginExecution(ctx)
	if beginErr != nil {
		go func() { defer close(out); sendStream(ctx, out, NewStreamEvent("error", beginErr.Error(), nil)) }()
		return out
	}
	q.RefreshRuntimeConfig()
	q.CoreEngine.AgentRuntime.ResetOutputRecovery(state.RunToken(), q.Config.APIMaxTokens)
	emit := func(event StreamEvent) {
		q.CoreEngine.AgentRuntime.ObserveRunEvent(state.RunToken(), event)
		sendStream(runCtx, out, event)
	}
	go func() {
		defer close(out)
		defer q.CoreEngine.finishExecution(runCtx, state)
		defer q.CoreEngine.SessionRuntime.forwardPermissions(runCtx, out)()
		if strings.HasPrefix(userPrompt, "/") {
			cmd := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(userPrompt, "/")))
			switch cmd {
			case "help", "tokens", "skill", "mcp", "compact", "compress":
				emit(NewStreamEvent("text", fmt.Sprintf("[system] The '%s' command is handled by the REPL.\n", userPrompt), nil))
				emit(NewStreamEvent("done", "", nil))
				return
			}
		}
		ctx = runCtx

		state.Messages = StripTransientContextMessages(state.Messages)
		if state.ContextSnapshot != nil {
			state.ContextSnapshot.Attachments = nil
		}
		state.MemoryQueryText = strings.TrimSpace(userPrompt)
		q.buildOrRefreshSystemPrompt(state)
		normalizedPrompt := userPrompt
		if strings.HasPrefix(userPrompt, "/") {
			skill, args := q.parseSkillInvocation(userPrompt)
			if skill == nil {
				emit(NewStreamEvent("error", "Unknown command: "+userPrompt, nil))
				emit(NewStreamEvent("done", "", nil))
				return
			}
			_ = q.CoreEngine.AgentRuntime.Transition(state.RunToken(), AgentPhaseExecutingTools)
			execution, err := q.executeSlashSkill(ctx, out, *skill, args, state, []string{q.CoreEngine.SessionID, q.CoreEngine.AgentID})
			if err != nil {
				emit(NewStreamEvent("error", err.Error(), nil))
				emit(NewStreamEvent("done", "", nil))
				return
			}
			if err := q.recordSkillInvocation(ctx, *skill, execution, state.TurnCount); err != nil {
				emit(NewStreamEvent("error", err.Error(), map[string]any{"runtime_journal": true}))
				emit(NewStreamEvent("done", "", nil))
				return
			}
			normalizedPrompt = q.normalizeSkillUserPrompt(skill.CanonicalName, args)
			if execution.Mode == "fork" {
				resultText := "Skill '" + skill.CanonicalName + "' completed."
				if execution.ResultText != nil && *execution.ResultText != "" {
					resultText = *execution.ResultText
				}
				// Fork usage belongs to its child runtime and is included in session totals.
				q.commitUserTurn(state, normalizedPrompt)
				if err := q.beginRuntimeRun(ctx, state); err != nil {
					emit(NewStreamEvent("error", err.Error(), map[string]any{"runtime_journal": true}))
					emit(NewStreamEvent("done", "", nil))
					return
				}
				AppendAssistantMessage(state, nil, resultText, nil, "")
				if q.CoreEngine != nil && q.CoreEngine.RuntimeEvents != nil {
					if err := q.CoreEngine.RuntimeEvents.RecordAssistant(ctx, state.Messages[len(state.Messages)-1], harness.UsageRecordedPayload{InputTokens: execution.InputTokens, OutputTokens: execution.OutputTokens}); err != nil {
						_ = q.CoreEngine.RuntimeEvents.EndRun(ctx, "failed", err.Error())
						emit(NewStreamEvent("error", err.Error(), map[string]any{"runtime_journal": true}))
						emit(NewStreamEvent("done", "", nil))
						return
					}
					_ = q.CoreEngine.RuntimeEvents.EndRun(ctx, "completed", "")
				}
				if q.CoreEngine != nil {
					tagLastMessageWithSessionTurn(state)
					q.CoreEngine.RecordSessionMemory(ctx, state, false)
				}
				state.TurnCount++
				_ = q.CoreEngine.publishDraft(context.Background(), state)
				emit(NewStreamEvent("text", resultText, nil))
				emit(NewStreamEvent("done", "", nil))
				return
			}
			if execution.Prompt != nil {
				state.Messages = append(state.Messages, q.CoreEngine.skillRegistryMessage(*skill, *execution.Prompt))
			}
		}
		q.commitUserTurn(state, normalizedPrompt)
		if err := q.beginRuntimeRun(ctx, state); err != nil {
			emit(NewStreamEvent("error", err.Error(), map[string]any{"runtime_journal": true}))
			emit(NewStreamEvent("done", "", nil))
			return
		}
		lastType := ""
		for event := range q.CoreEngine.queryDraftLoop(ctx, state) {
			if event.Type == "done" && q.CoreEngine.RuntimeEvents != nil {
				status := "completed"
				reason := event.Content
				if ctx.Err() != nil || strings.Contains(strings.ToLower(event.Content), "abort") {
					status = "interrupted"
				} else if lastType == "error" {
					status = "failed"
				}
				if err := q.CoreEngine.RuntimeEvents.EndRun(ctx, status, reason); err != nil {
					emit(NewStreamEvent("error", err.Error(), map[string]any{"runtime_journal": true}))
					return
				}
			}
			emit(event)
			if event.Type != "thinking" && event.Type != "tool_progress" {
				lastType = event.Type
			}
		}
	}()
	return out
}

func (q *QueryEngine) beginRuntimeRun(ctx context.Context, state *AgentState) error {
	if q == nil || q.CoreEngine == nil || q.CoreEngine.RuntimeEvents == nil || state == nil || len(state.Messages) == 0 {
		return nil
	}
	message := state.Messages[len(state.Messages)-1]
	if q.CoreEngine.Runtime != nil {
		result, err := q.CoreEngine.Runtime.Hooks.RunStarting.Run(ctx, RunHookContext{SessionID: q.CoreEngine.SessionID, Message: message, State: state})
		if err != nil {
			return err
		}
		message = result.Value.Message
		state.Messages[len(state.Messages)-1] = message
		if err := q.CoreEngine.RuntimeEvents.BeginRun(ctx, message); err != nil {
			return err
		}
		return q.CoreEngine.Runtime.AppendHookEvents(ctx, q.CoreEngine.RuntimeEvents, result.Events)
	}
	return q.CoreEngine.RuntimeEvents.BeginRun(ctx, message)
}

func (q *QueryEngine) buildOrRefreshSystemPrompt(state *AgentState) {
	if state == nil {
		return
	}
	cfg := q.Config
	cfg.CWD = skills.ResolveSkillContextCWD(q.Config.CWD, nil)
	memorySection := ""
	if q.Config.LongTermMemoryEnabled {
		memorySection = agentContext.BuildMemorySection(&q.Config)
	}
	if prompt, err := agentContext.BuildSystemPromptWithConfig(cfg, memorySection); err == nil && strings.TrimSpace(prompt) != "" {
		state.SystemPrompt = appendHarnessSystemPrompt(prompt, cfg.HarnessMode)
		return
	}
	state.SystemPrompt = appendHarnessSystemPrompt(BuildSystemPrompt(cfg.CWD), cfg.HarnessMode)
}

func StripTransientContextMessages(messages []map[string]any) []map[string]any {
	messages = skills.StripSkillContextMessages(messages, nil)
	messages = memory.StripMemoryContextMessages(messages, "")
	return stripTaskNotificationMessages(messages)
}

func stripTaskNotificationMessages(messages []map[string]any) []map[string]any {
	kept := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		metadata, _ := message["metadata"].(map[string]any)
		if source, _ := metadata["source"].(string); source == "task_notification" {
			continue
		}
		kept = append(kept, message)
	}
	return kept
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
		"_session_id":        sid,
		"_agent_id":          q.CoreEngine.sessionMemoryAgentID(),
		"runtime_dir":        q.Config.ProjectRuntimeDir,
		"_skill_persistence": q.CoreEngine.skillPersistence,
		"_skill_agent_scope": "main",
		"_turn_count":        state.TurnCount,
		"_session_runtime":   q.CoreEngine.SessionRuntime,
	}
	return executor.Execute(ctx, skill, args, sid, func(req skills.SkillShellPermissionRequest) bool {
		_ = q.CoreEngine.AgentRuntime.Transition(state.RunToken(), AgentPhaseExecutingTools)
		return q.requestSkillShellPermission(ctx, out, req)
	}, q.CoreEngine.Registry, ClonePermissionState(state.PermissionState), extraContext)
}

func (q *QueryEngine) requestSkillShellPermission(ctx context.Context, out chan<- StreamEvent, req skills.SkillShellPermissionRequest) bool {
	if q.CoreEngine.AgentRuntime.YoloEnabled() {
		return true
	}
	token, ok := runTokenFromContext(ctx)
	if !ok {
		return false
	}
	ticket, event, err := q.CoreEngine.AgentRuntime.BeginPermission(ctx, token, PermissionSkill, "", NewStreamEvent("permission_needed", "", map[string]any{"skill_shell_request": req, "dangerous": true, "risk": "high"}), false)
	if err != nil {
		return false
	}
	if !sendStream(ctx, out, event) {
		q.CoreEngine.AgentRuntime.cancelPermission(ticket)
		return false
	}
	answer, err := q.CoreEngine.AgentRuntime.AwaitPermission(ctx, ticket)
	return err == nil && permissionGranted(answer)
}

func (q *QueryEngine) normalizeSkillUserPrompt(skillName, skillArgs string) string {
	if strings.TrimSpace(skillArgs) != "" {
		return fmt.Sprintf("Use skill '%s' with arguments: %s", skillName, skillArgs)
	}
	return fmt.Sprintf("Use skill '%s'.", skillName)
}

func (q *QueryEngine) recordSkillInvocation(ctx context.Context, skill skills.SkillSpec, execution skills.SkillExecutionResult, turnCount int) error {
	path := skill.SkillFile
	if path == "" {
		path = skill.Directory
	}
	if q.CoreEngine.RuntimeEvents != nil {
		if err := q.CoreEngine.RuntimeEvents.AppendPending(ctx,
			harness.PendingEvent{Type: harness.EventSkillInvoked, Audience: []harness.Audience{harness.AudienceInternal},
				Payload: map[string]any{"name": skill.CanonicalName, "path": path, "mode": execution.Mode, "turn_count": turnCount}},
			harness.PendingEvent{Type: harness.EventSkillContextAttached, Audience: []harness.Audience{harness.AudienceModel, harness.AudienceInternal},
				Payload: map[string]any{"name": skill.CanonicalName, "mode": execution.Mode}},
		); err != nil {
			return err
		}
	}
	if q.CoreEngine.skillPersistence != nil && execution.Prompt != nil && *execution.Prompt != "" {
		q.CoreEngine.skillPersistence.RecordInvocation("main", skill.CanonicalName, path, *execution.Prompt, turnCount)
	}
	return nil
}

func (e *CoreExecutionEngine) skillRegistryMessage(skill skills.SkillSpec, prompt string) map[string]any {
	loader := skills.NewSkillLoader(e.Config)
	executor := skills.NewSkillExecutor(loader, skills.NewPromptProcessor(e.Config))
	return executor.BuildInlineSkillMessage(skill, prompt, true)
}

func (q *QueryEngine) commitUserTurn(state *AgentState, normalizedPrompt string) {
	if !state.MemoryQueryTimeExplicit || state.MemoryQueryTime.IsZero() {
		state.MemoryQueryTime = queryReferenceTimeFromMessages(state.Messages, normalizedPrompt)
		if state.MemoryQueryTime.IsZero() {
			state.MemoryQueryTime = time.Now().UTC()
		}
	}
	state.MemoryQueryTimeExplicit = false
	if stateHasConsecutiveUserPrompt(state, normalizedPrompt) {
		state.LastQuery = normalizedPrompt
		if q.CoreEngine != nil {
			_ = q.CoreEngine.publishDraft(context.Background(), state)
		}
		return
	}
	state.LastQuery = normalizedPrompt
	state.UserTurnCount++
	state.MemoryWritesSinceExtraction = false
	msg := map[string]any{
		"role":    "user",
		"content": []map[string]any{{"type": "text", "text": normalizedPrompt}},
		"metadata": map[string]any{
			"session_user_turn": state.UserTurnCount,
			"timestamp":         state.MemoryQueryTime.Format(time.RFC3339Nano),
		},
	}
	state.Messages = append(state.Messages, msg)
	if q.CoreEngine != nil {
		_ = q.CoreEngine.publishDraft(context.Background(), state)
	}
}

func queryReferenceTimeFromMessages(messages []map[string]any, prompt string) time.Time {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if strings.ToLower(strings.TrimSpace(stringFromAny(message["role"]))) != "user" ||
			strings.TrimSpace(visibleMessageText(message["content"])) != strings.TrimSpace(prompt) {
			continue
		}
		for _, value := range []any{message["timestamp"]} {
			if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(stringFromAny(value))); err == nil {
				return parsed.UTC()
			}
		}
		if metadata, ok := message["metadata"].(map[string]any); ok {
			if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(stringFromAny(metadata["timestamp"]))); err == nil {
				return parsed.UTC()
			}
		}
		break
	}
	return time.Time{}
}

func stateHasConsecutiveUserPrompt(state *AgentState, normalizedPrompt string) bool {
	if state == nil || len(state.Messages) == 0 {
		return false
	}
	last := state.Messages[len(state.Messages)-1]
	if stringFromAny(last["role"]) != "user" {
		return false
	}
	return userMessagePlainText(last["content"]) == strings.TrimSpace(normalizedPrompt)
}

func userMessagePlainText(content any) string {
	if text, ok := content.(string); ok {
		return strings.TrimSpace(text)
	}
	var parts []string
	switch blocks := content.(type) {
	case []map[string]any:
		for _, block := range blocks {
			if stringFromAny(block["type"]) == "text" {
				parts = append(parts, strings.TrimSpace(stringFromAny(block["text"])))
			}
		}
	case []any:
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok || stringFromAny(block["type"]) != "text" {
				continue
			}
			parts = append(parts, strings.TrimSpace(stringFromAny(block["text"])))
		}
	}
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			filtered = append(filtered, strings.TrimSpace(part))
		}
	}
	return strings.TrimSpace(strings.Join(filtered, "\n\n"))
}

func (q *QueryEngine) Checkpoint(ctx context.Context) (AgentSnapshot, error) {
	recovery := q.CoreEngine.ExportSkillRecoverySnapshot()
	var tasks []map[string]any
	if q.CoreEngine.TaskRuntime != nil {
		tasks = q.CoreEngine.TaskRuntime.ExportSnapshot()
	}
	return q.CoreEngine.AgentRuntime.Checkpoint(ctx, recovery, tasks)
}
