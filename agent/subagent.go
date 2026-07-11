package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"LuminaCode/agentContext"
	"LuminaCode/api"
	"LuminaCode/config"
	"LuminaCode/longmemory"
	"LuminaCode/memory"
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
}

type SubAgent struct {
	Config            config.Config
	Registry          *coretools.ToolRegistry
	Definition        AgentDef
	ParentState       *AgentState
	Model             string
	AgentType         string
	ExtraContext      coretools.ExecutionContext
	ThinkingBudget    *int
	MaxTurns          int
	Aborted           bool
	TotalInputTokens  int
	TotalOutputTokens int
	TimeoutSeconds    int
}

func NewSubAgent(cfg config.Config, registry *coretools.ToolRegistry, definition AgentDef, parentState *AgentState, modelOverride, agentType string, extraContext coretools.ExecutionContext, thinkingBudgetTokens ...*int) *SubAgent {
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
	return &SubAgent{Config: cfg, Registry: registry, Definition: definition, ParentState: parentState, Model: model, AgentType: agentType, ExtraContext: extraContext, ThinkingBudget: thinkingBudget, MaxTurns: maxTurns, TimeoutSeconds: timeoutSeconds}
}

func (s *SubAgent) Abort() {
	s.Aborted = true
}

func (s *SubAgent) Run(ctx context.Context, prompt string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := s.subagentRunContext(ctx)
	defer cancel()
	sessionState := s.createSessionState(runCtx, prompt)
	result := s.ExecuteOneRequest(runCtx, prompt, sessionState)
	if result.TimedOut && ctx.Err() == nil {
		finalizeCtx, finalizeCancel := context.WithTimeout(ctx, time.Duration(SubagentFinalizeSeconds)*time.Second)
		result = s.finalizeTimedOutRun(finalizeCtx, prompt, sessionState, result.FinalText)
		finalizeCancel()
	}
	if result.TimedOut {
		result.FinalText = s.wrapTimedOutResult(result.FinalText)
	}
	s.TotalInputTokens += result.TotalInputTokens
	s.TotalOutputTokens += result.TotalOutputTokens
	return result.FinalText, nil
}

func (s *SubAgent) CreateSessionState(prompt string) *SubAgentSessionState {
	return s.createSessionState(context.Background(), prompt)
}

func (s *SubAgent) createSessionState(ctx context.Context, prompt string) *SubAgentSessionState {
	abortCheck, _ := s.ExtraContext["abort_check"].(func() bool)
	if abortCheck == nil {
		abortCheck = func() bool { return false }
	}
	messages := s.buildInitialMessages(ctx, prompt)
	return &SubAgentSessionState{
		Messages:          messages,
		SystemPrompt:      s.buildSystemPrompt(),
		SurfacedMemoryIDs: memory.RecalledMemoryIDs(messages),
		ScopeID:           stringFromContext(s.ExtraContext, "scope_id", ""),
		CurrentTaskID:     stringFromContext(s.ExtraContext, "current_task_id", ""),
		AbortCheck:        abortCheck,
	}
}

