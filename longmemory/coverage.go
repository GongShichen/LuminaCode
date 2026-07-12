package longmemory

import (
	"context"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

func BuildCoverageFacets(plan QueryPlan, expansion QueryExpansion, maxFacets int) []CoverageFacet {
	if maxFacets <= 0 {
		maxFacets = 8
	}
	validGlobalTemporal := effectiveTemporalConstraints(expansion.TemporalConstraints)
	facets := make([]CoverageFacet, 0, len(expansion.Facets)+1)
	if len(expansion.Facets) > 0 {
		facets = append(facets, CoverageFacet{Text: strings.TrimSpace(plan.Query),
			Entities:      anchorsPresentInText(plan.Query, expansion.Entities),
			Relations:     anchorsPresentInText(plan.Query, expansion.RelationTerms),
			TemporalHints: append([]TemporalConstraint(nil), validGlobalTemporal...), Required: true})
	}
	for _, draft := range expansion.Facets {
		facets = append(facets, CoverageFacet{Text: strings.TrimSpace(draft.Text),
			Entities:      append([]string(nil), draft.Entities...),
			Relations:     append([]string(nil), draft.Relations...),
			TemporalHints: append([]TemporalConstraint(nil), effectiveTemporalConstraints(draft.TemporalConstraints)...),
			Required:      true})
	}
	if len(facets) == 0 {
		facets = append(facets, CoverageFacet{Text: strings.TrimSpace(plan.Query),
			Entities: append([]string(nil), expansion.Entities...), Relations: append([]string(nil), expansion.RelationTerms...),
			TemporalHints: append([]TemporalConstraint(nil), validGlobalTemporal...), Required: true})
	}
	seen := map[string]struct{}{}
	result := make([]CoverageFacet, 0, minInt(maxFacets, len(facets)))
	for _, facet := range facets {
		key := normalizeCoverageText(facet.Text)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		facet.FacetID = StableID(ScopeProject, "coverage", itoa(len(result)), key)
		result = append(result, facet)
		if len(result) >= maxFacets {
			break
		}
	}
	facets = result
	return facets
}

func BuildCoverageLedger(candidates []CandidateScore, facets []CoverageFacet, opts HybridSearchOptions, tokenBudget int) ([]CandidateScore, CoverageLedger) {
	candidates = deduplicateSourceDocuments(candidates)
	supportTarget := opts.CoverageSupportTarget
	if supportTarget <= 0 {
		supportTarget = 0.82
	}
	ledger := CoverageLedger{Facets: append([]CoverageFacet(nil), facets...), CoveredBy: map[string][]string{},
		FacetStates: map[string]FacetCoverageState{}, StopReason: "candidate_exhausted"}
	explicitRequirements := false
	for _, facet := range facets {
		explicitRequirements = explicitRequirements || facet.Required
	}
	for _, facet := range facets {
		required := facet.Required || !explicitRequirements
		if required {
			ledger.Uncovered = append(ledger.Uncovered, facet.FacetID)
		}
		ledger.FacetStates[facet.FacetID] = FacetCoverageState{FacetID: facet.FacetID,
			Required: required, RemainingNeed: 1, Sources: map[string]float64{}, AnchorSupport: map[string]float64{},
			ObservableAnchors: observableFacetAnchors(facet, candidates)}
	}
	if tokenBudget <= 0 {
		tokenBudget = opts.TargetContextTokens
	}
	if tokenBudget <= 0 || len(candidates) == 0 {
		ledger.StopReason = "no_candidates_or_budget"
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
	selectedIDs, selectedSources, selectedSessions := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	for len(selected) < limit {
		if coverageTargetReached(ledger.FacetStates, supportTarget) {
			ledger.StopReason = "support_target_reached"
			break
		}
		bestIndex, bestUtility := -1, -1.0
		hasUnselected, hasWithinBudget, hasIndependentSupport := false, false, false
		var bestDecision CoverageDecision
		for index, candidate := range candidates {
			if _, ok := selectedIDs[candidate.MemoryID]; ok {
				continue
			}
			hasUnselected = true
			cost := maxInt(1, estimateTokens(candidate.Entry.Title+" "+candidate.Entry.Content))
			if ledger.TokenUsage+cost > tokenBudget {
				continue
			}
			hasWithinBudget = true
			supports, marginalGain := candidateFacetSupports(candidate, facets, ledger.FacetStates, maxRelevance, opts)
			// Retrieval is recall-first: an absolute marginal threshold becomes
			// progressively harder to satisfy as probabilistic support accumulates.
			// Continue while a candidate adds any independent support and let the
			// explicit evidence budgets bound the work.
			if marginalGain <= 0 {
				continue
			}
			hasIndependentSupport = true
			sourceGain := 0.0
			sourceKey := coverageSourceGroup(candidate.Entry)
			if _, seenSession := selectedSessions[candidate.Entry.SourceSessionID]; !seenSession && candidate.Entry.SourceSessionID != "" {
				sourceGain = 1
			} else if _, seenSource := selectedSources[sourceKey]; !seenSource && sourceKey != "\x00" {
				// Another message in the same Session is useful, but it is not an
				// independent Session-level perspective.
				sourceGain = 0.25
			}
			provenance := epistemicStrength(candidate.Entry.EpistemicStatus)
			coherence := candidateCoherence(candidate.Entry, facets)
			relevance := candidate.FusedScore / maxRelevance
			utility := opts.CoverageRelevanceWeight*relevance + opts.CoverageFacetWeight*marginalGain +
				opts.CoverageProvenanceWeight*provenance + opts.CoverageSourceWeight*sourceGain +
				opts.CoverageCoherenceWeight*coherence
			targetAtomTokens := opts.AtomTargetTokens
			if targetAtomTokens <= 0 {
				targetAtomTokens = defaultAtomTargetTokens
			}
			// Token budget already enforces absolute packet size. Normalize the
			// marginal cost by the intended atom size so complete assertions are
			// not displaced by tiny topical fragments solely because they are a
			// sentence longer.
			normalizedCost := math.Max(1, float64(cost)/float64(targetAtomTokens))
			perCost := utility / math.Sqrt(normalizedCost)
			if perCost > bestUtility || (math.Abs(perCost-bestUtility) < 1e-12 && candidate.MemoryID < candidates[bestIndex].MemoryID) {
				bestIndex, bestUtility = index, perCost
				coveredFacets := make([]string, 0, len(supports))
				for _, support := range supports {
					if support.Support > 0 {
						coveredFacets = append(coveredFacets, support.FacetID)
					}
				}
				bestDecision = CoverageDecision{DocumentID: candidate.MemoryID, CoveredFacets: coveredFacets,
					Utility: utility, UtilityPerCost: perCost, EstimatedTokens: cost, MarginalGain: marginalGain,
					Supports: supports, ScoreBreakdown: map[string]float64{"relevance": relevance,
						"facet": marginalGain, "provenance": provenance, "source": sourceGain, "coherence": coherence}}
			}
		}
		if bestIndex < 0 {
			switch {
			case !hasUnselected:
				ledger.StopReason = "candidate_exhausted"
			case !hasWithinBudget:
				ledger.StopReason = "token_budget_reached"
			case !hasIndependentSupport:
				ledger.StopReason = "no_independent_support"
			default:
				ledger.StopReason = "candidate_exhausted"
			}
			break
		}
		chosen := candidates[bestIndex]
		selected = append(selected, chosen)
		selectedIDs[chosen.MemoryID] = struct{}{}
		selectedSources[coverageSourceGroup(chosen.Entry)] = struct{}{}
		if chosen.Entry.SourceSessionID != "" {
			selectedSessions[chosen.Entry.SourceSessionID] = struct{}{}
		}
		ledger.Selected = append(ledger.Selected, bestDecision)
		ledger.TokenUsage += bestDecision.EstimatedTokens
		for _, support := range bestDecision.Supports {
			state := ledger.FacetStates[support.FacetID]
			if support.Support > state.Sources[support.SourceGroup] {
				state.Sources[support.SourceGroup] = support.Support
			}
			for _, anchor := range support.AnchorHits {
				if support.AnchorSupport > state.AnchorSupport[anchor] {
					state.AnchorSupport[anchor] = support.AnchorSupport
				}
			}
			facet := coverageFacetByID(facets, support.FacetID)
			state.SourceMass = supportMass(state.Sources)
			state.AnchorCoverage = facetAnchorCoverage(facet, state.AnchorSupport, state.ObservableAnchors)
			state.SupportMass = combinedCoverageSupport(state.SourceMass, state.AnchorCoverage, len(state.ObservableAnchors) > 0)
			state.RemainingNeed = math.Max(0, 1-state.SupportMass)
			state.EvidenceIDs = append(state.EvidenceIDs, chosen.MemoryID)
			ledger.FacetStates[support.FacetID] = state
			ledger.CoveredBy[support.FacetID] = append(ledger.CoveredBy[support.FacetID], chosen.MemoryID)
		}
	}
	if len(selected) >= limit && !coverageTargetReached(ledger.FacetStates, supportTarget) {
		ledger.StopReason = "atom_limit_reached"
	}
	selected = appendCorroboratedEvidenceReserve(candidates, selected, &ledger, opts, tokenBudget, limit)
	ledger.Uncovered = ledger.Uncovered[:0]
	for _, facet := range facets {
		state := ledger.FacetStates[facet.FacetID]
		if state.Required && state.SupportMass < supportTarget {
			ledger.Uncovered = append(ledger.Uncovered, facet.FacetID)
		}
	}
	return selected, ledger
}

// appendCorroboratedEvidenceReserve uses otherwise idle packet budget for
// candidates independently supported by multiple retrieval signal families.
// Coverage probabilities remain unchanged: reserve evidence improves recall
// without falsely claiming that another information need was satisfied.
func appendCorroboratedEvidenceReserve(candidates, selected []CandidateScore, ledger *CoverageLedger,
	opts HybridSearchOptions, tokenBudget, limit int) []CandidateScore {
	if ledger == nil || len(selected) >= limit || ledger.TokenUsage >= tokenBudget {
		return selected
	}
	selectedIDs := make(map[string]struct{}, len(selected))
	selectedSources := make(map[string]struct{}, len(selected))
	for _, candidate := range selected {
		selectedIDs[candidate.MemoryID] = struct{}{}
		selectedSources[coverageSourceGroup(candidate.Entry)] = struct{}{}
	}
	maxRelevance := 0.0
	for _, candidate := range candidates {
		maxRelevance = math.Max(maxRelevance, candidate.FusedScore)
	}
	if maxRelevance <= 0 {
		maxRelevance = 1
	}
	for _, candidate := range candidates {
		if len(selected) >= limit {
			break
		}
		if _, exists := selectedIDs[candidate.MemoryID]; exists || !hasCorroboratedNativeSignals(candidate) {
			continue
		}
		source := coverageSourceGroup(candidate.Entry)
		if _, duplicate := selectedSources[source]; duplicate {
			continue
		}
		cost := maxInt(1, estimateTokens(candidate.Entry.Title+" "+candidate.Entry.Content))
		if ledger.TokenUsage+cost > tokenBudget {
			continue
		}
		selected = append(selected, candidate)
		selectedIDs[candidate.MemoryID] = struct{}{}
		selectedSources[source] = struct{}{}
		ledger.TokenUsage += cost
		ledger.Selected = append(ledger.Selected, CoverageDecision{DocumentID: candidate.MemoryID,
			Utility: candidate.FusedScore / maxRelevance, UtilityPerCost: candidate.FusedScore / maxRelevance,
			EstimatedTokens: cost, ScoreBreakdown: map[string]float64{"reserve": 1,
				"relevance": candidate.FusedScore / maxRelevance}})
	}
	return selected
}

func hasCorroboratedNativeSignals(candidate CandidateScore) bool {
	families := map[string]struct{}{}
	for _, contribution := range candidate.Contributions {
		if !contribution.Native || strings.TrimSpace(contribution.SignalFamily) == "" {
			continue
		}
		families[contribution.SignalFamily] = struct{}{}
	}
	return len(families) >= 2
}

func anchorsPresentInText(text string, anchors []string) []string {
	terms := coverageTerms(text)
	result := make([]string, 0, len(anchors))
	for _, anchor := range anchors {
		anchorTerms := coverageTerms(anchor)
		if len(anchorTerms) == 0 {
			continue
		}
		present := true
		for term := range anchorTerms {
			if _, ok := terms[term]; !ok {
				present = false
				break
			}
		}
		if present {
			result = append(result, anchor)
		}
	}
	return result
}

func candidateFacetSupports(candidate CandidateScore, facets []CoverageFacet,
	states map[string]FacetCoverageState, maxRelevance float64, opts HybridSearchOptions) ([]FacetSupport, float64) {
	supportTarget := opts.CoverageSupportTarget
	if supportTarget <= 0 {
		supportTarget = 0.82
	}
	relevance := candidate.FusedScore / maxRelevance
	provenance := epistemicStrength(candidate.Entry.EpistemicStatus)
	families := uniqueSignalFamilies(candidate.Contributions)
	diversity := math.Min(1, float64(len(families))/6)
	supports := make([]FacetSupport, 0, len(facets))
	totalGain := 0.0
	for _, facet := range facets {
		alignment := facetAlignment(candidate.Entry, facet)
		temporal := facetTemporalCoherence(candidate.Entry, facet)
		quality := 0.50*alignment + 0.20*diversity + 0.20*provenance + 0.10*temporal
		// A topically similar document is not necessarily evidence for this
		// particular facet. Keep a small semantic contribution for paraphrases,
		// while requiring anchor alignment for strong accumulated support.
		supportValue := relevance * quality * (0.25 + 0.75*alignment)
		supportValue = math.Max(0, math.Min(1, supportValue))
		state := states[facet.FacetID]
		anchorHits := matchingFacetAnchors(candidate.Entry, facet)
		sourceGroup := coverageAssertionGroup(candidate.Entry, facet, anchorHits)
		before := state.SupportMass
		updated := cloneSupportSources(state.Sources)
		if supportValue > updated[sourceGroup] {
			updated[sourceGroup] = supportValue
		}
		updatedAnchors := cloneSupportSources(state.AnchorSupport)
		// Exact anchors from stronger provenance should be able to improve an
		// existing weaker paraphrase. Otherwise an early suggestion or
		// acknowledgement can permanently crowd out the original observation.
		anchorSupport := math.Max(supportValue, provenance*(0.4+0.6*alignment))
		for _, anchor := range anchorHits {
			if anchorSupport > updatedAnchors[anchor] {
				updatedAnchors[anchor] = anchorSupport
			}
		}
		after := combinedCoverageSupport(supportMass(updated),
			facetAnchorCoverage(facet, updatedAnchors, state.ObservableAnchors), len(state.ObservableAnchors) > 0)
		gain := math.Max(0, after-before)
		if before >= supportTarget {
			gain = 0
		} else {
			gain = math.Min(gain, supportTarget-before)
		}
		totalGain += gain
		supports = append(supports, FacetSupport{FacetID: facet.FacetID, DocumentID: candidate.MemoryID,
			Relevance: relevance, AnchorAlignment: alignment, SignalDiversity: diversity,
			Provenance: provenance, TemporalCoherence: temporal, SourceGroup: sourceGroup,
			SignalFamilies: families, AnchorHits: anchorHits, AnchorSupport: anchorSupport, Support: supportValue})
	}
	// Facets are independent information needs. Averaging by facet count makes
	// one missing need progressively cheaper as a query becomes more complex.
	return supports, math.Min(1, totalGain)
}

func coverageFacetByID(facets []CoverageFacet, facetID string) CoverageFacet {
	for _, facet := range facets {
		if facet.FacetID == facetID {
			return facet
		}
	}
	return CoverageFacet{FacetID: facetID}
}

func matchingFacetAnchors(entry Entry, facet CoverageFacet) []string {
	entryTerms := coverageTerms(entry.Title + " " + entry.Content + " " + strings.Join(entry.Entities, " "))
	anchors := facetAnchorTerms(facet)
	var hits []string
	for anchor := range anchors {
		if _, ok := entryTerms[anchor]; ok {
			hits = append(hits, anchor)
		}
	}
	sort.Strings(hits)
	return hits
}

func facetAnchorTerms(facet CoverageFacet) map[string]struct{} {
	if !facet.Required {
		return coverageTerms(facet.Text + " " + strings.Join(facet.Entities, " ") + " " + strings.Join(facet.Relations, " "))
	}
	explicit := coverageTerms(strings.Join(facet.Entities, " ") + " " + strings.Join(facet.Relations, " "))
	if len(explicit) > 0 {
		return explicit
	}
	return coverageTerms(facet.Text)
}

func observableFacetAnchors(facet CoverageFacet, candidates []CandidateScore) []string {
	anchors := facetAnchorTerms(facet)
	observed := map[string]struct{}{}
	for _, candidate := range candidates {
		entryTerms := coverageTerms(candidate.Entry.Title + " " + candidate.Entry.Content + " " + strings.Join(candidate.Entry.Entities, " "))
		for anchor := range anchors {
			if _, ok := entryTerms[anchor]; ok {
				observed[anchor] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(observed))
	for anchor := range observed {
		result = append(result, anchor)
	}
	sort.Strings(result)
	return result
}

func facetAnchorCoverage(_ CoverageFacet, values map[string]float64, observable []string) float64 {
	if len(observable) == 0 {
		return 0
	}
	anchors := make(map[string]struct{}, len(observable))
	for _, anchor := range observable {
		anchors[anchor] = struct{}{}
	}
	if len(anchors) == 0 {
		return 0
	}
	total := 0.0
	for anchor := range anchors {
		total += math.Max(0, math.Min(1, values[anchor]))
	}
	return total / float64(len(anchors))
}

func combinedCoverageSupport(sourceMass, anchorCoverage float64, hasObservableAnchors bool) float64 {
	if !hasObservableAnchors {
		return sourceMass
	}
	return 0.7*sourceMass + 0.3*anchorCoverage
}

func facetAlignment(entry Entry, facet CoverageFacet) float64 {
	entryText := strings.ToLower(entry.Title + " " + entry.Content + " " + strings.Join(entry.Entities, " "))
	entryTerms := coverageTerms(entryText)
	facetTerms := coverageTerms(facet.Text)
	matched := 0
	for term := range facetTerms {
		if _, ok := entryTerms[term]; ok {
			matched++
		}
	}
	termScore := 0.0
	if len(facetTerms) > 0 {
		termScore = math.Min(1, float64(matched)/math.Min(4, float64(len(facetTerms))))
	}
	anchors, anchorHits := 0, 0
	for _, anchor := range append(append([]string(nil), facet.Entities...), facet.Relations...) {
		anchor = strings.ToLower(strings.TrimSpace(anchor))
		if anchor == "" {
			continue
		}
		anchors++
		if strings.Contains(entryText, anchor) {
			anchorHits++
		}
	}
	anchorScore := 0.0
	if anchors > 0 {
		anchorScore = float64(anchorHits) / float64(anchors)
	}
	return math.Max(termScore, anchorScore)
}

func facetTemporalCoherence(entry Entry, facet CoverageFacet) float64 {
	if len(facet.TemporalHints) == 0 {
		return 0.5
	}
	if entry.OccurredAt.IsZero() && entry.ValidFrom.IsZero() {
		return 0
	}
	return 1
}

func coverageSourceGroup(entry Entry) string {
	if entry.MessageID != "" {
		return entry.SourceSessionID + "\x00" + entry.MessageID
	}
	if entry.ParentID != "" {
		return entry.SourceSessionID + "\x00" + entry.ParentID
	}
	return entry.SourceSessionID + "\x00" + entry.MemoryID
}

var (
	coverageQuotedValue = regexp.MustCompile(`["“]([^"”]{1,96})["”]`)
	coverageNumberValue = regexp.MustCompile(`\b\d+(?:[./:-]\d+)*\b`)
)

func coverageAssertionGroup(entry Entry, facet CoverageFacet, anchorHits []string) string {
	if len(anchorHits) == 0 {
		return facet.FacetID + "\x00unanchored"
	}
	parts := append([]string(nil), anchorHits...)
	sort.Strings(parts)
	entryText := entry.Title + " " + entry.Content + " " + strings.Join(entry.Entities, " ")
	entryTerms := coverageTerms(entryText)
	direct := !facet.Required || matchesEveryCoveragePhrase(entryTerms,
		append(append([]string(nil), facet.Entities...), facet.Relations...))
	if direct {
		values := coverageAssertionValues(entry)
		parts = append(parts, values...)
	}
	return facet.FacetID + "\x00" + strings.Join(normalizeStrings(parts), "|")
}

func matchesEveryCoveragePhrase(entryTerms map[string]struct{}, phrases []string) bool {
	for _, phrase := range phrases {
		terms := coverageTerms(phrase)
		if len(terms) == 0 {
			continue
		}
		matched := false
		for term := range terms {
			if _, ok := entryTerms[term]; ok {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func coverageAssertionValues(entry Entry) []string {
	values := append([]string(nil), entry.Entities...)
	text := entry.Title + " " + entry.Content
	for _, match := range coverageQuotedValue.FindAllStringSubmatch(text, -1) {
		if len(match) > 1 {
			values = append(values, normalizeCoverageText(match[1]))
		}
	}
	values = append(values, coverageNumberValue.FindAllString(strings.ToLower(text), -1)...)
	values = normalizeStrings(values)
	if len(values) > 8 {
		values = values[:8]
	}
	return values
}

func uniqueSignalFamilies(contributions []SignalContribution) []string {
	seen := map[string]struct{}{}
	for _, contribution := range contributions {
		if contribution.SignalFamily != "" && contribution.Native {
			seen[contribution.SignalFamily] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for family := range seen {
		result = append(result, family)
	}
	sort.Strings(result)
	return result
}

func cloneSupportSources(values map[string]float64) map[string]float64 {
	result := make(map[string]float64, len(values)+1)
	for key, value := range values {
		result[key] = value
	}
	return result
}

func supportMass(sources map[string]float64) float64 {
	remaining := 1.0
	for _, support := range sources {
		remaining *= 1 - math.Max(0, math.Min(1, support))
	}
	return 1 - remaining
}

func coverageTargetReached(states map[string]FacetCoverageState, target float64) bool {
	if len(states) == 0 {
		return false
	}
	required := 0
	for _, state := range states {
		if !state.Required {
			continue
		}
		required++
		if state.SupportMass < target {
			return false
		}
	}
	return required > 0
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

func coverageTerms(value string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, term := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '/' && r != '.'
	}) {
		term = strings.Trim(term, ".")
		if len([]rune(term)) >= 2 && !isRetrievalStopword(term) {
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
	packet := EvidencePacket{Plan: plan, ReferenceTime: opts.ReferenceTime, SourceCoverage: map[string]int{},
		Facets: append([]CoverageFacet(nil), ledger.Facets...), Coverage: ledger}
	coreBudget := minInt(opts.CoreContextTokens, opts.MaxContextTokens)
	for _, block := range blocks {
		cost := estimateTokens(block.Label + " " + block.Content)
		if packet.EstimatedTokens+cost <= coreBudget {
			packet.CoreBlocks = append(packet.CoreBlocks, block)
			packet.Guidance = append(packet.Guidance, block)
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
		containerKey := atom.ContainerID
		if containerKey == "" {
			containerKey = atom.AtomID
		}
		key := atom.SessionID + "\x00" + atom.MessageID + "\x00" + containerKey
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
	sort.SliceStable(seeds, func(i, j int) bool {
		if seeds[i].score == seeds[j].score {
			return seeds[i].entry.MemoryID < seeds[j].entry.MemoryID
		}
		return seeds[i].score > seeds[j].score
	})
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
		if atom, ok := atomByID[documentIDs[0]]; ok {
			evidence.Metadata["sequence_no"] = atom.SequenceNo
			evidence.Metadata["container_id"] = atom.ContainerID
			evidence.Metadata["container_kind"] = atom.ContainerKind
			evidence.Metadata["container_ordinal"] = atom.ContainerOrdinal
			evidence.Metadata["heading_path"] = atom.HeadingPath
		}
		packet.Evidence = append(packet.Evidence, evidence)
		packet.Bundles = append(packet.Bundles, evidenceBundle(evidence, ledger))
		packet.Documents = append(packet.Documents, documentFromEntry(entry, text))
		packet.SourceCoverage[entry.SourceSessionID]++
		packet.EstimatedTokens += cost
	}
	contextBudget := int(float64(target) * opts.EvidenceContextBudgetRatio)
	if !opts.StructuralContextEnabled {
		contextBudget = 0
	} else if opts.StructuralContextTokens > 0 {
		contextBudget = minInt(contextBudget, opts.StructuralContextTokens)
	}
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
				for index := range packet.Bundles {
					if packet.Bundles[index].SessionID == neighbor.SessionID && packet.Bundles[index].MessageID == neighbor.MessageID {
						packet.Bundles[index].ContextAtomIDs = append(packet.Bundles[index].ContextAtomIDs, neighbor.AtomID)
						break
					}
				}
				packet.Documents = append(packet.Documents, documentFromEntry(entry, neighbor.Text))
				packet.SourceCoverage[neighbor.SessionID]++
				packet.EstimatedTokens += cost
				contextUsed += cost
			}
		}
	}
	packet.Timeline = timelineFromDocuments(packet.Documents)
	assertions, assertionErr := s.BuildAssertionViews(ctx, selected, opts.ReferenceTime)
	if assertionErr != nil {
		packet.Warnings = append(packet.Warnings, "assertion register: "+assertionErr.Error())
	} else {
		packet.Assertions = assertions
	}
	if len(ledger.Uncovered) > 0 {
		packet.Warnings = append(packet.Warnings, "coverage ledger retained "+itoa(len(ledger.Uncovered))+" uncovered facet(s)")
	}
	return packet, nil
}

func evidenceBundle(evidence Evidence, ledger CoverageLedger) EvidenceBundle {
	var facetIDs []string
	for _, decision := range ledger.Selected {
		if decision.DocumentID == evidence.DocumentID || containsString(evidence.DocumentIDs, decision.DocumentID) {
			facetIDs = append(facetIDs, decision.CoveredFacets...)
		}
	}
	path, _ := evidence.Metadata["heading_path"].([]string)
	return EvidenceBundle{BundleID: StableID(evidence.ScopeType, evidence.ScopeKey, "evidence-bundle", evidence.MemoryID),
		FacetIDs: normalizeStrings(facetIDs), SeedAtomIDs: append([]string(nil), evidence.DocumentIDs...),
		SessionID: evidence.SourceSession, MessageID: firstString(evidence.SourceMessages),
		StructuralPath: path, Text: evidence.Text, Role: stringFromMetadata(evidence.Metadata, "role"),
		EpistemicStatus: stringFromMetadata(evidence.Metadata, "epistemic_status"), OccurredAt: evidence.OccurredAt}
}

func stringFromMetadata(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
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
