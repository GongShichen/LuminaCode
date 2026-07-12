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
	embeddingBefore := EmbeddingStats(embedder)
	queries := normalizeStrings(append([]string{query.Text}, expansion.Queries...))
	for _, facet := range expansion.Facets {
		queries = append(queries, facet.Text)
	}
	for _, constraint := range expansion.TemporalConstraints {
		queries = append(queries, constraint.FromText, constraint.ToText, constraint.AtText)
	}
	queries = normalizeStrings(queries)
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
		RunID:                       StableID(ScopeProject, "retrieval", query.Text, formatTime(started)),
		Query:                       query,
		Expansion:                   expansion,
		ExpansionModel:              opts.ExpansionModel,
		ExpansionError:              opts.ExpansionError,
		ExpansionWaitMS:             opts.ExpansionWaitMS,
		CreatedAt:                   started,
		StopReason:                  "completed",
		ReferenceTime:               query.Timestamp,
		GlobalChannelCandidates:     map[string]int{},
		PerSessionChannelCandidates: map[string]map[string]int{},
		NativeChannelCandidates:     map[string]int{},
		CoverageFacets:              normalizeStrings(append(append([]string(nil), expansion.Queries...), append(expansion.Entities, expansion.RelationTerms...)...)),
		QueryExpansionParseMode:     expansion.ParseMode,
	}
	trace := RetrievalTrace{
		RunID:         run.RunID,
		SessionID:     query.SessionID,
		TeamSessionID: query.TeamSessionID,
		AgentID:       query.AgentID,
		Plan:          plan,
		CreatedAt:     started,
	}
	cacheKey := ""
	if opts.CacheEnabled && opts.CacheTTL > 0 {
		var scopeKey string
		cacheKey, scopeKey = s.retrievalCacheKey(ctx, query, expansion, opts)
		if cached, ok := getCachedRetrieval(cacheKey); ok {
			run.CacheHit, run.CacheKeyScope = true, scopeKey
			run.ChannelResults = append([]ChannelResult(nil), cached.channelResults...)
			run.GlobalChannelCandidates = cached.globalCounts
			run.NativeChannelCandidates = cached.nativeCounts
			run.PerSessionChannelCandidates = cached.perSessionCounts
			run.CoverageLedger = cached.coverageLedger
			run.DuplicateSignalSuppression = cached.duplicateSignalSuppression
			run.ResidualSweepCandidates = cached.residualSweepCandidates
			run.EmbeddingTrace = EmbeddingTrace{}
			run.CanonicalEntities, run.CanonicalEvents = cached.canonicalEntities, cached.canonicalEvents
			run.SelectedIDs = append([]string(nil), cached.selectedIDs...)
			run.InjectedIDs = evidenceIDs(cached.packet.Evidence)
			run.Evidence = append([]Evidence(nil), cached.packet.Evidence...)
			run.EstimatedTokens = cached.packet.EstimatedTokens
			run.DurationMS = time.Since(started).Milliseconds()
			trace.SelectedIDs, trace.EstimatedTokens, trace.DurationMS = run.SelectedIDs, run.EstimatedTokens, run.DurationMS
			trace.Run = &run
			if !opts.SuppressTrace {
				_ = s.RecordRetrievalTrace(context.WithoutCancel(ctx), trace)
			}
			return AllChannelResult{Packet: cached.packet, Trace: trace, Run: run}, nil
		}
		run.CacheKeyScope = scopeKey
	}

	combined := map[string]*CandidateScore{}
	var warnings []string
	channelIndexes := map[string]int{}
	mergeOutputs := func(outputs []baseChannelOutput, delta bool) {
		for _, output := range outputs {
			channelResult := ChannelResult{Channel: output.name, Query: output.query,
				DurationMS: output.done.Sub(output.started).Milliseconds()}
			if output.err != nil {
				channelResult.Error = output.err.Error()
				warnings = append(warnings, output.name+": "+output.err.Error())
			} else {
				mergeChannelEntries(combined, output.name, output.entries, opts.RRFK)
				channelResult.Candidates = retrievalCandidates(output.entries, output.name)
				run.GlobalChannelCandidates[output.name] += len(output.entries)
				run.NativeChannelCandidates[output.name] += len(output.entries)
			}
			if index, ok := channelIndexes[output.name]; ok && delta {
				existing := &run.ChannelResults[index]
				existing.Query = strings.Trim(strings.Join([]string{existing.Query, channelResult.Query}, " | "), " |")
				existing.DurationMS += channelResult.DurationMS
				existing.Candidates = append(existing.Candidates, channelResult.Candidates...)
				if channelResult.Error != "" {
					existing.Error = strings.Trim(strings.Join([]string{existing.Error, channelResult.Error}, "; "), "; ")
				}
			} else {
				channelIndexes[output.name] = len(run.ChannelResults)
				run.ChannelResults = append(run.ChannelResults, channelResult)
			}
		}
	}
	mergeOutputs(s.executeBaseChannels(ctx, queries, expansion, query, embedder, opts), false)
	if opts.ExpansionFuture != nil {
		var expanded ExpansionResult
		var received bool
		select {
		case expanded, received = <-opts.ExpansionFuture:
		default:
			if opts.ExpansionDeadline.IsZero() {
				// No configured timeout means recall quality takes priority: wait
				// for expansion unless the user cancels the parent request.
				select {
				case expanded, received = <-opts.ExpansionFuture:
				case <-ctx.Done():
				}
				break
			}
			wait := opts.ExpansionAdditionalWait
			if wait <= 0 {
				wait = 750 * time.Millisecond
			}
			// The expansion call owns a total deadline. If baseline retrieval
			// finishes early, keep accepting the future until that deadline so a
			// healthy request is not mislabeled as an incremental-window failure.
			if remaining := time.Until(opts.ExpansionDeadline); remaining > wait {
				wait = remaining
			}
			if wait < 0 {
				wait = 0
			}
			timer := time.NewTimer(wait)
			select {
			case expanded, received = <-opts.ExpansionFuture:
			case <-timer.C:
			case <-ctx.Done():
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		if received {
			expansion = expanded.Expansion
			if expansion.ParseMode == "" && expanded.Error != "" {
				expansion.ParseMode = "base_only"
			}
			run.Expansion, run.ExpansionModel, run.ExpansionError = expansion, expanded.Model, expanded.Error
			run.ExpansionWaitMS, run.QueryExpansionParseMode = expanded.DurationMS, expansion.ParseMode
			run.CoverageFacets = normalizeStrings(append(append([]string(nil), expansion.Queries...), append(expansion.Entities, expansion.RelationTerms...)...))
			deltaQueries := expansionQueries(expansion)
			if len(deltaQueries) > 0 {
				mergeOutputs(s.executeBaseChannels(ctx, deltaQueries, expansion, query, embedder, opts), true)
				queries = normalizeStrings(append(queries, deltaQueries...))
			}
			plan.Subqueries, plan.Entities = append([]string(nil), queries...), normalizeStrings(expansion.Entities)
			trace.Plan = plan
		} else {
			run.ExpansionError = "query expansion did not finish before the configured deadline"
			run.QueryExpansionParseMode = "base_only"
		}
	}
	sort.SliceStable(run.ChannelResults, func(i, j int) bool {
		return channelOrder(run.ChannelResults[i].Channel) < channelOrder(run.ChannelResults[j].Channel)
	})

	var selectedSessionRanks []sessionRank
	if opts.SessionRetrieval {
		sessionCtx, sessionCancel := localRetrievalContext(ctx, opts.LocalTimeout)
		sessionRanks, sessionErr := s.rankSessions(sessionCtx, queries, query.Scopes, opts.SessionCandidates)
		sessionCancel()
		if sessionErr != nil {
			warnings = append(warnings, "session-scoped retrieval: "+sessionErr.Error())
		} else if len(sessionRanks) > 0 {
			selectedSessionRanks = append([]sessionRank(nil), sessionRanks...)
			for _, item := range sessionRanks {
				run.SelectedSessions = append(run.SelectedSessions, item.SessionID)
			}
			scoped, counts, scopedWarnings := s.searchSelectedSessions(ctx, sessionRanks, queries,
				expansion, query.Scopes, embedder, opts)
			run.PerSessionChannelCandidates = counts
			warnings = append(warnings, scopedWarnings...)
			mergeSessionScopedCandidates(combined, scoped, opts.RRFK)
		}
	}

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
		graphCtx, graphCancel := localRetrievalContext(ctx, opts.LocalTimeout)
		graphScores, err := s.ExpandGraph(graphCtx, seeds, query.Scopes, opts.GraphMaxHops, opts.GraphCandidates)
		graphCancel()
		if err != nil {
			graphResult.Error = err.Error()
			warnings = append(warnings, channelGraph+": "+err.Error())
		} else {
			graphEntries, loadErr := s.entriesForGraph(ctx, graphScores)
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
	for _, candidate := range combined {
		families := map[string]struct{}{}
		for _, contribution := range candidate.Contributions {
			families[contribution.SignalFamily] = struct{}{}
		}
		if len(candidate.ChannelRanks) > len(families) {
			run.DuplicateSignalSuppression += len(candidate.ChannelRanks) - len(families)
		}
	}

	candidates := preferEvidenceAtoms(filterExcludedCandidates(candidateSlice(combined), opts.ExcludeIDs), opts.RRFK)
	opts.CoverageFacets = run.CoverageFacets
	facets := BuildCoverageFacets(plan, expansion, opts.CoverageMaxFacets)
	primaryBudget := int(float64(minInt(opts.TargetContextTokens, opts.MaxContextTokens)) * opts.EvidencePrimaryBudgetRatio)
	_, initialLedger := BuildCoverageLedger(candidates, facets, opts, primaryBudget)
	if needsResidualCoverage(initialLedger, opts.CoverageSupportTarget) && opts.CoverageCompletionRounds > 0 {
		residual := s.searchResidualCoverage(ctx, query, expansion, initialLedger, selectedSessionRanks, embedder, opts)
		run.ResidualSweepCandidates = len(residual)
		for _, candidate := range residual {
			mergeCandidateScore(combined, candidate, opts.RRFK)
		}
		candidates = preferEvidenceAtoms(filterExcludedCandidates(candidateSlice(combined), opts.ExcludeIDs), opts.RRFK)
	}
	selected, ledger := BuildCoverageLedger(candidates, facets, opts,
		int(float64(minInt(opts.TargetContextTokens, opts.MaxContextTokens))*(opts.EvidencePrimaryBudgetRatio+opts.EvidenceCompletionBudgetRatio)))
	run.CoverageLedger = ledger
	for _, item := range selected {
		run.SelectedIDs = append(run.SelectedIDs, item.MemoryID)
		if stored := combined[item.MemoryID]; stored != nil {
			stored.Selected = true
		}
	}
	for _, item := range combined {
		if !item.Selected && item.DropReason == "" {
			item.DropReason = "lower_coverage_utility_or_budget"
		}
	}

	blocks, err := s.ListCoreBlocks(ctx, query.Scopes)
	if err != nil {
		warnings = append(warnings, "core blocks: "+err.Error())
	}
	opts.ReferenceTime = query.Timestamp
	packet, packetErr := s.BuildAtomEvidencePacket(ctx, plan, selected, ledger, blocks, opts)
	if packetErr != nil {
		warnings = append(warnings, "evidence packet: "+packetErr.Error())
		run.StopReason = "evidence_packet_error"
	}
	packet.Warnings = append(packet.Warnings, warnings...)
	if opts.CanonicalEntityEnabled {
		entities, entityErr := s.SearchCanonicalEntities(ctx, strings.Join(queries, " "), query.Scopes)
		if entityErr != nil {
			packet.Warnings = append(packet.Warnings, "canonical entities: "+entityErr.Error())
		} else {
			run.CanonicalEntities = entities
		}
	}
	if opts.CanonicalEventEnabled {
		events, eventErr := s.SearchCanonicalEvents(ctx, strings.Join(queries, " "), query.Scopes)
		if eventErr != nil {
			packet.Warnings = append(packet.Warnings, "canonical events: "+eventErr.Error())
		} else {
			run.CanonicalEvents, packet.CanonicalEvents = events, events
			for _, event := range events {
				packet.Timeline = append(packet.Timeline, TimelineEntry{EventID: event.EventID, Text: event.Summary,
					OccurredAt: event.OccurredAt, ValidFrom: event.ValidFrom, ValidUntil: event.ValidUntil})
			}
			sort.SliceStable(packet.Timeline, func(i, j int) bool {
				left, right := packet.Timeline[i].OccurredAt, packet.Timeline[j].OccurredAt
				if left.IsZero() {
					left = packet.Timeline[i].ValidFrom
				}
				if right.IsZero() {
					right = packet.Timeline[j].ValidFrom
				}
				return left.Before(right)
			})
		}
	}
	run.Evidence = append([]Evidence(nil), packet.Evidence...)
	run.InjectedIDs = evidenceIDs(packet.Evidence)
	run.EstimatedTokens = packet.EstimatedTokens
	run.EmbeddingTrace = EmbeddingStatsDelta(embeddingBefore, EmbeddingStats(embedder))
	run.DurationMS = time.Since(started).Milliseconds()
	trace.Candidates = candidateSlice(combined)
	trace.SelectedIDs = append([]string(nil), run.SelectedIDs...)
	trace.EstimatedTokens = packet.EstimatedTokens
	trace.DurationMS = run.DurationMS
	trace.Run = &run
	if packetErr != nil {
		trace.Error = packetErr.Error()
	}
	if !opts.SuppressTrace {
		if err := s.RecordRetrievalTrace(context.WithoutCancel(ctx), trace); err != nil {
			packet.Warnings = append(packet.Warnings, "record retrieval trace: "+err.Error())
		}
		_ = s.MarkAccess(context.WithoutCancel(ctx), run.InjectedIDs)
	}
	if cacheKey != "" && packetErr == nil {
		putCachedRetrieval(cacheKey, packet, run, opts.CacheTTL)
	}
	return AllChannelResult{Packet: packet, Trace: trace, Run: run}, packetErr
}

func (s *Store) executeBaseChannels(ctx context.Context, queries []string, expansion QueryExpansion,
	query MemoryQuery, embedder Embedder, opts HybridSearchOptions) []baseChannelOutput {
	queries = normalizeStrings(queries)
	outputs := make(chan baseChannelOutput, 5)
	var wait sync.WaitGroup
	run := func(name string, fn func(context.Context) ([]Entry, error)) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			started := time.Now()
			entries, err := fn(ctx)
			outputs <- baseChannelOutput{name: name, query: strings.Join(queries, " | "), entries: entries,
				err: err, started: started, done: time.Now()}
		}()
	}
	run(channelBM25, func(channelCtx context.Context) ([]Entry, error) {
		return s.searchBM25Queries(channelCtx, queries, query.Scopes, opts)
	})
	run(channelVector, func(channelCtx context.Context) ([]Entry, error) {
		vectorCtx, cancel := embeddingRetrievalContext(channelCtx, embedder, opts.LocalTimeout)
		defer cancel()
		return s.searchVectorQueries(vectorCtx, queries, query.Scopes, embedder, opts)
	})
	run(channelEntity, func(channelCtx context.Context) ([]Entry, error) {
		return s.SearchEntities(channelCtx, queries, expansion.Entities, query.Scopes, opts.FTSCandidates)
	})
	run(channelTemporal, func(channelCtx context.Context) ([]Entry, error) {
		return s.SearchTemporal(channelCtx, queries, expansion.TemporalConstraints, query.Scopes, opts.FTSCandidates)
	})
	run(channelSession, func(channelCtx context.Context) ([]Entry, error) {
		return s.SearchSessions(channelCtx, queries, query.Scopes, opts.FTSCandidates)
	})
	go func() { wait.Wait(); close(outputs) }()
	result := make([]baseChannelOutput, 0, 5)
	for output := range outputs {
		result = append(result, output)
	}
	return result
}

