package longmemory

import (
	"context"
	"math"
	"sort"
	"strings"
	"unicode"
)

func BuildCoverageFacets(plan QueryPlan, expansion QueryExpansion, maxFacets int) []CoverageFacet {
	if maxFacets <= 0 {
		maxFacets = 8
	}
	texts := normalizeStrings(append([]string{plan.Query}, expansion.Queries...))
	if len(texts) == 0 {
		texts = []string{strings.TrimSpace(plan.Query)}
	}
	if len(texts) > maxFacets {
		texts = texts[:maxFacets]
	}
	facets := make([]CoverageFacet, 0, len(texts))
	for index, text := range texts {
		facets = append(facets, CoverageFacet{FacetID: StableID(ScopeProject, "coverage", itoa(index), text),
			Text: text, Entities: append([]string(nil), expansion.Entities...),
			Relations:     append([]string(nil), expansion.RelationTerms...),
			TemporalHints: append([]TemporalConstraint(nil), expansion.TemporalConstraints...)})
	}
	return facets
}

func BuildCoverageLedger(candidates []CandidateScore, facets []CoverageFacet, opts HybridSearchOptions, tokenBudget int) ([]CandidateScore, CoverageLedger) {
	candidates = deduplicateSourceDocuments(candidates)
	ledger := CoverageLedger{Facets: append([]CoverageFacet(nil), facets...), CoveredBy: map[string][]string{}}
	for _, facet := range facets {
		ledger.Uncovered = append(ledger.Uncovered, facet.FacetID)
	}
	if tokenBudget <= 0 {
		tokenBudget = opts.TargetContextTokens
	}
	if tokenBudget <= 0 || len(candidates) == 0 {
		return nil, ledger
	}
	limit := opts.AtomMaxSelected
	if limit <= 0 {
		limit = maxInt(opts.MaxItems, 32)
	}
	maxRelevance := 0.0
	for _, candidate := range candidates {
		maxRelevance = math.Max(maxRelevance, candidate.FusedScore)
	}
	if maxRelevance <= 0 {
		maxRelevance = 1
	}
	selected := make([]CandidateScore, 0, minInt(limit, len(candidates)))
	selectedIDs, selectedSources := map[string]struct{}{}, map[string]struct{}{}
	covered := map[string]struct{}{}
	for len(selected) < limit {
		bestIndex, bestUtility := -1, -1.0
		var bestDecision CoverageDecision
		for index, candidate := range candidates {
			if _, ok := selectedIDs[candidate.MemoryID]; ok {
				continue
			}
			cost := maxInt(1, estimateTokens(candidate.Entry.Title+" "+candidate.Entry.Content))
			if ledger.TokenUsage+cost > tokenBudget {
				continue
			}
			matched := matchingFacets(candidate.Entry, facets)
			uncoveredGain := 0.0
			for _, facetID := range matched {
				if _, ok := covered[facetID]; !ok {
					uncoveredGain++
				}
			}
			if len(facets) > 0 {
				uncoveredGain /= float64(len(facets))
			}
			sourceGain := 0.0
			sourceKey := candidate.Entry.SourceSessionID + "\x00" + candidate.Entry.MessageID
			if _, ok := selectedSources[sourceKey]; !ok && sourceKey != "\x00" {
				sourceGain = 1
			}
			novelty := 1.0
			for _, prior := range selected {
				novelty = math.Min(novelty, 1-lexicalSimilarity(candidate.Entry, prior.Entry))
			}
			provenance := epistemicStrength(candidate.Entry.EpistemicStatus)
			coherence := candidateCoherence(candidate.Entry, facets)
			relevance := candidate.FusedScore / maxRelevance
			utility := opts.CoverageRelevanceWeight*relevance + opts.CoverageFacetWeight*uncoveredGain +
				opts.CoverageProvenanceWeight*provenance + opts.CoverageSourceWeight*(0.6*sourceGain+0.4*novelty) +
				opts.CoverageCoherenceWeight*coherence
			perCost := utility / math.Sqrt(float64(cost))
			if perCost > bestUtility || (math.Abs(perCost-bestUtility) < 1e-12 && candidate.MemoryID < candidates[bestIndex].MemoryID) {
				bestIndex, bestUtility = index, perCost
				bestDecision = CoverageDecision{DocumentID: candidate.MemoryID, CoveredFacets: matched,
					Utility: utility, UtilityPerCost: perCost, EstimatedTokens: cost,
					ScoreBreakdown: map[string]float64{"relevance": relevance, "facet": uncoveredGain,
						"provenance": provenance, "source": sourceGain, "novelty": novelty, "coherence": coherence}}
			}
		}
		if bestIndex < 0 {
			break
		}
		chosen := candidates[bestIndex]
		selected = append(selected, chosen)
		selectedIDs[chosen.MemoryID] = struct{}{}
		selectedSources[chosen.Entry.SourceSessionID+"\x00"+chosen.Entry.MessageID] = struct{}{}
		ledger.Selected = append(ledger.Selected, bestDecision)
		ledger.TokenUsage += bestDecision.EstimatedTokens
		for _, facetID := range bestDecision.CoveredFacets {
			covered[facetID] = struct{}{}
			ledger.CoveredBy[facetID] = append(ledger.CoveredBy[facetID], chosen.MemoryID)
		}
	}
	ledger.Uncovered = ledger.Uncovered[:0]
	for _, facet := range facets {
		if _, ok := covered[facet.FacetID]; !ok {
			ledger.Uncovered = append(ledger.Uncovered, facet.FacetID)
		}
	}
	return selected, ledger
}

