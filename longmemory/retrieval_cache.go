package longmemory

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type retrievalCacheEntry struct {
	packet            EvidencePacket
	selectedIDs       []string
	channelResults    []ChannelResult
	globalCounts      map[string]int
	perSessionCounts  map[string]map[string]int
	canonicalEntities []CanonicalEntity
	canonicalEvents   []CanonicalEvent
	expiresAt         time.Time
}

var sharedRetrievalCache = struct {
	sync.Mutex
	items map[string]retrievalCacheEntry
}{items: map[string]retrievalCacheEntry{}}

func (s *Store) retrievalCacheKey(ctx context.Context, query MemoryQuery, expansion QueryExpansion, opts HybridSearchOptions) (string, string) {
	generation := s.indexGeneration(ctx)
	scopes := append([]Scope(nil), query.Scopes...)
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].Type == scopes[j].Type {
			return scopes[i].Key < scopes[j].Key
		}
		return scopes[i].Type < scopes[j].Type
	})
	scopeJSON, _ := json.Marshal(scopes)
	contextJSON, _ := json.Marshal(query.RecentContext)
	expansionJSON, _ := json.Marshal(expansion)
	excludeIDs := make([]string, 0, len(opts.ExcludeIDs))
	for id := range opts.ExcludeIDs {
		excludeIDs = append(excludeIDs, id)
	}
	sort.Strings(excludeIDs)
	optionKey := struct {
		FTS, Vector, Graph, Hops, RRFK, MaxItems, Sessions, ChunksPerSession, SessionChunks int
		Relevance, Novelty, Facet, Source                                                   float64
		CoreTokens, TargetTokens, MaxTokens, Neighbors                                      int
		CanonicalEntity, CanonicalEvent                                                     bool
		Exclude                                                                             []string
	}{opts.FTSCandidates, opts.VectorCandidates, opts.GraphCandidates, opts.GraphMaxHops, opts.RRFK,
		opts.MaxItems, opts.SessionCandidates, opts.ChunksPerSession, opts.SessionChunkCandidates,
		opts.MMRRelevanceWeight, opts.MMRNoveltyWeight, opts.MMRFacetWeight, opts.MMRSourceWeight,
		opts.CoreContextTokens, opts.TargetContextTokens, opts.MaxContextTokens, opts.NeighborChunks,
		opts.CanonicalEntityEnabled, opts.CanonicalEventEnabled, excludeIDs}
	optionsJSON, _ := json.Marshal(optionKey)
	bucket := query.Timestamp.UTC().Truncate(time.Minute).Format(time.RFC3339)
	scopeKey := string(scopeJSON)
	material := strings.Join([]string{strings.ToLower(strings.Join(strings.Fields(query.Text), " ")),
		string(contextJSON), bucket, scopeKey, query.SessionID, query.TeamSessionID, query.AgentID,
		string(expansionJSON), string(optionsJSON), strconv.FormatInt(generation, 10)}, "\x00")
	key := StableID(ScopeProject, s.path, "retrieval-cache", material)
	return key, scopeKey
}

func getCachedRetrieval(key string) (retrievalCacheEntry, bool) {
	sharedRetrievalCache.Lock()
	defer sharedRetrievalCache.Unlock()
	item, ok := sharedRetrievalCache.items[key]
	if !ok {
		return retrievalCacheEntry{}, false
	}
	if time.Now().After(item.expiresAt) {
		delete(sharedRetrievalCache.items, key)
		return retrievalCacheEntry{}, false
	}
	return item, true
}

func putCachedRetrieval(key string, packet EvidencePacket, run RetrievalRun, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	sharedRetrievalCache.Lock()
	defer sharedRetrievalCache.Unlock()
	sharedRetrievalCache.items[key] = retrievalCacheEntry{packet: packet, selectedIDs: append([]string(nil), run.SelectedIDs...),
		channelResults: append([]ChannelResult(nil), run.ChannelResults...), globalCounts: run.GlobalChannelCandidates,
		perSessionCounts: run.PerSessionChannelCandidates, canonicalEntities: run.CanonicalEntities,
		canonicalEvents: run.CanonicalEvents, expiresAt: time.Now().Add(ttl)}
	if len(sharedRetrievalCache.items) > 512 {
		now := time.Now()
		for current, item := range sharedRetrievalCache.items {
			if now.After(item.expiresAt) {
				delete(sharedRetrievalCache.items, current)
			}
		}
	}
}

func (s *Store) indexGeneration(ctx context.Context) int64 {
	var value string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM memory_schema WHERE key='index_generation'`).Scan(&value); err != nil {
		return 0
	}
	generation, _ := strconv.ParseInt(value, 10, 64)
	return generation
}

func (s *Store) bumpIndexGeneration(ctx context.Context) {
	_, _ = s.db.ExecContext(ctx, `INSERT INTO memory_schema(key, value, updated_at) VALUES ('index_generation', '1', ?)
		ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(memory_schema.value AS INTEGER)+1 AS TEXT), updated_at=excluded.updated_at`, formatTime(time.Now().UTC()))
}