func expansionQueries(expansion QueryExpansion) []string {
	queries := append([]string(nil), expansion.Queries...)
	for _, facet := range expansion.Facets {
		queries = append(queries, facet.Text)
	}
	for _, constraint := range expansion.TemporalConstraints {
		queries = append(queries, constraint.FromText, constraint.ToText, constraint.AtText)
	}
	return normalizeStrings(queries)
}

func needsResidualCoverage(ledger CoverageLedger, trigger float64) bool {
	for _, state := range ledger.FacetStates {
		if state.Required && state.SupportMass < trigger {
			return true
		}
	}
	return false
}

func localRetrievalContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

func embeddingRetrievalContext(ctx context.Context, embedder Embedder, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, scheduled := embedder.(*ScheduledEmbedder); scheduled {
		return ctx, func() {}
	}
	return localRetrievalContext(ctx, timeout)
}

func preferEvidenceAtoms(candidates []CandidateScore, rrfK int) []CandidateScore {
	atomMessages := map[string][]int{}
	atomSessions := map[string]struct{}{}
	for index, candidate := range candidates {
		if candidate.Entry.DocumentKind != "atom" {
			continue
		}
		if candidate.Entry.MessageID != "" {
			atomMessages[candidate.Entry.MessageID] = append(atomMessages[candidate.Entry.MessageID], index)
		}
		if candidate.Entry.SourceSessionID != "" {
			atomSessions[candidate.Entry.SourceSessionID] = struct{}{}
		}
	}
	if len(atomMessages) == 0 && len(atomSessions) == 0 {
		return candidates
	}
	// Chunk/vector and memory signals often identify the right source message
	// while BM25 identifies the precise atom. Transfer those orthogonal signals
	// before removing the coarser document; otherwise atom-first selection throws
	// away the semantic evidence that found the message in the first place.
	for _, candidate := range candidates {
		if candidate.Entry.DocumentKind == "atom" || candidate.Entry.DocumentKind == "session" {
			continue
		}
		messageIDs := append([]string(nil), candidate.Entry.SourceMessageIDs...)
		if candidate.Entry.MessageID != "" {
			messageIDs = append(messageIDs, candidate.Entry.MessageID)
		}
		for _, messageID := range normalizeStrings(messageIDs) {
			for _, atomIndex := range atomMessages[messageID] {
				atom := &candidates[atomIndex]
				var semantic []SignalContribution
				semanticChannels := map[string]struct{}{}
				for _, contribution := range candidate.Contributions {
					if contribution.SignalFamily == "semantic" {
						semantic = append(semantic, contribution)
						semanticChannels[contribution.Channel] = struct{}{}
					}
				}
				atom.Contributions = mergeContributions(atom.Contributions, semantic)
				for channel, rank := range candidate.ChannelRanks {
					if _, ok := semanticChannels[channel]; !ok {
						continue
					}
					if prior, ok := atom.ChannelRanks[channel]; !ok || rank < prior {
						atom.ChannelRanks[channel] = rank
					}
					if candidate.ChannelScores[channel] > atom.ChannelScores[channel] {
						atom.ChannelScores[channel] = candidate.ChannelScores[channel]
					}
				}
				recalculateOrthogonalScore(atom, rrfK)
			}
		}
	}
	filtered := make([]CandidateScore, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Entry.DocumentKind == "atom" {
			filtered = append(filtered, candidate)
			continue
		}
		covered := false
		for _, messageID := range candidate.Entry.SourceMessageIDs {
			if len(atomMessages[messageID]) > 0 {
				covered = true
				break
			}
		}
		if candidate.Entry.DocumentKind == "session" {
			_, covered = atomSessions[candidate.Entry.SourceSessionID]
		}
		if !covered {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func (s *Store) searchBM25Queries(ctx context.Context, queries []string, scopes []Scope, opts HybridSearchOptions) ([]Entry, error) {
	return mergeQueryEntries(queries, func(query string) ([]Entry, error) {
		memoryEntries, err := s.Search(ctx, SearchOptions{Query: query, Scopes: scopes, Limit: opts.FTSCandidates,
			MaxCandidates: opts.FTSCandidates, ExcludeIDs: opts.ExcludeIDs})
		if err != nil {
			return nil, err
		}
		atomEntries, err := s.SearchAtomsKeyword(ctx, []string{query}, scopes, "", opts.FTSCandidates)
		if err != nil {
			return nil, err
		}
		if len(atomEntries) == 0 {
			atomEntries, err = s.SearchChunkBM25(ctx, []string{query}, scopes, opts.FTSCandidates)
			if err != nil {
				return nil, err
			}
		}
		return appendUniqueEntries(atomEntries, memoryEntries), nil
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
				atomEntries, atomErr := s.SearchAtomsSemantic(ctx, vectors[index], embedder.Model(), scopes, "", opts.VectorCandidates)
				if atomErr != nil {
					return nil, atomErr
				}
				if len(atomEntries) == 0 {
					atomEntries, atomErr = s.SearchChunkVector(ctx, vectors[index], embedder.Model(), scopes, opts.VectorCandidates)
					if atomErr != nil {
						return nil, atomErr
					}
				}
				return appendUniqueEntries(atomEntries, appendUniqueEntries(memoryEntries, sessionEntries)), nil
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
			item = &CandidateScore{MemoryID: entry.MemoryID, DocumentID: entry.MemoryID, Entry: entry,
				ChannelRanks: map[string]int{}, ChannelScores: map[string]float64{}}
			combined[entry.MemoryID] = item
		}
		item.ChannelRanks[channel] = rank + 1
		item.ChannelScores[channel] = entry.Score
		family, native := entrySignalFamily(channel, entry)
		item.Contributions = mergeContributions(item.Contributions, []SignalContribution{{Channel: channel,
			SignalFamily: family, Rank: rank + 1, Score: entry.Score, Native: native}})
		recalculateOrthogonalScore(item, rrfK)
	}
}

func entrySignalFamily(channel string, entry Entry) (string, bool) {
	switch entry.MatchReason {
	case "temporal_text_fallback":
		return "lexical", false
	case "entity_text_fallback":
		return "lexical", false
	}
	return channelSignalFamily(channel), true
}

func mergeSessionScopedCandidates(combined map[string]*CandidateScore, sessions map[string][]CandidateScore, rrfK int) {
	for _, candidates := range sessions {
		for _, candidate := range candidates {
			item := combined[candidate.MemoryID]
			if item == nil {
				copy := candidate
				copy.ChannelRanks = map[string]int{}
				copy.ChannelScores = map[string]float64{}
				item = &copy
				combined[candidate.MemoryID] = item
			}
			item.Contributions = mergeContributions(item.Contributions, candidate.Contributions)
			for channel, channelRank := range candidate.ChannelRanks {
				if prior, exists := item.ChannelRanks[channel]; !exists || channelRank < prior {
					item.ChannelRanks[channel] = channelRank
				}
				item.ChannelScores[channel] = candidate.ChannelScores[channel]
			}
			recalculateOrthogonalScore(item, rrfK)
		}
	}
}

func channelSignalFamily(channel string) string {
	switch channel {
	case channelBM25:
		return "lexical"
	case channelVector:
		return "semantic"
	case channelEntity:
		return "structured_entity"
	case channelTemporal:
		return "structured_time"
	case channelSession:
		return "session_parent"
	case channelGraph:
		return "relation"
	default:
		return channel
	}
}

func recalculateOrthogonalScore(item *CandidateScore, rrfK int) {
	if rrfK <= 0 {
		rrfK = 60
	}
	bestByFamily := map[string]SignalContribution{}
	for _, contribution := range item.Contributions {
		prior, ok := bestByFamily[contribution.SignalFamily]
		if !ok || contribution.Rank < prior.Rank {
			bestByFamily[contribution.SignalFamily] = contribution
		}
	}
	item.FusedScore = 0
	for _, contribution := range bestByFamily {
		item.FusedScore += 1 / float64(rrfK+maxInt(contribution.Rank, 1))
	}
}

func mergeCandidateScore(combined map[string]*CandidateScore, candidate CandidateScore, rrfK int) {
	item := combined[candidate.MemoryID]
	if item == nil {
		copy := candidate
		copy.ChannelRanks = cloneStringIntMap(candidate.ChannelRanks)
		copy.ChannelScores = cloneStringFloatMapMemory(candidate.ChannelScores)
		copy.Contributions = append([]SignalContribution(nil), candidate.Contributions...)
		combined[candidate.MemoryID] = &copy
		return
	}
	item.Contributions = mergeContributions(item.Contributions, candidate.Contributions)
	for channel, rank := range candidate.ChannelRanks {
		if prior, ok := item.ChannelRanks[channel]; !ok || rank < prior {
			item.ChannelRanks[channel] = rank
		}
		item.ChannelScores[channel] = candidate.ChannelScores[channel]
	}
	recalculateOrthogonalScore(item, rrfK)
}

func cloneStringIntMap(value map[string]int) map[string]int {
	result := map[string]int{}
	for key, item := range value {
		result[key] = item
	}
	return result
}

func cloneStringFloatMapMemory(value map[string]float64) map[string]float64 {
	result := map[string]float64{}
	for key, item := range value {
		result[key] = item
	}
	return result
}

func (s *Store) searchSelectedSessions(ctx context.Context, sessions []sessionRank, queries []string,
	expansion QueryExpansion, scopes []Scope, embedder Embedder, opts HybridSearchOptions) (map[string][]CandidateScore, map[string]map[string]int, []string) {
	results := make(map[string][]CandidateScore, len(sessions))
	counts := make(map[string]map[string]int, len(sessions))
	var warnings []string
	var vectors [][]float32
	if embedder != nil {
		var err error
		embedCtx, cancel := embeddingRetrievalContext(ctx, embedder, opts.LocalTimeout)
		vectors, err = embedder.Embed(embedCtx, queries, EmbeddingQuery)
		cancel()
		if err != nil {
			warnings = append(warnings, "session vector: "+err.Error())
			vectors = nil
		}
	}
	type sessionResult struct {
		id         string
		candidates []CandidateScore
		counts     map[string]int
		warnings   []string
	}
	output := make(chan sessionResult, len(sessions))
	var wait sync.WaitGroup
	for _, session := range sessions {
		wait.Add(1)
		go func(session sessionRank) {
			defer wait.Done()
			sessionCtx, cancel := localRetrievalContext(ctx, opts.LocalTimeout)
			defer cancel()
			candidates, channelCounts, channelWarnings := s.searchOneSelectedSession(sessionCtx, session, queries,
				expansion, vectors, embedder, opts)
			output <- sessionResult{id: session.SessionID, candidates: candidates, counts: channelCounts, warnings: channelWarnings}
		}(session)
	}
	go func() { wait.Wait(); close(output) }()
	for item := range output {
		results[item.id], counts[item.id] = item.candidates, item.counts
		warnings = append(warnings, item.warnings...)
	}
	_ = scopes
	return results, counts, warnings
}

func (s *Store) searchOneSelectedSession(ctx context.Context, session sessionRank, queries []string,
	expansion QueryExpansion, vectors [][]float32, embedder Embedder, opts HybridSearchOptions) ([]CandidateScore, map[string]int, []string) {
	var warnings []string
	sessionScopes := []Scope{{Type: session.ScopeType, Key: session.ScopeKey}}
	channels := map[string][]Entry{}
	bm25, err := s.SearchAtomsKeyword(ctx, queries, sessionScopes, session.SessionID, opts.SessionChunkCandidates)
	if err != nil {
		warnings = append(warnings, session.SessionID+" bm25: "+err.Error())
	} else {
		channels[channelBM25] = bm25
	}
	entities, err := s.SearchAtomsEntity(ctx, expansion.Entities, sessionScopes,
		session.SessionID, opts.SessionChunkCandidates)
	if err != nil {
		warnings = append(warnings, session.SessionID+" entity: "+err.Error())
	} else {
		channels[channelEntity] = entities
	}
	temporalEntries, err := s.SearchAtomsTemporal(ctx, expansion.TemporalConstraints, sessionScopes,
		session.SessionID, opts.SessionChunkCandidates)
	if err != nil {
		warnings = append(warnings, session.SessionID+" temporal: "+err.Error())
	} else {
		channels[channelTemporal] = temporalEntries
	}
	if len(vectors) == len(queries) {
		var vectorEntries []Entry
		for _, vector := range vectors {
			entries, vectorErr := s.SearchAtomsSemantic(ctx, vector, embedder.Model(), sessionScopes, session.SessionID,
				opts.SessionChunkCandidates)
			if vectorErr != nil {
				warnings = append(warnings, session.SessionID+" vector: "+vectorErr.Error())
				continue
			}
			if len(entries) == 0 {
				entries, vectorErr = s.searchSessionChunkVector(ctx, session.SessionID, vector, embedder.Model(),
					sessionScopes, opts.SessionChunkCandidates)
				if vectorErr != nil {
					warnings = append(warnings, session.SessionID+" vector chunk fallback: "+vectorErr.Error())
					continue
				}
			}
			vectorEntries = appendUniqueEntries(vectorEntries, entries)
		}
		channels[channelVector] = vectorEntries
	}
	combined := map[string]*CandidateScore{}
	counts := map[string]int{}
	for _, channel := range []string{channelBM25, channelVector, channelEntity, channelTemporal} {
		entries := channels[channel]
		counts[channel] = len(entries)
		mergeChannelEntries(combined, channel, entries, opts.RRFK)
	}
	candidates := preferEvidenceAtoms(candidateSlice(combined), opts.RRFK)
	if len(candidates) > opts.ChunksPerSession {
		facets := BuildCoverageFacets(QueryPlan{Query: firstNonEmptyQuery(queries)}, expansion, opts.CoverageMaxFacets)
		sessionOpts := opts
		sessionOpts.AtomMaxSelected = opts.ChunksPerSession
		selected, _ := BuildCoverageLedger(candidates, facets, sessionOpts, maxInt(opts.TargetContextTokens, 1200))
		selectedIDs := map[string]struct{}{}
		for _, candidate := range selected {
			selectedIDs[candidate.MemoryID] = struct{}{}
		}
		for _, candidate := range candidates {
			if len(selected) >= opts.ChunksPerSession {
				break
			}
			if _, exists := selectedIDs[candidate.MemoryID]; exists {
				continue
			}
			selected = append(selected, candidate)
			selectedIDs[candidate.MemoryID] = struct{}{}
		}
		candidates = selected
	}
	return candidates, counts, warnings
}

func firstNonEmptyQuery(queries []string) string {
	for _, query := range queries {
		if query = strings.TrimSpace(query); query != "" {
			return query
		}
	}
	return "memory evidence"
}

func (s *Store) searchResidualCoverage(ctx context.Context, query MemoryQuery, expansion QueryExpansion,
	ledger CoverageLedger, sessions []sessionRank, embedder Embedder, opts HybridSearchOptions) []CandidateScore {
	uncovered := map[string]struct{}{}
	for _, facetID := range ledger.Uncovered {
		uncovered[facetID] = struct{}{}
	}
	var queries []string
	for _, facet := range ledger.Facets {
		if _, ok := uncovered[facet.FacetID]; ok {
			queries = append(queries, facet.Text)
		}
	}
	queries = normalizeStrings(append([]string{query.Text}, queries...))
	if len(queries) == 0 {
		return nil
	}
	type result struct {
		channel string
		entries []Entry
	}
	output := make(chan result, 5)
	var wait sync.WaitGroup
	run := func(channel string, fn func() ([]Entry, error)) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			entries, _ := fn()
			output <- result{channel: channel, entries: entries}
		}()
	}
	run(channelBM25, func() ([]Entry, error) {
		return s.SearchAtomsKeyword(ctx, queries, query.Scopes, "", opts.FTSCandidates)
	})
	run(channelVector, func() ([]Entry, error) {
		if embedder == nil {
			return nil, nil
		}
		embedCtx, cancel := embeddingRetrievalContext(ctx, embedder, opts.LocalTimeout)
		defer cancel()
		vectors, err := embedder.Embed(embedCtx, queries, EmbeddingQuery)
		if err != nil {
			return nil, err
		}
		var entries []Entry
		for _, vector := range vectors {
			current, searchErr := s.SearchAtomsSemantic(ctx, vector, embedder.Model(), query.Scopes, "", opts.VectorCandidates)
			if searchErr != nil {
				return nil, searchErr
			}
			entries = appendUniqueEntries(entries, current)
		}
		return entries, nil
	})
	run(channelEntity, func() ([]Entry, error) {
		return s.SearchAtomsEntity(ctx, append(append([]string(nil), expansion.Entities...), queries...), query.Scopes, "", opts.FTSCandidates)
	})
	run(channelTemporal, func() ([]Entry, error) {
		return s.SearchAtomsTemporal(ctx, expansion.TemporalConstraints, query.Scopes, "", opts.FTSCandidates)
	})
	run(channelSession, func() ([]Entry, error) { return s.SearchSessions(ctx, queries, query.Scopes, opts.SessionCandidates) })
	go func() { wait.Wait(); close(output) }()
	combined := map[string]*CandidateScore{}
	for item := range output {
		mergeChannelEntries(combined, item.channel, item.entries, opts.RRFK)
	}
	if len(sessions) > 0 {
		scoped, _, _ := s.searchSelectedSessions(ctx, sessions, queries, expansion, query.Scopes, embedder, opts)
		mergeSessionScopedCandidates(combined, scoped, opts.RRFK)
	}
	stage := candidateSlice(combined)
	seedLimit := minInt(len(stage), maxInt(opts.GraphCandidates, 8))
	if seedLimit > 0 {
		seeds := make([]string, seedLimit)
		for index := range seeds {
			seeds[index] = stage[index].MemoryID
		}
		if scores, err := s.ExpandGraph(ctx, seeds, query.Scopes, opts.GraphMaxHops, opts.GraphCandidates); err == nil {
			if entries, loadErr := s.entriesForGraph(ctx, scores); loadErr == nil {
				mergeChannelEntries(combined, channelGraph, entries, opts.RRFK)
			}
		}
	}
	return candidateSlice(combined)
}

func retrievalCandidates(entries []Entry, channel string) []RetrievalCandidate {
	result := make([]RetrievalCandidate, 0, len(entries))
	for rank, entry := range entries {
		result = append(result, RetrievalCandidate{MemoryID: entry.MemoryID, DocumentID: entry.MemoryID,
			DocumentKind: entry.DocumentKind, ParentID: entry.ParentID, Entry: entry,
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
	found := map[string]struct{}{}
	activeEntries := entries[:0]
	for index := range entries {
		if entries[index].Status != StatusActive {
			continue
		}
		found[entries[index].MemoryID] = struct{}{}
		entries[index].Score = scores[entries[index].MemoryID]
		entries[index].MatchReason = channelGraph
		activeEntries = append(activeEntries, entries[index])
	}
	entries = activeEntries
	var missing []string
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	chunks, chunkErr := s.GetChunks(ctx, missing)
	if chunkErr != nil {
		return nil, chunkErr
	}
	for _, chunk := range chunks {
		found[chunk.ChunkID] = struct{}{}
		entries = append(entries, chunkEntry(chunk, scores[chunk.ChunkID], channelGraph))
	}
	missing = missing[:0]
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	atoms, atomErr := s.GetAtoms(ctx, missing)
	if atomErr != nil {
		return nil, atomErr
	}
	for _, atom := range atoms {
		entries = append(entries, atomEntry(atom, scores[atom.AtomID], channelGraph))
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Score > entries[j].Score })
	return entries, nil
}

func evidenceIDs(evidence []Evidence) []string {
	ids := make([]string, 0, len(evidence))
	for _, item := range evidence {
		if len(item.DocumentIDs) > 0 {
			ids = append(ids, item.DocumentIDs...)
		} else if item.MemoryID != "" {
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
