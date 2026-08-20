package backend

import (
	"encoding/json"
	"sync"
	"time"

	luminacli "LuminaCode/cli"
	"LuminaCode/harness"
	luminaui "LuminaCode/ui"

	"github.com/google/uuid"
)

type permissionWaiter struct {
	sessionID string
	ch        chan string
}

type WSRendererBridge struct {
	tenantID  string
	sessionID string
	emit      EventEmitter
	nextSeq   func() int64

	mu          sync.Mutex
	permissions map[string]permissionWaiter
	selections  map[string]chan *string
}

func NewWSRendererBridge(tenantID, sessionID string, emit EventEmitter, nextSeq func() int64) *WSRendererBridge {
	return &WSRendererBridge{
		tenantID:    tenantID,
		sessionID:   sessionID,
		emit:        emit,
		nextSeq:     nextSeq,
		permissions: map[string]permissionWaiter{},
		selections:  map[string]chan *string{},
	}
}

func (b *WSRendererBridge) Mount(frame luminaui.RenderFrame)    { b.emitFrame("frame.snapshot", frame) }
func (b *WSRendererBridge) Update(frame luminaui.RenderFrame)   { b.emitFrame("frame.snapshot", frame) }
func (b *WSRendererBridge) Shutdown(frame luminaui.RenderFrame) { b.emitFrame("frame.shutdown", frame) }

func (b *WSRendererBridge) ShowModal(state map[string]any) {
	b.emitEvent("modal.show", state)
}

func (b *WSRendererBridge) ClearModal() {
	b.emitEvent("modal.clear", map[string]any{})
}

func (b *WSRendererBridge) AskPermission(prompt any, dangerous bool) string {
	requestID := uuid.NewString()
	ch := make(chan string, 1)
	b.mu.Lock()
	b.permissions[requestID] = permissionWaiter{sessionID: b.sessionID, ch: ch}
	b.mu.Unlock()
	actions := luminacli.Phase1PermissionActionLabels
	if name, _ := permissionPromptNameAndInput(prompt); name == "wsl-sandbox-setup" {
		actions = []string{"Install sandbox", "Run locally"}
	}
	b.emitEvent("permission_requested", map[string]any{
		"request_id": requestID,
		"prompt":     prompt,
		"dangerous":  dangerous,
		"actions":    actions,
	})
	select {
	case decision := <-ch:
		return luminacli.NormalizePermissionAnswer(decision)
	case <-time.After(24 * time.Hour):
		return "deny"
	}
}

func permissionPromptNameAndInput(prompt any) (string, map[string]any) {
	switch value := prompt.(type) {
	case map[string]any:
		name, _ := value["name"].(string)
		input, _ := value["input"].(map[string]any)
		if input == nil {
			input = map[string]any{}
		}
		return name, input
	default:
		return "", nil
	}
}

func (b *WSRendererBridge) ResolvePermission(requestID, decision string) bool {
	b.mu.Lock()
	waiter, ok := b.permissions[requestID]
	if ok {
		delete(b.permissions, requestID)
	}
	b.mu.Unlock()
	if !ok || waiter.sessionID != b.sessionID {
		return false
	}
	waiter.ch <- luminacli.NormalizePermissionAnswer(decision)
	return true
}

func (b *WSRendererBridge) PickFromList(title string, options [][2]string) *string {
	requestID := uuid.NewString()
	ch := make(chan *string, 1)
	b.mu.Lock()
	b.selections[requestID] = ch
	b.mu.Unlock()
	b.emitEvent("selection_requested", map[string]any{
		"request_id": requestID,
		"title":      title,
		"options":    options,
	})
	select {
	case choice := <-ch:
		return choice
	case <-time.After(24 * time.Hour):
		return nil
	}
}

func (b *WSRendererBridge) ResolveSelection(requestID string, choice *string) bool {
	b.mu.Lock()
	ch, ok := b.selections[requestID]
	if ok {
		delete(b.selections, requestID)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	ch <- choice
	return true
}

func (b *WSRendererBridge) emitFrame(eventType string, frame luminaui.RenderFrame) {
	b.emitEvent(eventType, frame)
}

func (b *WSRendererBridge) emitEvent(eventType string, payload any) {
	if b == nil || b.emit == nil {
		return
	}
	b.emit(PushEvent{
		TenantID:  b.tenantID,
		Type:      "event",
		SessionID: b.sessionID,
		Seq:       b.nextSeq(),
		Event: map[string]any{
			"type":    eventType,
			"payload": payload,
		},
	})
}

func (b *WSRendererBridge) emitRuntimeEvents(events []harness.Event) {
	if b == nil || b.emit == nil {
		return
	}
	for _, event := range events {
		b.emit(PushEvent{
			TenantID: b.tenantID, Type: "event", ProtocolVersion: 3, SessionID: b.sessionID, StreamID: event.StreamID,
			Seq: event.Seq, EventID: event.ID, EventType: event.Type, SchemaVersion: event.SchemaVersion,
			Durable: true, Timestamp: event.OccurredAt.UTC().Format(time.RFC3339Nano), Payload: json.RawMessage(event.Payload),
			Event: map[string]any{"type": "runtime.event", "payload": event},
		})
	}
}
