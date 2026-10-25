package agent

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"strings"
	"sync"
	"time"

	"LuminaCode/agentContext"
	"LuminaCode/api"
	"LuminaCode/config"
	"LuminaCode/harness"
	"LuminaCode/memory"
	"LuminaCode/security"
	"LuminaCode/skills"
	coretools "LuminaCode/tools"
)

const (
	MaxSubagentTurns                   = 50
	MaxContinuationRetries             = 2
	DefaultSubagentTimeoutSeconds      = 300
	SubagentFinalizeSeconds            = 30
	AgentToolHardTimeoutGraceSecs      = 10
	AgentToolDefaultHardTimeoutSeconds = DefaultSubagentTimeoutSeconds + SubagentFinalizeSeconds + AgentToolHardTimeoutGraceSecs
)

var truncatedStopReasons = stringSet("max_tokens", "length", "token_limit")

type SubAgentSessionState struct {
	Runtime                *AgentRuntime
	draft                  *AgentState
	Messages               []map[string]any
	SystemPrompt           string
	SurfacedMemoryIDs      map[string]struct{}
	RecentToolObservations []map[string]any
	TotalToolUseCount      int
	ScopeID                string
	CurrentTaskID          string
	TotalInputTokens       int
	TotalOutputTokens      int
	AbortCheck             func() bool
}

type SubAgentRequestResult struct {
	FinalText         string
	TotalInputTokens  int
	TotalOutputTokens int
	TimedOut          bool
	Error             error
}

type SubAgent struct {
	Config         config.Config
	Registry       *coretools.ToolRegistry
	Definition     AgentDef
	Permissions    *security.PermissionState
	Runtime        *AgentRuntime
	Model          string
	AgentType      string
	ExtraContext   coretools.ExecutionContext
	ThinkingBudget *int
	MaxTurns       int

	runtimeMu         sync.RWMutex
	executionMu       sync.Mutex
	TotalInputTokens  int
	TotalOutputTokens int
	TimeoutSeconds    int
	scopeID           string
}

func NewSubAgent(cfg config.Config, registry *coretools.ToolRegistry, definition AgentDef, parentState *AgentState, modelOverride, agentType string, extraContext coretools.ExecutionContext, thinkingBudgetTokens ...*int) *SubAgent {
	extra := make(coretools.ExecutionContext, len(extraContext)+1)
	for key, value := range extraContext {
		extra[key] = value
	}
	extra["_session_lease"] = sessionLeaseFrom(extraContext)
	delete(extra, "_child_admission")
	extraContext = extra
	model := modelOverride
	if model == "" {
		model = definition.Model
	}
	if model == "" {
		model = cfg.APIModel
	}
	maxTurns := definition.MaxTurns
	if maxTurns <= 0 {
		maxTurns = MaxSubagentTurns
	}
	var thinkingBudget *int
	if len(thinkingBudgetTokens) > 0 {
		thinkingBudget = thinkingBudgetTokens[0]
	}
	timeoutSeconds := intFromAny(extraContext["subagent_timeout_seconds"])
	if timeoutSeconds <= 0 {
		timeoutSeconds = DefaultSubagentTimeoutSeconds
	}
	return &SubAgent{Config: cfg, Registry: registry, Definition: definition, Permissions: ClonePermissionState(func() *security.PermissionState {
		if parentState != nil {
			return parentState.PermissionState
		}
		return nil
	}()), Model: model, AgentType: agentType, ExtraContext: extraContext, ThinkingBudget: thinkingBudget, MaxTurns: maxTurns, TimeoutSeconds: timeoutSeconds, scopeID: "subagent:" + uuid.NewString()}
}

func (s *SubAgent) Abort() {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	if s.Runtime != nil {
		s.Runtime.Abort()
	}
}

func (s *SubAgent) Run(ctx context.Context, prompt string) (string, error) {
	if !s.executionMu.TryLock() {
		return ErrAgentBusy.Error(), ErrAgentBusy
	}
	defer s.executionMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := s.subagentRunContext(ctx)
	defer cancel()
	sessionState := s.newSessionState()
	result := s.executeOneRequest(runCtx, prompt, sessionState)
	if result.TimedOut && ctx.Err() == nil {
		finalizeCtx, finalizeCancel := context.WithTimeout(ctx, time.Duration(SubagentFinalizeSeconds)*time.Second)
		result = s.finalizeTimedOutRequest(finalizeCtx, prompt, sessionState, result.FinalText)
		finalizeCancel()
	}
	if result.TimedOut {
		result.FinalText = s.wrapTimedOutResult(result.FinalText)
	}
	s.TotalInputTokens += result.TotalInputTokens
	s.TotalOutputTokens += result.TotalOutputTokens
	return result.FinalText, result.Error
}

