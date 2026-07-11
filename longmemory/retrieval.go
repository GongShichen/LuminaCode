package longmemory

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"
)

type HybridSearchOptions struct {
	FTSCandidates          int
	VectorCandidates       int
	GraphCandidates        int
	GraphMaxHops           int
	RRFK                   int
	MMRLambda              float64
	MMRRelevanceWeight     float64
	MMRNoveltyWeight       float64
	MMRFacetWeight         float64
	MMRSourceWeight        float64
	SessionRetrieval       bool
	SessionCandidates      int
	ChunksPerSession       int
	SessionChunkCandidates int
	MaxItems               int
	CoreContextTokens      int
	TargetContextTokens    int
	MaxContextTokens       int
	LocalTimeout           time.Duration
	SessionID              string
	TeamSessionID          string
	AgentID                string
	ExcludeIDs             map[string]struct{}
	ExpansionModel         string
	ExpansionError         string
	ExpansionWaitMS        int64
	NeighborChunks         int
	ReferenceTime          time.Time
	CoverageFacets         []string
	CanonicalEntityEnabled bool
	CanonicalEventEnabled  bool
	CacheEnabled           bool
	CacheTTL               time.Duration
	SuppressTrace          bool
}

func (s *Store) BuildEvidencePacket(ctx context.Context, plan QueryPlan, selected []CandidateScore, blocks []CoreBlock, opts HybridSearchOptions) (EvidencePacket, error) {
	packet := EvidencePacket{Plan: plan, ReferenceTime: opts.ReferenceTime}
	coreBudget := minInt(opts.CoreContextTokens, opts.MaxContextTokens)
	for _, block := range blocks {
		cost := estimateTokens(block.Label + " " + block.Content)
		if packet.EstimatedTokens+cost > coreBudget {
			continue
		}
		packet.CoreBlocks = append(packet.CoreBlocks, block)
		packet.EstimatedTokens += cost
	}
	packet.SourceCoverage = map[string]int{}
	ids := make([]string, 0, len(selected))
	for _, item := range selected {
		if item.Entry.DocumentKind == "chunk" {
			ids = append(ids, item.MemoryID)
		}
	}
	chunks, err := s.GetChunks(ctx, ids)
	if err != nil {
		return packet, err
	}
	chunkByID := map[string]EvidenceChunk{}
	for _, chunk := range chunks {
		chunkByID[chunk.ChunkID] = chunk
	}
	target := minInt(opts.TargetContextTokens, opts.MaxContextTokens)
	if target <= 0 {
		target = opts.MaxContextTokens
	}
	seen := map[string]struct{}{}
	appendEntry := func(entry Entry, score float64, sourceChunks []EvidenceChunk) {
		if _, ok := seen[entry.MemoryID]; ok {
			return
		}
		remaining := minInt(target, opts.MaxContextTokens) - packet.EstimatedTokens
		if remaining <= 0 {
			return
		}
		text := strings.TrimSpace(entry.Content)
		if text == "" {
			text = strings.TrimSpace(entry.Summary)
		}
		if text == "" || estimateTokens(entry.Title+" "+text) > remaining {
			return
		}
		evidence := Evidence{
			DocumentID: entry.MemoryID, DocumentKind: entry.DocumentKind, ParentID: entry.ParentID,
			MemoryID: entry.MemoryID, Title: entry.Title, Text: text, ScopeType: entry.ScopeType,
			ScopeKey: entry.ScopeKey, MemoryType: entry.MemoryType, SourceSession: entry.SourceSessionID,
			SourceMessages: append([]string(nil), entry.SourceMessageIDs...), SourcePaths: append([]string(nil), entry.SourcePaths...),
			OccurredAt: entry.OccurredAt, ValidFrom: entry.ValidFrom, ValidUntil: entry.ValidUntil,
			Confidence: entry.Confidence, Score: score, Metadata: map[string]any{"role": entry.Role},
		}
		if len(sourceChunks) == 0 {
			evidence.DocumentIDs = []string{entry.MemoryID}
		} else {
			for _, chunk := range sourceChunks {
				evidence.DocumentIDs = append(evidence.DocumentIDs, chunk.ChunkID)
			}
		}
		if evidence.OccurredAt.IsZero() {
			evidence.OccurredAt = entry.ValidFrom
		}
		cost := estimateTokens(evidence.Title + " " + evidence.Text)
		if cost > remaining {
			return
		}
		for _, documentID := range evidence.DocumentIDs {
			seen[documentID] = struct{}{}
		}
		packet.Evidence = append(packet.Evidence, evidence)
		if len(sourceChunks) == 0 {
			packet.Documents = append(packet.Documents, documentFromEntry(entry, text))
		} else {
			for _, chunk := range sourceChunks {
				packet.Documents = append(packet.Documents, documentFromEntry(chunkEntry(chunk, score, "neighbor"), chunk.Text))
			}
		}
		packet.SourceCoverage[entry.SourceSessionID]++
		packet.EstimatedTokens += cost
	}
	for _, item := range selected {
		entry := item.Entry
		var sourceChunks []EvidenceChunk
		if chunk, ok := chunkByID[item.MemoryID]; ok && opts.NeighborChunks > 0 {
			neighbors, neighborErr := s.NeighborChunks(ctx, chunk, opts.NeighborChunks)
			if neighborErr != nil {
				packet.Warnings = append(packet.Warnings, "neighbor chunks: "+neighborErr.Error())
			} else {
				for _, neighbor := range neighbors {
					if _, alreadyUsed := seen[neighbor.ChunkID]; !alreadyUsed {
						sourceChunks = append(sourceChunks, neighbor)
					}
				}
				entry.Content = mergeNeighborChunkText(sourceChunks)
				entry.Summary = entry.Content
			}
		}
		appendEntry(entry, item.FusedScore, sourceChunks)
	}
	packet.Timeline = timelineFromDocuments(packet.Documents)
	return packet, nil
}

