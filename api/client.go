package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const defaultLLMHTTPTimeout = 5 * time.Minute

type CacheEdit struct {
	ToolUseID string `json:"tool_use_id"`
	Action    string `json:"action"`
}

type Response struct {
	Text         string           `json:"text"`
	ToolCalls    []map[string]any `json:"tool_calls"`
	StopReason   string           `json:"stop_reason"`
	InputTokens  int              `json:"input_tokens"`
	OutputTokens int              `json:"output_tokens"`
}

type LLMRequestOptions struct {
	AnthropicCacheEdits []CacheEdit `json:"anthropic_cache_edits,omitempty"`
}

type CompleteOptions struct {
	MaxTokens int
	Tools     []map[string]any
}

type LLMClient interface {
	StreamChat(
		ctx context.Context,
		systemPrompt string,
		messages []map[string]any,
		tools []map[string]any,
		options *LLMRequestOptions,
	) <-chan EventResult

	Complete(
		ctx context.Context,
		systemPrompt string,
		messages []map[string]any,
		opts CompleteOptions,
	) (string, error)
}

type Pricing struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

var pricing = map[string]Pricing{
	"claude-opus-4":     {Input: 15.00, Output: 75.00},
	"claude-sonnet-4":   {Input: 3.00, Output: 15.00},
	"claude-haiku-4":    {Input: 0.80, Output: 4.00},
	"deepseek-v4-pro":   {Input: 0.55, Output: 2.19},
	"deepseek-v4-flash": {Input: 0.14, Output: 0.28},
	"deepseek-reasoner": {Input: 0.55, Output: 2.19},
	"deepseek-chat":     {Input: 0.27, Output: 1.10},
	"gpt-5":             {Input: 1.25, Output: 10.00},
	"gpt-5-mini":        {Input: 0.35, Output: 2.00},
	"gpt-5-nano":        {Input: 0.10, Output: 0.50},
	"gpt-4.1":           {Input: 2.00, Output: 8.00},
	"gpt-4.1-mini":      {Input: 0.40, Output: 2.00},
	"gpt-4.1-nano":      {Input: 0.10, Output: 0.80},
	"gpt-4o":            {Input: 2.50, Output: 10.00},
	"gpt-4o-mini":       {Input: 0.15, Output: 0.60},
	"o4-mini":           {Input: 1.10, Output: 4.40},
	"o3":                {Input: 10.00, Output: 40.00},
	"o3-mini":           {Input: 1.10, Output: 4.40},
	"o1":                {Input: 15.00, Output: 60.00},
}

var defaultPricing = Pricing{Input: 3.00, Output: 15.00}

func GetPricing(model string) Pricing {
	modelLower := strings.ToLower(strings.TrimSpace(model))
	if p, ok := pricing[modelLower]; ok {
		return p
	}

	prefixes := make([]string, 0, len(pricing))
	for prefix := range pricing {
		prefixes = append(prefixes, prefix)
	}
	sort.Slice(prefixes, func(i, j int) bool {
		return len(prefixes[i]) > len(prefixes[j])
	})

	for _, prefix := range prefixes {
		if strings.HasPrefix(modelLower, prefix) {
			return pricing[prefix]
		}
	}

	return defaultPricing
}

func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

func isOpenAIModel(model string) bool {
	modelLower := strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range []string{"gpt-", "o1", "o3", "o4"} {
		if strings.HasPrefix(modelLower, prefix) {
			return true
		}
	}
	return false
}

func isOpenAICompatibleModel(model string) bool {
	return isDeepSeekModel(model) || isOpenAIModel(model)
}

func normalizeAPIType(apiType string) string {
	switch strings.ToLower(strings.TrimSpace(apiType)) {
	case "", "auto":
		return "auto"
	case "openai", "openai-compatible", "openai_compatible", "deepseek":
		return "openai_compatible"
	case "anthropic", "claude":
		return "anthropic"
	default:
		return "auto"
	}
}

func useOpenAICompatible(model string, apiType string) bool {
	switch normalizeAPIType(apiType) {
	case "openai_compatible":
		return true
	case "anthropic":
		return false
	default:
		return isOpenAICompatibleModel(model)
	}
}

func AnthropicToolsToOpenAI(tools []map[string]any) []map[string]any {
	openaiTools := make([]map[string]any, 0, len(tools))

	for _, tool := range tools {
		parameters, ok := tool["input_schema"]
		if !ok {
			parameters, ok = tool["parameters"]
			if !ok {
				parameters = map[string]any{}
			}
		}

		openaiTools = append(openaiTools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        getString(tool, "name"),
				"description": getString(tool, "description"),
				"parameters":  parameters,
			},
		})
	}

	return openaiTools
}

