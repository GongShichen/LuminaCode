package ui

import (
	"fmt"
	"math"
	"strings"

	"LuminaCode/agent"
	"LuminaCode/agentContext"
	"LuminaCode/config"
)

type ContextWindowSnapshot struct {
	ModelName   string
	UsedTokens  int
	LimitTokens int
}

func BuildContextWindowSnapshot(cfg config.Config, state *agent.AgentState) ContextWindowSnapshot {
	used := 0
	if state != nil {
		used = agentContext.TokenCountWithEstimation(state.Messages)
		if state.SystemPrompt != "" {
			used += agentContext.RoughEstimate(state.SystemPrompt)
		}
	}
	return ContextWindowSnapshot{
		ModelName:   cfg.APIModel,
		UsedTokens:  used,
		LimitTokens: cfg.CompressionContextLimit(),
	}
}

func FormatContextWindowStatus(model string, usedTokens, limitTokens int, width int) string {
	if width <= 0 {
		width = 20
	}
	if model == "" && limitTokens <= 0 {
		return ""
	}
	label := "Model: " + firstNonEmpty(model, "unknown")
	if limitTokens <= 0 {
		return label + " | Context: n/a"
	}
	if usedTokens < 0 {
		usedTokens = 0
	}
	ratio := float64(usedTokens) / float64(limitTokens)
	percent := int(math.Round(ratio * 100))
	if percent < 0 {
		percent = 0
	}
	bar := FormatContextProgressBar(usedTokens, limitTokens, width)
	return fmt.Sprintf("%s | Context %s %d%% %s/%s", label, bar, percent, formatTokenCount(usedTokens), formatTokenCount(limitTokens))
}

func FormatContextProgressBar(usedTokens, limitTokens int, width int) string {
	if width <= 0 {
		width = 20
	}
	if limitTokens <= 0 {
		return "[" + strings.Repeat("-", width) + "]"
	}
	if usedTokens < 0 {
		usedTokens = 0
	}
	filled := int(math.Round(float64(usedTokens) / float64(limitTokens) * float64(width)))
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}

func formatTokenCount(tokens int) string {
	if tokens < 0 {
		tokens = 0
	}
	if tokens >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(tokens)/1_000_000)
	}
	if tokens >= 1000 {
		return fmt.Sprintf("%dK", tokens/1000)
	}
	return fmt.Sprintf("%d", tokens)
}