func deduplicateSourceDocuments(candidates []CandidateScore) []CandidateScore {
	best := map[string]CandidateScore{}
	for _, candidate := range candidates {
		key := candidate.Entry.MessageID + "\x00" + normalizeCoverageText(candidate.Entry.Content)
		if candidate.Entry.MessageID == "" {
			key = candidate.MemoryID
		}
		prior, exists := best[key]
		if !exists || candidate.FusedScore > prior.FusedScore {
			if exists {
				candidate.Contributions = mergeContributions(candidate.Contributions, prior.Contributions)
			}
			best[key] = candidate
		} else {
			prior.Contributions = mergeContributions(prior.Contributions, candidate.Contributions)
			best[key] = prior
		}
	}
	result := make([]CandidateScore, 0, len(best))
	for _, candidate := range best {
		result = append(result, candidate)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].FusedScore == result[j].FusedScore {
			return result[i].MemoryID < result[j].MemoryID
		}
		return result[i].FusedScore > result[j].FusedScore
	})
	return result
}

func matchingFacets(entry Entry, facets []CoverageFacet) []string {
	entryTerms := coverageTerms(entry.Title + " " + entry.Content + " " + strings.Join(entry.Entities, " "))
	var result []string
	for _, facet := range facets {
		facetTerms := coverageTerms(facet.Text + " " + strings.Join(facet.Entities, " ") + " " + strings.Join(facet.Relations, " "))
		matched := 0
		for term := range facetTerms {
			if _, ok := entryTerms[term]; ok {
				matched++
			}
		}
		threshold := 1
		if len(facetTerms) > 6 {
			threshold = 2
		}
		if matched >= threshold {
			result = append(result, facet.FacetID)
		}
	}
	return result
}

func coverageTerms(value string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, term := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '/' && r != '.'
	}) {
		if len([]rune(term)) >= 2 {
			result[term] = struct{}{}
		}
	}
	return result
}

func normalizeCoverageText(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func epistemicStrength(status string) float64 {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "reported", "observed":
		return 1
	case "derived":
		return 0.8
	case "suggested":
		return 0.5
	case "hypothetical":
		return 0.3
	case "questioned":
		return 0.25
	default:
		return 0.6
	}
}

func candidateCoherence(entry Entry, facets []CoverageFacet) float64 {
	text := strings.ToLower(entry.Content + " " + strings.Join(entry.Entities, " "))
	for _, facet := range facets {
		for _, entity := range facet.Entities {
			if entity != "" && strings.Contains(text, strings.ToLower(entity)) {
				return 1
			}
		}
		if len(facet.TemporalHints) > 0 && (!entry.OccurredAt.IsZero() || !entry.ValidFrom.IsZero()) {
			return 1
		}
	}
	return 0
}

