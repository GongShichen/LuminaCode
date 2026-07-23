package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRetryableTransportErrorRecognizesWindowsConnectionReset(t *testing.T) {
	err := errors.New("wsarecv: An existing connection was forcibly closed by the remote host")
	if _, ok := retryableTransportError(context.Background(), err).(RetryableError); !ok {
		t.Fatal("Windows connection reset should be retryable")
	}
}

func TestProviderCooldownEscalatesAndRecovers(t *testing.T) {
	resetProviderCooldownForTest()
	defer resetProviderCooldownForTest()

	key := providerCooldownKey("https://token-plan-cn.xiaomimimo.com/anthropic/messages")
	if key != "token-plan-cn.xiaomimimo.com" {
		t.Fatalf("unexpected cooldown key: %q", key)
	}

	noteProviderRateLimit(key)
	providerCooldown.Lock()
	first := providerCooldown.entries[key]
	providerCooldown.Unlock()
	if first.Level != 1 {
		t.Fatalf("first 429 should set level 1, got %d", first.Level)
	}
	if wait := time.Until(first.Until); wait <= 0 || wait > 11*time.Second {
		t.Fatalf("first cooldown outside 10s window: %v", wait)
	}

	noteProviderRateLimit(key)
	providerCooldown.Lock()
	second := providerCooldown.entries[key]
	providerCooldown.Unlock()
	if second.Level != 2 {
		t.Fatalf("second 429 should set level 2, got %d", second.Level)
	}
	if wait := time.Until(second.Until); wait < 25*time.Second || wait > 31*time.Second {
		t.Fatalf("second cooldown outside 30s window: %v", wait)
	}

	noteProviderRateLimit(key)
	noteProviderRateLimit(key)
	providerCooldown.Lock()
	capped := providerCooldown.entries[key]
	providerCooldown.Unlock()
	if capped.Level != 3 {
		t.Fatalf("cooldown should cap at level 3, got %d", capped.Level)
	}
	if wait := time.Until(capped.Until); wait < 55*time.Second || wait > 61*time.Second {
		t.Fatalf("capped cooldown outside 60s window: %v", wait)
	}

	noteProviderSuccess(key)
	providerCooldown.Lock()
	recovered := providerCooldown.entries[key]
	providerCooldown.Unlock()
	if recovered.Level != 2 || !recovered.Until.IsZero() {
		t.Fatalf("success should step cooldown down without active wait: %#v", recovered)
	}
}

func TestRetryWithBackoffTreatsQuotaExhaustedAsFatal(t *testing.T) {
	resetProviderCooldownForTest()
	defer resetProviderCooldownForTest()

	calls := 0
	streamFn := func(context.Context, string, map[string]string, []byte) <-chan EventResult {
		calls++
		out := make(chan EventResult, 1)
		out <- EventResult{Err: RetryableStatusError{APIStatusError: NewAPIStatusError(
			"mimo", 429, "429 Too Many Requests", []byte(`{"message":"quota exhausted","type":"limitation"}`),
		)}}
		close(out)
		return out
	}
	build := func() (RequestParts, error) {
		return RequestParts{URL: "https://token-plan-cn.xiaomimimo.com/anthropic/messages"}, nil
	}
	var events []map[string]any
	for item := range RetryWithBackoff(context.Background(), streamFn, build, DefaultRetryConfig()) {
		if item.Err != nil {
			t.Fatal(item.Err)
		}
		events = append(events, item.Event)
	}
	if calls != 1 {
		t.Fatalf("quota exhaustion should not retry, calls=%d", calls)
	}
	if len(events) != 1 || events[0]["error_type"] != "api_quota_exhausted" {
		t.Fatalf("expected quota exhausted event, got %#v", events)
	}

	for item := range RetryWithBackoff(context.Background(), streamFn, build, DefaultRetryConfig()) {
		if item.Err != nil {
			t.Fatal(item.Err)
		}
		events = append(events, item.Event)
	}
	if calls != 1 {
		t.Fatalf("provider fuse should fail fast without another request, calls=%d", calls)
	}
	if !strings.Contains(strings.ToLower(events[len(events)-1]["message"].(string)), "quota exhausted") {
		t.Fatalf("fuse should preserve quota reason: %#v", events[len(events)-1])
	}
}

func TestHTTP402AlwaysTripsQuotaFuse(t *testing.T) {
	err := NewAPIStatusError("provider", 402, "402", []byte(`{"message":"account unavailable"}`))
	if !IsQuotaExhaustedError(err) {
		t.Fatal("HTTP 402 was not classified as a hard quota failure")
	}
}
