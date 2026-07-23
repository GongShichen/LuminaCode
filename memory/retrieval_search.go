package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	bgeChannelLimit    = 128
	bgeRRFPoolLimit    = 384
	bgeBaseEventLimit  = 128
	bgePPRSeedLimit    = 64
	bgePPRAddLimit     = 64
	bgeExactEventLimit = 192
	bgeFinalLimit      = 192
)

type scoredSidecarID struct {
	id    string
	score float64
}

type sidecarCandidate struct {
	spanID, eventID, sourceEventID, space, contextID, actor, occurredAt, content string
	tokenCount, spanIndex, windowCount                                           int
	denseBlob                                                                    []byte
	ranks                                                                        map[string]int
	rawScores                                                                    map[string]float64
	rrf, denseScore, sparseScore, maxSim, ppr, score                             float64
	queryTokenScores                                                             []float64
	queryTokenWeights                                                            []float64
	matchPosition                                                                int
	component                                                                    string
}

type sidecarSpan struct {
	spanID, eventID, content string
	tokenCount, spanIndex    int
}

type exactSidecarDiagnostics struct {
	spans      int
	eventScore time.Duration
	spanSelect time.Duration
}

type sidecarSearchDiagnostics struct {
	fts, dense, sparse, rrfUnion, maxSim, seeds, ppr, pprAdditions        int
	exactEvents, exactSpans, encodeBatches                                int
	contextExpansionContexts, contextExpandedEvents, contextExpandedSpans int
	ledgerEvents, sidecarEvents                                           int
	latency                                                               map[string]time.Duration
	schema, tokenizerHash                                                 string
	candidateSourceEvents, candidateContexts                              []string
	exactSourceEvents, exactContexts                                      []string
}

