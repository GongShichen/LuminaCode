package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"LuminaCode/api"
)

func writeSSEEvent(w http.ResponseWriter, event map[string]any) {
	encoded, _ := json.Marshal(event)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
}

func writeOpenAITextStream(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	writeSSEEvent(w, map[string]any{
		"id": "msg-1", "choices": []any{map[string]any{
			"delta": map[string]any{"content": text}, "finish_reason": "stop",
		}}, "usage": map[string]any{"prompt_tokens": 2, "completion_tokens": 3},
	})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func writeOpenAIToolStream(w http.ResponseWriter, name string, input map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	encoded, _ := json.Marshal(input)
	midpoint := len(encoded) / 2
	firstCall := map[string]any{
		"index": 0, "id": "call-1",
		"function": map[string]any{"name": name, "arguments": string(encoded[:midpoint])},
	}
	writeSSEEvent(w, map[string]any{
		"id": "msg-1", "choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{firstCall}},
		}},
	})
	secondCall := map[string]any{
		"index": 0, "function": map[string]any{"arguments": string(encoded[midpoint:])},
	}
	writeSSEEvent(w, map[string]any{
		"id": "msg-1", "choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{secondCall}}, "finish_reason": "tool_calls",
		}}, "usage": map[string]any{"prompt_tokens": 2, "completion_tokens": 4},
	})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func writeAnthropicTextStream(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	writeSSEEvent(w, map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg-1", "usage": map[string]any{"input_tokens": 2},
	}})
	writeSSEEvent(w, map[string]any{"type": "content_block_delta", "delta": map[string]any{
		"type": "text_delta", "text": text,
	}})
	writeSSEEvent(w, map[string]any{"type": "message_delta", "delta": map[string]any{
		"stop_reason": "end_turn",
	}, "usage": map[string]any{"output_tokens": 3}})
}

func writeAnthropicToolStream(w http.ResponseWriter, name string, input map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	writeSSEEvent(w, map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg-1", "usage": map[string]any{"input_tokens": 2},
	}})
	writeSSEEvent(w, map[string]any{"type": "content_block_start", "content_block": map[string]any{
		"type": "tool_use", "id": "call-1", "name": name,
	}})
	encoded, _ := json.Marshal(input)
	midpoint := len(encoded) / 2
	for _, part := range []string{string(encoded[:midpoint]), string(encoded[midpoint:])} {
		writeSSEEvent(w, map[string]any{"type": "content_block_delta", "delta": map[string]any{
			"type": "input_json_delta", "partial_json": part,
		}})
	}
	writeSSEEvent(w, map[string]any{"type": "content_block_stop"})
	writeSSEEvent(w, map[string]any{"type": "message_delta", "delta": map[string]any{
		"stop_reason": "tool_use",
	}, "usage": map[string]any{"output_tokens": 4}})
}

func TestOpenAICompatibleMessageConversionAcceptsGoBlockSlicesLikePythonLists(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
			t.Fatalf("invalid request body: %v body=%s", err, bodyBytes)
		}
		writeOpenAITextStream(w, "ok")
	}))
	defer server.Close()

	client, err := api.NewAPIClient("test-key", server.URL, "gpt-5", 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	messages := []map[string]any{
		{"role": "assistant", "content": []map[string]any{
			{"type": "text", "text": "I will call a tool."},
			{"type": "tool_use", "id": "tool-1", "name": "read_file", "input": map[string]any{"file_path": "main.go"}},
		}},
		{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "tool-1", "content": []map[string]any{{"type": "text", "text": "file body"}}},
		}},
	}
	if _, err := client.Complete(context.Background(), "sys", messages, api.CompleteOptions{MaxTokens: 32}); err != nil {
		t.Fatal(err)
	}

	openAIMessages, _ := requestBody["messages"].([]any)
	if len(openAIMessages) != 3 {
		t.Fatalf("expected system, assistant, tool messages, got %#v", openAIMessages)
	}
	assistantMsg, _ := openAIMessages[1].(map[string]any)
	if assistantMsg["role"] != "assistant" || assistantMsg["content"] != "I will call a tool." {
		t.Fatalf("unexpected assistant message conversion: %#v", assistantMsg)
	}
	toolCalls, _ := assistantMsg["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("expected tool_calls from []map content blocks, got %#v", assistantMsg)
	}
	toolCall, _ := toolCalls[0].(map[string]any)
	function, _ := toolCall["function"].(map[string]any)
	if function["name"] != "read_file" || !strings.Contains(fmt.Sprint(function["arguments"]), "main.go") {
		t.Fatalf("unexpected tool call conversion: %#v", toolCall)
	}
	toolMsg, _ := openAIMessages[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "tool-1" || toolMsg["content"] != "file body" {
		t.Fatalf("unexpected tool result conversion: %#v", toolMsg)
	}
}

