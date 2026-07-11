package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"LuminaCode/config"
	"LuminaCode/longmemory"
	"LuminaCode/memory"
)

type MemoryRecall = memory.MemoryRecall

func RunMemoryRecallWithQuery(ctx context.Context, state *AgentState, query string) []MemoryRecall {
	return RunMemoryRecallWithConfig(ctx, config.GetConfig(), state, query)
}

func RunMemoryRecallWithConfig(ctx context.Context, cfg config.Config, state *AgentState, query string) []MemoryRecall {
	return RunMemoryRecallWithRuntime(ctx, cfg, state, query, nil)
}

func RunMemoryRecallWithRuntime(ctx context.Context, cfg config.Config, state *AgentState, query string, expansionFactory MemoryExpansionClientFactory) []MemoryRecall {
	if state == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	if cfg.LongTermMemoryEnabled {
		return recallLongTermMemories(ctx, cfg, state, query, expansionFactory)
	}
	return nil
}

func recallLongTermMemories(ctx context.Context, cfg config.Config, state *AgentState, query string, expansionFactory MemoryExpansionClientFactory) []MemoryRecall {
	store, err := longmemory.Open(ctx, cfg.LongTermMemoryStore)
	if err != nil {
		return nil
	}
	defer store.Close()
	limit := cfg.MemoryAtomMaxSelected
	if limit <= 0 {
		limit = 32
	}
	scopes := longmemory.RuntimeScopes(cfg.CWD, memoryAgentType(state), state.MemoryTeamName, memoryTeamAgentID(state))
	queryTime := state.MemoryQueryTime
	if queryTime.IsZero() {
		queryTime = time.Now().UTC()
	}
	memoryQuery := longmemory.MemoryQuery{
		Text:          strings.TrimSpace(query),
		RecentContext: recentMemoryContext(state.Messages, 4, 2000),
		Timestamp:     queryTime,
		Scopes:        scopes,
		SessionID:     cfgSessionID(state),
		TeamSessionID: state.MemoryTeamSessionID,
		AgentID:       memoryAgentType(state),
	}
	catalog, catalogErr := store.InspectCatalog(ctx, scopes)
	var embedder longmemory.Embedder
	if cfg.MemoryEmbeddingEnabled {
		embedder = configuredMemoryEmbedder(cfg)
	}
	searchOptions := func(expansionModel, expansionError string, waitMS int64) longmemory.HybridSearchOptions {
		return longmemory.HybridSearchOptions{
			FTSCandidates:                 cfg.MemoryFTSCandidates,
			VectorCandidates:              cfg.MemoryVectorCandidates,
			GraphCandidates:               cfg.MemoryGraphCandidates,
			GraphMaxHops:                  cfg.MemoryGraphMaxHops,
			RRFK:                          cfg.MemoryRRFK,
			SessionRetrieval:              cfg.MemorySessionRetrievalEnabled,
			SessionCandidates:             cfg.MemorySessionCandidates,
			ChunksPerSession:              cfg.MemoryChunksPerSession,
			SessionChunkCandidates:        cfg.MemorySessionChunkCandidates,
			MaxItems:                      limit,
			CoreContextTokens:             cfg.MemoryCoreContextTokens,
			TargetContextTokens:           cfg.MemoryContextTargetTokens,
			MaxContextTokens:              cfg.MemoryContextMaxTokens,
			LocalTimeout:                  time.Duration(cfg.MemoryRetrievalLocalTimeoutSeconds * float64(time.Second)),
			SessionID:                     cfgSessionID(state),
			TeamSessionID:                 state.MemoryTeamSessionID,
			AgentID:                       memoryAgentType(state),
			ExcludeIDs:                    memory.RecalledMemoryIDs(state.Messages),
			ExpansionModel:                expansionModel,
			ExpansionError:                expansionError,
			ExpansionWaitMS:               waitMS,
			NeighborChunks:                cfg.MemoryAdjacentChunkWindow,
			ReferenceTime:                 queryTime,
			CanonicalEntityEnabled:        cfg.MemoryCanonicalEntityEnabled,
			CanonicalEventEnabled:         cfg.MemoryCanonicalEventEnabled,
			CacheEnabled:                  cfg.MemoryRetrievalCacheEnabled,
			CacheTTL:                      time.Duration(cfg.MemoryRetrievalCacheTTLSeconds * float64(time.Second)),
			AtomMaxSelected:               cfg.MemoryAtomMaxSelected,
			CoverageMaxFacets:             cfg.MemoryCoverageMaxFacets,
			CoverageCompletionRounds:      cfg.MemoryCoverageCompletionRounds,
			CoverageRelevanceWeight:       cfg.MemoryCoverageRelevanceWeight,
			CoverageFacetWeight:           cfg.MemoryCoverageFacetWeight,
			CoverageProvenanceWeight:      cfg.MemoryCoverageProvenanceWeight,
			CoverageSourceWeight:          cfg.MemoryCoverageSourceWeight,
			CoverageCoherenceWeight:       cfg.MemoryCoverageCoherenceWeight,
			EvidencePrimaryBudgetRatio:    cfg.MemoryEvidencePrimaryBudgetRatio,
			EvidenceCompletionBudgetRatio: cfg.MemoryEvidenceCompletionBudgetRatio,
			EvidenceContextBudgetRatio:    cfg.MemoryEvidenceContextBudgetRatio,
		}
	}
	type retrievalResult struct {
		value longmemory.AllChannelResult
		err   error
	}
	originalCh := make(chan retrievalResult, 1)
	go func() {
		baselineOptions := searchOptions("", "", 0)
		baselineOptions.SuppressTrace = true
		value, searchErr := store.SearchAllChannels(ctx, memoryQuery, longmemory.QueryExpansion{}, embedder, baselineOptions)
		originalCh <- retrievalResult{value: value, err: searchErr}
	}()
	expansionStarted := time.Now()
	expansion, expansionModel, expansionError := expandMemoryQuery(ctx, cfg, memoryQuery, catalog, expansionFactory)
	expansionWaitMS := time.Since(expansionStarted).Milliseconds()
	if catalogErr != nil {
		if expansionError != "" {
			expansionError += "; "
		}
		expansionError += "inspect memory catalog: " + catalogErr.Error()
	}
	var result longmemory.AllChannelResult
	var searchErr error
	if len(expansion.Queries)+len(expansion.Entities)+len(expansion.TemporalConstraints)+len(expansion.RelationTerms)+len(expansion.ProvenanceHints) > 0 {
		result, searchErr = store.SearchAllChannels(ctx, memoryQuery, expansion, embedder,
			searchOptions(expansionModel, expansionError, expansionWaitMS))
		original := <-originalCh
		if searchErr != nil && original.err == nil {
			result, searchErr = original.value, nil
		}
	} else {
		original := <-originalCh
		result, searchErr = original.value, original.err
		result.Run.ExpansionModel, result.Run.ExpansionError, result.Run.ExpansionWaitMS = expansionModel, expansionError, expansionWaitMS
		result.Trace.Run = &result.Run
		_ = store.RecordRetrievalTrace(context.WithoutCancel(ctx), result.Trace)
	}
	if searchErr != nil {
		return nil
	}
	var ids []string
	recalls := make([]MemoryRecall, 0, len(result.Packet.Evidence)+1)
	if len(result.Packet.CoreBlocks) > 0 {
		var blockLines []string
		for _, block := range result.Packet.CoreBlocks {
			blockLines = append(blockLines, block.Label+":\n"+block.Content)
		}
		recalls = append(recalls, MemoryRecall{Filename: "core-memory", FilePath: "longmemory://core",
			Content:    "Core long-term memory blocks:\n" + strings.Join(blockLines, "\n\n"),
			MemoryType: memory.MemoryTypeUser, RecallID: "core-memory", Score: 1})
	}
	for _, evidence := range result.Packet.Evidence {
		if len(evidence.DocumentIDs) > 0 {
			ids = append(ids, evidence.DocumentIDs...)
		} else {
			ids = append(ids, evidence.MemoryID)
		}
		recalls = append(recalls, MemoryRecall{
			Filename:   evidence.MemoryID,
			FilePath:   "longmemory://" + evidence.MemoryID,
			Content:    formatLongTermEvidence(evidence),
			MemoryType: mapLongMemoryType(evidence.MemoryType),
			RecallID:   evidence.MemoryID,
			Score:      evidence.Score,
		})
	}
	if len(result.Packet.CanonicalEvents) > 0 {
		lines := []string{"Canonical event timeline (ordered by event time):"}
		for _, event := range result.Packet.CanonicalEvents {
			when := event.OccurredAt
			if when.IsZero() {
				when = event.ValidFrom
			}
			line := "- " + event.Title
			if !when.IsZero() {
				line += " @ " + when.UTC().Format(time.RFC3339)
			}
			if strings.TrimSpace(event.Summary) != "" {
				line += ": " + strings.TrimSpace(event.Summary)
			}
			lines = append(lines, line)
			ids = append(ids, event.SourceChunks...)
		}
		recalls = append(recalls, MemoryRecall{Filename: "canonical-event-timeline", FilePath: "longmemory://events",
			Content: strings.Join(lines, "\n"), MemoryType: memory.MemoryTypeReference,
			RecallID: "canonical-event-timeline", Score: 1})
	}
	_ = store.RecordUsed(ctx, longmemory.UsedRecord{
		SessionID:     cfgSessionID(state),
		TeamSessionID: state.MemoryTeamSessionID,
		AgentID:       memoryAgentType(state),
		Query:         query,
		MemoryIDs:     ids,
	})
	reference := "Reference time for this user turn: " + queryTime.UTC().Format(time.RFC3339) +
		"\nInterpret relative dates and order evidence against this reference time. Use provenance and valid time when evidence conflicts."
	if len(recalls) == 0 {
		recalls = append(recalls, MemoryRecall{Filename: "query-reference-time", FilePath: "longmemory://reference-time",
			Content: reference, MemoryType: memory.MemoryTypeReference, RecallID: "query-reference-time", Score: 1})
	} else {
		recalls[0].Content = reference + "\n\n" + recalls[0].Content
	}
	return recalls
}

