package session

import (
	"context"
	"errors"

	"LuminaCode/harness"
)

var ErrSessionFenceLost = errors.New("session fencing token is stale")

// RuntimeStore is the durable event-sourced session boundary shared by local
// SQLite and clustered PostgreSQL implementations.
type RuntimeStore interface {
	harness.EventStore
	harness.BlobStore
	harness.ConsumerStore
	SessionID() string
	StreamHead(context.Context, string) (int64, error)
	IntegrityCheck(context.Context) error
	WithProjectionLock(context.Context, func() error) error
}
