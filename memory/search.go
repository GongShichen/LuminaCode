package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/viant/sqlite-vec/vector"
)

const searchRRFK = 60

type queryAnalysis struct {
	tokens       []string
	literalTerms []string
	weights      map[string]float64
	frequencies  map[string]int
}

type searchDocument struct {
	ID             string
	Space          string
	ResourceKind   string
	ResourceID     string
	Content        string
	KeysText       string
	ContextID      string
	Actor          string
	OccurredAt     time.Time
	SlotID         string
	Status         SemanticStatus
	SourceEventIDs []string
	LedgerSeq      int64
}

type rankedCandidate struct {
	document          searchDocument
	contextEvents     []contextExpansionEvent
	ranks             map[string]int
	channelScores     map[string]float64
	score             float64
	maxSimScore       float64
	queryTokenScores  []float64
	queryTokenWeights []float64
	coverage          float64
	graphComponent    string
	matchPosition     int
	spanTokens        int
	reasons           []string
	stateOnly         bool
}

func (f *Fabric) Search(ctx context.Context, request SearchRequest) (SearchResult, error) {
	started := time.Now()
	result := SearchResult{}
	if f == nil || f.ledger == nil || f.index == nil {
		return result, errors.New("memory fabric is closed")
	}
	request.Space = normalizeSpace(request.Space)
	request.Query = strings.TrimSpace(request.Query)
	if request.Query == "" {
		return result, errors.New("memory search query is required")
	}
	if request.ReferenceTime.IsZero() {
		request.ReferenceTime = f.now()
	} else {
		request.ReferenceTime = request.ReferenceTime.UTC()
	}
	if request.MaxEvidence <= 0 {
		request.MaxEvidence = f.options.MaxEvidence
	}
	if request.MaxContextTokens <= 0 {
		request.MaxContextTokens = f.options.TargetContextTokens
	}
	request.MaxContextTokens = minIntMemory(request.MaxContextTokens, f.options.MaxContextTokens)
	analysis := analyzeMemoryQuery(request.Query)

	searchCtx := ctx
	cancel := func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline && f.options.SearchLatencyBudget > 0 {
		searchCtx, cancel = context.WithTimeout(ctx, f.options.SearchLatencyBudget)
	}
	defer cancel()
	analysis = f.weightQueryTerms(searchCtx, request.Space, analysis)
	result.Diagnostics.QueryTerms = searchTermDiagnostics(analysis)

	candidates := map[string]*rankedCandidate{}
	fts, _ := f.searchFTS(searchCtx, request.Space, analysis.tokens, f.options.CandidateLimit)
	mergeSearchChannel(candidates, "fts", fts)
	result.Diagnostics.FTSCandidates = len(fts)
	facets, _ := f.searchFTSFacets(searchCtx, request.Space, analysis.tokens, 6)
	for token, documents := range facets {
		mergeSearchChannel(candidates, "facet:"+token, documents)
	}
	if len(facets) > 0 {
		result.Route = append(result.Route, "lexical-facets")
	}
	keys, _ := f.searchKeys(searchCtx, request.Space, analysis, f.options.CandidateLimit/2)
	mergeSearchChannel(candidates, "keys", keys)
	result.Route = append(result.Route, "lexical")
	vectorDocs, _ := f.searchVector(searchCtx, request.Space, request.Query, analysis, f.options.CandidateLimit)
	mergeSearchChannel(candidates, "vector", vectorDocs)
	result.Diagnostics.VectorCandidates = len(vectorDocs)
	if len(vectorDocs) > 0 {
		result.Route = append(result.Route, "vector")
	}

	overlay, lag := f.searchOverlay(searchCtx, request.Space, analysis)
	mergeSearchChannel(candidates, "overlay", overlay)
	result.Diagnostics.IndexLag = lag
	ordered := orderSearchCandidates(candidates, analysis)
	ordered = filterSearchTimeAndState(ordered, analysis, request.ReferenceTime)
	ordered = prioritizeContextDiversity(ordered)
	result.Route = append(result.Route, "context-diversity")
	var expanded int
	contextLimit := 12
	expansionCtx, expansionCancel := contextWithOptionalTimeout(ctx, f.options.SearchLatencyBudget)
	ordered, expanded = f.expandContextCandidates(expansionCtx, request.Space, ordered, analysis,
		request.ReferenceTime, contextLimit)
	expansionCancel()
	if expanded > 0 {
		result.Route = append(result.Route, "context-expand")
	}
	ordered = prioritizeContextDiversity(ordered)
	var evidence []Evidence
	var nodeIDs []string
	var deduplicated int
	bgeEnabled := f.options.RetrievalEncoder != nil
	if bgeEnabled {
		hybrid, hybridDiagnostics, hybridErr := f.searchBGERetrieval(ctx, request, analysis)
		if hybridErr != nil {
			return result, fmt.Errorf("BGE-M3 retrieval failed: %w", hybridErr)
		}
		if len(hybrid) > 0 {
			ordered = mergeBGEWithState(hybrid, ordered)
			selectionStarted := time.Now()
			evidence, nodeIDs, deduplicated = selectSubmodularEvidence(ordered, analysis,
				request.MaxEvidence, request.MaxContextTokens)
			evidence = orderEvidenceByContextAndTime(evidence)
			hybridDiagnostics.latency["selection"] = time.Since(selectionStarted)
		}
		result.Route = append(result.Route, "bge-m3-hybrid", "event-dense-sparse", "ppr", "submodular-evidence")
		applySidecarDiagnostics(&result.Diagnostics, hybridDiagnostics, f.options.RetrievalEncoder.Revision())
	}
	if !bgeEnabled && len(evidence) == 0 {
		evidence, nodeIDs, deduplicated = selectEvidence(ordered, analysis,
			request.MaxEvidence, request.MaxContextTokens)
	}
	result.Evidence = evidence
	result.Diagnostics.Deduplicated = deduplicated
	result.Diagnostics.SelectedSourceEvents = sortedSourceEvents(evidence)
	result.Diagnostics.SelectedContextIDs = selectedEvidenceContextIDs(evidence, ordered)
	for _, item := range evidence {
		result.Diagnostics.EvidenceTokens += maxIntMemory(1, estimateTokens(item.Content))
	}
	materializeCtx, materializeCancel := contextWithOptionalTimeout(ctx, f.options.SearchLatencyBudget)
	defer materializeCancel()
	nodes, err := f.loadMemoryNodes(materializeCtx, nodeIDs)
	if err != nil {
		return result, err
	}
	result.CurrentView = filterCurrentNodes(nodes, request.ReferenceTime)
	result.Conflicts, _ = f.loadRelevantConflicts(materializeCtx, request.Space, result.CurrentView, ordered)
	result.Insufficient = memorySearchInsufficient(analysis, result)
	result.Route = uniqueStrings(result.Route)
	result.Diagnostics.Route = append([]string(nil), result.Route...)
	result.Diagnostics.Duration = time.Since(started)
	if request.IncludeDiagnostics {
		result.Diagnostics.Candidates = summarizeSearchCandidates(ordered, evidence, 96, 600)
	} else {
		result.Diagnostics = SearchDiagnostics{}
	}
	return result, nil
}

func selectedEvidenceContextIDs(evidence []Evidence, candidates []*rankedCandidate) []string {
	contextsBySource := make(map[string]string, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil || candidate.document.ContextID == "" {
			continue
		}
		for _, sourceID := range candidate.document.SourceEventIDs {
			if sourceID != "" {
				contextsBySource[sourceID] = candidate.document.ContextID
			}
		}
	}
	var result []string
	for _, item := range evidence {
		if item.ContextID != "" {
			result = append(result, item.ContextID)
		}
		for _, sourceID := range item.SourceEventIDs {
			if contextID := contextsBySource[sourceID]; contextID != "" {
				result = append(result, contextID)
			}
		}
	}
	sort.Strings(result)
	return uniqueStrings(result)
}

