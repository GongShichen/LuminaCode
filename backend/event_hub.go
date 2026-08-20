package backend

import "sync"

type eventClient interface {
	write(any)
	close()
}

type scopedEventClient interface{ accepts(PushEvent) bool }

// EventHub owns the live WebSocket subscribers used by backend push events.
type EventHub struct {
	mu       sync.Mutex
	clients  map[eventClient]struct{}
	closed   bool
	seen     map[string]struct{}
	seenFIFO []string
}

func NewEventHub() *EventHub {
	return &EventHub{clients: map[eventClient]struct{}{}, seen: map[string]struct{}{}}
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
	if event.EventID != "" {
		key := event.TenantID + "\x00" + event.SessionID + "\x00" + event.EventID
		if _, duplicate := h.seen[key]; duplicate {
			h.mu.Unlock()
			return
		}
		h.seen[key] = struct{}{}
		h.seenFIFO = append(h.seenFIFO, key)
		if len(h.seenFIFO) > 4096 {
			delete(h.seen, h.seenFIFO[0])
			h.seenFIFO = h.seenFIFO[1:]
		}
	}
	clients := make([]eventClient, 0, len(h.clients))
	for client := range h.clients {
		if scoped, ok := client.(scopedEventClient); ok && !scoped.accepts(event) {
			continue
		}
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
