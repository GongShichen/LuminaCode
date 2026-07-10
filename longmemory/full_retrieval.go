package longmemory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	channelBM25     = "bm25"
	channelVector   = "vector"
	channelEntity   = "entity"
	channelTemporal = "temporal"
	channelSession  = "session"
	channelGraph    = "graph"
)

type AllChannelResult struct {
	Packet EvidencePacket
	Trace  RetrievalTrace
	Run    RetrievalRun
}

type baseChannelOutput struct {
	name    string
	query   string
	entries []Entry
	err     error
	started time.Time
	done    time.Time
}

// SearchAllChannels always executes BM25, vector, entity, temporal, Session and
// graph retrieval. Query expansion may improve the queries, but cannot disable
// a channel, change scopes or filter memory types.
func (s *Store) SearchAllChannels(ctx context.Context, query MemoryQuery, expansion QueryExpansion, embedder Embedder, opts HybridSearchOptions) (AllChannelResult, error) {
	started := time.Now().UTC()
	queries := normalizeStrings(append([]string{query.Text}, expansion.Queries...))
	if len(queries) == 0 {
		queries = []string{strings.TrimSpace(query.Text)}
	}
	plan := QueryPlan{
		Query:               strings.TrimSpace(query.Text),
		Subqueries:          queries,
		Entities:            normalizeStrings(expansion.Entities),
		Scopes:              append([]Scope(nil), query.Scopes...),
		TargetContextTokens: opts.TargetContextTokens,
	}
	opts = normalizeHybridOptions(opts, plan)
	run := RetrievalRun{
		RunID:          StableID(ScopeProject, "retrieval", query.Text, formatTime(started)),
		Query:          query,
		Expansion:      expansion,
		ExpansionModel: opts.ExpansionModel,
		ExpansionError: opts.ExpansionError,
		CreatedAt:      started,
		StopReason:     "completed",
	}
	trace := RetrievalTrace{
		RunID:         run.RunID,
		SessionID:     query.SessionID,
		TeamSessionID: query.TeamSessionID,
		AgentID:       query.AgentID,
		Plan:          plan,
		CreatedAt:     started,
	}

	localCtx := ctx
	cancel := func() {}
	if opts.LocalTimeout > 0 {
		localCtx, cancel = context.WithTimeout(ctx, opts.LocalTimeout)
	}
	defer cancel()

	outputs := make(chan baseChannelOutput, 5)
	var wait sync.WaitGroup
	runChannel := func(name string, fn func(context.Context) ([]Entry, error)) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			channelStarted := time.Now()
			entries, err := fn(localCtx)
			outputs <- baseChannelOutput{name: name, query: strings.Join(queries, " | "), entries: entries,
				err: err, started: channelStarted, done: time.Now()}
		}()
	}
	runChannel(channelBM25, func(channelCtx context.Context) ([]Entry, error) {
		return s.searchBM25Queries(channelCtx, queries, query.Scopes, opts)
	})
	runChannel(channelVector, func(channelCtx context.Context) ([]Entry, error) {
		return s.searchVectorQueries(channelCtx, queries, query.Scopes, embedder, opts)
	})
	runChannel(channelEntity, func(channelCtx context.Context) ([]Entry, error) {
		return s.SearchEntities(channelCtx, queries, expansion.Entities, query.Scopes, opts.FTSCandidates)
	})
	runChannel(channelTemporal, func(channelCtx context.Context) ([]Entry, error) {
		return s.SearchTemporal(channelCtx, queries, expansion.TemporalConstraints, query.Scopes, opts.FTSCandidates)
	})
	runChannel(channelSession, func(channelCtx context.Context) ([]Entry, error) {
		return s.SearchSessions(channelCtx, queries, query.Scopes, opts.FTSCandidates)
	})
	go func() {
		wait.Wait()
		close(outputs)
	}()

	combined := map[string]*CandidateScore{}
	var warnings []string
	for output := range outputs {
		channelResult := ChannelResult{Channel: output.name, Query: output.query,
			DurationMS: output.done.Sub(output.started).Milliseconds()}
		if output.err != nil {
			channelResult.Error = output.err.Error()
			warnings = append(warnings, output.name+": "+output.err.Error())
		} else {
			mergeChannelEntries(combined, output.name, output.entries, opts.RRFK)
			channelResult.Candidates = retrievalCandidates(output.entries, output.name)
		}
		run.ChannelResults = append(run.ChannelResults, channelResult)
	}
	sort.SliceStable(run.ChannelResults, func(i, j int) bool {
		return channelOrder(run.ChannelResults[i].Channel) < channelOrder(run.ChannelResults[j].Channel)
	})

	stageOne := candidateSlice(combined)
	stageOne = filterExcludedCandidates(stageOne, opts.ExcludeIDs)
	graphStarted := time.Now()
	graphResult := ChannelResult{Channel: channelGraph, Query: strings.Join(expansion.RelationTerms, " | ")}
	seedLimit := minInt(len(stageOne), maxInt(opts.GraphCandidates, 8))
	if seedLimit == 0 {
		graphResult.Error = "no_graph_seed"
	} else {
		seeds := make([]string, seedLimit)
		for index := range seeds {
			seeds[index] = stageOne[index].MemoryID
		}
		graphScores, err := s.ExpandGraph(localCtx, seeds, query.Scopes, opts.GraphMaxHops, opts.GraphCandidates)
		if err != nil {
			graphResult.Error = err.Error()
			warnings = append(warnings, channelGraph+": "+err.Error())
		} else {
			graphEntries, loadErr := s.entriesForGraph(localCtx, graphScores)
			if loadErr != nil {
				graphResult.Error = loadErr.Error()
				warnings = append(warnings, channelGraph+": "+loadErr.Error())
			} else {
				mergeChannelEntries(combined, channelGraph, graphEntries, opts.RRFK)
				for _, entry := range graphEntries {
					if item := combined[entry.MemoryID]; item != nil {
						item.GraphScore = graphScores[entry.MemoryID]
					}
				}
				graphResult.Candidates = retrievalCandidates(graphEntries, channelGraph)
			}
		}
	}
	graphResult.DurationMS = time.Since(graphStarted).Milliseconds()
	run.ChannelResults = append(run.ChannelResults, graphResult)

	candidates := filterExcludedCandidates(candidateSlice(combined), opts.ExcludeIDs)
	var embeddings map[string][]float32
	if embedder != nil && len(candidates) > 0 {
		ids := make([]string, len(candidates))
		for index := range candidates {
			ids[index] = candidates[index].MemoryID
		}
		loaded, err := s.LoadEmbeddings(localCtx, ids, embedder.Model())
		if err != nil {
			warnings = append(warnings, "load candidate embeddings: "+err.Error())
		} else {
			embeddings = loaded
		}
	}
	selected := selectWithMMR(candidates, opts.MaxItems, opts.MMRLambda, embeddings)
	selected = diversifySessions(selected, candidates, opts.MaxItems)
	for _, item := range selected {
		run.SelectedIDs = append(run.SelectedIDs, item.MemoryID)
		if stored := combined[item.MemoryID]; stored != nil {
			stored.Selected = true
		}
	}
	for _, item := range combined {
		if !item.Selected && item.DropReason == "" {
			item.DropReason = "lower_fused_or_mmr_score"
		}
	}

	blocks, err := s.ListCoreBlocks(localCtx, query.Scopes)
	if err != nil {
		warnings = append(warnings, "core blocks: "+err.Error())
	}
	packet, packetErr := s.BuildEvidencePacket(localCtx, plan, selected, blocks, opts)
	if packetErr != nil {
		warnings = append(warnings, "evidence packet: "+packetErr.Error())
		run.StopReason = "evidence_packet_error"
	}
	packet.Warnings = append(packet.Warnings, warnings...)
	run.Evidence = append([]Evidence(nil), packet.Evidence...)
	run.InjectedIDs = evidenceIDs(packet.Evidence)
	run.EstimatedTokens = packet.EstimatedTokens
	run.DurationMS = time.Since(started).Milliseconds()
	trace.Candidates = candidateSlice(combined)
	trace.SelectedIDs = append([]string(nil), run.SelectedIDs...)
	trace.EstimatedTokens = packet.EstimatedTokens
	trace.DurationMS = run.DurationMS
	trace.Run = &run
	if packetErr != nil {
		trace.Error = packetErr.Error()
	}
	if err := s.RecordRetrievalTrace(context.WithoutCancel(ctx), trace); err != nil {
		packet.Warnings = append(packet.Warnings, "record retrieval trace: "+err.Error())
	}
	_ = s.MarkAccess(context.WithoutCancel(ctx), run.InjectedIDs)
	return AllChannelResult{Packet: packet, Trace: trace, Run: run}, packetErr
}

