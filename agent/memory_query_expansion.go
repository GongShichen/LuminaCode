package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"LuminaCode/api"
	"LuminaCode/config"
	"LuminaCode/longmemory"

	"github.com/araddon/dateparse"
)

type MemoryExpansionClientFactory func(context.Context, string) (api.LLMClient, error)

func expandMemoryQuery(ctx context.Context, cfg config.Config, query longmemory.MemoryQuery, catalog longmemory.MemoryCatalog, factory MemoryExpansionClientFactory) (longmemory.QueryExpansion, string, string) {
	if !cfg.MemoryQueryExpansionEnabled || factory == nil {
		return longmemory.QueryExpansion{}, "", ""
	}
	model := strings.TrimSpace(cfg.MemoryQueryExpansionModel)
	if model == "" || strings.EqualFold(model, "inherit") {
		model = cfg.APIModel
	}
	client, err := factory(ctx, model)
	if err != nil {
		return longmemory.QueryExpansion{}, model, err.Error()
	}
	expansionCtx := ctx
	cancel := func() {}
	if cfg.MemoryQueryExpansionTimeoutSeconds > 0 {
		expansionCtx, cancel = context.WithTimeout(ctx,
			time.Duration(cfg.MemoryQueryExpansionTimeoutSeconds*float64(time.Second)))
	}
	defer cancel()
	payload := map[string]any{
		"query":          query.Text,
		"recent_context": query.RecentContext,
		"current_time":   query.Timestamp.Format(time.RFC3339),
		"memory_catalog": catalog,
	}
	encoded, _ := json.Marshal(payload)
	system := "Expand a memory retrieval query without selecting retrieval channels or filtering memory. " +
		"Use the ExpandMemoryQuery tool exactly once. Preserve uncertainty and do not invent facts."
	tool := memoryExpansionToolSchema(cfg.MemoryQueryExpansionMaxQueries)
	var input map[string]any
	var streamErr error
	for result := range client.StreamChat(expansionCtx, system,
		[]map[string]any{{"role": "user", "content": string(encoded)}}, []map[string]any{tool}, nil) {
		if result.Err != nil {
			streamErr = result.Err
			continue
		}
		if stringFromAny(result.Event["type"]) == "error" {
			streamErr = fmt.Errorf("%s", stringFromAny(result.Event["message"]))
			continue
		}
		if stringFromAny(result.Event["type"]) == "tool_use" &&
			strings.EqualFold(stringFromAny(result.Event["name"]), "ExpandMemoryQuery") {
			input, _ = result.Event["input"].(map[string]any)
		}
	}
	if streamErr != nil {
		return longmemory.QueryExpansion{}, model, streamErr.Error()
	}
	if input == nil {
		return longmemory.QueryExpansion{}, model, "query expansion model did not call ExpandMemoryQuery"
	}
	expansion, err := parseQueryExpansion(input, cfg.MemoryQueryExpansionMaxQueries)
	if err != nil {
		return longmemory.QueryExpansion{}, model, err.Error()
	}
	return expansion, model, ""
}

func ExpandMemoryQuery(ctx context.Context, cfg config.Config, query longmemory.MemoryQuery, catalog longmemory.MemoryCatalog, factory MemoryExpansionClientFactory) (longmemory.QueryExpansion, string, string) {
	return expandMemoryQuery(ctx, cfg, query, catalog, factory)
}

func memoryExpansionToolSchema(maxQueries int) map[string]any {
	if maxQueries <= 0 {
		maxQueries = 5
	}
	return map[string]any{
		"name":        "ExpandMemoryQuery",
		"description": "Generate generic query rewrites, normalized entities, time constraints and relation terms. It cannot select retrieval channels, scopes, memory types, limits or memory IDs.",
		"input_schema": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"queries": map[string]any{"type": "array", "maxItems": maxQueries,
					"items": map[string]any{"type": "string"}},
				"entities": map[string]any{"type": "array", "maxItems": 16,
					"items": map[string]any{"type": "string"}},
				"temporal_constraints": map[string]any{"type": "array", "maxItems": 4,
					"items": map[string]any{"type": "object", "additionalProperties": false,
						"properties": map[string]any{
							"from": map[string]any{"type": "string"}, "to": map[string]any{"type": "string"},
							"at": map[string]any{"type": "string"}, "order": map[string]any{"type": "string", "enum": []string{"asc", "desc", "none"}},
						}}},
				"relation_terms": map[string]any{"type": "array", "maxItems": 16,
					"items": map[string]any{"type": "string"}},
			},
		},
	}
}

func parseQueryExpansion(input map[string]any, maxQueries int) (longmemory.QueryExpansion, error) {
	if maxQueries <= 0 {
		maxQueries = 5
	}
	expansion := longmemory.QueryExpansion{
		Queries:       stringList(input["queries"], maxQueries),
		Entities:      stringList(input["entities"], 16),
		RelationTerms: stringList(input["relation_terms"], 16),
	}
	if rawConstraints, ok := input["temporal_constraints"].([]any); ok {
		if len(rawConstraints) > 4 {
			rawConstraints = rawConstraints[:4]
		}
		for _, raw := range rawConstraints {
			value, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			constraint := longmemory.TemporalConstraint{Order: strings.ToLower(strings.TrimSpace(stringFromAny(value["order"])))}
			if constraint.Order == "" {
				constraint.Order = "none"
			}
			if constraint.Order != "none" && constraint.Order != "asc" && constraint.Order != "desc" {
				constraint.Order = "none"
			}
			constraint.FromText = strings.TrimSpace(stringFromAny(value["from"]))
			constraint.ToText = strings.TrimSpace(stringFromAny(value["to"]))
			constraint.AtText = strings.TrimSpace(stringFromAny(value["at"]))
			constraint.From, _ = parseExpansionTime(value["from"])
			constraint.To, _ = parseExpansionTime(value["to"])
			constraint.At, _ = parseExpansionTime(value["at"])
			expansion.TemporalConstraints = append(expansion.TemporalConstraints, constraint)
		}
	}
	return expansion, nil
}

func parseExpansionTime(value any) (time.Time, error) {
	text := strings.TrimSpace(stringFromAny(value))
	if text == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		parsed, err = dateparse.ParseAny(text)
		if err != nil {
			return time.Time{}, err
		}
	}
	return parsed.UTC(), nil
}

func stringList(value any, limit int) []string {
	var values []string
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if text := strings.TrimSpace(stringFromAny(item)); text != "" {
				values = append(values, text)
			}
		}
	case []string:
		values = append(values, typed...)
	}
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
		if len(result) >= limit {
			break
		}
	}
	sort.Strings(result)
	return result
}
