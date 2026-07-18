package api

import (
	"context"
	"errors"
	"testing"
)

func TestRetryableTransportErrorRecognizesWindowsConnectionReset(t *testing.T) {
	err := errors.New("wsarecv: An existing connection was forcibly closed by the remote host")
	if _, ok := retryableTransportError(context.Background(), err).(RetryableError); !ok {
		t.Fatal("Windows connection reset should be retryable")
	}
}