func (s *SubAgent) ExecuteOneRequest(ctx context.Context, prompt string, sessionState *SubAgentSessionState) SubAgentRequestResult {
	if sessionState == nil {
		sessionState = s.createSessionState(ctx, prompt)
	}
	if sessionState.AbortCheck == nil {
		sessionState.AbortCheck = func() bool { return false }
	}
	if sessionState.SurfacedMemoryIDs == nil {
		sessionState.SurfacedMemoryIDs = map[string]struct{}{}
	}
	messages := sessionState.Messages
	systemPrompt := sessionState.SystemPrompt
	continuationRetries := 0
	shouldDrainNotifications := true
	fullText := ""

	for turn := 0; turn < s.MaxTurns; turn++ {
		if ctx.Err() != nil {
			return s.contextStopResult(sessionState, fullText, ctx.Err())
		}
		if s.Aborted || sessionState.AbortCheck() {
			return SubAgentRequestResult{
				FinalText:         "Sub-agent aborted by user.",
				TotalInputTokens:  sessionState.TotalInputTokens,
				TotalOutputTokens: sessionState.TotalOutputTokens,
			}
		}
		if shouldDrainNotifications {
			s.drainPendingNotifications(sessionState)
		}
		shouldDrainNotifications = true

		runtime := skills.CollectInlineSkillRuntime(messages)
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
				FinalText:         "Sub-agent API call failed: " + err.Error(),
				TotalInputTokens:  sessionState.TotalInputTokens,
				TotalOutputTokens: sessionState.TotalOutputTokens,
			}
		}
		toolSchemas := activeRegistry.GetAPISchemas()
		var toolCalls []coretools.ToolCall
		fullText = ""
		var thinkingBlocks []map[string]any
		currentMessageID := ""
		outputTruncated := false

		streamCtx := api.ContextWithStreamIdleTimeout(ctx, time.Duration(s.Config.APIStreamIdleTimeoutSeconds*float64(time.Second)))
		for result := range client.StreamChat(streamCtx, systemPrompt, prepareSubagentAPIMessages(messages), toolSchemas, nil) {
			if result.Err != nil {
				if ctx.Err() != nil {
					return s.contextStopResult(sessionState, fullText, ctx.Err())
				}
				return SubAgentRequestResult{
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
				sessionState.TotalInputTokens += intFromAny(event["input_tokens"])
				sessionState.TotalOutputTokens += intFromAny(event["output_tokens"])
			case "error":
				if ctx.Err() != nil {
					return s.contextStopResult(sessionState, fullText, ctx.Err())
				}
				msg := stringFromAny(event["message"])
				return SubAgentRequestResult{
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

		if outputTruncated && continuationRetries < MaxContinuationRetries {
			continuationRetries++
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

		execCtx := s.buildExecutionContext()
		execCtx["_registry"] = activeRegistry
		var toolResults []map[string]any
		var observations []map[string]any
		for _, tc := range toolCalls {
			result := activeRegistry.Execute(ctx, tc, execCtx)
			tool := activeRegistry.Get(tc.Name)
			content := result.Content
			if tool != nil && !result.IsError {
				if formatted, err := tool.FormatLargeResult(ctx, content, s.Config.MaxToolOutputChars, tc.ID, s.Config.ProjectRuntimeDir); err == nil {
					content = formatted
				}
			}
			toolResults = append(toolResults, map[string]any{"type": "tool_result", "tool_use_id": tc.ID, "content": content})
			observations = append(observations, map[string]any{"call": tc, "result": result, "tool": tool, "content": content})
		}
		sessionState.RecentToolObservations = observations
		sessionState.TotalToolUseCount += len(observations)
		continuationRetries = 0
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
	if err == context.Canceled {
		return SubAgentRequestResult{
			FinalText:         "Sub-agent aborted by user.",
			TotalInputTokens:  sessionState.TotalInputTokens,
			TotalOutputTokens: sessionState.TotalOutputTokens,
		}
	}
	return SubAgentRequestResult{
		FinalText:         s.partialTimeoutText(sessionState, currentText),
		TotalInputTokens:  sessionState.TotalInputTokens,
		TotalOutputTokens: sessionState.TotalOutputTokens,
		TimedOut:          true,
	}
}

func (s *SubAgent) finalizeTimedOutRun(ctx context.Context, prompt string, sessionState *SubAgentSessionState, fallback string) SubAgentRequestResult {
	if sessionState == nil {
		return SubAgentRequestResult{FinalText: fallback, TimedOut: true}
	}
	finalPrompt := fmt.Sprintf("You have reached the %d second sub-agent time limit. Stop using tools. Based only on the original task, conversation, tool results already available above, and the runtime partial-progress summary below, return the most useful final answer now. Be concise but include concrete findings, uncertainties, and next steps.\n\nOriginal task:\n%s\n\nRuntime partial-progress summary:\n%s", s.timeoutSeconds(), prompt, fallback)
	messages := append([]map[string]any{}, sessionState.Messages...)
	messages = append(messages, map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": finalPrompt}}})
	model := s.Model
	if model == "" {
		model = s.Config.APIModel
	}
	client, err := CreateConfiguredLLMClient(s.Config, model, s.Config.APIMaxTokens, s.ThinkingBudget, api.DefaultRetryConfigPtr())
	if err != nil {
		return SubAgentRequestResult{FinalText: fallback, TotalInputTokens: sessionState.TotalInputTokens, TotalOutputTokens: sessionState.TotalOutputTokens, TimedOut: true}
	}
	fullText := ""
	streamCtx := api.ContextWithStreamIdleTimeout(ctx, time.Duration(s.Config.APIStreamIdleTimeoutSeconds*float64(time.Second)))
	for result := range client.StreamChat(streamCtx, sessionState.SystemPrompt, prepareSubagentAPIMessages(messages), nil, nil) {
		if result.Err != nil {
			return SubAgentRequestResult{FinalText: fallback, TotalInputTokens: sessionState.TotalInputTokens, TotalOutputTokens: sessionState.TotalOutputTokens, TimedOut: true}
		}
		event := result.Event
		switch stringFromAny(event["type"]) {
		case "text_delta":
			fullText += stringFromAny(event["text"])
		case "usage":
			sessionState.TotalInputTokens += intFromAny(event["input_tokens"])
			sessionState.TotalOutputTokens += intFromAny(event["output_tokens"])
		}
	}
	fullText = strings.TrimSpace(fullText)
	if fullText == "" {
		fullText = fallback
	}
	sessionState.Messages = append(messages, map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": fullText}}})
	return SubAgentRequestResult{
		FinalText:         fullText,
		TotalInputTokens:  sessionState.TotalInputTokens,
		TotalOutputTokens: sessionState.TotalOutputTokens,
		TimedOut:          true,
	}
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
		packet, _ := s.recallLongTermAgentEvidence(ctx, prompt, nil)
		if msg := longmemory.BuildEvidenceContextMessage(packet); msg != nil {
			messages = append(messages, msg)
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

func (s *SubAgent) recallLongTermAgentEvidence(ctx context.Context, query string, surfaced map[string]struct{}) (longmemory.EvidencePacket, []string) {
	if !s.Config.LongTermMemoryEnabled || strings.TrimSpace(query) == "" {
		return longmemory.EvidencePacket{}, nil
	}
	store, err := longmemory.Open(ctx, s.Config.LongTermMemoryStore)
	if err != nil {
		return longmemory.EvidencePacket{}, nil
	}
	defer store.Close()
	agentType := strings.TrimSpace(s.AgentType)
	if agentType == "" {
		agentType = strings.TrimSpace(s.Definition.Name)
	}
	teamID, _ := s.ExtraContext["team_session_id"].(string)
	teamAgentID, _ := s.ExtraContext["team_agent_id"].(string)
	teamName, _ := s.ExtraContext["team_name"].(string)
	scopes := longmemory.RuntimeScopes(s.Config.CWD, agentType, teamName, teamAgentID)
	limit := s.Config.MemoryRecallMaxItems
	if limit <= 0 {
		limit = 8
	}
	memoryQuery := longmemory.MemoryQuery{Text: strings.TrimSpace(query), Timestamp: time.Now().UTC(), Scopes: scopes,
		SessionID: s.sessionIDForMemoryUse(), TeamSessionID: teamID, AgentID: agentType}
	catalog, catalogErr := store.InspectCatalog(ctx, scopes)
	expansion, expansionModel, expansionError := expandMemoryQuery(ctx, s.Config, memoryQuery, catalog,
		func(ctx context.Context, model string) (api.LLMClient, error) {
			return CreateConfiguredLLMClient(s.Config, model, 1024, nil, api.DefaultRetryConfigPtr())
		})
	if catalogErr != nil {
		if expansionError != "" {
			expansionError += "; "
		}
		expansionError += "inspect memory catalog: " + catalogErr.Error()
	}
	var embedder longmemory.Embedder
	if s.Config.MemoryEmbeddingEnabled {
		if local, embedErr := longmemory.SharedLocalEmbedder(s.Config.MemoryEmbeddingModel, s.Config.MemoryEmbeddingModelDir); embedErr == nil {
			embedder = local
		}
	}
	result, err := store.SearchAllChannels(ctx, memoryQuery, expansion, embedder, longmemory.HybridSearchOptions{
		FTSCandidates:       s.Config.MemoryFTSCandidates,
		VectorCandidates:    s.Config.MemoryVectorCandidates,
		GraphCandidates:     s.Config.MemoryGraphCandidates,
		GraphMaxHops:        s.Config.MemoryGraphMaxHops,
		RRFK:                s.Config.MemoryRRFK,
		MMRLambda:           s.Config.MemoryMMRLambda,
		MaxItems:            limit,
		CoreContextTokens:   s.Config.MemoryCoreContextTokens,
		TargetContextTokens: s.Config.MemoryContextTargetTokens,
		MaxContextTokens:    s.Config.MemoryContextMaxTokens,
		LocalTimeout:        time.Duration(s.Config.MemoryRetrievalLocalTimeoutSeconds * float64(time.Second)),
		SessionID:           s.sessionIDForMemoryUse(),
		TeamSessionID:       teamID,
		AgentID:             agentType,
		ExcludeIDs:          surfaced,
		ExpansionModel:      expansionModel,
		ExpansionError:      expansionError,
		NeighborChunks:      s.Config.MemoryEvidenceNeighborChunks,
	})
	if err != nil {
		return longmemory.EvidencePacket{}, nil
	}
	ids := make([]string, 0, len(result.Packet.Evidence))
	for _, evidence := range result.Packet.Evidence {
		ids = append(ids, evidence.MemoryID)
	}
	_ = store.RecordUsed(ctx, longmemory.UsedRecord{
		SessionID:     s.sessionIDForMemoryUse(),
		TeamSessionID: teamID,
		AgentID:       agentType,
		Query:         query,
		MemoryIDs:     ids,
	})
	return result.Packet, ids
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

func (s *SubAgent) buildExecutionContext() coretools.ExecutionContext {
	execCtx := coretools.ExecutionContext{
		"cwd":                     s.Config.CWD,
		"config":                  s.Config,
		"runtime_dir":             s.Config.ProjectRuntimeDir,
		"web_search_scope":        s.Config.WebSearchCacheScope,
		"allowed_read_roots":      compactToolRoots(s.Config.CWD, s.Config.ProjectRuntimeDir),
		"allowed_write_roots":     compactToolRoots(s.Config.CWD, s.Config.ProjectRuntimeDir),
		"parent_state":            s.ParentState,
		"_registry":               s.Registry,
		"_pending_skill_messages": []map[string]any{},
	}
	for key, value := range s.ExtraContext {
		execCtx[key] = value
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
	return longmemory.ResolveProjectRoot(s.Config.CWD)
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