func timelineFromDocuments(documents []RetrievalDocument) []TimelineEntry {
	timeline := make([]TimelineEntry, 0, len(documents))
	for _, document := range documents {
		timeline = append(timeline, TimelineEntry{DocumentID: document.DocumentID, SessionID: document.SessionID,
			MessageID: document.MessageID, Role: document.Role, Text: document.Text, OccurredAt: document.OccurredAt,
			ValidFrom: document.ValidFrom, ValidUntil: document.ValidUntil})
	}
	sort.SliceStable(timeline, func(i, j int) bool {
		left, right := timeline[i].OccurredAt, timeline[j].OccurredAt
		if left.IsZero() {
			left = timeline[i].ValidFrom
		}
		if right.IsZero() {
			right = timeline[j].ValidFrom
		}
		if left.Equal(right) {
			return timeline[i].DocumentID < timeline[j].DocumentID
		}
		if left.IsZero() {
			return false
		}
		if right.IsZero() {
			return true
		}
		return left.Before(right)
	})
	return timeline
}

func mergeNeighborChunkText(chunks []EvidenceChunk) string {
	if len(chunks) == 0 {
		return ""
	}
	sort.SliceStable(chunks, func(i, j int) bool { return chunks[i].StartRune < chunks[j].StartRune })
	merged := []rune(strings.TrimSpace(chunks[0].Text))
	coveredEnd := chunks[0].EndRune
	for _, chunk := range chunks[1:] {
		current := []rune(strings.TrimSpace(chunk.Text))
		overlap := coveredEnd - chunk.StartRune
		if overlap < 0 {
			merged = append(merged, '\n')
			overlap = 0
		}
		if overlap < len(current) {
			merged = append(merged, current[overlap:]...)
		}
		if chunk.EndRune > coveredEnd {
			coveredEnd = chunk.EndRune
		}
	}
	return strings.TrimSpace(string(merged))
}