func evidenceHasMatchReason(evidence []Evidence, reason string) bool {
	for _, item := range evidence {
		if containsString(item.MatchReasons, reason) {
			return true
		}
	}
	return false
}

func contextWithOptionalTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); hasDeadline || timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

func summarizeSearchCandidates(candidates []*rankedCandidate, evidence []Evidence, limit, maxRunes int) []SearchCandidateDiagnostic {
	selected := make(map[string]struct{}, len(evidence))
	for _, item := range evidence {
		selected[item.ID] = struct{}{}
		for _, sourceID := range item.SourceEventIDs {
			selected[sourceID] = struct{}{}
		}
	}
	if limit <= 0 || limit > len(candidates) {
		limit = len(candidates)
	}
	result := make([]SearchCandidateDiagnostic, 0, limit)
	for _, candidate := range candidates[:limit] {
		content := strings.TrimSpace(candidate.document.Content)
		if maxRunes > 0 {
			runes := []rune(content)
			if len(runes) > maxRunes {
				content = string(runes[:maxRunes])
			}
		}
		_, isSelected := selected[candidate.document.ID]
		if !isSelected {
			for _, sourceID := range candidate.document.SourceEventIDs {
				if _, isSelected = selected[sourceID]; isSelected {
					break
				}
			}
		}
		result = append(result, SearchCandidateDiagnostic{
			ID: candidate.document.ID, ResourceID: candidate.document.ResourceID,
			ResourceKind: candidate.document.ResourceKind, ContextID: candidate.document.ContextID,
			Status: candidate.document.Status, Score: candidate.score,
			ChannelRanks:   cloneSearchRanks(candidate.ranks),
			ChannelScores:  cloneFloatScores(candidate.channelScores),
			MaxSimScore:    candidate.maxSimScore,
			QueryCoverage:  candidate.coverage,
			MatchReasons:   uniqueStrings(candidate.reasons),
			SourceEventIDs: append([]string(nil), candidate.document.SourceEventIDs...),
			Content:        content, Selected: isSelected,
		})
	}
	return result
}

func cloneSearchRanks(ranks map[string]int) map[string]int {
	if len(ranks) == 0 {
		return nil
	}
	result := make(map[string]int, len(ranks))
	for channel, rank := range ranks {
		result[channel] = rank
	}
	return result
}

func orderEvidenceByContextAndTime(evidence []Evidence) []Evidence {
	if len(evidence) < 2 {
		return evidence
	}
	contextScore := make(map[string]float64, len(evidence))
	for _, item := range evidence {
		if item.Score > contextScore[item.ContextID] {
			contextScore[item.ContextID] = item.Score
		}
	}
	sort.SliceStable(evidence, func(i, j int) bool {
		left, right := evidence[i], evidence[j]
		if left.ContextID == right.ContextID {
			if !left.OccurredAt.Equal(right.OccurredAt) {
				return left.OccurredAt.Before(right.OccurredAt)
			}
			return left.ID < right.ID
		}
		if contextScore[left.ContextID] == contextScore[right.ContextID] {
			return left.ContextID < right.ContextID
		}
		return contextScore[left.ContextID] > contextScore[right.ContextID]
	})
	return evidence
}

type contextExpansionEvent struct {
	id         string
	actor      string
	content    string
	occurredAt time.Time
	affinity   float64
	seedRank   int
}

func (f *Fabric) expandContextCandidates(ctx context.Context, space string, candidates []*rankedCandidate,
	analysis queryAnalysis, reference time.Time, maxContexts int) ([]*rankedCandidate, int) {
	if maxContexts <= 0 || len(candidates) == 0 {
		return candidates, 0
	}
	type contextSeed struct {
		id          string
		score       float64
		ranks       map[string]int
		reasons     []string
		sourceRanks map[string]int
	}
	seedsByID := map[string]*contextSeed{}
	var seedOrder []string
	for candidateIndex, candidate := range candidates {
		contextID := strings.TrimSpace(candidate.document.ContextID)
		if contextID == "" {
			continue
		}
		seed := seedsByID[contextID]
		if seed == nil {
			seed = &contextSeed{id: contextID, ranks: map[string]int{}, sourceRanks: map[string]int{}}
			seedsByID[contextID] = seed
			seedOrder = append(seedOrder, contextID)
		}
		seed.score = math.Max(seed.score, candidate.score)
		for channel, rank := range candidate.ranks {
			if old, ok := seed.ranks[channel]; !ok || rank < old {
				seed.ranks[channel] = rank
			}
		}
		seed.reasons = append(seed.reasons, candidate.reasons...)
		for _, sourceID := range candidate.document.SourceEventIDs {
			if previous, ok := seed.sourceRanks[sourceID]; !ok || candidateIndex+1 < previous {
				seed.sourceRanks[sourceID] = candidateIndex + 1
			}
		}
	}
	expansions := make([]*rankedCandidate, 0, maxContexts)
	for _, contextID := range seedOrder {
		seed := seedsByID[contextID]
		expansion, ok := f.buildContextExpansion(ctx, space, contextID, analysis, reference, seed.sourceRanks)
		if ok {
			expansion.score = seed.score * 1.05
			expansion.ranks = make(map[string]int, len(seed.ranks)+1)
			for channel, rank := range seed.ranks {
				expansion.ranks[channel] = rank
			}
			expansion.reasons = append(expansion.reasons, seed.reasons...)
			expansion.ranks["context"] = len(expansions) + 1
			expansions = append(expansions, expansion)
		}
		if len(expansions) >= maxContexts {
			break
		}
	}
	if len(expansions) == 0 {
		return candidates, 0
	}
	expandedSources := make(map[string]struct{})
	for _, expansion := range expansions {
		for _, sourceID := range expansion.document.SourceEventIDs {
			expandedSources[sourceID] = struct{}{}
		}
	}
	combined := append([]*rankedCandidate{}, expansions...)
	var stateOnly []*rankedCandidate
	for _, candidate := range candidates {
		sourceExpanded := documentUsesAnySource(candidate.document, expandedSources)
		if candidate.document.ResourceKind == "event" || candidate.document.ResourceKind == "chunk" {
			if sourceExpanded {
				continue
			}
		}
		if candidate.document.ResourceKind == "node" && sourceExpanded {
			candidate.stateOnly = true
			stateOnly = append(stateOnly, candidate)
			continue
		}
		combined = append(combined, candidate)
	}
	sort.SliceStable(combined, func(i, j int) bool {
		if combined[i].score == combined[j].score {
			return combined[i].document.OccurredAt.After(combined[j].document.OccurredAt)
		}
		return combined[i].score > combined[j].score
	})
	combined = append(combined, stateOnly...)
	return combined, len(expansions)
}

func documentUsesAnySource(document searchDocument, sourceIDs map[string]struct{}) bool {
	for _, sourceID := range document.SourceEventIDs {
		if _, exists := sourceIDs[sourceID]; exists {
			return true
		}
	}
	return false
}

func (f *Fabric) buildContextExpansion(ctx context.Context, space, contextID string, analysis queryAnalysis,
	reference time.Time, sourceRanks map[string]int) (*rankedCandidate, bool) {
	rows, err := f.ledger.QueryContext(ctx, `SELECT event_id, actor, content, occurred_at FROM events
		WHERE space=? AND context_id=? AND tombstoned=0 AND occurred_at<=? ORDER BY occurred_at, event_id`,
		space, contextID, formatFabricTime(reference))
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	var events []contextExpansionEvent
	for rows.Next() {
		var event contextExpansionEvent
		var occurredAt string
		if rows.Scan(&event.id, &event.actor, &event.content, &occurredAt) != nil {
			continue
		}
		event.occurredAt = parseFabricTime(occurredAt)
		event.affinity = contextEventAffinity(event.content, analysis)
		if sourceRanks != nil {
			event.seedRank = sourceRanks[event.id]
		}
		events = append(events, event)
	}
	if len(events) == 0 {
		return nil, false
	}
	hasMatchingEvent := false
	for _, event := range events {
		if event.affinity > 0 || event.seedRank > 0 {
			hasMatchingEvent = true
			break
		}
	}
	if !hasMatchingEvent {
		return nil, false
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].affinity != events[j].affinity {
			return events[i].affinity > events[j].affinity
		}
		if (events[i].seedRank > 0) != (events[j].seedRank > 0) {
			return events[i].seedRank > 0
		}
		if events[i].seedRank > 0 && events[i].seedRank != events[j].seedRank {
			return events[i].seedRank < events[j].seedRank
		}
		return events[i].occurredAt.After(events[j].occurredAt)
	})
	const primaryExpansionEvents, maxExpansionTokens = 4, 500
	selectedEvents := selectContextExpansionEvents(events, primaryExpansionEvents)
	document, ok := renderContextExpansionDocument(space, contextID, selectedEvents, analysis,
		primaryExpansionEvents, maxExpansionTokens)
	if !ok {
		return nil, false
	}
	candidate := &rankedCandidate{document: document,
		contextEvents: append([]contextExpansionEvent(nil), events...),
		ranks:         map[string]int{"context": 1}, coverage: weightedQueryCoverage(document.Content, analysis),
		reasons: []string{"context-expand"}}
	return candidate, true
}