type LLMClientBase struct {
	APIKey               string
	BaseURL              string
	Model                string
	MaxTokens            int
	ThinkingBudgetTokens *int
	RetryConfig          RetryConfig
	HTTPClient           *http.Client
}

func NewLLMClientBase(
	apiKey string,
	baseURL string,
	model string,
	maxTokens int,
	thinkingBudgetTokens *int,
	retryConfig *RetryConfig,
) (LLMClientBase, error) {
	if apiKey == "" {
		return LLMClientBase{}, fmt.Errorf("API key must be configured explicitly")
	}
	if baseURL == "" {
		return LLMClientBase{}, fmt.Errorf("API base URL must be configured explicitly")
	}
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	if maxTokens == 0 {
		maxTokens = 16000
	}

	cfg := DefaultRetryConfig()
	if retryConfig != nil {
		cfg = *retryConfig
	}

	return LLMClientBase{
		APIKey:               apiKey,
		BaseURL:              strings.TrimRight(baseURL, "/"),
		Model:                model,
		MaxTokens:            maxTokens,
		ThinkingBudgetTokens: thinkingBudgetTokens,
		RetryConfig:          cfg,
		HTTPClient:           &http.Client{Timeout: defaultLLMHTTPTimeout},
	}, nil
}

func (c *LLMClientBase) RetryRequest(
	ctx context.Context,
	makeRequest func(context.Context, *http.Client) (*http.Response, error),
	timeout time.Duration,
) (map[string]any, error) {
	cfg := c.RetryConfig
	var lastErr error

	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		resp, err := makeRequest(reqCtx, c.HTTPClient)
		cancel()

		if err != nil {
			lastErr = err
			if attempt < cfg.MaxRetries {
				if !sleepBackoff(ctx, attempt, cfg) {
					return nil, ctx.Err()
				}
				continue
			}
			break
		}

		if resp == nil {
			lastErr = fmt.Errorf("nil HTTP response")
			if attempt < cfg.MaxRetries {
				if !sleepBackoff(ctx, attempt, cfg) {
					return nil, ctx.Err()
				}
				continue
			}
			break
		}

		bodyBytes, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if attempt < cfg.MaxRetries {
				if !sleepBackoff(ctx, attempt, cfg) {
					return nil, ctx.Err()
				}
				continue
			}
			break
		}

		if resp.StatusCode == http.StatusOK {
			var data map[string]any
			if err := json.Unmarshal(bodyBytes, &data); err != nil {
				return nil, fmt.Errorf("invalid JSON in 200 response: %.200s", string(bodyBytes))
			}
			return data, nil
		}

		statusErr := NewAPIStatusError("", resp.StatusCode, resp.Status, bodyBytes)
		if isRetryableHTTPError(resp.StatusCode, statusErr.Body, cfg) && attempt < cfg.MaxRetries {
			if !sleepBackoff(ctx, attempt, cfg) {
				return nil, ctx.Err()
			}
			continue
		}

		return nil, statusErr
	}

	return nil, fmt.Errorf("request failed after %d attempts: %w", cfg.MaxRetries+1, lastErr)
}

func isRetryableHTTPError(statusCode int, errorText string, cfg RetryConfig) bool {
	if _, ok := cfg.RetryableStatuses[statusCode]; ok {
		return true
	}

	if _, ok := cfg.ConditionalRetryStatuses[statusCode]; ok {
		for keyword := range cfg.NonRetryableKeywords {
			if strings.Contains(errorText, keyword) {
				return false
			}
		}
		return true
	}

	return false
}

type AnthropicClient struct {
	LLMClientBase
}

var _ LLMClient = (*AnthropicClient)(nil)

