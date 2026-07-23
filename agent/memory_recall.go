package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"LuminaCode/config"
	"LuminaCode/memory"
)

type MemoryRecall = memory.MemoryRecall

func RunMemoryRecallWithEngine(ctx context.Context, cfg config.Config, state *AgentState, query string,
	engine memory.Engine) []MemoryRecall {
	result, referenceTime, err := SearchMemoryWithEngine(ctx, cfg, state, query, engine)
	if err != nil {
		return nil
	}
	return MemoryRecallsFromSearchResult(result, referenceTime)
}

func SearchMemoryWithEngine(ctx context.Context, cfg config.Config, state *AgentState, query string,
	engine memory.Engine) (memory.SearchResult, time.Time, error) {
	if state == nil || strings.TrimSpace(query) == "" || !cfg.LongTermMemoryEnabled ||
		!cfg.UsesMemoryFabric() || engine == nil {
		return memory.SearchResult{}, time.Time{}, nil
	}
	referenceTime := state.MemoryQueryTime
	if referenceTime.IsZero() {
		referenceTime = time.Now().UTC()
	}
	result, err := engine.Search(ctx, memory.SearchRequest{
		Space: fabricMemorySpace(cfg), Query: strings.TrimSpace(query), ReferenceTime: referenceTime,
		ContextID: cfgSessionID(state), MaxEvidence: cfg.MemoryRecallMaxItems,
		MaxContextTokens: cfg.MemoryContextMaxTokens, IncludeDiagnostics: true,
	})
	return result, referenceTime, err
}

func MemoryRecallsFromSearchResult(result memory.SearchResult, referenceTime time.Time) []MemoryRecall {
	if len(result.Evidence) == 0 && len(result.Conflicts) == 0 && !result.Insufficient {
		return nil
	}
	content, ids := formatFabricSearchResult(result, referenceTime)
	if strings.TrimSpace(content) == "" {
		return nil
	}
	return []MemoryRecall{{
		Filename: "memory-fabric", Content: content, RecallID: "memory-fabric", RecallIDs: ids,
	}}
}

func formatFabricSearchResult(result memory.SearchResult, referenceTime time.Time) (string, []string) {
	var output strings.Builder
	fmt.Fprintf(&output, "Reference time: %s\n", referenceTime.UTC().Format(time.RFC3339))
	fmt.Fprintf(&output, "Local memory route: %s.\n", strings.Join(result.Route, ", "))
	ids := make([]string, 0, len(result.Evidence))
	if len(result.Evidence) > 0 {
		output.WriteString("\nEvidence:\n")
	}
	for index, evidence := range result.Evidence {
		id := strings.TrimSpace(evidence.ResourceID)
		if id == "" {
			id = strings.TrimSpace(evidence.ID)
		}
		ids = append(ids, id)
		ids = append(ids, evidence.SourceEventIDs...)
		fmt.Fprintf(&output, "[%d] id=%s kind=%s", index+1, id, evidence.ResourceKind)
		if !evidence.OccurredAt.IsZero() {
			fmt.Fprintf(&output, " time=%s", evidence.OccurredAt.UTC().Format(time.RFC3339))
		}
		if evidence.ContextID != "" {
			fmt.Fprintf(&output, " context=%s", evidence.ContextID)
		}
		if evidence.Actor != "" {
			fmt.Fprintf(&output, " actor=%s", evidence.Actor)
		}
		if evidence.Status != "" {
			fmt.Fprintf(&output, " status=%s", evidence.Status)
		}
		if evidence.SlotID != "" {
			fmt.Fprintf(&output, " slot=%s", evidence.SlotID)
		}
		if len(evidence.SourceEventIDs) > 0 {
			fmt.Fprintf(&output, " sources=%s", strings.Join(evidence.SourceEventIDs, ","))
		}
		fmt.Fprintf(&output, "\n%s\n", strings.TrimSpace(evidence.Content))
	}
	if len(result.Conflicts) > 0 {
		output.WriteString("\nUnresolved memory conflicts:\n")
		for _, conflict := range result.Conflicts {
			fmt.Fprintf(&output, "- conflict=%s slot=%s generation=%s; retain alternatives and do not silently choose.\n",
				conflict.ID, conflict.SlotID, conflict.Generation)
			ids = append(ids, conflict.ID)
		}
	}
	if result.Insufficient {
		output.WriteString("\nMemory evidence is insufficient for the exact requested target. Do not substitute a nearby entity or guess.\n")
	}
	return strings.TrimSpace(output.String()), uniqueMemoryRecallIDs(ids)
}

func uniqueMemoryRecallIDs(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
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

func cfgSessionID(state *AgentState) string {
	if state == nil {
		return ""
	}
	return strings.TrimSpace(state.MemorySessionID)
}