// CreateSessionState returns a detached session shell. ExecuteOneRequest
// initializes its prompt and recalled memory under an accepted runtime lease.
func (s *SubAgent) CreateSessionState(_ string) *SubAgentSessionState {
	return s.newSessionState()
}

// Session construction performs no I/O. Preparation belongs to the accepted
// run, so Abort can cancel memory recall before any model request is issued.
func (s *SubAgent) newSessionState() *SubAgentSessionState {
	abortCheck, _ := s.ExtraContext["abort_check"].(func() bool)
	if abortCheck == nil {
		abortCheck = func() bool { return false }
	}
	return &SubAgentSessionState{
		ScopeID:       stringFromContext(s.ExtraContext, "scope_id", s.scopeID),
		CurrentTaskID: stringFromContext(s.ExtraContext, "current_task_id", ""),
		AbortCheck:    abortCheck,
	}
}

func (s *SubAgent) ExecuteOneRequest(ctx context.Context, prompt string, sessionState *SubAgentSessionState) (result SubAgentRequestResult) {
	if !s.executionMu.TryLock() {
		return SubAgentRequestResult{Error: ErrAgentBusy, FinalText: ErrAgentBusy.Error()}
	}
	defer s.executionMu.Unlock()
	return s.executeOneRequest(ctx, prompt, sessionState)
}