func TestOpenAICompatibleToolCallArgumentsUsePythonJSONDumpsFormatting(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
			t.Fatalf("invalid request body: %v body=%s", err, bodyBytes)
		}
		writeOpenAITextStream(w, "ok")
	}))
	defer server.Close()

	client, err := api.NewAPIClient("test-key", server.URL, "gpt-5", 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	messages := []map[string]any{{
		"role": "assistant",
		"content": []map[string]any{{
			"type": "tool_use",
			"id":   "tool-1",
			"name": "inspect",
			"input": map[string]any{
				"text": "值<tag>",
				"n":    1,
			},
		}},
	}}
	if _, err := client.Complete(context.Background(), "sys", messages, api.CompleteOptions{MaxTokens: 32}); err != nil {
		t.Fatal(err)
	}
	openAIMessages, _ := requestBody["messages"].([]any)
	assistantMsg, _ := openAIMessages[1].(map[string]any)
	toolCalls, _ := assistantMsg["tool_calls"].([]any)
	toolCall, _ := toolCalls[0].(map[string]any)
	function, _ := toolCall["function"].(map[string]any)
	arguments, _ := function["arguments"].(string)
	for _, want := range []string{`"text": "值<tag>"`, `"n": 1`} {
		if !strings.Contains(arguments, want) {
			t.Fatalf("tool call arguments should match Python json.dumps formatting, missing %q in %q", want, arguments)
		}
	}
	if strings.Contains(arguments, `"text":"`) || strings.Contains(arguments, `\u003c`) {
		t.Fatalf("tool call arguments should use Python separators and avoid Go HTML escaping, got %q", arguments)
	}
}

func TestOpenAICompatibleDropsMixedIDToolResultCarrierLikePython(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
			t.Fatalf("invalid request body: %v body=%s", err, bodyBytes)
		}
		writeOpenAITextStream(w, "ok")
	}))
	defer server.Close()

	client, err := api.NewAPIClient("test-key", server.URL, "gpt-5", 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	messages := []map[string]any{
		{"role": "assistant", "content": []map[string]any{{
			"type": "tool_use", "id": "call-1", "name": "glob_match", "input": map[string]any{"pattern": "**/*.go"},
		}}},
		{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "call-1", "content": "a.go"},
			{"type": "tool_result", "tool_use_id": "call-2", "content": "other turn result"},
		}},
		{"role": "assistant", "content": []map[string]any{{
			"type": "tool_use", "id": "call-2", "name": "grep_search", "input": map[string]any{"pattern": "needle", "path": "src"},
		}}},
	}
	if _, err := client.Complete(context.Background(), "", messages, api.CompleteOptions{MaxTokens: 32}); err != nil {
		t.Fatal(err)
	}

	openAIMessages, _ := requestBody["messages"].([]any)
	if len(openAIMessages) != 4 {
		t.Fatalf("expected two assistant messages plus synthesized fallback tools, got %#v", openAIMessages)
	}
	firstFallback, _ := openAIMessages[1].(map[string]any)
	secondFallback, _ := openAIMessages[3].(map[string]any)
	for _, item := range []struct {
		msg map[string]any
		id  string
	}{
		{firstFallback, "call-1"},
		{secondFallback, "call-2"},
	} {
		if item.msg["role"] != "tool" || item.msg["tool_call_id"] != item.id || !strings.Contains(fmt.Sprint(item.msg["content"]), "interrupted or lost") {
			t.Fatalf("expected Python fallback tool message for %s, got %#v", item.id, item.msg)
		}
	}
	for _, raw := range openAIMessages {
		msg, _ := raw.(map[string]any)
		if msg["role"] == "tool" && (msg["content"] == "a.go" || msg["content"] == "other turn result") {
			t.Fatalf("mixed-id carrier tool result should be dropped like Python, got %#v", openAIMessages)
		}
	}
}