func normalizeHybridOptions(opts HybridSearchOptions, plan QueryPlan) HybridSearchOptions {
	if opts.FTSCandidates <= 0 {
		opts.FTSCandidates = 40
	}
	if opts.VectorCandidates <= 0 {
		opts.VectorCandidates = 40
	}
	if opts.GraphCandidates <= 0 {
		opts.GraphCandidates = 20
	}
	if opts.GraphMaxHops == 0 {
		opts.GraphMaxHops = 2
	}
	if opts.RRFK <= 0 {
		opts.RRFK = 60
	}
	if opts.MMRLambda <= 0 || opts.MMRLambda > 1 {
		opts.MMRLambda = 0.75
	}
	if opts.MMRRelevanceWeight+opts.MMRNoveltyWeight+opts.MMRFacetWeight+opts.MMRSourceWeight <= 0 {
		opts.MMRRelevanceWeight, opts.MMRNoveltyWeight = 0.60, 0.20
		opts.MMRFacetWeight, opts.MMRSourceWeight = 0.15, 0.05
	}
	if opts.SessionCandidates <= 0 {
		opts.SessionCandidates = 12
	}
	if opts.ChunksPerSession <= 0 {
		opts.ChunksPerSession = 6
	}
	if opts.SessionChunkCandidates <= 0 {
		opts.SessionChunkCandidates = 64
	}
	if opts.MaxItems <= 0 {
		opts.MaxItems = 8
	}
	if opts.CoreContextTokens <= 0 {
		opts.CoreContextTokens = 512
	}
	if opts.TargetContextTokens <= 0 {
		opts.TargetContextTokens = plan.TargetContextTokens
	}
	if opts.MaxContextTokens <= 0 {
		opts.MaxContextTokens = 6000
	}
	return opts
}

func selectWithCoverageMMR(candidates []CandidateScore, limit int, opts HybridSearchOptions, embeddings map[string][]float32) []CandidateScore {
	if limit <= 0 || len(candidates) == 0 {
		return nil
	}
	selected := make([]CandidateScore, 0, minInt(limit, len(candidates)))
	selectedIDs, coveredFacets, coveredSessions := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	for len(selected) < limit {
		best, bestScore := -1, math.Inf(-1)
		for index := range candidates {
			candidate := candidates[index]
			if _, ok := selectedIDs[candidate.MemoryID]; ok {
				continue
			}
			maxSimilarity := 0.0
			for _, prior := range selected {
				similarity := cosineSimilarity(embeddings[candidate.MemoryID], embeddings[prior.MemoryID])
				if candidate.Entry.SourceSessionID != "" && candidate.Entry.SourceSessionID == prior.Entry.SourceSessionID {
					similarity = math.Max(similarity, 0.65)
				}
				maxSimilarity = math.Max(maxSimilarity, similarity)
			}
			facetGain := candidateFacetGain(candidate, opts.CoverageFacets, coveredFacets)
			sourceGain := 0.0
			if candidate.Entry.SourceSessionID != "" {
				if _, seen := coveredSessions[candidate.Entry.SourceSessionID]; !seen {
					sourceGain = 1
				}
			}
			relevance := candidate.FusedScore * float64(maxInt(opts.RRFK, 1))
			score := opts.MMRRelevanceWeight*relevance + opts.MMRNoveltyWeight*(1-maxSimilarity) +
				opts.MMRFacetWeight*facetGain + opts.MMRSourceWeight*sourceGain
			if score > bestScore {
				best, bestScore = index, score
			}
		}
		if best < 0 {
			break
		}
		chosen := candidates[best]
		selected = append(selected, chosen)
		selectedIDs[chosen.MemoryID] = struct{}{}
		if chosen.Entry.SourceSessionID != "" {
			coveredSessions[chosen.Entry.SourceSessionID] = struct{}{}
		}
		markCandidateFacets(chosen, opts.CoverageFacets, coveredFacets)
	}
	return selected
}

func candidateFacetGain(candidate CandidateScore, facets []string, covered map[string]struct{}) float64 {
	if len(facets) == 0 {
		return 0
	}
	text := strings.ToLower(candidate.Entry.Title + " " + candidate.Entry.Summary + " " + candidate.Entry.Content + " " + strings.Join(candidate.Entry.Entities, " "))
	gain := 0
	for _, facet := range facets {
		key := strings.ToLower(strings.TrimSpace(facet))
		if key == "" {
			continue
		}
		if _, seen := covered[key]; !seen && strings.Contains(text, key) {
			gain++
		}
	}
	return float64(gain) / float64(len(facets))
}

