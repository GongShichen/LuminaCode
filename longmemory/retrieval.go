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
	ids := make([]string, 0, len(selected))
	for _, item := range selected {
		ids = append(ids, item.MemoryID)
	}
	spans, err := s.ListEvidenceSpans(ctx, ids)
	if err != nil {
		return packet, err
	}
	target := minInt(opts.TargetContextTokens, opts.MaxContextTokens)
	if target <= 0 {
		target = opts.MaxContextTokens
	}
	for _, item := range selected {
		entry := item.Entry
		text, occurredAt, sourceMessages := bestEvidenceText(plan.Query, entry, spans[entry.MemoryID])
		evidence := Evidence{
			MemoryID: entry.MemoryID, Title: entry.Title, Text: text, ScopeType: entry.ScopeType,
			ScopeKey: entry.ScopeKey, MemoryType: entry.MemoryType, SourceSession: entry.SourceSessionID,
			SourceMessages: append([]string(nil), entry.SourceMessageIDs...), SourcePaths: append([]string(nil), entry.SourcePaths...),
			OccurredAt: occurredAt, ValidFrom: entry.ValidFrom, ValidUntil: entry.ValidUntil,
			Confidence: entry.Confidence, Score: item.FusedScore,
		}
		for _, sourceMessage := range sourceMessages {
			if sourceMessage != "" && !containsString(evidence.SourceMessages, sourceMessage) {
				evidence.SourceMessages = append(evidence.SourceMessages, sourceMessage)
			}
		}
		cost := estimateTokens(evidence.Title + " " + evidence.Text)
		if packet.EstimatedTokens+cost > target || packet.EstimatedTokens+cost > opts.MaxContextTokens {
			continue
		}
		packet.Evidence = append(packet.Evidence, evidence)
		packet.EstimatedTokens += cost
	}
	return packet, nil
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
	a := tokenSet(left.Title + " " + left.Summary + " " + strings.Join(left.Entities, " "))
	b := tokenSet(right.Title + " " + right.Summary + " " + strings.Join(right.Entities, " "))
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersection := 0
	for token := range a {
		if _, ok := b[token]; ok {
			intersection++
		}
	}
	return float64(intersection) / float64(len(a)+len(b)-intersection)
}

func tokenSet(text string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, token := range strings.Fields(strings.ToLower(text)) {
		token = strings.Trim(token, ",.;:!?，。；：！？()[]{}")
		if len([]rune(token)) >= 2 {
			result[token] = struct{}{}
		}
	}
	return result
}

func bestEvidenceText(query string, entry Entry, spans []EvidenceSpan) (string, time.Time, []string) {
	if len(spans) == 0 {
		text := strings.TrimSpace(entry.Content)
		if text == "" {
			text = entry.Summary
		}
		return clipEvidence(text, 1800), entry.ValidFrom, nil
	}
	queryTokens := tokenSet(query)
	type rankedSpan struct {
		span    EvidenceSpan
		overlap int
		tokens  int
	}
	ranked := make([]rankedSpan, 0, len(spans))
	for _, span := range spans {
		spanTokens := tokenSet(span.Text)
		overlap := 0
		for token := range spanTokens {
			if _, ok := queryTokens[token]; ok {
				overlap++
			}
		}
		ranked = append(ranked, rankedSpan{span: span, overlap: overlap, tokens: len(spanTokens)})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].overlap != ranked[j].overlap {
			return ranked[i].overlap > ranked[j].overlap
		}
		if ranked[i].tokens != ranked[j].tokens {
			return ranked[i].tokens < ranked[j].tokens
		}
		return ranked[i].span.OccurredAt.Before(ranked[j].span.OccurredAt)
	})

	const maxSpans = 3
	const maxSpanRunes = 600
	parts := make([]string, 0, maxSpans)
	messages := make([]string, 0, maxSpans)
	occurredAt := ranked[0].span.OccurredAt
	for _, item := range ranked {
		if len(parts) >= maxSpans {
			break
		}
		text := clipEvidence(item.span.Text, maxSpanRunes)
		if strings.TrimSpace(text) == "" {
			continue
		}
		parts = append(parts, text)
		messages = append(messages, item.span.MessageID)
		if occurredAt.IsZero() || (!item.span.OccurredAt.IsZero() && item.span.OccurredAt.Before(occurredAt)) {
			occurredAt = item.span.OccurredAt
		}
	}
	return strings.Join(parts, "\n\n---\n\n"), occurredAt, normalizeStrings(messages)
}

func clipEvidence(text string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes]) + "\n...[evidence truncated]"
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