func (s *SubAgent) executeOneRequest(ctx context.Context, prompt string, sessionState *SubAgentSessionState) (result SubAgentRequestResult) {
	lease := sessionLeaseFrom(s.ExtraContext)
	ctx, release := lease.executionContext(ctx)
	defer release()
	if sessionState == nil {
		sessionState = s.newSessionState()
	}
	defer func() {
		result.TotalInputTokens = sessionState.TotalInputTokens
		result.TotalOutputTokens = sessionState.TotalOutputTokens
	}()
	if sessionState.AbortCheck == nil {
		sessionState.AbortCheck = func() bool { return false }
	}
	if sessionState.SurfacedMemoryIDs == nil {
		sessionState.SurfacedMemoryIDs = map[string]struct{}{}
	}
	if sessionState.Runtime == nil {
		initial := NewAgentState()
		initial.Messages = sessionState.Messages
		initial.SystemPrompt = sessionState.SystemPrompt
		initial.PermissionState = ClonePermissionState(s.Permissions)
		initial.MemorySessionID = s.sessionIDForMemoryUse()
		initial.MemoryAgentID = firstNonEmptyString(sessionState.ScopeID, s.AgentType)
		initial.MemoryAgentType = s.AgentType
		sessionState.Runtime = NewAgentRuntime(s.Config, &initial)
	}
	s.runtimeMu.Lock()
	s.Runtime = sessionState.Runtime
	runCtx, draft, beginErr := s.Runtime.Begin(ctx, s.Config.APIMaxTokens)
	s.runtimeMu.Unlock()
	if beginErr != nil {
		if errors.Is(beginErr, context.Canceled) || errors.Is(beginErr, context.DeadlineExceeded) {
			return s.contextStopResult(sessionState, "", beginErr)
		}
		return SubAgentRequestResult{Error: beginErr, FinalText: beginErr.Error()}
	}
	ctx = runCtx
	sessionState.draft = draft
	// Run-scoped hints expire between reusable-worker requests. New inline hints
	// remain in sessionState.Messages and are collected into this run below.
	draft.ContextSnapshot.Attachments = nil
	defer s.Runtime.EndRun(draft.RunToken())
	defer func() {
		if ctx.Err() != nil && !errors.Is(result.Error, ctx.Err()) {
			result = s.contextStopResult(sessionState, result.FinalText, ctx.Err())
		}
		draft.Messages = sessionState.Messages
		draft.SystemPrompt = sessionState.SystemPrompt
		draft.TotalInputTokens = sessionState.TotalInputTokens
		draft.TotalOutputTokens = sessionState.TotalOutputTokens
		if err := s.Runtime.Commit(context.WithoutCancel(ctx), draft); err != nil && result.Error == nil {
			result.Error = err
		}
		if result.Error != nil {
			s.Runtime.FailRun(draft.RunToken(), result.Error.Error())
		}
	}()
	if err := lease.register(ctx, draft.MemoryAgentID, s.Runtime); err != nil {
		return SubAgentRequestResult{Error: err, FinalText: err.Error()}
	}
	if store, ok := s.ExtraContext["_runtime_store"].(harness.EventStore); ok && !s.Runtime.HasDurableStore() {
		if err := s.Runtime.BindStoreContext(ctx, store, firstNonEmptyString(sessionState.ScopeID, draft.MemoryAgentID)); err != nil {
			return SubAgentRequestResult{Error: err, FinalText: err.Error()}
		}
	}
	if sessionState.Messages == nil {
		sessionState.Messages = s.buildInitialMessages(ctx, prompt)
		sessionState.SurfacedMemoryIDs = memory.RecalledMemoryIDs(sessionState.Messages)
	}
	if ctx.Err() != nil {
		return s.contextStopResult(sessionState, "", ctx.Err())
	}
	if sessionState.SystemPrompt == "" {
		sessionState.SystemPrompt = s.buildSystemPrompt()
	}
	messages := sessionState.Messages
	systemPrompt := sessionState.SystemPrompt
	shouldDrainNotifications := true
	fullText := ""

requestLoop:
	for turn := 0; turn < s.MaxTurns; turn++ {
		if ctx.Err() != nil {
			return s.contextStopResult(sessionState, fullText, ctx.Err())
		}
		if sessionState.AbortCheck() {
			s.Runtime.cancelRun(draft.RunToken(), "worker stopped")
			return s.contextStopResult(sessionState, fullText, context.Canceled)
		}
		if shouldDrainNotifications {
			s.drainPendingNotifications(sessionState)
			messages = sessionState.Messages
		}
		shouldDrainNotifications = true

		if err := s.Runtime.Transition(draft.RunToken(), AgentPhasePreparing); err != nil {
			return SubAgentRequestResult{Error: err, FinalText: err.Error()}
		}
		draft.Messages = messages
		runtime := skills.CollectInlineSkillRuntime(contextSkillMessages(draft))
		activeRegistry := s.Registry
		if runtime.HasAllowedTools {
			allow := map[string]struct{}{}
			for _, name := range runtime.AllowedToolNames {
				allow[name] = struct{}{}
			}
			activeRegistry = s.Registry.FilteredCopy(allow, nil, false, false)
		}
		model := s.Model
		if runtime.ModelOverride != nil && *runtime.ModelOverride != "" {
			model = *runtime.ModelOverride
		}
		client, err := CreateConfiguredLLMClient(s.Config, model, s.Config.APIMaxTokens, s.ThinkingBudget, api.DefaultRetryConfigPtr())
		if err != nil {
			return SubAgentRequestResult{
				Error:             err,
				FinalText:         "Sub-agent API call failed: " + err.Error(),
				TotalInputTokens:  sessionState.TotalInputTokens,
				TotalOutputTokens: sessionState.TotalOutputTokens,
			}
		}
		toolSchemas := activeRegistry.GetAPISchemas()
		draft.Messages = messages
		draft.TotalInputTokens = sessionState.TotalInputTokens
		draft.TotalOutputTokens = sessionState.TotalOutputTokens
		draft.SystemPrompt = systemPrompt
		if err := s.Runtime.Commit(ctx, draft); err != nil {
			return SubAgentRequestResult{Error: err, FinalText: err.Error()}
		}
		cfg := s.Config
		cfg.APIModel = model
		s.Runtime.Context.Configure(cfg)
		if agentContext.RequestBudget(systemPrompt, agentContext.Render(s.Runtime.Context.Snapshot()), toolSchemas, cfg.CompressionContextLimit()).Total > cfg.CompressionTriggerTokens() {
			_ = s.Runtime.StartRecovery(draft.RunToken(), RecoveryContext, "context compression")
		}
		prepared, prepareErr := s.Runtime.Context.Build(ctx, agentContext.BuildInput{SystemPrompt: systemPrompt, ToolSchemas: toolSchemas, Evidence: s.Runtime.Snapshot().Transcript, Normalize: prepareSubagentAPIMessages})
		if prepared.HistoryReplaced {
			systemPrompt = s.buildSystemPrompt()
			sessionState.SystemPrompt = systemPrompt
			prepared.Next.SystemPrompt = systemPrompt
			prepared.Compiled.SystemPrompt = systemPrompt
			prepared.Compiled, prepareErr = s.Runtime.Context.Validate(prepared.Compiled)
			prepared.Next.Budget = prepared.Compiled.Budget
		}
		if ctx.Err() != nil {
			return s.contextStopResult(sessionState, fullText, ctx.Err())
		}
		if err := s.Runtime.CommitContext(ctx, prepared, draft); err != nil {
			return SubAgentRequestResult{Error: err, FinalText: err.Error()}
		}
		sessionState.TotalInputTokens = draft.TotalInputTokens
		sessionState.TotalOutputTokens = draft.TotalOutputTokens
		if prepareErr != nil {
			return SubAgentRequestResult{Error: prepareErr, FinalText: "Sub-agent context failed: " + prepareErr.Error(), TotalInputTokens: sessionState.TotalInputTokens, TotalOutputTokens: sessionState.TotalOutputTokens}
		}
		messages = draft.Messages
		sessionState.Messages = messages
		requestID := uuid.NewString()
		usageRecorded := false
		var toolCalls []coretools.ToolCall
		fullText = ""
		var thinkingBlocks []map[string]any
		currentMessageID := ""
		outputTruncated := false

		streamCtx := api.ContextWithStreamIdleTimeout(ctx, time.Duration(s.Config.APIStreamIdleTimeoutSeconds*float64(time.Second)))
		if err := s.Runtime.Transition(draft.RunToken(), AgentPhaseCallingModel); err != nil {
			return SubAgentRequestResult{Error: err, FinalText: err.Error()}
		}
		for result := range client.StreamChat(streamCtx, systemPrompt, prepared.Compiled.Messages, toolSchemas, nil) {
			if result.Err != nil {
				if ctx.Err() == nil && IsPTLErrorMessage(result.Err.Error()) && s.Runtime.TryRecovery(draft.RunToken(), RecoveryContext, 3) {
					recovery, recoveryErr := s.Runtime.Context.Compact(ctx, agentContext.CompactReactive)
					if recoveryErr == nil && recovery.Succeeded && s.Runtime.CommitContext(ctx, recovery, draft) == nil {
						messages = draft.Messages
						sessionState.Messages = messages
						continue requestLoop
					}
				}
				if ctx.Err() != nil {
					return s.contextStopResult(sessionState, fullText, ctx.Err())
				}
				return SubAgentRequestResult{
					Error:             result.Err,
					FinalText:         "Sub-agent API call failed: " + result.Err.Error(),
					TotalInputTokens:  sessionState.TotalInputTokens,
					TotalOutputTokens: sessionState.TotalOutputTokens,
				}
			}
			event := result.Event
			switch stringFromAny(event["type"]) {
			case "text_delta":
				fullText += stringFromAny(event["text"])
			case "tool_use":
				input := map[string]any{}
				if raw, ok := event["input"].(map[string]any); ok {
					input = raw
				}
				toolCalls = append(toolCalls, coretools.ToolCall{ID: stringFromAny(event["id"]), Name: stringFromAny(event["name"]), Input: input})
			case "thinking":
				thinkingBlocks = append(thinkingBlocks, map[string]any{"type": "thinking", "thinking": stringFromAny(event["text"]), "signature": stringFromAny(event["signature"])})
			case "redacted_thinking":
				thinkingBlocks = append(thinkingBlocks, map[string]any{"type": "redacted_thinking", "data": stringFromAny(event["data"])})
			case "thinking_delta":
				if len(thinkingBlocks) > 0 && thinkingBlocks[len(thinkingBlocks)-1]["type"] == "thinking" {
					last := thinkingBlocks[len(thinkingBlocks)-1]
					last["thinking"] = stringFromAny(last["thinking"]) + stringFromAny(event["text"])
				}
			case "signature_delta":
				if len(thinkingBlocks) > 0 && thinkingBlocks[len(thinkingBlocks)-1]["type"] == "thinking" {
					last := thinkingBlocks[len(thinkingBlocks)-1]
					last["signature"] = stringFromAny(last["signature"]) + stringFromAny(event["signature"])
				}
			case "message_id":
				currentMessageID = stringFromAny(event["id"])
			case "stop_reason":
				reason := stringFromAny(event["stop_reason"])
				if reason == "" {
					reason = stringFromAny(event["reason"])
				}
				if _, ok := truncatedStopReasons[reason]; ok {
					outputTruncated = true
				}
			case "usage":
				if usageRecorded {
					continue
				}
				usageRecorded = true
				if draft.UsageRequests == nil {
					draft.UsageRequests = map[string]bool{}
				}
				draft.UsageRequests[requestID] = true
				sessionState.TotalInputTokens += intFromAny(event["input_tokens"])
				sessionState.TotalOutputTokens += intFromAny(event["output_tokens"])
			case "error":
				if ctx.Err() != nil {
					return s.contextStopResult(sessionState, fullText, ctx.Err())
				}
				msg := stringFromAny(event["message"])
				if IsPTLErrorMessage(msg) && s.Runtime.TryRecovery(draft.RunToken(), RecoveryContext, 3) {
					recovery, recoveryErr := s.Runtime.Context.Compact(ctx, agentContext.CompactReactive)
					if recoveryErr == nil && recovery.Succeeded && s.Runtime.CommitContext(ctx, recovery, draft) == nil {
						messages = draft.Messages
						sessionState.Messages = messages
						continue requestLoop
					}
				}
				return SubAgentRequestResult{
					Error:             fmt.Errorf("%s", msg),
					FinalText:         "Sub-agent error: " + msg,
					TotalInputTokens:  sessionState.TotalInputTokens,
					TotalOutputTokens: sessionState.TotalOutputTokens,
				}
			}
		}
		if ctx.Err() != nil {
			return s.contextStopResult(sessionState, fullText, ctx.Err())
		}

		assistantContent := append([]map[string]any{}, thinkingBlocks...)
		if fullText != "" {
			assistantContent = append(assistantContent, map[string]any{"type": "text", "text": fullText})
		}
		for _, tc := range toolCalls {
			assistantContent = append(assistantContent, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": tc.Input})
		}
		assistantMsg := map[string]any{"role": "assistant", "content": assistantContent}
		if currentMessageID != "" {
			assistantMsg["id"] = currentMessageID
		}
		messages = append(messages, assistantMsg)
		sessionState.Messages = messages

		if outputTruncated && s.Runtime.TryRecovery(draft.RunToken(), RecoveryOutput, MaxContinuationRetries) {
			messages = append(messages, map[string]any{"role": "user", "content": []map[string]any{{
				"type": "text",
				"text": "Your previous response was cut off due to output length limits. Please continue exactly where you left off. Do not repeat any content you already generated.",
			}}})
			sessionState.Messages = messages
			shouldDrainNotifications = false
			continue
		}

		if len(toolCalls) == 0 {
			if fullText == "" {
				fullText = "(Sub-agent produced no output.)"
			}
			return SubAgentRequestResult{FinalText: fullText, TotalInputTokens: sessionState.TotalInputTokens, TotalOutputTokens: sessionState.TotalOutputTokens}
		}

		if err := s.Runtime.Transition(draft.RunToken(), AgentPhaseExecutingTools); err != nil {
			return SubAgentRequestResult{Error: err, FinalText: err.Error()}
		}
		execCtx := s.buildExecutionContext(draft.RunToken())
		execCtx["_registry"] = activeRegistry
		var toolResults []map[string]any
		var observations []map[string]any
		for _, tc := range toolCalls {
			result := activeRegistry.Execute(ctx, tc, execCtx)
			tool := activeRegistry.Get(tc.Name)
			content := result.Content
			if tool != nil && !result.IsError {
				if formatted, err := tool.FormatLargeResult(ctx, content, s.Config.MaxToolOutputChars, tc.ID, s.Config.ToolResultsDir(s.sessionIDForMemoryUse())); err == nil {
					content = formatted
				}
			}
			toolResults = append(toolResults, map[string]any{"type": "tool_result", "tool_use_id": tc.ID, "content": content})
			observations = append(observations, map[string]any{"call": tc, "result": result, "tool": tool, "content": content})
		}
		sessionState.RecentToolObservations = observations
		sessionState.TotalToolUseCount += len(observations)
		s.Runtime.ResetContinuationRetries(draft.RunToken())
		if len(toolResults) > 0 {
			messages = append(messages, map[string]any{"role": "user", "content": toolResults})
			if pending, ok := execCtx["_pending_skill_messages"].([]map[string]any); ok && len(pending) > 0 {
				messages = append(messages, pending...)
				execCtx["_pending_skill_messages"] = []map[string]any{}
			}
			sessionState.Messages = messages
		}
	}
	last := "(none)"
	if fullText != "" {
		last = TruncateResult(fullText, 500)
	}
	return SubAgentRequestResult{
		Error:             fmt.Errorf("sub-agent reached maximum turns"),
		FinalText:         fmt.Sprintf("Sub-agent reached maximum turns (%d). Last response: %s", s.MaxTurns, last),
		TotalInputTokens:  sessionState.TotalInputTokens,
		TotalOutputTokens: sessionState.TotalOutputTokens,
	}
}

func (s *SubAgent) subagentRunContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	softDeadline := time.Now().Add(time.Duration(s.timeoutSeconds()) * time.Second)
	if parentDeadline, ok := ctx.Deadline(); ok && parentDeadline.Before(softDeadline) {
		return context.WithDeadline(ctx, parentDeadline)
	}
	return context.WithDeadline(ctx, softDeadline)
}