func removeExpandedSourceProjections(candidates []*rankedCandidate) []*rankedCandidate {
	expandedSources := make(map[string]struct{})
	for _, candidate := range candidates {
		if candidate.document.ResourceKind != "context" {
			continue
		}
		for _, sourceID := range candidate.document.SourceEventIDs {
			expandedSources[sourceID] = struct{}{}
		}
	}
	if len(expandedSources) == 0 {
		return candidates
	}
	result := make([]*rankedCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if (candidate.document.ResourceKind == "event" || candidate.document.ResourceKind == "chunk") &&
			documentUsesAnySource(candidate.document, expandedSources) {
			continue
		}
		if candidate.document.ResourceKind == "node" && documentUsesAnySource(candidate.document, expandedSources) {
			candidate.stateOnly = true
		}
		result = append(result, candidate)
	}
	return result
}

func renderContextExpansionDocument(space, contextID string, selectedEvents []contextExpansionEvent,
	analysis queryAnalysis, primaryEvents, maxTokens int) (searchDocument, bool) {
	if len(selectedEvents) == 0 {
		return searchDocument{}, false
	}
	tokenBudgets := contextExpansionTokenBudgets(len(selectedEvents), primaryEvents, maxTokens)
	var sections, sourceIDs []string
	usedTokens := 0
	latest := selectedEvents[0].occurredAt
	for index, event := range selectedEvents {
		content := strings.TrimSpace(event.content)
		if content == "" {
			continue
		}
		remaining := maxTokens - usedTokens
		if remaining <= 0 {
			break
		}
		budget := minIntMemory(tokenBudgets[index], remaining)
		if budget <= 0 {
			budget = remaining
		}
		content = extractMemorySearchSnippet(content, analysis, budget)
		if content == "" {
			continue
		}
		content = strings.Join(strings.Fields(content), " ")
		sections = append(sections, formatFabricTime(event.occurredAt)+" "+event.actor+": "+content)
		sourceIDs = append(sourceIDs, event.id)
		usedTokens += estimateTokens(content)
		if event.occurredAt.After(latest) {
			latest = event.occurredAt
		}
	}
	if len(sections) == 0 {
		return searchDocument{}, false
	}
	content := strings.Join(sections, "\n")
	id := stableFabricID("context-evidence", space, contextID, content)
	return searchDocument{ID: id, Space: space, ResourceKind: "context",
		ResourceID: contextID, Content: content, ContextID: contextID, OccurredAt: latest,
		Status: SemanticRawOnly, SourceEventIDs: uniqueStrings(sourceIDs)}, true
}

func selectContextExpansionEvents(events []contextExpansionEvent, limit int) []contextExpansionEvent {
	if limit <= 0 || len(events) == 0 {
		return nil
	}
	return append([]contextExpansionEvent(nil), events[:minIntMemory(limit, len(events))]...)
}

func contextExpansionTokenBudgets(count, primary, total int) []int {
	budgets := make([]int, count)
	if count == 0 || total <= 0 {
		return budgets
	}
	if primary <= 0 || count <= primary {
		for index := range budgets {
			budgets[index] = total / count
		}
		return budgets
	}
	supplements := count - primary
	const supplementTokens = 36
	primaryTokens := total - supplements*supplementTokens
	for index := range budgets {
		if index < primary {
			budgets[index] = primaryTokens / primary
		} else {
			budgets[index] = supplementTokens
		}
	}
	return budgets
}

func contextEventAffinity(content string, analysis queryAnalysis) float64 {
	normalized := normalizeClaim(content)
	if normalized == "" {
		return 0
	}
	score := weightedQueryCoverage(content, analysis)
	for _, term := range analysis.literalTerms {
		if term = normalizeClaim(term); term != "" && strings.Contains(normalized, term) {
			score++
		}
	}
	return score
}

func truncateMemorySearchText(content string, maxTokens int) string {
	maxRunes := maxTokens * 3
	runes := []rune(strings.TrimSpace(content))
	if maxRunes <= 0 {
		return ""
	}
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return strings.TrimSpace(string(runes[:maxRunes]))
}

func extractMemorySearchSnippet(content string, analysis queryAnalysis, maxTokens int) string {
	content = strings.TrimSpace(content)
	maxRunes := maxTokens * 3
	runes := []rune(content)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return truncateMemorySearchText(content, maxTokens)
	}
	lowerRunes := []rune(strings.ToLower(content))
	terms := normalizeStringList(append(append([]string(nil), analysis.literalTerms...), analysis.tokens...), 24)
	starts := map[int]struct{}{0: {}, len(runes) - maxRunes: {}}
	addStart := func(start int) {
		if start < 0 {
			start = 0
		}
		if start+maxRunes > len(runes) {
			start = len(runes) - maxRunes
		}
		starts[start] = struct{}{}
	}
	for _, term := range terms {
		termRunes := []rune(strings.ToLower(strings.TrimSpace(term)))
		if len(termRunes) < 2 {
			continue
		}
		for offset := 0; offset+len(termRunes) <= len(lowerRunes); {
			relative := strings.Index(string(lowerRunes[offset:]), string(termRunes))
			if relative < 0 {
				break
			}
			match := offset + len([]rune(string(lowerRunes[offset:])[:relative]))
			addStart(match - maxRunes/3)
			offset = match + maxIntMemory(1, len(termRunes))
		}
	}
	bestStart, bestScore := 0, -1.0
	for start := range starts {
		window := string(runes[start:minIntMemory(len(runes), start+maxRunes)])
		score := contextEventAffinity(window, analysis)
		if score > bestScore || (score == bestScore && start < bestStart) {
			bestStart, bestScore = start, score
		}
	}
	end := minIntMemory(len(runes), bestStart+maxRunes)
	snippet := strings.TrimSpace(string(runes[bestStart:end]))
	if bestStart > 0 {
		snippet = "..." + snippet
	}
	if end < len(runes) {
		snippet += "..."
	}
	return snippet
}

type memorySearchSnippetWindow struct {
	start int
	end   int
}

type memorySearchSnippetWindowFeatures struct {
	window   memorySearchSnippetWindow
	tokens   map[string]struct{}
	affinity float64
}

func extractMemorySearchMultiWindowSnippet(content string, analysis queryAnalysis, maxTokens int) string {
	return extractMemorySearchMultiWindowSnippetAt(content, analysis, maxTokens, 0, 0)
}