func (s *Store) searchBM25Queries(ctx context.Context, queries []string, scopes []Scope, opts HybridSearchOptions) ([]Entry, error) {
	return mergeQueryEntries(queries, func(query string) ([]Entry, error) {
		return s.Search(ctx, SearchOptions{Query: query, Scopes: scopes, Limit: opts.FTSCandidates,
			MaxCandidates: opts.FTSCandidates, ExcludeIDs: opts.ExcludeIDs})
	})
}

func (s *Store) searchVectorQueries(ctx context.Context, queries []string, scopes []Scope, embedder Embedder, opts HybridSearchOptions) ([]Entry, error) {
	if embedder == nil {
		return nil, fmt.Errorf("embedding unavailable")
	}
	vectors, err := embedder.Embed(ctx, queries, EmbeddingQuery)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(queries) {
		return nil, fmt.Errorf("embedding count mismatch: got %d, want %d", len(vectors), len(queries))
	}
	return mergeQueryEntries(queries, func(query string) ([]Entry, error) {
		for index, current := range queries {
			if current == query {
				memoryEntries, memoryErr := s.SearchVector(ctx, vectors[index], embedder.Model(), SearchOptions{Scopes: scopes,
					Limit: opts.VectorCandidates, MaxCandidates: opts.VectorCandidates, ExcludeIDs: opts.ExcludeIDs})
				if memoryErr != nil {
					return nil, memoryErr
				}
				sessionEntries, sessionErr := s.SearchSessionVector(ctx, vectors[index], embedder.Model(), scopes, opts.VectorCandidates)
				if sessionErr != nil {
					return nil, sessionErr
				}
				return appendUniqueEntries(memoryEntries, sessionEntries), nil
			}
		}
		return nil, nil
	})
}