func (f *Fabric) searchBGERetrieval(ctx context.Context, request SearchRequest,
	analysis queryAnalysis) ([]*rankedCandidate, sidecarSearchDiagnostics, error) {
	diagnostics := sidecarSearchDiagnostics{latency: map[string]time.Duration{}}
	if f == nil || f.sidecar == nil || f.options.RetrievalEncoder == nil {
		return nil, diagnostics, errors.New("BGE retrieval is not configured")
	}
	stage := time.Now()
	if err := f.SyncRetrievalSidecar(ctx); err != nil {
		return nil, diagnostics, err
	}
	f.sidecarMu.RLock()
	defer f.sidecarMu.RUnlock()
	if f.sidecar == nil {
		return nil, diagnostics, errors.New("BGE retrieval sidecar is closed")
	}
	diagnostics.latency["sidecar_sync"] = time.Since(stage)
	stage = time.Now()
	var encoded []RetrievalEncoding
	var err error
	if encoder, channelsOnly := f.options.RetrievalEncoder.(RetrievalChannelEncoder); channelsOnly {
		encoded, err = encoder.EncodeChannels(ctx, []string{request.Query}, RetrievalQuery)
	} else {
		encoded, err = f.options.RetrievalEncoder.Encode(ctx, []string{request.Query}, RetrievalQuery)
	}
	if err != nil || len(encoded) != 1 {
		if err == nil {
			err = fmt.Errorf("BGE query encoding count %d", len(encoded))
		}
		return nil, diagnostics, err
	}
	query := encoded[0]
	diagnostics.latency["query_encode"] = time.Since(stage)

	stage = time.Now()
	channels := make(map[string][]scoredSidecarID, 3)
	channels["bge_fts"], err = f.sidecarFTS(ctx, request.Space, analysis, request.ReferenceTime, bgeChannelLimit)
	if err != nil {
		return nil, diagnostics, err
	}
	channels["bge_dense"], err = f.sidecarDense(ctx, request.Space, query.Dense, request.ReferenceTime, bgeChannelLimit)
	if err != nil {
		return nil, diagnostics, err
	}
	channels["bge_sparse"], err = f.sidecarSparse(ctx, request.Space, query.Sparse, request.ReferenceTime, bgeChannelLimit)
	if err != nil {
		return nil, diagnostics, err
	}
	diagnostics.fts, diagnostics.dense, diagnostics.sparse = len(channels["bge_fts"]), len(channels["bge_dense"]), len(channels["bge_sparse"])
	diagnostics.latency["channels"] = time.Since(stage)

	stage = time.Now()
	rrfRanks := map[string]map[string]int{}
	rawScores := map[string]map[string]float64{}
	for channel, values := range channels {
		for index, value := range values {
			if rrfRanks[value.id] == nil {
				rrfRanks[value.id] = map[string]int{}
			}
			rrfRanks[value.id][channel] = index + 1
			if rawScores[value.id] == nil {
				rawScores[value.id] = map[string]float64{}
			}
			rawScores[value.id][channel] = value.score
		}
	}
	ids := make([]string, 0, len(rrfRanks))
	for id := range rrfRanks {
		ids = append(ids, id)
	}
	candidates, err := f.loadSidecarEventCandidates(ctx, ids)
	if err != nil {
		return nil, diagnostics, err
	}
	for _, candidate := range candidates {
		candidate.ranks = rrfRanks[candidate.eventID]
		candidate.rawScores = rawScores[candidate.eventID]
		for _, rank := range candidate.ranks {
			candidate.rrf += 1 / float64(searchRRFK+rank)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].rrf == candidates[j].rrf {
			return candidates[i].spanID < candidates[j].spanID
		}
		return candidates[i].rrf > candidates[j].rrf
	})
	if len(candidates) > bgeRRFPoolLimit {
		candidates = candidates[:bgeRRFPoolLimit]
	}
	diagnostics.rrfUnion = len(candidates)
	for _, candidate := range candidates {
		diagnostics.candidateSourceEvents = appendUniqueDiagnosticValue(
			diagnostics.candidateSourceEvents, candidate.sourceEventID)
		diagnostics.candidateContexts = appendUniqueDiagnosticValue(
			diagnostics.candidateContexts, candidate.contextID)
	}
	diagnostics.latency["rrf_union"] = time.Since(stage)

	stage = time.Now()
	seedCount := minIntMemory(bgePPRSeedLimit, len(candidates))
	diagnostics.seeds = seedCount
	seedScores := map[string]float64{}
	for _, candidate := range candidates[:seedCount] {
		value := math.Max(candidate.rrf, 0) + 1e-6
		key := "event:" + candidate.eventID
		if value > seedScores[key] {
			seedScores[key] = value
		}
	}
	ppr, components, err := f.sidecarPersonalizedPageRank(ctx, request.Space, seedScores, request.ReferenceTime)
	if err != nil {
		return nil, diagnostics, err
	}
	baseCount := minIntMemory(bgeBaseEventLimit, len(candidates))
	shortlist := append([]*sidecarCandidate(nil), candidates[:baseCount]...)
	seen := make(map[string]struct{}, bgeExactEventLimit)
	for _, candidate := range shortlist {
		candidate.ppr = ppr["event:"+candidate.eventID]
		candidate.component = components["event:"+candidate.eventID]
		seen[candidate.eventID] = struct{}{}
	}
	pprEventIDs := topUnseenPPREventIDs(ppr, seen, bgePPRAddLimit)
	diagnostics.ppr = len(pprEventIDs)
	additional, err := f.loadSidecarEventCandidates(ctx, pprEventIDs)
	if err != nil {
		return nil, diagnostics, err
	}
	for _, candidate := range additional {
		if len(shortlist) >= bgeExactEventLimit {
			break
		}
		if _, ok := seen[candidate.eventID]; ok {
			continue
		}
		if normalizeSpace(candidate.space) != normalizeSpace(request.Space) {
			continue
		}
		occurred := parseFabricTime(candidate.occurredAt)
		if !occurred.IsZero() && occurred.After(request.ReferenceTime) {
			continue
		}
		candidate.ranks = rrfRanks[candidate.eventID]
		candidate.rawScores = rawScores[candidate.eventID]
		for _, rank := range candidate.ranks {
			candidate.rrf += 1 / float64(searchRRFK+rank)
		}
		candidate.ppr = ppr["event:"+candidate.eventID]
		candidate.component = components["event:"+candidate.eventID]
		shortlist = append(shortlist, candidate)
		seen[candidate.eventID] = struct{}{}
		diagnostics.pprAdditions++
	}
	diagnostics.latency["ppr"] = time.Since(stage)

	stage = time.Now()
	shortlist, exactDiagnostics, err := f.fastScoreSidecarEvents(ctx, query, analysis, shortlist)
	if err != nil {
		return nil, diagnostics, err
	}
	sort.SliceStable(shortlist, func(i, j int) bool {
		if shortlist[i].score == shortlist[j].score {
			return shortlist[i].eventID < shortlist[j].eventID
		}
		return shortlist[i].score > shortlist[j].score
	})
	diagnostics.latency["event_score"] = exactDiagnostics.eventScore
	diagnostics.latency["span_select"] = exactDiagnostics.spanSelect

	stage = time.Now()
	expansionContexts := topSidecarContextIDs(shortlist, len(shortlist))
	diagnostics.contextExpansionContexts = len(expansionContexts)
	exactSources := make(map[string]struct{}, len(shortlist))
	for _, candidate := range shortlist {
		exactSources[candidate.sourceEventID] = struct{}{}
	}
	expanded, err := f.loadSidecarContextEvents(ctx, request.Space, expansionContexts,
		request.ReferenceTime, exactSources)
	if err != nil {
		return nil, diagnostics, err
	}
	diagnostics.contextExpandedEvents = len(expanded)
	for _, candidate := range expanded {
		candidate.component = components["event:"+candidate.eventID]
		candidate.rawScores = map[string]float64{"bge_context_expansion": 1}
		diagnostics.candidateSourceEvents = appendUniqueDiagnosticValue(
			diagnostics.candidateSourceEvents, candidate.sourceEventID)
		diagnostics.candidateContexts = appendUniqueDiagnosticValue(
			diagnostics.candidateContexts, candidate.contextID)
	}
	diagnostics.latency["context_expansion_load"] = time.Since(stage)
	stage = time.Now()
	expanded, expansionDiagnostics, err := f.fastScoreSidecarEvents(ctx, query, analysis, expanded)
	if err != nil {
		return nil, diagnostics, err
	}
	diagnostics.contextExpandedSpans = expansionDiagnostics.spans
	diagnostics.latency["context_event_score"] = expansionDiagnostics.eventScore
	diagnostics.latency["context_span_select"] = expansionDiagnostics.spanSelect
	shortlist = append(shortlist, expanded...)
	sort.SliceStable(shortlist, func(i, j int) bool {
		if shortlist[i].score == shortlist[j].score {
			return shortlist[i].eventID < shortlist[j].eventID
		}
		return shortlist[i].score > shortlist[j].score
	})
	if len(shortlist) > bgeFinalLimit {
		shortlist = shortlist[:bgeFinalLimit]
	}
	diagnostics.exactEvents = len(shortlist)
	diagnostics.exactSpans = exactDiagnostics.spans + expansionDiagnostics.spans
	diagnostics.encodeBatches = 0
	diagnostics.maxSim = 0
	diagnostics.latency["event_score"] += expansionDiagnostics.eventScore
	diagnostics.latency["span_select"] += expansionDiagnostics.spanSelect
	for _, candidate := range shortlist {
		diagnostics.exactSourceEvents = appendUniqueDiagnosticValue(
			diagnostics.exactSourceEvents, candidate.sourceEventID)
		diagnostics.exactContexts = appendUniqueDiagnosticValue(
			diagnostics.exactContexts, candidate.contextID)
	}
	diagnostics.latency["exact_final"] = time.Since(stage)
	_ = f.sidecar.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_vectors`).Scan(&diagnostics.sidecarEvents)
	_ = f.ledger.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE tombstoned=0`).Scan(&diagnostics.ledgerEvents)
	_ = f.sidecar.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&diagnostics.schema)
	_ = f.sidecar.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='tokenizer_hash'`).Scan(&diagnostics.tokenizerHash)

	result := make([]*rankedCandidate, 0, len(shortlist))
	for _, candidate := range shortlist {
		occurred := parseFabricTime(candidate.occurredAt)
		if !occurred.IsZero() && occurred.After(request.ReferenceTime) {
			continue
		}
		result = append(result, &rankedCandidate{document: searchDocument{ID: candidate.spanID, Space: candidate.space,
			ResourceKind: "event", ResourceID: candidate.eventID, Content: candidate.content, ContextID: candidate.contextID,
			Actor: candidate.actor, OccurredAt: occurred, SourceEventIDs: []string{candidate.sourceEventID}}, ranks: candidate.ranks,
			score: candidate.score, coverage: weightedQueryCoverage(candidate.content, analysis),
			channelScores: cloneFloatScores(candidate.rawScores), maxSimScore: candidate.maxSim,
			queryTokenScores:  append([]float64(nil), candidate.queryTokenScores...),
			queryTokenWeights: append([]float64(nil), candidate.queryTokenWeights...),
			graphComponent:    candidate.component, matchPosition: candidate.matchPosition,
			spanTokens: candidate.tokenCount,
			reasons:    bgeCandidateReasons(candidate)})
	}
	return result, diagnostics, nil
}

func (f *Fabric) loadSidecarContextEvents(ctx context.Context, space string, contextIDs []string,
	reference time.Time, excluded map[string]struct{}) ([]*sidecarCandidate, error) {
	contextIDs = uniqueStrings(contextIDs)
	if len(contextIDs) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(contextIDs)+2)
	for _, contextID := range contextIDs {
		args = append(args, contextID)
	}
	args = append(args, normalizeSpace(space), formatFabricTime(reference))
	rows, err := f.sidecar.QueryContext(ctx, `SELECT event_id,source_event_id,space,context_id,actor,occurred_at,
		content,token_count,window_count,dense_fp16 FROM event_vectors WHERE context_id IN (`+
		placeholders(len(contextIDs))+`) AND space=? AND (occurred_at='' OR occurred_at<=?)
		ORDER BY context_id,occurred_at,event_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []*sidecarCandidate
	for rows.Next() {
		item := &sidecarCandidate{}
		if err := rows.Scan(&item.eventID, &item.sourceEventID, &item.space, &item.contextID, &item.actor,
			&item.occurredAt, &item.content, &item.tokenCount, &item.windowCount, &item.denseBlob); err != nil {
			return nil, err
		}
		if _, skip := excluded[item.sourceEventID]; skip {
			continue
		}
		item.spanID = item.eventID
		result = append(result, item)
	}
	return result, rows.Err()
}