func extractMemorySearchMultiWindowSnippetAt(content string, analysis queryAnalysis, maxTokens,
	matchPosition, spanTokens int) string {
	content = strings.TrimSpace(content)
	maxRunes := maxTokens * 3
	runes := []rune(content)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return truncateMemorySearchText(content, maxTokens)
	}
	if snippet, ok := extractMemorySearchSentenceSnippetAt(runes, analysis, maxRunes,
		matchPosition, spanTokens); ok {
		return snippet
	}

	// Reserve enough space for clipping and gap markers before splitting the
	// fixed budget between a primary fact window and a smaller context window.
	const markerRunes = 11
	available := maxRunes - markerRunes
	if available < 48 {
		return extractMemorySearchSnippet(content, analysis, maxTokens)
	}
	secondaryRunes := available / 4
	primaryRunes := available - secondaryRunes
	terms := normalizeStringList(append(append([]string(nil), analysis.literalTerms...), analysis.tokens...), 24)
	var primaryStarts []int
	if matchPosition > 0 && spanTokens > 0 {
		center := matchPosition * len(runes) / spanTokens
		primaryStarts = append(primaryStarts, center-primaryRunes/2)
	}
	primaries := memorySearchSnippetWindowCandidates(runes, terms, primaryRunes, primaryStarts...)
	secondaries := memorySearchSnippetWindowCandidates(runes, terms, secondaryRunes)
	primaryFeatures := memorySearchSnippetWindowFeatureSet(runes, primaries, analysis)
	secondaryFeatures := memorySearchSnippetWindowFeatureSet(runes, secondaries, analysis)
	primary, secondary := memorySearchSnippetWindow{}, memorySearchSnippetWindow{}
	bestUtility := -math.MaxFloat64
	for _, primaryFeature := range primaryFeatures {
		for _, secondaryFeature := range secondaryFeatures {
			if windowsOverlap(primaryFeature.window, secondaryFeature.window) {
				continue
			}
			overlap := tokenJaccardSets(primaryFeature.tokens, secondaryFeature.tokens)
			boundary := 0.0
			if secondaryFeature.window.start == 0 || secondaryFeature.window.end == len(runes) {
				boundary = 1
			}
			gap := primaryFeature.window.start - secondaryFeature.window.end
			if secondaryFeature.window.start >= primaryFeature.window.end {
				gap = secondaryFeature.window.start - primaryFeature.window.end
			}
			adjacency := 1 / (1 + float64(maxIntMemory(0, gap))/float64(maxIntMemory(1, secondaryRunes)))
			utility := .75*primaryFeature.affinity +
				.10*weightedQueryCoverageAcrossTokenSets(primaryFeature.tokens, secondaryFeature.tokens, analysis) +
				.05*weightedNewQueryCoverage(secondaryFeature.tokens, primaryFeature.tokens, analysis) +
				.04*(1-overlap) + .03*boundary + .03*adjacency
			if utility > bestUtility ||
				(utility == bestUtility && snippetWindowPairLess(primaryFeature.window, secondaryFeature.window,
					primary, secondary)) {
				primary, secondary, bestUtility = primaryFeature.window, secondaryFeature.window, utility
			}
		}
	}
	if secondary.end <= secondary.start || windowsOverlap(primary, secondary) {
		return extractMemorySearchSnippet(content, analysis, maxTokens)
	}

	windows := []memorySearchSnippetWindow{primary, secondary}
	sort.Slice(windows, func(i, j int) bool { return windows[i].start < windows[j].start })
	var builder strings.Builder
	if windows[0].start > 0 {
		builder.WriteString("...")
	}
	builder.WriteString(strings.TrimSpace(string(runes[windows[0].start:windows[0].end])))
	if windows[0].end < windows[1].start {
		builder.WriteString("\n...\n")
	}
	builder.WriteString(strings.TrimSpace(string(runes[windows[1].start:windows[1].end])))
	if windows[1].end < len(runes) {
		builder.WriteString("...")
	}
	snippet := strings.TrimSpace(builder.String())
	if estimateTokens(snippet) > maxTokens {
		return truncateMemorySearchText(snippet, maxTokens)
	}
	return snippet
}

func extractMemorySearchSentenceSnippetAt(runes []rune, analysis queryAnalysis, maxRunes,
	matchPosition, spanTokens int) (string, bool) {
	spans := memorySearchSentenceSpans(runes)
	if len(spans) < 2 || maxRunes <= 0 {
		return "", false
	}
	features := memorySearchSnippetWindowFeatureSet(runes, spans, analysis)
	center := -1
	if matchPosition > 0 && spanTokens > 0 {
		center = matchPosition * len(runes) / spanTokens
	}

	selected := make([]memorySearchSnippetWindow, 0, 3)
	selectedTokens := map[string]struct{}{}
	usedRunes := 0
	for len(selected) < 3 {
		best := -1
		bestUtility := -math.MaxFloat64
		for index, feature := range features {
			alreadySelected := false
			for _, prior := range selected {
				if feature.window == prior {
					alreadySelected = true
					break
				}
			}
			if alreadySelected {
				continue
			}
			cost := feature.window.end - feature.window.start
			if len(selected) > 0 {
				cost++
			}
			if usedRunes+cost > maxRunes {
				continue
			}
			containsMatch := 0.0
			if center >= feature.window.start && center < feature.window.end {
				containsMatch = 1
			}
			matchUtility := .02 * containsMatch
			if feature.affinity <= 0 {
				matchUtility = .25 * containsMatch
			}
			questionPenalty := memorySearchSentenceQuestionPenalty(runes, feature.window)
			utility := feature.affinity + .20*weightedQueryCoverageAcrossTokenSets(
				feature.tokens, nil, analysis) + matchUtility - questionPenalty
			if len(selected) > 0 {
				adjacency := 0.0
				precedingContext := 0.0
				for _, prior := range selected {
					gap := feature.window.start - prior.end
					if prior.start >= feature.window.end {
						gap = prior.start - feature.window.end
					}
					adjacency = math.Max(adjacency, 1/(1+float64(maxIntMemory(0, gap))/
						float64(maxIntMemory(1, feature.window.end-feature.window.start))))
					if feature.window.end <= prior.start &&
						memorySearchSentenceSpansAdjacent(runes, feature.window, prior) {
						precedingContext = 1
					}
				}
				utility = .65*feature.affinity +
					.25*weightedNewQueryCoverage(feature.tokens, selectedTokens, analysis) +
					.10*adjacency + .15*precedingContext + matchUtility - questionPenalty
			}
			if utility > bestUtility || (utility == bestUtility &&
				(best < 0 || feature.window.start < features[best].window.start)) {
				best, bestUtility = index, utility
			}
		}
		if best < 0 || (len(selected) == 0 && bestUtility <= 0) ||
			(len(selected) > 0 && bestUtility < .08) {
			break
		}
		window := features[best].window
		if len(selected) > 0 {
			usedRunes++
		}
		usedRunes += window.end - window.start
		selected = append(selected, window)
		for token := range features[best].tokens {
			selectedTokens[token] = struct{}{}
		}
	}
	if len(selected) == 0 {
		return "", false
	}

	sort.Slice(selected, func(i, j int) bool { return selected[i].start < selected[j].start })
	var builder strings.Builder
	if selected[0].start > 0 {
		builder.WriteString("...")
	}
	for index, window := range selected {
		if index > 0 {
			if memorySearchSentenceSpansAdjacent(runes, selected[index-1], window) {
				builder.WriteByte(' ')
			} else {
				builder.WriteString("\n...\n")
			}
		}
		builder.WriteString(strings.TrimSpace(string(runes[window.start:window.end])))
	}
	if selected[len(selected)-1].end < len(runes) {
		builder.WriteString("...")
	}
	snippet := strings.TrimSpace(builder.String())
	if len([]rune(snippet)) > maxRunes || estimateTokens(snippet) > (maxRunes+2)/3 {
		return "", false
	}
	return snippet, true
}

func memorySearchSentenceQuestionPenalty(runes []rune, window memorySearchSnippetWindow) float64 {
	for index := window.end - 1; index >= window.start; index-- {
		if unicode.IsSpace(runes[index]) {
			continue
		}
		if runes[index] == '?' {
			return .30
		}
		break
	}
	return 0
}

func memorySearchSentenceSpansAdjacent(runes []rune, left, right memorySearchSnippetWindow) bool {
	if left.end > right.start {
		return false
	}
	for _, current := range runes[left.end:right.start] {
		if !unicode.IsSpace(current) {
			return false
		}
	}
	return true
}

