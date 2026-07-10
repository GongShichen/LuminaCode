package test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"LuminaCode/api"
	"LuminaCode/config"
)

type fallbackClientStub struct {
	stream        []api.EventResult
	completeText  string
	completeErr   error
	streamCalls   int
	completeCalls int
}

func (s *fallbackClientStub) StreamChat(context.Context, string, []map[string]any, []map[string]any, *api.LLMRequestOptions) <-chan api.EventResult {
	s.streamCalls++
	out := make(chan api.EventResult, len(s.stream))
	for _, item := range s.stream {
		out <- item
	}
	close(out)
	return out
}

func (s *fallbackClientStub) Complete(context.Context, string, []map[string]any, api.CompleteOptions) (string, error) {
	s.completeCalls++
	return s.completeText, s.completeErr
}

func TestFallbackClientSwitchesOnRetryableFailureBeforeOutput(t *testing.T) {
	primary := &fallbackClientStub{stream: []api.EventResult{{Event: map[string]any{
		"type": "error", "message": "too many requests", "status_code": 429, "raw_error": `{"code":"429"}`,
	}}}}
	secondary := &fallbackClientStub{stream: []api.EventResult{{Event: map[string]any{"type": "text_delta", "text": "fallback answer"}}}}
	client := api.NewFallbackLLMClient(primary, secondary, "primary", "secondary")

	var events []map[string]any
	for item := range client.StreamChat(context.Background(), "", nil, nil, nil) {
		if item.Err != nil {
			t.Fatal(item.Err)
		}
		events = append(events, item.Event)
	}
	if secondary.streamCalls != 1 || len(events) != 2 {
		t.Fatalf("expected one fallback request and two events, calls=%d events=%#v", secondary.streamCalls, events)
	}
	if events[0]["type"] != "model_fallback" || events[0]["fallback_model"] != "secondary" {
		t.Fatalf("missing fallback transition event: %#v", events[0])
	}
	if events[1]["text"] != "fallback answer" {
		t.Fatalf("unexpected fallback output: %#v", events[1])
	}
}

func TestFallbackClientDoesNotSwitchAfterOutput(t *testing.T) {
	primary := &fallbackClientStub{stream: []api.EventResult{
		{Event: map[string]any{"type": "text_delta", "text": "partial"}},
		{Event: map[string]any{"type": "error", "message": "too many requests", "status_code": 429}},
	}}
	secondary := &fallbackClientStub{stream: []api.EventResult{{Event: map[string]any{"type": "text_delta", "text": "duplicate"}}}}
	client := api.NewFallbackLLMClient(primary, secondary, "primary", "secondary")

	var eventTypes []any
	for item := range client.StreamChat(context.Background(), "", nil, nil, nil) {
		eventTypes = append(eventTypes, item.Event["type"])
	}
	if secondary.streamCalls != 0 {
		t.Fatal("fallback must not run after the primary emitted visible output")
	}
	if !reflect.DeepEqual(eventTypes, []any{"text_delta", "error"}) {
		t.Fatalf("primary stream was not preserved: %#v", eventTypes)
	}
}

func TestFallbackClientDoesNotSwitchOnPermanentClientError(t *testing.T) {
	primary := &fallbackClientStub{stream: []api.EventResult{{Event: map[string]any{
		"type": "error", "message": "invalid API key", "status_code": 401,
	}}}}
	secondary := &fallbackClientStub{}
	client := api.NewFallbackLLMClient(primary, secondary, "primary", "secondary")

	for range client.StreamChat(context.Background(), "", nil, nil, nil) {
	}
	if secondary.streamCalls != 0 {
		t.Fatal("fallback must not hide authentication or configuration errors")
	}
}

func TestFallbackClientCompleteAndCombinedFailure(t *testing.T) {
	primaryErr := api.NewAPIStatusError("primary", 503, "503 Service Unavailable", []byte(`{"error":"down"}`))
	primary := &fallbackClientStub{completeErr: primaryErr}
	secondary := &fallbackClientStub{completeText: "ok"}
	client := api.NewFallbackLLMClient(primary, secondary, "primary", "secondary")
	text, err := client.Complete(context.Background(), "", nil, api.CompleteOptions{})
	if err != nil || text != "ok" || secondary.completeCalls != 1 {
		t.Fatalf("complete fallback failed: text=%q error=%v calls=%d", text, err, secondary.completeCalls)
	}

	secondary.completeText = ""
	secondary.completeErr = errors.New("fallback transport failed")
	_, err = client.Complete(context.Background(), "", nil, api.CompleteOptions{})
	if err == nil || !strings.Contains(err.Error(), "primary model failed") || !strings.Contains(err.Error(), "fallback model failed") {
		t.Fatalf("combined error lost a provider failure: %v", err)
	}
}

func TestDefaultsFilesCoverAllSupportedConfigKeys(t *testing.T) {
	want := config.DefaultJSONKeys()
	for _, name := range []string{"defaults.json", "defaults.json.example"} {
		path := filepath.Join("..", ".Lumina", "CONFIG", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := json.Unmarshal(data, &values); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		got := make([]string, 0, len(values))
		for key := range values {
			got = append(got, key)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s is out of sync with config fields\nwant: %v\n got: %v", path, want, got)
		}
	}
}

func TestFallbackConfigLoadsAndReloadsFromUserDefaults(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	configDir := filepath.Join(home, ".lumina", "CONFIG")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "defaults.json")
	write := func(model string, enabled bool) {
		t.Helper()
		payload := map[string]any{
			"fallback_api_enabled":  enabled,
			"fallback_api_key":      "fallback-key",
			"fallback_api_base_url": "https://fallback.example/anthropic",
			"fallback_api_model":    model,
			"fallback_api_type":     "anthropic",
		}
		data, _ := json.Marshal(payload)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("fallback-one", true)
	t.Setenv("HOME", home)
	t.Setenv("LUMINA_RESOURCE_ROOT", "")
	t.Setenv("LUMINA_HOME", "")
	cfg := config.NewConfigForCWD(cwd)
	if !cfg.FallbackAPIEnabled || cfg.FallbackAPIModel != "fallback-one" || cfg.FallbackAPIKey != "fallback-key" {
		t.Fatalf("fallback defaults were not loaded: %#v", cfg)
	}
	write("fallback-two", true)
	reloaded := config.ReloadDynamicConfig(cfg)
	if reloaded.FallbackAPIModel != "fallback-two" || reloaded.FallbackAPIBaseURL != "https://fallback.example/anthropic" {
		t.Fatalf("fallback defaults were not hot reloaded: %#v", reloaded)
	}
}
