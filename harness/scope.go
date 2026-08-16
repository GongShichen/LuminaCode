package harness

import (
	"fmt"
	"sort"
	"sync"
)

type ScopeKind string

const (
	ScopeGlobal  ScopeKind = "global"
	ScopeSession ScopeKind = "session"
	ScopeTeam    ScopeKind = "team"
	ScopeAgent   ScopeKind = "agent"
	ScopeSkill   ScopeKind = "skill"
)

type CapabilityKey string

type capabilityEntry struct {
	id       uint64
	priority int
	provider any
}

type ScopeDescription struct {
	ID           string          `json:"id"`
	Kind         ScopeKind       `json:"kind"`
	ParentID     string          `json:"parent_id,omitempty"`
	Capabilities []CapabilityKey `json:"capabilities"`
}

type ScopeTree struct {
	ScopeDescription
	Children []ScopeTree `json:"children,omitempty"`
}

type RuntimeScope struct {
	mu        sync.RWMutex
	id        string
	kind      ScopeKind
	parent    *RuntimeScope
	nextID    uint64
	closed    bool
	providers map[CapabilityKey][]capabilityEntry
	children  map[string]*RuntimeScope
	disposers []Dispose
}

func NewScope(id string, kind ScopeKind, parent *RuntimeScope) *RuntimeScope {
	return &RuntimeScope{
		id: id, kind: kind, parent: parent,
		providers: map[CapabilityKey][]capabilityEntry{},
		children:  map[string]*RuntimeScope{},
	}
}

func (s *RuntimeScope) ID() string { return s.id }

func (s *RuntimeScope) Child(id string, kind ScopeKind) (*RuntimeScope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("scope %q is closed", s.id)
	}
	if _, exists := s.children[id]; exists {
		return nil, fmt.Errorf("child scope %q already exists", id)
	}
	child := NewScope(id, kind, s)
	s.children[id] = child
	return child, nil
}

func (s *RuntimeScope) Provide(key CapabilityKey, provider any, priority int) (Dispose, error) {
	if key == "" || provider == nil {
		return nil, fmt.Errorf("capability key and provider are required")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("scope %q is closed", s.id)
	}
	for _, entry := range s.providers[key] {
		if entry.priority == priority {
			s.mu.Unlock()
			return nil, fmt.Errorf("capability %q already has priority %d in scope %q", key, priority, s.id)
		}
	}
	s.nextID++
	id := s.nextID
	s.providers[key] = append(s.providers[key], capabilityEntry{id: id, priority: priority, provider: provider})
	sort.SliceStable(s.providers[key], func(i, j int) bool {
		return s.providers[key][i].priority > s.providers[key][j].priority
	})
	var once sync.Once
	dispose := func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			entries := s.providers[key]
			for i := range entries {
				if entries[i].id == id {
					entries = append(entries[:i], entries[i+1:]...)
					break
				}
			}
			if len(entries) == 0 {
				delete(s.providers, key)
			} else {
				s.providers[key] = entries
			}
		})
	}
	s.disposers = append(s.disposers, dispose)
	s.mu.Unlock()
	return dispose, nil
}

func (s *RuntimeScope) Resolve(key CapabilityKey) (any, bool) {
	for current := s; current != nil; current = current.parent {
		current.mu.RLock()
		entries := current.providers[key]
		if len(entries) > 0 {
			provider := entries[0].provider
			current.mu.RUnlock()
			return provider, true
		}
		current.mu.RUnlock()
	}
	return nil, false
}

func (s *RuntimeScope) Describe() ScopeDescription {
	s.mu.RLock()
	defer s.mu.RUnlock()
	description := ScopeDescription{ID: s.id, Kind: s.kind, Capabilities: make([]CapabilityKey, 0, len(s.providers))}
	if s.parent != nil {
		description.ParentID = s.parent.id
	}
	for key := range s.providers {
		description.Capabilities = append(description.Capabilities, key)
	}
	sort.Slice(description.Capabilities, func(i, j int) bool { return description.Capabilities[i] < description.Capabilities[j] })
	return description
}

func (s *RuntimeScope) DescribeTree() ScopeTree {
	description := s.Describe()
	s.mu.RLock()
	children := make([]*RuntimeScope, 0, len(s.children))
	for _, child := range s.children {
		children = append(children, child)
	}
	s.mu.RUnlock()
	sort.Slice(children, func(i, j int) bool { return children[i].id < children[j].id })
	tree := ScopeTree{ScopeDescription: description}
	for _, child := range children {
		tree.Children = append(tree.Children, child.DescribeTree())
	}
	return tree
}

func (s *RuntimeScope) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	children := make([]*RuntimeScope, 0, len(s.children))
	for _, child := range s.children {
		children = append(children, child)
	}
	disposers := append([]Dispose(nil), s.disposers...)
	s.mu.Unlock()
	for _, child := range children {
		_ = child.Close()
	}
	for i := len(disposers) - 1; i >= 0; i-- {
		disposers[i]()
	}
	if s.parent != nil {
		s.parent.mu.Lock()
		delete(s.parent.children, s.id)
		s.parent.mu.Unlock()
	}
	return nil
}
