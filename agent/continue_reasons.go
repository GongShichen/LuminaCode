package agent

type ContinueReason string

const (
	ContinueReasonNextTurn                ContinueReason = "next_turn"
	ContinueReasonMaxOutputTokensEscalate ContinueReason = "max_output_tokens_escalate"
	ContinueReasonMaxOutputTokensRecovery ContinueReason = "max_output_tokens_recovery"
	ContinueReasonCollapseDrainRetry      ContinueReason = "collapse_drain_retry"
	ContinueReasonReactiveCompactRetry    ContinueReason = "reactive_compact_retry"
)