func TestOpenAICompatibleSynthesizesMissingToolMessageLikePython(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
			t.Fatalf("invalid request body: %v body=%s", err, bodyBytes)
		}
		writeOpenAITextStream(w, "ok")
	}))
	defer server.Close()

	client, err := api.NewAPIClient("test-key", server.URL, "gpt-5", 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	messages := []map[string]any{{
		"role": "assistant",
		"content": []map[string]any{{
			"type":  "tool_use",
			"id":    "call-1",
			"name":  "glob_match",
			"input": map[string]any{"pattern": "**/*.go"},
		}},
	}}
	if _, err := client.Complete(context.Background(), "", messages, api.CompleteOptions{MaxTokens: 32}); err != nil {
		t.Fatal(err)
	}

	openAIMessages, _ := requestBody["messages"].([]any)
	if len(openAIMessages) != 2 {
		t.Fatalf("expected assistant plus synthesized fallback tool message, got %#v", openAIMessages)
	}
	assistantMsg, _ := openAIMessages[0].(map[string]any)
	if assistantMsg["role"] != "assistant" || assistantMsg["content"] != "" {
		t.Fatalf("tool-only assistant should have explicit empty content like Python, got %#v", assistantMsg)
	}
	toolMsg, _ := openAIMessages[1].(map[string]any)
	if toolMsg["role"] != "tool" ||
		toolMsg["tool_call_id"] != "call-1" ||
		!strings.Contains(fmt.Sprint(toolMsg["content"]), "interrupted or lost") {
		t.Fatalf("expected synthesized Python fallback tool message, got %#v", toolMsg)
	}
}

func TestOpenAICompatibleAPITypeSupportsCustomModelNames(t *testing.T) {
	var requestBody map[string]any
	var requestPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
			t.Fatalf("invalid request body: %v body=%s", err, bodyBytes)
		}
		writeOpenAITextStream(w, "ok")
	}))
	defer server.Close()

	client, err := api.NewAPIClient("test-key", server.URL, "custom-router-model", 256, nil, nil, "openai_compatible")
	if err != nil {
		t.Fatal(err)
	}
	text, err := client.Complete(context.Background(), "sys", []map[string]any{{"role": "user", "content": "hi"}}, api.CompleteOptions{MaxTokens: 32})
	if err != nil {
		t.Fatal(err)
	}
	if text != "ok" {
		t.Fatalf("unexpected response text: %q", text)
	}
	if requestPath != "/chat/completions" {
		t.Fatalf("expected OpenAI-compatible endpoint for custom model, got %s", requestPath)
	}
	if requestBody["model"] != "custom-router-model" || requestBody["stream"] != true ||
		requestBody["max_completion_tokens"] != float64(32) {
		t.Fatalf("unexpected OpenAI-compatible request body: %#v", requestBody)
	}
}

func TestOpenAICompatibleAcceptsFullChatCompletionsBaseURLLikeLuminaDefaults(t *testing.T) {
	var requestPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		writeOpenAITextStream(w, "ok")
	}))
	defer server.Close()

	client, err := api.NewAPIClient("test-key", server.URL+"/chat/completions", "deepseek-v4-pro", 256, nil, nil, "openai_compatible")
	if err != nil {
		t.Fatal(err)
	}
	text, err := client.Complete(context.Background(), "sys", []map[string]any{{"role": "user", "content": "hi"}}, api.CompleteOptions{MaxTokens: 32})
	if err != nil {
		t.Fatal(err)
	}
	if text != "ok" {
		t.Fatalf("unexpected response text: %q", text)
	}
	if requestPath != "/chat/completions" {
		t.Fatalf("full chat completions base URL should not append endpoint twice, got %s", requestPath)
	}
}

