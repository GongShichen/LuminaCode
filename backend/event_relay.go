package backend

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"LuminaCode/cluster"
	"LuminaCode/config"
	"LuminaCode/session"
)

type EventRelay struct {
	cfg     config.Config
	hub     *EventHub
	runtime *cluster.RedisRuntime
	queue   chan PushEvent
	done    chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
}

func NewEventRelay(cfg config.Config, hub *EventHub, runtime *cluster.RedisRuntime) (*EventRelay, func()) {
	relay := &EventRelay{cfg: cfg, hub: hub, runtime: runtime, queue: make(chan PushEvent, 1024), done: make(chan struct{})}
	if cfg.UsesClusterRuntime() && runtime != nil {
		relay.wg.Add(1)
		go relay.run()
	}
	return relay, relay.Close
}

func (r *EventRelay) Publish(event PushEvent) {
	if event.TenantID == "" {
		event.TenantID = session.LocalTenantID
	}
	r.hub.Publish(event)
	if !r.cfg.UsesClusterRuntime() || r.runtime == nil || event.SessionID == "" {
		return
	}
	select {
	case r.queue <- event:
	default:
		// Durable events remain recoverable from PostgreSQL by sequence. A full
		// live queue must never block the owner runtime or turn Redis into a fact
		// source.
		slog.Warn("cluster live event queue full", "session_id", event.SessionID)
	}
}

func (r *EventRelay) run() {
	defer r.wg.Done()
	for {
		select {
		case <-r.done:
			return
		case event := <-r.queue:
			payload, err := json.Marshal(event)
			if err != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err = r.runtime.Publish(ctx, cluster.EventNotification{TenantID: event.TenantID,
				SessionID: event.SessionID, OriginInstance: r.cfg.InstanceID, EventID: event.EventID,
				Seq: event.Seq, Payload: payload})
			cancel()
			if err != nil {
				slog.Warn("publish cluster live event", "session_id", event.SessionID, "error", err)
			}
		}
	}
}

func (r *EventRelay) Close() {
	r.once.Do(func() {
		close(r.done)
		r.wg.Wait()
	})
}
