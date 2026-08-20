package cluster

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestMemoryRuntimeImplementsLeaseRPCAndNotifications(t *testing.T) {
	runtime := NewMemoryRuntime()
	defer runtime.Close()
	lease, err := runtime.Acquire(context.Background(), "tenant", "session", "owner",
		time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Acquire(context.Background(), "tenant", "session", "other",
		time.Second, 100*time.Millisecond); err != ErrSessionOwned {
		t.Fatalf("second owner error=%v", err)
	}

	notifications, cancel, err := runtime.Subscribe(context.Background(), "tenant", "session")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if err := runtime.Publish(context.Background(), EventNotification{TenantID: "tenant",
		SessionID: "session", Payload: json.RawMessage(`{"ok":true}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-notifications:
	case <-time.After(time.Second):
		t.Fatal("notification was not delivered")
	}

	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	done := make(chan error, 1)
	go func() {
		commands, readErr := runtime.Read(ctx, "owner", "consumer", 1, time.Second)
		if readErr != nil || len(commands) != 1 {
			done <- readErr
			return
		}
		done <- runtime.Respond(ctx, commands[0].GatewayInstanceID,
			CommandResponse{RequestID: commands[0].RequestID, OK: true})
	}()
	response, err := runtime.Forward(ctx, CommandEnvelope{RequestID: "request", GatewayInstanceID: "gateway",
		OwnerInstanceID: "owner"})
	if err != nil || !response.OK {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}