func (s *SubAgent) timeoutSeconds() int {
	if s.TimeoutSeconds > 0 {
		return s.TimeoutSeconds
	}
	return DefaultSubagentTimeoutSeconds
}

func (s *SubAgent) contextStopResult(sessionState *SubAgentSessionState, currentText string, err error) SubAgentRequestResult {
	if errors.Is(err, context.Canceled) {
		return SubAgentRequestResult{
			Error:             err,
			FinalText:         "Sub-agent aborted by user.",
			TotalInputTokens:  sessionState.TotalInputTokens,
			TotalOutputTokens: sessionState.TotalOutputTokens,
		}
	}
	return SubAgentRequestResult{
		Error:             err,
		FinalText:         s.partialTimeoutText(sessionState, currentText),
		TotalInputTokens:  sessionState.TotalInputTokens,
		TotalOutputTokens: sessionState.TotalOutputTokens,
		TimedOut:          true,
	}
}

func (s *SubAgent) finalizeTimedOutRun(ctx context.Context, prompt string, sessionState *SubAgentSessionState, fallback string) SubAgentRequestResult {
	if !s.executionMu.TryLock() {
		return SubAgentRequestResult{Error: ErrAgentBusy, FinalText: fallback, TimedOut: true}
	}
	defer s.executionMu.Unlock()
	return s.finalizeTimedOutRequest(ctx, prompt, sessionState, fallback)
}

