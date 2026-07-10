package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
)

type FallbackClientConfig struct {
	Enabled     bool
	APIKey      string
	BaseURL     string
	Model       string
	APIType     string
	MaxTokens   int
	RetryConfig *RetryConfig
}

type FallbackLLMClient struct {
	primary       LLMClient
	fallback      LLMClient
	primaryModel  string
	fallbackModel string
}

var _ LLMClient = (*FallbackLLMClient)(nil)

type ModelFallbackError struct {
	Primary  error
	Fallback error
}

func (e ModelFallbackError) Error() string {
	return fmt.Sprintf("primary model failed: %v; fallback model failed: %v", e.Primary, e.Fallback)
}

func (e ModelFallbackError) Unwrap() []error {
	return []error{e.Primary, e.Fallback}
}

func NewFallbackLLMClient(primary LLMClient, fallback LLMClient, primaryModel, fallbackModel string) *FallbackLLMClient {
	return &FallbackLLMClient{
		primary:       primary,
		fallback:      fallback,
		primaryModel:  strings.TrimSpace(primaryModel),
		fallbackModel: strings.TrimSpace(fallbackModel),
	}
}

func CreateLLMClientWithFallback(
	apiKey string,
	baseURL string,
	model string,
	maxTokens int,
	thinkingBudgetTokens *int,
	retryConfig *RetryConfig,
	apiType string,
	fallback *FallbackClientConfig,
) (LLMClient, error) {
	primary, err := CreateLLMClient(apiKey, baseURL, model, maxTokens, thinkingBudgetTokens, retryConfig, apiType)
	if err != nil {
		return nil, err
	}
	if fallback == nil || !fallback.Enabled {
		return primary, nil
	}
	if strings.TrimSpace(fallback.APIKey) == "" || strings.TrimSpace(fallback.BaseURL) == "" || strings.TrimSpace(fallback.Model) == "" {
		return nil, fmt.Errorf("fallback API is enabled but fallback_api_key, fallback_api_base_url, and fallback_api_model must all be configured")
	}
	fallbackMaxTokens := fallback.MaxTokens
	if fallbackMaxTokens <= 0 {
		fallbackMaxTokens = maxTokens
	}
	fallbackRetry := fallback.RetryConfig
	if fallbackRetry == nil {
		fallbackRetry = retryConfig
	}
	secondary, err := CreateLLMClient(
		fallback.APIKey,
		fallback.BaseURL,
		fallback.Model,
		fallbackMaxTokens,
		thinkingBudgetTokens,
		fallbackRetry,
		fallback.APIType,
	)
	if err != nil {
		return nil, fmt.Errorf("configure fallback model: %w", err)
	}
	return NewFallbackLLMClient(primary, secondary, model, fallback.Model), nil
}

func (c *FallbackLLMClient) Complete(
	ctx context.Context,
	systemPrompt string,
	messages []map[string]any,
	opts CompleteOptions,
) (string, error) {
	text, err := c.primary.Complete(ctx, systemPrompt, messages, opts)
	if err == nil || !fallbackEligibleError(ctx, err) {
		return text, err
	}
	fallbackText, fallbackErr := c.fallback.Complete(ctx, systemPrompt, messages, opts)
	if fallbackErr != nil {
		return "", ModelFallbackError{Primary: err, Fallback: fallbackErr}
	}
	return fallbackText, nil
}

