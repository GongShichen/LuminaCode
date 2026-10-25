package agent

import (
	"context"

	coretools "LuminaCode/tools"
)

// A session lease survives parent completion, but expires on Clear/Reset.
// It is captured before asynchronous preparation and never refreshed by a child.
type sessionLease struct {
	owner *SessionRuntime
	epoch uint64
	ctx   context.Context
}

func (s *SessionRuntime) lease() sessionLease {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return sessionLease{owner: s, epoch: s.epoch, ctx: s.lifetime}
}

func sessionLeaseFrom(extra coretools.ExecutionContext) sessionLease {
	if lease, ok := extra["_session_lease"].(sessionLease); ok {
		return lease
	}
	if session, ok := extra["_session_runtime"].(*SessionRuntime); ok {
		return session.lease()
	}
	return sessionLease{}
}

func (l sessionLease) checkLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.owner != nil && (l.epoch != l.owner.epoch || l.ctx.Err() != nil) {
		return ErrStaleState
	}
	return nil
}

func (l sessionLease) with(ctx context.Context, fn func() error) error {
	if l.owner != nil {
		l.owner.mu.RLock()
		defer l.owner.mu.RUnlock()
	}
	if err := l.checkLocked(ctx); err != nil {
		return err
	}
	return fn()
}

func (l sessionLease) register(ctx context.Context, id string, runtime *AgentRuntime) error {
	if l.owner == nil {
		return ctx.Err()
	}
	l.owner.mu.Lock()
	defer l.owner.mu.Unlock()
	if err := l.checkLocked(ctx); err != nil {
		return err
	}
	runtime.mu.Lock()
	if runtime.sessionContext != nil && runtime.sessionContext.Err() != nil {
		runtime.mu.Unlock()
		return ErrStaleState
	}
	runtime.sessionContext = l.ctx
	runtime.mu.Unlock()
	l.owner.agents[id] = runtime
	return nil
}

func (l sessionLease) executionContext(ctx context.Context) (context.Context, func()) {
	if l.owner == nil {
		return ctx, func() {}
	}
	runCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.ctx, cancel)
	if l.ctx.Err() != nil {
		cancel()
	}
	return runCtx, func() { stop(); cancel() }
}

type childAdmission struct {
	session sessionLease
	parent  AgentServices
}

func admissionFrom(extra coretools.ExecutionContext) childAdmission {
	if admission, ok := extra["_child_admission"].(childAdmission); ok {
		return admission
	}
	parent, _ := extra["agent_services"].(AgentServices)
	return childAdmission{session: sessionLeaseFrom(extra), parent: parent}
}

func (a childAdmission) with(ctx context.Context, fn func() error) error {
	return a.session.with(ctx, func() error {
		if a.parent.Runtime != nil {
			// This is the admission point relative to parent Abort/EndRun. Do not
			// retain the actor lock while task observers write their journal events.
			if err := a.parent.Runtime.withRun(RunToken{Generation: a.parent.Generation, ID: a.parent.RunID}, func() {}); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn()
	})
}