func (s *SubAgent) finalizeTimedOutRequest(ctx context.Context, prompt string, sessionState *SubAgentSessionState, fallback string) (result SubAgentRequestResult) {
	result = SubAgentRequestResult{FinalText: fallback, TimedOut: true}
	if sessionState == nil || sessionState.Runtime == nil {
		result.Error = errors.New("sub-agent finalization requires an initialized runtime")
		return
	}
	result.TotalInputTokens = sessionState.TotalInputTokens
	result.TotalOutputTokens = sessionState.TotalOutputTokens
	runtime := sessionState.Runtime
	s.runtimeMu.Lock()
	s.Runtime = runtime
	var runCtx context.Context
	var draft *AgentState
	var err error
	if sessionState.draft != nil {
		runCtx, draft, err = runtime.beginRecoveryRun(ctx, sessionState.draft.RunToken(), s.Config.APIMaxTokens)
	} else {
		runCtx, draft, err = runtime.Begin(ctx, s.Config.APIMaxTokens)
	}
	s.runtimeMu.Unlock()
	if err != nil {
		result.Error = err
		return
	}
	ctx = runCtx
	defer runtime.EndRun(draft.RunToken())
	defer func() {
		result.TotalInputTokens = sessionState.TotalInputTokens
		result.TotalOutputTokens = sessionState.TotalOutputTokens
		if ctx.Err() != nil {
			result.Error = ctx.Err()
		}
		draft.Messages = sessionState.Messages
		draft.SystemPrompt = sessionState.SystemPrompt
		draft.TotalInputTokens = sessionState.TotalInputTokens
		draft.TotalOutputTokens = sessionState.TotalOutputTokens
		if commitErr := runtime.Commit(context.WithoutCancel(ctx), draft); commitErr != nil && result.Error == nil {
			result.Error = commitErr
		}
		if result.Error != nil {
			runtime.FailRun(draft.RunToken(), result.Error.Error())
		}
	}()
	if result.Error = runtime.StartRecovery(draft.RunToken(), RecoveryOutput, "sub-agent timeout finalization"); result.Error != nil {
		return
	}
	finalPrompt := fmt.Sprintf("You have reached the %d second sub-agent time limit. Stop using tools. Based only on the original task, conversation, tool results already available above, and the runtime partial-progress summary below, return the most useful final answer now. Be concise but include concrete findings, uncertainties, and next steps.\n\nOriginal task:\n%s\n\nRuntime partial-progress summary:\n%s", s.timeoutSeconds(), prompt, fallback)
	messages := append([]map[string]any{}, sessionState.Messages...)
	messages = append(messages, map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": finalPrompt}}})
	model := firstNonEmptyString(s.Model, s.Config.APIModel)
	client, err := CreateConfiguredLLMClient(s.Config, model, s.Config.APIMaxTokens, s.ThinkingBudget, api.DefaultRetryConfigPtr())
	if err != nil {
		result.Error = err
		return
	}
	draft.Messages = messages
	draft.SystemPrompt = sessionState.SystemPrompt
	if result.Error = runtime.Commit(ctx, draft); result.Error != nil {
		return
	}
	// Once the prompt is committed, all exits preserve that same working view.
	sessionState.Messages = messages
	prepared, prepareErr := runtime.Context.Build(ctx, agentContext.BuildInput{SystemPrompt: sessionState.SystemPrompt, Normalize: prepareSubagentAPIMessages, Evidence: runtime.Snapshot().Transcript})
	if result.Error = runtime.CommitContext(ctx, prepared, draft); result.Error != nil {
		return
	}
	sessionState.Messages = draft.Messages
	if prepareErr != nil {
		result.Error = prepareErr
		return
	}
	if result.Error = runtime.Transition(draft.RunToken(), AgentPhaseCallingModel); result.Error != nil {
		return
	}
	var fullText strings.Builder
	requestID := uuid.NewString()
	usageRecorded := false
	streamCtx := api.ContextWithStreamIdleTimeout(ctx, time.Duration(s.Config.APIStreamIdleTimeoutSeconds*float64(time.Second)))
	for item := range client.StreamChat(streamCtx, prepared.Compiled.SystemPrompt, prepared.Compiled.Messages, nil, nil) {
		if item.Err != nil {
			result.Error = item.Err
			return
		}
		event := item.Event
		switch stringFromAny(event["type"]) {
		case "error":
			result.Error = fmt.Errorf("sub-agent finalization failed: %s", stringFromAny(event["message"]))
			return
		case "text_delta":
			fullText.WriteString(stringFromAny(event["text"]))
		case "usage":
			if usageRecorded {
				continue
			}
			usageRecorded = true
			if draft.UsageRequests == nil {
				draft.UsageRequests = map[string]bool{}
			}
			draft.UsageRequests[requestID] = true
			sessionState.TotalInputTokens += intFromAny(event["input_tokens"])
			sessionState.TotalOutputTokens += intFromAny(event["output_tokens"])
		}
	}
	if ctx.Err() != nil {
		result.Error = ctx.Err()
		return
	}
	text := strings.TrimSpace(fullText.String())
	if text == "" {
		result.Error = errors.New("sub-agent finalization produced no output")
		return
	}
	sessionState.Messages = append(sessionState.Messages, map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": text}}})
	result.FinalText = text
	return
}

func (s *SubAgent) partialTimeoutText(sessionState *SubAgentSessionState, currentText string) string {
	var sections []string
	if strings.TrimSpace(currentText) != "" {
		sections = append(sections, "Partial assistant text:\n"+strings.TrimSpace(currentText))
	} else if text := latestAssistantText(sessionState.Messages); text != "" {
		sections = append(sections, "Latest assistant text:\n"+text)
	}
	if observations := formatRecentSubagentObservations(sessionState.RecentToolObservations, 6); len(observations) > 0 {
		sections = append(sections, "Recent tool observations:\n"+strings.Join(observations, "\n"))
	}
	if len(sections) == 0 {
		sections = append(sections, "No text or tool observations were produced before the timeout.")
	}
	return fmt.Sprintf("Sub-agent reached its %ds timeout and is returning partial progress instead of a tool error.\n\n%s", s.timeoutSeconds(), strings.Join(sections, "\n\n"))
}

func (s *SubAgent) wrapTimedOutResult(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		text = "No final text was produced before the timeout."
	}
	return fmt.Sprintf("[Sub-agent timeout]\nThis sub-agent reached its %d second timeout. The answer below is based only on information already collected before timeout. If this is insufficient, the main agent may ask this sub-agent, or a narrower follow-up sub-agent, to continue querying from a more focused prompt.\n\n%s", s.timeoutSeconds(), text)
}

func latestAssistantText(messages []map[string]any) string {
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		if stringFromAny(message["role"]) != "assistant" {
			continue
		}
		var parts []string
		for _, block := range contentBlocks(message["content"]) {
			if stringFromAny(block["type"]) == "text" {
				if text := strings.TrimSpace(stringFromAny(block["text"])); text != "" {
					parts = append(parts, text)
				}
			}
		}
		if len(parts) > 0 {
			return strings.TrimSpace(strings.Join(parts, "\n\n"))
		}
	}
	return ""
}