func (c *AnthropicClient) Complete(
	ctx context.Context,
	systemPrompt string,
	messages []map[string]any,
	opts CompleteOptions,
) (string, error) {
	url := c.BaseURL + "/v1/messages"
	headers := map[string]string{
		"x-api-key":         c.APIKey,
		"anthropic-version": "2023-06-01",
		"content-type":      "application/json",
	}

	systemBlock := []map[string]any{
		{
			"type":          "text",
			"text":          systemPrompt,
			"cache_control": map[string]any{"type": "ephemeral"},
		},
	}

	body := map[string]any{
		"model":    c.Model,
		"system":   systemBlock,
		"messages": messages,
		"stream":   false,
	}

	if len(opts.Tools) > 0 {
		body["tools"] = opts.Tools
	}
	if c.ThinkingBudgetTokens != nil && *c.ThinkingBudgetTokens > 0 {
		body["thinking"] = map[string]any{
			"type":          "enabled",
			"budget_tokens": *c.ThinkingBudgetTokens,
		}
	}

	data, err := c.RetryRequest(ctx, func(reqCtx context.Context, httpClient *http.Client) (*http.Response, error) {
		return doJSONRequest(reqCtx, httpClient, http.MethodPost, url, headers, body)
	}, 30*time.Second)
	if err != nil {
		return "", err
	}

	contentBlocks, _ := data["content"].([]any)
	var parts []string
	for _, rawBlock := range contentBlocks {
		block, ok := rawBlock.(map[string]any)
		if !ok {
			continue
		}
		if getString(block, "type") == "text" {
			parts = append(parts, getString(block, "text"))
		}
	}

	return strings.Join(parts, ""), nil
}

func (c *AnthropicClient) StreamChat(
	ctx context.Context,
	systemPrompt string,
	messages []map[string]any,
	tools []map[string]any,
	options *LLMRequestOptions,
) <-chan EventResult {
	url := c.BaseURL + "/v1/messages"
	headers := map[string]string{
		"x-api-key":         c.APIKey,
		"anthropic-version": "2023-06-01",
		"content-type":      "application/json",
	}

	apiTools := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		parameters, ok := tool["input_schema"]
		if !ok {
			parameters, ok = tool["parameters"]
			if !ok {
				parameters = map[string]any{}
			}
		}

		apiTools = append(apiTools, map[string]any{
			"name":         getString(tool, "name"),
			"description":  getString(tool, "description"),
			"input_schema": parameters,
		})
	}
	if len(apiTools) > 0 {
		apiTools[len(apiTools)-1]["cache_control"] = map[string]any{"type": "ephemeral"}
	}

	systemBlock := []map[string]any{
		{
			"type":          "text",
			"text":          systemPrompt,
			"cache_control": map[string]any{"type": "ephemeral"},
		},
	}

	bodyMap := map[string]any{
		"model":    c.Model,
		"system":   systemBlock,
		"messages": messages,
		"stream":   true,
	}
	if len(apiTools) > 0 {
		bodyMap["tools"] = apiTools
	}
	if c.ThinkingBudgetTokens != nil && *c.ThinkingBudgetTokens > 0 {
		bodyMap["thinking"] = map[string]any{
			"type":          "enabled",
			"budget_tokens": *c.ThinkingBudgetTokens,
		}
	}

	bodyBytes, err := json.Marshal(bodyMap)
	if err != nil {
		return singleErrorEvent(err)
	}

	return RetryWithBackoff(ctx, c.streamAnthropic, func() (RequestParts, error) {
		return RequestParts{
			URL:     url,
			Headers: headers,
			Body:    bodyBytes,
		}, nil
	}, c.RetryConfig)
}

