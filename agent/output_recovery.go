package agent

import coretools "LuminaCode/tools"

const (
	MaxOutputRecoveryRetries = 3
	EscalatedMaxTokens       = 64_000
)

type OutputRecoveryState struct {
	Retries          int
	Escalated        bool
	CurrentMaxTokens int
}

func NewOutputRecoveryState(apiMaxTokens int) OutputRecoveryState {
	return OutputRecoveryState{CurrentMaxTokens: apiMaxTokens}
}

func (s *OutputRecoveryState) ResetRetries() { s.Retries = 0 }

func (s *OutputRecoveryState) ResetAfterHistoryReplace(apiMaxTokens int) {
	s.Retries = 0
	s.Escalated = false
	s.CurrentMaxTokens = apiMaxTokens
}

func IsOutputTruncatedStopReason(reason string) bool {
	return reason == "max_tokens" || reason == "length" || reason == "token_limit"
}

type OutputRecoveryResult struct {
	Action string
	Event  *StreamEvent
	Reason ContinueReason
}

func (r OutputRecoveryResult) ShouldContinue() bool {
	return r.Action == "retry" || r.Action == "continue"
}

func (r OutputRecoveryResult) ShouldReturn() bool { return r.Action == "fail" }

func HandleOutputTruncation(state *AgentState, recovery *OutputRecoveryState, toolCalls []coretools.ToolCall, thinkingContent []map[string]any, fullText, currentMessageID string, inputTokens, outputTokens int) OutputRecoveryResult {
	if len(toolCalls) > 0 {
		return OutputRecoveryResult{Action: "proceed"}
	}
	if !recovery.Escalated {
		recovery.Escalated = true
		recovery.CurrentMaxTokens = EscalatedMaxTokens
		state.LastContinueReason = string(ContinueReasonMaxOutputTokensEscalate)
		return OutputRecoveryResult{Action: "retry", Reason: ContinueReasonMaxOutputTokensEscalate}
	}
	if recovery.Retries < MaxOutputRecoveryRetries {
		recovery.Retries++
		state.LastContinueReason = string(ContinueReasonMaxOutputTokensRecovery)
		CommitTextOnlyTruncation(state, thinkingContent, fullText, currentMessageID, inputTokens, outputTokens)
		return OutputRecoveryResult{Action: "continue", Reason: ContinueReasonMaxOutputTokensRecovery}
	}
	event := NewStreamEvent("error", "Output repeatedly truncated after 3 continuation attempts. The response may be too long for the model's output limit. Try breaking the task into smaller steps.", nil)
	return OutputRecoveryResult{Action: "fail", Event: &event}
}

func CommitTextOnlyTruncation(state *AgentState, thinkingContent []map[string]any, fullText, currentMessageID string, inputTokens, outputTokens int) {
	CommitAssistantTurn(state, thinkingContent, fullText, nil, currentMessageID, inputTokens, outputTokens)
	AppendContinuationPrompt(state)
}
