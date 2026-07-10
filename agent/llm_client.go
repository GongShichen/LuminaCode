package agent

import (
	"LuminaCode/api"
	"LuminaCode/config"
	"LuminaCode/llmclient"
)

func CreateConfiguredLLMClient(
	cfg config.Config,
	model string,
	maxTokens int,
	thinkingBudgetTokens *int,
	retryConfig *api.RetryConfig,
) (api.LLMClient, error) {
	return llmclient.Create(cfg, model, maxTokens, thinkingBudgetTokens, retryConfig)
}