func (c *AnthropicClient) streamAnthropic(
	ctx context.Context,
	url string,
	headers map[string]string,
	body []byte,
) <-chan EventResult {
	out := make(chan EventResult)

	go func() {
		defer close(out)

		resp, err := doRawJSONRequest(ctx, c.HTTPClient, http.MethodPost, url, headers, body)
		if err != nil {
			out <- EventResult{Err: err}
			return
		}
		defer func(Body io.ReadCloser) {
			_ = Body.Close()
		}(resp.Body)

		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			statusErr := NewAPIStatusError("Anthropic", resp.StatusCode, resp.Status, bodyBytes)

			if isRetryableAnthropicStreamError(resp.StatusCode, statusErr.Body) {
				out <- EventResult{Err: RetryableStatusError{APIStatusError: statusErr}}
				return
			}

			out <- EventResult{Event: map[string]any{
				"type":        "error",
				"message":     statusErr.Error(),
				"status_code": resp.StatusCode,
				"raw_error":   statusErr.Body,
			}}
			return
		}

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

		currentToolID := ""
		currentToolName := ""
		currentToolInput := ""
		savedInputTokens := 0

		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}

			dataStr := strings.TrimSpace(line[len("data:"):])
			if dataStr == "" {
				continue
			}

			var data map[string]any
			if err := json.Unmarshal([]byte(dataStr), &data); err != nil {
				continue
			}

			switch getString(data, "type") {
			case "error":
				errorObj, _ := data["error"].(map[string]any)
				message := getString(errorObj, "message")
				if message == "" {
					message = "Unknown API error"
				}
				out <- EventResult{Event: map[string]any{"type": "error", "message": message}}
				return

			case "message_start":
				message, _ := data["message"].(map[string]any)
				if id := getString(message, "id"); id != "" {
					out <- EventResult{Event: map[string]any{"type": "message_id", "id": id}}
				}
				usage, _ := message["usage"].(map[string]any)
				savedInputTokens = getInt(usage, "input_tokens")

			case "content_block_start":
				contentBlock, _ := data["content_block"].(map[string]any)
				blockType := getString(contentBlock, "type")

				switch blockType {
				case "tool_use":
					currentToolID = getString(contentBlock, "id")
					currentToolName = getString(contentBlock, "name")
					currentToolInput = ""
				case "thinking":
					out <- EventResult{Event: map[string]any{
						"type":      "thinking",
						"text":      getString(contentBlock, "thinking"),
						"signature": getString(contentBlock, "signature"),
					}}
				case "redacted_thinking":
					out <- EventResult{Event: map[string]any{
						"type": "redacted_thinking",
						"data": getString(contentBlock, "data"),
					}}
				}

			case "content_block_delta":
				delta, _ := data["delta"].(map[string]any)
				switch getString(delta, "type") {
				case "text_delta":
					out <- EventResult{Event: map[string]any{"type": "text_delta", "text": getString(delta, "text")}}
				case "input_json_delta":
					currentToolInput += getString(delta, "partial_json")
				case "thinking_delta":
					out <- EventResult{Event: map[string]any{"type": "thinking_delta", "text": getString(delta, "thinking")}}
				case "signature_delta":
					out <- EventResult{Event: map[string]any{"type": "signature_delta", "signature": getString(delta, "signature")}}
				}

			case "content_block_stop":
				if currentToolID == "" {
					continue
				}

				parsedInput := map[string]any{}
				if strings.TrimSpace(currentToolInput) != "" {
					if err := json.Unmarshal([]byte(currentToolInput), &parsedInput); err != nil {
						out <- EventResult{Event: map[string]any{
							"type":    "error",
							"message": fmt.Sprintf("Failed to parse tool_use input for %s: malformed JSON", currentToolName),
						}}
						currentToolID = ""
						currentToolName = ""
						currentToolInput = ""
						continue
					}
				}

				out <- EventResult{Event: map[string]any{
					"type":  "tool_use",
					"id":    currentToolID,
					"name":  currentToolName,
					"input": parsedInput,
				}}

				currentToolID = ""
				currentToolName = ""
				currentToolInput = ""

			case "message_delta":
				usage, _ := data["usage"].(map[string]any)
				delta, _ := data["delta"].(map[string]any)

				out <- EventResult{Event: map[string]any{
					"type":                        "usage",
					"input_tokens":                savedInputTokens,
					"output_tokens":               getInt(usage, "output_tokens"),
					"cache_read_input_tokens":     getInt(usage, "cache_read_input_tokens"),
					"cache_creation_input_tokens": getInt(usage, "cache_creation_input_tokens"),
				}}

				stopReason := getString(delta, "stop_reason")
				if stopReason == "" {
					stopReason = "end_turn"
				}
				out <- EventResult{Event: map[string]any{"type": "stop_reason", "stop_reason": stopReason}}
			}
		}

		if err := scanner.Err(); err != nil {
			out <- EventResult{Err: err}
		}
	}()

	return out
}

type OpenAICompatibleClient struct {
	LLMClientBase
}

var _ LLMClient = (*OpenAICompatibleClient)(nil)

func (c *OpenAICompatibleClient) buildHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + c.APIKey,
		"Content-Type":  "application/json",
	}
}

func (c *OpenAICompatibleClient) chatCompletionsURL() string {
	baseURL := strings.TrimRight(c.BaseURL, "/")
	if strings.HasSuffix(baseURL, "/chat/completions") {
		return baseURL
	}
	if strings.HasSuffix(baseURL, "/v1") {
		baseURL = strings.TrimSuffix(baseURL, "/v1")
	}
	return baseURL + "/chat/completions"
}

func (c *OpenAICompatibleClient) buildMessages(systemPrompt string, messages []map[string]any) []map[string]any {
	converted := convertAnthropicMessagesToOpenAI(messages)
	result := make([]map[string]any, 0, len(converted)+1)

	if systemPrompt != "" {
		result = append(result, map[string]any{
			"role":    "system",
			"content": systemPrompt,
		})
	}

	result = append(result, converted...)
	return result
}