func (s *Store) BuildAtomEvidencePacket(ctx context.Context, plan QueryPlan, selected []CandidateScore,
	ledger CoverageLedger, blocks []CoreBlock, opts HybridSearchOptions) (EvidencePacket, error) {
	packet := EvidencePacket{Plan: plan, ReferenceTime: opts.ReferenceTime, SourceCoverage: map[string]int{}}
	coreBudget := minInt(opts.CoreContextTokens, opts.MaxContextTokens)
	for _, block := range blocks {
		cost := estimateTokens(block.Label + " " + block.Content)
		if packet.EstimatedTokens+cost <= coreBudget {
			packet.CoreBlocks = append(packet.CoreBlocks, block)
			packet.EstimatedTokens += cost
		}
	}
	target := minInt(opts.TargetContextTokens, opts.MaxContextTokens)
	if target <= 0 {
		target = opts.MaxContextTokens
	}
	directBudget := int(float64(target) * (opts.EvidencePrimaryBudgetRatio + opts.EvidenceCompletionBudgetRatio))
	if directBudget <= 0 {
		directBudget = target
	}
	atomIDs := make([]string, 0, len(selected))
	for _, candidate := range selected {
		if candidate.Entry.DocumentKind == "atom" {
			atomIDs = append(atomIDs, candidate.MemoryID)
		}
	}
	atoms, err := s.GetAtoms(ctx, atomIDs)
	if err != nil {
		return packet, err
	}
	atomByID := map[string]EvidenceAtom{}
	for _, atom := range atoms {
		atomByID[atom.AtomID] = atom
	}
	seen := map[string]struct{}{}
	selectedAtomIDs := map[string]struct{}{}
	type evidenceSeed struct {
		entry       Entry
		text        string
		documentIDs []string
		score       float64
	}
	seeds := make([]evidenceSeed, 0, len(selected))
	messageSeed := map[string]int{}
	for _, candidate := range selected {
		atom, isAtom := atomByID[candidate.MemoryID]
		if !isAtom {
			seeds = append(seeds, evidenceSeed{entry: candidate.Entry, text: strings.TrimSpace(candidate.Entry.Content),
				documentIDs: []string{candidate.MemoryID}, score: candidate.FusedScore})
			continue
		}
		selectedAtomIDs[atom.AtomID] = struct{}{}
		key := atom.SessionID + "\x00" + atom.MessageID
		if seedIndex, ok := messageSeed[key]; ok {
			seed := &seeds[seedIndex]
			seed.documentIDs = append(seed.documentIDs, atom.AtomID)
			seed.score = math.Max(seed.score, candidate.FusedScore)
			continue
		}
		messageSeed[key] = len(seeds)
		seeds = append(seeds, evidenceSeed{entry: atomEntry(atom, candidate.FusedScore, "coverage"),
			text: atom.Text, documentIDs: []string{atom.AtomID}, score: candidate.FusedScore})
	}
	for seedIndex := range seeds {
		seed := &seeds[seedIndex]
		if len(seed.documentIDs) <= 1 || seed.entry.DocumentKind != "atom" {
			continue
		}
		group := make([]EvidenceAtom, 0, len(seed.documentIDs))
		for _, atomID := range seed.documentIDs {
			if atom, ok := atomByID[atomID]; ok {
				group = append(group, atom)
			}
		}
		sort.SliceStable(group, func(i, j int) bool { return group[i].StartRune < group[j].StartRune })
		parts := make([]string, 0, len(group))
		seed.documentIDs = seed.documentIDs[:0]
		for _, atom := range group {
			parts = append(parts, strings.TrimSpace(atom.Text))
			seed.documentIDs = append(seed.documentIDs, atom.AtomID)
		}
		seed.text = strings.Join(parts, "\n")
		seed.entry = atomEntry(group[0], seed.score, "coverage")
	}
	for _, seed := range seeds {
		entry, text, documentIDs := seed.entry, seed.text, seed.documentIDs
		if text == "" {
			continue
		}
		cost := estimateTokens(entry.Title + " " + text)
		if packet.EstimatedTokens+cost > minInt(coreBudget+directBudget, opts.MaxContextTokens) {
			continue
		}
		key := entry.MessageID + "\x00" + normalizeCoverageText(text)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		evidence := Evidence{DocumentID: entry.MemoryID, DocumentKind: entry.DocumentKind, ParentID: entry.ParentID,
			MemoryID: entry.MemoryID, DocumentIDs: documentIDs, Title: entry.Title, Text: text,
			ScopeType: entry.ScopeType, ScopeKey: entry.ScopeKey, MemoryType: entry.MemoryType,
			SourceSession: entry.SourceSessionID, SourceMessages: append([]string(nil), entry.SourceMessageIDs...),
			OccurredAt: entry.OccurredAt, ValidFrom: entry.ValidFrom, ValidUntil: entry.ValidUntil,
			Confidence: entry.Confidence, Score: seed.score,
			Metadata: map[string]any{"role": entry.Role, "epistemic_status": entry.EpistemicStatus}}
		packet.Evidence = append(packet.Evidence, evidence)
		packet.Documents = append(packet.Documents, documentFromEntry(entry, text))
		packet.SourceCoverage[entry.SourceSessionID]++
		packet.EstimatedTokens += cost
	}
	contextBudget := int(float64(target) * opts.EvidenceContextBudgetRatio)
	contextUsed := 0
	if contextBudget > 0 && opts.NeighborChunks > 0 {
		for _, candidate := range selected {
			atom, ok := atomByID[candidate.MemoryID]
			if !ok {
				continue
			}
			neighbors, neighborErr := s.NeighborAtoms(ctx, atom, opts.NeighborChunks)
			if neighborErr != nil {
				packet.Warnings = append(packet.Warnings, "neighbor atoms: "+neighborErr.Error())
				continue
			}
			for _, neighbor := range neighbors {
				if _, selected := selectedAtomIDs[neighbor.AtomID]; selected {
					continue
				}
				key := neighbor.MessageID + "\x00" + normalizeCoverageText(neighbor.Text)
				if _, used := seen[key]; used {
					continue
				}
				cost := estimateTokens(neighbor.Text)
				if contextUsed+cost > contextBudget || packet.EstimatedTokens+cost > minInt(coreBudget+target, opts.MaxContextTokens) {
					continue
				}
				seen[key] = struct{}{}
				entry := atomEntry(neighbor, candidate.FusedScore*0.5, "neighbor_context")
				packet.Evidence = append(packet.Evidence, Evidence{DocumentID: neighbor.AtomID,
					DocumentKind: "atom_context", ParentID: neighbor.ChunkID, MemoryID: neighbor.AtomID,
					DocumentIDs: []string{neighbor.AtomID}, Title: entry.Title, Text: neighbor.Text,
					ScopeType: neighbor.ScopeType, ScopeKey: neighbor.ScopeKey, MemoryType: TypeEpisodic,
					SourceSession: neighbor.SessionID, SourceMessages: []string{neighbor.MessageID},
					OccurredAt: neighbor.OccurredAt, ValidFrom: neighbor.ValidFrom, ValidUntil: neighbor.ValidUntil,
					Score:    candidate.FusedScore * 0.5,
					Metadata: map[string]any{"role": neighbor.Role, "epistemic_status": neighbor.EpistemicStatus, "context_only": true}})
				packet.Documents = append(packet.Documents, documentFromEntry(entry, neighbor.Text))
				packet.SourceCoverage[neighbor.SessionID]++
				packet.EstimatedTokens += cost
				contextUsed += cost
			}
		}
	}
	packet.Timeline = timelineFromDocuments(packet.Documents)
	if len(ledger.Uncovered) > 0 {
		packet.Warnings = append(packet.Warnings, "coverage ledger retained "+itoa(len(ledger.Uncovered))+" uncovered facet(s)")
	}
	return packet, nil
}

func mergeContributions(left, right []SignalContribution) []SignalContribution {
	best := map[string]SignalContribution{}
	for _, item := range append(append([]SignalContribution(nil), left...), right...) {
		key := item.SignalFamily + "\x00" + item.Channel
		if prior, ok := best[key]; !ok || item.Rank < prior.Rank {
			best[key] = item
		}
	}
	result := make([]SignalContribution, 0, len(best))
	for _, item := range best {
		result = append(result, item)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].SignalFamily == result[j].SignalFamily {
			return result[i].Channel < result[j].Channel
		}
		return result[i].SignalFamily < result[j].SignalFamily
	})
	return result
}