func mergeQueryEntries(queries []string, search func(string) ([]Entry, error)) ([]Entry, error) {
	best := map[string]Entry{}
	for _, query := range queries {
		entries, err := search(query)
		if err != nil {
			return nil, err
		}
		for rank, entry := range entries {
			entry.Score += 1 / float64(rank+1)
			if prior, ok := best[entry.MemoryID]; !ok || entry.Score > prior.Score {
				best[entry.MemoryID] = entry
			}
		}
	}
	result := make([]Entry, 0, len(best))
	for _, entry := range best {
		result = append(result, entry)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Score > result[j].Score })
	return result, nil
}

func mergeChannelEntries(combined map[string]*CandidateScore, channel string, entries []Entry, rrfK int) {
	for rank, entry := range appendUniqueEntries(nil, entries) {
		item := combined[entry.MemoryID]
		if item == nil {
			item = &CandidateScore{MemoryID: entry.MemoryID, Entry: entry,
				ChannelRanks: map[string]int{}, ChannelScores: map[string]float64{}}
			combined[entry.MemoryID] = item
		}
		item.ChannelRanks[channel] = rank + 1
		item.ChannelScores[channel] = entry.Score
		item.FusedScore += 1 / float64(rrfK+rank+1)
	}
}

func retrievalCandidates(entries []Entry, channel string) []RetrievalCandidate {
	result := make([]RetrievalCandidate, 0, len(entries))
	for rank, entry := range entries {
		result = append(result, RetrievalCandidate{MemoryID: entry.MemoryID, Entry: entry,
			SourceSession: entry.SourceSessionID, SourceMessages: append([]string(nil), entry.SourceMessageIDs...),
			Scope: Scope{Type: entry.ScopeType, Key: entry.ScopeKey}, ValidFrom: entry.ValidFrom,
			ValidUntil: entry.ValidUntil, ChannelRanks: map[string]int{channel: rank + 1},
			ChannelScores: map[string]float64{channel: entry.Score}})
	}
	return result
}

func (s *Store) entriesForGraph(ctx context.Context, scores map[string]float64) ([]Entry, error) {
	if len(scores) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	entries, err := s.GetMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	for index := range entries {
		entries[index].Score = scores[entries[index].MemoryID]
		entries[index].MatchReason = channelGraph
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Score > entries[j].Score })
	return entries, nil
}

func diversifySessions(selected, candidates []CandidateScore, limit int) []CandidateScore {
	if limit <= 1 || len(candidates) <= 1 {
		return selected
	}
	result := make([]CandidateScore, 0, minInt(limit, len(candidates)))
	seenIDs, seenSessions := map[string]struct{}{}, map[string]struct{}{}
	for _, candidate := range candidates {
		session := candidate.Entry.SourceSessionID
		if session == "" {
			continue
		}
		if _, exists := seenSessions[session]; exists {
			continue
		}
		result = append(result, candidate)
		seenIDs[candidate.MemoryID] = struct{}{}
		seenSessions[session] = struct{}{}
		if len(result) >= limit {
			return result
		}
	}
	for _, candidate := range selected {
		if _, exists := seenIDs[candidate.MemoryID]; exists {
			continue
		}
		result = append(result, candidate)
		seenIDs[candidate.MemoryID] = struct{}{}
		if len(result) >= limit {
			return result
		}
	}
	for _, candidate := range candidates {
		if _, exists := seenIDs[candidate.MemoryID]; exists {
			continue
		}
		result = append(result, candidate)
		if len(result) >= limit {
			break
		}
	}
	return result
}

func evidenceIDs(evidence []Evidence) []string {
	ids := make([]string, 0, len(evidence))
	for _, item := range evidence {
		if item.MemoryID != "" {
			ids = append(ids, item.MemoryID)
		}
	}
	return normalizeStrings(ids)
}

func channelOrder(channel string) int {
	switch channel {
	case channelBM25:
		return 0
	case channelVector:
		return 1
	case channelEntity:
		return 2
	case channelTemporal:
		return 3
	case channelSession:
		return 4
	default:
		return 5
	}
}