func TestDeepSeekAnthropicDefaultsSendClaudeCompatibleRequest(t *testing.T) {
	var requestPath string
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		bodyBytes, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
			t.Fatalf("invalid request body: %v body=%s", err, bodyBytes)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("x-api-key") != "" {
			t.Fatalf("DeepSeek Anthropic-compatible request used the wrong authentication headers: %#v", r.Header)
		}
		writeAnthropicTextStream(w, "ok")
	}))
	defer server.Close()

	client, err := api.NewAPIClient("test-key", server.URL, "deepseek-v4-pro[1m]", 256, nil, nil, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	text, err := client.Complete(context.Background(), "sys", []map[string]any{{"role": "user", "content": "hi"}}, api.CompleteOptions{MaxTokens: 32})
	if err != nil {
		t.Fatal(err)
	}
	if text != "ok" {
		t.Fatalf("unexpected response text: %q", text)
	}
	if requestPath != "/v1/messages" {
		t.Fatalf("deepseek anthropic base should use Claude-compatible messages path, got %s", requestPath)
	}
	if requestBody["model"] != "deepseek-v4-pro[1m]" || requestBody["stream"] != true ||
		requestBody["max_tokens"] != float64(32) {
		t.Fatalf("unexpected anthropic request body: %#v", requestBody)
	}
}

func TestDeepSeekStructuredCompletionDisablesThinkingForForcedTool(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
			t.Fatalf("invalid request body: %v", err)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("missing DeepSeek bearer authentication: %#v", r.Header)
		}
		writeAnthropicToolStream(w, "ExpandMemoryQuery", map[string]any{"queries": []any{"Aurora"}})
	}))
	defer server.Close()
	client, err := api.NewAPIClient("test-key", server.URL, "deepseek-v4-pro[1m]", 256, nil, nil, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.CompleteStructured(context.Background(), "sys", []map[string]any{{"role": "user", "content": "hi"}},
		api.StructuredCompletionOptions{RequiredTool: "ExpandMemoryQuery", DisableThinking: true,
			Tools: []map[string]any{{"name": "ExpandMemoryQuery", "input_schema": map[string]any{"type": "object"}}}})
	if err != nil || len(response.ToolCalls) != 1 {
		t.Fatalf("structured DeepSeek response failed: %#v error=%v", response, err)
	}
	thinking, _ := requestBody["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Fatalf("forced tool request did not disable thinking: %#v", requestBody)
	}
	if requestBody["stream"] != true {
		t.Fatalf("structured completion must stream: %#v", requestBody)
	}
}

func TestOpenAICompatibleStructuredCompletionStreamsSplitToolInput(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
			t.Fatalf("invalid request body: %v", err)
		}
		writeOpenAIToolStream(w, "ExtractMemoryBatch", map[string]any{"memories": []any{}})
	}))
	defer server.Close()
	client, err := api.NewAPIClient("test-key", server.URL, "deepseek-chat", 256, nil, nil, "openai_compatible")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.CompleteStructured(context.Background(), "sys", nil, api.StructuredCompletionOptions{
		MaxTokens: 128, RequiredTool: "ExtractMemoryBatch", DisableThinking: true,
		Tools: []map[string]any{{"name": "ExtractMemoryBatch", "input_schema": map[string]any{"type": "object"}}},
	})
	if err != nil || len(response.ToolCalls) != 1 {
		t.Fatalf("streamed structured completion failed: response=%#v error=%v", response, err)
	}
	input, _ := response.ToolCalls[0]["input"].(map[string]any)
	if _, ok := input["memories"]; !ok {
		t.Fatalf("split tool arguments were not reconstructed: %#v", response.ToolCalls[0])
	}
	toolChoice, _ := requestBody["tool_choice"].(map[string]any)
	if requestBody["stream"] != true || requestBody["max_completion_tokens"] != float64(128) ||
		toolChoice["type"] != "function" {
		t.Fatalf("structured request did not preserve streaming options: %#v", requestBody)
	}
}