func memorySearchSentenceSpans(runes []rune) []memorySearchSnippetWindow {
	spans := make([]memorySearchSnippetWindow, 0, 8)
	start := 0
	appendSpan := func(end int) {
		for start < end && unicode.IsSpace(runes[start]) {
			start++
		}
		for end > start && unicode.IsSpace(runes[end-1]) {
			end--
		}
		if end > start {
			spans = append(spans, memorySearchSnippetWindow{start: start, end: end})
		}
		start = end
	}
	for index, current := range runes {
		boundary := current == '\n'
		if current == '.' || current == '?' || current == '!' {
			boundary = index+1 == len(runes) || unicode.IsSpace(runes[index+1])
		}
		if boundary {
			appendSpan(index + 1)
		}
	}
	appendSpan(len(runes))
	return spans
}

func memorySearchSnippetWindowCandidates(runes []rune, terms []string, windowRunes int,
	extraStarts ...int) []memorySearchSnippetWindow {
	if len(runes) == 0 || windowRunes <= 0 {
		return nil
	}
	windowRunes = minIntMemory(windowRunes, len(runes))
	lowerRunes := []rune(strings.ToLower(string(runes)))
	starts := map[int]struct{}{0: {}, len(runes) - windowRunes: {}}
	addStart := func(start int) {
		start = maxIntMemory(0, minIntMemory(start, len(runes)-windowRunes))
		starts[start] = struct{}{}
	}
	for _, start := range extraStarts {
		addStart(start)
	}
	for _, term := range terms {
		termRunes := []rune(strings.ToLower(strings.TrimSpace(term)))
		if len(termRunes) < 2 {
			continue
		}
		for offset := 0; offset+len(termRunes) <= len(lowerRunes); {
			relative := strings.Index(string(lowerRunes[offset:]), string(termRunes))
			if relative < 0 {
				break
			}
			match := offset + len([]rune(string(lowerRunes[offset:])[:relative]))
			addStart(match - windowRunes/6)
			addStart(match - windowRunes/3)
			addStart(match - windowRunes/2)
			addStart(match - 2*windowRunes/3)
			offset = match + maxIntMemory(1, len(termRunes))
		}
	}
	orderedStarts := make([]int, 0, len(starts))
	for start := range starts {
		orderedStarts = append(orderedStarts, start)
	}
	sort.Ints(orderedStarts)
	windows := make([]memorySearchSnippetWindow, 0, len(orderedStarts))
	for _, start := range orderedStarts {
		windows = append(windows, memorySearchSnippetWindow{
			start: start,
			end:   minIntMemory(len(runes), start+windowRunes),
		})
	}
	return windows
}

func memorySearchSnippetWindowFeatureSet(runes []rune, windows []memorySearchSnippetWindow,
	analysis queryAnalysis) []memorySearchSnippetWindowFeatures {
	features := make([]memorySearchSnippetWindowFeatures, 0, len(windows))
	for _, window := range windows {
		content := string(runes[window.start:window.end])
		features = append(features, memorySearchSnippetWindowFeatures{
			window:   window,
			tokens:   queryTokenSet(content),
			affinity: contextEventAffinity(content, analysis),
		})
	}
	return features
}

func snippetWindowPairLess(primary, secondary, bestPrimary, bestSecondary memorySearchSnippetWindow) bool {
	if bestPrimary.end == 0 {
		return true
	}
	if primary.start != bestPrimary.start {
		return primary.start < bestPrimary.start
	}
	return secondary.start < bestSecondary.start
}

func weightedNewQueryCoverage(contentTokens, coveredTokens map[string]struct{}, analysis queryAnalysis) float64 {
	matched, total := 0.0, 0.0
	for _, token := range analysis.tokens {
		weight := analysis.weights[token]
		if weight <= 0 {
			weight = 1
		}
		total += weight
		if _, covered := coveredTokens[token]; covered {
			continue
		}
		if _, present := contentTokens[token]; present {
			matched += weight
		}
	}
	if total <= 0 {
		return 0
	}
	return matched / total
}

func weightedQueryCoverageAcrossTokenSets(left, right map[string]struct{}, analysis queryAnalysis) float64 {
	matched, total := 0.0, 0.0
	for _, token := range analysis.tokens {
		weight := analysis.weights[token]
		if weight <= 0 {
			weight = 1
		}
		total += weight
		_, leftPresent := left[token]
		_, rightPresent := right[token]
		if leftPresent || rightPresent {
			matched += weight
		}
	}
	if total <= 0 {
		return 0
	}
	return matched / total
}

func windowsOverlap(left, right memorySearchSnippetWindow) bool {
	return left.start < right.end && right.start < left.end
}

type weightedQueryTerm struct {
	text      string
	weight    float64
	frequency int
	position  int
}

func (f *Fabric) weightQueryTerms(ctx context.Context, space string, analysis queryAnalysis) queryAnalysis {
	analysis.weights = map[string]float64{}
	analysis.frequencies = map[string]int{}
	if len(analysis.tokens) == 0 {
		return analysis
	}
	var contextCount int
	err := f.index.QueryRowContext(ctx, `SELECT COUNT(DISTINCT CASE
		WHEN context_id='' THEN doc_id ELSE context_id END) FROM documents WHERE space=?`, space).Scan(&contextCount)
	if err != nil || contextCount <= 0 {
		for _, token := range analysis.tokens {
			analysis.weights[token] = 1
		}
		return analysis
	}
	terms := make([]weightedQueryTerm, 0, len(analysis.tokens))
	for position, token := range analysis.tokens {
		match := ftsTermsFromTokens([]string{token}, 1)
		if match == "" {
			continue
		}
		var frequency int
		err := f.index.QueryRowContext(ctx, `SELECT COUNT(DISTINCT CASE
			WHEN d.context_id='' THEN d.doc_id ELSE d.context_id END)
			FROM document_fts JOIN documents d ON d.doc_id=document_fts.doc_id
			WHERE document_fts MATCH ? AND d.space=?`, match, space).Scan(&frequency)
		if err != nil || frequency <= 0 {
			continue
		}
		weight := math.Log((float64(contextCount)+1)/(float64(frequency)+1)) + 1
		terms = append(terms, weightedQueryTerm{text: token, weight: weight, frequency: frequency, position: position})
	}
	if len(terms) == 0 {
		limit := minIntMemory(8, len(analysis.tokens))
		analysis.tokens = append([]string(nil), analysis.tokens[:limit]...)
		for _, token := range analysis.tokens {
			analysis.weights[token] = 1
		}
		return analysis
	}
	sort.SliceStable(terms, func(i, j int) bool {
		if terms[i].weight == terms[j].weight {
			return terms[i].position < terms[j].position
		}
		return terms[i].weight > terms[j].weight
	})
	if len(terms) > 8 {
		terms = terms[:8]
	}
	analysis.tokens = make([]string, 0, len(terms))
	for _, term := range terms {
		analysis.tokens = append(analysis.tokens, term.text)
		analysis.weights[term.text] = term.weight
		analysis.frequencies[term.text] = term.frequency
	}
	return analysis
}

func searchTermDiagnostics(analysis queryAnalysis) []SearchTermDiagnostic {
	result := make([]SearchTermDiagnostic, 0, len(analysis.tokens))
	for _, token := range analysis.tokens {
		result = append(result, SearchTermDiagnostic{Text: token, Weight: analysis.weights[token],
			ContextFrequency: analysis.frequencies[token]})
	}
	return result
}

func weightedQueryCoverage(content string, analysis queryAnalysis) float64 {
	if strings.TrimSpace(content) == "" || len(analysis.tokens) == 0 {
		return 0
	}
	contentTokens := queryTokenSet(content)
	matched, total := 0.0, 0.0
	for _, token := range analysis.tokens {
		weight := analysis.weights[token]
		if weight <= 0 {
			weight = 1
		}
		total += weight
		if _, ok := contentTokens[token]; ok {
			matched += weight
		}
	}
	if total == 0 {
		return 0
	}
	return matched / total
}

