package agentContext

import (
	"LuminaCode/api"
	"strings"
)

type CacheState struct {
	Pending          []api.CacheEdit
	Pinned           []api.CacheEdit
	ConsumedInFlight []api.CacheEdit
	CreatedAt        float64
}

func (m *ContextManager) QueueCacheEdits(edits []CacheEdit) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(edits) == 0 {
		return
	}
	for _, edit := range edits {
		action := "delete"
		if edit.Action != nil && *edit.Action != "" {
			action = *edit.Action
		}
		if edit.ToolUseID != "" {
			m.cache.Pending = append(m.cache.Pending, api.CacheEdit{ToolUseID: edit.ToolUseID, Action: action})
		}
	}
}

func (m *ContextManager) CacheEditStateSnapshot() CacheState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return CacheState{
		Pending:          append([]api.CacheEdit(nil), m.cache.Pending...),
		Pinned:           append([]api.CacheEdit(nil), m.cache.Pinned...),
		ConsumedInFlight: append([]api.CacheEdit(nil), m.cache.ConsumedInFlight...),
		CreatedAt:        m.cache.CreatedAt,
	}
}

func (m *ContextManager) ConsumeCacheEditsForRequest(visibleToolResultIDs map[string]struct{}, models ...string) *api.LLMRequestOptions {
	return m.consumeCacheEditsForRequest(visibleToolResultIDs, models...)
}

func (m *ContextManager) PinConsumedCacheEdits(generations ...uint64) {
	m.pinConsumedCacheEdits(generations...)
}

func (m *ContextManager) RestoreInFlightCacheEdits(generations ...uint64) {
	m.restoreInFlightCacheEdits(generations...)
}

func (m *ContextManager) consumeCacheEditsForRequest(visibleToolResultIDs map[string]struct{}, models ...string) *api.LLMRequestOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.consumeCacheLocked(visibleToolResultIDs, models...)
}
func (m *ContextManager) ConsumeCacheEditsForGeneration(ids map[string]struct{}, model string, generation uint64) *api.LLMRequestOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation != m.state.Generation {
		return &api.LLMRequestOptions{}
	}
	return m.consumeCacheLocked(ids, model)
}
func (m *ContextManager) consumeCacheLocked(visibleToolResultIDs map[string]struct{}, models ...string) *api.LLMRequestOptions {
	model := m.cfg.APIModel
	if len(models) > 0 {
		model = models[0]
	}
	if !strings.HasPrefix(strings.ToLower(model), "claude-") {
		m.cache = CacheState{}
		return &api.LLMRequestOptions{}
	}
	if !m.cfg.AnthropicCacheEditsEnabled {
		return &api.LLMRequestOptions{}
	}

	pinned := filterVisibleCacheEdits(m.cache.Pinned, visibleToolResultIDs)
	pending := filterVisibleCacheEdits(m.cache.Pending, visibleToolResultIDs)
	m.cache.Pinned = pinned
	m.cache.Pending = nil
	m.cache.ConsumedInFlight = pending

	edits := make([]api.CacheEdit, 0, len(pinned)+len(pending))
	edits = append(edits, pinned...)
	edits = append(edits, pending...)
	return &api.LLMRequestOptions{AnthropicCacheEdits: edits}
}

func (m *ContextManager) pinConsumedCacheEdits(generations ...uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(generations) > 0 && generations[0] != m.state.Generation {
		return
	}
	if len(m.cache.ConsumedInFlight) == 0 {
		return
	}
	existing := map[string]struct{}{}
	for _, edit := range m.cache.Pinned {
		existing[edit.ToolUseID] = struct{}{}
	}
	for _, edit := range m.cache.ConsumedInFlight {
		if _, ok := existing[edit.ToolUseID]; ok {
			continue
		}
		m.cache.Pinned = append(m.cache.Pinned, edit)
		existing[edit.ToolUseID] = struct{}{}
	}
	m.cache.ConsumedInFlight = nil
}

func (m *ContextManager) restoreInFlightCacheEdits(generations ...uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(generations) > 0 && generations[0] != m.state.Generation {
		return
	}
	if len(m.cache.ConsumedInFlight) == 0 {
		return
	}
	restored := make([]api.CacheEdit, 0, len(m.cache.ConsumedInFlight)+len(m.cache.Pending))
	restored = append(restored, m.cache.ConsumedInFlight...)
	restored = append(restored, m.cache.Pending...)
	m.cache.Pending = restored
	m.cache.ConsumedInFlight = nil
}

func filterVisibleCacheEdits(edits []api.CacheEdit, visibleToolResultIDs map[string]struct{}) []api.CacheEdit {
	if len(edits) == 0 {
		return nil
	}
	out := make([]api.CacheEdit, 0, len(edits))
	for _, edit := range edits {
		if _, ok := visibleToolResultIDs[edit.ToolUseID]; ok {
			out = append(out, edit)
		}
	}
	return out
}

func (m *ContextManager) ResetCache() { m.mu.Lock(); defer m.mu.Unlock(); m.cache = CacheState{} }