func TestOpenAICompatibleChatCompletionsURLNormalizesDeepSeekBaseURLLikePython(t *testing.T) {
	for _, suffix := range []string{"", "/", "/v1", "/v1/", "/chat/completions"} {
		t.Run(suffix, func(t *testing.T) {
			var requestPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestPath = r.URL.Path
				writeOpenAITextStream(w, "ok")
			}))
			defer server.Close()

			client, err := api.NewAPIClient("test-key", server.URL+suffix, "deepseek-chat", 256, nil, nil, "openai_compatible")
			if err != nil {
				t.Fatal(err)
			}
			text, err := client.Complete(context.Background(), "sys", []map[string]any{{"role": "user", "content": "hi"}}, api.CompleteOptions{MaxTokens: 32})
			if err != nil {
				t.Fatal(err)
			}
			if text != "ok" || requestPath != "/chat/completions" {
				t.Fatalf("base URL suffix %q should normalize like Python, text=%q path=%s", suffix, text, requestPath)
			}
		})
	}
}

func TestParseSSEStreamHandlesLongDataLinesLikePython(t *testing.T) {
	longText := strings.Repeat("x", 128*1024)
	stream := "data:{\"text\":\"ignored-no-space\"}\ndata: {\"text\":\"" + longText + "\"}\n:data comment\n\ndata: [DONE]\n"
	events, errs := api.ParseSSEStream(strings.NewReader(stream))
	var got []map[string]any
	for event := range events {
		got = append(got, event)
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("unexpected SSE parser error: %v", err)
		}
	}
	if len(got) != 1 || got[0]["text"] != longText {
		t.Fatalf("unexpected parsed events: len=%d first=%#v", len(got), got)
	}
}

func TestOpenAICompatibleStreamRetriesHTTP500LikePython(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			http.Error(w, "temporary overload", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	retryConfig := api.DefaultRetryConfig()
	retryConfig.MaxRetries = 1
	retryConfig.BaseDelay = 0
	retryConfig.Jitter = false
	client, err := api.NewAPIClient("test-key", server.URL, "gpt-5", 256, nil, &retryConfig)
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for item := range client.StreamChat(context.Background(), "sys", nil, nil, nil) {
		if item.Err != nil {
			t.Fatalf("unexpected stream error: %v", item.Err)
		}
		events = append(events, item.Event)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected one retry after HTTP 500, got %d calls", calls.Load())
	}
	if len(events) == 0 || events[0]["type"] != "text_delta" || events[0]["text"] != "ok" {
		t.Fatalf("unexpected stream events: %#v", events)
	}
}

func TestAnthropicStreamConditionalRetryDoesNotTreatPermissionAsPermanentLikePython(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			http.Error(w, "permission overload", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"usage\":{\"input_tokens\":2}}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3},\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
	}))
	defer server.Close()

	retryConfig := api.DefaultRetryConfig()
	retryConfig.MaxRetries = 1
	retryConfig.BaseDelay = 0
	retryConfig.Jitter = false
	client, err := api.NewAPIClient("test-key", server.URL, "claude-sonnet-4", 256, nil, &retryConfig, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for item := range client.StreamChat(context.Background(), "sys", nil, nil, nil) {
		if item.Err != nil {
			t.Fatalf("unexpected stream error: %v", item.Err)
		}
		events = append(events, item.Event)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected Anthropic stream retry despite lowercase permission keyword, got %d calls", calls.Load())
	}
	if len(events) < 2 || events[1]["type"] != "text_delta" || events[1]["text"] != "ok" {
		t.Fatalf("unexpected Anthropic stream events: %#v", events)
	}
}

