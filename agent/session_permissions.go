package agent

import "context"

type sessionPermission struct {
	ctx   context.Context
	event StreamEvent
}

// PermissionContext exposes only the lifetime of the identified wait. A UI
// using an outer submit context must still wake when the owning run aborts.
func (s *SessionRuntime) PermissionContext(id string) (context.Context, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, runtime := range s.agents {
		runtime.mu.RLock()
		pending := runtime.run.Pending[id]
		if pending != nil {
			ctx := pending.ctx
			runtime.mu.RUnlock()
			return ctx, true
		}
		runtime.mu.RUnlock()
	}
	return nil, false
}

// SetPermissionHandler binds a session-lived UI transport. Background workers
// must never retain the output channel (or cancellation) of their parent's run.
func (s *SessionRuntime) SetPermissionHandler(handler func(context.Context, StreamEvent)) {
	s.permissionMu.Lock()
	s.permissionHandler = handler
	var queued []sessionPermission
	if handler != nil {
		queued, s.permissions = s.permissions, nil
	}
	s.permissionMu.Unlock()
	for _, pending := range queued {
		s.PublishPermission(pending.ctx, pending.event)
	}
}

func (s *SessionRuntime) permissionPending(event StreamEvent) bool {
	id := PermissionRequestID(event)
	if id == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, runtime := range s.agents {
		runtime.mu.RLock()
		pending := runtime.run.Pending[id] != nil && runtime.run.Phase.Active() && runtime.run.Phase != AgentPhaseCancelling && runtime.run.Context.Err() == nil
		runtime.mu.RUnlock()
		if pending {
			return true
		}
	}
	return false
}

// PublishPermission retains a pending request while no foreground run consumes
// events. The owning child's context and ticket fence both delivery and reply.
func (s *SessionRuntime) PublishPermission(ctx context.Context, event StreamEvent) bool {
	if ctx.Err() != nil || !s.permissionPending(event) {
		return false
	}
	s.permissionMu.Lock()
	if ctx.Err() != nil || !s.permissionPending(event) {
		s.permissionMu.Unlock()
		return false
	}
	handler := s.permissionHandler
	if handler == nil {
		// Prune expired requests when a new request arrives, including during idle.
		live := s.permissions[:0]
		for _, pending := range s.permissions {
			if pending.ctx.Err() == nil && s.permissionPending(pending.event) {
				live = append(live, pending)
			}
		}
		s.permissions = append(live, sessionPermission{ctx, event})
		select {
		case s.permissionReady <- struct{}{}:
		default:
		}
	}
	s.permissionMu.Unlock()
	if handler != nil {
		go func() {
			if ctx.Err() == nil && s.permissionPending(event) {
				handler(ctx, event)
			}
		}()
	}
	return true
}

// forwardPermissions is scoped to the consumer, not the publisher. Joining it
// before closing out prevents sends racing with parent completion or Reset.
func (s *SessionRuntime) forwardPermissions(ctx context.Context, out chan<- StreamEvent) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if ctx.Err() != nil {
				return
			}
			s.permissionMu.Lock()
			var pending sessionPermission
			if len(s.permissions) > 0 {
				pending = s.permissions[0]
				s.permissions = s.permissions[1:]
			}
			s.permissionMu.Unlock()
			if pending.ctx == nil {
				select {
				case <-ctx.Done():
					return
				case <-s.permissionReady:
					continue
				}
			}
			if pending.ctx.Err() != nil || !s.permissionPending(pending.event) {
				continue
			}
			select {
			case <-pending.ctx.Done():
				continue
			case out <- pending.event:
			case <-ctx.Done():
				s.PublishPermission(pending.ctx, pending.event)
				return
			}
		}
	}()
	return func() { cancel(); <-done }
}
