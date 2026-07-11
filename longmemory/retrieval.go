package longmemory

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"
)

type HybridSearchOptions struct {
	FTSCandidates       int
	VectorCandidates    int
	GraphCandidates     int
	GraphMaxHops        int
	RRFK                int
	MMRLambda           float64
	MaxItems            int
	CoreContextTokens   int
	TargetContextTokens int
	MaxContextTokens    int
	LocalTimeout        time.Duration
	SessionID           string
	TeamSessionID       string
	AgentID             string
	ExcludeIDs          map[string]struct{}
	ExpansionModel      string
	ExpansionError      string
	NeighborChunks      int
}

func (s *Store) BuildEvidencePacket(ctx context.Context, plan QueryPlan, selected []CandidateScore, blocks []CoreBlock, opts HybridSearchOptions) (EvidencePacket, error) {
	packet := EvidencePacket{Plan: plan}
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
	return packet, nil
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