func TestAnthropicThinkingBudgetZeroIsOmittedLikePython(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
			t.Fatalf("invalid request body: %v body=%s", err, bodyBytes)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"usage\":{\"input_tokens\":2}}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3},\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
	}))
	defer server.Close()

	zero := 0
	client, err := api.NewAPIClient("test-key", server.URL, "claude-sonnet-4", 256, &zero, nil, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	for item := range client.StreamChat(context.Background(), "sys", nil, nil, nil) {
		if item.Err != nil {
			t.Fatalf("unexpected stream error: %v", item.Err)
		}
	}
	if _, ok := requestBody["thinking"]; ok {
		t.Fatalf("zero thinking_budget_tokens should be omitted like Python, got %#v", requestBody["thinking"])
	}
}

func TestProviderSpecificCacheEditOptionsStayInternalLikePython(t *testing.T) {
	for _, tc := range []struct {
		name    string
		model   string
		apiType string
	}{
		{name: "anthropic", model: "claude-sonnet-4", apiType: "anthropic"},
		{name: "openai-compatible", model: "custom-model", apiType: "openai_compatible"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requestBody map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				bodyBytes, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
					t.Fatalf("invalid request body: %v body=%s", err, bodyBytes)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				switch tc.apiType {
				case "anthropic":
					_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"usage\":{\"input_tokens\":2}}}\n\n")
					_, _ = fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3},\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
				default:
					_, _ = fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\n")
					_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
				}
			}))
			defer server.Close()

			client, err := api.NewAPIClient("test-key", server.URL, tc.model, 256, nil, nil, tc.apiType)
			if err != nil {
				t.Fatal(err)
			}
			options := &api.LLMRequestOptions{AnthropicCacheEdits: []api.CacheEdit{{ToolUseID: "tool-1", Action: "delete"}}}
			for item := range client.StreamChat(context.Background(), "sys", nil, nil, options) {
				if item.Err != nil {
					t.Fatalf("unexpected stream error: %v", item.Err)
				}
			}
			if _, ok := requestBody["anthropic_cache_edits"]; ok {
				t.Fatalf("cache edit options are internal and should not be sent to provider like Python, body=%#v", requestBody)
			}
			if _, ok := requestBody["cache_edits"]; ok {
				t.Fatalf("cache edit options are internal and should not be sent to provider like Python, body=%#v", requestBody)
			}
			if len(options.AnthropicCacheEdits) != 1 || options.AnthropicCacheEdits[0].ToolUseID != "tool-1" {
				t.Fatalf("request options should remain caller-owned like Python dataclass, got %#v", options)
			}
		})
	}
}

func TestOpenAICompatibleStreamErrorPreservesRawStatusAndBody(t *testing.T) {
	errorBody := strings.Repeat("错", 501)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, errorBody, http.StatusBadRequest)
	}))
	defer server.Close()

	client, err := api.NewAPIClient("test-key", server.URL, "custom-model", 256, nil, nil, "openai_compatible")
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for item := range client.StreamChat(context.Background(), "sys", nil, nil, nil) {
		if item.Err != nil {
			t.Fatalf("unexpected stream error: %v", item.Err)
		}
		events = append(events, item.Event)
	}
	if len(events) != 1 || events[0]["type"] != "error" {
		t.Fatalf("expected one error event, got %#v", events)
	}
	message, _ := events[0]["message"].(string)
	if !strings.Contains(message, "custom-model API error 400 400 Bad Request") ||
		!strings.Contains(message, strings.Repeat("错", 501)) {
		t.Fatalf("error should preserve status and full raw body, got %q", message)
	}
	if events[0]["status_code"] != http.StatusBadRequest || events[0]["raw_error"] != errorBody+"\n" {
		t.Fatalf("error event should expose status_code and raw_error metadata, got %#v", events[0])
	}
}

func TestAPIErrorMetadataExposesTransportErrors(t *testing.T) {
	err := &url.Error{Op: "Post", URL: "https://example.invalid/v1/messages", Err: errors.New("EOF")}
	metadata := api.ErrorMetadata(err)
	if metadata["error_type"] != "transport_error" {
		t.Fatalf("expected transport_error, got %#v", metadata)
	}
	if metadata["operation"] != "Post" || metadata["request_url"] != "https://example.invalid/v1/messages" {
		t.Fatalf("transport metadata should preserve operation and URL, got %#v", metadata)
	}
	if metadata["raw_error"] != "EOF" {
		t.Fatalf("transport metadata should preserve raw error, got %#v", metadata)
	}
	if _, ok := metadata["status_code"]; ok {
		t.Fatalf("transport error should not invent an HTTP status_code, got %#v", metadata)
	}
}

