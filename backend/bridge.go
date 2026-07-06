package backend

import (
	"sync"
	"time"

	luminacli "LuminaCode/cli"
	luminaui "LuminaCode/ui"

	"github.com/google/uuid"
)

type permissionWaiter struct {
	sessionID string
	ch        chan string
}

type WSRendererBridge struct {
	sessionID string
	emit      func(PushEvent)
	nextSeq   func() int64

	mu          sync.Mutex
	permissions map[string]permissionWaiter
	selections  map[string]chan *string
}

func NewWSRendererBridge(sessionID string, emit func(PushEvent), nextSeq func() int64) *WSRendererBridge {
	return &WSRendererBridge{
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
	b.emitEvent("permission_requested", map[string]any{
		"request_id": requestID,
		"prompt":     prompt,
		"dangerous":  dangerous,
		"actions":    luminacli.Phase1PermissionActionLabels,
	})
	select {
	case decision := <-ch:
		return luminacli.NormalizePermissionAnswer(decision)
	case <-time.After(24 * time.Hour):
		return "deny"
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
		Type:      "event",
		SessionID: b.sessionID,
		Seq:       b.nextSeq(),
		Event: map[string]any{
			"type":    eventType,
			"payload": payload,
		},
	})
}
