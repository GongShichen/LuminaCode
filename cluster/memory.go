package cluster

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemoryRuntime is a deterministic test implementation of the coordinator,
// command bus, and notifier contracts. It is intentionally process-local and
// must never be selected by production wiring.
type MemoryRuntime struct {
	mu        sync.Mutex
	closed    bool
	instances map[string]memoryInstance
	owners    map[string]memoryOwner
	fences    map[string]int64
	commands  map[string]chan CommandEnvelope
	responses map[string]chan CommandResponse
	subs      map[string]map[chan EventNotification]struct{}
}

type memoryInstance struct {
	info      InstanceInfo
	expiresAt time.Time
}

type memoryOwner struct {
	owner     Owner
	expiresAt time.Time
}

func NewMemoryRuntime() *MemoryRuntime {
	return &MemoryRuntime{instances: map[string]memoryInstance{}, owners: map[string]memoryOwner{},
		fences: map[string]int64{}, commands: map[string]chan CommandEnvelope{},
		responses: map[string]chan CommandResponse{}, subs: map[string]map[chan EventNotification]struct{}{}}
}

func (m *MemoryRuntime) RegisterInstance(_ context.Context, info InstanceInfo, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("memory cluster runtime is closed")
	}
	m.instances[info.InstanceID] = memoryInstance{info: info, expiresAt: time.Now().Add(ttl)}
	return nil
}

func (m *MemoryRuntime) UnregisterInstance(_ context.Context, instanceID string) error {
	m.mu.Lock()
	delete(m.instances, instanceID)
	m.mu.Unlock()
	return nil
}

func (m *MemoryRuntime) Lookup(_ context.Context, tenantID, sessionID string) (*Owner, error) {
	key := memorySessionKey(tenantID, sessionID)
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.owners[key]
	if !ok || time.Now().After(entry.expiresAt) {
		delete(m.owners, key)
		return nil, nil
	}
	owner := entry.owner
	return &owner, nil
}

func (m *MemoryRuntime) Acquire(_ context.Context, tenantID, sessionID, instanceID string,
	ttl, renewEvery time.Duration) (SessionLease, error) {
	if ttl <= 0 || renewEvery <= 0 || renewEvery >= ttl {
		return nil, errors.New("invalid lease timing")
	}
	key := memorySessionKey(tenantID, sessionID)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("memory cluster runtime is closed")
	}
	if current, ok := m.owners[key]; ok && time.Now().Before(current.expiresAt) {
		m.mu.Unlock()
		return nil, ErrSessionOwned
	}
	m.fences[key]++
	owner := Owner{InstanceID: instanceID, LeaseID: uuid.NewString(), FenceToken: m.fences[key]}
	m.owners[key] = memoryOwner{owner: owner, expiresAt: time.Now().Add(ttl)}
	m.mu.Unlock()
	lease := &memoryLease{runtime: m, key: key, owner: owner, ttl: ttl, renewEvery: renewEvery,
		lost: make(chan struct{}), stop: make(chan struct{})}
	go lease.renew()
	return lease, nil
}

type memoryLease struct {
	runtime     *MemoryRuntime
	key         string
	owner       Owner
	ttl         time.Duration
	renewEvery  time.Duration
	lost        chan struct{}
	stop        chan struct{}
	lostOnce    sync.Once
	releaseOnce sync.Once
}

func (l *memoryLease) Owner() Owner          { return l.owner }
func (l *memoryLease) Lost() <-chan struct{} { return l.lost }

func (l *memoryLease) renew() {
	ticker := time.NewTicker(l.renewEvery)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			l.runtime.mu.Lock()
			entry, ok := l.runtime.owners[l.key]
			if !ok || entry.owner.LeaseID != l.owner.LeaseID || l.runtime.closed {
				l.runtime.mu.Unlock()
				l.markLost()
				return
			}
			entry.expiresAt = time.Now().Add(l.ttl)
			l.runtime.owners[l.key] = entry
			l.runtime.mu.Unlock()
		}
	}
}