func (c *OpenAICompatibleClient) Complete(
	ctx context.Context,
	systemPrompt string,
	messages []map[string]any,
	opts CompleteOptions,
) (string, error) {
	url := c.chatCompletionsURL()
	headers := c.buildHeaders()
	openaiMessages := c.buildMessages(systemPrompt, messages)

	body := map[string]any{
		"model":    c.Model,
		"messages": openaiMessages,
		"stream":   false,
	}
	if len(opts.Tools) > 0 {
		body["tools"] = AnthropicToolsToOpenAI(opts.Tools)
	}

	data, err := c.RetryRequest(ctx, func(reqCtx context.Context, httpClient *http.Client) (*http.Response, error) {
		return doJSONRequest(reqCtx, httpClient, http.MethodPost, url, headers, body)
	}, 30*time.Second)
	if err != nil {
		return "", err
	}

	choices, _ := data["choices"].([]any)
	if len(choices) == 0 {
		return "", nil
	}

	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	return getString(message, "content"), nil
}

func (c *OpenAICompatibleClient) StreamChat(
	ctx context.Context,
	systemPrompt string,
	messages []map[string]any,
	tools []map[string]any,
	options *LLMRequestOptions,
) <-chan EventResult {
	url := c.chatCompletionsURL()
	headers := c.buildHeaders()
	openaiMessages := c.buildMessages(systemPrompt, messages)

	bodyMap := map[string]any{
		"model":          c.Model,
		"messages":       openaiMessages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}

	if len(tools) > 0 {
		bodyMap["tools"] = AnthropicToolsToOpenAI(tools)
	}

	bodyBytes, err := json.Marshal(bodyMap)
	if err != nil {
		return singleErrorEvent(err)
	}

	return RetryWithBackoff(ctx, c.streamOpenAICompatible, func() (RequestParts, error) {
		return RequestParts{
			URL:     url,
			Headers: headers,
			Body:    bodyBytes,
		}, nil
	}, c.RetryConfig)
}

func (c *OpenAICompatibleClient) streamOpenAICompatible(
	ctx context.Context,
	url string,
	headers map[string]string,
	body []byte,
) <-chan EventResult {
	out := make(chan EventResult)

	go func() {
		defer close(out)

		resp, err := doRawJSONRequest(ctx, c.HTTPClient, http.MethodPost, url, headers, body)
		if err != nil {
			out <- EventResult{Err: err}
			return
		}
		defer func(Body io.ReadCloser) {
			_ = Body.Close()
		}(resp.Body)

		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			statusErr := NewAPIStatusError("DeepSeek", resp.StatusCode, resp.Status, bodyBytes)

			if isRetryableOpenAICompatibleStreamError(resp.StatusCode, statusErr.Body) {
				out <- EventResult{Err: RetryableStatusError{APIStatusError: statusErr}}
				return
			}

			out <- EventResult{Event: map[string]any{
				"type":        "error",
				"message":     statusErr.Error(),
				"status_code": resp.StatusCode,
				"raw_error":   statusErr.Body,
			}}
			return
		}

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

		type toolCallBuffer struct {
			ID        string
			Name      string
			Arguments string
		}

		toolCallBuffers := map[int]*toolCallBuffer{}
		finishReason := ""
		lastData := map[string]any{}

		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}

			dataStr := strings.TrimSpace(line[len("data:"):])
			if dataStr == "[DONE]" {
				break
			}
			if dataStr == "" {
				continue
			}

			var data map[string]any
			if err := json.Unmarshal([]byte(dataStr), &data); err != nil {
				continue
			}
			lastData = data

			choices, _ := data["choices"].([]any)
			if len(choices) == 0 {
				continue
			}

			choice, _ := choices[0].(map[string]any)
			delta, _ := choice["delta"].(map[string]any)

			if fr := getString(choice, "finish_reason"); fr != "" {
				finishReason = fr
			}

			if textContent := getString(delta, "content"); textContent != "" {
				out <- EventResult{Event: map[string]any{"type": "text_delta", "text": textContent}}
			}

			tcList, _ := delta["tool_calls"].([]any)
			for _, rawTC := range tcList {
				tc, ok := rawTC.(map[string]any)
				if !ok {
					continue
				}

				idx := getInt(tc, "index")
				buf := toolCallBuffers[idx]
				if buf == nil {
					buf = &toolCallBuffer{
						ID: getString(tc, "id"),
					}
					toolCallBuffers[idx] = buf
				}

				if id := getString(tc, "id"); id != "" {
					buf.ID = id
				}

				fn, _ := tc["function"].(map[string]any)
				if name := getString(fn, "name"); name != "" {
					buf.Name = name
				}
				if args := getString(fn, "arguments"); args != "" {
					buf.Arguments += args
				}
			}
		}

		if err := scanner.Err(); err != nil {
			out <- EventResult{Err: err}
			return
		}

		if id := getString(lastData, "id"); id != "" {
			out <- EventResult{Event: map[string]any{"type": "message_id", "id": id}}
		}

		if finishReason == "tool_calls" {
			indexes := make([]int, 0, len(toolCallBuffers))
			for idx := range toolCallBuffers {
				indexes = append(indexes, idx)
			}
			sort.Ints(indexes)

			for _, idx := range indexes {
				buf := toolCallBuffers[idx]
				if buf.Name != "" {
					parsed := map[string]any{}
					if strings.TrimSpace(buf.Arguments) != "" {
						if err := json.Unmarshal([]byte(buf.Arguments), &parsed); err != nil {
							parsed = map[string]any{}
						}
					}

					out <- EventResult{Event: map[string]any{
						"type":  "tool_use",
						"id":    buf.ID,
						"name":  buf.Name,
						"input": parsed,
					}}
				} else if buf.Arguments != "" {
					out <- EventResult{Event: map[string]any{
						"type":    "error",
						"message": fmt.Sprintf("Tool call at index %d is missing a function name; arguments were: %s", idx, truncateString(buf.Arguments, 200)),
					}}
				}
			}
		}

		usage, _ := lastData["usage"].(map[string]any)
		if len(usage) > 0 {
			out <- EventResult{Event: map[string]any{
				"type":          "usage",
				"input_tokens":  getInt(usage, "prompt_tokens"),
				"output_tokens": getInt(usage, "completion_tokens"),
			}}
		}

		stopReasonMap := map[string]string{
			"stop":           "end_turn",
			"tool_calls":     "tool_use",
			"length":         "max_tokens",
			"content_filter": "refusal",
		}

		mapped := stopReasonMap[finishReason]
		if mapped == "" {
			if finishReason == "" {
				mapped = "end_turn"
			} else {
				mapped = finishReason
			}
		}

		out <- EventResult{Event: map[string]any{"type": "stop_reason", "stop_reason": mapped}}
	}()

	return out
}

