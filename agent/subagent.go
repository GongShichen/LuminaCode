package agent

import (
	"context"
	"fmt"
	"strings"

	"LuminaCode/agentContext"
	"LuminaCode/api"
	"LuminaCode/config"
	"LuminaCode/memory"
	"LuminaCode/skills"
	coretools "LuminaCode/tools"
)

const (
	MaxSubagentTurns       = 50
	MaxContinuationRetries = 2
)

var truncatedStopReasons = stringSet("max_tokens", "length", "token_limit")
var subagentReadLikeToolNames = stringSet("read_file", "grep_search", "glob_match", "search", "tool_search")

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
	return &SubAgent{Config: cfg, Registry: registry, Definition: definition, ParentState: parentState, Model: model, AgentType: agentType, ExtraContext: extraContext, ThinkingBudget: thinkingBudget, MaxTurns: maxTurns}
}

func (s *SubAgent) Abort() {
	s.Aborted = true
}

func (s *SubAgent) Run(ctx context.Context, prompt string) (string, error) {
	sessionState := s.createSessionState(ctx, prompt)
	result := s.ExecuteOneRequest(ctx, prompt, sessionState)
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
		SurfacedMemoryIDs: memory.RecalledMemoryIDs(messages, "agent_memory_recall"),
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
		client, err := api.CreateLLMClient(s.Config.APIKey, s.Config.APIBaseURL, model, s.Config.APIMaxTokens, s.ThinkingBudget, api.DefaultRetryConfigPtr(), s.Config.APIType)
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

		for result := range client.StreamChat(ctx, systemPrompt, prepareSubagentAPIMessages(messages), toolSchemas, nil) {
			if result.Err != nil {
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
				msg := stringFromAny(event["message"])
				return SubAgentRequestResult{
					FinalText:         "Sub-agent error: " + msg,
					TotalInputTokens:  sessionState.TotalInputTokens,
					TotalOutputTokens: sessionState.TotalOutputTokens,
				}
			}
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
				if formatted, err := tool.FormatLargeResult(ctx, content, s.Config.MaxToolOutputChars, tc.ID, s.Config.SessionDir); err == nil {
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
			sessionState.SurfacedMemoryIDs, messages = s.appendFreshAgentMemories(ctx, messages, prompt, observations, sessionState.SurfacedMemoryIDs)
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
	if s.Config.AutoMemoryEnabled {
		agentMemory = memory.BuildAgentMemoryPrompt(s.AgentType, s.resolveProjectRoot())
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
	if s.Config.AutoMemoryEnabled {
		projectRoot := s.resolveProjectRoot()
		messages = append(messages, memory.BuildAgentMemoryContextMessages(s.AgentType, projectRoot)...)
		recalled := memory.RecallAgentMemoriesForQuery(ctx, s.AgentType, projectRoot, prompt, s.recallClientFactory(), nil, nil)
		if msg := memory.BuildRecalledAgentMemoriesMessage(recalled, "agent_memory_recall"); msg != nil {
			messages = append(messages, msg)
		}
	}
	messages = append(messages, map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": prompt}}})
	return messages
}

func (s *SubAgent) buildExecutionContext() coretools.ExecutionContext {
	execCtx := coretools.ExecutionContext{
		"cwd":                     s.Config.CWD,
		"config":                  s.Config,
		"allowed_read_roots":      []string{s.Config.CWD},
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

func (s *SubAgent) appendFreshAgentMemories(ctx context.Context, messages []map[string]any, taskPrompt string, toolObservations []map[string]any, surfaced map[string]struct{}) (map[string]struct{}, []map[string]any) {
	if !s.Config.AutoMemoryEnabled || !s.shouldTriggerAgentMemoryRecall(toolObservations) {
		return surfaced, messages
	}
	recallQuery := s.buildAgentMemoryRecallQuery(taskPrompt, toolObservations)
	if recallQuery == "" {
		return surfaced, messages
	}
	updated := map[string]struct{}{}
	for key := range surfaced {
		updated[key] = struct{}{}
	}
	recalled := memory.RecallAgentMemoriesForQuery(ctx, s.AgentType, s.resolveProjectRoot(), recallQuery, s.recallClientFactory(), GetRecentToolNames(messages), updated)
	msg := memory.BuildRecalledAgentMemoriesMessage(recalled, "agent_memory_recall")
	if msg == nil {
		return surfaced, messages
	}
	messages = append(messages, msg)
	for _, item := range recalled {
		id := item.RecallID
		if id == "" {
			id = item.Filename
		}
		if strings.HasSuffix(id, ".md") {
			updated[id] = struct{}{}
		}
	}
	return updated, messages
}

func (s *SubAgent) shouldTriggerAgentMemoryRecall(toolObservations []map[string]any) bool {
	if len(toolObservations) == 0 {
		return false
	}
	for _, observation := range toolObservations {
		if result, ok := observation["result"].(coretools.ToolResult); ok && result.IsError {
			return true
		}
		if IsReadLikeObservationWithNames(observation, subagentReadLikeToolNames) {
			return true
		}
	}
	return false
}

func (s *SubAgent) buildAgentMemoryRecallQuery(taskPrompt string, toolObservations []map[string]any) string {
	var errors []string
	var observations []string
	for _, observation := range toolObservations {
		call, ok := observation["call"].(coretools.ToolCall)
		if !ok {
			continue
		}
		content := ClipRecallText(stringFromAny(observation["content"]))
		details := FormatToolInputForRecall(call.Input)
		label := call.Name
		if details != "" {
			label += " (" + details + ")"
		}
		line := "- " + label + ": " + content
		if result, ok := observation["result"].(coretools.ToolResult); ok && result.IsError {
			errors = append(errors, line)
		} else if IsReadLikeObservationWithNames(observation, subagentReadLikeToolNames) {
			observations = append(observations, line)
		}
	}
	parts := []string{"Task: " + taskPrompt}
	if len(errors) > 0 {
		parts = append(parts, "", "Recent tool errors:")
		parts = append(parts, limitStringList(errors, 3)...)
	} else if len(observations) > 0 {
		parts = append(parts, "", "Recent observations:")
		parts = append(parts, limitStringList(observations, 3)...)
	}
	return strings.Join(parts, "\n")
}

type apiMemoryClient struct {
	client api.LLMClient
}

func (c apiMemoryClient) Complete(ctx context.Context, systemPrompt string, messages []map[string]any, maxTokens int) (string, error) {
	return c.client.Complete(ctx, systemPrompt, messages, api.CompleteOptions{MaxTokens: maxTokens})
}

func (s *SubAgent) recallClientFactory() memory.ClientFactory {
	return func(ctx context.Context) (memory.CompletionClient, error) {
		client, err := api.CreateLLMClient(s.Config.APIKey, s.Config.APIBaseURL, s.Model, 256, nil, api.DefaultRetryConfigPtr(), s.Config.APIType)
		if err != nil {
			return nil, err
		}
		return apiMemoryClient{client: client}, nil
	}
}

func (s *SubAgent) resolveProjectRoot() string {
	return memory.ResolveAgentMemoryProjectRoot(s.Config.CWD)
}

func limitStringList(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	return values[:limit]
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