func queryTokenSet(value string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, token := range queryTokens(value, 0) {
		result[token] = struct{}{}
	}
	return result
}

func (f *Fabric) searchFTS(ctx context.Context, space string, tokens []string, limit int) ([]searchDocument, error) {
	match := ftsTermsFromTokens(tokens, 16)
	if match == "" {
		return nil, nil
	}
	rows, err := f.index.QueryContext(ctx, `SELECT d.doc_id, d.space, d.resource_kind, d.resource_id,
		d.content, d.keys_text, d.context_id, d.occurred_at, d.slot_id, d.semantic_status,
		d.source_event_ids_json, d.ledger_seq
		FROM document_fts JOIN documents d ON d.doc_id=document_fts.doc_id
		WHERE document_fts MATCH ? AND d.space=? ORDER BY bm25(document_fts) LIMIT ?`, match, space, limit)
	if err != nil {
		return nil, err
	}
	return scanSearchDocuments(rows)
}

func (f *Fabric) searchFTSFacets(ctx context.Context, space string, tokens []string,
	perToken int) (map[string][]searchDocument, error) {
	result := map[string][]searchDocument{}
	if perToken <= 0 {
		return result, nil
	}
	for _, token := range normalizeStringList(tokens, 12) {
		if len([]rune(token)) < 3 {
			continue
		}
		// A facet can be a natural-language target rather than one token. Use
		// its informative terms so harmless insertions (for example, "just")
		// do not turn target lookup into an exact-phrase miss.
		match := ftsTerms(token, 12)
		if match == "" {
			continue
		}
		rows, err := f.index.QueryContext(ctx, `SELECT d.doc_id, d.space, d.resource_kind, d.resource_id,
			d.content, d.keys_text, d.context_id, d.occurred_at, d.slot_id, d.semantic_status,
			d.source_event_ids_json, d.ledger_seq
			FROM document_fts JOIN documents d ON d.doc_id=document_fts.doc_id
			WHERE document_fts MATCH ? AND d.space=? ORDER BY bm25(document_fts) LIMIT ?`,
			match, space, perToken)
		if err != nil {
			return result, err
		}
		documents, err := scanSearchDocuments(rows)
		if err != nil {
			return result, err
		}
		if len(documents) > 0 {
			result[token] = documents
		}
	}
	return result, nil
}