func formatRecentSubagentObservations(observations []map[string]any, limit int) []string {
	if limit <= 0 || len(observations) == 0 {
		return nil
	}
	start := len(observations) - limit
	if start < 0 {
		start = 0
	}
	out := make([]string, 0, len(observations)-start)
	for _, observation := range observations[start:] {
		toolName := "tool"
		if tc, ok := observation["call"].(coretools.ToolCall); ok && tc.Name != "" {
			toolName = tc.Name
		}
		content := strings.TrimSpace(stringFromAny(observation["content"]))
		if content == "" {
			content = "(empty result)"
		}
		out = append(out, fmt.Sprintf("- %s: %s", toolName, TruncateResult(strings.Split(content, "\n")[0], 240)))
	}
	return out
}

func (s *SubAgent) buildSystemPrompt() string {
	if override, _ := s.ExtraContext["system_prompt_override"].(string); strings.TrimSpace(override) != "" {
		return override
	}
	cwd := s.Config.CWD
	if wt, _ := s.ExtraContext["worktree_cwd"].(string); wt != "" {
		cwd = wt
	}
	gitContext := agentContext.GetGitContext(cwd, 3.0, true)
	agentMemory := ""
	if s.Config.LongTermMemoryEnabled {
		agentMemory = s.longTermAgentMemoryBehavior()
	}
	sections, err := agentContext.BuildSubagentPromptSections(s.Definition.Name, s.Definition.Description, cwd, s.MaxTurns, gitContext, agentMemory)
	if err == nil {
		return agentContext.AssemblePromptSections(sections)
	}
	prompt := BuildSystemPrompt(cwd) + "\n\nSub-agent: " + s.Definition.Name + "\n" + s.Definition.Description
	if agentMemory != "" {
		prompt += agentMemory
	}
	return prompt
}