func CreateLLMClient(
	apiKey string,
	baseURL string,
	model string,
	maxTokens int,
	thinkingBudgetTokens *int,
	retryConfig *RetryConfig,
	apiType ...string,
) (LLMClient, error) {
	base, err := NewLLMClientBase(apiKey, baseURL, model, maxTokens, thinkingBudgetTokens, retryConfig)
	if err != nil {
		return nil, err
	}

	selectedAPIType := "auto"
	if len(apiType) > 0 {
		selectedAPIType = apiType[0]
	}
	if useOpenAICompatible(model, selectedAPIType) {
		return &OpenAICompatibleClient{LLMClientBase: base}, nil
	}

	return &AnthropicClient{LLMClientBase: base}, nil
}

type Client struct {
	delegate LLMClient
}

func NewAPIClient(
	apiKey string,
	baseURL string,
	model string,
	maxTokens int,
	thinkingBudgetTokens *int,
	retryConfig *RetryConfig,
	apiType ...string,
) (*Client, error) {
	delegate, err := CreateLLMClient(apiKey, baseURL, model, maxTokens, thinkingBudgetTokens, retryConfig, apiType...)
	if err != nil {
		return nil, err
	}

	return &Client{delegate: delegate}, nil
}

func (c *Client) Complete(
	ctx context.Context,
	systemPrompt string,
	messages []map[string]any,
	opts CompleteOptions,
) (string, error) {
	return c.delegate.Complete(ctx, systemPrompt, messages, opts)
}

func (c *Client) StreamChat(
	ctx context.Context,
	systemPrompt string,
	messages []map[string]any,
	tools []map[string]any,
	options *LLMRequestOptions,
) <-chan EventResult {
	return c.delegate.StreamChat(ctx, systemPrompt, messages, tools, options)
}

func doJSONRequest(
	ctx context.Context,
	httpClient *http.Client,
	method string,
	url string,
	headers map[string]string,
	body map[string]any,
) (*http.Response, error) {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	return doRawJSONRequest(ctx, httpClient, method, url, headers, bodyBytes)
}

func doRawJSONRequest(
	ctx context.Context,
	httpClient *http.Client,
	method string,
	url string,
	headers map[string]string,
	body []byte,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	for key, value := range headers {
		req.Header.Set(key, value)
	}

	return httpClient.Do(req)
}

func singleErrorEvent(err error) <-chan EventResult {
	ch := make(chan EventResult, 1)
	ch <- EventResult{Err: err}
	close(ch)
	return ch
}