func topSidecarContextIDs(candidates []*sidecarCandidate, limit int) []string {
	if limit <= 0 {
		return nil
	}
	result := make([]string, 0, limit)
	seen := make(map[string]struct{}, limit)
	for _, candidate := range candidates {
		if candidate == nil || candidate.contextID == "" {
			continue
		}
		if _, exists := seen[candidate.contextID]; exists {
			continue
		}
		seen[candidate.contextID] = struct{}{}
		result = append(result, candidate.contextID)
		if len(result) == limit {
			break
		}
	}
	return result
}

func bgeCandidateReasons(candidate *sidecarCandidate) []string {
	reasons := []string{"bge-m3", "event-dense-sparse", "graph"}
	if candidate != nil && candidate.rawScores["bge_context_expansion"] > 0 {
		reasons = append(reasons, "context-event-expansion")
	}
	return reasons
}

func (f *Fabric) sidecarFTS(ctx context.Context, space string, analysis queryAnalysis,
	reference time.Time, limit int) ([]scoredSidecarID, error) {
	match := ftsTermsFromTokens(analysis.tokens, 16)
	if match == "" {
		return nil, nil
	}
	rows, err := f.sidecar.QueryContext(ctx, `SELECT spans.event_id,bm25(span_fts) FROM span_fts
		JOIN spans ON spans.span_id=span_fts.span_id
		WHERE span_fts MATCH ? AND span_fts.space=? AND (spans.occurred_at='' OR spans.occurred_at<=?)
		ORDER BY bm25(span_fts),spans.event_id,spans.span_index LIMIT ?`, match, normalizeSpace(space),
		formatFabricTime(reference), maxIntMemory(limit, limit*8))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]scoredSidecarID, 0, limit)
	seen := make(map[string]struct{}, limit)
	for rows.Next() {
		var id string
		var bm25 float64
		if err := rows.Scan(&id, &bm25); err != nil {
			return nil, err
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, scoredSidecarID{id: id, score: -bm25})
		if len(result) >= limit {
			break
		}
	}
	return result, rows.Err()
}

