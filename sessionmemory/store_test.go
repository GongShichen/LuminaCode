package sessionmemory

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"LuminaCode/config"
	"LuminaCode/harness"
)

func TestIngestEventsUsesSourceEventIDForIdempotency(t *testing.T) {
	ctx := context.Background()
	cfg := config.NewConfig()
	cfg.SessionDir = t.TempDir()
	cfg.SessionMemoryDir = cfg.SessionDir
	cfg.SessionMemoryEnabled = true
	store, err := Open(ctx, cfg, "session-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	message, _ := json.Marshal(map[string]any{"role": "user", "content": "hello"})
	payload, _ := json.Marshal(harness.MessageAppendedPayload{Message: message, UserTurn: 1})
	event := harness.Event{Seq: 1, ID: "event-1", Type: harness.EventMessageUserAppended, OccurredAt: time.Now(), Payload: payload}
	if err := store.IngestEvents(ctx, []harness.Event{event, event}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE source_event_id = ?`, event.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("event was ingested %d times", count)
	}
}