func (c *FallbackLLMClient) StreamChat(
	ctx context.Context,
	systemPrompt string,
	messages []map[string]any,
	tools []map[string]any,
	options *LLMRequestOptions,
) <-chan EventResult {
	out := make(chan EventResult)
	go func() {
		defer close(out)
		producedOutput := false
		primaryStream := c.primary.StreamChat(ctx, systemPrompt, messages, tools, options)
		for item := range primaryStream {
			if item.Err != nil {
				if !producedOutput && fallbackEligibleError(ctx, item.Err) {
					c.streamFallback(ctx, out, systemPrompt, messages, tools, options, item.Err.Error())
					return
				}
				if !sendFallbackResult(ctx, out, item) {
					return
				}
				continue
			}
			if item.Event["type"] == "error" && !producedOutput {
				if reason, ok := fallbackEligibleEvent(item.Event); ok {
					c.streamFallback(ctx, out, systemPrompt, messages, tools, options, reason)
					return
				}
			}
			if streamEventProducedOutput(item.Event) {
				producedOutput = true
			}
			if !sendFallbackResult(ctx, out, item) {
				return
			}
		}
	}()
	return out
}

func (c *FallbackLLMClient) streamFallback(
	ctx context.Context,
	out chan<- EventResult,
	systemPrompt string,
	messages []map[string]any,
	tools []map[string]any,
	options *LLMRequestOptions,
	reason string,
) {
	if !sendFallbackResult(ctx, out, EventResult{Event: map[string]any{
		"type":           "model_fallback",
		"primary_model":  c.primaryModel,
		"fallback_model": c.fallbackModel,
		"reason":         reason,
	}}) {
		return
	}
	for item := range c.fallback.StreamChat(ctx, systemPrompt, messages, tools, options) {
		if item.Err != nil {
			item.Err = ModelFallbackError{Primary: errors.New(reason), Fallback: item.Err}
		} else if item.Event["type"] == "error" {
			fallbackMessage := stringValue(item.Event["message"])
			cloned := make(map[string]any, len(item.Event)+2)
			for key, value := range item.Event {
				cloned[key] = value
			}
			cloned["primary_error"] = reason
			cloned["fallback_error"] = fallbackMessage
			cloned["message"] = fmt.Sprintf("primary model failed: %s; fallback model failed: %s", reason, fallbackMessage)
			item.Event = cloned
		}
		if !sendFallbackResult(ctx, out, item) {
			return
		}
	}
}

func sendFallbackResult(ctx context.Context, out chan<- EventResult, result EventResult) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- result:
		return true
	}
}

func streamEventProducedOutput(event map[string]any) bool {
	switch event["type"] {
	case "text_delta", "tool_use", "thinking", "thinking_delta", "redacted_thinking":
		return true
	default:
		return false
	}
}

func fallbackEligibleEvent(event map[string]any) (string, bool) {
	message := stringValue(event["message"])
	if containsNonFallbackError(message) || containsNonFallbackError(stringValue(event["raw_error"])) {
		return message, false
	}
	if status := intValue(event["status_code"]); status != 0 {
		return message, status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
	}
	if retryable, ok := event["retryable"].(bool); ok && retryable {
		return message, true
	}
	errorType := strings.ToLower(strings.TrimSpace(stringValue(event["error_type"])))
	if errorType == "transport_error" || errorType == "api_stream_error" {
		return message, true
	}
	return message, strings.Contains(strings.ToLower(message), "request failed after")
}

func fallbackEligibleError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return false
	}
	if containsNonFallbackError(err.Error()) {
		return false
	}
	var statusErr APIStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode == http.StatusRequestTimeout || statusErr.StatusCode == http.StatusTooManyRequests || statusErr.StatusCode >= 500
	}
	var retryableStatus RetryableStatusError
	if errors.As(err, &retryableStatus) {
		return true
	}
	var retryable RetryableError
	if errors.As(err, &retryable) {
		return true
	}
	var idleTimeout StreamIdleTimeoutError
	if errors.As(err, &idleTimeout) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary())
}

func containsNonFallbackError(value string) bool {
	lower := strings.ToLower(value)
	for _, fragment := range []string{
		"invalid_request_error",
		"invalid api key",
		"invalid_api_key",
		"authentication",
		"authorization",
		"permission",
		"model_not_found",
		"not_found",
	} {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		parsed, _ := strconv.Atoi(typed)
		return parsed
	default:
		return 0
	}
}
