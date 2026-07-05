package agent

import (
	"strings"

	"LuminaCode/agentContext"
	"LuminaCode/config"
)

var ptlErrorKeywords = []string{
	"too_long", "token limit", "context length", "prompt is too long",
	"maximum context", "exceeds the maximum",
	"input length", "reduce the length",
}

func IsPTLErrorMessage(msg string) bool {
	low := strings.ToLower(msg)
	for _, kw := range ptlErrorKeywords {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

type PTLRecoveryManager struct {
	Config  config.Config
	Regions []any
}

func (m *PTLRecoveryManager) Recover(state *AgentState, errorMsg string) (string, *StreamEvent) {
	if m.TryCollapseDrain(state) {
		return "retry", nil
	}
	if m.ReactiveCompact(state) {
		return "retry", nil
	}
	state.RecentApiErrors = append(state.RecentApiErrors, "Prompt too long: "+TruncateResult(errorMsg, 200))
	if len(state.RecentApiErrors) > 5 {
		state.RecentApiErrors = state.RecentApiErrors[len(state.RecentApiErrors)-5:]
	}
	event := NewStreamEvent("error", "Prompt too long - unable to reduce context enough. Please start a new session or manually trim the conversation.", nil)
	return "fail", &event
}

func (m *PTLRecoveryManager) TryCollapseDrain(state *AgentState) bool {
	before := agentContext.TokenCountWithEstimation(state.Messages)
	if before == 0 {
		return false
	}
	collapsed := agentContext.CollapseMessages(state.Messages, 5)
	after := agentContext.TokenCountWithEstimation(collapsed)
	if after >= before {
		collapsed = agentContext.CollapseMessages(state.Messages, 2)
		after = agentContext.TokenCountWithEstimation(collapsed)
	}
	collapsed = RepairOrphanTools(collapsed)
	after = agentContext.TokenCountWithEstimation(collapsed)
	if after < before {
		state.Messages = collapsed
		m.Regions = nil
		state.CacheBreakPoints.Clear()
		state.LastContinueReason = string(ContinueReasonCollapseDrainRetry)
		return true
	}
	return false
}

func (m *PTLRecoveryManager) ReactiveCompact(state *AgentState) bool {
	before := agentContext.TokenCountWithEstimation(state.Messages)
	if before == 0 {
		return false
	}
	pipeline := agentContext.DefaultContextPipeline()
	pipeline.Config = m.Config
	compressed, stats := pipeline.Compress(
		state.Messages,
		before,
		state.SystemPrompt,
		m.Config.CompressionContextLimit(),
		m.Config.CompressionThreshold(),
		state,
		nil,
		true,
	)
	if stats.LevelReached >= 3 {
		state.Messages = RepairOrphanTools(compressed)
		m.Regions = nil
		state.CacheBreakPoints.Clear()
		state.LastContinueReason = string(ContinueReasonReactiveCompactRetry)
		return true
	}
	return false
}