func isSystemHintText(text string) bool {
	return strings.Contains(text, "<system_hint>")
}

func toolMessageFromResult(result map[string]any) map[string]any {
	content := getString(result, "content")
	if getBool(result, "is_error") {
		content = "[ERROR] " + content
	}

	return map[string]any{
		"role":         "tool",
		"tool_call_id": getString(result, "tool_use_id"),
		"content":      content,
	}
}

func fallbackToolMessage(toolCallID string) map[string]any {
	return map[string]any{
		"role":         "tool",
		"tool_call_id": toolCallID,
		"content":      "Tool execution was interrupted or lost before the provider request was retried.",
	}
}

type splitBlocksResult struct {
	TextParts   []string
	ToolCalls   []map[string]any
	ToolResults []map[string]any
}

func normalizeContentBlocks(content any) ([]map[string]any, bool) {
	switch blocks := content.(type) {
	case []map[string]any:
		return blocks, true
	case []any:
		normalized := make([]map[string]any, 0, len(blocks))
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				return nil, false
			}
			normalized = append(normalized, block)
		}
		return normalized, true
	default:
		return nil, false
	}
}

func splitAnthropicBlocks(content []map[string]any) splitBlocksResult {
	result := splitBlocksResult{
		TextParts:   []string{},
		ToolCalls:   []map[string]any{},
		ToolResults: []map[string]any{},
	}

	for _, block := range content {
		switch getString(block, "type") {
		case "text":
			result.TextParts = append(result.TextParts, getString(block, "text"))

		case "tool_use":
			arguments := pythonJSONDumps(block["input"])

			result.ToolCalls = append(result.ToolCalls, map[string]any{
				"id":   getString(block, "id"),
				"type": "function",
				"function": map[string]any{
					"name":      getString(block, "name"),
					"arguments": arguments,
				},
			})

		case "tool_result":
			result.ToolResults = append(result.ToolResults, map[string]any{
				"tool_use_id": getString(block, "tool_use_id"),
				"content":     extractToolResultText(block),
				"is_error":    getBool(block, "is_error"),
			})
		}
	}

	return result
}

type consumeResult struct {
	ToolMessages []map[string]any
	TrailingText string
	MatchedIDs   map[string]struct{}
}

func consumeToolResultCarrier(message map[string]any, expectedIDs map[string]struct{}) (*consumeResult, bool) {
	if getString(message, "role") != "user" {
		return nil, false
	}

	content, ok := normalizeContentBlocks(message["content"])
	if !ok {
		return nil, false
	}

	textParts := []string{}
	matchedIDs := map[string]struct{}{}
	toolMessages := []map[string]any{}
	sawToolResult := false

	for _, block := range content {
		switch getString(block, "type") {
		case "tool_result":
			toolUseID := getString(block, "tool_use_id")
			if _, ok := expectedIDs[toolUseID]; !ok {
				return nil, false
			}

			sawToolResult = true
			matchedIDs[toolUseID] = struct{}{}
			toolMessages = append(toolMessages, toolMessageFromResult(map[string]any{
				"tool_use_id": toolUseID,
				"content":     extractToolResultText(block),
				"is_error":    getBool(block, "is_error"),
			}))

		case "text":
			text := getString(block, "text")
			if isSystemHintText(text) {
				textParts = append(textParts, text)
				continue
			}
			return nil, false

		default:
			return nil, false
		}
	}

	if !sawToolResult {
		return nil, false
	}

	return &consumeResult{
		ToolMessages: toolMessages,
		TrailingText: strings.Join(nonEmptyStrings(textParts), "\n"),
		MatchedIDs:   matchedIDs,
	}, true
}

