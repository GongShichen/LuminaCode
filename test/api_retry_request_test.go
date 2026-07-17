package test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"LuminaCode/api"
)

func TestRetryRequestKeepsAttemptContextAliveWhileReadingBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`{"value":"complete"}`))
	}))
	defer server.Close()

	client := api.LLMClientBase{
		ProviderName: "test",
		RetryConfig:  api.DefaultRetryConfig(),
		HTTPClient:   server.Client(),
	}
	data, err := client.RetryRequest(context.Background(), func(ctx context.Context, httpClient *http.Client) (*http.Response, error) {
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		if requestErr != nil {
			return nil, requestErr
		}
		return httpClient.Do(request)
	}, time.Second)
	if err != nil {
		t.Fatalf("read delayed response body: %v", err)
	}
	if data["value"] != "complete" {
		t.Fatalf("unexpected response: %#v", data)
	}
}