func configuredMemoryEmbedder(cfg config.Config) longmemory.Embedder {
	local, err := longmemory.SharedLocalEmbedder(cfg.MemoryEmbeddingModel, cfg.MemoryEmbeddingModelDir)
	if err != nil {
		return nil
	}
	return longmemory.SharedEmbeddingScheduler(local, longmemory.EmbeddingSchedulerOptions{
		BatchSize:         cfg.MemoryEmbeddingBatchSize,
		BatchWait:         time.Duration(cfg.MemoryEmbeddingBatchWaitMS) * time.Millisecond,
		QueryCacheEntries: cfg.MemoryEmbeddingQueryCacheEntries,
		ExecutionTimeout:  time.Duration(cfg.MemoryEmbeddingExecutionTimeout * float64(time.Second)),
	})
}

func recentMemoryContext(messages []map[string]any, maxMessages, maxTokens int) []longmemory.MessageExcerpt {
	if maxMessages <= 0 {
		maxMessages = 4
	}
	visible := StripTransientContextMessages(messages)
	result := make([]longmemory.MessageExcerpt, 0, maxMessages)
	tokens := 0
	for index := len(visible) - 1; index >= 0 && len(result) < maxMessages; index-- {
		message := visible[index]
		role := strings.ToLower(strings.TrimSpace(stringFromAny(message["role"])))
		if role != "user" && role != "assistant" {
			continue
		}
		metadata, _ := message["metadata"].(map[string]any)
		if source := strings.ToLower(strings.TrimSpace(stringFromAny(metadata["source"]))); source != "" && source != "user" && source != "assistant" {
			continue
		}
		text := visibleMessageText(message["content"])
		if text == "" {
			continue
		}
		cost := maxIntAgent(1, len([]rune(text))/3)
		if maxTokens > 0 && tokens+cost > maxTokens {
			remaining := maxTokens - tokens
			if remaining <= 0 {
				break
			}
			runes := []rune(text)
			maxRunes := remaining * 3
			if len(runes) > maxRunes {
				text = string(runes[len(runes)-maxRunes:])
			}
			cost = remaining
		}
		var timestamp time.Time
		for _, value := range []any{message["timestamp"], metadata["timestamp"]} {
			if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(stringFromAny(value))); err == nil {
				timestamp = parsed.UTC()
				break
			}
		}
		result = append(result, longmemory.MessageExcerpt{Role: role, Text: text, Timestamp: timestamp})
		tokens += cost
	}
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result
}