func (s *SubAgent) buildInitialMessages(ctx context.Context, prompt string) []map[string]any {
	var messages []map[string]any
	if s.Config.LongTermMemoryEnabled {
		engine, _ := s.ExtraContext["_memory_engine"].(memory.Engine)
		state := &AgentState{MemorySessionID: s.sessionIDForMemoryUse(), MemoryAgentType: s.AgentType,
			MemoryQueryTime: time.Now().UTC()}
		if recalled := RunMemoryRecallWithEngine(ctx, s.Config, state, prompt, engine); len(recalled) > 0 {
			if msg := memory.BuildRecalledMemoriesMessage(recalled); msg != nil {
				messages = append(messages, msg)
			}
		}
	}
	messages = append(messages, map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": prompt}}})
	return messages
}

func (s *SubAgent) longTermAgentMemoryBehavior() string {
	agentType := strings.TrimSpace(s.AgentType)
	if agentType == "" {
		agentType = strings.TrimSpace(s.Definition.Name)
	}
	if agentType == "" {
		agentType = "subagent"
	}
	return fmt.Sprintf("## Role Long-Term Memory\n\nAgent-type memory: `%s`.\n\nThis sub-agent may receive long-term memories for user, project, and agent_type scopes. Treat recalled memories as durable hints and verify current project files before relying on code or path claims. Do not write memory files directly; durable memories are extracted by LuminaCode's structured memory runtime.", agentType)
}

func (s *SubAgent) sessionIDForMemoryUse() string {
	if value, _ := s.ExtraContext["_session_id"].(string); value != "" {
		return value
	}
	if value, _ := s.ExtraContext["session_id"].(string); value != "" {
		return value
	}
	return ""
}

func (s *SubAgent) buildExecutionContext(tokens ...RunToken) coretools.ExecutionContext {
	s.runtimeMu.RLock()
	runtime := s.Runtime
	s.runtimeMu.RUnlock()
	execCtx := coretools.ExecutionContext{
		"cwd":                     s.Config.CWD,
		"config":                  s.Config,
		"runtime_dir":             s.Config.ProjectRuntimeDir,
		"web_search_scope":        s.Config.WebSearchCacheScope,
		"allowed_read_roots":      compactToolRoots(s.Config.CWD, s.Config.ProjectRuntimeDir),
		"allowed_write_roots":     compactToolRoots(s.Config.CWD, s.Config.ProjectRuntimeDir),
		"agent_services":          runtime,
		"spawn_permissions":       ClonePermissionState(s.Permissions),
		"_registry":               s.Registry,
		"_pending_skill_messages": []map[string]any{},
	}
	for key, value := range s.ExtraContext {
		execCtx[key] = value
	}
	scope := stringFromContext(s.ExtraContext, "scope_id", s.scopeID)
	if runtime != nil {
		snapshot := runtime.Snapshot()
		scope = firstNonEmptyString(snapshot.Identity.AgentID, scope)
		token := runtime.RunSnapshot().Token
		if len(tokens) > 0 {
			token = tokens[0]
		}
		execCtx["agent_services"] = runtime.ServicesFor(token)
		execCtx["spawn_permissions"] = ClonePermissionState(snapshot.Permissions.Policy)
	}
	execCtx["scope_id"] = scope
	execCtx["_agent_id"] = scope
	execCtx["_skill_agent_scope"] = scope
	if runtime != nil {
		token := runtime.RunSnapshot().Token
		if len(tokens) > 0 {
			token = tokens[0]
		}
		execCtx["_request_skill_shell_permission"] = func(req skills.SkillShellPermissionRequest) bool {
			ctx := runtime.contextForRun(token)
			if ctx.Err() != nil {
				return false
			}
			if runtime.YoloEnabled() {
				return true
			}
			ticket, event, err := runtime.BeginPermission(ctx, token, PermissionSkill, "", NewStreamEvent("permission_needed", "", map[string]any{"skill_shell_request": req, "agent_id": scope, "dangerous": true, "risk": "high"}), false)
			if err != nil {
				return false
			}
			session, ok := s.ExtraContext["_session_runtime"].(*SessionRuntime)
			if !ok || !session.PublishPermission(ctx, event) {
				runtime.cancelPermission(ticket)
				return false
			}
			answer, err := runtime.AwaitPermission(ctx, ticket)
			return err == nil && permissionGranted(answer)
		}
	}
	if wt, _ := s.ExtraContext["worktree_cwd"].(string); wt != "" {
		execCtx["cwd"] = wt
	}
	return execCtx
}

func (s *SubAgent) drainPendingNotifications(sessionState *SubAgentSessionState) {
	raw := s.ExtraContext["_drain_pending_notifications"]
	switch drain := raw.(type) {
	case func(string) []map[string]any:
		if messages := drain(sessionState.ScopeID); len(messages) > 0 {
			sessionState.Messages = append(sessionState.Messages, messages...)
		}
	case func(string, string) []map[string]any:
		if messages := drain(sessionState.ScopeID, sessionState.CurrentTaskID); len(messages) > 0 {
			sessionState.Messages = append(sessionState.Messages, messages...)
		}
	}
}

func (s *SubAgent) resolveProjectRoot() string {
	return s.Config.ProjectRoot()
}

func prepareSubagentAPIMessages(messages []map[string]any) []map[string]any {
	cleaned := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		cp := map[string]any{}
		for key, value := range msg {
			if key == "metadata" || key == "isMeta" {
				continue
			}
			cp[key] = value
		}
		cleaned = append(cleaned, cp)
	}
	return cleaned
}
