package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"time"
)

type RetryConfig struct {
	MaxRetries               int
	BaseDelay                time.Duration
	MaxDelay                 time.Duration
	BackoffFactor            float64
	Jitter                   bool
	RetryableStatuses        map[int]struct{}
	ConditionalRetryStatuses map[int]struct{}
	NonRetryableKeywords     map[string]struct{}
}

func DefaultRetryConfigPtr() *RetryConfig {
	return &RetryConfig{
		MaxRetries:    3,
		BaseDelay:     time.Second,
		MaxDelay:      60 * time.Second,
		BackoffFactor: 2.0,
		Jitter:        true,
		RetryableStatuses: map[int]struct{}{
			429: {},
		},
		ConditionalRetryStatuses: map[int]struct{}{
			503: {},
			529: {},
		},
		NonRetryableKeywords: map[string]struct{}{
			"model_not_found":       {},
			"invalid_request_error": {},
			"invalid_api_key":       {},
			"authentication":        {},
			"authorization":         {},
			"not_found":             {},
			"permission":            {},
		},
	}
}

func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:    3,
		BaseDelay:     time.Second,
		MaxDelay:      60 * time.Second,
		BackoffFactor: 2.0,
		Jitter:        true,
		RetryableStatuses: map[int]struct{}{
			429: {},
		},
		ConditionalRetryStatuses: map[int]struct{}{
			503: {},
			529: {},
		},
		NonRetryableKeywords: map[string]struct{}{
			"model_not_found":       {},
			"invalid_request_error": {},
			"invalid_api_key":       {},
			"authentication":        {},
			"authorization":         {},
			"not_found":             {},
			"permission":            {},
		},
	}
}

type RequestParts struct {
	URL     string
	Headers map[string]string
	Body    []byte
}

type EventResult struct {
	Event map[string]any
	Err   error
}

type BuildRequestFunc func() (RequestParts, error)

type StreamFunc func(
	ctx context.Context,
	url string,
	headers map[string]string,
	body []byte,
) <-chan EventResult

type RetryableError struct {
	Message string
}

func (e RetryableError) Error() string {
	return e.Message
}

func NewRetryableError(message string) error {
	return RetryableError{Message: message}
}

type APIStatusError struct {
	Provider   string
	StatusCode int
	Status     string
	Body       string
}

func (e APIStatusError) Error() string {
	prefix := "API error"
	if e.Provider != "" {
		prefix = e.Provider + " API error"
	}
	status := e.Status
	if status == "" && e.StatusCode > 0 {
		status = http.StatusText(e.StatusCode)
	}
	if status != "" {
		return fmt.Sprintf("%s %d %s: %s", prefix, e.StatusCode, status, e.Body)
	}
	return fmt.Sprintf("%s %d: %s", prefix, e.StatusCode, e.Body)
}

func NewAPIStatusError(provider string, statusCode int, status string, body []byte) APIStatusError {
	return APIStatusError{
		Provider:   provider,
		StatusCode: statusCode,
		Status:     status,
		Body:       string(body),
	}
}

type RetryableStatusError struct {
	APIStatusError
}

func (e RetryableStatusError) Error() string {
	return e.APIStatusError.Error()
}

func RetryWithBackoff(
	ctx context.Context,
	streamFn StreamFunc,
	buildRequest BuildRequestFunc,
	config RetryConfig,
) <-chan EventResult {
	out := make(chan EventResult)

	go func() {
		defer close(out)

		var lastErr error
		var lastError string

		for attempt := 0; attempt <= config.MaxRetries; attempt++ {
			req, err := buildRequest()
			if err != nil {
				lastErr = err
				lastError = err.Error()

				if !isRetryableError(err) {
					sendEvent(ctx, out, errorEvent(err.Error()))
					return
				}

				if attempt < config.MaxRetries {
					if !sleepBackoff(ctx, attempt, config) {
						sendErr(ctx, out, ctx.Err())
						return
					}
					continue
				}

				break
			}

			stream := streamFn(ctx, req.URL, req.Headers, req.Body)

			shouldRetry := false

			for item := range stream {
				if item.Err != nil {
					lastErr = item.Err
					lastError = item.Err.Error()

					if isRetryableError(item.Err) {
						shouldRetry = true
						break
					}

					sendEvent(ctx, out, errorEvent(item.Err.Error()))
					return
				}

				if item.Event["type"] == "error" {
					sendEvent(ctx, out, item.Event)
					return
				}

				if !sendEvent(ctx, out, item.Event) {
					return
				}
			}

			if !shouldRetry {
				return
			}

			if attempt < config.MaxRetries {
				if !sleepBackoff(ctx, attempt, config) {
					sendErr(ctx, out, ctx.Err())
					return
				}
				continue
			}
		}

		sendEvent(ctx, out, map[string]any{
			"type": "error",
			"message": fmt.Sprintf(
				"Request failed after %d attempts. Last error: %s",
				config.MaxRetries+1,
				lastError,
			),
			"status_code": statusCodeFromError(lastErr),
			"raw_error":   rawBodyFromError(lastErr),
		})
	}()

	return out
}

func statusCodeFromError(err error) any {
	if err == nil {
		return nil
	}
	var retryableStatus RetryableStatusError
	if errors.As(err, &retryableStatus) {
		return retryableStatus.StatusCode
	}
	var statusErr APIStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode
	}
	return nil
}

func rawBodyFromError(err error) any {
	if err == nil {
		return nil
	}
	var retryableStatus RetryableStatusError
	if errors.As(err, &retryableStatus) {
		return retryableStatus.Body
	}
	var statusErr APIStatusError
	if errors.As(err, &statusErr) {
		return statusErr.Body
	}
	return nil
}

func sleepBackoff(ctx context.Context, attempt int, config RetryConfig) bool {
	delay := float64(config.BaseDelay) * math.Pow(config.BackoffFactor, float64(attempt))

	duration := time.Duration(delay)
	if duration > config.MaxDelay {
		duration = config.MaxDelay
	}

	if config.Jitter {
		duration = time.Duration(float64(duration) * (0.5 + rand.Float64()))
	}

	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func errorEvent(message string) map[string]any {
	return map[string]any{
		"type":    "error",
		"message": message,
	}
}

func sendEvent(ctx context.Context, out chan<- EventResult, event map[string]any) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- EventResult{Event: event}:
		return true
	}
}

func sendErr(ctx context.Context, out chan<- EventResult, err error) bool {
	if err == nil {
		return true
	}

	select {
	case <-ctx.Done():
		return false
	case out <- EventResult{Err: err}:
		return true
	}
}

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	var retryable RetryableError
	if errors.As(err, &retryable) {
		return true
	}
	var retryableStatus RetryableStatusError
	return errors.As(err, &retryableStatus)
}
