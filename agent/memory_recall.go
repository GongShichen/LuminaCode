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
	limit := cfg.MemoryRecallMaxItems
	if limit <= 0 {
		limit = 8
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
	expansion, expansionModel, expansionError := expandMemoryQuery(ctx, cfg, memoryQuery, catalog, expansionFactory)
	if catalogErr != nil {
		if expansionError != "" {
			expansionError += "; "
		}
		expansionError += "inspect memory catalog: " + catalogErr.Error()
	}
	var embedder longmemory.Embedder
	if cfg.MemoryEmbeddingEnabled {
		if local, embedErr := longmemory.SharedLocalEmbedder(cfg.MemoryEmbeddingModel, cfg.MemoryEmbeddingModelDir); embedErr == nil {
			embedder = local
		}
	}
	result, err := store.SearchAllChannels(ctx, memoryQuery, expansion, embedder, longmemory.HybridSearchOptions{
		FTSCandidates:       cfg.MemoryFTSCandidates,
		VectorCandidates:    cfg.MemoryVectorCandidates,
		GraphCandidates:     cfg.MemoryGraphCandidates,
		GraphMaxHops:        cfg.MemoryGraphMaxHops,
		RRFK:                cfg.MemoryRRFK,
		MMRLambda:           cfg.MemoryMMRLambda,
		MaxItems:            limit,
		CoreContextTokens:   cfg.MemoryCoreContextTokens,
		TargetContextTokens: cfg.MemoryContextTargetTokens,
		MaxContextTokens:    cfg.MemoryContextMaxTokens,
		LocalTimeout:        time.Duration(cfg.MemoryRetrievalLocalTimeoutSeconds * float64(time.Second)),
		SessionID:           cfgSessionID(state),
		TeamSessionID:       state.MemoryTeamSessionID,
		AgentID:             memoryAgentType(state),
		ExcludeIDs:          memory.RecalledMemoryIDs(state.Messages),
		ExpansionModel:      expansionModel,
		ExpansionError:      expansionError,
		NeighborChunks:      cfg.MemoryEvidenceNeighborChunks,
	})
	if err != nil || (len(result.Packet.Evidence) == 0 && len(result.Packet.CoreBlocks) == 0) {
		return nil
	}
	if len(result.Packet.Evidence) > limit {
		result.Packet.Evidence = result.Packet.Evidence[:limit]
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
	_ = store.RecordUsed(ctx, longmemory.UsedRecord{
		SessionID:     cfgSessionID(state),
		TeamSessionID: state.MemoryTeamSessionID,
		AgentID:       memoryAgentType(state),
		Query:         query,
		MemoryIDs:     ids,
	})
	return recalls
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
		result = append(result, longmemory.MessageExcerpt{Role: role, Text: text})
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