func markCandidateFacets(candidate CandidateScore, facets []string, covered map[string]struct{}) {
	text := strings.ToLower(candidate.Entry.Title + " " + candidate.Entry.Summary + " " + candidate.Entry.Content + " " + strings.Join(candidate.Entry.Entities, " "))
	for _, facet := range facets {
		key := strings.ToLower(strings.TrimSpace(facet))
		if key != "" && strings.Contains(text, key) {
			covered[key] = struct{}{}
		}
	}
}

func candidateSlice(values map[string]*CandidateScore) []CandidateScore {
	result := make([]CandidateScore, 0, len(values))
	for _, item := range values {
		result = append(result, *item)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].FusedScore == result[j].FusedScore {
			return result[i].MemoryID < result[j].MemoryID
		}
		return result[i].FusedScore > result[j].FusedScore
	})
	return result
}

func selectWithMMR(candidates []CandidateScore, limit int, lambda float64, embeddings map[string][]float32) []CandidateScore {
	if limit <= 0 || len(candidates) == 0 {
		return nil
	}
	selected := make([]CandidateScore, 0, minInt(limit, len(candidates)))
	selectedIDs := map[string]struct{}{}
	for len(selected) < limit && len(selected) < len(candidates) {
		best := -1
		bestScore := math.Inf(-1)
		for index := range candidates {
			candidate := candidates[index]
			if _, ok := selectedIDs[candidate.MemoryID]; ok {
				continue
			}
			maxSimilarity := 0.0
			for _, prior := range selected {
				similarity := cosineSimilarity(embeddings[candidate.MemoryID], embeddings[prior.MemoryID])
				if similarity == 0 {
					similarity = lexicalSimilarity(candidate.Entry, prior.Entry)
				}
				if candidate.Entry.SourceSessionID != "" && candidate.Entry.SourceSessionID == prior.Entry.SourceSessionID {
					similarity = math.Max(similarity, 0.65)
				}
				maxSimilarity = math.Max(maxSimilarity, similarity)
			}
			score := lambda*candidate.FusedScore - (1-lambda)*maxSimilarity/float64(60)
			if score > bestScore {
				best, bestScore = index, score
			}
		}
		if best < 0 {
			break
		}
		selected = append(selected, candidates[best])
		selectedIDs[candidates[best].MemoryID] = struct{}{}
	}
	return selected
}

func cosineSimilarity(left, right []float32) float64 {
	if len(left) == 0 || len(left) != len(right) {
		return 0
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		l, r := float64(left[index]), float64(right[index])
		dot += l * r
		leftNorm += l * l
		rightNorm += r * r
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	return dot / math.Sqrt(leftNorm*rightNorm)
}

func lexicalSimilarity(left, right Entry) float64 {
	return 0
}

func documentFromEntry(entry Entry, text string) RetrievalDocument {
	return RetrievalDocument{DocumentID: entry.MemoryID, Kind: entry.DocumentKind, ParentID: entry.ParentID,
		Scope: Scope{Type: entry.ScopeType, Key: entry.ScopeKey}, SessionID: entry.SourceSessionID,
		MessageID: entry.MessageID, Role: entry.Role, Text: text, OccurredAt: entry.OccurredAt,
		ValidFrom: entry.ValidFrom, ValidUntil: entry.ValidUntil}
}

func estimateTokens(text string) int {
	runes := len([]rune(text))
	if runes == 0 {
		return 0
	}
	return maxInt(1, int(math.Ceil(float64(runes)/3.2)))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func filterExcludedCandidates(candidates []CandidateScore, excluded map[string]struct{}) []CandidateScore {
	if len(excluded) == 0 {
		return candidates
	}
	filtered := candidates[:0]
	for _, candidate := range candidates {
		if _, ok := excluded[candidate.MemoryID]; ok {
			continue
		}
		filtered = append(filtered, candidate)
	}
	return filtered
}