func (f *Fabric) sidecarDense(ctx context.Context, space string, query []float32,
	reference time.Time, limit int) ([]scoredSidecarID, error) {
	if len(query) == 0 {
		return nil, nil
	}
	rows, err := f.sidecar.QueryContext(ctx, `SELECT event_id,dense_fp16 FROM event_vectors
		WHERE space=? AND (occurred_at='' OR occurred_at<=?)`, normalizeSpace(space), formatFabricTime(reference))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []scoredSidecarID
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, err
		}
		dense := decodeFP16Vector(blob)
		if len(dense) != len(query) {
			continue
		}
		values = append(values, scoredSidecarID{id: id, score: dotFloat32(query, dense)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(values, func(i, j int) bool {
		if values[i].score == values[j].score {
			return values[i].id < values[j].id
		}
		return values[i].score > values[j].score
	})
	if len(values) > limit {
		values = values[:limit]
	}
	return values, nil
}

func (f *Fabric) sidecarSparse(ctx context.Context, space string, query map[int64]float32,
	reference time.Time, limit int) ([]scoredSidecarID, error) {
	if len(query) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(query))
	for id := range query {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	scores := map[string]float64{}
	const sqliteTokenBatch = 256
	for start := 0; start < len(ids); start += sqliteTokenBatch {
		end := minIntMemory(len(ids), start+sqliteTokenBatch)
		args := make([]any, 0, end-start+2)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		args = append(args, normalizeSpace(space), formatFabricTime(reference))
		rows, err := f.sidecar.QueryContext(ctx, `SELECT p.event_id,p.token_id,p.weight FROM event_sparse_postings p
			JOIN event_vectors e ON e.event_id=p.event_id WHERE p.token_id IN (`+placeholders(end-start)+`)
			AND e.space=? AND (e.occurred_at='' OR e.occurred_at<=?)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var eventID string
			var tokenID int64
			var weight float64
			if err := rows.Scan(&eventID, &tokenID, &weight); err != nil {
				_ = rows.Close()
				return nil, err
			}
			scores[eventID] += float64(query[tokenID]) * weight
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	values := make([]scoredSidecarID, 0, len(scores))
	for id, score := range scores {
		values = append(values, scoredSidecarID{id: id, score: score})
	}
	sort.SliceStable(values, func(i, j int) bool {
		if values[i].score == values[j].score {
			return values[i].id < values[j].id
		}
		return values[i].score > values[j].score
	})
	if len(values) > limit {
		values = values[:limit]
	}
	return values, nil
}

func (f *Fabric) loadSidecarEventCandidates(ctx context.Context, ids []string) ([]*sidecarCandidate, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := f.sidecar.QueryContext(ctx, `SELECT event_id,source_event_id,space,context_id,actor,occurred_at,
		content,token_count,window_count,dense_fp16 FROM event_vectors WHERE event_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []*sidecarCandidate
	for rows.Next() {
		item := &sidecarCandidate{}
		if err := rows.Scan(&item.eventID, &item.sourceEventID, &item.space, &item.contextID, &item.actor,
			&item.occurredAt, &item.content, &item.tokenCount, &item.windowCount, &item.denseBlob); err != nil {
			return nil, err
		}
		item.spanID = item.eventID
		result = append(result, item)
	}
	return result, rows.Err()
}

func (f *Fabric) loadSidecarSpansByEvents(ctx context.Context, ids []string) ([]sidecarSpan, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var result []sidecarSpan
	const eventBatch = 256
	for start := 0; start < len(ids); start += eventBatch {
		end := minIntMemory(len(ids), start+eventBatch)
		args := make([]any, end-start)
		for index, id := range ids[start:end] {
			args[index] = id
		}
		rows, err := f.sidecar.QueryContext(ctx, `SELECT span_id,event_id,content,token_count,span_index
			FROM spans WHERE event_id IN (`+placeholders(len(args))+`) ORDER BY event_id,span_index`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var span sidecarSpan
			if err := rows.Scan(&span.spanID, &span.eventID, &span.content, &span.tokenCount,
				&span.spanIndex); err != nil {
				_ = rows.Close()
				return nil, err
			}
			result = append(result, span)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (f *Fabric) fastScoreSidecarEvents(ctx context.Context, query RetrievalEncoding,
	analysis queryAnalysis, candidates []*sidecarCandidate) ([]*sidecarCandidate, exactSidecarDiagnostics, error) {
	diagnostics := exactSidecarDiagnostics{}
	if len(candidates) == 0 {
		return nil, diagnostics, nil
	}
	eventScoreStarted := time.Now()
	ids := make([]string, len(candidates))
	byEvent := make(map[string]*sidecarCandidate, len(candidates))
	for index, candidate := range candidates {
		ids[index] = candidate.eventID
		byEvent[candidate.eventID] = candidate
	}
	sparseScores, err := f.sidecarSparseScoresForEvents(ctx, query.Sparse, byEvent)
	if err != nil {
		return nil, diagnostics, err
	}
	for _, candidate := range candidates {
		dense := decodeFP16Vector(candidate.denseBlob)
		if len(query.Dense) != len(dense) {
			continue
		}
		candidate.denseScore = dotFloat32(query.Dense, dense)
		candidate.sparseScore = sparseScores[candidate.eventID]
		candidate.score = fusedBGEEventScore(candidate.denseScore, candidate.sparseScore)
		if candidate.rawScores == nil {
			candidate.rawScores = map[string]float64{}
		}
		candidate.rawScores["bge_dense_event"] = candidate.denseScore
		candidate.rawScores["bge_sparse_event"] = candidate.sparseScore
		candidate.rawScores["bge_fused_event"] = candidate.score
	}
	diagnostics.eventScore = time.Since(eventScoreStarted)

	spanStarted := time.Now()
	spans, err := f.loadSidecarSpansByEvents(ctx, ids)
	if err != nil {
		return nil, diagnostics, err
	}
	diagnostics.spans = len(spans)
	bestCoverage := make(map[string]float64, len(candidates))
	for _, span := range spans {
		candidate := byEvent[span.eventID]
		if candidate == nil {
			continue
		}
		coverage := weightedQueryCoverage(span.content, analysis)
		if candidate.spanID != candidate.eventID &&
			(coverage < bestCoverage[span.eventID] ||
				(coverage == bestCoverage[span.eventID] && span.spanID >= candidate.spanID)) {
			continue
		}
		candidate.spanID = span.spanID
		candidate.content = span.content
		candidate.tokenCount = span.tokenCount
		candidate.spanIndex = span.spanIndex
		bestCoverage[span.eventID] = coverage
	}
	diagnostics.spanSelect = time.Since(spanStarted)
	result := make([]*sidecarCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.spanID != candidate.eventID {
			result = append(result, candidate)
		}
	}
	return result, diagnostics, nil
}

func (f *Fabric) sidecarSparseScoresForEvents(ctx context.Context, query map[int64]float32,
	events map[string]*sidecarCandidate) (map[string]float64, error) {
	scores := make(map[string]float64, len(events))
	if len(query) == 0 || len(events) == 0 {
		return scores, nil
	}
	tokenIDs := make([]int64, 0, len(query))
	for tokenID := range query {
		tokenIDs = append(tokenIDs, tokenID)
	}
	sort.Slice(tokenIDs, func(i, j int) bool { return tokenIDs[i] < tokenIDs[j] })
	const tokenBatch = 256
	for start := 0; start < len(tokenIDs); start += tokenBatch {
		end := minIntMemory(len(tokenIDs), start+tokenBatch)
		args := make([]any, end-start)
		for index, tokenID := range tokenIDs[start:end] {
			args[index] = tokenID
		}
		rows, err := f.sidecar.QueryContext(ctx, `SELECT event_id,token_id,weight
			FROM event_sparse_postings WHERE token_id IN (`+placeholders(len(args))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var eventID string
			var tokenID int64
			var weight float64
			if err := rows.Scan(&eventID, &tokenID, &weight); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if _, wanted := events[eventID]; wanted {
				scores[eventID] += float64(query[tokenID]) * weight
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return scores, nil
}

func sparseDotProduct(left, right map[int64]float32) float64 {
	if len(left) > len(right) {
		left, right = right, left
	}
	var result float64
	for tokenID, weight := range left {
		result += float64(weight * right[tokenID])
	}
	return result
}

func fusedBGEExactScore(dense, sparse, colbert float64) float64 {
	return (dense + .3*sparse + colbert) / 2.3
}

func fusedBGEEventScore(dense, sparse float64) float64 {
	return (dense + .3*sparse) / 1.3
}

func cloneFloatScores(scores map[string]float64) map[string]float64 {
	if len(scores) == 0 {
		return nil
	}
	result := make(map[string]float64, len(scores))
	for channel, score := range scores {
		result[channel] = score
	}
	return result
}

func appendUniqueDiagnosticValue(values []string, value string) []string {
	if value == "" || containsString(values, value) {
		return values
	}
	return append(values, value)
}

func multiVectorMaxSim(query, document []RetrievalTokenVector) float64 {
	score, _, _ := multiVectorMaxSimDetails(query, document)
	return score
}

func retrievalTokenWeights(tokens []RetrievalTokenVector) []float64 {
	weights := make([]float64, len(tokens))
	for index, token := range tokens {
		if token.Weight > 0 {
			weights[index] = float64(token.Weight)
		}
	}
	return weights
}

func multiVectorMaxSimPosition(query, document []RetrievalTokenVector) (float64, int) {
	score, position, _ := multiVectorMaxSimDetails(query, document)
	return score, position
}

func multiVectorMaxSimDetails(query, document []RetrievalTokenVector) (float64, int, []float64) {
	if len(query) == 0 || len(document) == 0 {
		return 0, 0, nil
	}
	var total float64
	positionScores := map[int]float64{}
	queryTokenScores := make([]float64, len(query))
	for queryIndex, q := range query {
		best := -math.MaxFloat64
		bestPosition := 0
		for _, d := range document {
			if len(q.Values) != len(d.Values) {
				continue
			}
			score := dotFloat32(q.Values, d.Values)
			if score > best {
				best = score
				bestPosition = d.Position
			}
		}
		if best > -math.MaxFloat64 {
			total += best
			queryTokenScores[queryIndex] = best
			positionScores[bestPosition] += best
		}
	}
	bestPosition := 0
	bestPositionScore := -math.MaxFloat64
	for position, score := range positionScores {
		if score > bestPositionScore || (score == bestPositionScore && position < bestPosition) {
			bestPosition, bestPositionScore = position, score
		}
	}
	return total / float64(len(query)), bestPosition, queryTokenScores
}

func dotFloat32(left, right []float32) float64 {
	var result float64
	for i, value := range left {
		result += float64(value * right[i])
	}
	return result
}

func (f *Fabric) sidecarPersonalizedPageRank(ctx context.Context, space string, seeds map[string]float64,
	reference time.Time) (map[string]float64, map[string]string, error) {
	blocked := map[string]struct{}{}
	futureRows, err := f.sidecar.QueryContext(ctx, `SELECT event_id FROM event_vectors
		WHERE space!=? OR (occurred_at!='' AND occurred_at>?)`, normalizeSpace(space), formatFabricTime(reference))
	if err != nil {
		return nil, nil, err
	}
	for futureRows.Next() {
		var eventID string
		if err := futureRows.Scan(&eventID); err != nil {
			_ = futureRows.Close()
			return nil, nil, err
		}
		blocked["event:"+eventID] = struct{}{}
	}
	if err := futureRows.Close(); err != nil {
		return nil, nil, err
	}
	rows, err := f.sidecar.QueryContext(ctx, `SELECT source_id,target_id,weight FROM graph_edges`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	adj := map[string]map[string]float64{}
	undirected := map[string][]string{}
	for rows.Next() {
		var source, target string
		var weight float64
		if err := rows.Scan(&source, &target, &weight); err != nil {
			return nil, nil, err
		}
		if _, skip := blocked[source]; skip {
			continue
		}
		if _, skip := blocked[target]; skip {
			continue
		}
		if adj[source] == nil {
			adj[source] = map[string]float64{}
		}
		adj[source][target] = weight
		undirected[source] = append(undirected[source], target)
		undirected[target] = append(undirected[target], source)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var seedTotal float64
	for _, value := range seeds {
		seedTotal += value
	}
	if seedTotal == 0 {
		return map[string]float64{}, map[string]string{}, nil
	}
	personal := map[string]float64{}
	for id, value := range seeds {
		personal[id] = value / seedTotal
	}
	rank := map[string]float64{}
	for id, value := range personal {
		rank[id] = value
	}
	const damping = .85
	for iteration := 0; iteration < 30; iteration++ {
		next := map[string]float64{}
		for id, value := range personal {
			next[id] = (1 - damping) * value
		}
		var dangling float64
		for source, value := range rank {
			var total float64
			for _, weight := range adj[source] {
				total += weight
			}
			if total == 0 {
				dangling += value
				continue
			}
			for target, weight := range adj[source] {
				next[target] += damping * value * weight / total
			}
		}
		if dangling > 0 {
			for id, value := range personal {
				next[id] += damping * dangling * value
			}
		}
		var delta float64
		keys := map[string]struct{}{}
		for id := range rank {
			keys[id] = struct{}{}
		}
		for id := range next {
			keys[id] = struct{}{}
		}
		for id := range keys {
			delta += math.Abs(next[id] - rank[id])
		}
		rank = next
		if delta < 1e-6 {
			break
		}
	}
	components := map[string]string{}
	var componentIndex int
	for node := range undirected {
		if _, seen := components[node]; seen {
			continue
		}
		componentIndex++
		label := fmt.Sprintf("component:%d", componentIndex)
		queue := []string{node}
		components[node] = label
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			for _, neighbor := range undirected[current] {
				if _, seen := components[neighbor]; seen {
					continue
				}
				components[neighbor] = label
				queue = append(queue, neighbor)
			}
		}
	}
	return rank, components, nil
}

func topUnseenPPREventIDs(rank map[string]float64, seen map[string]struct{}, limit int) []string {
	type item struct {
		id    string
		score float64
	}
	var values []item
	for id, score := range rank {
		if strings.HasPrefix(id, "event:") {
			eventID := strings.TrimPrefix(id, "event:")
			if _, exists := seen[eventID]; exists {
				continue
			}
			values = append(values, item{eventID, score})
		}
	}
	sort.SliceStable(values, func(i, j int) bool {
		if values[i].score == values[j].score {
			return values[i].id < values[j].id
		}
		return values[i].score > values[j].score
	})
	if len(values) > limit {
		values = values[:limit]
	}
	result := make([]string, len(values))
	for i := range values {
		result[i] = values[i].id
	}
	return result
}

func mergeBGEWithState(hybrid, baseline []*rankedCandidate) []*rankedCandidate {
	result := append([]*rankedCandidate(nil), hybrid...)
	seenSources := map[string]struct{}{}
	for _, candidate := range hybrid {
		for _, id := range candidate.document.SourceEventIDs {
			seenSources[id] = struct{}{}
		}
	}
	for _, candidate := range baseline {
		if !candidate.stateOnly {
			continue
		}
		overlap := false
		for _, id := range candidate.document.SourceEventIDs {
			if _, ok := seenSources[id]; ok {
				overlap = true
				break
			}
		}
		if !overlap {
			result = append(result, candidate)
		}
	}
	return result
}

func selectSubmodularEvidence(candidates []*rankedCandidate, analysis queryAnalysis, maxItems, maxTokens int) ([]Evidence, []string, int) {
	if maxItems <= 0 || maxTokens <= 0 {
		return nil, nil, 0
	}
	remaining := append([]*rankedCandidate(nil), candidates...)
	selectedSources := map[string]struct{}{}
	selectedTerms := map[string]struct{}{}
	selectedContexts := map[string]struct{}{}
	selectedComponents := map[string]struct{}{}
	selectedSemantic := make([]float64, maxQueryTokenScoreCount(candidates))
	totalQueryTermWeight := 0.0
	for _, weight := range analysis.weights {
		if weight <= 0 {
			weight = 1
		}
		totalQueryTermWeight += weight
	}
	var selectedContents []map[string]struct{}
	var evidence []Evidence
	var nodeIDs []string
	tokens := 0
	deduplicated := 0
	for _, candidate := range candidates {
		if candidate.stateOnly && candidate.document.ResourceKind == "node" {
			nodeIDs = append(nodeIDs, candidate.document.ResourceID)
		}
	}
	for len(remaining) > 0 && len(evidence) < maxItems {
		best := -1
		bestUtility := -math.MaxFloat64
		bestContent := ""
		bestContentTokens := map[string]struct{}{}
		bestTokens := 0
		var bestSemantic []float64
		for index, candidate := range remaining {
			if candidate.stateOnly {
				continue
			}
			duplicate := false
			for _, id := range candidate.document.SourceEventIDs {
				if _, ok := selectedSources[id]; ok {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
			selectionTarget := maxIntMemory(24, maxTokens/maxIntMemory(1, maxItems))
			selectionContent := dynamicEvidenceSnippet(candidate.document.Content, analysis,
				selectionTarget,
				candidate.matchPosition, candidate.spanTokens)
			contentTokens := queryTokenSet(selectionContent)
			cumulativeFairBudget := (len(evidence) + 1) * maxTokens / maxIntMemory(1, maxItems)
			payloadTarget := minIntMemory(selectionTarget*2,
				maxIntMemory(selectionTarget, cumulativeFairBudget-tokens))
			payloadContent := extractMemorySearchMultiWindowSnippetAt(candidate.document.Content,
				analysis, payloadTarget, candidate.matchPosition, candidate.spanTokens)
			overlap := 0.0
			for _, prior := range selectedContents {
				overlap = math.Max(overlap, tokenJaccardSets(contentTokens, prior))
			}
			if overlap > .85 {
				continue
			}
			newTermWeight := 0.0
			for term := range contentTokens {
				if weight, queryTerm := analysis.weights[term]; queryTerm {
					if _, seen := selectedTerms[term]; !seen {
						if weight <= 0 {
							weight = 1
						}
						newTermWeight += weight
					}
				}
			}
			itemTokens := maxIntMemory(1, estimateTokens(payloadContent))
			if tokens+itemTokens > maxTokens {
				continue
			}
			semanticGain := semanticFacilityGain(candidate.queryTokenScores, selectedSemantic,
				candidate.queryTokenWeights)
			relevance := math.Max(0, candidate.score)
			lexicalGain := 0.0
			if totalQueryTermWeight > 0 {
				lexicalGain = newTermWeight / totalQueryTermWeight
			}
			contextGain := 0.0
			if contextID := candidate.document.ContextID; contextID != "" {
				if _, seen := selectedContexts[contextID]; !seen {
					contextGain = 1
				}
			}
			componentGain := 0.0
			if component := candidate.graphComponent; component != "" {
				if _, seen := selectedComponents[component]; !seen {
					componentGain = 1
				}
			}
			utility := relevance + .005*semanticGain + .005*lexicalGain +
				.002*contextGain + .001*componentGain - .01*overlap
			if utility > bestUtility || (utility == bestUtility &&
				(best < 0 || candidate.document.ID < remaining[best].document.ID)) {
				best = index
				bestUtility = utility
				bestContent = payloadContent
				bestContentTokens = contentTokens
				bestTokens = itemTokens
				bestSemantic = candidate.queryTokenScores
			}
		}
		if best < 0 {
			break
		}
		candidate := remaining[best]
		document := candidate.document
		evidence = append(evidence, Evidence{ID: document.ID, ResourceID: document.ResourceID,
			ResourceKind: document.ResourceKind, Content: bestContent, Score: candidate.score,
			OccurredAt: document.OccurredAt, ContextID: document.ContextID, Actor: document.Actor,
			SlotID: document.SlotID, Status: document.Status,
			SourceEventIDs: append([]string(nil), document.SourceEventIDs...),
			MatchReasons:   uniqueStrings(candidate.reasons)})
		tokens += bestTokens
		for _, id := range document.SourceEventIDs {
			selectedSources[id] = struct{}{}
		}
		set := bestContentTokens
		selectedContents = append(selectedContents, set)
		for term := range set {
			selectedTerms[term] = struct{}{}
		}
		if document.ContextID != "" {
			selectedContexts[document.ContextID] = struct{}{}
		}
		if candidate.graphComponent != "" {
			selectedComponents[candidate.graphComponent] = struct{}{}
		}
		mergeSemanticFacility(selectedSemantic, bestSemantic)
		remaining = append(remaining[:best], remaining[best+1:]...)
	}
	deduplicated = maxIntMemory(0, len(candidates)-len(selectedSources))
	return evidence, uniqueStrings(nodeIDs), deduplicated
}

func maxQueryTokenScoreCount(candidates []*rankedCandidate) int {
	maximum := 0
	for _, candidate := range candidates {
		if candidate != nil && len(candidate.queryTokenScores) > maximum {
			maximum = len(candidate.queryTokenScores)
		}
	}
	return maximum
}

func semanticFacilityGain(candidate, selected, weights []float64) float64 {
	if len(candidate) == 0 {
		return 0
	}
	weighted := len(weights) == len(candidate)
	weightTotal := 0.0
	if weighted {
		for _, weight := range weights {
			weightTotal += math.Max(0, weight)
		}
		weighted = weightTotal > 0
	}
	var gain, denominator float64
	for index, score := range candidate {
		score = math.Max(0, math.Min(1, score))
		weight := 1.0
		if weighted {
			weight = math.Max(0, weights[index])
		}
		denominator += weight
		covered := 0.0
		if index < len(selected) {
			covered = selected[index]
		}
		if score > covered {
			gain += weight * (score - covered)
		}
	}
	if denominator <= 0 {
		return 0
	}
	return gain / denominator
}

func mergeSemanticFacility(selected, candidate []float64) {
	for index, score := range candidate {
		if index >= len(selected) {
			break
		}
		score = math.Max(0, math.Min(1, score))
		if score > selected[index] {
			selected[index] = score
		}
	}
}

func dynamicEvidenceSnippet(content string, analysis queryAnalysis, targetTokens, matchPosition, spanTokens int) string {
	if estimateTokens(content) <= targetTokens {
		return strings.TrimSpace(content)
	}
	if matchPosition > 0 && spanTokens > 0 {
		if snippet := maxSimWindows(content, targetTokens, matchPosition, spanTokens); snippet != "" {
			return snippet
		}
	}
	return extractMemorySearchSnippet(content, analysis, targetTokens)
}

func maxSimWindows(content string, targetTokens, matchPosition, spanTokens int) string {
	words := strings.Fields(content)
	if len(words) == 0 || targetTokens <= 0 || spanTokens <= 0 {
		return ""
	}
	targetWords := maxIntMemory(8, targetTokens*3/4)
	if targetWords >= len(words) {
		return strings.TrimSpace(content)
	}
	center := matchPosition * len(words) / spanTokens
	if center >= len(words) {
		center = len(words) - 1
	}
	firstWords := minIntMemory(targetWords, 72)
	start := center - firstWords/2
	if start < 0 {
		start = 0
	}
	end := minIntMemory(len(words), start+firstWords)
	start = maxIntMemory(0, end-firstWords)
	first := strings.Join(words[start:end], " ")
	remaining := targetWords - (end - start)
	if remaining < 8 || end == len(words) {
		return first
	}
	secondStart := minIntMemory(len(words)-1, end+maxIntMemory(1, (len(words)-end)/2-remaining/2))
	secondEnd := minIntMemory(len(words), secondStart+minIntMemory(72, remaining))
	if secondEnd <= secondStart {
		return first
	}
	return first + "\n...\n" + strings.Join(words[secondStart:secondEnd], " ")
}

func tokenJaccardSets(left, right map[string]struct{}) float64 {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	intersection := 0
	union := map[string]struct{}{}
	for token := range left {
		union[token] = struct{}{}
		if _, ok := right[token]; ok {
			intersection++
		}
	}
	for token := range right {
		union[token] = struct{}{}
	}
	return float64(intersection) / float64(len(union))
}

func applySidecarDiagnostics(target *SearchDiagnostics, source sidecarSearchDiagnostics, revision string) {
	target.BGEFTSCandidates = source.fts
	target.BGEDenseCandidates = source.dense
	target.BGESparseCandidates = source.sparse
	target.MaxSimCandidates = source.maxSim
	target.PPRSeedEvents = source.seeds
	target.PPRCandidates = source.ppr
	target.RRFCandidates = source.rrfUnion
	target.PPRAdditions = source.pprAdditions
	target.ExactScoredEvents = source.exactEvents
	target.ExactScoredSpans = source.exactSpans
	target.DocumentEncodeBatches = source.encodeBatches
	target.ContextExpansionContexts = source.contextExpansionContexts
	target.ContextExpandedEvents = source.contextExpandedEvents
	target.ContextExpandedSpans = source.contextExpandedSpans
	target.StageLatency = source.latency
	target.RetrievalModelRevision = revision
	target.RetrievalSidecarSchema = source.schema
	target.RetrievalTokenizerHash = source.tokenizerHash
	target.LedgerEvents = source.ledgerEvents
	target.SidecarEvents = source.sidecarEvents
	target.SidecarAligned = source.ledgerEvents == source.sidecarEvents
	target.CandidateSourceEvents = append([]string(nil), source.candidateSourceEvents...)
	target.CandidateContextIDs = append([]string(nil), source.candidateContexts...)
	target.ExactSourceEvents = append([]string(nil), source.exactSourceEvents...)
	target.ExactContextIDs = append([]string(nil), source.exactContexts...)
}