func TestOpenAICompatibleCompleteErrorPreservesStatusCodeAndRawBody(t *testing.T) {
	errorBody := `{"error":{"message":"bad request detail","code":"E_BAD"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, errorBody)
	}))
	defer server.Close()

	client, err := api.NewAPIClient("test-key", server.URL, "custom-model", 256, nil, nil, "openai_compatible")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Complete(context.Background(), "sys", nil, api.CompleteOptions{})
	if err == nil {
		t.Fatal("expected complete request to fail")
	}
	var statusErr api.APIStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected APIStatusError, got %T %v", err, err)
	}
	if statusErr.StatusCode != http.StatusBadRequest || statusErr.Body != errorBody {
		t.Fatalf("status error should preserve code/body, got %#v", statusErr)
	}
	if !strings.Contains(err.Error(), "custom-model API error 400 400 Bad Request") || !strings.Contains(err.Error(), errorBody) {
		t.Fatalf("wrapped error should include status and raw body, got %q", err.Error())
	}
}

func TestOpenAICompatibleCompleteConditionalRetryIsCaseSensitiveLikePython(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			http.Error(w, "Permission overload", http.StatusServiceUnavailable)
			return
		}
		writeOpenAITextStream(w, "ok")
	}))
	defer server.Close()

	retryConfig := api.DefaultRetryConfig()
	retryConfig.MaxRetries = 1
	retryConfig.BaseDelay = 0
	retryConfig.Jitter = false
	client, err := api.NewAPIClient("test-key", server.URL, "gpt-5", 256, nil, &retryConfig)
	if err != nil {
		t.Fatal(err)
	}
	text, err := client.Complete(context.Background(), "sys", []map[string]any{{"role": "user", "content": "hi"}}, api.CompleteOptions{MaxTokens: 32})
	if err != nil {
		t.Fatal(err)
	}
	if text != "ok" || calls.Load() != 2 {
		t.Fatalf("expected case-sensitive conditional retry like Python, text=%q calls=%d", text, calls.Load())
	}
}

func TestOpenAICompatibleStreamIdleTimeoutReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, ": keepalive\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	client, err := api.NewAPIClient("test-key", server.URL, "gpt-5", 256, nil, nil, "openai_compatible")
	if err != nil {
		t.Fatal(err)
	}
	ctx := api.ContextWithStreamIdleTimeout(context.Background(), 20*time.Millisecond)
	var gotMessage string
	for item := range client.StreamChat(ctx, "sys", nil, nil, nil) {
		if item.Err != nil {
			gotMessage = item.Err.Error()
			break
		}
		if item.Event["type"] == "error" {
			gotMessage = fmt.Sprint(item.Event["message"])
			break
		}
	}
	if !strings.Contains(gotMessage, "API stream idle timeout") {
		t.Fatalf("expected stream idle timeout error, got %q", gotMessage)
	}
}

func TestAnthropicStreamIdleTimeoutReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, ": keepalive\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	client, err := api.NewAPIClient("test-key", server.URL, "claude-sonnet-4", 256, nil, nil, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	ctx := api.ContextWithStreamIdleTimeout(context.Background(), 20*time.Millisecond)
	var gotMessage string
	for item := range client.StreamChat(ctx, "sys", nil, nil, nil) {
		if item.Err != nil {
			gotMessage = item.Err.Error()
			break
		}
		if item.Event["type"] == "error" {
			gotMessage = fmt.Sprint(item.Event["message"])
			break
		}
	}
	if !strings.Contains(gotMessage, "API stream idle timeout") {
		t.Fatalf("expected stream idle timeout error, got %q", gotMessage)
	}
}