func convertAnthropicMessagesToOpenAI(messages []map[string]any) []map[string]any {
	converted := []map[string]any{}
	i := 0

	for i < len(messages) {
		msg := messages[i]
		role := getStringDefault(msg, "role", "user")
		content := msg["content"]

		if contentStr, ok := content.(string); ok {
			converted = append(converted, map[string]any{"role": role, "content": contentStr})
			i++
			continue
		}

		contentList, ok := normalizeContentBlocks(content)
		if !ok {
			converted = append(converted, map[string]any{"role": role, "content": fmt.Sprint(content)})
			i++
			continue
		}

		parts := splitAnthropicBlocks(contentList)

		if role == "assistant" && len(parts.ToolCalls) > 0 {
			assistantEntry := map[string]any{
				"role":       "assistant",
				"content":    strings.Join(parts.TextParts, "\n"),
				"tool_calls": parts.ToolCalls,
			}
			converted = append(converted, assistantEntry)

			expectedIDs := map[string]struct{}{}
			for _, call := range parts.ToolCalls {
				id := getString(call, "id")
				if id != "" {
					expectedIDs[id] = struct{}{}
				}
			}

			i++

			for i < len(messages) {
				consumed, ok := consumeToolResultCarrier(messages[i], expectedIDs)
				if !ok {
					break
				}

				converted = append(converted, consumed.ToolMessages...)
				if consumed.TrailingText != "" {
					converted = append(converted, map[string]any{"role": "user", "content": consumed.TrailingText})
				}

				for id := range consumed.MatchedIDs {
					delete(expectedIDs, id)
				}
				i++
			}

			missingIDs := make([]string, 0, len(expectedIDs))
			for id := range expectedIDs {
				missingIDs = append(missingIDs, id)
			}
			sort.Strings(missingIDs)

			for _, missingID := range missingIDs {
				converted = append(converted, fallbackToolMessage(missingID))
			}
			continue
		}

		if role == "user" && len(parts.ToolResults) > 0 {
			if len(parts.TextParts) > 0 {
				converted = append(converted, map[string]any{
					"role":    "user",
					"content": strings.Join(parts.TextParts, "\n"),
				})
			}
			i++
			continue
		}

		if len(parts.TextParts) > 0 {
			converted = append(converted, map[string]any{
				"role":    role,
				"content": strings.Join(parts.TextParts, "\n"),
			})
			i++
			continue
		}

		if role == "assistant" || role == "user" {
			converted = append(converted, map[string]any{"role": role, "content": ""})
			i++
			continue
		}

		i++
	}

	return converted
}

func extractToolResultText(block map[string]any) string {
	resultContent := block["content"]

	if s, ok := resultContent.(string); ok {
		return s
	}

	if items, ok := resultContent.([]any); ok {
		parts := []string{}
		for _, rawItem := range items {
			item, ok := rawItem.(map[string]any)
			if ok && getString(item, "type") == "text" {
				parts = append(parts, getString(item, "text"))
			}
		}
		return strings.Join(parts, "\n")
	}

	if items, ok := resultContent.([]map[string]any); ok {
		parts := []string{}
		for _, item := range items {
			if getString(item, "type") == "text" {
				parts = append(parts, getString(item, "text"))
			}
		}
		return strings.Join(parts, "\n")
	}

	if resultContent == nil {
		return ""
	}

	return fmt.Sprint(resultContent)
}

func isRetryableOpenAICompatibleStreamError(statusCode int, errorText string) bool {
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	if statusCode == http.StatusInternalServerError || statusCode == http.StatusServiceUnavailable || statusCode == 529 {
		for _, keyword := range []string{
			"model_not_found",
			"invalid_request_error",
			"invalid_api_key",
			"authentication",
			"authorization",
			"not_found",
		} {
			if strings.Contains(errorText, keyword) {
				return false
			}
		}
		return true
	}
	return false
}

func isRetryableAnthropicStreamError(statusCode int, errorText string) bool {
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	if statusCode == http.StatusServiceUnavailable || statusCode == 529 {
		for _, keyword := range []string{
			"model_not_found",
			"invalid_request_error",
			"invalid_api_key",
			"authentication",
			"authorization",
			"not_found",
		} {
			if strings.Contains(errorText, keyword) {
				return false
			}
		}
		return true
	}
	return false
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key].(string)
	if !ok {
		return ""
	}
	return v
}

func getStringDefault(m map[string]any, key string, fallback string) string {
	v := getString(m, key)
	if v == "" {
		return fallback
	}
	return v
}

func getInt(m map[string]any, key string) int {
	if m == nil {
		return 0
	}

	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	default:
		return 0
	}
}

func getBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	v, ok := m[key].(bool)
	return ok && v
}

func truncateString(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}

func nonEmptyStrings(items []string) []string {
	result := make([]string, 0, len(items))
	for _, item := range items {
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

func pythonJSONDumps(value any) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "{}"
	}
	return string(spacePythonJSONSeparators(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))))
}

func spacePythonJSONSeparators(data []byte) []byte {
	out := make([]byte, 0, len(data)+8)
	inString := false
	escaped := false
	for _, b := range data {
		out = append(out, b)
		if escaped {
			escaped = false
			continue
		}
		if inString {
			if b == '\\' {
				escaped = true
			} else if b == '"' {
				inString = false
			}
			continue
		}
		if b == '"' {
			inString = true
			continue
		}
		if b == ':' || b == ',' {
			out = append(out, ' ')
		}
	}
	return out
}
