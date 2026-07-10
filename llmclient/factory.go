package llmclient

import (
	"LuminaCode/api"
	"LuminaCode/config"
)

func Create(
	cfg config.Config,
	model string,
	maxTokens int,
	thinkingBudgetTokens *int,
	retryConfig *api.RetryConfig,
) (api.LLMClient, error) {
	if model == "" {
		model = cfg.APIModel
	}
	fallback := &api.FallbackClientConfig{
		Enabled:     cfg.FallbackAPIEnabled,
		APIKey:      cfg.FallbackAPIKey,
		BaseURL:     cfg.FallbackAPIBaseURL,
		Model:       cfg.FallbackAPIModel,
		APIType:     cfg.FallbackAPIType,
		MaxTokens:   maxTokens,
		RetryConfig: retryConfig,
	}
	return api.CreateLLMClientWithFallback(
		cfg.APIKey,
		cfg.APIBaseURL,
		model,
		maxTokens,
		thinkingBudgetTokens,
		retryConfig,
		cfg.APIType,
		fallback,
	)
}
