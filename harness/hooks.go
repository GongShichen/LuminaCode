package harness

import (
	"context"
	"sort"
	"sync"
)

type Dispose func()

type HookResult[T any] struct {
	Value  T
	Events []PendingEvent
}

type Hook[T any] func(context.Context, T) (HookResult[T], error)

type hookEntry[T any] struct {
	id       uint64
	priority int
	order    uint64
	hook     Hook[T]
}

// HookPoint is a deterministic waterfall. It is intentionally generic so
// lifecycle payloads remain typed instead of devolving into map[string]any.
type HookPoint[T any] struct {
	mu      sync.RWMutex
	nextID  uint64
	entries []hookEntry[T]
}

func (p *HookPoint[T]) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.entries)
}

func (p *HookPoint[T]) Register(priority int, hook Hook[T]) Dispose {
	if hook == nil {
		return func() {}
	}
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	p.entries = append(p.entries, hookEntry[T]{id: id, priority: priority, order: id, hook: hook})
	sort.SliceStable(p.entries, func(i, j int) bool {
		if p.entries[i].priority != p.entries[j].priority {
			return p.entries[i].priority < p.entries[j].priority
		}
		return p.entries[i].order < p.entries[j].order
	})
	p.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			for i := range p.entries {
				if p.entries[i].id == id {
					p.entries = append(p.entries[:i], p.entries[i+1:]...)
					return
				}
			}
		})
	}
}

func (p *HookPoint[T]) Run(ctx context.Context, input T) (HookResult[T], error) {
	p.mu.RLock()
	entries := append([]hookEntry[T](nil), p.entries...)
	p.mu.RUnlock()
	result := HookResult[T]{Value: input}
	for _, entry := range entries {
		next, err := entry.hook(ctx, result.Value)
		if err != nil {
			return result, err
		}
		result.Value = next.Value
		result.Events = append(result.Events, next.Events...)
	}
	return result, nil
}