func visibleMessageText(content any) string {
	if text, ok := content.(string); ok {
		return strings.TrimSpace(text)
	}
	var parts []string
	appendBlock := func(block map[string]any) {
		if kind := strings.ToLower(stringFromAny(block["type"])); kind == "text" || kind == "output_text" {
			if text := strings.TrimSpace(stringFromAny(block["text"])); text != "" {
				parts = append(parts, text)
			}
		}
	}
	switch blocks := content.(type) {
	case []map[string]any:
		for _, block := range blocks {
			appendBlock(block)
		}
	case []any:
		for _, raw := range blocks {
			if block, ok := raw.(map[string]any); ok {
				appendBlock(block)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func maxIntAgent(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func formatLongTermEvidence(evidence longmemory.Evidence) string {
	parts := []string{
		"Long-term evidence ID: " + evidence.MemoryID,
		"Document kind: " + firstNonEmptyString(evidence.DocumentKind, "memory"),
		"Scope: " + string(evidence.ScopeType) + "/" + evidence.ScopeKey,
		"Type: " + string(evidence.MemoryType),
		"Confidence: " + formatFloat(evidence.Confidence),
	}
	if role := strings.TrimSpace(stringFromAny(evidence.Metadata["role"])); role != "" {
		parts = append(parts, "Provenance role: "+role)
	}
	if status := strings.TrimSpace(stringFromAny(evidence.Metadata["epistemic_status"])); status != "" {
		parts = append(parts, "Epistemic status: "+status)
	}
	if !evidence.OccurredAt.IsZero() {
		parts = append(parts, "Occurred at: "+evidence.OccurredAt.Format(time.RFC3339))
	}
	if !evidence.ValidFrom.IsZero() {
		parts = append(parts, "Valid from: "+evidence.ValidFrom.Format(time.RFC3339))
	}
	if !evidence.ValidUntil.IsZero() {
		parts = append(parts, "Valid until: "+evidence.ValidUntil.Format(time.RFC3339)+" (historical or superseded)")
	}
	if evidence.SourceSession != "" {
		parts = append(parts, "Source session: "+evidence.SourceSession)
	}
	if len(evidence.SourceMessages) > 0 {
		parts = append(parts, "Source messages: "+strings.Join(evidence.SourceMessages, ", "))
	}
	if len(evidence.DocumentIDs) > 1 {
		parts = append(parts, "Evidence chunks: "+strings.Join(evidence.DocumentIDs, ", "))
	}
	if len(evidence.SourcePaths) > 0 {
		parts = append(parts, "Source paths: "+strings.Join(evidence.SourcePaths, ", "))
	}
	parts = append(parts, "Evidence:\n"+evidence.Text)
	parts = append(parts, "Reminder: verify current files and code behavior before relying on path-specific or code-specific evidence.")
	return strings.Join(parts, "\n")
}

func mapLongMemoryType(t longmemory.MemoryType) memory.MemoryType {
	switch t {
	case longmemory.TypeFeedback:
		return memory.MemoryTypeFeedback
	case longmemory.TypeProject:
		return memory.MemoryTypeProject
	case longmemory.TypeReference:
		return memory.MemoryTypeReference
	default:
		return memory.MemoryTypeUser
	}
}

func cfgSessionID(state *AgentState) string {
	if state == nil {
		return ""
	}
	return strings.TrimSpace(state.MemorySessionID)
}

func memoryAgentType(state *AgentState) string {
	if state == nil {
		return "main"
	}
	if strings.TrimSpace(state.MemoryAgentType) != "" {
		return strings.TrimSpace(state.MemoryAgentType)
	}
	if strings.TrimSpace(state.MemoryAgentID) != "" {
		return strings.TrimSpace(state.MemoryAgentID)
	}
	return "main"
}

func memoryTeamAgentID(state *AgentState) string {
	if state == nil {
		return ""
	}
	if strings.TrimSpace(state.MemoryTeamAgentID) != "" {
		return strings.TrimSpace(state.MemoryTeamAgentID)
	}
	if strings.TrimSpace(state.MemoryAgentID) != "" && strings.TrimSpace(state.MemoryTeamName) != "" {
		return strings.TrimSpace(state.MemoryAgentID)
	}
	return ""
}

func formatFloat(value float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", value), "0"), ".")
}
