package backend

import "sync"

type eventClient interface {
	write(any)
	close()
}

// EventHub owns the live WebSocket subscribers used by backend push events.
type EventHub struct {
	mu      sync.Mutex
	clients map[eventClient]struct{}
	closed  bool
}

func NewEventHub() *EventHub {
	return &EventHub{clients: map[eventClient]struct{}{}}
}

func (h *EventHub) Register(client eventClient) bool {
	if client == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.clients[client] = struct{}{}
	return true
}

func (h *EventHub) Unregister(client eventClient) int {
	h.mu.Lock()
	delete(h.clients, client)
	remaining := len(h.clients)
	h.mu.Unlock()
	return remaining
}

func (h *EventHub) CountExcluding(excluded eventClient) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for client := range h.clients {
		if client != excluded {
			count++
		}
	}
	return count
}

func (h *EventHub) Publish(event PushEvent) {
	h.mu.Lock()
	clients := make([]eventClient, 0, len(h.clients))
	for client := range h.clients {
		clients = append(clients, client)
	}
	h.mu.Unlock()
	for _, client := range clients {
		client.write(event)
	}
}

func (h *EventHub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	clients := make([]eventClient, 0, len(h.clients))
	for client := range h.clients {
		clients = append(clients, client)
	}
	clear(h.clients)
	h.mu.Unlock()
	for _, client := range clients {
		client.close()
	}
}