func (f *Fabric) searchKeys(ctx context.Context, space string, analysis queryAnalysis, limit int) ([]searchDocument, error) {
	keys := append([]string{normalizeClaim(strings.Join(analysis.literalTerms, " "))}, analysis.literalTerms...)
	keys = append(keys, analysis.tokens...)
	keys = normalizeStringList(keys, 32)
	if len(keys) == 0 {
		return nil, nil
	}
	args := []any{space}
	for _, key := range keys {
		args = append(args, key)
	}
	args = append(args, limit)
	rows, err := f.index.QueryContext(ctx, `SELECT d.doc_id, d.space, d.resource_kind, d.resource_id,
		d.content, d.keys_text, d.context_id, d.occurred_at, d.slot_id, d.semantic_status,
		d.source_event_ids_json, d.ledger_seq
		FROM key_postings k JOIN documents d ON d.doc_id=k.doc_id
		WHERE k.space=? AND k.key_text IN (`+placeholders(len(keys))+`)
		GROUP BY d.doc_id ORDER BY SUM(k.weight) DESC, d.occurred_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	return scanSearchDocuments(rows)
}

func (f *Fabric) searchSlots(ctx context.Context, space string, analysis queryAnalysis, limit int) ([]searchDocument, error) {
	tokens := normalizeStringList(append(analysis.tokens, analysis.literalTerms...), 24)
	if len(tokens) == 0 {
		return nil, nil
	}
	attributeTokens := make([]string, 0, len(tokens))
	for _, token := range tokens {
		attributeTokens = append(attributeTokens, normalizeKey(token))
	}
	args := []any{space, SemanticActive, SemanticScoped, SemanticPendingResolution, SemanticSuperseded}
	for _, token := range tokens {
		args = append(args, token)
	}
	for _, token := range tokens {
		args = append(args, token)
	}
	for _, token := range attributeTokens {
		args = append(args, token)
	}
	args = append(args, limit)
	rows, err := f.ledger.QueryContext(ctx, `SELECT DISTINCT n.node_id FROM memory_nodes n
		WHERE n.space=? AND n.status IN (?, ?, ?, ?) AND n.tombstoned=0 AND (
			EXISTS (SELECT 1 FROM identity_aliases a WHERE a.identity_id=n.subject_identity_id
				AND a.status='active' AND a.normalized_alias IN (`+placeholders(len(tokens))+`)) OR
			EXISTS (SELECT 1 FROM node_keys k WHERE k.node_id=n.node_id
				AND k.key_text IN (`+placeholders(len(tokens))+`)) OR
			n.attribute_key IN (`+placeholders(len(attributeTokens))+`)
		) ORDER BY n.valid_from DESC, n.created_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return f.loadIndexedDocuments(ctx, ids)
}

func (f *Fabric) searchVector(ctx context.Context, space, query string, _ queryAnalysis, limit int) ([]searchDocument, error) {
	if f.options.Vectorizer == nil || limit <= 0 {
		return nil, nil
	}
	compatible, err := f.vectorModelCompatible(ctx)
	if err != nil || !compatible {
		return nil, err
	}
	vectors, err := f.options.Vectorizer.Embed(ctx, []string{query}, VectorQuery)
	if err != nil || len(vectors) != 1 || len(vectors[0]) != f.options.Vectorizer.Dimensions() {
		return nil, err
	}
	encoded, err := vector.EncodeEmbedding(vectors[0])
	if err != nil {
		return nil, err
	}
	rows, err := f.index.QueryContext(ctx, `SELECT doc_id FROM memory_vectors
		WHERE dataset_id=? AND doc_id MATCH ? LIMIT ?`, vectorDataset(space, "content"), encoded, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return f.loadIndexedDocuments(ctx, ids)
}

func scanSearchDocuments(rows *sql.Rows) ([]searchDocument, error) {
	defer rows.Close()
	var result []searchDocument
	for rows.Next() {
		var document searchDocument
		var occurredAt, sourcesJSON string
		if err := rows.Scan(&document.ID, &document.Space, &document.ResourceKind, &document.ResourceID,
			&document.Content, &document.KeysText, &document.ContextID, &occurredAt, &document.SlotID,
			&document.Status, &sourcesJSON, &document.LedgerSeq); err != nil {
			return nil, err
		}
		document.OccurredAt = parseFabricTime(occurredAt)
		_ = json.Unmarshal([]byte(sourcesJSON), &document.SourceEventIDs)
		result = append(result, document)
	}
	return result, rows.Err()
}

func (f *Fabric) loadIndexedDocuments(ctx context.Context, ids []string) ([]searchDocument, error) {
	ids = uniqueStrings(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := f.index.QueryContext(ctx, `SELECT doc_id, space, resource_kind, resource_id, content,
		keys_text, context_id, occurred_at, slot_id, semantic_status, source_event_ids_json, ledger_seq
		FROM documents WHERE doc_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	documents, err := scanSearchDocuments(rows)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]searchDocument, len(documents))
	for _, document := range documents {
		byID[document.ID] = document
	}
	ordered := make([]searchDocument, 0, len(documents))
	for _, id := range ids {
		if document, ok := byID[id]; ok {
			ordered = append(ordered, document)
		}
	}
	return ordered, nil
}

func mergeSearchChannel(all map[string]*rankedCandidate, channel string, documents []searchDocument) {
	for index, document := range documents {
		candidate := all[document.ID]
		if candidate == nil {
			candidate = &rankedCandidate{document: document, ranks: map[string]int{}}
			all[document.ID] = candidate
		}
		rank := index + 1
		if old, ok := candidate.ranks[channel]; !ok || rank < old {
			candidate.ranks[channel] = rank
		}
		candidate.reasons = append(candidate.reasons, channel)
	}
}

func orderSearchCandidates(all map[string]*rankedCandidate, analysis queryAnalysis) []*rankedCandidate {
	ordered := make([]*rankedCandidate, 0, len(all))
	maxScore := 0.0
	for _, candidate := range all {
		candidate.score = 0
		families := map[string]int{}
		for channel, rank := range candidate.ranks {
			family := channel
			if channel == "keys" || channel == "fts" || channel == "overlay" {
				family = "lexical"
			}
			if previous, ok := families[family]; !ok || rank < previous {
				families[family] = rank
			}
		}
		for _, rank := range families {
			candidate.score += 1 / float64(searchRRFK+rank)
		}
		if candidate.document.Status == SemanticPendingResolution {
			candidate.score *= 0.7
		}
		maxScore = math.Max(maxScore, candidate.score)
		ordered = append(ordered, candidate)
	}
	if maxScore > 0 {
		for _, candidate := range ordered {
			rrfScore := candidate.score / maxScore
			candidate.coverage = weightedQueryCoverage(candidate.document.Content+" "+candidate.document.KeysText, analysis)
			candidate.score = 0.7*rrfScore + 0.3*candidate.coverage
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].score == ordered[j].score {
			return ordered[i].document.OccurredAt.After(ordered[j].document.OccurredAt)
		}
		return ordered[i].score > ordered[j].score
	})
	return ordered
}

func prioritizeContextDiversity(candidates []*rankedCandidate) []*rankedCandidate {
	// Apply diversity only to candidates that earned lexical or vector support.
	// This prevents a weak projection from jumping ahead only
	// because it belongs to a new context.
	result := make([]*rankedCandidate, 0, len(candidates))
	representatives := make(map[string]*rankedCandidate)
	for _, candidate := range candidates {
		if !candidateQualifiedForDiversity(candidate) {
			continue
		}
		contextID := strings.TrimSpace(candidate.document.ContextID)
		if contextID == "" {
			continue
		}
		current := representatives[contextID]
		if current == nil || (candidate.document.ResourceKind == "context" && current.document.ResourceKind != "context") {
			representatives[contextID] = candidate
		}
	}
	seenContexts := map[string]struct{}{}
	deferred := make([]*rankedCandidate, 0, len(candidates))
	fallback := make([]*rankedCandidate, 0, len(candidates))
	weak := make([]*rankedCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if !candidateQualifiedForDiversity(candidate) {
			weak = append(weak, candidate)
			continue
		}
		contextID := strings.TrimSpace(candidate.document.ContextID)
		if contextID == "" {
			fallback = append(fallback, candidate)
			continue
		}
		if _, exists := seenContexts[contextID]; exists {
			if representatives[contextID] != candidate {
				deferred = append(deferred, candidate)
			}
			continue
		}
		seenContexts[contextID] = struct{}{}
		representative := representatives[contextID]
		result = append(result, representative)
		if representative != candidate {
			deferred = append(deferred, candidate)
		}
	}
	result = append(result, fallback...)
	result = append(result, deferred...)
	result = append(result, weak...)
	return result
}

func candidateQualifiedForDiversity(candidate *rankedCandidate) bool {
	if candidate == nil {
		return false
	}
	if candidate.coverage > 0 {
		return true
	}
	for channel, rank := range candidate.ranks {
		if rank > 12 {
			continue
		}
		if channel == "fts" || channel == "keys" || channel == "vector" || channel == "overlay" ||
			channel == "context" || strings.HasPrefix(channel, "facet:") {
			return true
		}
	}
	return false
}

func filterSearchTimeAndState(candidates []*rankedCandidate, _ queryAnalysis, reference time.Time) []*rankedCandidate {
	result := make([]*rankedCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		status := candidate.document.Status
		if status == SemanticRejected || status == SemanticQuarantined || status == SemanticTombstoned {
			continue
		}
		if !candidate.document.OccurredAt.IsZero() && candidate.document.OccurredAt.After(reference) {
			continue
		}
		result = append(result, candidate)
	}
	return result
}

func selectEvidence(candidates []*rankedCandidate, analysis queryAnalysis, maxItems, maxTokens int) ([]Evidence, []string, int) {
	var evidence []Evidence
	var nodeIDs []string
	for _, candidate := range candidates {
		if candidate.stateOnly && candidate.document.ResourceKind == "node" {
			nodeIDs = append(nodeIDs, candidate.document.ResourceID)
		}
	}
	claims := map[string]struct{}{}
	sourceSlot := map[string]struct{}{}
	tokens := 0
	deduplicated := 0
	for _, candidate := range candidates {
		if candidate.stateOnly {
			continue
		}
		document := candidate.document
		claim := normalizeClaim(document.Content)
		if _, ok := claims[claim]; ok {
			deduplicated++
			continue
		}
		if document.SlotID != "" {
			duplicateSource := false
			for _, sourceID := range document.SourceEventIDs {
				key := sourceID + "\x1f" + document.SlotID
				if _, ok := sourceSlot[key]; ok {
					duplicateSource = true
					break
				}
			}
			if duplicateSource {
				deduplicated++
				continue
			}
		}
		cost := estimateTokens(document.Content)
		if len(evidence) > 0 && tokens+cost > maxTokens {
			continue
		}
		item := Evidence{ID: document.ID, ResourceID: document.ResourceID, ResourceKind: document.ResourceKind,
			Content: document.Content, Score: candidate.score, OccurredAt: document.OccurredAt,
			ContextID: document.ContextID, Actor: document.Actor, SlotID: document.SlotID, Status: document.Status,
			SourceEventIDs: append([]string(nil), document.SourceEventIDs...), MatchReasons: uniqueStrings(candidate.reasons)}
		evidence = append(evidence, item)
		tokens += cost
		claims[claim] = struct{}{}
		for _, sourceID := range document.SourceEventIDs {
			sourceSlot[sourceID+"\x1f"+document.SlotID] = struct{}{}
		}
		if document.ResourceKind == "node" {
			nodeIDs = append(nodeIDs, document.ResourceID)
		}
		if len(evidence) >= maxItems {
			break
		}
	}
	return evidence, uniqueStrings(nodeIDs), deduplicated
}

func filterCurrentNodes(nodes []MemoryNode, reference time.Time) []MemoryNode {
	var result []MemoryNode
	for _, node := range nodes {
		allowed := node.Status == SemanticActive || node.Status == SemanticScoped || node.Status == SemanticSuperseded
		if !allowed || (!node.ValidFrom.IsZero() && node.ValidFrom.After(reference)) ||
			(!node.ValidUntil.IsZero() && node.ValidUntil.Before(reference)) {
			continue
		}
		result = append(result, node)
	}
	sort.SliceStable(result, func(i, j int) bool {
		return firstTimeMemory(result[i].ValidFrom, result[i].CreatedAt).Before(firstTimeMemory(result[j].ValidFrom, result[j].CreatedAt))
	})
	return result
}

func (f *Fabric) loadRelevantConflicts(ctx context.Context, space string, nodes []MemoryNode,
	candidates []*rankedCandidate) ([]Conflict, error) {
	var slotIDs []string
	for _, node := range nodes {
		slotIDs = append(slotIDs, node.SlotID)
	}
	for _, candidate := range candidates {
		if candidate.document.Status == SemanticPendingResolution {
			slotIDs = append(slotIDs, candidate.document.SlotID)
		}
	}
	slotIDs = uniqueStrings(slotIDs)
	if len(slotIDs) == 0 {
		return nil, nil
	}
	args := []any{normalizeSpace(space), SemanticPendingResolution, SemanticUnresolved}
	for _, id := range slotIDs {
		args = append(args, id)
	}
	rows, err := f.ledger.QueryContext(ctx, `SELECT conflict_id FROM conflict_sets
		WHERE space=? AND status IN (?, ?) AND slot_id IN (`+placeholders(len(slotIDs))+`)
		ORDER BY critical DESC, created_at`, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	var result []Conflict
	for _, id := range ids {
		conflict, err := f.loadConflict(ctx, id, false)
		if err == nil {
			result = append(result, conflict)
		}
	}
	return result, nil
}

func (f *Fabric) searchOverlay(ctx context.Context, space string, analysis queryAnalysis) ([]searchDocument, int64) {
	var indexed, maximum int64
	_ = f.index.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM index_meta WHERE key='indexed_ledger_seq'`).Scan(&indexed)
	_ = f.ledger.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM outbox`).Scan(&maximum)
	lag := maximum - indexed
	if lag <= 0 {
		return nil, 0
	}
	rows, err := f.ledger.QueryContext(ctx, `SELECT resource_kind, resource_id FROM outbox
		WHERE status!='done' ORDER BY seq LIMIT 32`)
	if err != nil {
		return nil, lag
	}
	defer rows.Close()
	queryText := strings.Join(append(analysis.tokens, analysis.literalTerms...), " ")
	var result []searchDocument
	for rows.Next() {
		var kind, id string
		if rows.Scan(&kind, &id) != nil {
			continue
		}
		switch kind {
		case "event":
			event, err := scanEvent(f.ledger.QueryRowContext(ctx, `SELECT event_id, space, context_id, session_id,
				actor, source_kind, content, occurred_at, source_ref, metadata_json FROM events
				WHERE event_id=? AND space=? AND tombstoned=0`, id, space))
			if err == nil && textLocallyMatches(event.Content, queryText, analysis.tokens) {
				result = append(result, searchDocument{ID: event.ID, Space: event.Space, ResourceKind: "event",
					ResourceID: event.ID, Content: event.Content, ContextID: event.ContextID,
					OccurredAt: event.OccurredAt, Status: SemanticEventDurable, SourceEventIDs: []string{event.ID}})
			}
		case "node":
			node, err := f.loadMemoryNode(ctx, id)
			if err == nil && textLocallyMatches(node.Statement+" "+strings.Join(node.Keys, " "), queryText, analysis.tokens) {
				var sources []string
				for _, source := range node.Sources {
					sources = append(sources, source.EventID)
				}
				result = append(result, searchDocument{ID: node.ID, Space: node.Space, ResourceKind: "node",
					ResourceID: node.ID, Content: node.Statement, ContextID: node.ContextID, SlotID: node.SlotID,
					OccurredAt: firstTimeMemory(node.ValidFrom, node.CreatedAt), Status: node.Status,
					SourceEventIDs: uniqueStrings(sources)})
			}
		}
	}
	return result, lag
}

func textLocallyMatches(text, query string, tokens []string) bool {
	text = normalizeClaim(text)
	if query != "" && strings.Contains(text, normalizeClaim(query)) {
		return true
	}
	for _, token := range tokens {
		if len([]rune(token)) > 1 && strings.Contains(text, token) {
			return true
		}
	}
	return false
}

func memorySearchInsufficient(analysis queryAnalysis, result SearchResult) bool {
	if len(result.Evidence) == 0 || len(result.Conflicts) > 0 {
		return true
	}
	return false
}

var (
	quotedMemoryTerm = regexp.MustCompile(`["“”']([^"“”']{2,})["“”']`)
	pathMemoryTerm   = regexp.MustCompile(`(?:[A-Za-z]:)?(?:[/~.][^\s,，;；:：!?！？]+)+`)
	idMemoryTerm     = regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9_-]{5,}\b`)
	uuidMemoryTerm   = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

func analyzeMemoryQuery(query string) queryAnalysis {
	analysis := queryAnalysis{tokens: queryTokens(query, 32), weights: map[string]float64{}, frequencies: map[string]int{}}
	for _, token := range analysis.tokens {
		analysis.weights[token] = 1
	}
	for _, match := range quotedMemoryTerm.FindAllStringSubmatch(query, -1) {
		if len(match) > 1 {
			analysis.literalTerms = append(analysis.literalTerms, match[1])
		}
	}
	paths := pathMemoryTerm.FindAllString(query, -1)
	analysis.literalTerms = append(analysis.literalTerms, paths...)
	for _, candidate := range idMemoryTerm.FindAllString(query, -1) {
		// Identifier-shaped values remain literal lexical features. They never
		// select or bypass a retrieval stage.
		if strings.ContainsAny(candidate, "_0123456789") {
			analysis.literalTerms = append(analysis.literalTerms, candidate)
		}
	}
	standalone := strings.Trim(strings.TrimSpace(query), "`")
	if uuidMemoryTerm.MatchString(standalone) {
		analysis.literalTerms = append(analysis.literalTerms, standalone)
	}
	analysis.literalTerms = normalizeStringList(analysis.literalTerms, 8)
	return analysis
}

func queryTokens(value string, limit int) []string {
	fields := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		// Match SQLite FTS5's default unicode tokenizer. Structured paths and
		// identifiers are detected separately and do not need to remain one token.
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
	})
	var result []string
	for _, field := range fields {
		field = strings.Trim(field, ".,:;!?，。；：！？()[]{}<>")
		if field == "" {
			continue
		}
		if isSingleASCIILetter(field) {
			continue
		}
		field = normalizeClaim(field)
		result = append(result, field)
		if variant := singularQueryToken(field); variant != "" {
			result = append(result, variant)
		}
		result = append(result, inflectionQueryTokens(field)...)
	}
	result = normalizeStringList(result, 0)
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result
}

func inflectionQueryTokens(token string) []string {
	runes := []rune(token)
	if len(runes) <= 4 {
		return nil
	}
	var variants []string
	addStem := func(stem string) {
		if len([]rune(stem)) >= 3 && stem != token {
			variants = append(variants, stem)
		}
	}
	if strings.HasSuffix(token, "ied") && len(runes) > 5 {
		addStem(strings.TrimSuffix(token, "ied") + "y")
		return normalizeStringList(variants, 0)
	}
	if strings.HasSuffix(token, "ed") {
		stem := strings.TrimSuffix(token, "ed")
		addStem(stem)
		addStem(trimDoubledFinalRune(stem))
		addStem(strings.TrimSuffix(token, "d"))
	}
	if strings.HasSuffix(token, "ing") {
		stem := strings.TrimSuffix(token, "ing")
		addStem(stem)
		addStem(trimDoubledFinalRune(stem))
		addStem(stem + "e")
	}
	return normalizeStringList(variants, 0)
}

func trimDoubledFinalRune(value string) string {
	runes := []rune(value)
	if len(runes) >= 2 && runes[len(runes)-1] == runes[len(runes)-2] {
		return string(runes[:len(runes)-1])
	}
	return value
}

func isSingleASCIILetter(value string) bool {
	runes := []rune(value)
	return len(runes) == 1 && ((runes[0] >= 'a' && runes[0] <= 'z') || (runes[0] >= 'A' && runes[0] <= 'Z'))
}

func singularQueryToken(token string) string {
	runes := []rune(token)
	if len(runes) <= 4 {
		return ""
	}
	if strings.HasSuffix(token, "ies") && len(runes) > 5 {
		return strings.TrimSuffix(token, "ies") + "y"
	}
	if strings.HasSuffix(token, "ses") || strings.HasSuffix(token, "xes") || strings.HasSuffix(token, "zes") ||
		strings.HasSuffix(token, "ches") || strings.HasSuffix(token, "shes") {
		return strings.TrimSuffix(token, "es")
	}
	if strings.HasSuffix(token, "s") && !strings.HasSuffix(token, "ss") {
		return strings.TrimSuffix(token, "s")
	}
	return ""
}

func ftsTerms(value string, limit int) string {
	return ftsTermsFromTokens(queryTokens(value, limit), limit)
}

func ftsTermsFromTokens(tokens []string, limit int) string {
	if limit > 0 && len(tokens) > limit {
		tokens = tokens[:limit]
	}
	parts := make([]string, 0, len(tokens))
	for _, token := range tokens {
		token = strings.ReplaceAll(token, `"`, `""`)
		if token != "" {
			parts = append(parts, `"`+token+`"`)
		}
	}
	return strings.Join(parts, " OR ")
}