func (l *memoryLease) Release(context.Context) error {
	l.releaseOnce.Do(func() {
		close(l.stop)
		l.runtime.mu.Lock()
		if entry, ok := l.runtime.owners[l.key]; ok && entry.owner.LeaseID == l.owner.LeaseID {
			delete(l.runtime.owners, l.key)
		}
		l.runtime.mu.Unlock()
		l.markLost()
	})
	return nil
}

func (l *memoryLease) markLost() { l.lostOnce.Do(func() { close(l.lost) }) }

func (m *MemoryRuntime) Forward(ctx context.Context, command CommandEnvelope) (CommandResponse, error) {
	m.mu.Lock()
	commandCh := m.commands[command.OwnerInstanceID]
	if commandCh == nil {
		commandCh = make(chan CommandEnvelope, 128)
		m.commands[command.OwnerInstanceID] = commandCh
	}
	responseCh := make(chan CommandResponse, 1)
	responseKey := command.GatewayInstanceID + "\x1f" + command.RequestID
	m.responses[responseKey] = responseCh
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.responses, responseKey)
		m.mu.Unlock()
	}()
	select {
	case commandCh <- command:
	case <-ctx.Done():
		return CommandResponse{}, ctx.Err()
	}
	select {
	case response := <-responseCh:
		return response, nil
	case <-ctx.Done():
		return CommandResponse{}, ctx.Err()
	}
}

func (m *MemoryRuntime) Read(ctx context.Context, instanceID, _ string, count int64,
	block time.Duration) ([]CommandEnvelope, error) {
	m.mu.Lock()
	commandCh := m.commands[instanceID]
	if commandCh == nil {
		commandCh = make(chan CommandEnvelope, 128)
		m.commands[instanceID] = commandCh
	}
	m.mu.Unlock()
	if count <= 0 {
		count = 1
	}
	timer := time.NewTimer(block)
	defer timer.Stop()
	var result []CommandEnvelope
	select {
	case command := <-commandCh:
		command.StreamID = uuid.NewString()
		result = append(result, command)
	case <-timer.C:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	for int64(len(result)) < count {
		select {
		case command := <-commandCh:
			command.StreamID = uuid.NewString()
			result = append(result, command)
		default:
			return result, nil
		}
	}
	return result, nil
}

func (m *MemoryRuntime) Respond(_ context.Context, gatewayInstanceID string, response CommandResponse) error {
	key := gatewayInstanceID + "\x1f" + response.RequestID
	m.mu.Lock()
	responseCh := m.responses[key]
	delete(m.responses, key)
	m.mu.Unlock()
	if responseCh == nil {
		return errors.New("cluster response waiter not found")
	}
	responseCh <- response
	return nil
}

func (*MemoryRuntime) Ack(context.Context, string, string) error { return nil }

func (m *MemoryRuntime) Publish(_ context.Context, notification EventNotification) error {
	key := memorySessionKey(notification.TenantID, notification.SessionID)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("memory cluster runtime is closed")
	}
	for subscriber := range m.subs[key] {
		select {
		case subscriber <- notification:
		default:
		}
	}
	m.mu.Unlock()
	return nil
}

func (m *MemoryRuntime) Subscribe(_ context.Context, tenantID, sessionID string) (<-chan EventNotification,
	func(), error) {
	key := memorySessionKey(tenantID, sessionID)
	channel := make(chan EventNotification, 32)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, nil, errors.New("memory cluster runtime is closed")
	}
	if m.subs[key] == nil {
		m.subs[key] = map[chan EventNotification]struct{}{}
	}
	m.subs[key][channel] = struct{}{}
	m.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			m.mu.Lock()
			delete(m.subs[key], channel)
			m.mu.Unlock()
			close(channel)
		})
	}
	return channel, cancel, nil
}

func (m *MemoryRuntime) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.subs = map[string]map[chan EventNotification]struct{}{}
	m.mu.Unlock()
	return nil
}

func memorySessionKey(tenantID, sessionID string) string { return tenantID + "\x1f" + sessionID }

var _ SessionCoordinator = (*MemoryRuntime)(nil)
var _ ClusterCommandBus = (*MemoryRuntime)(nil)
var _ EventNotifier = (*MemoryRuntime)(nil)
